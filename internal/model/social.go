package model

import (
	"time"

	"gorm.io/gorm"
)

// EntityLike 统一点赞表（ink_entity_like），替代 ink_novel_like / ink_chapter_like / ink_video_like。
// entity_type: novel / chapter / video
type EntityLike struct {
	EntityType string    `gorm:"primaryKey;size:20"`
	EntityID   uint      `gorm:"primaryKey"`
	UserID     uint      `gorm:"primaryKey"`
	NovelID    uint      `gorm:"index"` // 供小说级联删除（novel时=EntityID，chapter时=所属novel_id，video时=0）
	CreatedAt  time.Time
}

func (EntityLike) TableName() string { return "ink_entity_like" }

// EntityComment 统一评论表（ink_entity_comment），替代 ink_novel_comment / ink_chapter_comment / ink_video_comment。
// entity_type: novel / chapter / video
type EntityComment struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	EntityType string    `json:"entity_type" gorm:"size:20;index:idx_entity_comment,priority:1"`
	EntityID   uint      `json:"entity_id" gorm:"index:idx_entity_comment,priority:2"`
	NovelID    uint      `json:"novel_id" gorm:"index"` // 供小说级联删除
	UserID     uint      `json:"user_id"`
	Content    string    `json:"content" gorm:"size:2000"`
	ParentID   *uint     `json:"parent_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (EntityComment) TableName() string { return "ink_entity_comment" }

// VideoLike DTO（兼容旧代码，不再对应独立表）
type VideoLike struct {
	VideoID   uint      `json:"video_id"`
	UserID    uint      `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// VideoComment DTO（兼容旧代码，不再对应独立表）
type VideoComment struct {
	ID        uint      `json:"id"`
	VideoID   uint      `json:"video_id"`
	UserID    uint      `json:"user_id"`
	Content   string    `json:"content"`
	ParentID  *uint     `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NovelLike DTO（兼容旧代码，不再对应独立表）
type NovelLike struct {
	NovelID   uint      `json:"novel_id"`
	UserID    uint      `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// NovelComment DTO（兼容旧代码，不再对应独立表）
type NovelComment struct {
	ID        uint      `json:"id"`
	NovelID   uint      `json:"novel_id"`
	UserID    uint      `json:"user_id"`
	Content   string    `json:"content"`
	ParentID  *uint     `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChapterLike DTO（兼容旧代码，不再对应独立表）
type ChapterLike struct {
	ChapterID uint      `json:"chapter_id"`
	UserID    uint      `json:"user_id"`
	NovelID   uint      `json:"novel_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ChapterComment DTO（兼容旧代码，不再对应独立表）
type ChapterComment struct {
	ID        uint      `json:"id"`
	ChapterID uint      `json:"chapter_id"`
	NovelID   uint      `json:"novel_id"`
	UserID    uint      `json:"user_id"`
	Content   string    `json:"content"`
	ParentID  *uint     `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReadingProgress 用户阅读进度（ink_reading_progress）每用户每小说一条
type ReadingProgress struct {
	UserID    uint      `gorm:"uniqueIndex:idx_rp_user_novel;not null" json:"user_id"`
	NovelID   uint      `gorm:"uniqueIndex:idx_rp_user_novel;not null" json:"novel_id"`
	ChapterNo int       `json:"chapter_no"`
	ChapterID uint      `json:"chapter_id"`
	ScrollPct int       `json:"scroll_pct"` // 0-100 阅读进度百分比
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

func (ReadingProgress) TableName() string { return "ink_reading_progress" }

// ChapterReadRecord 章节已读标记（ink_chapter_read_record）复合主键
type ChapterReadRecord struct {
	UserID    uint      `gorm:"primaryKey" json:"user_id"`
	ChapterID uint      `gorm:"primaryKey" json:"chapter_id"`
	NovelID   uint      `gorm:"index" json:"novel_id"`
	ReadAt    time.Time `json:"read_at"`
}

func (ChapterReadRecord) TableName() string { return "ink_chapter_read_record" }

// PlatformAccount 外部平台账号绑定
type PlatformAccount struct {
	ID           uint       `json:"id" gorm:"primaryKey"`
	TenantID     uint       `json:"tenant_id" gorm:"index"`
	Platform     string     `json:"platform" gorm:"size:30"`   // bilibili|douyin|youtube|wechat_video
	AccountName  string     `json:"account_name" gorm:"size:200"`
	UID          string     `json:"uid" gorm:"size:200"`
	AccessToken  string     `json:"-" gorm:"size:2000"`
	RefreshToken string     `json:"-" gorm:"size:2000"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Status       string     `json:"status" gorm:"size:20;default:'active'"` // active|expired|revoked
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (PlatformAccount) TableName() string { return "ink_platform_account" }

// BeforeSave encrypts AccessToken and RefreshToken before persisting to the database.
func (p *PlatformAccount) BeforeSave(tx *gorm.DB) error {
	if p.AccessToken != "" {
		enc, err := EncryptField(p.AccessToken)
		if err != nil {
			return err
		}
		p.AccessToken = enc
	}
	if p.RefreshToken != "" {
		enc, err := EncryptField(p.RefreshToken)
		if err != nil {
			return err
		}
		p.RefreshToken = enc
	}
	return nil
}

// AfterFind decrypts AccessToken and RefreshToken after loading from the database.
func (p *PlatformAccount) AfterFind(tx *gorm.DB) error {
	if p.AccessToken != "" {
		dec, err := DecryptField(p.AccessToken)
		if err == nil {
			p.AccessToken = dec
		}
	}
	if p.RefreshToken != "" {
		dec, err := DecryptField(p.RefreshToken)
		if err == nil {
			p.RefreshToken = dec
		}
	}
	return nil
}

// VideoPublishRecord 视频外部发布记录
type VideoPublishRecord struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	VideoID     uint       `json:"video_id" gorm:"index"`
	Platform    string     `json:"platform" gorm:"size:30"`
	AccountID   uint       `json:"account_id"`
	ExternalID  string     `json:"external_id" gorm:"size:500"`
	ExternalURL string     `json:"external_url" gorm:"size:2000"`
	Status      string     `json:"status" gorm:"size:20;default:'pending'"` // pending|uploading|processing|published|failed
	ErrorMsg    string     `json:"error_msg" gorm:"size:500"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (VideoPublishRecord) TableName() string { return "ink_video_publish_record" }
