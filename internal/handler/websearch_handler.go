package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// WebSearchHandler exposes the internal web search endpoint used as the
// default MCP tool endpoint for "web_search".
type WebSearchHandler struct {
	searcher service.WebSearcher
}

func NewWebSearchHandler(searcher service.WebSearcher) *WebSearchHandler {
	return &WebSearchHandler{searcher: searcher}
}

// Search handles POST /api/v1/tools/web-search
// Body:    {"query": "...", "max_results": 3}
// Returns: {"results": [{title, url, content}]}
func (h *WebSearchHandler) Search(c *gin.Context) {
	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	if req.MaxResults <= 0 {
		req.MaxResults = 3
	}
	if req.MaxResults > 10 {
		req.MaxResults = 10
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	results, err := h.searcher.Search(ctx, req.Query, req.MaxResults)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}
