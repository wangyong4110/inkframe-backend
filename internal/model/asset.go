package model

import (
	"encoding/json"
	"time"
)

const (
	AssetScopePersonal = "personal"
	AssetScopePublic   = "public"

	AssetStatusProcessing    = "processing"    // 上传/处理中
	AssetStatusActive        = "active"
	AssetStatusPendingReview = "pending_review"
	AssetStatusRejected      = "rejected"
	AssetStatusFailed        = "failed"        // 上传/处理失败
	AssetStatusTrash         = "trash"
	AssetStatusWithdrawn     = "withdrawn"
)

// AssetMediaMeta 媒体元数据（JSON存储）
type AssetMediaMeta struct {
	StorageURL   string  `json:"storage_url"`
	ThumbnailURL string  `json:"thumbnail_url"`
	PreviewURL   string  `json:"preview_url"`
	SourceURL    string  `json:"source_url"`
	Attribution  string  `json:"attribution"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Duration     float64 `json:"duration"`
	FileSize     int64   `json:"file_size"`
	MimeType     string  `json:"mime_type"`
	AspectRatio  string  `json:"aspect_ratio"`
	ColorPalette string  `json:"color_palette"`
	Metadata     string  `json:"metadata"`
	// DominantColor 已迁移至 Asset.DominantColor 独立列（支持 WHERE 检索）
	// Description 已迁移至 Asset.Description 独立列（支持 FULLTEXT 检索，见 2026-07-16-v1）
}

// AssetQualityMeta 质量与来源元数据（JSON存储）
type AssetQualityMeta struct {
	QualityScore  float64 `json:"quality_score"`
	QualityIssues string  `json:"quality_issues"`
	SafetyScore   float64 `json:"safety_score"`
	SafetyChecked bool    `json:"safety_checked"`
	DeletedBy     *uint   `json:"deleted_by"`
	NovelID       *uint   `json:"novel_id"`
	VideoID       *uint   `json:"video_id"`
	ShotID        *uint   `json:"shot_id"`
	// UseCount/LikeCount 已迁移至 Asset 独立列（支持 ORDER BY / WHERE 检索）
}

// Asset is the central asset table (ink_asset).
// scope='personal': belongs exclusively to creator_id.
// scope='public': platform-shared (crawled assets use creator_id=0).
type Asset struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Scope       string `json:"scope" gorm:"size:20;default:'personal';index"`
	TenantID    uint   `json:"tenant_id" gorm:"index"`
	CreatorID   uint   `json:"creator_id" gorm:"index"`
	Title       string `json:"title" gorm:"size:500;index"`
	Description string `json:"description" gorm:"type:text"` // FULLTEXT 索引见 cmd/server/schema.go（2026-07-16-v1）
	Type        string `json:"type" gorm:"size:20;index"`     // image|video|audio|text
	SubType     string `json:"sub_type" gorm:"size:30;index"` // shot|character_ref|scene|bgm|sfx|voice|template|stock|cutout
	Source      string `json:"source" gorm:"size:20;index"`   // platform|crawled|uploaded

	// Copyright
	ExternalID string `json:"external_id" gorm:"size:200;index:idx_external_id"`
	License    string `json:"license" gorm:"size:100;index"`

	// Perceptual hash (dedup)
	PHash string `json:"phash" gorm:"size:64;index"`

	// Public library stats (indexed for ranking / ORDER BY / WHERE)
	ValueScore    float64 `json:"value_score" gorm:"default:0;index"`
	UseCount      int     `json:"use_count" gorm:"default:0;index"`
	LikeCount     int     `json:"like_count" gorm:"default:0;index"`
	DominantColor string  `json:"dominant_color" gorm:"size:20;index"`

	// JSON 合并字段（减少列数）
	MediaMeta   AssetMediaMeta   `json:"media_meta" gorm:"column:asset_media_meta;serializer:json;type:text"`
	QualityMeta AssetQualityMeta `json:"quality_meta" gorm:"column:asset_quality_meta;serializer:json;type:text"`

	// Status & soft-delete
	Status    string     `json:"status" gorm:"size:20;default:'active';index"`
	DeletedAt *time.Time `json:"deleted_at,omitempty" gorm:"index"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Tags []Tag `json:"tags,omitempty" gorm:"many2many:ink_asset_tag_map"`
}

func (Asset) TableName() string { return "ink_asset" }

