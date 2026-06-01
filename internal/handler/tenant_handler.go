package handler

import (
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// tenantCodeRe validates tenant codes: 2-32 alphanumeric chars, hyphens, underscores.
var tenantCodeRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

// TenantHandler 租户管理处理器
type TenantHandler struct {
	tenantService *service.TenantService
}

func NewTenantHandler(tenantService *service.TenantService) *TenantHandler {
	return &TenantHandler{tenantService: tenantService}
}

// ListTenants 获取租户列表（仅 admin 可查看所有租户）
// GET /api/v1/tenants
func (h *TenantHandler) ListTenants(c *gin.Context) {
	if c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden: admin only")
		return
	}

	p := parsePagination(c)

	tenants, total, err := h.tenantService.ListTenants(p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"items":      tenants,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (int(total) + p.PageSize - 1) / p.PageSize,
	})
}

// GetTenant 获取租户详情
// GET /api/v1/tenants/:id
func (h *TenantHandler) GetTenant(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if getTenantID(c) != id && c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	tenant, err := h.tenantService.GetTenant(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "tenant not found")
		return
	}

	respondOK(c, tenant)
}

// CreateTenant 创建租户
// POST /api/v1/tenants
func (h *TenantHandler) CreateTenant(c *gin.Context) {
	var req struct {
		Name         string `json:"name" binding:"required"`
		Code         string `json:"code" binding:"required"`
		Plan         string `json:"plan"`
		MaxUsers     int    `json:"max_users"`
		Description  string `json:"description"`
		ContactEmail string `json:"contact_email"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if !tenantCodeRe.MatchString(req.Code) {
		respondErr(c, http.StatusBadRequest, "tenant code must be 2-32 alphanumeric chars")
		return
	}

	if req.Plan == "" {
		req.Plan = "free"
	}

	tenant := &model.Tenant{
		Name:   req.Name,
		Code:   req.Code,
		Plan:   req.Plan,
		Status: "active",
	}
	quota := model.TenantQuota{
		MaxProjects:  5,
		MaxStorageMB: 1000,
		MaxUsers:     3,
		BillingCycle: "monthly",
	}
	if req.MaxUsers > 0 {
		quota.MaxUsers = req.MaxUsers
	}
	tenant.SetQuota(quota)
	tenant.SetProfile(model.TenantProfile{
		Description:  req.Description,
		ContactEmail: req.ContactEmail,
	})

	if err := h.tenantService.CreateTenant(tenant); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, tenant)
}

// UpdateTenant 更新租户
// PUT /api/v1/tenants/:id
func (h *TenantHandler) UpdateTenant(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if getTenantID(c) != id && c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	tenant, err := h.tenantService.GetTenant(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "tenant not found")
		return
	}

	var req struct {
		Name         string `json:"name"`
		Plan         string `json:"plan"`
		Status       string `json:"status"`
		MaxUsers     int    `json:"max_users"`
		Description  string `json:"description"`
		ContactEmail string `json:"contact_email"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if req.Name != "" {
		tenant.Name = req.Name
	}
	if req.Plan != "" {
		tenant.Plan = req.Plan
	}
	if req.Status != "" {
		tenant.Status = req.Status
	}
	if req.MaxUsers > 0 || req.Description != "" || req.ContactEmail != "" {
		quota := tenant.GetQuota()
		if req.MaxUsers > 0 {
			quota.MaxUsers = req.MaxUsers
		}
		tenant.SetQuota(quota)

		profile := tenant.GetProfile()
		if req.Description != "" {
			profile.Description = req.Description
		}
		if req.ContactEmail != "" {
			profile.ContactEmail = req.ContactEmail
		}
		tenant.SetProfile(profile)
	}

	if err := h.tenantService.UpdateTenant(tenant); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Invalidate subscription cache so next request re-reads the updated status/plan/expiry.
	middleware.InvalidateTenantSubCache(uint(id))

	respondOK(c, tenant)
}

// DeleteTenant 删除租户（需要 admin 角色或本租户成员）
// DELETE /api/v1/tenants/:id
func (h *TenantHandler) DeleteTenant(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if c.GetString("user_role") != "admin" && getTenantID(c) != id {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	if err := h.tenantService.DeleteTenant(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetQuota 获取租户配额
// GET /api/v1/tenants/:id/quota
func (h *TenantHandler) GetQuota(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if getTenantID(c) != id && c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	quota, err := h.tenantService.GetQuota(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, quota)
}

// ListMembers 获取租户成员列表
// GET /api/v1/tenants/:id/members
func (h *TenantHandler) ListMembers(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if getTenantID(c) != id && c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	members, err := h.tenantService.ListMembers(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, members)
}

// AddMember 添加租户成员
// POST /api/v1/tenants/:id/members
func (h *TenantHandler) AddMember(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !h.isAdminOrOwnerOfTenant(c, id) {
		respondErr(c, http.StatusForbidden, "forbidden: admin or owner required")
		return
	}

	var req struct {
		UserID uint   `json:"user_id" binding:"required"`
		Role   string `json:"role"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}

	if err := h.tenantService.AddMember(uint(id), req.UserID, req.Role); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// RemoveMember 移除租户成员
// DELETE /api/v1/tenants/:id/members/:user_id
func (h *TenantHandler) RemoveMember(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	userId, ok := parseID(c, "user_id")
	if !ok {
		return
	}

	if getTenantID(c) != id && c.GetString("user_role") != "admin" {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	if err := h.tenantService.RemoveMember(uint(id), uint(userId)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// UpdateMemberRole 更新成员角色
// PUT /api/v1/tenants/:id/members/:user_id/role
func (h *TenantHandler) UpdateMemberRole(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	userId, ok := parseID(c, "user_id")
	if !ok {
		return
	}

	if !h.isAdminOrOwnerOfTenant(c, id) {
		respondErr(c, http.StatusForbidden, "forbidden: admin or owner required")
		return
	}

	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if err := h.tenantService.UpdateMemberRole(uint(id), uint(userId), req.Role); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// isAdminOrOwnerOfTenant returns true if the requesting user is:
//   - a system-level admin (user_role == "admin"), OR
//   - an owner or admin of the specified tenant.
func (h *TenantHandler) isAdminOrOwnerOfTenant(c *gin.Context, tenantID uint) bool {
	if c.GetString("user_role") == "admin" {
		return true
	}
	userID, _ := c.Get("user_id")
	uid, ok := userID.(uint)
	if !ok || uid == 0 {
		return false
	}
	role, err := h.tenantService.GetMemberRole(tenantID, uid)
	if err != nil {
		return false
	}
	return role == "owner" || role == "admin"
}
