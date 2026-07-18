package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

type ImageStylePresetHandler struct {
	svc *service.ImageStylePresetService
}

func NewImageStylePresetHandler(svc *service.ImageStylePresetService) *ImageStylePresetHandler {
	return &ImageStylePresetHandler{svc: svc}
}

// ListImageStylePresets GET /image-style-presets
func (h *ImageStylePresetHandler) ListImageStylePresets(c *gin.Context) {
	presets, err := h.svc.List()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, presets)
}

// GetImageStylePreset GET /image-style-presets/:id
func (h *ImageStylePresetHandler) GetImageStylePreset(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	p, err := h.svc.GetByID(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, p)
}

// CreateImageStylePreset POST /image-style-presets
func (h *ImageStylePresetHandler) CreateImageStylePreset(c *gin.Context) {
	var p model.ImageStylePreset
	if err := c.ShouldBindJSON(&p); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	p.IsBuiltin = false
	if err := h.svc.Create(&p); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, p)
}

// UpdateImageStylePreset PUT /image-style-presets/:id
func (h *ImageStylePresetHandler) UpdateImageStylePreset(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	var p model.ImageStylePreset
	if err := c.ShouldBindJSON(&p); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.Update(uint(id), &p); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, p)
}

// DeleteImageStylePreset DELETE /image-style-presets/:id
func (h *ImageStylePresetHandler) DeleteImageStylePreset(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if err := h.svc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"deleted": true})
}
