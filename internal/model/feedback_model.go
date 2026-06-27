package model

import (
	"time"

	"gorm.io/gorm"
)

// FeedbackAdminMeta 管理员处理元数据（JSON存储）
type FeedbackAdminMeta struct {
	AdminNote    string     `json:"admin_note"`
	ReplyContent string     `json:"reply_content"`
	RepliedAt    *time.Time `json:"replied_at"`
	ResolvedAt   *time.Time `json:"resolved_at"`
	PageURL      string     `json:"page_url"`
	UserAgent    string     `json:"user_agent"`
	Screenshots  string     `json:"screenshots"`
	ContactEmail string     `json:"contact_email"`
}

type UserFeedback struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:0"`
	UserID   uint `json:"user_id" gorm:"index;not null;default:0"`

	Type    string `json:"type" gorm:"size:20;not null;default:'general'"`
	Title   string `json:"title" gorm:"size:200"`
	Content string `json:"content" gorm:"type:text;not null"`
	Rating  int    `json:"rating" gorm:"default:0"`

	Status   string `json:"status" gorm:"size:20;not null;default:'pending'"`
	Priority string `json:"priority" gorm:"size:20;not null;default:'medium'"`

	// JSON 合并字段（减少列数）
	AdminMeta FeedbackAdminMeta `json:"admin_meta" gorm:"column:feedback_admin_meta;serializer:json;type:text"`

	SeqNo uint `json:"seq_no" gorm:"index"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (UserFeedback) TableName() string { return "ink_user_feedback" }

type CreateFeedbackRequest struct {
	Type         string   `json:"type" binding:"required"`
	Title        string   `json:"title"`
	Content      string   `json:"content" binding:"required"`
	Rating       int      `json:"rating"`
	PageURL      string   `json:"page_url"`
	UserAgent    string   `json:"user_agent"`
	Screenshots  []string `json:"screenshots"`
	ContactEmail string   `json:"contact_email"`
}

type UpdateFeedbackRequest struct {
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	AdminNote string `json:"admin_note"`
}

type ReplyFeedbackRequest struct {
	Content string `json:"content" binding:"required"`
}
