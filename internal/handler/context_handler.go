package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ContextHandler 生成上下文处理器
type ContextHandler struct {
	contextService *service.GenerationContextService
}

func NewContextHandler(contextService *service.GenerationContextService) *ContextHandler {
	return &ContextHandler{contextService: contextService}
}

// GetContext 获取生成上下文
// GET /api/v1/novels/:id/context
func (h *ContextHandler) GetContext(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	chapterNo, _ := strconv.Atoi(c.Query("chapter_no"))

	context, err := h.contextService.GetContext(uint(novelId), chapterNo)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, context)
}

// BuildPrompt 构建生成提示词
// POST /api/v1/novels/:id/prompt
func (h *ContextHandler) BuildPrompt(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	var req struct {
		ChapterNo    int     `json:"chapter_no"`
		Style        string  `json:"style,omitempty"`
		ExtraPrompt  string  `json:"extra_prompt,omitempty"`
		MaxContextLen int     `json:"max_context_len,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	result, err := h.contextService.BuildGenerationPrompt(uint(novelId), req.ChapterNo, req.Style, req.ExtraPrompt, req.MaxContextLen)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// PreviewContext 预览上下文摘要
// GET /api/v1/novels/:id/context/preview
func (h *ContextHandler) PreviewContext(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	preview, err := h.contextService.GetContextPreview(uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, preview)
}
