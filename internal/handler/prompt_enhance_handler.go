package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// PromptEnhanceHandler MCP 工具：Prompt 增强端点
type PromptEnhanceHandler struct {
	svc *service.PromptEnhanceService
}

func NewPromptEnhanceHandler(svc *service.PromptEnhanceService) *PromptEnhanceHandler {
	return &PromptEnhanceHandler{svc: svc}
}

// Enhance 处理 POST /api/v1/tools/prompt-enhance 请求
func (h *PromptEnhanceHandler) Enhance(c *gin.Context) {
	var req struct {
		SceneDescription string `json:"scene_description"`
		Style            string `json:"style"`
		Type             string `json:"type"` // "image" or "video"
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.SceneDescription == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scene_description is required"})
		return
	}

	ctx, cancel := requestContext(c, 30*time.Second)
	defer cancel()

	result, err := h.svc.Enhance(ctx, req.SceneDescription, req.Style, req.Type)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
