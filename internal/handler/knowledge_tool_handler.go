package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// KnowledgeToolHandler 知识库 MCP 工具端点（区别于 KnowledgeHandler 的 CRUD 操作）
type KnowledgeToolHandler struct {
	knowledgeSvc *service.KnowledgeService
}

func NewKnowledgeToolHandler(svc *service.KnowledgeService) *KnowledgeToolHandler {
	return &KnowledgeToolHandler{knowledgeSvc: svc}
}

// Search 处理 POST /api/v1/tools/knowledge-search 请求
func (h *KnowledgeToolHandler) Search(c *gin.Context) {
	var req struct {
		NovelID uint   `json:"novel_id"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	req.Limit = clampMaxResults(req.Limit, 5, 10)

	ctx, cancel := requestContext(c, 10*time.Second)
	defer cancel()

	var novelID *uint
	if req.NovelID > 0 {
		id := req.NovelID
		novelID = &id
	}

	results, err := h.knowledgeSvc.SearchKnowledge(ctx, req.Query, req.Limit, novelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type knowledgeResult struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Type    string   `json:"type"`
		Tags    []string `json:"tags"`
	}
	out := make([]knowledgeResult, 0, len(results))
	for _, kb := range results {
		var tags []string
		if kb.Tags != "" {
			_ = json.Unmarshal([]byte(kb.Tags), &tags)
		}
		out = append(out, knowledgeResult{
			Title:   kb.Title,
			Content: kb.Content,
			Type:    kb.Type,
			Tags:    tags,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": out})
}
