package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// StyleHandler 风格控制器
type StyleHandler struct {
	styleService *service.StyleService
}

func NewStyleHandler(styleService *service.StyleService) *StyleHandler {
	return &StyleHandler{styleService: styleService}
}

// GetDefaultStyle 获取默认风格配置
// GET /api/v1/styles/default
func (h *StyleHandler) GetDefaultStyle(c *gin.Context) {
	style, err := h.styleService.GetDefaultStyle()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    style,
	})
}

// BuildStylePrompt 构建风格提示词
// POST /api/v1/styles/prompt
func (h *StyleHandler) BuildStylePrompt(c *gin.Context) {
	var req struct {
		NarrativeVoice       string  `json:"narrative_voice"`
		NarrativeDistance    string  `json:"narrative_distance"`
		EmotionalTone        string  `json:"emotional_tone"`
		SentenceComplexity   string  `json:"sentence_complexity"`
		DescriptionDensity   string  `json:"description_density"`
		DialogueRatio        float64 `json:"dialogue_ratio"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	prompt, err := h.styleService.BuildStylePrompt(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"prompt": prompt,
		},
	})
}

// GetStylePresets 获取风格预设列表
// GET /api/v1/styles/presets
func (h *StyleHandler) GetStylePresets(c *gin.Context) {
	presets := h.styleService.GetStylePresets()

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    presets,
	})
}

// ApplyStylePreset 应用风格预设
// POST /api/v1/styles/presets/:name/apply
func (h *StyleHandler) ApplyStylePreset(c *gin.Context) {
	name := c.Param("name")

	style, err := h.styleService.ApplyPreset(name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "preset not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    style,
	})
}
