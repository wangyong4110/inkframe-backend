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
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, style)
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
		respondBadRequest(c, err.Error())
		return
	}

	prompt, err := h.styleService.BuildStylePrompt(&req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"prompt": prompt,
	})
}

// GetStylePresets 获取风格预设列表
// GET /api/v1/styles/presets
func (h *StyleHandler) GetStylePresets(c *gin.Context) {
	presets := h.styleService.GetStylePresets()

	respondOK(c, presets)
}

// ApplyStylePreset 应用风格预设
// POST /api/v1/styles/presets/:name/apply
func (h *StyleHandler) ApplyStylePreset(c *gin.Context) {
	name := c.Param("name")

	style, err := h.styleService.ApplyPreset(name)
	if err != nil {
		respondErr(c, http.StatusNotFound, "preset not found")
		return
	}

	respondOK(c, style)
}
