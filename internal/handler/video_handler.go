package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// VideoHandler 视频处理器
type VideoHandler struct {
	videoService        *service.VideoService
	storyboardService   *service.StoryboardService
	enhancementService  *service.VideoEnhancementService
	consistencyService  *service.CharacterConsistencyService
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
	}
}

// CreateVideo 创建视频项目
// POST /api/v1/novels/:novel_id/videos
func (h *VideoHandler) CreateVideo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
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
// GET /api/v1/videos
func (h *VideoHandler) ListVideos(c *gin.Context) {
	novelIdStr := c.Query("novel_id")
	var novelId *uint
	if novelIdStr != "" {
		if id, err := strconv.ParseUint(novelIdStr, 10, 32); err == nil {
			novelId = new(uint)
			*novelId = uint(id)
		}
	}

	status := c.Query("status")
	p := parsePagination(c)

	videos, total, err := h.videoService.ListVideos(novelId, status, p.Page, p.PageSize)
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

// GenerateStoryboard 生成分镜
// POST /api/v1/videos/:id/storyboard/generate
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
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	result, err := h.storyboardService.GenerateStoryboard(uint(videoId), req.ChapterID, req.Characters, req.Style)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
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

	respondOK(c, shots)
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
		VideoURL      string                   `json:"video_url" binding:"required"`
		Enhancements  []model.EnhancementConfig `json:"enhancements"`
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

	if err := h.videoService.GenerateAllShotVideos(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	go h.videoService.PollAndStitchVideo(uint(id))

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

	shot, err := h.videoService.GenerateSingleShot(uint(videoID), uint(shotID))
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

	shots, err := h.videoService.BatchGenerateShots(uint(videoID), req.ShotIDs, req.QualityTier)
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
