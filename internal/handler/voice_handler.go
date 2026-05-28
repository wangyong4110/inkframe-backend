package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

func (h *VideoHandler) ListVoiceSegments(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	segs, err := h.videoService.ListVoiceSegments(uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, segs)
}

// AppendVoiceSegment POST /videos/:id/shots/:shot_id/segments
func (h *VideoHandler) AppendVoiceSegment(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var input service.VoiceSegmentInput
	if !bindJSON(c, &input) {
		return
	}
	seg, err := h.videoService.AppendVoiceSegment(uint(shotID), input)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": seg})
}

// InsertVoiceSegment POST /videos/:id/shots/:shot_id/segments/insert
// body: { after_seq_no: int, text, speaker, voice_id }
func (h *VideoHandler) InsertVoiceSegment(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var req struct {
		AfterSeqNo int `json:"after_seq_no"`
		service.VoiceSegmentInput
	}
	if !bindJSON(c, &req) {
		return
	}
	seg, err := h.videoService.InsertVoiceSegment(uint(shotID), req.AfterSeqNo, req.VoiceSegmentInput)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": seg})
}

// UpdateVoiceSegment PUT /videos/:id/shots/:shot_id/segments/:seg_id
func (h *VideoHandler) UpdateVoiceSegment(c *gin.Context) {
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	var input service.VoiceSegmentInput
	if !bindJSON(c, &input) {
		return
	}
	seg, err := h.videoService.UpdateVoiceSegment(uint(segID), input)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, seg)
}

// DeleteVoiceSegment DELETE /videos/:id/shots/:shot_id/segments/:seg_id
func (h *VideoHandler) DeleteVoiceSegment(c *gin.Context) {
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	if err := h.videoService.DeleteVoiceSegment(uint(segID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// GenerateSegmentVoice POST /videos/:id/shots/:shot_id/segments/:seg_id/voice
// 为单条语音段落生成 TTS 音频（异步 Task，与单镜头配音接口对称）
func (h *VideoHandler) GenerateSegmentVoice(c *gin.Context) {
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	var req struct {
		NarrationVoice string `json:"narration_voice"`
	}
	_ = c.ShouldBindJSON(&req)

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("片段 #%d 配音生成", segID), "segment", uint(segID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"narration_voice": req.NarrationVoice,
	})

	go func(taskID string, sID uint, narrationVoice string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateSegmentVoice task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		const maxRetries = 3
		var audioErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			audioErr = h.videoService.GenerateSegmentAudio(sID, tenantID, narrationVoice)
			if audioErr == nil {
				break
			}
			logger.Printf("[VideoHandler] GenerateSegmentVoice task %s seg %d attempt %d/%d: %v",
				taskID, sID, attempt, maxRetries, audioErr)
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt*2) * time.Second)
			}
		}
		if audioErr != nil {
			h.taskSvc.Fail(taskID, audioErr.Error()) //nolint:errcheck
			return
		}
		seg, _ := h.videoService.GetVoiceSegment(sID)
		h.taskSvc.UpdateProgress(taskID, 90)    //nolint:errcheck
		h.taskSvc.Complete(taskID, seg) //nolint:errcheck
	}(task.TaskID, uint(segID), req.NarrationVoice)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "片段配音任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// ServeSegmentAudio GET /videos/:id/shots/:shot_id/segments/:seg_id/audio
// 提供语音段落的音频文件（file:// 本地文件直接 serve；http(s):// 重定向）
func (h *VideoHandler) ServeSegmentAudio(c *gin.Context) {
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	seg, err := h.videoService.GetVoiceSegment(uint(segID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "segment not found")
		return
	}
	if seg.AudioPath == "" {
		respondErr(c, http.StatusNotFound, "no audio generated")
		return
	}
	if strings.HasPrefix(seg.AudioPath, "file://") {
		path := strings.TrimPrefix(seg.AudioPath, "file://")
		c.Header("Content-Type", "audio/mpeg")
		c.File(path)
		return
	}
	c.Redirect(http.StatusFound, seg.AudioPath)
}

// ─────────────────────────────────────────────────────────────────────────────
// 分镜插入 / 复制 / 删除
// ─────────────────────────────────────────────────────────────────────────────

// InsertShot POST /videos/:id/shots/insert
// body: { after_shot_no: int, narration, description, duration }
func (h *VideoHandler) InsertShot(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req struct {
		AfterShotNo int     `json:"after_shot_no"`
		Narration   string  `json:"narration"`
		Description string  `json:"description"`
		Duration    float64 `json:"duration"`
	}
	if !bindJSON(c, &req) {
		return
	}
	shot, err := h.videoService.InsertShot(uint(videoID), req.AfterShotNo, req.Narration, req.Description, req.Duration)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": shot})
}

