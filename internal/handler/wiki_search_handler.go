package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// WikiSearchHandler exposes a wiki / encyclopedia search endpoint.
// Currently a stub – the handler is never wired (nil) so the route is skipped.
type WikiSearchHandler struct{}

// NewWikiSearchHandler creates a WikiSearchHandler.
func NewWikiSearchHandler() *WikiSearchHandler {
	return &WikiSearchHandler{}
}

// Search handles POST /api/v1/tools/wiki-search.
func (h *WikiSearchHandler) Search(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"results": []any{}, "provider": "wiki"})
}
