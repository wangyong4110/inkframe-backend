package model

import "time"

// NovelMember 小说协作成员
type NovelMember struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	NovelID     uint       `json:"novel_id" gorm:"uniqueIndex:idx_novel_member,priority:1;not null"`
	UserID      uint       `json:"user_id" gorm:"uniqueIndex:idx_novel_member,priority:2;index;not null"`
	Role        string     `json:"role" gorm:"size:20;not null;default:'viewer'"` // owner/editor/viewer
	Status      string     `json:"status" gorm:"size:20;not null;default:'active'"` // active/pending
	InvitedBy   uint       `json:"invited_by" gorm:"default:0"`
	InviteToken     string     `json:"-" gorm:"size:64;index"`
	InviteExpiresAt *time.Time `json:"-" gorm:"index"`
	JoinedAt        *time.Time `json:"joined_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (NovelMember) TableName() string { return "ink_novel_member" }

// EditingLock 编辑锁（防止多人同时编辑同一实体）
type EditingLock struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	EntityType   string    `json:"entity_type" gorm:"size:50;not null;uniqueIndex:idx_editing_lock,priority:1"`
	EntityID     uint      `json:"entity_id" gorm:"not null;uniqueIndex:idx_editing_lock,priority:2"`
	LockedBy     uint      `json:"locked_by" gorm:"not null"`
	LockedByName string    `json:"locked_by_name" gorm:"size:100"`
	ExpiresAt    time.Time `json:"expires_at" gorm:"not null;index"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (EditingLock) TableName() string { return "ink_editing_lock" }