// CopyShot POST /videos/:id/shots/:shot_id/copy
// body: { after_shot_no?: int }  (-1 or omitted = right after source shot)
func (h *VideoHandler) CopyShot(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var req struct {
		AfterShotNo int `json:"after_shot_no"`
	}
	req.AfterShotNo = -1 // default: right after source
	_ = c.ShouldBindJSON(&req)

	shot, err := h.videoService.CopyShotAfter(uint(shotID), req.AfterShotNo)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": shot})
}

// DeleteShot DELETE /videos/:id/shots/:shot_id
func (h *VideoHandler) DeleteShot(c *gin.Context) {
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	if err := h.videoService.DeleteShot(uint(shotID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// ListShotSFXItems GET /videos/:id/shots/:shot_id/sfx-items
func (h *VideoHandler) ListShotSFXItems(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	shotID, err := strconv.Atoi(c.Param("shot_id"))
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	items, err := h.sfxItemRepo.ListByShotID(uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list sfx items")
		return
	}
	respondOK(c, items)
}

// UpdateShotSFXItem PUT /videos/:id/shots/:shot_id/sfx-items/:item_id
// 支持部分更新：volume, loop_enabled, fade_in_ms, fade_out_ms, start_offset
func (h *VideoHandler) UpdateShotSFXItem(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	itemID, err := strconv.Atoi(c.Param("item_id"))
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	var req struct {
		Volume      *float64 `json:"volume"`
		LoopEnabled *bool    `json:"loop_enabled"`
		FadeInMs    *int     `json:"fade_in_ms"`
		FadeOutMs   *int     `json:"fade_out_ms"`
		StartOffset *float64 `json:"start_offset"`
	}
	if !bindJSON(c, &req) {
		return
	}
	fields := map[string]interface{}{}
	if req.Volume != nil {
		fields["volume"] = *req.Volume
	}
	if req.LoopEnabled != nil {
		fields["loop_enabled"] = *req.LoopEnabled
	}
	if req.FadeInMs != nil {
		fields["fade_in_ms"] = *req.FadeInMs
	}
	if req.FadeOutMs != nil {
		fields["fade_out_ms"] = *req.FadeOutMs
	}
	if req.StartOffset != nil {
		fields["start_offset"] = *req.StartOffset
	}
	if len(fields) == 0 {
		respondBadRequest(c, "no updatable fields provided")
		return
	}
	if err := h.sfxItemRepo.UpdateFields(uint(itemID), fields); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update sfx item")
		return
	}
	respondOK(c, fields)
}

// ToggleShotSFXItem PATCH /videos/:id/shots/:shot_id/sfx-items/:item_id/disabled
func (h *VideoHandler) ToggleShotSFXItem(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	itemID, err := strconv.Atoi(c.Param("item_id"))
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.sfxItemRepo.UpdateDisabled(uint(itemID), req.Disabled); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update sfx item")
		return
	}
	respondOK(c, gin.H{"id": itemID, "disabled": req.Disabled})
}

// DeleteShotSFXItem DELETE /videos/:id/shots/:shot_id/sfx-items/:item_id
func (h *VideoHandler) DeleteShotSFXItem(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	itemID, err := strconv.Atoi(c.Param("item_id"))
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	if err := h.sfxItemRepo.Delete(uint(itemID)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete sfx item")
		return
	}
	respondOK(c, nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// 批量配音（单任务，顺序处理，最多10个，避免TTS限流）
// ─────────────────────────────────────────────────────────────────────────────

// BatchGenerateVoice POST /videos/:id/shots/batch-voice
// 为视频所有分镜批量生成配音，作为单个异步任务处理。
// 内部每批并发 5 个，批间隔 1s，避免 TTS API 限流；已有配音的分镜自动跳过。
func (h *VideoHandler) BatchGenerateVoice(c *gin.Context) {
	const batchSize = 5

	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		NarrationVoice  string `json:"narration_voice"`
		SubtitleEnabled bool   `json:"subtitle_enabled"`
		SkipExisting    *bool  `json:"skip_existing"` // nil/true=跳过已有配音
	}
	_ = c.ShouldBindJSON(&req)
	skipExisting := req.SkipExisting == nil || *req.SkipExisting // default true

	allShots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	// 若前端未传 narration_voice，从视频配置中读取
	narrationVoice := req.NarrationVoice
	if narrationVoice == "" {
		if video, err := h.videoService.GetVideo(uint(videoID)); err == nil {
			if vc := h.videoService.GetNovelVideoConfig(video.NovelID); vc != nil {
				narrationVoice = vc.NarrationVoice
			}
		}
	}

	// 筛选需要生成配音的分镜（有文本，且未有配音或强制重生）
	var targets []*model.StoryboardShot
	for _, s := range allShots {
		if s.Narration == "" && s.Dialogue == "" && s.Description == "" {
			continue
		}
		if skipExisting && s.AudioPath != "" {
			continue
		}
		targets = append(targets, s)
	}

	if len(targets) == 0 {
		respondOK(c, gin.H{"message": "所有分镜已有配音，无需重新生成", "count": 0})
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("批量配音（%d 个分镜）", len(targets)), "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"narration_voice": narrationVoice,
	})

	go func(taskID string, shots []*model.StoryboardShot, narrationVoice string, subtitleEnabled bool) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateVoice task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		total := len(shots)
		var doneCount atomic.Int32

		for i := 0; i < total; i += batchSize {
			end := i + batchSize
			if end > total {
				end = total
			}
			batch := shots[i:end]

			var wg sync.WaitGroup
			for _, shot := range batch {
				wg.Add(1)
				go func(s *model.StoryboardShot) {
					defer wg.Done()
					if err := h.videoService.GenerateShotAudio(s, tenantID, narrationVoice); err != nil {
						logger.Printf("[VideoHandler] BatchGenerateVoice task %s shot %d failed: %v", taskID, s.ShotNo, err)
					}
					done := int(doneCount.Add(1))
					h.taskSvc.UpdateProgress(taskID, done*100/total) //nolint:errcheck
				}(shot)
			}
			wg.Wait()

			// 批次间隔 1s，避免触发 TTS API 限流
			if end < total {
				time.Sleep(1 * time.Second)
			}
		}

		// 统计最终结果
		finalShots, _ := h.videoService.GetStoryboard(uint(videoID))
		shotSet := make(map[uint]bool, len(shots))
		for _, s := range shots {
			shotSet[s.ID] = true
		}
		success, fail := 0, 0
		for _, s := range finalShots {
			if !shotSet[s.ID] {
				continue
			}
			if s.AudioPath != "" {
				success++
			} else {
				fail++
			}
		}

		h.taskSvc.Complete(taskID, gin.H{"success": success, "fail": fail, "total": total}) //nolint:errcheck
		logger.Printf("[VideoHandler] BatchGenerateVoice task %s done: success=%d fail=%d", taskID, success, fail)
	}(task.TaskID, targets, narrationVoice, req.SubtitleEnabled)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": fmt.Sprintf("批量配音任务已提交（共 %d 个分镜，每批 %d 个并发）", len(targets), batchSize),
		"data":    gin.H{"task_id": task.TaskID, "shot_count": len(targets)},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// BGM 背景音乐 AI 分析 & 生成
// ─────────────────────────────────────────────────────────────────────────────

// ListBGMSegments GET /videos/:id/bgm/segments
func (h *VideoHandler) ListBGMSegments(c *gin.Context) {
	if h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM repository not configured")
		return
	}
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	segs, err := h.bgmRepo.ListByVideoID(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, segs)
}

