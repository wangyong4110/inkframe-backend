package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
				logger.Errorf("[VideoHandler] GenerateSegmentVoice task %s panic: %v", taskID, r)
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

// ImportShotSFXItem POST /videos/:id/shots/:shot_id/sfx-items
// 手动导入音效条目，支持三种方式：
//  1. application/json + {"asset_id": N}: 从素材库导入（复用已有音效）
//  2. application/json + {"url": "..."}:   直接提供音频 URL
//  3. multipart/form-data + file 字段:     上传本地音频文件（.mp3/.wav/.ogg/.m4a/.flac）
func (h *VideoHandler) ImportShotSFXItem(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var audioURL, tag, sfxType, source string
	var volume float64 = 0.4
	var durationSecs float64

	ct := c.ContentType()
	if strings.HasPrefix(ct, "multipart/form-data") {
		// ── 方式3：上传本地音频文件 ────────────────────────────────────────────
		if h.storageSvc == nil {
			respondErr(c, http.StatusNotImplemented, "storage not configured")
			return
		}
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			respondBadRequest(c, "no file uploaded (field: file)")
			return
		}
		defer file.Close()

		allowedExts := map[string]string{
			".mp3":  "audio/mpeg",
			".wav":  "audio/wav",
			".ogg":  "audio/ogg",
			".m4a":  "audio/mp4",
			".flac": "audio/flac",
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		mimeType, ok2 := allowedExts[ext]
		if !ok2 {
			respondBadRequest(c, "unsupported audio format: "+ext+" (allowed: mp3, wav, ogg, m4a, flac)")
			return
		}
		objectKey := fmt.Sprintf("sfx/import/%s%s", uuid.New().String(), ext)
		uploadedURL, err := h.storageSvc.Upload(c.Request.Context(), objectKey, file, header.Size, mimeType)
		if err != nil {
			respondErr(c, http.StatusInternalServerError, "upload failed: "+err.Error())
			return
		}
		audioURL = uploadedURL
		tag = c.PostForm("tag")
		sfxType = c.PostForm("sfx_type")
		source = "upload"
		if v := c.PostForm("volume"); v != "" {
			fmt.Sscanf(v, "%f", &volume)
		}
	} else {
		// ── JSON 模式 ─────────────────────────────────────────────────────────
		var req struct {
			AssetID uint    `json:"asset_id"` // 方式1：素材库
			URL     string  `json:"url"`      // 方式2：直接 URL
			Tag     string  `json:"tag"`
			SFXType string  `json:"sfx_type"`
			Volume  float64 `json:"volume"`
		}
		if !bindJSON(c, &req) {
			return
		}

		if req.AssetID != 0 {
			// ── 方式1：从素材库导入 ──────────────────────────────────────────
			if h.assetRepo == nil {
				respondErr(c, http.StatusNotImplemented, "asset repo not configured")
				return
			}
			asset, err := h.assetRepo.GetByID(req.AssetID)
			if err != nil {
				respondErr(c, http.StatusNotFound, "asset not found")
				return
			}
			if asset.MediaMeta.StorageURL == "" {
				respondBadRequest(c, "asset has no audio URL")
				return
			}
			audioURL = asset.MediaMeta.StorageURL
			durationSecs = asset.MediaMeta.Duration
			if req.Tag != "" {
				tag = req.Tag
			} else {
				tag = asset.Title
			}
			source = "asset_lib"
			// 导入后增加使用计数
			_ = h.assetRepo.IncrUseCount(req.AssetID)
		} else {
			// ── 方式2：直接提供 URL ──────────────────────────────────────────
			if req.URL == "" {
				respondBadRequest(c, "asset_id or url is required")
				return
			}
			audioURL = req.URL
			source = "import"
		}

		// tag 已在各分支内设置；仅覆写 sfx_type / volume
		if tag == "" {
			tag = req.Tag
		}
		sfxType = req.SFXType
		if req.Volume > 0 {
			volume = req.Volume
		}
	}

	if audioURL == "" {
		respondBadRequest(c, "audio URL is required")
		return
	}
	if sfxType == "" {
		sfxType = "action"
	}
	if volume <= 0 || volume > 1 {
		volume = 0.4
	}

	item := &model.ShotSFXItem{
		ShotID:       shotID,
		Tag:          tag,
		URL:          audioURL,
		Volume:       volume,
		Source:       source,
		SFXType:      sfxType,
		DurationSecs: durationSecs,
	}
	if err := h.sfxItemRepo.AppendAtomic(item); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create sfx item")
		return
	}
	proxyURL := ""
	if item.URL != "" {
		proxyURL = fmt.Sprintf("/api/v1/sfx-items/%d/audio", item.ID)
	}
	respondCreated(c, sfxItemDTO{ShotSFXItem: item, AudioURL: proxyURL})
}