// MarshalJSON 把 MediaMeta/QualityMeta 拍平到顶层输出。这两个字段用 JSON 序列化进单独的
// DB 列纯粹是为了减少列数（见上面 "JSON 合并字段" 注释），只是存储层的优化，从来不是 API
// 响应形状的一部分——前端 Asset 类型（inkframe-frontend/types/index.ts）一直按顶层字段
// （storage_url/thumbnail_url/width/height/...）读取，如果不拍平，序列化出来的是
// {"media_meta":{"storage_url":...}}，前端读到的所有这些字段永远是 undefined。
func (a Asset) MarshalJSON() ([]byte, error) {
	type alias Asset // 用别名类型避免调用自身 MarshalJSON 导致无限递归
	return json.Marshal(&struct {
		*alias
		MediaMeta   *AssetMediaMeta   `json:"media_meta,omitempty"`
		QualityMeta *AssetQualityMeta `json:"quality_meta,omitempty"`

		StorageURL    string  `json:"storage_url"`
		ThumbnailURL  string  `json:"thumbnail_url,omitempty"`
		PreviewURL    string  `json:"preview_url,omitempty"`
		SourceURL     string  `json:"source_url,omitempty"`
		Attribution   string  `json:"attribution,omitempty"`
		Width         int     `json:"width,omitempty"`
		Height        int     `json:"height,omitempty"`
		Duration      float64 `json:"duration,omitempty"`
		FileSize      int64   `json:"file_size,omitempty"`
		MimeType      string  `json:"mime_type,omitempty"`
		AspectRatio   string  `json:"aspect_ratio,omitempty"`
		ColorPalette  string  `json:"color_palette,omitempty"`
		Metadata      string  `json:"metadata,omitempty"`
		QualityScore  float64 `json:"quality_score,omitempty"`
		QualityIssues string  `json:"quality_issues,omitempty"`
		SafetyScore   float64 `json:"safety_score,omitempty"`
		SafetyChecked bool    `json:"safety_checked,omitempty"`
		DeletedBy     *uint   `json:"deleted_by,omitempty"`
		NovelID       *uint   `json:"novel_id,omitempty"`
		VideoID       *uint   `json:"video_id,omitempty"`
		ShotID        *uint   `json:"shot_id,omitempty"`
	}{
		alias: (*alias)(&a),
		// MediaMeta/QualityMeta 显式留空（配合 omitempty）以覆盖 *alias 提升出来的同名嵌套字段，
		// Go 的 json 包在字段名冲突时浅层字段优先，所以这里能盖掉 alias.MediaMeta 的嵌套输出。
		StorageURL:    a.MediaMeta.StorageURL,
		ThumbnailURL:  a.MediaMeta.ThumbnailURL,
		PreviewURL:    a.MediaMeta.PreviewURL,
		SourceURL:     a.MediaMeta.SourceURL,
		Attribution:   a.MediaMeta.Attribution,
		Width:         a.MediaMeta.Width,
		Height:        a.MediaMeta.Height,
		Duration:      a.MediaMeta.Duration,
		FileSize:      a.MediaMeta.FileSize,
		MimeType:      a.MediaMeta.MimeType,
		AspectRatio:   a.MediaMeta.AspectRatio,
		ColorPalette:  a.MediaMeta.ColorPalette,
		Metadata:      a.MediaMeta.Metadata,
		QualityScore:  a.QualityMeta.QualityScore,
		QualityIssues: a.QualityMeta.QualityIssues,
		SafetyScore:   a.QualityMeta.SafetyScore,
		SafetyChecked: a.QualityMeta.SafetyChecked,
		DeletedBy:     a.QualityMeta.DeletedBy,
		NovelID:       a.QualityMeta.NovelID,
		VideoID:       a.QualityMeta.VideoID,
		ShotID:        a.QualityMeta.ShotID,
	})
}

// Tag is the tag dictionary (ink_tag).
type Tag struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Name      string    `json:"name" gorm:"size:100;uniqueIndex"`
	Slug      string    `json:"slug" gorm:"size:100;uniqueIndex"`
	Category  string    `json:"category" gorm:"size:50;index"` // style|mood|subject|color|angle|genre|audio|quality|language|custom
	UseCount  int       `json:"use_count" gorm:"default:0"`
	IsSystem  bool      `json:"is_system" gorm:"default:false"`
	CreatedAt time.Time `json:"created_at"`
}

func (Tag) TableName() string { return "ink_tag" }

// AssetTagMap is the many2many join table (ink_asset_tag_map).
type AssetTagMap struct {
	AssetID    uint      `gorm:"primaryKey;index"`
	TagID      uint      `gorm:"primaryKey"`
	Source     string    `gorm:"size:20"` // ai|manual
	Confidence float64
	CreatedAt  time.Time
}

func (AssetTagMap) TableName() string { return "ink_asset_tag_map" }

// AssetPublishRequest tracks the workflow for publishing a personal asset into the public library (ink_asset_publish_request).
type AssetPublishRequest struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	AssetID     uint       `json:"asset_id" gorm:"index"`
	RequestedBy uint       `json:"requested_by"`
	Status      string     `json:"status" gorm:"size:20;default:'pending'"` // pending|approved|rejected
	AutoPassed  bool       `json:"auto_passed"`
	ReviewerID  *uint      `json:"reviewer_id"`
	ReviewNote  string     `json:"review_note" gorm:"size:500"`
	ReviewedAt  *time.Time `json:"reviewed_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (AssetPublishRequest) TableName() string { return "ink_asset_publish_request" }

// CrawlJobStats 爬取统计（JSON存储）
type CrawlJobStats struct {
	TotalFound  int        `json:"total_found"`
	Imported    int        `json:"imported"`
	Skipped     int        `json:"skipped"`
	Failed      int        `json:"failed"`
	ErrorMsg    string     `json:"error_msg"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CrawlDepth  int        `json:"crawl_depth"`
	URLPattern  string     `json:"url_pattern"`
}

