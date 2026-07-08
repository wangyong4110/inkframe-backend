package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SysAdminHandler exposes system-admin REST endpoints.
type SysAdminHandler struct {
	svc      *service.SysAdminService
	mcpSvc   *service.McpService
	auditSvc *service.AuditService
}

// NewSysAdminHandler creates a new SysAdminHandler.
func NewSysAdminHandler(svc *service.SysAdminService) *SysAdminHandler {
	return &SysAdminHandler{svc: svc}
}

func (h *SysAdminHandler) WithAuditService(svc *service.AuditService) *SysAdminHandler {
	h.auditSvc = svc
	return h
}

func (h *SysAdminHandler) WithMcpService(svc *service.McpService) *SysAdminHandler {
	h.mcpSvc = svc
	return h
}

// GetOverview returns platform-wide statistics.
func (h *SysAdminHandler) GetOverview(c *gin.Context) {
	data, err := h.svc.GetOverview()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// ListTenants returns a paginated list of tenants.
func (h *SysAdminHandler) ListTenants(c *gin.Context) {
	p := parsePagination(c)
	search := c.Query("search")
	status := c.Query("status")
	tenants, total, err := h.svc.ListTenants(p.Page, p.PageSize, search, status)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": tenants, "total": total, "page": p.Page, "size": p.PageSize})
}

// GetTenant returns a single tenant by ID.
func (h *SysAdminHandler) GetTenant(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	t, err := h.svc.GetTenant(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, t)
}

// UpdateTenant updates a tenant's mutable fields.
func (h *SysAdminHandler) UpdateTenant(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var req service.UpdateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	t, err := h.svc.UpdateTenant(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.tenant_update",
			ResourceType: "tenant", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, t)
}

// DeleteTenant soft-deletes a tenant.
func (h *SysAdminHandler) DeleteTenant(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := h.svc.DeleteTenant(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.tenant_delete",
			ResourceType: "tenant", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "deleted"})
}

// ListUsers returns a paginated list of users.
func (h *SysAdminHandler) ListUsers(c *gin.Context) {
	p := parsePagination(c)
	search := c.Query("search")
	role := c.Query("role")
	users, total, err := h.svc.ListUsers(p.Page, p.PageSize, search, role)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": users, "total": total, "page": p.Page, "size": p.PageSize})
}

// GetUser returns a single user by ID.
func (h *SysAdminHandler) GetUser(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	u, err := h.svc.GetUser(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, u)
}

// UpdateUser updates a user's role and/or status.
func (h *SysAdminHandler) UpdateUser(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var req service.UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	u, err := h.svc.UpdateUser(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.user_update",
			ResourceType: "user", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, u)
}

// ImpersonateUser generates a short-lived token for the target user.
func (h *SysAdminHandler) ImpersonateUser(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	token, err := h.svc.ImpersonateUser(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.impersonate",
			ResourceType: "user", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"token": token, "expires_in": "1h"})
}

// ResetUserPassword resets the specified user's password.
func (h *SysAdminHandler) ResetUserPassword(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var req struct {
		Password string `json:"password" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.ResetUserPassword(uint(id), req.Password); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.user_reset_password",
			ResourceType: "user", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "password reset"})
}

// ListTasks returns a paginated list of async tasks.
func (h *SysAdminHandler) ListTasks(c *gin.Context) {
	p := parsePagination(c)
	status := c.Query("status")
	tasks, total, err := h.svc.ListTasks(p.Page, p.PageSize, status)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": tasks, "total": total, "page": p.Page, "size": p.PageSize})
}

// CancelTask cancels an async task by ID.
func (h *SysAdminHandler) CancelTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := h.svc.CancelTask(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "cancelled"})
}

// GetMetrics returns a runtime/business metrics snapshot for the admin dashboard.
func (h *SysAdminHandler) GetMetrics(c *gin.Context) {
	snap, err := h.svc.GetMetrics()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, snap)
}

// ListAuditLogs returns a paginated list of audit log entries.
func (h *SysAdminHandler) ListAuditLogs(c *gin.Context) {
	p := parsePagination(c)
	entityType := c.Query("entity_type")
	var userID uint
	if n, err := strconv.Atoi(c.Query("user_id")); err == nil {
		userID = uint(n)
	}
	logs, total, err := h.svc.ListAuditLogs(p.Page, p.PageSize, entityType, userID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": logs, "total": total, "page": p.Page, "size": p.PageSize})
}

// ListSettings returns all system settings.
func (h *SysAdminHandler) ListSettings(c *gin.Context) {
	settings, err := h.svc.ListSettings()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, settings)
}

// UpdateSettings upserts system settings.
func (h *SysAdminHandler) UpdateSettings(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.UpdateSettings(req); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.settings_update",
			ResourceType: "system", IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "updated"})
}

// ListNovels returns a paginated list of all novels across tenants.
func (h *SysAdminHandler) ListNovels(c *gin.Context) {
	p := parsePagination(c)
	search := c.Query("search")
	novels, total, err := h.svc.ListNovels(p.Page, p.PageSize, search)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": novels, "total": total, "page": p.Page, "size": p.PageSize})
}

// GetAssetGovernance returns per-tenant storage usage statistics.
func (h *SysAdminHandler) GetAssetGovernance(c *gin.Context) {
	data, err := h.svc.GetAssetGovernance()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// GetAIInfra returns AI provider/model statistics.
func (h *SysAdminHandler) GetAIInfra(c *gin.Context) {
	stats, err := h.svc.GetAIInfraStats()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, stats)
}

// BroadcastNotification sends a notification to all active users.
func (h *SysAdminHandler) BroadcastNotification(c *gin.Context) {
	var req struct {
		Title   string `json:"title" binding:"required"`
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.BroadcastNotification(req.Title, req.Content); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.broadcast",
			ResourceType: "notification", ResourceName: req.Title, IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "broadcast sent"})
}

// NotifyTenant sends a notification to all active users in a tenant.
func (h *SysAdminHandler) NotifyTenant(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var req struct {
		Title   string `json:"title" binding:"required"`
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.NotifyTenant(uint(id), req.Title, req.Content); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "sent"})
}

// ListExperiments returns a paginated list of AI model comparison experiments.
func (h *SysAdminHandler) ListExperiments(c *gin.Context) {
	p := parsePagination(c)
	data, total, err := h.svc.ListExperiments(p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": data, "total": total, "page": p.Page, "size": p.PageSize})
}

// GetTaskFailureStats returns failure statistics grouped by task type.
func (h *SysAdminHandler) GetTaskFailureStats(c *gin.Context) {
	data, err := h.svc.GetTaskFailureStats()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// GetUserRegistrationTrend returns daily user registration counts.
func (h *SysAdminHandler) GetUserRegistrationTrend(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	data, err := h.svc.GetUserRegistrationTrend(days)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// GetContentOverview returns platform-wide content engagement statistics.
func (h *SysAdminHandler) GetContentOverview(c *gin.Context) {
	data, err := h.svc.GetContentOverview()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// GetModelUsageStats returns aggregated AI model usage statistics.
func (h *SysAdminHandler) GetModelUsageStats(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	data, err := h.svc.GetModelUsageStats(days)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, data)
}

// ChangePassword changes the currently authenticated system admin's password.
func (h *SysAdminHandler) ChangePassword(c *gin.Context) {
	adminIDRaw, _ := c.Get("user_id")
	var req struct {
		Password string `json:"password" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	uid, _ := adminIDRaw.(uint)
	if err := h.svc.ChangeAdminPassword(uid, req.Password); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: uid, Action: "sysadmin.change_password",
			ResourceType: "user", ResourceID: uid, IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "password changed"})
}

