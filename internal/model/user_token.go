package model

import (
	"time"

	"gorm.io/gorm"
)

// UserToken 用于密码重置和邮箱验证的一次性令牌
type UserToken struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	UserID    uint           `json:"user_id" gorm:"not null;index"`
	Token     string         `json:"token" gorm:"size:64;uniqueIndex;not null"`
	TokenType string         `json:"token_type" gorm:"size:30;not null"` // "reset_password" | "verify_email"
	ExpiresAt time.Time      `json:"expires_at" gorm:"not null;index"`
	UsedAt    *time.Time     `json:"used_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (UserToken) TableName() string { return "ink_user_token" }
