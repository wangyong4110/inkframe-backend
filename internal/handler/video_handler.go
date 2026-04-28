package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
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

	video, err := h.videoService.GetVideo(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found")
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
		ChapterID  uint     `json:"chapter_id"`
		Characters []string `json:"characters"`
		Style      string   `json:"style,omitempty"`
		Provider   string   `json:"provider,omitempty"` // 指定 LLM 提供者，可为空
	}
	// 所有字段均可选，body 为空时忽略 EOF
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeStoryboardGen, "分镜脚本生成", "video", uint(videoId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		result, err := h.storyboardService.GenerateStoryboard(uint(videoId), req.ChapterID, req.Characters, req.Style, req.Provider)
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			fmt.Printf("GenerateStoryboard task %s failed: %v\n", taskID, err)
			return
		}
		h.taskSvc.Complete(taskID, result) //nolint:errcheck
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "分镜生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// GetStoryboardGenStatus 查询分镜生成任务状态
// GET /api/v1/videos/:id/storyboard/generate/:task_id
func (h *VideoHandler) GetStoryboardGenStatus(c *gin.Context) {
	taskID := c.Param("task_id")
	task, err := h.taskSvc.Get(taskID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}
	respondOK(c, task)
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
		c.Header("Cache-Control", "public, max-age=86400")
		c.File(filePath)
		return
	}
	// /api/v1/media/:id 相对路径 — 重定向
	c.Redirect(http.StatusFound, shot.AudioPath)
}

// UpdateStoryboardShot 更新分镜
// PUT /api/v1/videos/:id/storyboard/:shot_id
func (h *VideoHandler) UpdateStoryboardShot(c *gin.Context) {
	shotId, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}

	var req model.StoryboardShot
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	shot, err := h.videoService.UpdateShot(uint(shotId), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, shot)
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

	status, err := h.videoService.GetStatus(uint(id))
	if err != nil {
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

	video, err := h.videoService.GetVideo(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.videoService.GenerateAllShotVideos(uint(id)); err != nil {
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
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"output_path": outputPath,
	})
}

// GenerateSingleShot 生成单个分镜
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

	shot, err := h.videoService.GenerateSingleShot(uint(videoID), uint(shotID), req.Provider)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "shot generation queued",
		"data":    shot,
	})
}

// BatchGenerateShots 批量生成分镜
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

	shots, err := h.videoService.BatchGenerateShots(uint(videoID), req.ShotIDs, req.QualityTier, req.Provider)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "batch shot generation queued",
		"data":    shots,
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
	if shot.Dialogue == "" {
		respondBadRequest(c, "shot has no dialogue text")
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoiceGen,
		fmt.Sprintf("镜头 #%d 配音生成", shot.ShotNo), "shot", uint(shotID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string, shot *model.StoryboardShot) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		if err := h.videoService.GenerateShotAudio(shot); err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, gin.H{"audio_url": shot.AudioPath, "shot_id": shot.ID}) //nolint:errcheck
	}(task.TaskID, shot)

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

	video, err := h.videoService.GetVideo(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found")
		return
	}

	shots, err := h.videoService.GetStoryboard(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	result, err := h.capcutService.ExportCapCutDraft(video, shots)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Length", strconv.Itoa(len(result.Data)))
	c.Data(http.StatusOK, "application/zip", result.Data)
}