// CrawlJob tracks crawler runs that import assets directly into the public library (ink_crawl_job).
type CrawlJob struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	TaskID    string `json:"task_id" gorm:"size:50;index"` // AsyncTask.TaskID for lifecycle management
	TenantID  uint   `json:"tenant_id" gorm:"index"`
	Source    string `json:"source" gorm:"size:50"`
	Query     string `json:"query" gorm:"size:500"`
	AssetType string `json:"asset_type" gorm:"size:20"`
	License   string `json:"license" gorm:"size:100"`
	Limit     int    `json:"limit"`
	Status    string `json:"status" gorm:"size:20;default:'pending'"`
	CreatedBy uint   `json:"created_by"`

	// JSON 合并字段（减少列数）
	Stats CrawlJobStats `json:"stats" gorm:"column:crawl_stats;serializer:json;type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (CrawlJob) TableName() string { return "ink_crawl_job" }

// AssetLike records user likes for public-library assets (ink_asset_like).
type AssetLike struct {
	AssetID   uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"primaryKey"`
	CreatedAt time.Time
}

func (AssetLike) TableName() string { return "ink_asset_like" }

// AssetUsage tracks how assets are used across the platform (ink_asset_usage).
type AssetUsage struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	AssetID    uint      `json:"asset_id" gorm:"index"`
	UsedByType string    `json:"used_by_type" gorm:"size:30"` // video_shot|bgm_segment|sfx_item|export|download
	UsedByID   uint      `json:"used_by_id"`
	TenantID   uint      `json:"tenant_id"`
	UserID     uint      `json:"user_id"`
	Context    string    `json:"context" gorm:"type:text"` // JSON extra info
	UsedAt     time.Time `json:"used_at"`
}

func (AssetUsage) TableName() string { return "ink_asset_usage" }

// AssetComment supports collaboration comments with optional coordinate anchors (ink_asset_comment).
type AssetComment struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	AssetID   uint      `json:"asset_id" gorm:"index"`
	UserID    uint      `json:"user_id"`
	Content   string    `json:"content" gorm:"size:2000"`
	ParentID  *uint     `json:"parent_id"`
	XRatio    *float64  `json:"x_ratio"` // image annotation (0-1)
	YRatio    *float64  `json:"y_ratio"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AssetComment) TableName() string { return "ink_asset_comment" }

// AssetShareLink enables no-login external sharing of assets or collections (ink_asset_share_link).
type AssetShareLink struct {
	ID              uint       `json:"id" gorm:"primaryKey"`
	Token           string     `json:"token" gorm:"size:64;uniqueIndex"`
	AssetID         *uint      `json:"asset_id"`
	CollectionID    *uint      `json:"collection_id"`
	CreatedBy       uint       `json:"created_by"`
	ExpiresAt       *time.Time `json:"expires_at"`
	Password        string     `json:"-" gorm:"size:200"` // bcrypt hash
	ViewCount       int        `json:"view_count" gorm:"default:0"`
	DownloadAllowed bool       `json:"download_allowed" gorm:"default:false"`
	CreatedAt       time.Time  `json:"created_at"`
}

func (AssetShareLink) TableName() string { return "ink_asset_share_link" }

// SearchLog records search queries for gap analysis (ink_search_log).
type SearchLog struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Query       string    `json:"query" gorm:"size:500;index"`
	ResultCount int       `json:"result_count"`
	SearchScope string    `json:"search_scope" gorm:"size:20"` // personal|public|all
	TenantID    uint      `json:"tenant_id"`
	SearchedAt  time.Time `json:"searched_at" gorm:"index"`
}

func (SearchLog) TableName() string { return "ink_search_log" }

// AssetStorageQuota tracks personal-library storage per tenant (ink_asset_storage_quota).
type AssetStorageQuota struct {
	TenantID           uint      `json:"tenant_id" gorm:"primaryKey"`
	StorageUsedBytes   int64     `json:"storage_used_bytes" gorm:"default:0"`
	StorageLimitBytes  int64     `json:"storage_limit_bytes" gorm:"default:10737418240"` // 10 GB
	CrawlUsedThisMonth int       `json:"crawl_used_this_month" gorm:"default:0"`
	CrawlLimitPerMonth int       `json:"crawl_limit_per_month" gorm:"default:500"`
	// QuotaMonth tracks which calendar month CrawlUsedThisMonth belongs to (format: "2006-01").
	// ResetMonthlyCrawl checks this field and only resets counts when the stored month differs from the current month.
	QuotaMonth string    `json:"quota_month" gorm:"size:7;default:''"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (AssetStorageQuota) TableName() string { return "ink_asset_storage_quota" }

