package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

type FeedbackHandler struct {
	svc *service.FeedbackService
}

func NewFeedbackHandler(svc *service.FeedbackService) *FeedbackHandler {
	return &FeedbackHandler{svc: svc}
}

func (h *FeedbackHandler) Submit(c *gin.Context) {
	var req model.CreateFeedbackRequest
	if !bindJSON(c, &req) {
		return
	}
	userID := getUserID(c)
	tenantID := getTenantID(c)
	f, err := h.svc.Create(&req, userID, tenantID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, f)
}

func (h *FeedbackHandler) ListMyFeedback(c *gin.Context) {
	userID := getUserID(c)
	p := parsePagination(c)
	items, total, err := h.svc.ListForUser(userID, p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": items, "total": total, "page": p.Page, "size": p.PageSize})
}

func (h *FeedbackHandler) AdminList(c *gin.Context) {
	p := parsePagination(c)
	status := c.Query("status")
	typ := c.Query("type")
	priority := c.Query("priority")
	items, total, err := h.svc.ListForAdmin(p.Page, p.PageSize, status, typ, priority)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": items, "total": total, "page": p.Page, "size": p.PageSize})
}

func (h *FeedbackHandler) AdminGet(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	f, err := h.svc.GetByID(id)
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, f)
}

func (h *FeedbackHandler) AdminUpdate(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.UpdateFeedbackRequest
	if !bindJSON(c, &req) {
		return
	}
	f, err := h.svc.UpdateStatus(id, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, f)
}

func (h *FeedbackHandler) AdminReply(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.ReplyFeedbackRequest
	if !bindJSON(c, &req) {
		return
	}
	f, err := h.svc.Reply(id, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, f)
}

func (h *FeedbackHandler) AdminStats(c *gin.Context) {
	stats, err := h.svc.GetStats()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, stats)
}
