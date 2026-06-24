package model

import "time"

const (
	ContentEntityNovel = "novel"
	ContentEntityVideo = "video"
)

// ContentStats 内容统计（浏览/点赞/评论），独立于主内容行以减少写竞争
type ContentStats struct {
	EntityType   string    `json:"entity_type" gorm:"primaryKey;size:20"`
	EntityID     uint      `json:"entity_id" gorm:"primaryKey"`
	ViewCount    int       `json:"view_count" gorm:"not null;default:0"`
	LikeCount    int       `json:"like_count" gorm:"not null;default:0"`
	CommentCount int       `json:"comment_count" gorm:"not null;default:0"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (ContentStats) TableName() string { return "ink_content_stats" }
