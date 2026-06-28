package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

type DramaTemplateHandler struct {
	svc *service.DramaTemplateService
}

func NewDramaTemplateHandler(svc *service.DramaTemplateService) *DramaTemplateHandler {
	return &DramaTemplateHandler{svc: svc}
}

// ListTemplates GET /drama-templates
func (h *DramaTemplateHandler) ListTemplates(c *gin.Context) {
	templates, err := h.svc.List()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, templates)
}

// GetTemplate GET /drama-templates/:id
func (h *DramaTemplateHandler) GetTemplate(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	t, err := h.svc.GetByID(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, t)
}

// CreateTemplate POST /drama-templates
func (h *DramaTemplateHandler) CreateTemplate(c *gin.Context) {
	var t model.DramaTemplate
	if err := c.ShouldBindJSON(&t); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	t.IsBuiltin = false
	if err := h.svc.Create(&t); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, t)
}

// UpdateTemplate PUT /drama-templates/:id
func (h *DramaTemplateHandler) UpdateTemplate(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	var t model.DramaTemplate
	if err := c.ShouldBindJSON(&t); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.Update(uint(id), &t); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, t)
}

// DeleteTemplate DELETE /drama-templates/:id
func (h *DramaTemplateHandler) DeleteTemplate(c *gin.Context) {
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
