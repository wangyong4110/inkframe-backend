package model

import (
	"time"

	"gorm.io/gorm"
)

// Skill 小说技能/功法体系
type Skill struct {
	ID      uint           `json:"id" gorm:"primaryKey"`
	NovelID uint           `json:"novel_id" gorm:"index;not null"`
	Name        string         `json:"name" gorm:"size:100;not null"`
	SkillType   string         `json:"skill_type" gorm:"size:50"`   // 主动/被动/天赋/武技/法术/功法
	Level       int            `json:"level" gorm:"default:1"`
	Description string         `json:"description" gorm:"type:text"`
	Effect      string         `json:"effect" gorm:"type:text"`     // 效果描述
	ImagePath   string         `json:"image_path" gorm:"size:500"`  // 效果图 URL
	Tags        string         `json:"tags" gorm:"size:500"`        // JSON array of tags
	ChapterNo   int            `json:"chapter_no" gorm:"default:0"` // 首次出现章节
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Skill) TableName() string { return "ink_skill" }

type CreateSkillRequest struct {
	Name        string `json:"name" binding:"required"`
	SkillType   string `json:"skill_type"`
	Level       int    `json:"level"`
	Description string `json:"description"`
	Effect      string `json:"effect"`
	Tags        string `json:"tags"`
	ChapterNo   int    `json:"chapter_no"`
}

type UpdateSkillRequest struct {
	Name        string `json:"name"`
	SkillType   string `json:"skill_type"`
	Level       int    `json:"level"`
	Description string `json:"description"`
	Effect      string `json:"effect"`
	Tags        string `json:"tags"`
	ChapterNo   int    `json:"chapter_no"`
}