// ──────────────────────────────────────────────────────────────────────────────
// Sysadmin MCP Tool Management
// ──────────────────────────────────────────────────────────────────────────────

func (h *SysAdminHandler) mcpGuard(c *gin.Context) bool {
	if h.mcpSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "MCP service not available")
		return false
	}
	return true
}

// AdminListMcpTools GET /sysadmin/mcp/tools
func (h *SysAdminHandler) AdminListMcpTools(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	tools, err := h.mcpSvc.AdminListTools()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, tools)
}

// AdminGetMcpTool GET /sysadmin/mcp/tools/:id
func (h *SysAdminHandler) AdminGetMcpTool(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tool, err := h.mcpSvc.AdminGetTool(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	respondOK(c, tool)
}

// AdminCreateMcpTool POST /sysadmin/mcp/tools
func (h *SysAdminHandler) AdminCreateMcpTool(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	var req model.AdminCreateMcpToolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	tool, err := h.mcpSvc.AdminCreateTool(&req)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.mcp_tool_create",
			ResourceType: "mcp_tool", ResourceID: tool.ID, IP: c.ClientIP(),
		})
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": tool})
}

// AdminUpdateMcpTool PUT /sysadmin/mcp/tools/:id
func (h *SysAdminHandler) AdminUpdateMcpTool(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.AdminUpdateMcpToolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	tool, err := h.mcpSvc.AdminUpdateTool(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.mcp_tool_update",
			ResourceType: "mcp_tool", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, tool)
}

// AdminDeleteMcpTool DELETE /sysadmin/mcp/tools/:id
func (h *SysAdminHandler) AdminDeleteMcpTool(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.mcpSvc.AdminDeleteTool(uint(id)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.mcp_tool_delete",
			ResourceType: "mcp_tool", ResourceID: uint(id), IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"message": "deleted"})
}

// AdminTestMcpTool POST /sysadmin/mcp/tools/:id/test
func (h *SysAdminHandler) AdminTestMcpTool(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	result, err := h.mcpSvc.TestTool(uint(id))
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, result)
}

// ──────────────────────────────────────────────────────────────────────────────
// Sysadmin MCP Feature Binding
// ──────────────────────────────────────────────────────────────────────────────

// AdminListFeatureBindings GET /sysadmin/mcp/features
func (h *SysAdminHandler) AdminListFeatureBindings(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	list, err := h.mcpSvc.ListFeatureBindings()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, list)
}

// AdminUpsertFeatureBinding PUT /sysadmin/mcp/features/:key
func (h *SysAdminHandler) AdminUpsertFeatureBinding(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	featureKey := c.Param("key")
	var req model.UpdateFeatureBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	dto, err := h.mcpSvc.UpsertFeatureBinding(featureKey, &req)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			UserID: getUserID(c), Action: "sysadmin.mcp_feature_bind",
			ResourceType: "mcp_feature", IP: c.ClientIP(),
		})
	}
	respondOK(c, dto)
}

// AdminDeleteFeatureBinding DELETE /sysadmin/mcp/features/:key
func (h *SysAdminHandler) AdminDeleteFeatureBinding(c *gin.Context) {
	if !h.mcpGuard(c) {
		return
	}
	featureKey := c.Param("key")
	// 删除绑定 = 恢复内置默认（直接删除记录）
	req := &model.UpdateFeatureBindingRequest{McpToolID: nil, Enabled: true}
	dto, err := h.mcpSvc.UpsertFeatureBinding(featureKey, req)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, dto)
}

// AdminListSystemFeatures GET /sysadmin/mcp/system-features
func (h *SysAdminHandler) AdminListSystemFeatures(c *gin.Context) {
	respondOK(c, service.SystemFeatures)
}

