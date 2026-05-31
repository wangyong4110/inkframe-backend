package model

import (
	"time"

	"gorm.io/gorm"
)

// Notification 站内通知
type Notification struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	TenantID   uint           `json:"tenant_id" gorm:"not null;index"`
	UserID     uint           `json:"user_id" gorm:"not null;index:idx_notif_user_read"`
	IsRead     bool           `json:"is_read" gorm:"default:false;index:idx_notif_user_read"`
	EventType  string         `json:"event_type" gorm:"size:50;not null"` // "chapter_done"|"novel_done"|"review_done"|"crawl_done"|"publish_done"
	Title      string         `json:"title" gorm:"size:200;not null"`
	Body       string         `json:"body" gorm:"type:text"`
	EntityType string         `json:"entity_type" gorm:"size:30"` // "novel"|"chapter"|"video"|"crawl"
	EntityID   uint           `json:"entity_id"`
	LinkPath   string         `json:"link_path" gorm:"size:500"` // 前端跳转路径，如 "/novel/123/chapter/5"
	CreatedAt  time.Time      `json:"created_at"`
	DeletedAt  gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Notification) TableName() string { return "ink_notification" }
