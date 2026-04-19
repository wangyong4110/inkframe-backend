package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// NovelHandler 小说处理器
type NovelHandler struct {
	novelService *service.NovelService
}

func NewNovelHandler(novelService *service.NovelService) *NovelHandler {
	return &NovelHandler{novelService: novelService}
}

// Create 创建小说
// @Summary 创建小说
// @Tags novels
// @Accept json
// @Produce json
// @Param request body service.CreateNovelRequest true "创建请求"
// @Success 201 {object} model.Novel
// @Router /api/novels [post]
func (h *NovelHandler) Create(c *gin.Context) {
	var req service.CreateNovelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	novel, err := h.novelService.Create(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, novel)
}

// Get 获取小说
// @Summary 获取小说
// @Tags novels
// @Produce json
// @Param id path int true "小说ID"
// @Success 200 {object} model.Novel
// @Router /api/novels/{id} [get]
func (h *NovelHandler) Get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	novel, err := h.novelService.GetNovel(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "novel not found"})
		return
	}

	c.JSON(http.StatusOK, novel)
}

// List 获取小说列表
// @Summary 获取小说列表
// @Tags novels
// @Produce json
// @Param page query int false "页码"
// @Param page_size query int false "每页数量"
// @Param status query string false "状态"
// @Param genre query string false "类型"
// @Success 200 {array} model.Novel
// @Router /api/novels [get]
func (h *NovelHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	filters := make(map[string]interface{})
	if status := c.Query("status"); status != "" {
		filters["status"] = status
	}
	if genre := c.Query("genre"); genre != "" {
		filters["genre"] = genre
	}

	novels, total, err := h.novelService.ListNovels(page, pageSize, filters)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  novels,
		"total": total,
		"page":  page,
		"size":  pageSize,
	})
}

// Update 更新小说
// @Summary 更新小说
// @Tags novels
// @Accept json
// @Produce json
// @Param id path int true "小说ID"
// @Param request body model.Novel true "小说数据"
// @Success 200 {object} model.Novel
// @Router /api/novels/{id} [put]
func (h *NovelHandler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var novel service.CreateNovelRequest
	if err := c.ShouldBindJSON(&novel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	existing, err := h.novelService.GetNovel(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "novel not found"})
		return
	}

	// 更新字段
	existing.Title = novel.Title
	existing.Description = novel.Description
	existing.Genre = novel.Genre

	if err := h.novelService.UpdateNovel(existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, existing)
}

// Delete 删除小说
// @Summary 删除小说
// @Tags novels
// @Param id path int true "小说ID"
// @Success 204
// @Router /api/novels/{id} [delete]
func (h *NovelHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	if err := h.novelService.DeleteNovel(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

// GenerateOutline 生成大纲
// @Summary 生成大纲
// @Tags novels
// @Accept json
// @Produce json
// @Param id path int true "小说ID"
// @Param request body service.GenerateOutlineRequest true "生成大纲请求"
// @Success 200 {object} service.OutlineResult
// @Router /api/novels/{id}/outline [post]
func (h *NovelHandler) GenerateOutline(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var req service.GenerateOutlineRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.NovelID = uint(id)

	result, err := h.novelService.GenerateOutline(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// ChapterHandler 章节处理器
type ChapterHandler struct {
	novelService *service.NovelService
	chapterRepo interface {
		GetByID(id uint) (interface{}, error)
		ListByNovel(novelID uint) ([]interface{}, error)
		Create(interface{}) error
	}
}

func NewChapterHandler(novelService *service.NovelService) *ChapterHandler {
	return &ChapterHandler{novelService: novelService}
}

// List 获取章节列表
// @Summary 获取章节列表
// @Tags chapters
// @Produce json
// @Param novel_id path int true "小说ID"
// @Success 200 {array} model.Chapter
// @Router /api/novels/{novel_id}/chapters [get]
func (h *ChapterHandler) List(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	// TODO: 实现获取章节列表
	c.JSON(http.StatusOK, gin.H{})
}

// Generate 生成章节
// @Summary 生成章节
// @Tags chapters
// @Accept json
// @Produce json
// @Param novel_id path int true "小说ID"
// @Param request body service.GenerateChapterRequest true "生成章节请求"
// @Success 200 {object} model.Chapter
// @Router /api/novels/{novel_id}/chapters [post]
func (h *ChapterHandler) Generate(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel_id"})
		return
	}

	var req service.GenerateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.NovelID = uint(novelID)

	chapter, err := h.novelService.GenerateChapter(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, chapter)
}

// ModelHandler 模型处理器
type ModelHandler struct {
	modelService *service.ModelService
}

func NewModelHandler(modelService *service.ModelService) *ModelHandler {
	return &ModelHandler{modelService: modelService}
}

// ListProviders 获取提供商列表
// @Summary 获取提供商列表
// @Tags models
// @Produce json
// @Success 200 {array} model.ModelProvider
// @Router /api/model-providers [get]
func (h *ModelHandler) ListProviders(c *gin.Context) {
	// TODO: 实现获取提供商列表
	c.JSON(http.StatusOK, gin.H{})
}

// ListModels 获取模型列表
// @Summary 获取模型列表
// @Tags models
// @Produce json
// @Success 200 {array} model.AIModel
// @Router /api/models [get]
func (h *ModelHandler) ListModels(c *gin.Context) {
	// TODO: 实现获取模型列表
	c.JSON(http.StatusOK, gin.H{})
}

// SelectModel 选择模型
// @Summary 选择模型
// @Tags models
// @Accept json
// @Produce json
// @Param request body SelectModelRequest true "选择模型请求"
// @Success 200 {object} model.AIModel
// @Router /api/model/select [post]
func (h *ModelHandler) SelectModel(c *gin.Context) {
	var req SelectModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	model, err := h.modelService.SelectModel(req.TaskType, req.Strategy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, model)
}

// SelectModelRequest 选择模型请求
type SelectModelRequest struct {
	TaskType string `json:"task_type" binding:"required"`
	Strategy string `json:"strategy" binding:"required"`
}

// VideoHandler 视频处理器
type VideoHandler struct {
	videoService *service.VideoService
}

func NewVideoHandler(videoService *service.VideoService) *VideoHandler {
	return &VideoHandler{videoService: videoService}
}

// Create 创建视频
// @Summary 创建视频
// @Tags videos
// @Accept json
// @Produce json
// @Param request body CreateVideoRequest true "创建视频请求"
// @Success 201 {object} model.Video
// @Router /api/videos [post]
func (h *VideoHandler) Create(c *gin.Context) {
	var req CreateVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	video, err := h.videoService.CreateVideo(req.NovelID, req.ChapterID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, video)
}

// CreateVideoRequest 创建视频请求
type CreateVideoRequest struct {
	NovelID   uint  `json:"novel_id" binding:"required"`
	ChapterID *uint `json:"chapter_id"`
}

// GenerateStoryboard 生成分镜
// @Summary 生成分镜
// @Tags videos
// @Accept json
// @Produce json
// @Param id path int true "视频ID"
// @Success 200 {array} model.StoryboardShot
// @Router /api/videos/{id}/storyboard [post]
func (h *VideoHandler) GenerateStoryboard(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	shots, err := h.videoService.GenerateStoryboard(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, shots)
}

// HealthHandler 健康检查处理器
type HealthHandler struct{}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// Check 健康检查
// @Summary 健康检查
// @Tags health
// @Produce json
// @Success 200 {object} map[string]string
// @Router /health [get]
func (h *HealthHandler) Check(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   "2024-01-19T17:00:00Z",
	})
}
