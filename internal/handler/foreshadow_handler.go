package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ForeshadowHandler 伏笔管理处理器
type ForeshadowHandler struct {
	svc *service.ForeshadowCRUDService
}

func NewForeshadowHandler(svc *service.ForeshadowCRUDService) *ForeshadowHandler {
	return &ForeshadowHandler{svc: svc}
}

// ListForeshadows GET /novels/:id/foreshadows
func (h *ForeshadowHandler) ListForeshadows(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.ListByNovel(c.Request.Context(), novelID)
	if err != nil {
		logger.Printf("[ForeshadowHandler] ListForeshadows: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to list foreshadows")
		return
	}
	respondOK(c, gin.H{"foreshadows": list, "total": len(list)})
}

// CreateForeshadow POST /novels/:id/foreshadows
func (h *ForeshadowHandler) CreateForeshadow(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var f model.Foreshadow
	if !bindJSON(c, &f) {
		return
	}
	if f.Title == "" {
		respondBadRequest(c, "title is required")
		return
	}
	f.NovelID = novelID
	f.TenantID = getTenantID(c)
	if f.Status == "" {
		f.Status = "planted"
	}
	if err := h.svc.Create(c.Request.Context(), &f); err != nil {
		logger.Printf("[ForeshadowHandler] CreateForeshadow: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to create foreshadow")
		return
	}
	respondCreated(c, f)
}

// ListUnfulfilledForeshadows GET /novels/:id/foreshadows/unfulfilled
func (h *ForeshadowHandler) ListUnfulfilledForeshadows(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.ListUnfulfilled(c.Request.Context(), novelID)
	if err != nil {
		logger.Printf("[ForeshadowHandler] ListUnfulfilled: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to list unfulfilled foreshadows")
		return
	}
	respondOK(c, gin.H{"foreshadows": list, "total": len(list)})
}

// UpdateForeshadow PUT /novels/:id/foreshadows/:foreshadow_id
func (h *ForeshadowHandler) UpdateForeshadow(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	foreshadowID, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}
	var updates map[string]interface{}
	if !bindJSON(c, &updates) {
		return
	}
	f, err := h.svc.Update(c.Request.Context(), foreshadowID, updates)
	if err != nil {
		logger.Printf("[ForeshadowHandler] UpdateForeshadow: id=%d err=%v", foreshadowID, err)
		respondErr(c, http.StatusInternalServerError, "failed to update foreshadow")
		return
	}
	respondOK(c, f)
}

// DeleteForeshadow DELETE /novels/:id/foreshadows/:foreshadow_id
func (h *ForeshadowHandler) DeleteForeshadow(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	foreshadowID, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), foreshadowID); err != nil {
		logger.Printf("[ForeshadowHandler] DeleteForeshadow: id=%d err=%v", foreshadowID, err)
		respondErr(c, http.StatusInternalServerError, "failed to delete foreshadow")
		return
	}
	respondOK(c, gin.H{"deleted": true})
}
