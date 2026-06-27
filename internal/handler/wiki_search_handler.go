package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// WikiSearchHandler serves POST /api/v1/tools/wiki-search.
// This is the HTTP backend for the built-in "wiki_search" MCP tool.
type WikiSearchHandler struct {
	searcher service.WikiSearcher
}

// NewWikiSearchHandler creates a WikiSearchHandler.
func NewWikiSearchHandler(searcher service.WikiSearcher) *WikiSearchHandler {
	return &WikiSearchHandler{searcher: searcher}
}

type wikiSearchRequest struct {
	Query      string `json:"query" binding:"required"`
	MaxResults int    `json:"max_results"`
}

// Search handles POST /api/v1/tools/wiki-search.
// Body: {"query": "...", "max_results": 3}
// Response: {"results": [{title, url, extract}]}
func (h *WikiSearchHandler) Search(c *gin.Context) {
	var req wikiSearchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	req.MaxResults = clampMaxResults(req.MaxResults, 3, 5)

	ctx, cancel := requestContext(c, 15*time.Second)
	defer cancel()

	results, err := h.searcher.Search(ctx, req.Query, req.MaxResults)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if results == nil {
		results = []service.WikiSearchResult{}
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}
