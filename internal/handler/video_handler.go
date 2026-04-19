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
	videoService      *service.VideoService
	storyboardService *service.StoryboardService
	enhancementService *service.VideoEnhancementService
}

func NewVideoHandler(
	videoService *service.VideoService,
	storyboardService *service.StoryboardService,
	enhancementService *service.VideoEnhancementService,
) *VideoHandler {
	return &VideoHandler{
		videoService:       videoService,
		storyboardService:  storyboardService,
		enhancementService: enhancementService,
	}
}

// CreateVideo 创建视频项目
// POST /api/v1/novels/:novel_id/videos
func (h *VideoHandler) CreateVideo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req model.CreateVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	video, err := h.videoService.CreateVideo(uint(novelId), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    video,
	})
}

// GetVideo 获取视频详情
// GET /api/v1/videos/:id
func (h *VideoHandler) GetVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	video, err := h.videoService.GetVideo(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    video,
	})
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
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	videos, total, err := h.videoService.ListVideos(novelId, status, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"items":      videos,
			"total":      total,
			"page":       page,
			"page_size":  pageSize,
			"total_page": (total + pageSize - 1) / pageSize,
		},
	})
}

// UpdateVideo 更新视频
// PUT /api/v1/videos/:id
func (h *VideoHandler) UpdateVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	var req model.UpdateVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	video, err := h.videoService.UpdateVideo(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    video,
	})
}

// DeleteVideo 删除视频
// DELETE /api/v1/videos/:id
func (h *VideoHandler) DeleteVideo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	if err := h.videoService.DeleteVideo(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	var req struct {
		ChapterID  uint     `json:"chapter_id"`
		Characters []string `json:"characters"`
		Style      string   `json:"style,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.storyboardService.GenerateStoryboard(uint(videoId), req.ChapterID, req.Characters, req.Style)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
}

// AnalyzeEmotions 情感分析
// POST /api/v1/storyboard/analyze-emotions
func (h *VideoHandler) AnalyzeEmotions(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.storyboardService.AnalyzeEmotions(req.Content)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
}

// EnhanceVideo 增强视频
// POST /api/v1/video/enhance
func (h *VideoHandler) EnhanceVideo(c *gin.Context) {
	var req struct {
		VideoURL      string                   `json:"video_url" binding:"required"`
		Enhancements  []model.EnhancementConfig `json:"enhancements"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.enhancementService.EnhanceVideo(req.VideoURL, req.Enhancements)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.enhancementService.GetRecommendations(req.FPS, req.Resolution, req.Duration, req.Style)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
}

// StartVideoGeneration 开始视频生成
// POST /api/v1/videos/:id/generate
func (h *VideoHandler) StartVideoGeneration(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	taskId, err := h.videoService.StartGeneration(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"task_id": taskId,
		},
	})
}

// GetVideoStatus 获取视频生成状态
// GET /api/v1/videos/:id/status
func (h *VideoHandler) GetVideoStatus(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid video id"})
		return
	}

	status, err := h.videoService.GetStatus(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    status,
	})
}
