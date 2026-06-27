package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ImageRefSearchHandler serves POST /api/v1/tools/image-ref-search.
// This is the HTTP backend for the built-in "image_ref_search" MCP tool.
type ImageRefSearchHandler struct {
	searcher service.ImageRefSearcher
}

// NewImageRefSearchHandler creates an ImageRefSearchHandler.
func NewImageRefSearchHandler(searcher service.ImageRefSearcher) *ImageRefSearchHandler {
	return &ImageRefSearchHandler{searcher: searcher}
}

type imageRefSearchRequest struct {
	Query      string `json:"query" binding:"required"`
	MaxResults int    `json:"max_results"`
}

// Search handles POST /api/v1/tools/image-ref-search.
// Body: {"query": "xianxia cultivation misty mountains", "max_results": 3}
// Response: {"results": [{url, thumb_url, tags, page_url}]}
func (h *ImageRefSearchHandler) Search(c *gin.Context) {
	var req imageRefSearchRequest
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
		results = []service.ImageRefResult{}
	}
	c.JSON(http.StatusOK, gin.H{
		"results":  results,
		"provider": h.searcher.Name(),
	})
}
