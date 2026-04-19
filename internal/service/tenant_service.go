package service

import (
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/repository"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// ============================================
// 租户服务 - Tenant Service
// ============================================

// TenantService 租户服务
type TenantService struct {
	tenantRepo *TenantRepository
	userRepo   *UserRepository
}

func NewTenantService(tenantRepo *TenantRepository, userRepo *UserRepository) *TenantService {
	return &TenantService{
		tenantRepo: tenantRepo,
		userRepo:   userRepo,
	}
}

// CreateTenant 创建租户
func (s *TenantService) CreateTenant(req *CreateTenantRequest) (*model.Tenant, error) {
	// 检查编码是否唯一
	existing, _ := s.tenantRepo.GetByCode(req.Code)
	if existing != nil {
		return nil, fmt.Errorf("租户编码已存在")
	}

	tenant := &model.Tenant{
		Name:         req.Name,
		Code:         req.Code,
		Plan:         req.Plan,
		Status:       "active",
		MaxProjects:  req.MaxProjects,
		MaxUsers:     req.MaxUsers,
		MaxStorageMB: req.MaxStorageMB,
	}

	if req.Settings != nil {
		settings, _ := json.Marshal(req.Settings)
		tenant.Settings = string(settings)
	}

	if err := s.tenantRepo.Create(tenant); err != nil {
		return nil, err
	}

	return tenant, nil
}

// GetTenant 获取租户
func (s *TenantService) GetTenant(id uint) (*model.Tenant, error) {
	return s.tenantRepo.GetByID(id)
}

// GetTenantByCode 通过编码获取租户
func (s *TenantService) GetTenantByCode(code string) (*model.Tenant, error) {
	return s.tenantRepo.GetByCode(code)
}

// UpdateTenant 更新租户
func (s *TenantService) UpdateTenant(id uint, req *UpdateTenantRequest) (*model.Tenant, error) {
	tenant, err := s.tenantRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	if req.Name != "" {
		tenant.Name = req.Name
	}
	if req.Logo != "" {
		tenant.Logo = req.Logo
	}
	if req.Description != "" {
		tenant.Description = req.Description
	}
	if req.ContactEmail != "" {
		tenant.ContactEmail = req.ContactEmail
	}
	if req.ContactPhone != "" {
		tenant.ContactPhone = req.ContactPhone
	}

	if err := s.tenantRepo.Update(tenant); err != nil {
		return nil, err
	}

	return tenant, nil
}

// DeleteTenant 删除租户
func (s *TenantService) DeleteTenant(id uint) error {
	return s.tenantRepo.Delete(id)
}

// ListTenants 列出租户
func (s *TenantService) ListTenants(page, pageSize int) ([]*model.Tenant, int64, error) {
	return s.tenantRepo.List(page, pageSize)
}

// 添加租户成员
func (s *TenantService) AddMember(tenantID uint, req *AddTenantMemberRequest) error {
	member := &model.TenantUser{
		TenantID: tenantID,
		UserID:   req.UserID,
		Role:     req.Role,
		Nickname: req.Nickname,
		Status:   "active",
	}

	return s.tenantRepo.AddMember(member)
}

// 移除租户成员
func (s *TenantService) RemoveMember(tenantID, userID uint) error {
	return s.tenantRepo.RemoveMember(tenantID, userID)
}

// 获取租户成员列表
func (s *TenantService) GetMembers(tenantID uint) ([]*model.TenantUser, error) {
	return s.tenantRepo.GetMembers(tenantID)
}

// 更新成员角色
func (s *TenantService) UpdateMemberRole(tenantID, userID uint, role string) error {
	return s.tenantRepo.UpdateMemberRole(tenantID, userID, role)
}

// 检查配额
func (s *TenantService) CheckQuota(tenantID uint) (*QuotaInfo, error) {
	tenant, err := s.tenantRepo.GetByID(tenantID)
	if err != nil {
		return nil, err
	}

	return &QuotaInfo{
		Projects: Quota{
			Used:  tenant.UsedProjects,
			Limit: tenant.MaxProjects,
		},
		Users: Quota{
			Used:  tenant.UsedUsers,
			Limit: tenant.MaxUsers,
		},
		StorageMB: Quota{
			Used:  tenant.UsedStorageMB,
			Limit: tenant.MaxStorageMB,
		},
	}, nil
}

// 更新配额使用
func (s *TenantService) UpdateUsage(tenantID uint, projectDelta, userDelta, storageDelta int) error {
	return s.tenantRepo.UpdateUsage(tenantID, projectDelta, userDelta, storageDelta)
}

// ============================================
// 多项目管理服务
// ============================================

// ProjectService 项目服务
type ProjectService struct {
	tenantRepo  *TenantRepository
	projectRepo *ProjectRepository
	novelRepo   *repository.NovelRepository
}

func NewProjectService(
	tenantRepo *TenantRepository,
	projectRepo *ProjectRepository,
	novelRepo *repository.NovelRepository,
) *ProjectService {
	return &ProjectService{
		tenantRepo:  tenantRepo,
		projectRepo: projectRepo,
		novelRepo:   novelRepo,
	}
}

// CreateProject 创建项目
func (s *ProjectService) CreateProject(tenantID uint, req *CreateProjectRequest) (*model.TenantProject, error) {
	// 检查租户配额
	tenant, err := s.tenantRepo.GetByID(tenantID)
	if err != nil {
		return nil, fmt.Errorf("租户不存在")
	}

	if tenant.UsedProjects >= tenant.MaxProjects {
		return nil, fmt.Errorf("项目数量已达上限 (%d/%d)", tenant.UsedProjects, tenant.MaxProjects)
	}

	project := &model.TenantProject{
		TenantID:    tenantID,
		ProjectType: req.Type,
		Name:        req.Name,
		Status:      "active",
	}

	if req.Settings != nil {
		settings, _ := json.Marshal(req.Settings)
		project.Settings = string(settings)
	}
	if req.Tags != nil {
		tags, _ := json.Marshal(req.Tags)
		project.Tags = string(tags)
	}

	if err := s.projectRepo.Create(project); err != nil {
		return nil, err
	}

	// 更新租户配额
	s.tenantRepo.UpdateUsage(tenantID, 1, 0, 0)

	return project, nil
}

// GetProject 获取项目
func (s *ProjectService) GetProject(tenantID, projectID uint) (*model.TenantProject, error) {
	return s.projectRepo.GetByID(tenantID, projectID)
}

// ListProjects 列出项目
func (s *ProjectService) ListProjects(tenantID uint) ([]*model.TenantProject, error) {
	return s.projectRepo.ListByTenant(tenantID)
}

// UpdateProject 更新项目
func (s *ProjectService) UpdateProject(tenantID, projectID uint, req *UpdateProjectRequest) (*model.TenantProject, error) {
	project, err := s.projectRepo.GetByID(tenantID, projectID)
	if err != nil {
		return nil, err
	}

	if req.Name != "" {
		project.Name = req.Name
	}
	if req.Status != "" {
		project.Status = req.Status
	}

	if err := s.projectRepo.Update(project); err != nil {
		return nil, err
	}

	return project, nil
}

// DeleteProject 删除项目
func (s *ProjectService) DeleteProject(tenantID, projectID uint) error {
	project, err := s.projectRepo.GetByID(tenantID, projectID)
	if err != nil {
		return err
	}

	// 如果项目下有小说，先处理
	if project.ProjectType == "novel" {
		// 查询该项目的所有小说并删除或转移
		novels, _ := s.novelRepo.ListByTenantAndProject(tenantID, projectID)
		for _, novel := range novels {
			s.novelRepo.Delete(novel.ID)
		}
	}

	// 更新租户配额
	s.tenantRepo.UpdateUsage(tenantID, -1, 0, 0)

	return s.projectRepo.Delete(projectID)
}

// GetProjectStats 获取项目统计
func (s *ProjectService) GetProjectStats(tenantID, projectID uint) (*ProjectStats, error) {
	project, err := s.projectRepo.GetByID(tenantID, projectID)
	if err != nil {
		return nil, err
	}

	stats := &ProjectStats{
		Project: project,
	}

	if project.ProjectType == "novel" {
		novels, _ := s.novelRepo.ListByTenantAndProject(tenantID, projectID)
		stats.NovelCount = len(novels)

		totalWords := 0
		for _, novel := range novels {
			totalWords += novel.TotalWords
		}
		stats.TotalWords = totalWords
	}

	return stats, nil
}

// ============================================
// 请求/响应结构
// ============================================

// 创建租户请求
type CreateTenantRequest struct {
	Name         string                 `json:"name" binding:"required"`
	Code         string                 `json:"code" binding:"required"`
	Plan         string                 `json:"plan"`
	MaxProjects  int                    `json:"max_projects"`
	MaxUsers     int                    `json:"max_users"`
	MaxStorageMB int                    `json:"max_storage_mb"`
	Settings     map[string]interface{} `json:"settings"`
}

// 更新租户请求
type UpdateTenantRequest struct {
	Name         string `json:"name"`
	Logo         string `json:"logo"`
	Description  string `json:"description"`
	ContactEmail string `json:"contact_email"`
	ContactPhone string `json:"contact_phone"`
}

// 添加租户成员请求
type AddTenantMemberRequest struct {
	UserID   uint   `json:"user_id" binding:"required"`
	Role     string `json:"role"`
	Nickname string `json:"nickname"`
}

// 创建项目请求
type CreateProjectRequest struct {
	Name     string                 `json:"name" binding:"required"`
	Type     string                 `json:"type"` // novel, custom
	Settings map[string]interface{} `json:"settings"`
	Tags     []string               `json:"tags"`
}

// 更新项目请求
type UpdateProjectRequest struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// 配额信息
type QuotaInfo struct {
	Projects  Quota `json:"projects"`
	Users     Quota `json:"users"`
	StorageMB Quota `json:"storage_mb"`
}

type Quota struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// 项目统计
type ProjectStats struct {
	Project    *model.TenantProject `json:"project"`
	NovelCount int                  `json:"novel_count"`
	TotalWords int                  `json:"total_words"`
}

// ============================================
// Repository 接口
// ============================================

// TenantRepository 租户仓库接口
type TenantRepository interface {
	Create(tenant *model.Tenant) error
	GetByID(id uint) (*model.Tenant, error)
	GetByCode(code string) (*model.Tenant, error)
	Update(tenant *model.Tenant) error
	Delete(id uint) error
	List(page, pageSize int) ([]*model.Tenant, int64, error)
	AddMember(member *model.TenantUser) error
	RemoveMember(tenantID, userID uint) error
	GetMembers(tenantID uint) ([]*model.TenantUser, error)
	UpdateMemberRole(tenantID, userID uint, role string) error
	UpdateUsage(tenantID uint, projectDelta, userDelta, storageDelta int) error
}

// ProjectRepository 项目仓库接口
type ProjectRepository interface {
	Create(project *model.TenantProject) error
	GetByID(tenantID, id uint) (*model.TenantProject, error)
	Update(project *model.TenantProject) error
	Delete(id uint) error
	ListByTenant(tenantID uint) ([]*model.TenantProject, error)
}

// UserRepository 用户仓库接口
type UserRepository interface {
	Create(user *model.User) error
	GetByID(id uint) (*model.User, error)
	GetByEmail(email string) (*model.User, error)
	Update(user *model.User) error
	Delete(id uint) error
}
