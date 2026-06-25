package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// WebhookHandler handles webhook subscription management.
type WebhookHandler struct {
	svc      *service.WebhookService
	auditSvc *service.AuditService
}

// NewWebhookHandler creates a new WebhookHandler.
func NewWebhookHandler(svc *service.WebhookService) *WebhookHandler {
	return &WebhookHandler{svc: svc}
}

func (h *WebhookHandler) WithAuditService(svc *service.AuditService) *WebhookHandler {
	h.auditSvc = svc
	return h
}

// List GET /webhooks — list subscriptions for current tenant
func (h *WebhookHandler) List(c *gin.Context) {
	tenantID := getTenantID(c)
	subs, err := h.svc.ListSubscriptions(tenantID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": subs})
}

// Create POST /webhooks — create subscription
func (h *WebhookHandler) Create(c *gin.Context) {
	tenantID := getTenantID(c)

	var req struct {
		URL    string   `json:"url" binding:"required"`
		Secret string   `json:"secret"`
		Events []string `json:"events"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if !strings.HasPrefix(req.URL, "https://") {
		respondBadRequest(c, "webhook URL must start with https://")
		return
	}

	sub, err := h.svc.CreateSubscription(tenantID, req.URL, req.Secret, req.Events)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "webhook.create", ResourceType: "webhook",
			ResourceID: sub.ID, ResourceName: req.URL, IP: c.ClientIP(),
		})
	}
	respondCreated(c, sub)
}

// Delete DELETE /webhooks/:id — delete a subscription (must own it)
func (h *WebhookHandler) Delete(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	if err := h.svc.DeleteSubscription(id, tenantID); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c),
			Action: "webhook.delete", ResourceType: "webhook", ResourceID: uint(id),
			IP: c.ClientIP(),
		})
	}
	respondOK(c, nil)
}

// TestWebhook POST /webhooks/:id/test — send a test delivery to the subscription
func (h *WebhookHandler) TestWebhook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	if err := h.svc.TestWebhook(id, tenantID); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "test delivery successful"})
}
