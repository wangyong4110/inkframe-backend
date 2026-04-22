package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// TenantHandler 租户管理处理器
type TenantHandler struct {
	tenantService *service.TenantService
}

func NewTenantHandler(tenantService *service.TenantService) *TenantHandler {
	return &TenantHandler{tenantService: tenantService}
}

// ListTenants 获取租户列表
// GET /api/v1/tenants
func (h *TenantHandler) ListTenants(c *gin.Context) {
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
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
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	tenant := &model.Tenant{
		Name:         req.Name,
		Code:         req.Code,
		Plan:         req.Plan,
		MaxUsers:     req.MaxUsers,
		Description:  req.Description,
		ContactEmail: req.ContactEmail,
		Status:       "active",
	}
	if tenant.Plan == "" {
		tenant.Plan = "free"
	}
	if tenant.MaxUsers == 0 {
		tenant.MaxUsers = 3
	}

	if err := h.tenantService.CreateTenant(tenant); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, tenant)
}

// UpdateTenant 更新租户
// PUT /api/v1/tenants/:id
func (h *TenantHandler) UpdateTenant(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
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
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	if req.MaxUsers > 0 {
		tenant.MaxUsers = req.MaxUsers
	}
	if req.Description != "" {
		tenant.Description = req.Description
	}
	if req.ContactEmail != "" {
		tenant.ContactEmail = req.ContactEmail
	}

	if err := h.tenantService.UpdateTenant(tenant); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, tenant)
}

// DeleteTenant 删除租户
// DELETE /api/v1/tenants/:id
func (h *TenantHandler) DeleteTenant(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
		return
	}

	var req struct {
		UserID uint   `json:"user_id" binding:"required"`
		Role   string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
		return
	}
	userId, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid user id")
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid tenant id")
		return
	}
	userId, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid user id")
		return
	}

	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
