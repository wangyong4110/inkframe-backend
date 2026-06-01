package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// AuditHandler handles audit log queries.
type AuditHandler struct {
	svc *service.AuditService
}

// NewAuditHandler creates a new AuditHandler.
func NewAuditHandler(svc *service.AuditService) *AuditHandler {
	return &AuditHandler{svc: svc}
}

// List GET /audit-logs?action=&page=&page_size= — list audit logs for current tenant
func (h *AuditHandler) List(c *gin.Context) {
	tenantID := getTenantID(c)
	action := c.Query("action")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	logs, total, err := h.svc.List(tenantID, action, page, pageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": logs, "total": total})
}