// sfxItemDTO wraps ShotSFXItem and adds a computed audio_url field for browser playback.
// file:// URLs (local server files) are replaced with a backend proxy URL.
type sfxItemDTO struct {
	*model.ShotSFXItem
	AudioURL string `json:"audio_url"`
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
	dtos := make([]sfxItemDTO, len(items))
	for i, item := range items {
		audioURL := item.URL
		if item.URL != "" {
			// 统一走后端代理，避免 OSS/CDN CORS 问题
			audioURL = fmt.Sprintf("/api/v1/sfx-items/%d/audio", item.ID)
		}
		dtos[i] = sfxItemDTO{ShotSFXItem: item, AudioURL: audioURL}
	}
	respondOK(c, dtos)
}

// ServeSFXItemAudio GET /api/v1/sfx-items/:item_id/audio
// 代理播放 file:// 本地音效文件（ElevenLabs/AudioLDM 生成但 OSS 上传失败时的兜底）。
func (h *VideoHandler) ServeSFXItemAudio(c *gin.Context) {
	if h.sfxItemRepo == nil {
		respondErr(c, http.StatusNotImplemented, "SFX item repo not configured")
		return
	}
	itemID, err := strconv.Atoi(c.Param("item_id"))
	if err != nil {
		respondBadRequest(c, "invalid item id")
		return
	}
	item, err := h.sfxItemRepo.GetByID(uint(itemID))
	if err != nil || item == nil {
		respondErr(c, http.StatusNotFound, "sfx item not found")
		return
	}
	if strings.HasPrefix(item.URL, "http://") || strings.HasPrefix(item.URL, "https://") {
		// 服务端代理，避免浏览器直接访问 OSS/CDN 时的 CORS 问题
		proxySFXAudio(c, item.URL)
		return
	}
	if !strings.HasPrefix(item.URL, "file://") {
		c.Redirect(http.StatusFound, item.URL)
		return
	}
	filePath := strings.TrimPrefix(item.URL, "file://")
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.Header("Content-Type", "audio/mpeg")
	c.File(filePath)
}

