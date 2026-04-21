package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// UserRepository 用户仓库
type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Create(user *model.User) error {
	return r.db.Create(user).Error
}

func (r *UserRepository) GetByID(id uint) (*model.User, error) {
	var user model.User
	err := r.db.First(&user, id).Error
	return &user, err
}

func (r *UserRepository) GetByEmail(email string) (*model.User, error) {
	var user model.User
	err := r.db.Where("email = ?", email).First(&user).Error
	return &user, err
}

func (r *UserRepository) GetByUsername(username string) (*model.User, error) {
	var user model.User
	err := r.db.Where("username = ?", username).First(&user).Error
	return &user, err
}

func (r *UserRepository) UpdateLastLogin(id uint) error {
	return r.db.Model(&model.User{}).Where("id = ?", id).
		Update("last_login_at", time.Now()).Error
}

// TenantRepository 租户仓库
type TenantRepository struct {
	db *gorm.DB
}

func NewTenantRepository(db *gorm.DB) *TenantRepository {
	return &TenantRepository{db: db}
}

func (r *TenantRepository) Create(tenant *model.Tenant) error {
	return r.db.Create(tenant).Error
}

func (r *TenantRepository) GetByID(id uint) (*model.Tenant, error) {
	var tenant model.Tenant
	err := r.db.First(&tenant, id).Error
	return &tenant, err
}

func (r *TenantRepository) GetByCode(code string) (*model.Tenant, error) {
	var tenant model.Tenant
	err := r.db.Where("code = ?", code).First(&tenant).Error
	return &tenant, err
}

func (r *TenantRepository) IncrUsedUsers(tenantID uint) error {
	return r.db.Model(&model.Tenant{}).Where("id = ?", tenantID).
		UpdateColumn("used_users", gorm.Expr("used_users + 1")).Error
}

func (r *TenantRepository) List(page, pageSize int) ([]*model.Tenant, int64, error) {
	var tenants []*model.Tenant
	var total int64
	if err := r.db.Model(&model.Tenant{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	if err := r.db.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&tenants).Error; err != nil {
		return nil, 0, err
	}
	return tenants, total, nil
}

func (r *TenantRepository) Update(tenant *model.Tenant) error {
	return r.db.Save(tenant).Error
}

func (r *TenantRepository) Delete(id uint) error {
	return r.db.Delete(&model.Tenant{}, id).Error
}

// TenantUserRepository 租户用户关联仓库
type TenantUserRepository struct {
	db *gorm.DB
}

func NewTenantUserRepository(db *gorm.DB) *TenantUserRepository {
	return &TenantUserRepository{db: db}
}

func (r *TenantUserRepository) Create(tu *model.TenantUser) error {
	return r.db.Create(tu).Error
}

func (r *TenantUserRepository) GetByTenantAndUser(tenantID, userID uint) (*model.TenantUser, error) {
	var tu model.TenantUser
	err := r.db.Where("tenant_id = ? AND user_id = ?", tenantID, userID).First(&tu).Error
	return &tu, err
}

func (r *TenantUserRepository) GetFirstByUser(userID uint) (*model.TenantUser, error) {
	var tu model.TenantUser
	err := r.db.Where("user_id = ? AND status = ?", userID, "active").
		Order("CASE WHEN role = 'owner' THEN 0 ELSE 1 END").
		First(&tu).Error
	return &tu, err
}

func (r *TenantUserRepository) ListByTenant(tenantID uint) ([]*model.TenantUser, error) {
	var members []*model.TenantUser
	err := r.db.Where("tenant_id = ?", tenantID).Find(&members).Error
	return members, err
}

func (r *TenantUserRepository) DeleteByTenantAndUser(tenantID, userID uint) error {
	return r.db.Where("tenant_id = ? AND user_id = ?", tenantID, userID).Delete(&model.TenantUser{}).Error
}

func (r *TenantUserRepository) UpdateRole(tenantID, userID uint, role string) error {
	return r.db.Model(&model.TenantUser{}).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Update("role", role).Error
}
