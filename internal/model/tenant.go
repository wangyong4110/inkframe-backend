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

	// 配额使用量（需要原子 UPDATE，保留独立列）
	UsedProjects  int `json:"used_projects" gorm:"default:0;comment:已用项目数"`
	UsedStorageMB int `json:"used_storage_mb" gorm:"default:0;comment:已用存储MB"`
	UsedUsers     int `json:"used_users" gorm:"default:0;comment:已用用户数"`

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
	ID        uint   `json:"id" gorm:"primaryKey"`
	TenantID  uint   `json:"tenant_id" gorm:"index;not null"`
	UserID    uint   `json:"user_id" gorm:"index;not null"`
	Role      string `json:"role" gorm:"size:20;default:member;comment:角色 owner/admin/member/viewer"`
	Nickname  string `json:"nickname" gorm:"size:50;comment:在租户内的昵称"`
	Avatar    string `json:"avatar" gorm:"size:500;comment:头像"`
	Status    string `json:"status" gorm:"size:20;default:active;comment:状态"`
	
	// 权限
	Permissions string `json:"permissions" gorm:"type:text;comment:自定义权限JSON"`
	
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (TenantUser) TableName() string {
	return "tenant_users"
}

// TenantProject 租户项目关联
type TenantProject struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	TenantID  uint   `json:"tenant_id" gorm:"index;not null"`
	ProjectID uint   `json:"project_id" gorm:"index;not null;comment:项目ID(小说ID或自定义项目ID)"`
	ProjectType string `json:"project_type" gorm:"size:20;default:novel;comment:项目类型 novel/custom"`
	Name      string `json:"name" gorm:"size:100;comment:项目名称"`
	Status    string `json:"status" gorm:"size:20;default:active;comment:状态"`
	
	// 成员
	Members   string `json:"members" gorm:"type:text;comment:成员列表JSON"`
	
	// 设置
	Settings string `json:"settings" gorm:"type:text;comment:项目设置JSON"`
	Tags     string `json:"tags" gorm:"type:text;comment:标签JSON"`
	
	// 配额
	StorageUsed int64 `json:"storage_used" gorm:"default:0;comment:已用存储字节"`
	
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (TenantProject) TableName() string {
	return "tenant_projects"
}

// User 用户
type User struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	UUID      string    `json:"uuid" gorm:"size:36;uniqueIndex;not null"`
	Username  string    `json:"username" gorm:"size:50;uniqueIndex;not null"`
	Email     string    `json:"email" gorm:"size:100;uniqueIndex;not null"`
	Phone     string    `json:"phone" gorm:"size:20;index"`
	Password  string    `json:"-" gorm:"size:100;not null"`
	Nickname  string    `json:"nickname" gorm:"size:50"`
	Avatar    string    `json:"avatar" gorm:"size:500"`
	Status    string    `json:"status" gorm:"size:20;default:active"`
	Role      string    `json:"role" gorm:"size:20;default:user;comment:系统角色 admin/user"`
	
	// OAuth
	OAuthProvider string `json:"oauth_provider" gorm:"column:oauth_provider;size:20;comment:OAuth提供商"`
	OAuthID       string `json:"oauth_id"       gorm:"column:oauth_id;size:100;comment:OAuth ID"`
	
	// 设置
	Settings    string `json:"settings" gorm:"type:text;comment:用户设置JSON"`
	Preferences string `json:"preferences" gorm:"type:text;comment:偏好设置JSON"`
	
	// 统计
	TotalProjects int `json:"total_projects" gorm:"default:0"`
	TotalNovels   int `json:"total_novels" gorm:"default:0"`
	TotalWords    int `json:"total_words" gorm:"default:0"`
	
	// 时间戳
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	LastLoginAt *time.Time     `json:"last_login_at"`
	DeletedAt   gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (User) TableName() string {
	return "users"
}
