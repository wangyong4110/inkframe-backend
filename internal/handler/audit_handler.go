package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// AuditHandler handles audit log queries.
type AuditHandler struct {
	svc      *service.AuditService
	novelRepo *repository.NovelRepository
}

// NewAuditHandler creates a new AuditHandler.
func NewAuditHandler(svc *service.AuditService) *AuditHandler {
	return &AuditHandler{svc: svc}
}

// WithNovelRepo injects the NovelRepository for permission checks.
func (h *AuditHandler) WithNovelRepo(repo *repository.NovelRepository) *AuditHandler {
	h.novelRepo = repo
	return h
}

// List GET /audit-logs?action=&page=&page_size= — list audit logs for current tenant (legacy)
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

// ListNovelLogs GET /novels/:id/audit-logs?page=1&page_size=20
// Returns project-level audit logs; requires the novel to belong to caller's tenant.
func (h *AuditHandler) ListNovelLogs(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}

	tenantID := getTenantID(c)

	// Permission check: verify novel belongs to caller's tenant
	if h.novelRepo != nil {
		novel, err := h.novelRepo.GetByID(novelID)
		if err != nil || novel == nil {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
		if novel.TenantID != tenantID {
			respondErr(c, http.StatusForbidden, "access denied")
			return
		}
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	logs, total, err := h.svc.ListByNovel(novelID, tenantID, page, pageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"data":      logs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// ListMyLogs GET /users/me/audit-logs?page=1&page_size=20
// Returns user-level audit logs for the currently authenticated user.
func (h *AuditHandler) ListMyLogs(c *gin.Context) {
	userID := getUserID(c)
	if userID == 0 {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}

	tenantID := getTenantID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	logs, total, err := h.svc.ListByUser(userID, tenantID, page, pageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"data":      logs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}
