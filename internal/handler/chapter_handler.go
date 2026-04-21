package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ChapterHandler 章节处理器
type ChapterHandler struct {
	chapterService     *service.ChapterService
	versionService     *service.ChapterVersionService
	qualityService     *service.QualityControlService
}

func NewChapterHandler(
	chapterService *service.ChapterService,
	versionService *service.ChapterVersionService,
	qualityService *service.QualityControlService,
) *ChapterHandler {
	return &ChapterHandler{
		chapterService: chapterService,
		versionService: versionService,
		qualityService: qualityService,
	}
}

// CreateChapter 创建章节
// POST /api/v1/novels/:novel_id/chapters
func (h *ChapterHandler) CreateChapter(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req model.CreateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chapter, err := h.chapterService.CreateChapter(uint(novelId), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// GetChapter 获取章节详情
// GET /api/v1/chapters/:id
func (h *ChapterHandler) GetChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	chapter, err := h.chapterService.GetChapter(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chapter not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// ListChapters 获取章节列表
// GET /api/v1/novels/:novel_id/chapters
func (h *ChapterHandler) ListChapters(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	chapters, err := h.chapterService.ListChapters(uint(novelId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapters,
	})
}

// UpdateChapter 更新章节
// PUT /api/v1/chapters/:id
func (h *ChapterHandler) UpdateChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	var req model.UpdateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chapter, err := h.chapterService.UpdateChapter(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// DeleteChapter 删除章节
// DELETE /api/v1/chapters/:id
func (h *ChapterHandler) DeleteChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	if err := h.chapterService.DeleteChapter(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateChapter 生成章节内容
// POST /api/v1/chapters/generate
func (h *ChapterHandler) GenerateChapter(c *gin.Context) {
	var req model.GenerateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 支持通过 Header 临时覆盖 AI 模型/provider
	if override := c.GetHeader("X-Model-Override"); override != "" && req.ModelOverride == "" {
		req.ModelOverride = override
	}

	chapter, err := h.chapterService.GenerateChapter(getTenantID(c), req.NovelID, &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	modelUsed := req.ModelOverride
	if modelUsed == "" {
		modelUsed = h.chapterService.GetDefaultProviderName()
	}

	c.JSON(http.StatusOK, gin.H{
		"code":       0,
		"message":    "success",
		"data":       chapter,
		"model_used": modelUsed,
	})
}

// RegenerateChapter 重新生成章节
// POST /api/v1/chapters/:id/regenerate
func (h *ChapterHandler) RegenerateChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chapter, err := h.chapterService.RegenerateChapter(getTenantID(c), uint(id), req.Prompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// GetVersions 获取章节版本历史
// GET /api/v1/chapters/:id/versions
func (h *ChapterHandler) GetVersions(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	versions, err := h.versionService.GetVersions(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    versions,
	})
}

// RestoreVersion 恢复版本
// POST /api/v1/chapters/:id/versions/:version_no/restore
func (h *ChapterHandler) RestoreVersion(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}
	versionNo, err := strconv.Atoi(c.Param("version_no"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version no"})
		return
	}

	chapter, err := h.versionService.RestoreVersion(uint(id), versionNo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// GetChapterByNo 根据章节号获取章节
// GET /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) GetChapterByNo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter no"})
		return
	}

	chapter, err := h.chapterService.GetChapterByNo(uint(novelId), chapterNo)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chapter not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// UpdateChapterByNo 根据章节号更新章节
// PUT /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) UpdateChapterByNo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter no"})
		return
	}

	var req model.UpdateChapterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chapter, err := h.chapterService.UpdateChapterByNo(uint(novelId), chapterNo, &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    chapter,
	})
}

// DeleteChapterByNo 根据章节号删除章节
// DELETE /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) DeleteChapterByNo(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter no"})
		return
	}

	if err := h.chapterService.DeleteChapterByNo(uint(novelId), chapterNo); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetQualityReport 获取质量报告
// GET /api/v1/chapters/:id/quality-report
func (h *ChapterHandler) GetQualityReport(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	report, err := h.qualityService.CheckChapter(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    report,
	})
}

// QualityCheck 质量检查
// POST /api/v1/chapters/:id/quality-check
func (h *ChapterHandler) QualityCheck(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	report, err := h.qualityService.CheckChapter(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    report,
	})
}

// ApproveChapter 审核通过章节
// POST /api/v1/chapters/:id/approve
func (h *ChapterHandler) ApproveChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	var req struct {
		Comment string `json:"comment"`
	}
	_ = c.ShouldBindJSON(&req) // comment is optional; ignore bind errors

	if err := h.chapterService.ApproveChapter(uint(id), req.Comment); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// RejectChapter 驳回章节
// POST /api/v1/chapters/:id/reject
func (h *ChapterHandler) RejectChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chapter id"})
		return
	}

	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.chapterService.RejectChapter(uint(id), req.Reason); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}
