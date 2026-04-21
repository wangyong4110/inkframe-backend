package handler

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// NovelHandler 小说处理器
type NovelHandler struct {
	novelService          *service.NovelService
	chapterService        *service.ChapterService
	foreshadowService     *service.ForeshadowService
	timelineService       *service.TimelineService
	qualityControlService *service.QualityControlService
}

func NewNovelHandler(
	novelService *service.NovelService,
	chapterService *service.ChapterService,
	foreshadowService *service.ForeshadowService,
	timelineService *service.TimelineService,
	qualityControlService *service.QualityControlService,
) *NovelHandler {
	return &NovelHandler{
		novelService:          novelService,
		chapterService:        chapterService,
		foreshadowService:     foreshadowService,
		timelineService:       timelineService,
		qualityControlService: qualityControlService,
	}
}

// CreateNovel 创建小说
// POST /api/v1/novels
func (h *NovelHandler) CreateNovel(c *gin.Context) {
	var req model.CreateNovelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	novel, err := h.novelService.CreateNovel(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    novel,
	})
}

// GetNovel 获取小说详情
// GET /api/v1/novels/:id
func (h *NovelHandler) GetNovel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	novel, err := h.novelService.GetNovel(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "novel not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    novel,
	})
}

// ListNovels 获取小说列表
// GET /api/v1/novels
func (h *NovelHandler) ListNovels(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 1
	} else if pageSize > 100 {
		pageSize = 100
	}

	filters := map[string]interface{}{}
	if tenantID, ok := c.Get("tenant_id"); ok {
		filters["tenant_id"] = tenantID
	}
	if status := c.Query("status"); status != "" {
		filters["status"] = status
	}
	if genre := c.Query("genre"); genre != "" {
		filters["genre"] = genre
	}

	novels, total, err := h.novelService.ListNovelsFiltered(page, pageSize, filters)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"items":      novels,
			"total":      total,
			"page":       page,
			"page_size":  pageSize,
			"total_page": (int(total) + pageSize - 1) / pageSize,
		},
	})
}

// UpdateNovel 更新小说
// PUT /api/v1/novels/:id
func (h *NovelHandler) UpdateNovel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req model.UpdateNovelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	novel, err := h.novelService.UpdateNovel(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    novel,
	})
}

// DeleteNovel 删除小说
// DELETE /api/v1/novels/:id
func (h *NovelHandler) DeleteNovel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	if err := h.novelService.DeleteNovel(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateChapter 生成章节
// POST /api/v1/novels/:id/chapters/generate
func (h *NovelHandler) GenerateChapter(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req model.GenerateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 支持通过 Header 临时覆盖 AI 模型/provider
	if override := c.GetHeader("X-Model-Override"); override != "" && req.ModelOverride == "" {
		req.ModelOverride = override
	}

	chapter, err := h.chapterService.GenerateChapter(getTenantID(c), uint(novelId), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Async post-generation: foreshadow extraction + quality check (non-blocking)
	go func(ch *model.Chapter) {
		if _, err := h.foreshadowService.ExtractForeshadows(ch, ch.NovelID); err != nil {
			fmt.Printf("GenerateChapter: foreshadow extraction failed (ch %d): %v\n", ch.ID, err)
		}
	}(chapter)
	go func(chID uint) {
		if _, err := h.qualityControlService.CheckChapter(chID); err != nil {
			fmt.Printf("GenerateChapter: quality check failed (ch %d): %v\n", chID, err)
		}
	}(chapter.ID)

	modelUsed := req.ModelOverride
	if modelUsed == "" {
		modelUsed = h.novelService.GetAIService().GetDefaultProviderName()
	}

	c.JSON(http.StatusOK, gin.H{
		"code":       0,
		"message":    "success",
		"data":       chapter,
		"model_used": modelUsed,
	})
}

// GenerateOutline 生成大纲
// POST /api/v1/novels/:id/outline
func (h *NovelHandler) GenerateOutline(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req struct {
		ChapterNum int      `json:"chapter_num"`
		Prompt     string   `json:"prompt"`
		Keywords   []string `json:"keywords"`
	}
	c.ShouldBindJSON(&req)

	result, err := h.novelService.GenerateOutline(getTenantID(c), &service.GenerateOutlineRequest{
		NovelID:    uint(novelId),
		ChapterNum: req.ChapterNum,
		Prompt:     req.Prompt,
		Keywords:   req.Keywords,
	})
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

// GetForeshadows 获取伏笔列表
// GET /api/v1/novels/:id/foreshadows
func (h *NovelHandler) GetForeshadows(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	chapterNo, _ := strconv.Atoi(c.Query("chapter_no"))

	foreshadows, err := h.foreshadowService.GetForeshadows(uint(novelId), chapterNo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    foreshadows,
	})
}

// MarkForeshadowFulfilled 标记伏笔已回收
// POST /api/v1/novels/:id/foreshadows/:foreshadow_id/fulfill
func (h *NovelHandler) MarkForeshadowFulfilled(c *gin.Context) {
	novelId, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	foreshadowId, _ := strconv.ParseUint(c.Param("foreshadow_id"), 10, 32)

	var req struct {
		ChapterID uint `json:"chapter_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.foreshadowService.MarkFulfilledByID(uint(novelId), uint(foreshadowId), req.ChapterID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetTimeline 获取时间线
// GET /api/v1/novels/:id/timeline
func (h *NovelHandler) GetTimeline(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	timeline, err := h.timelineService.GetTimeline(uint(novelId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    timeline,
	})
}

// BuildTimeline 构建时间线
// POST /api/v1/novels/:id/timeline/build
func (h *NovelHandler) BuildTimeline(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	timeline, err := h.timelineService.BuildTimeline(uint(novelId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    timeline,
	})
}
