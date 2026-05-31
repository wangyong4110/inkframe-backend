package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// StoryPatternHandler serves POST /api/v1/tools/story-pattern.
// This is the HTTP backend for the built-in "story_pattern" MCP tool.
type StoryPatternHandler struct {
	svc *service.StoryPatternService
}

// NewStoryPatternHandler creates a StoryPatternHandler.
func NewStoryPatternHandler(svc *service.StoryPatternService) *StoryPatternHandler {
	return &StoryPatternHandler{svc: svc}
}

type storyPatternRequest struct {
	Genre      string `json:"genre"`
	Archetype  string `json:"archetype"`
	MaxResults int    `json:"max_results"`
}

// Search handles POST /api/v1/tools/story-pattern.
// Body: {"genre": "修仙", "archetype": "绝境逆袭", "max_results": 2}
// Response: {"patterns": [{id, name, beats, emotional_arc, ...}]}
func (h *StoryPatternHandler) Search(c *gin.Context) {
	var req storyPatternRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.MaxResults <= 0 || req.MaxResults > 4 {
		req.MaxResults = 2
	}

	patterns := h.svc.Search(req.Genre, req.Archetype, req.MaxResults)
	if patterns == nil {
		patterns = []service.StoryPattern{}
	}
	c.JSON(http.StatusOK, gin.H{"patterns": patterns})
}

// ListAll handles GET /api/v1/tools/story-pattern/list and GET /api/v1/story-patterns.
// Returns the full embedded pattern library for browsing in the frontend.
func (h *StoryPatternHandler) ListAll(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"patterns": h.svc.ListAll()})
}

// GetPattern handles GET /api/v1/story-patterns/:id.
// Returns a single story pattern by its ID string.
func (h *StoryPatternHandler) GetPattern(c *gin.Context) {
	id := c.Param("id")
	patterns := h.svc.ListAll()
	for _, p := range patterns {
		if p.ID == id {
			c.JSON(http.StatusOK, gin.H{"pattern": p})
			return
		}
	}
	respondErr(c, http.StatusNotFound, "pattern not found")
}

// SuggestForNovel handles POST /api/v1/novels/:id/story-patterns/suggest.
// Returns story patterns matched to the novel's genre.
func (h *StoryPatternHandler) SuggestForNovel(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	_ = novelID
	var req struct {
		Genre      string `json:"genre"`
		Archetype  string `json:"archetype"`
		MaxResults int    `json:"max_results"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.MaxResults <= 0 || req.MaxResults > 4 {
		req.MaxResults = 3
	}
	patterns := h.svc.Search(req.Genre, req.Archetype, req.MaxResults)
	if patterns == nil {
		patterns = []service.StoryPattern{}
	}
	c.JSON(http.StatusOK, gin.H{"patterns": patterns})
}
