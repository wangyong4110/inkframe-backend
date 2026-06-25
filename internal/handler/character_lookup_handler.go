package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// CharacterLookupHandler MCP 工具：角色档案查询端点
type CharacterLookupHandler struct {
	svc *service.CharacterLookupService
}

func NewCharacterLookupHandler(svc *service.CharacterLookupService) *CharacterLookupHandler {
	return &CharacterLookupHandler{svc: svc}
}

// Lookup 处理 POST /api/v1/tools/character-lookup 请求
func (h *CharacterLookupHandler) Lookup(c *gin.Context) {
	var req struct {
		NovelID       uint   `json:"novel_id"`
		CharacterName string `json:"character_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CharacterName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "character_name is required"})
		return
	}
	if req.NovelID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "novel_id is required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()

	result, err := h.svc.Lookup(ctx, req.NovelID, req.CharacterName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
