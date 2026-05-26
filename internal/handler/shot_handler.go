package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

func (h *VideoHandler) GenerateSingleShot(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var req struct {
		Provider string `json:"provider"`
	}
	c.ShouldBindJSON(&req) //nolint:errcheck — optional body

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("镜头 #%d 素材生成", shotID), "shot", uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateSingleShot task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		shot, genErr := h.videoService.GenerateSingleShot(uint(videoID), uint(shotID), req.Provider)
		if genErr != nil {
			logger.Printf("[VideoHandler] GenerateSingleShot task %s failed: %v", taskID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90)                                         //nolint:errcheck
		h.taskSvc.Complete(taskID, gin.H{"shot_id": shot.ID, "status": shot.Status}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "素材生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// BatchGenerateShots 批量生成分镜素材（异步任务模式，立即返回 task_id）
// POST /api/v1/videos/:id/shots/batch-generate
func (h *VideoHandler) BatchGenerateShots(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头素材", len(req.ShotIDs)), "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateShots task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		shots, genErr := h.videoService.BatchGenerateShots(uint(videoID), req.ShotIDs, req.QualityTier, progressFn, req.Provider)
		if genErr != nil {
			logger.Printf("[VideoHandler] BatchGenerateShots task %s failed: %v", taskID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_count": len(shots)}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "批量素材生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// BatchGenerateShotImages POST /videos/:id/shots/batch-images
// 批量为分镜生成参考图片（阶段一）。已有图片的分镜自动跳过（幂等）。
func (h *VideoHandler) BatchGenerateShotImages(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头图片", len(req.ShotIDs)), "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateShotImages task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		shots, genErr := h.videoService.BatchGenerateShotImages(uint(videoID), req.ShotIDs, progressFn)
		if genErr != nil {
			logger.Printf("[VideoHandler] BatchGenerateShotImages task %s failed: %v", taskID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_count": len(shots)}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "批量图片生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// BatchGenerateShotClips POST /videos/:id/shots/batch-clips
// 批量为已有图片的分镜生成 Ken Burns 动效视频（阶段二）。已有视频的分镜自动跳过（幂等）。
func (h *VideoHandler) BatchGenerateShotClips(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.BatchGenerateShotsRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeAssetGen,
		fmt.Sprintf("批量生成 %d 个镜头视频", len(req.ShotIDs)), "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateShotClips task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		shots, genErr := h.videoService.BatchGenerateShotClips(uint(videoID), req.ShotIDs, progressFn)
		if genErr != nil {
			logger.Printf("[VideoHandler] BatchGenerateShotClips task %s failed: %v", taskID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_count": len(shots)}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "批量视频生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// RefineShotImage POST /videos/:id/shots/:shot_id/refine-image
// 基于用户修改建议重新生成分镜图片（同步，直接返回新图片 URL）。
func (h *VideoHandler) RefineShotImage(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	var req struct {
		Suggestion string `json:"suggestion"`
	}
	if !bindJSON(c, &req) {
		return
	}

	newURL, err := h.videoService.RefineShotImage(uint(shotID), req.Suggestion)
	if err != nil {
		logger.Printf("[VideoHandler] RefineShotImage shot %d failed: %v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "图片重新生成失败，请重试")
		return
	}
	respondOK(c, gin.H{"image_url": newURL})
}

// BatchGenerateSFX POST /videos/:id/shots/sfx
// 为视频所有分镜批量自动生成音效（异步任务）。
// 已有 sfx_url 的分镜自动跳过（幂等）。
func (h *VideoHandler) BatchGenerateSFX(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	tenantID := getTenantID(c)

	shots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil || len(shots) == 0 {
		respondErr(c, http.StatusNotFound, "storyboard not found or empty")
		return
	}

	var sfxReq struct {
		UserContext string `json:"user_context"`
	}
	_ = c.ShouldBindJSON(&sfxReq)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSFXGen, "自动音效生成", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}

	go func(taskID string, userContext string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateSFX task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)        //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5) //nolint:errcheck
		ctx := context.Background()
		// Step 1: AI 批量分析所有分镜，生成精准的自然语言音效搜索词
		if err := h.sfxSvc.AnalyzeSFXForVideo(ctx, shots, tenantID, userContext); err != nil {
			logger.Printf("[VideoHandler] BatchGenerateSFX task %s: AI analyze failed (proceeding): %v", taskID, err)
		}
		h.taskSvc.UpdateProgress(taskID, 20) //nolint:errcheck
		// Step 2: 用更新后的 sfx_tags 搜索/生成实际音效文件
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, 20+pct*80/100) } //nolint:errcheck
		success, fail := h.sfxSvc.BatchAutoGenerateSFX(ctx, shots, tenantID, userContext, progressFn)
		h.taskSvc.Complete(taskID, gin.H{"success": success, "fail": fail}) //nolint:errcheck
		logger.Printf("[VideoHandler] BatchGenerateSFX task %s done: success=%d fail=%d", taskID, success, fail)
	}(task.TaskID, sfxReq.UserContext)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "音效生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// AnalyzeSFXTags POST /videos/:id/shots/sfx-tags
// 用 AI 批量分析分镜脚本，为每个镜头生成精准的自然语言音效搜索词，写入 sfx_tags 字段。
// 仅更新标签，不搜索/生成实际音频文件。
func (h *VideoHandler) AnalyzeSFXTags(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	tenantID := getTenantID(c)

	shots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil || len(shots) == 0 {
		respondErr(c, http.StatusNotFound, "storyboard not found or empty")
		return
	}

	var sfxTagsReq struct {
		UserContext string `json:"user_context"`
	}
	_ = c.ShouldBindJSON(&sfxTagsReq)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSFXGen, "AI 音效标签分析", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}

	go func(taskID string, userContext string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] AnalyzeSFXTags task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		if err := h.sfxSvc.AnalyzeSFXForVideo(ctx, shots, tenantID, userContext); err != nil {
			logger.Printf("[VideoHandler] AnalyzeSFXTags task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"count": len(shots)}) //nolint:errcheck
	}(task.TaskID, sfxTagsReq.UserContext)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "AI 音效分析任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GenerateShotSFX POST /videos/:id/shots/:shot_id/sfx
// 为单个分镜生成音效（异步任务）。
func (h *VideoHandler) GenerateShotSFX(c *gin.Context) {
	if h.sfxSvc == nil {
		respondErr(c, http.StatusNotImplemented, "SFX service not configured")
		return
	}
	videoID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	shotID, err := strconv.Atoi(c.Param("shot_id"))
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	tenantID := getTenantID(c)

	shot, err := h.videoService.GetShotByID(uint(videoID), uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "shot not found")
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSFXGen, "单镜头音效生成", "shot", uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}

	go func(taskID string, s *model.StoryboardShot) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateShotSFX task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		if err := h.sfxSvc.AutoGenerateSFX(ctx, s, tenantID); err != nil {
			logger.Printf("[VideoHandler] GenerateShotSFX task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_id": s.ID, "sfx_url": s.SFXURL}) //nolint:errcheck
	}(task.TaskID, shot)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "音效生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GenerateShotVoice 为单个分镜异步生成配音
// POST /api/v1/videos/:id/storyboard/:shot_id/voice
// 立即返回 202 + task_id，轮询 GET /api/v1/tasks/:task_id 获取结果
func (h *VideoHandler) GenerateShotVoice(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}

	shot, err := h.videoService.GetShotByID(uint(videoID), uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	if shot.Narration == "" && shot.Dialogue == "" && shot.Description == "" {
		respondBadRequest(c, "shot has no text content")
		return
	}

	var req struct {
		NarrationVoice  string `json:"narration_voice"`
		SubtitleEnabled bool   `json:"subtitle_enabled"`
		// SubtitleConfig 字幕样式参数（当前已解析，暂未持久化至 SRT；规划中实现 ASS 样式输出）
		SubtitleConfig struct {
			Position string `json:"position"`
			FontSize int    `json:"font_size"`
			Color    string `json:"color"`
			BgStyle  string `json:"bg_style"`
		} `json:"subtitle_config"`
	}
	_ = c.ShouldBindJSON(&req)

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("镜头 #%d 配音生成", shot.ShotNo), "shot", uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string, shot *model.StoryboardShot, narrationVoice string, subtitleEnabled bool) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateShotVoice task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		const maxRetries = 3
		var audioErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			audioErr = h.videoService.GenerateShotAudio(shot, tenantID, narrationVoice)
			if audioErr == nil {
				break
			}
			logger.Printf("[VideoHandler] GenerateShotVoice task %s shot %d attempt %d/%d failed: %v", taskID, shot.ShotNo, attempt, maxRetries, audioErr)
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt*2) * time.Second)
			}
		}
		if audioErr != nil {
			h.taskSvc.Fail(taskID, audioErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90) //nolint:errcheck

		result := gin.H{"audio_url": shot.AudioPath, "shot_id": shot.ID}
		if subtitleEnabled {
			srt := service.GenerateShotSRT(shot)
			if srt != "" {
				result["subtitle_srt"] = srt
			}
		}
		h.taskSvc.Complete(taskID, result) //nolint:errcheck
	}(task.TaskID, shot, req.NarrationVoice, req.SubtitleEnabled)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "配音生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GetDefaultConsistencyConfig 获取默认一致性配置
// GET /api/v1/consistency/default
func (h *VideoHandler) GetDefaultConsistencyConfig(c *gin.Context) {
	if h.consistencyService == nil {
		respondErr(c, http.StatusServiceUnavailable, "consistency service unavailable")
		return
	}
	level := h.consistencyService.GetDefaultConsistencyLevel()
	respondOK(c, level)
}

// CalculateConsistencyScore 计算一致性评分
// POST /api/v1/consistency/score
func (h *VideoHandler) CalculateConsistencyScore(c *gin.Context) {
	if h.consistencyService == nil {
		respondErr(c, http.StatusServiceUnavailable, "consistency service unavailable")
		return
	}

	var req struct {
		ReferenceImage  string   `json:"reference_image"`
		GeneratedImages []string `json:"generated_images"`
	}
	if !bindJSON(c, &req) {
		return
	}

	score, err := h.consistencyService.CalculateConsistencyScore(req.ReferenceImage, req.GeneratedImages)
	if err != nil {
		logger.Printf("[VideoHandler] CalculateConsistencyScore: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, score)
}

// ExportCapCutDraft 导出剪映草稿 ZIP
// GET /api/v1/videos/:id/export/capcut
func (h *VideoHandler) ExportCapCutDraft(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	novel, _ := h.videoService.GetNovelByID(video.NovelID) // 用于字幕样式配置，失败不阻断导出
	var bgmSegs []*model.VideoBGMSegment
	if h.bgmRepo != nil {
		bgmSegs, _ = h.bgmRepo.ListByVideoID(uint(id))
	}

	result, err := h.capcutService.ExportCapCutDraft(video, shots, novel, bgmSegs)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	c.Header("Content-Length", strconv.Itoa(len(result.Data)))
	c.Data(http.StatusOK, result.ContentType, result.Data)
}

// Export 多格式导出
// GET /api/v1/videos/:id/export/:format
// format: capcut | fcpxml | zip | srt
func (h *VideoHandler) Export(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	format := c.Param("format")

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id))
	if err != nil {
		logger.Printf("[VideoHandler] Export: videoID=%d get storyboard err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	var result *service.ExportResult
	switch format {
	case "fcpxml":
		result, err = h.capcutService.ExportFCPXML(video, shots)
	case "zip":
		result, err = h.capcutService.ExportResourceZip(video, shots)
	case "srt":
		result, err = h.capcutService.ExportSRT(video, shots)
	case "vtt":
		result, err = h.capcutService.ExportVTT(video, shots)
	case "edl":
		result, err = h.capcutService.ExportEDL(video, shots)
	case "otio":
		result, err = h.capcutService.ExportOTIO(video, shots)
	case "csv":
		result, err = h.capcutService.ExportCSV(video, shots)
	default: // "capcut" 或其他
		novel, _ := h.videoService.GetNovelByID(video.NovelID)
		var bgmSegs []*model.VideoBGMSegment
		if h.bgmRepo != nil {
			bgmSegs, _ = h.bgmRepo.ListByVideoID(uint(id))
		}
		result, err = h.capcutService.ExportCapCutDraft(video, shots, novel, bgmSegs)
	}

	if err != nil {
		logger.Printf("[VideoHandler] Export: videoID=%d format=%s err=%v", id, format, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	logger.Printf("[VideoHandler] Export: videoID=%d format=%s filename=%s size=%d", id, format, result.Filename, len(result.Data))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	c.Header("Content-Length", strconv.Itoa(len(result.Data)))
	c.Data(http.StatusOK, result.ContentType, result.Data)
}

// ─────────────────────────────────────────────────────────────────────────────
// 分镜语音段落 (VoiceSegment) 处理器
// ─────────────────────────────────────────────────────────────────────────────

// ListVoiceSegments GET /videos/:id/shots/:shot_id/segments
