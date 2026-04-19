package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// TenantHandler 租户处理器
type TenantHandler struct {
	tenantService *service.TenantService
	projectService *service.ProjectService
}

func NewTenantHandler(
	tenantService *service.TenantService,
	projectService *service.ProjectService,
) *TenantHandler {
	return &TenantHandler{
		tenantService:  tenantService,
		projectService: projectService,
	}
}

// ============================================
// 租户管理 API
// ============================================

// CreateTenant 创建租户
// POST /api/v1/tenants
func (h *TenantHandler) CreateTenant(c *gin.Context) {
	var req service.CreateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tenant, err := h.tenantService.CreateTenant(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    tenant,
	})
}

// GetTenant 获取租户
// GET /api/v1/tenants/:id
func (h *TenantHandler) GetTenant(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	tenant, err := h.tenantService.GetTenant(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    tenant,
	})
}

// GetTenantByCode 通过编码获取租户
// GET /api/v1/tenants/code/:code
func (h *TenantHandler) GetTenantByCode(c *gin.Context) {
	code := c.Param("code")

	tenant, err := h.tenantService.GetTenantByCode(code)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    tenant,
	})
}

// UpdateTenant 更新租户
// PUT /api/v1/tenants/:id
func (h *TenantHandler) UpdateTenant(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	var req service.UpdateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tenant, err := h.tenantService.UpdateTenant(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    tenant,
	})
}

// DeleteTenant 删除租户
// DELETE /api/v1/tenants/:id
func (h *TenantHandler) DeleteTenant(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	if err := h.tenantService.DeleteTenant(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// ListTenants 列出租户
// GET /api/v1/tenants
func (h *TenantHandler) ListTenants(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	tenants, total, err := h.tenantService.ListTenants(page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"items":      tenants,
			"total":      total,
			"page":       page,
			"page_size":  pageSize,
			"total_page": (total + int64(pageSize) - 1) / int64(pageSize),
		},
	})
}

// GetQuota 获取租户配额
// GET /api/v1/tenants/:id/quota
func (h *TenantHandler) GetQuota(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	quota, err := h.tenantService.CheckQuota(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    quota,
	})
}

// ============================================
// 租户成员管理 API
// ============================================

// AddMember 添加成员
// POST /api/v1/tenants/:id/members
func (h *TenantHandler) AddMember(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	var req service.AddTenantMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.tenantService.AddMember(uint(id), &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
	})
}

// RemoveMember 移除成员
// DELETE /api/v1/tenants/:id/members/:user_id
func (h *TenantHandler) RemoveMember(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	userID, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)

	if err := h.tenantService.RemoveMember(uint(tenantID), uint(userID)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetMembers 获取成员列表
// GET /api/v1/tenants/:id/members
func (h *TenantHandler) GetMembers(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	members, err := h.tenantService.GetMembers(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    members,
	})
}

// UpdateMemberRole 更新成员角色
// PUT /api/v1/tenants/:id/members/:user_id
func (h *TenantHandler) UpdateMemberRole(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	userID, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)

	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.tenantService.UpdateMemberRole(uint(tenantID), uint(userID), req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// ============================================
// 项目管理 API
// ============================================

// CreateProject 创建项目
// POST /api/v1/tenants/:id/projects
func (h *TenantHandler) CreateProject(c *gin.Context) {
	tenantID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	var req service.CreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	project, err := h.projectService.CreateProject(uint(tenantID), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    project,
	})
}

// GetProject 获取项目
// GET /api/v1/tenants/:id/projects/:project_id
func (h *TenantHandler) GetProject(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	projectID, _ := strconv.ParseUint(c.Param("project_id"), 10, 32)

	project, err := h.projectService.GetProject(uint(tenantID), uint(projectID))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    project,
	})
}

// ListProjects 列出项目
// GET /api/v1/tenants/:id/projects
func (h *TenantHandler) ListProjects(c *gin.Context) {
	tenantID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	projects, err := h.projectService.ListProjects(uint(tenantID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    projects,
	})
}

// UpdateProject 更新项目
// PUT /api/v1/tenants/:id/projects/:project_id
func (h *TenantHandler) UpdateProject(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	projectID, _ := strconv.ParseUint(c.Param("project_id"), 10, 32)

	var req service.UpdateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	project, err := h.projectService.UpdateProject(uint(tenantID), uint(projectID), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    project,
	})
}

// DeleteProject 删除项目
// DELETE /api/v1/tenants/:id/projects/:project_id
func (h *TenantHandler) DeleteProject(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	projectID, _ := strconv.ParseUint(c.Param("project_id"), 10, 32)

	if err := h.projectService.DeleteProject(uint(tenantID), uint(projectID)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetProjectStats 获取项目统计
// GET /api/v1/tenants/:id/projects/:project_id/stats
func (h *TenantHandler) GetProjectStats(c *gin.Context) {
	tenantID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	projectID, _ := strconv.ParseUint(c.Param("project_id"), 10, 32)

	stats, err := h.projectService.GetProjectStats(uint(tenantID), uint(projectID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    stats,
	})
}
