package model

import (
	"time"
)

// Tenant 租户/组织
type Tenant struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Name      string    `json:"name" gorm:"size:100;not null;comment:租户名称"`
	Code      string    `json:"code" gorm:"size:50;uniqueIndex;not null;comment:租户代码"`
	Logo      string    `json:"logo" gorm:"size:500;comment:Logo URL"`
	
	// 租户配置
	Settings  string    `json:"settings" gorm:"type:text;comment:租户配置JSON"`
	Plan      string    `json:"plan" gorm:"size:20;default:free;comment:套餐 free/pro/enterprise"`
	Status    string    `json:"status" gorm:"size:20;default:active;comment:状态 active/suspended/banned"`
	
	// 配额
	MaxProjects    int `json:"max_projects" gorm:"default:5;comment:最大项目数"`
	MaxStorageMB   int `json:"max_storage_mb" gorm:"default:1000;comment:最大存储MB"`
	MaxUsers       int `json:"max_users" gorm:"default:3;comment:最大用户数"`
	
	// 配额使用
	UsedProjects  int `json:"used_projects" gorm:"default:0;comment:已用项目数"`
	UsedStorageMB int `json:"used_storage_mb" gorm:"default:0;comment:已用存储MB"`
	UsedUsers     int `json:"used_users" gorm:"default:0;comment:已用用户数"`
	
	// 计费
	BillingCycle string    `json:"billing_cycle" gorm:"size:20;default:monthly;comment:计费周期"`
	ExpiresAt    time.Time `json:"expires_at" gorm:"comment:到期时间"`
	
	// 公共信息
	Description  string    `json:"description" gorm:"size:500;comment:描述"`
	ContactEmail  string    `json:"contact_email" gorm:"size:100;comment:联系邮箱"`
	ContactPhone  string    `json:"contact_phone" gorm:"size:20;comment:联系电话"`
	
	// SEO
	MetaTitle    string `json:"meta_title" gorm:"size:200;comment:SEO标题"`
	MetaKeywords string `json:"meta_keywords" gorm:"size:500;comment:SEO关键词"`
	MetaDesc    string `json:"meta_desc" gorm:"size:500;comment:SEO描述"`
	
	// 时间戳
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Tenant) TableName() string {
	return "tenants"
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
	OAuthProvider string `json:"oauth_provider" gorm:"size:20;comment:OAuth提供商"`
	OAuthID      string `json:"oauth_id" gorm:"size:100;comment:OAuth ID"`
	
	// 设置
	Settings    string `json:"settings" gorm:"type:text;comment:用户设置JSON"`
	Preferences string `json:"preferences" gorm:"type:text;comment:偏好设置JSON"`
	
	// 统计
	TotalProjects int `json:"total_projects" gorm:"default:0"`
	TotalNovels   int `json:"total_novels" gorm:"default:0"`
	TotalWords    int `json:"total_words" gorm:"default:0"`
	
	// 时间戳
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	LastLoginAt time.Time `json:"last_login_at"`
}

func (User) TableName() string {
	return "users"
}