// JamendoSearchBGM GET /videos/:id/bgm/search
// 代理搜索 Jamendo 音乐库（避免跨域），返回器乐曲目列表供前端选择。
// 查询参数：q（模糊搜索词）、tags（精确标签，空格分隔）、speed（slow/medium/fast）、
//
//	bpm_min、bpm_max（BPM范围，0=不限）、limit（默认10，最多50）。
func (h *VideoHandler) JamendoSearchBGM(c *gin.Context) {
	if h.bgmSvc == nil {
		respondErr(c, http.StatusNotImplemented, "BGM service not configured")
		return
	}
	bpmMin, _ := strconv.Atoi(c.DefaultQuery("bpm_min", "0"))
	bpmMax, _ := strconv.Atoi(c.DefaultQuery("bpm_max", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))

	tracks, err := h.bgmSvc.JamendoSearch(c.Request.Context(), service.JamendoSearchParams{
		Query:  c.Query("q"),
		Tags:   c.Query("tags"),
		Speed:  c.Query("speed"),
		BpmMin: bpmMin,
		BpmMax: bpmMax,
		Limit:  limit,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, tracks)
}

// ApplyBGMTrack PATCH /videos/:id/bgm/segments/:seg_id/track
// 将手动选中的 Jamendo 曲目应用到指定 BGM 分段，更新 URL/track_name/track_artist/source。
func (h *VideoHandler) ApplyBGMTrack(c *gin.Context) {
	if h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM repository not configured")
		return
	}
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	var req struct {
		URL         string `json:"url" binding:"required"`
		TrackName   string `json:"track_name"`
		TrackArtist string `json:"track_artist"`
		Source      string `json:"source"`
	}
	if !bindJSON(c, &req) {
		return
	}
	src := req.Source
	if src == "" {
		src = "jamendo"
	}
	if err := h.bgmRepo.UpdateTrack(uint(segID), req.URL, req.TrackName, req.TrackArtist, src); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"seg_id": segID})
}