// proxySFXAudio 服务端代理远程音频，支持 Range 请求（拖动进度条），消除 CORS。
func proxySFXAudio(c *gin.Context, remoteURL string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		c.Status(http.StatusBadGateway)
		return
	}
	if rangeH := c.GetHeader("Range"); rangeH != "" {
		req.Header.Set("Range", rangeH)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorf("[ServeSFXItemAudio] proxy fetch failed url=%s: %v", remoteURL, err)
		c.Status(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 透传关键响应头
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	} else {
		c.Header("Content-Type", "audio/mpeg")
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		c.Header("Content-Range", cr)
	}
	c.Header("Accept-Ranges", "bytes")
	c.Header("Cache-Control", "public, max-age=3600")

	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

// UpdateShotSFXItem PUT /videos/:id/shots/:shot_id/sfx-items/:item_id
// 支持部分更新：volume, loop_enabled, fade_in_ms, fade_out_ms, start_offset, play_count
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
		PlayCount   *int     `json:"play_count"`
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
	if req.PlayCount != nil {
		fields["play_count"] = *req.PlayCount
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

// ReorderShots POST /videos/:id/shots/reorder
// 交换两个分镜的 shot_no，实现拖拽排序。
// body: { from_shot_id: int, to_shot_id: int }
func (h *VideoHandler) ReorderShots(c *gin.Context) {
	var req struct {
		FromShotID uint `json:"from_shot_id" binding:"required"`
		ToShotID   uint `json:"to_shot_id" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	fromShotNo, toShotNo, err := h.videoService.ReorderShots(req.FromShotID, req.ToShotID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"from_shot_no": fromShotNo, "to_shot_no": toShotNo})
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
				narrationVoice = vc.Config.NarrationVoice
			}
		}
	}

	// 筛选需要生成配音的分镜（有文本，且未有配音或强制重生）
	var targets []*model.StoryboardShot
	for _, s := range allShots {
		if s.Narration == "" && s.GenMeta.Dialogue == "" && s.Description == "" {
			continue
		}
		if skipExisting {
			// Skip shots that already have audio in their voice segments
			segs, _ := h.videoService.ListVoiceSegments(s.ID)
			hasAudio := false
			for _, seg := range segs {
				if seg.AudioPath != "" {
					hasAudio = true
					break
				}
			}
			if hasAudio {
				continue
			}
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
				logger.Errorf("[VideoHandler] BatchGenerateVoice task %s panic: %v", taskID, r)
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
						logger.Errorf("[VideoHandler] BatchGenerateVoice task %s shot %d failed: %v", taskID, s.ShotNo, err)
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
			segs, _ := h.videoService.ListVoiceSegments(s.ID)
			hasAudio := false
			for _, seg := range segs {
				if seg.AudioPath != "" {
					hasAudio = true
					break
				}
			}
			if hasAudio {
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
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
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
	tenantID := getTenantID(c)
	bpmMin, _ := strconv.Atoi(c.DefaultQuery("bpm_min", "0"))
	bpmMax, _ := strconv.Atoi(c.DefaultQuery("bpm_max", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))

	tracks, err := h.bgmSvc.JamendoSearch(c.Request.Context(), tenantID, service.JamendoSearchParams{
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
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
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
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
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
		seg.Ducking.Enabled = *req.DuckingEnabled
	}
	if req.DuckingLevel != nil {
		seg.Ducking.Level = *req.DuckingLevel
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
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
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

// ProxyBGMAudio GET /videos/:id/bgm/proxy?url=<encoded>
// 代理播放 BGM 音频（Jamendo CDN / OSS），解决前端跨域限制。
// 支持 Range 请求（音频 seek）；仅允许 https:// 地址，禁止内网 IP。
func (h *VideoHandler) ProxyBGMAudio(c *gin.Context) {
	rawURL := c.Query("url")
	if rawURL == "" {
		respondBadRequest(c, "url parameter required")
		return
	}
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		respondBadRequest(c, "only http/https URLs are allowed")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid url")
		return
	}
	// 透传 Range 头，支持音频 seek
	if rng := c.GetHeader("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", "InkFrame-BGMProxy/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf("[BGMProxy] fetch %s failed: %v", rawURL, err)
		respondErr(c, http.StatusBadGateway, "failed to fetch audio")
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "audio/mpeg"
	}
	// 透传必要的响应头
	for _, h2 := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h2); v != "" {
			c.Header(h2, v)
		}
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.DataFromReader(resp.StatusCode, resp.ContentLength, ct, resp.Body, nil)
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
				logger.Errorf("[VideoHandler] AnalyzeBGMSegments task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.AnalyzeBGMForVideo(ctx, shots, h.bgmRepo, uint(videoID), tenantID, userPrompt)
		if err != nil {
			logger.Errorf("[VideoHandler] AnalyzeBGMSegments task %s failed: %v", taskID, err)
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
				logger.Errorf("[VideoHandler] GenerateBGM task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.GenerateBGMSegments(ctx, shots, h.bgmRepo, uint(videoID), tenantID, userPrompt, progressFn)
		if err != nil {
			logger.Errorf("[VideoHandler] GenerateBGM task %s failed: %v", taskID, err)
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

// MergeVoiceSegments POST /videos/:id/shots/:shot_id/voice/merge
// 将已生成的多段配音合并为单个音轨并更新分镜 audio_path。
func (h *VideoHandler) MergeVoiceSegments(c *gin.Context) {
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	audioURL, err := h.videoService.MergeVoiceSegments(c.Request.Context(), uint(shotID), getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"audio_url": audioURL})
}
