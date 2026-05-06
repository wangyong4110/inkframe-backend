package handler

import (
	"context"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// VideoHandler 视频处理器
type VideoHandler struct {
	videoService       *service.VideoService
	storyboardService  *service.StoryboardService
	enhancementService *service.VideoEnhancementService
	consistencyService *service.CharacterConsistencyService
	capcutService      *service.CapCutService
	taskSvc            *service.TaskService
	sfxSvc             *service.SFXService
	sfxItemRepo        *repository.ShotSFXItemRepository
	bgmSvc             *service.BGMService
	bgmRepo            *repository.VideoBGMSegmentRepository
}

func (h *VideoHandler) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *VideoHandler {
	h.sfxItemRepo = r
	return h
}

func (h *VideoHandler) WithBGMService(svc *service.BGMService) *VideoHandler {
	h.bgmSvc = svc
	return h
}

func (h *VideoHandler) WithBGMRepo(r *repository.VideoBGMSegmentRepository) *VideoHandler {
	h.bgmRepo = r
	return h
}

func NewVideoHandler(
	videoService *service.VideoService,
	storyboardService *service.StoryboardService,
	enhancementService *service.VideoEnhancementService,
	consistencyService *service.CharacterConsistencyService,
) *VideoHandler {
	return &VideoHandler{
		videoService:       videoService,
		storyboardService:  storyboardService,
		enhancementService: enhancementService,
		consistencyService: consistencyService,
		capcutService:      service.NewCapCutService(),
	}
}

func (h *VideoHandler) WithTaskService(svc *service.TaskService) *VideoHandler {
	h.taskSvc = svc
	return h
}

func (h *VideoHandler) WithSFXService(svc *service.SFXService) *VideoHandler {
	h.sfxSvc = svc
	return h
}

// getVideoForTenant 提取租户鉴权公共逻辑，减少重复代码。
// 返回 false 时已向 c 写入错误响应，调用方直接 return。
func (h *VideoHandler) getVideoForTenant(c *gin.Context, id uint) (*model.Video, bool) {
	video, err := h.videoService.GetVideoByTenant(id, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found")
		return nil, false
	}
	return video, true
}

// ListVideoProviders 列出已配置的视频生成提供者
// GET /api/v1/videos/providers
func (h *VideoHandler) ListVideoProviders(c *gin.Context) {
	respondOK(c, h.videoService.ListVideoProviders())
}

// CreateVideo 创建视频项目
// POST /api/v1/novels/:novel_id/videos
func (h *VideoHandler) CreateVideo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	var req model.CreateVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	video, err := h.videoService.CreateVideo(uint(novelId), &req)
	if err != nil {
		logger.Printf("[VideoHandler] CreateVideo: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, video)
}

// GetVideo 获取视频详情
// GET /api/v1/videos/:id
func (h *VideoHandler) GetVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	respondOK(c, video)
}

// ListVideos 获取视频列表
// GET /api/v1/videos?novel_id=&chapter_id=&status=
// GET /api/v1/novels/:id/videos?chapter_id=&status=
func (h *VideoHandler) ListVideos(c *gin.Context) {
	// novel_id: 优先读路径参数（来自 /novels/:id/videos 路由），其次读查询参数
	var novelId *uint
	if pathId := c.Param("id"); pathId != "" && pathId != "/" {
		if id, err := strconv.ParseUint(pathId, 10, 32); err == nil {
			novelId = new(uint)
			*novelId = uint(id)
		}
	}
	if novelId == nil {
		if q := c.Query("novel_id"); q != "" {
			if id, err := strconv.ParseUint(q, 10, 32); err == nil {
				novelId = new(uint)
				*novelId = uint(id)
			}
		}
	}

	var chapterID *uint
	if q := c.Query("chapter_id"); q != "" {
		if id, err := strconv.ParseUint(q, 10, 32); err == nil {
			chapterID = new(uint)
			*chapterID = uint(id)
		}
	}

	status := c.Query("status")
	p := parsePagination(c)

	videos, total, err := h.videoService.ListVideos(novelId, chapterID, status, p.Page, p.PageSize)
	if err != nil {
		logger.Printf("[VideoHandler] ListVideos: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"items":      videos,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (total + p.PageSize - 1) / p.PageSize,
	})
}

// UpdateVideo 更新视频
// PUT /api/v1/videos/:id
func (h *VideoHandler) UpdateVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req model.UpdateVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	video, err := h.videoService.UpdateVideo(uint(id), &req)
	if err != nil {
		logger.Printf("[VideoHandler] UpdateVideo: id=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, video)
}

// DeleteVideo 删除视频
// DELETE /api/v1/videos/:id
func (h *VideoHandler) DeleteVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	if err := h.videoService.DeleteVideo(uint(id)); err != nil {
		logger.Printf("[VideoHandler] DeleteVideo: id=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateStoryboard 生成分镜（异步任务）
// POST /api/v1/videos/:id/storyboard/generate
// 立即返回 202 + task_id，轮询 GET /:id/storyboard/generate/:task_id 获取结果
func (h *VideoHandler) GenerateStoryboard(c *gin.Context) {
	videoId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req struct {
		ChapterID      uint     `json:"chapter_id"`
		Characters     []string `json:"characters"`
		Style          string   `json:"style,omitempty"`
		Provider       string   `json:"provider,omitempty"`         // 指定 LLM 提供者，可为空
		UserPrompt     string   `json:"user_prompt,omitempty"`      // 用户自定义提示词
		Pacing         string   `json:"pacing,omitempty"`           // slow/normal/fast
		TargetDuration int      `json:"target_duration,omitempty"`  // 0=自动估算
		MaxTokens      int      `json:"max_tokens,omitempty"`       // 0=使用系统默认
		Temperature    float64  `json:"temperature,omitempty"`      // 0=使用系统默认
		TimeoutSeconds int      `json:"timeout_seconds,omitempty"`  // 0=使用系统默认(180s)
		VoiceMode      string   `json:"voice_mode,omitempty"`       // ""/"both"=对白+旁白, "narration"=仅旁白, "dialogue"=仅对白
	}
	// 所有字段均可选，body 为空时忽略 EOF
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}

	// 若请求携带节奏/时长配置，持久化到 Video 记录，后续 GenerateStoryboard 读取
	if req.Pacing != "" || req.TargetDuration != 0 {
		if err := h.videoService.UpdatePacingConfig(uint(videoId), req.Pacing, req.TargetDuration); err != nil {
			logger.Printf("[VideoHandler] UpdatePacingConfig failed (non-fatal): %v", err)
		}
	}

	tenantID := getTenantID(c)
	// 若该视频已有 pending/running 的分镜生成任务，先将其标记为 cancelled，
	// 让僵尸 goroutine 的 Complete/Fail 调用自动变 no-op，避免状态污染。
	h.taskSvc.CancelActiveByEntity("video", uint(videoId), service.TaskTypeStoryboardGen)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardGen, "分镜脚本生成", "video", uint(videoId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateStoryboard task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck

		overrides := service.StoryboardOverrides{
			MaxTokens:      req.MaxTokens,
			Temperature:    req.Temperature,
			TimeoutSeconds: req.TimeoutSeconds,
			VoiceMode:      req.VoiceMode,
		}
		result, err := h.storyboardService.GenerateStoryboard(uint(videoId), req.ChapterID, req.Characters, req.Style, req.Provider, req.UserPrompt, progressFn, overrides)
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			logger.Printf("[VideoHandler] GenerateStoryboard task %s failed: %v", taskID, err)
			return
		}
		// 只存 shot_count，不把完整分镜数组写入 result 列（JSON 可能超出 TEXT 65KB 限制导致 Update 失败，任务永远卡在 99%）
		var shotCount int
		if shots, ok := result.([]*model.StoryboardShot); ok {
			shotCount = len(shots)
		}
		h.taskSvc.Complete(taskID, gin.H{"shot_count": shotCount}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// shotWithAudio 在分镜基础上增加可直接播放的 audio_url 字段
type shotWithAudio struct {
	*model.StoryboardShot
	AudioURL string `json:"audio_url"`
}

// resolveAudioURL 将 AudioPath 转换为前端可用的 URL：
// - file:// → 指向后端 serve 端点（/api/v1/videos/:id/storyboard/:shot_id/audio）
// - http(s):// → 原样返回
// - 空 → 返回空字符串
func resolveAudioURL(videoID uint, shot *model.StoryboardShot) string {
	if shot.AudioPath == "" {
		return ""
	}
	if strings.HasPrefix(shot.AudioPath, "file://") {
		return fmt.Sprintf("/api/v1/videos/%d/storyboard/%d/audio", videoID, shot.ID)
	}
	return shot.AudioPath
}

// ReviewStoryboard 对分镜脚本进行 AI 专业审查（异步任务）
// POST /api/v1/videos/:id/storyboard/review
// 立即返回 202 + task_id，轮询 GET /:id/storyboard/review/:task_id 获取结果
func (h *VideoHandler) ReviewStoryboard(c *gin.Context) {
	videoId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoId)); !ok {
		return
	}

	var req struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&req) // 可选 body

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardReview, "分镜 AI 审查", "video", uint(videoId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] ReviewStoryboard task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		review, reviewErr := h.storyboardService.ReviewStoryboard(tenantID, uint(videoId), req.Provider)
		if reviewErr != nil {
			logger.Printf("[VideoHandler] ReviewStoryboard task %s failed: videoID=%d err=%v", taskID, videoId, reviewErr)
			h.taskSvc.Fail(taskID, reviewErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, review) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜审查任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GetStoryboard 获取分镜列表
// GET /api/v1/videos/:id/storyboard
func (h *VideoHandler) GetStoryboard(c *gin.Context) {
	videoId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(videoId))
	if err != nil {
		logger.Printf("[VideoHandler] GetStoryboard: videoID=%d err=%v", videoId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	result := make([]shotWithAudio, len(shots))
	for i, s := range shots {
		result[i] = shotWithAudio{
			StoryboardShot: s,
			AudioURL:       resolveAudioURL(uint(videoId), s),
		}
	}
	respondOK(c, result)
}

// ServeAudio 供前端播放配音文件
// GET /api/v1/videos/:id/storyboard/:shot_id/audio
func (h *VideoHandler) ServeAudio(c *gin.Context) {
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}

	shot, err := h.videoService.GetShot(uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "shot not found")
		return
	}

	if shot.AudioPath == "" {
		respondErr(c, http.StatusNotFound, "no audio for this shot")
		return
	}
	// HTTP/HTTPS URL（OSS 或 DB media endpoint）— 重定向
	if strings.HasPrefix(shot.AudioPath, "http://") || strings.HasPrefix(shot.AudioPath, "https://") {
		c.Redirect(http.StatusFound, shot.AudioPath)
		return
	}
	// file:// 本地路径（兼容未配置存储服务的情况）
	if strings.HasPrefix(shot.AudioPath, "file://") {
		filePath := strings.TrimPrefix(shot.AudioPath, "file://")
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.File(filePath)
		return
	}
	// /api/v1/media/:id 相对路径 — 重定向
	c.Redirect(http.StatusFound, shot.AudioPath)
}

// UpdateStoryboardShot 更新分镜（支持部分字段更新）
// PUT /api/v1/videos/:id/storyboard/:shot_id
func (h *VideoHandler) UpdateStoryboardShot(c *gin.Context) {
	shotId, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}

	var fields map[string]interface{}
	if err := c.ShouldBindJSON(&fields); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	shot, err := h.videoService.UpdateShotPartial(uint(shotId), fields)
	if err != nil {
		logger.Printf("[VideoHandler] UpdateStoryboardShot: shotID=%d err=%v", shotId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, shot)
}

// SetShotCharacters 手动绑定分镜角色
// PUT /api/v1/videos/:id/shots/:shot_id/characters
func (h *VideoHandler) SetShotCharacters(c *gin.Context) {
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	var body struct {
		CharacterIDs []uint `json:"character_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.videoService.SetShotCharacters(uint(shotID), body.CharacterIDs); err != nil {
		logger.Printf("[VideoHandler] SetShotCharacters: shotID=%d err=%v", shotID, err)
		respondErr(c, http.StatusInternalServerError, "failed to set shot characters")
		return
	}
	respondOK(c, nil)
}

// OptimizeStoryboardFromReview 根据 AI 审查报告一键优化分镜（异步任务）
// POST /api/v1/videos/:id/storyboard/optimize-from-review
// Body: StoryboardReview JSON（由 review 任务结果直接透传）+ 可选 provider
func (h *VideoHandler) OptimizeStoryboardFromReview(c *gin.Context) {
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	if _, ok := h.getVideoForTenant(c, uint(videoID)); !ok {
		return
	}

	var req struct {
		model.StoryboardReview
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "request body must contain a valid StoryboardReview: "+err.Error())
		return
	}
	if len(req.GlobalSuggestions) == 0 && len(req.ShotFeedback) == 0 {
		respondBadRequest(c, "审查报告中无改进建议，无需优化")
		return
	}

	tenantID := getTenantID(c)
	h.taskSvc.CancelActiveByEntity("video", uint(videoID), service.TaskTypeStoryboardOptimize)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardOptimize, "分镜一键优化", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	review := req.StoryboardReview
	provider := req.Provider
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] OptimizeStoryboardFromReview task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		count, optErr := h.storyboardService.OptimizeStoryboardFromReview(tenantID, uint(videoID), &review, provider)
		if optErr != nil {
			logger.Printf("[VideoHandler] OptimizeStoryboardFromReview task %s failed: %v", taskID, optErr)
			h.taskSvc.Fail(taskID, optErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"updated_shots": count}) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜优化任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// AnalyzeEmotions 情感分析
// POST /api/v1/storyboard/analyze-emotions
func (h *VideoHandler) AnalyzeEmotions(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	result, err := h.storyboardService.AnalyzeEmotions(req.Content)
	if err != nil {
		logger.Printf("[VideoHandler] AnalyzeEmotions: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// EnhanceVideo 增强视频
// POST /api/v1/video/enhance
func (h *VideoHandler) EnhanceVideo(c *gin.Context) {
	var req struct {
		VideoURL     string                    `json:"video_url" binding:"required"`
		Enhancements []model.EnhancementConfig `json:"enhancements"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	result, err := h.enhancementService.EnhanceVideo(req.VideoURL, req.Enhancements)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// GetEnhancementRecommendations 获取增强建议
// POST /api/v1/video/recommendations
func (h *VideoHandler) GetEnhancementRecommendations(c *gin.Context) {
	var req struct {
		FPS        int    `json:"fps"`
		Resolution string `json:"resolution"`
		Duration   int    `json:"duration"`
		Style      string `json:"style"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	result, err := h.enhancementService.GetRecommendations(req.FPS, req.Resolution, req.Duration, req.Style)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// StartVideoGeneration 开始视频生成
// POST /api/v1/videos/:id/generate
func (h *VideoHandler) StartVideoGeneration(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	taskId, err := h.videoService.StartGeneration(uint(id))
	if err != nil {
		logger.Printf("[VideoHandler] StartVideoGeneration: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"task_id": taskId,
	})
}

// GetVideoStatus 获取视频生成状态
// GET /api/v1/videos/:id/status
func (h *VideoHandler) GetVideoStatus(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	// 租户鉴权：确认该视频属于当前租户
	if _, ok := h.getVideoForTenant(c, uint(id)); !ok {
		return
	}

	status, err := h.videoService.GetStatus(uint(id))
	if err != nil {
		logger.Printf("[VideoHandler] GetVideoStatus: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, status)
}

// GenerateShotVideos 提交所有分镜视频生成任务，并后台轮询拼接
// POST /api/v1/videos/:id/shots/generate
func (h *VideoHandler) GenerateShotVideos(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	if err := h.videoService.GenerateAllShotVideos(uint(id)); err != nil {
		logger.Printf("[VideoHandler] GenerateShotVideos: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	// slideshow mode handles stitching internally; only poll for AI video mode
	if video.Mode != "slideshow" {
		go h.videoService.PollAndStitchVideo(uint(id))
	}

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "shot generation started",
	})
}

// ListShots 获取所有分镜状态
// GET /api/v1/videos/:id/shots
func (h *VideoHandler) ListShots(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id))
	if err != nil {
		logger.Printf("[VideoHandler] ListShots: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, shots)
}

// StitchVideoHandler 手动触发视频拼接
// POST /api/v1/videos/:id/stitch
func (h *VideoHandler) StitchVideoHandler(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	outputPath, err := h.videoService.StitchVideo(uint(id))
	if err != nil {
		logger.Printf("[VideoHandler] StitchVideo: videoID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"output_path": outputPath,
	})
}

// DownloadVideo 下载完整 MP4（拼接所有分镜后直接发送文件）
// GET /api/v1/videos/:id/download
func (h *VideoHandler) DownloadVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	// 如果已经有拼接好的文件，直接下载；否则先触发拼接
	outputPath := video.VideoPath
	if outputPath == "" {
		outputPath, err = h.videoService.StitchVideo(uint(id))
		if err != nil {
			logger.Printf("[VideoHandler] DownloadVideo stitch: videoID=%d err=%v", id, err)
			respondErr(c, http.StatusInternalServerError, "视频拼接失败")
			return
		}
	}

	filename := fmt.Sprintf("inkframe-video-%d.mp4", id)
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("Content-Type", "video/mp4")
	c.File(outputPath)
}

// GenerateSingleShot 生成单个分镜（异步任务模式，立即返回 task_id）
// POST /api/v1/videos/:id/shots/:shot_id/generate
func (h *VideoHandler) GenerateSingleShot(c *gin.Context) {
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
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
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		shot, genErr := h.videoService.GenerateSingleShot(uint(videoID), uint(shotID), req.Provider)
		if genErr != nil {
			logger.Printf("[VideoHandler] GenerateSingleShot task %s failed: %v", taskID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90) //nolint:errcheck
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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req model.BatchGenerateShotsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req model.BatchGenerateShotsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req model.BatchGenerateShotsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
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
	_, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}

	var req struct {
		Suggestion string `json:"suggestion"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSFXGen, "自动音效生成", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateSFX task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)     //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5) //nolint:errcheck
		ctx := context.Background()
		// Step 1: AI 批量分析所有分镜，生成精准的自然语言音效搜索词
		if err := h.sfxSvc.AnalyzeSFXForVideo(ctx, shots, tenantID); err != nil {
			logger.Printf("[VideoHandler] BatchGenerateSFX task %s: AI analyze failed (proceeding): %v", taskID, err)
		}
		h.taskSvc.UpdateProgress(taskID, 20) //nolint:errcheck
		// Step 2: 用更新后的 sfx_tags 搜索/生成实际音效文件
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, 20+pct*80/100) } //nolint:errcheck
		success, fail := h.sfxSvc.BatchAutoGenerateSFX(ctx, shots, tenantID, progressFn)
		h.taskSvc.Complete(taskID, gin.H{"success": success, "fail": fail}) //nolint:errcheck
		logger.Printf("[VideoHandler] BatchGenerateSFX task %s done: success=%d fail=%d", taskID, success, fail)
	}(task.TaskID)

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

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSFXGen, "AI 音效标签分析", "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "create task failed")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] AnalyzeSFXTags task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		if err := h.sfxSvc.AnalyzeSFXForVideo(ctx, shots, tenantID); err != nil {
			logger.Printf("[VideoHandler] AnalyzeSFXTags task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"count": len(shots)}) //nolint:errcheck
	}(task.TaskID)

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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}

	shot, err := h.videoService.GetShotByID(uint(videoID), uint(shotID))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	if shot.Dialogue == "" && shot.Description == "" {
		respondBadRequest(c, "shot has no text content")
		return
	}

	var req struct {
		NarrationVoice  string `json:"narration_voice"`
		SubtitleEnabled bool   `json:"subtitle_enabled"`
		SubtitleConfig  struct {
			Position string `json:"position"`
			FontSize  int    `json:"font_size"`
			Color     string `json:"color"`
			BgStyle   string `json:"bg_style"`
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
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
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
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
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

	result, err := h.capcutService.ExportCapCutDraft(video, shots, novel)
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
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
		result, err = h.capcutService.ExportCapCutDraft(video, shots, novel)
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
func (h *VideoHandler) ListVoiceSegments(c *gin.Context) {
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
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
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	var input service.VoiceSegmentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		respondBadRequest(c, err.Error())
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
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	var req struct {
		AfterSeqNo int `json:"after_seq_no"`
		service.VoiceSegmentInput
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	segID, err := strconv.ParseUint(c.Param("seg_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid segment id")
		return
	}
	var input service.VoiceSegmentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		respondBadRequest(c, err.Error())
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
	segID, err := strconv.ParseUint(c.Param("seg_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid segment id")
		return
	}
	if err := h.videoService.DeleteVoiceSegment(uint(segID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// GenerateSegmentVoice POST /videos/:id/shots/:shot_id/segments/:seg_id/voice
// 为单条语音段落生成 TTS 音频（同步，完成后返回更新后的段落）
func (h *VideoHandler) GenerateSegmentVoice(c *gin.Context) {
	segID, err := strconv.ParseUint(c.Param("seg_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid segment id")
		return
	}
	var req struct {
		NarrationVoice string `json:"narration_voice"`
	}
	_ = c.ShouldBindJSON(&req)

	tenantID := getTenantID(c)
	if err := h.videoService.GenerateSegmentAudio(uint(segID), tenantID, req.NarrationVoice); err != nil {
		logger.Printf("[VideoHandler] GenerateSegmentVoice seg %d: %v", segID, err)
		respondErr(c, http.StatusInternalServerError, "语音生成失败，请重试")
		return
	}
	seg, _ := h.videoService.GetVoiceSegment(uint(segID))
	respondOK(c, seg)
}

// ServeSegmentAudio GET /videos/:id/shots/:shot_id/segments/:seg_id/audio
// 提供语音段落的音频文件（file:// 本地文件直接 serve；http(s):// 重定向）
func (h *VideoHandler) ServeSegmentAudio(c *gin.Context) {
	segID, err := strconv.ParseUint(c.Param("seg_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid segment id")
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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}
	var req struct {
		AfterShotNo int     `json:"after_shot_no"`
		Narration   string  `json:"narration"`
		Description string  `json:"description"`
		Duration    float64 `json:"duration"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
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
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
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
		Volume float64 `json:"volume"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid body")
		return
	}
	item := &model.ShotSFXItem{}
	item.ID = uint(itemID)
	item.Volume = req.Volume
	if err := h.sfxItemRepo.Update(item); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update sfx item")
		return
	}
	respondOK(c, item)
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
// 为视频所有分镜批量生成配音，作为单个异步任务顺序处理。
// 每次最多处理 10 个分镜（避免 TTS API 限流），已有配音的分镜自动跳过。
func (h *VideoHandler) BatchGenerateVoice(c *gin.Context) {
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
		return
	}

	var req struct {
		NarrationVoice  string `json:"narration_voice"`
		SubtitleEnabled bool   `json:"subtitle_enabled"`
		MaxShots        int    `json:"max_shots"`    // 0=自动上限10
		SkipExisting    *bool  `json:"skip_existing"` // nil/true=跳过已有配音
	}
	_ = c.ShouldBindJSON(&req)
	maxShots := req.MaxShots
	if maxShots <= 0 || maxShots > 10 {
		maxShots = 10
	}
	skipExisting := req.SkipExisting == nil || *req.SkipExisting // default true

	allShots, err := h.videoService.GetStoryboard(uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
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
	if len(targets) > maxShots {
		targets = targets[:maxShots]
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("批量配音（%d 个分镜）", len(targets)), "video", uint(videoID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string, shots []*model.StoryboardShot, narrationVoice string, subtitleEnabled bool) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] BatchGenerateVoice task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		total := len(shots)
		success, fail := 0, 0
		for i, shot := range shots {
			h.taskSvc.UpdateProgress(taskID, i*100/total) //nolint:errcheck
			if err := h.videoService.GenerateShotAudio(shot, tenantID, narrationVoice); err != nil {
				logger.Printf("[VideoHandler] BatchGenerateVoice task %s shot %d failed: %v", taskID, shot.ShotNo, err)
				fail++
			} else {
				success++
			}
			// 每个分镜间隔 1s，避免触发 TTS API 限流
			if i < total-1 {
				time.Sleep(1 * time.Second)
			}
		}
		h.taskSvc.Complete(taskID, gin.H{"success": success, "fail": fail, "total": total}) //nolint:errcheck
		logger.Printf("[VideoHandler] BatchGenerateVoice task %s done: success=%d fail=%d", taskID, success, fail)
	}(task.TaskID, targets, req.NarrationVoice, req.SubtitleEnabled)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": fmt.Sprintf("批量配音任务已提交（共 %d 个分镜）", len(targets)),
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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
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
//           bpm_min、bpm_max（BPM范围，0=不限）、limit（默认10，最多50）。
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
	segID, err := strconv.ParseUint(c.Param("seg_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid seg_id")
		return
	}
	var req struct {
		URL         string `json:"url" binding:"required"`
		TrackName   string `json:"track_name"`
		TrackArtist string `json:"track_artist"`
		Source      string `json:"source"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid body: url is required")
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

// AnalyzeBGMSegments POST /videos/:id/bgm/analyze
// 仅执行 AI 分析（不搜索音频），返回分段计划（含搜索词）。
func (h *VideoHandler) AnalyzeBGMSegments(c *gin.Context) {
	if h.bgmSvc == nil || h.bgmRepo == nil {
		respondErr(c, http.StatusNotImplemented, "BGM service not configured")
		return
	}
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
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

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] AnalyzeBGMSegments task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.AnalyzeBGMForVideo(ctx, shots, h.bgmRepo, uint(videoID), tenantID)
		if err != nil {
			logger.Printf("[VideoHandler] AnalyzeBGMSegments task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, gin.H{"count": len(segs)}) //nolint:errcheck
	}(task.TaskID)

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
	videoID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid video id")
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

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[VideoHandler] GenerateBGM task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		ctx := context.Background()
		segs, err := h.bgmSvc.GenerateBGMSegments(ctx, shots, h.bgmRepo, uint(videoID), tenantID, progressFn)
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
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "BGM生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}