// UpdateBGMSegment PUT /videos/:id/bgm/segments/:seg_id
// 更新 BGM 分段通用字段（volume、ducking_enabled、ducking_level 等）。
func (h *VideoHandler) UpdateBGMSegment(c *gin.Context) {
	if h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM repository not configured")
		return
	}
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	seg, err := h.bgmRepo.GetByID(uint(segID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "BGM segment not found")
		return
	}
	var req struct {
		Volume         *float64 `json:"volume"`
		DuckingEnabled *bool    `json:"ducking_enabled"`
		DuckingLevel   *float64 `json:"ducking_level"`
		Disabled       *bool    `json:"disabled"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.Volume != nil {
		seg.Volume = *req.Volume
	}
	if req.DuckingEnabled != nil {
		seg.DuckingEnabled = *req.DuckingEnabled
	}
	if req.DuckingLevel != nil {
		seg.DuckingLevel = *req.DuckingLevel
	}
	if req.Disabled != nil {
		seg.Disabled = *req.Disabled
	}
	if err := h.bgmRepo.Update(seg); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update BGM segment")
		return
	}
	respondOK(c, seg)
}

// ToggleBGMSegment PATCH /videos/:id/bgm/segments/:seg_id/disabled
func (h *VideoHandler) ToggleBGMSegment(c *gin.Context) {
	if h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM repository not configured")
		return
	}
	segID, ok := parseID(c, "seg_id")
	if !ok {
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.bgmRepo.UpdateDisabled(uint(segID), req.Disabled); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update BGM segment")
		return
	}
	respondOK(c, gin.H{"id": segID, "disabled": req.Disabled})
}

// AnalyzeBGMSegments POST /videos/:id/bgm/analyze
// 仅执行 AI 分析（不搜索音频），返回分段计划（含搜索词）。
func (h *VideoHandler) AnalyzeBGMSegments(c *gin.Context) {
	if h.bgmSvc == nil || h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM service not configured")
		return
	}
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if len(shots) == 0 {
		respondBadRequest(c, "no shots found for this video")
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, "bgm_analyze",
		"BGM分段分析", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	var bgmReq struct {
		UserPrompt string `json:"user_prompt"`
	}
	_ = c.ShouldBindJSON(&bgmReq)
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"user_prompt": bgmReq.UserPrompt,
	})

	go func(taskID string, userPrompt string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] AnalyzeBGMSegments task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.AnalyzeBGMForVideo(ctx, shots, h.bgmRepo, uint(videoID), tenantID, userPrompt)
		if err != nil {
			logger.Printf("[VideoHandler] AnalyzeBGMSegments task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"count": len(segs)}) //nolint:errcheck
	}(task.TaskID, bgmReq.UserPrompt)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "BGM分段分析任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GenerateBGM POST /videos/:id/bgm/generate
// AI分析 + Jamendo搜索，一步完成所有BGM分段。
func (h *VideoHandler) GenerateBGM(c *gin.Context) {
	if h.bgmSvc == nil || h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM service not configured")
		return
	}
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if len(shots) == 0 {
		respondBadRequest(c, "no shots found for this video")
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, "bgm_generate",
		"BGM背景音乐生成", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	var bgmGenReq struct {
		UserPrompt string `json:"user_prompt"`
	}
	_ = c.ShouldBindJSON(&bgmGenReq)
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"user_prompt": bgmGenReq.UserPrompt,
	})

	go func(taskID string, userPrompt string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateBGM task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.GenerateBGMSegments(ctx, shots, h.bgmRepo, uint(videoID), tenantID, userPrompt, progressFn)
		if err != nil {
			logger.Printf("[VideoHandler] GenerateBGM task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		matched := 0
		for _, s := range segs {
			if s.URL != "" {
				matched++
			}
		}
		h.taskSvc.Complete(taskID, gin.H{"total": len(segs), "matched": matched}) //nolint:errcheck
		logger.Printf("[VideoHandler] GenerateBGM task %s done: total=%d matched=%d", taskID, len(segs), matched)
	}(task.TaskID, bgmGenReq.UserPrompt)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "BGM生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}
