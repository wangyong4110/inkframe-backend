package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// TenantQuota 租户配额配置（存为 JSON）
type TenantQuota struct {
	MaxProjects  int    `json:"max_projects"`
	MaxStorageMB int    `json:"max_storage_mb"`
	MaxUsers     int    `json:"max_users"`
	BillingCycle string `json:"billing_cycle"`
}

// TenantProfile 租户展示信息（存为 JSON）
type TenantProfile struct {
	Logo         string `json:"logo,omitempty"`
	Description  string `json:"description,omitempty"`
	ContactEmail string `json:"contact_email,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
	MetaTitle    string `json:"meta_title,omitempty"`
	MetaKeywords string `json:"meta_keywords,omitempty"`
	MetaDesc     string `json:"meta_desc,omitempty"`
}

// Tenant 租户/组织
type Tenant struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	Name   string `json:"name" gorm:"size:100;not null;comment:租户名称"`
	Code   string `json:"code" gorm:"size:50;uniqueIndex;not null;comment:租户代码"`
	Status string `json:"status" gorm:"size:20;default:active;comment:状态 active/suspended/banned"`
	Plan   string `json:"plan" gorm:"size:20;default:free;comment:套餐 free/pro/enterprise"`

	// 到期时间（nil = 永不过期）
	ExpiresAt *time.Time `json:"expires_at" gorm:"comment:到期时间"`

	// 配额使用量
	UsedUsers int `json:"used_users" gorm:"default:0;comment:已用用户数"`

	// 配额上限 + 计费周期（配置性，合并为 JSON）
	Quota string `json:"quota" gorm:"type:text;comment:配额配置JSON"`

	// 展示信息：logo/描述/联系/SEO（合并为 JSON）
	Profile string `json:"profile" gorm:"type:text;comment:租户展示信息JSON"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Tenant) TableName() string {
	return "tenants"
}

// GetQuota 解析配额配置，返回带默认值的结构体
func (t *Tenant) GetQuota() TenantQuota {
	q := TenantQuota{
		MaxProjects:  5,
		MaxStorageMB: 1000,
		MaxUsers:     3,
		BillingCycle: "monthly",
	}
	if t.Quota != "" {
		_ = json.Unmarshal([]byte(t.Quota), &q)
	}
	return q
}

// SetQuota 序列化配额配置
func (t *Tenant) SetQuota(q TenantQuota) {
	b, _ := json.Marshal(q)
	t.Quota = string(b)
}

// GetProfile 解析展示信息
func (t *Tenant) GetProfile() TenantProfile {
	var p TenantProfile
	if t.Profile != "" {
		_ = json.Unmarshal([]byte(t.Profile), &p)
	}
	return p
}

// SetProfile 序列化展示信息
func (t *Tenant) SetProfile(p TenantProfile) {
	b, _ := json.Marshal(p)
	t.Profile = string(b)
}

// TenantUser 租户用户关联
type TenantUser struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TenantID uint   `json:"tenant_id" gorm:"uniqueIndex:uniq_tenant_user;not null"`
	UserID   uint   `json:"user_id" gorm:"uniqueIndex:uniq_tenant_user;not null"`
	Role     string `json:"role" gorm:"size:20;default:member;comment:角色 owner/admin/member/viewer"`
	Nickname string `json:"nickname" gorm:"size:50;comment:在租户内的昵称"`
	Avatar   string `json:"avatar" gorm:"size:500;comment:头像"`
	Status   string `json:"status" gorm:"size:20;default:active;comment:状态"`

	// 权限
	Permissions string `json:"permissions" gorm:"type:text;comment:自定义权限JSON"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (TenantUser) TableName() string {
	return "tenant_users"
}

// UserSecurityMeta 安全与登录元数据（JSON存储）
type UserSecurityMeta struct {
	FailedLoginCount int        `json:"failed_login_count"`
	LockUntil        *time.Time `json:"lock_until"`
	EmailVerifiedAt  *time.Time `json:"email_verified_at"`
}

// UserOAuthMeta OAuth登录元数据（JSON存储）
type UserOAuthMeta struct {
	OAuthProvider string `json:"oauth_provider"`
	OAuthID       string `json:"oauth_id"`
}

// User 用户
type User struct {
	ID       uint    `json:"id" gorm:"primaryKey"`
	UUID     string  `json:"uuid" gorm:"size:36;uniqueIndex;not null"`
	Username string  `json:"username" gorm:"size:50;uniqueIndex;not null"`
	Email    string  `json:"email" gorm:"size:100;uniqueIndex;not null"`
	Phone    *string `json:"phone,omitempty" gorm:"size:20;uniqueIndex"`
	Password string  `json:"-" gorm:"size:100;not null"`
	Nickname string  `json:"nickname" gorm:"size:50"`
	Avatar   string  `json:"avatar" gorm:"size:500"`
	Status      string     `json:"status" gorm:"size:20;default:active"`
	Role        string     `json:"role" gorm:"size:20;default:user;comment:系统角色 admin/user"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty" gorm:"column:last_login_at"`

	// JSON 合并字段（减少列数）
	SecurityMeta UserSecurityMeta `json:"-" gorm:"column:security_meta;serializer:json;type:text"`
	OAuthMeta    UserOAuthMeta    `json:"-" gorm:"column:oauth_meta;serializer:json;type:text"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (User) TableName() string {
	return "users"
}

// System-level role constants (tenant-level roles are in constants.go)
const (
	RoleSystemAdmin = "system_admin"
	RoleUser        = "user"
)
