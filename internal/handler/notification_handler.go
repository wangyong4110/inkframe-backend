package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// NotificationHandler 站内通知处理器
type NotificationHandler struct {
	svc *service.NotificationService
}

// NewNotificationHandler 创建通知处理器
func NewNotificationHandler(svc *service.NotificationService) *NotificationHandler {
	return &NotificationHandler{svc: svc}
}

// List GET /notifications?unread=true&page=1&page_size=20
func (h *NotificationHandler) List(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	tenantID := c.MustGet("tenant_id").(uint)
	onlyUnread := c.Query("unread") == "true"
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	items, total, err := h.svc.List(userID, tenantID, onlyUnread, page, size)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": items, "total": total})
}

// UnreadCount GET /notifications/unread-count
func (h *NotificationHandler) UnreadCount(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	tenantID := c.MustGet("tenant_id").(uint)
	count, err := h.svc.UnreadCount(userID, tenantID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"count": count})
}

// MarkRead PUT /notifications/:id/read
func (h *NotificationHandler) MarkRead(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	userID := c.MustGet("user_id").(uint)
	if err := h.svc.MarkRead(id, userID); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// MarkAllRead PUT /notifications/read-all
func (h *NotificationHandler) MarkAllRead(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	tenantID := c.MustGet("tenant_id").(uint)
	if err := h.svc.MarkAllRead(userID, tenantID); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// Delete DELETE /notifications/:id
func (h *NotificationHandler) Delete(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	userID := c.MustGet("user_id").(uint)
	if err := h.svc.Delete(id, userID); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}
