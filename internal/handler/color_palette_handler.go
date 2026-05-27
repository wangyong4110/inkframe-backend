package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ColorPaletteHandler serves POST /api/v1/tools/color-palette.
// This is the HTTP backend for the built-in "color_palette" MCP tool.
type ColorPaletteHandler struct {
	svc *service.ColorPaletteService
}

// NewColorPaletteHandler creates a ColorPaletteHandler.
func NewColorPaletteHandler(svc *service.ColorPaletteService) *ColorPaletteHandler {
	return &ColorPaletteHandler{svc: svc}
}

type colorPaletteRequest struct {
	// Single mood lookup
	Mood string `json:"mood"`
	// Multi-mood lookup (returns merged/first-priority palette)
	Moods []string `json:"moods"`
}

// Get handles POST /api/v1/tools/color-palette.
// Body: {"mood": "tension"} or {"moods": ["battle", "tension"]}
// Response: {"palette": {...}} or {"palettes": [{...}]}
func (h *ColorPaletteHandler) Get(c *gin.Context) {
	var req colorPaletteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(req.Moods) > 0 {
		palettes := h.svc.GetMultiple(req.Moods)
		c.JSON(http.StatusOK, gin.H{"palettes": palettes})
		return
	}

	mood := strings.TrimSpace(req.Mood)
	if mood == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mood or moods is required"})
		return
	}

	palette := h.svc.GetPalette(mood)
	c.JSON(http.StatusOK, gin.H{"palette": palette})
}

// ListAll handles GET /api/v1/tools/color-palette/list.
// Returns the full palette library.
func (h *ColorPaletteHandler) ListAll(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"palettes": h.svc.ListAll()})
}
