package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// characterResponse converts a Character model to a response map, parsing JSON
// string fields (abilities, personality_tags) into proper JSON arrays so the
// frontend receives typed data rather than raw JSON strings.
func characterResponse(c *model.Character) gin.H {
	resp := gin.H{
		"id":               c.ID,
		"novel_id":         c.NovelID,
		"uuid":             c.UUID,
		"name":             c.Name,
		"role":             c.Role,
		"archetype":        c.Archetype,
		"appearance":       c.Appearance,
		"personality":      c.Personality,
		"background":       c.Background,
		"character_arc":    c.CharacterArc,
		"three_view_front": c.ThreeViewFront,
		"three_view_side":  c.ThreeViewSide,
		"three_view_back":  c.ThreeViewBack,
		"portrait":         c.Portrait,
		"cover_image":      c.CoverImage,
		"status":           c.Status,
		"created_at":       c.CreatedAt,
		"updated_at":       c.UpdatedAt,
	}
	// Parse JSON-stored array fields
	if c.Abilities != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.Abilities), &v); err == nil {
			resp["abilities"] = v
		}
	}
	if c.PersonalityTags != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.PersonalityTags), &v); err == nil {
			resp["personality_tags"] = v
		}
	}
	if c.VisualDesign != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.VisualDesign), &v); err == nil {
			resp["visual_design"] = v
		}
	}
	return resp
}

// CharacterHandler 角色处理器
type CharacterHandler struct {
	characterService  *service.CharacterService
	arcService        *service.CharacterArcService
	imageGenService   *service.ImageGenerationService
}

func NewCharacterHandler(
	characterService *service.CharacterService,
	arcService *service.CharacterArcService,
	imageGenService *service.ImageGenerationService,
) *CharacterHandler {
	return &CharacterHandler{
		characterService:  characterService,
		arcService:        arcService,
		imageGenService:   imageGenService,
	}
}

// CreateCharacter 创建角色
// POST /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) CreateCharacter(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req model.CreateCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	character, err := h.characterService.CreateCharacter(uint(novelId), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    characterResponse(character),
	})
}

// GetCharacter 获取角色详情
// GET /api/v1/characters/:id
func (h *CharacterHandler) GetCharacter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character id"})
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "character not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    characterResponse(character),
	})
}

// ListCharacters 获取角色列表
// GET /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) ListCharacters(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	characters, err := h.characterService.ListCharacters(uint(novelId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	resp := make([]gin.H, 0, len(characters))
	for _, ch := range characters {
		resp = append(resp, characterResponse(ch))
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    resp,
	})
}

// UpdateCharacter 更新角色
// PUT /api/v1/characters/:id
func (h *CharacterHandler) UpdateCharacter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character id"})
		return
	}

	var req model.UpdateCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	character, err := h.characterService.UpdateCharacter(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    characterResponse(character),
	})
}

// DeleteCharacter 删除角色
// DELETE /api/v1/characters/:id
func (h *CharacterHandler) DeleteCharacter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character id"})
		return
	}

	if err := h.characterService.DeleteCharacter(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateCharacterImage 生成角色图像
// POST /api/v1/characters/:id/images
func (h *CharacterHandler) GenerateCharacterImage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character id"})
		return
	}

	var req struct {
		Type     string `json:"type"` // portrait, expression, pose
		Emotion  string `json:"emotion,omitempty"`
		Action   string `json:"action,omitempty"`
		Style    string `json:"style,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "character not found"})
		return
	}

	image, err := h.imageGenService.GenerateCharacterImage(&model.GenerateImageRequest{
		Subject:     character.Name,
		Description: character.Appearance,
		Type:        req.Type,
		Emotion:     req.Emotion,
		Action:      req.Action,
		Style:       req.Style,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    image,
	})
}

// GenerateCharacterProfile AI生成角色档案
// POST /api/v1/novels/:novel_id/characters/generate
func (h *CharacterHandler) GenerateCharacterProfile(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	character, err := h.characterService.GenerateProfile(getTenantID(c), uint(novelId), req.Description)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    character,
	})
}

// GetCharacterArc 获取角色弧光
// GET /api/v1/novels/:novel_id/character-arcs/:character_id
func (h *CharacterHandler) GetCharacterArc(c *gin.Context) {
	novelId, _ := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	characterId, _ := strconv.ParseUint(c.Param("character_id"), 10, 32)

	arc, err := h.arcService.GetCharacterArc(uint(novelId), uint(characterId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    arc,
	})
}

// GetAllCharacterArcs 获取所有角色弧光
// GET /api/v1/novels/:novel_id/character-arcs
func (h *CharacterHandler) GetAllCharacterArcs(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid novel id"})
		return
	}

	arcs, err := h.arcService.GetAllArcs(uint(novelId))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    arcs,
	})
}

// UpdateCharacterArc 更新角色弧光
// PUT /api/v1/novels/:novel_id/character-arcs/:character_id
func (h *CharacterHandler) UpdateCharacterArc(c *gin.Context) {
	novelId, _ := strconv.ParseUint(c.Param("novel_id"), 10, 32)
	characterId, _ := strconv.ParseUint(c.Param("character_id"), 10, 32)

	var req struct {
		CurrentStage int    `json:"current_stage"`
		Note         string `json:"note,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	arc, err := h.arcService.UpdateArc(uint(novelId), uint(characterId), req.CurrentStage, req.Note)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    arc,
	})
}

// AnalyzeCharacterConsistency 分析角色一致性
// POST /api/v1/characters/:id/analyze-consistency
func (h *CharacterHandler) AnalyzeCharacterConsistency(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid character id"})
		return
	}

	var req struct {
		Images []string `json:"images" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	result, err := h.characterService.AnalyzeConsistency(uint(id), req.Images)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
}
