package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// WorldviewHandler 世界观处理器
type WorldviewHandler struct {
	worldviewService *service.WorldviewService
}

func NewWorldviewHandler(worldviewService *service.WorldviewService) *WorldviewHandler {
	return &WorldviewHandler{worldviewService: worldviewService}
}

// ListWorldviews 获取世界观列表
// GET /api/v1/worldviews
func (h *WorldviewHandler) ListWorldviews(c *gin.Context) {
	p := parsePagination(c)
	genre := c.Query("genre")

	worldviews, total, err := h.worldviewService.ListWorldviews(p.Page, p.PageSize, genre)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"items":      worldviews,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (int(total) + p.PageSize - 1) / p.PageSize,
	})
}

// GetWorldview 获取世界观详情
// GET /api/v1/worldviews/:id
func (h *WorldviewHandler) GetWorldview(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid worldview id")
		return
	}

	worldview, err := h.worldviewService.GetWorldview(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "worldview not found")
		return
	}

	respondOK(c, worldview)
}

// CreateWorldview 创建世界观
// POST /api/v1/worldviews
func (h *WorldviewHandler) CreateWorldview(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Genre       string `json:"genre"`
		Description string `json:"description"`
		MagicSystem string `json:"magic_system"`
		Geography   string `json:"geography"`
		Culture     string `json:"culture"`
		History     string `json:"history"`
		Technology  string `json:"technology"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	worldview := &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        req.Name,
		Genre:       req.Genre,
		Description: req.Description,
		MagicSystem: req.MagicSystem,
		Geography:   req.Geography,
		Culture:     req.Culture,
		History:     req.History,
		Technology:  req.Technology,
	}

	if err := h.worldviewService.CreateWorldview(worldview); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, worldview)
}

// UpdateWorldview 更新世界观
// PUT /api/v1/worldviews/:id
func (h *WorldviewHandler) UpdateWorldview(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid worldview id")
		return
	}

	worldview, err := h.worldviewService.GetWorldview(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "worldview not found")
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Genre       *string `json:"genre"`
		Description *string `json:"description"`
		MagicSystem *string `json:"magic_system"`
		Geography   *string `json:"geography"`
		Culture     *string `json:"culture"`
		History     *string `json:"history"`
		Technology  *string `json:"technology"`
		Rules       *string `json:"rules"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	if req.Name != nil {
		worldview.Name = *req.Name
	}
	if req.Genre != nil {
		worldview.Genre = *req.Genre
	}
	if req.Description != nil {
		worldview.Description = *req.Description
	}
	if req.MagicSystem != nil {
		worldview.MagicSystem = *req.MagicSystem
	}
	if req.Geography != nil {
		worldview.Geography = *req.Geography
	}
	if req.Culture != nil {
		worldview.Culture = *req.Culture
	}
	if req.History != nil {
		worldview.History = *req.History
	}
	if req.Technology != nil {
		worldview.Technology = *req.Technology
	}
	if req.Rules != nil {
		worldview.Rules = *req.Rules
	}

	if err := h.worldviewService.UpdateWorldview(worldview); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, worldview)
}

// DeleteWorldview 删除世界观
// DELETE /api/v1/worldviews/:id
func (h *WorldviewHandler) DeleteWorldview(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid worldview id")
		return
	}

	if err := h.worldviewService.DeleteWorldview(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateWorldview AI生成世界观
// POST /api/v1/worldviews/generate
func (h *WorldviewHandler) GenerateWorldview(c *gin.Context) {
	var req struct {
		Genre string   `json:"genre" binding:"required"`
		Hints []string `json:"hints"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	worldview, err := h.worldviewService.GenerateWorldview(getTenantID(c), req.Genre, req.Hints)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.worldviewService.CreateWorldview(worldview); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, worldview)
}

// ============================================
// WorldviewEntity CRUD
// ============================================

// ListEntities 获取世界观实体列表
// GET /api/v1/worldviews/:id/entities
func (h *WorldviewHandler) ListEntities(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid worldview id")
		return
	}
	entities, err := h.worldviewService.GetEntities(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, entities)
}

// CreateEntity 创建世界观实体
// POST /api/v1/worldviews/:id/entities
func (h *WorldviewHandler) CreateEntity(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid worldview id")
		return
	}
	var req struct {
		Type        string `json:"type" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		ImageURL    string `json:"image_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	entity := &model.WorldviewEntity{
		WorldviewID: uint(id),
		Type:        req.Type,
		Name:        req.Name,
		Description: req.Description,
		ImageURL:    req.ImageURL,
	}
	if err := h.worldviewService.CreateEntity(entity); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, entity)
}

// UpdateEntity 更新世界观实体
// PUT /api/v1/worldviews/:id/entities/:entity_id
func (h *WorldviewHandler) UpdateEntity(c *gin.Context) {
	entityID, err := strconv.ParseUint(c.Param("entity_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid entity id")
		return
	}
	entity, err := h.worldviewService.GetEntity(uint(entityID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "entity not found")
		return
	}
	var req struct {
		Type        *string `json:"type"`
		Name        *string `json:"name"`
		Description *string `json:"description"`
		ImageURL    *string `json:"image_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if req.Type != nil {
		entity.Type = *req.Type
	}
	if req.Name != nil {
		entity.Name = *req.Name
	}
	if req.Description != nil {
		entity.Description = *req.Description
	}
	if req.ImageURL != nil {
		entity.ImageURL = *req.ImageURL
	}
	if err := h.worldviewService.UpdateEntity(entity); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, entity)
}

// DeleteEntity 删除世界观实体
// DELETE /api/v1/worldviews/:id/entities/:entity_id
func (h *WorldviewHandler) DeleteEntity(c *gin.Context) {
	entityID, err := strconv.ParseUint(c.Param("entity_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid entity id")
		return
	}
	if err := h.worldviewService.DeleteEntity(uint(entityID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success"})
}
