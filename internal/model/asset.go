package model

import "time"

const (
	AssetScopePersonal = "personal"
	AssetScopePublic   = "public"

	AssetStatusActive        = "active"
	AssetStatusPendingReview = "pending_review"
	AssetStatusRejected      = "rejected"
	AssetStatusTrash         = "trash"
	AssetStatusWithdrawn     = "withdrawn"
)

// Asset is the central asset table (ink_asset).
// scope='personal': belongs exclusively to creator_id.
// scope='public': platform-shared (crawled assets use creator_id=0).
type Asset struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Scope       string `json:"scope" gorm:"size:20;default:'personal';index"`
	TenantID    uint   `json:"tenant_id" gorm:"index"`
	CreatorID   uint   `json:"creator_id" gorm:"index"`
	Title       string `json:"title" gorm:"size:500;index"`
	Description string `json:"description" gorm:"type:text"`
	Type        string `json:"type" gorm:"size:20;index"`    // image|video|audio|text
	SubType     string `json:"sub_type" gorm:"size:30;index"` // shot|character_ref|scene|bgm|sfx|voice|template|stock|cutout
	Source      string `json:"source" gorm:"size:20;index"`  // platform|crawled|uploaded

	// Storage
	StorageURL   string `json:"storage_url" gorm:"type:text"`
	ThumbnailURL string `json:"thumbnail_url" gorm:"type:text"`
	PreviewURL   string `json:"preview_url" gorm:"type:text"`

	// Copyright
	SourceURL   string `json:"source_url" gorm:"type:text"`
	ExternalID  string `json:"external_id" gorm:"size:200;index:idx_external_id"`
	License     string `json:"license" gorm:"size:100;index"` // CC0|CC-BY|CC-BY-SA|CC-BY-NC|PD|unsplash|pexels|pixabay|platform
	Attribution string `json:"attribution" gorm:"type:text"`

	// Media metadata
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Duration    float64 `json:"duration"`
	FileSize    int64   `json:"file_size"`
	MimeType    string  `json:"mime_type" gorm:"size:100"`
	AspectRatio string  `json:"aspect_ratio" gorm:"size:20"`

	// Quality & Safety
	QualityScore  float64 `json:"quality_score" gorm:"default:0"`
	QualityIssues string  `json:"quality_issues" gorm:"size:500"` // JSON array
	SafetyScore   float64 `json:"safety_score" gorm:"default:1"`
	SafetyChecked bool    `json:"safety_checked" gorm:"default:false"`

	// Perceptual hash (dedup)
	PHash string `json:"phash" gorm:"size:64;index"`

	// Color
	DominantColor string `json:"dominant_color" gorm:"size:10"`
	ColorPalette  string `json:"color_palette" gorm:"size:200"` // JSON array of hex strings

	// Extended metadata (JSON blob, type-specific)
	Metadata string `json:"metadata,omitempty" gorm:"type:text"`

	// Public library stats
	UseCount   int     `json:"use_count" gorm:"default:0"`
	LikeCount  int     `json:"like_count" gorm:"default:0"`
	ValueScore float64 `json:"value_score" gorm:"default:0;index"`

	// Share tracking
	SharedAt   *time.Time `json:"shared_at"`
	SharedBy   *uint      `json:"shared_by"`
	ReviewNote string     `json:"review_note" gorm:"size:500"`

	// Source tracing (platform-generated)
	NovelID *uint `json:"novel_id,omitempty"`
	VideoID *uint `json:"video_id,omitempty"`
	ShotID  *uint `json:"shot_id,omitempty"`

	// Status & soft-delete
	Status    string     `json:"status" gorm:"size:20;default:'active';index"`
	DeletedAt *time.Time `json:"deleted_at,omitempty" gorm:"index"`
	DeletedBy *uint      `json:"deleted_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Tags []Tag `json:"tags,omitempty" gorm:"many2many:ink_asset_tag_map"`
}

func (Asset) TableName() string { return "ink_asset" }

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

// AssetShareRequest tracks sharing workflow (ink_asset_share_request).
type AssetShareRequest struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	AssetID     uint       `json:"asset_id" gorm:"uniqueIndex"`
	RequestedBy uint       `json:"requested_by"`
	Status      string     `json:"status" gorm:"size:20;default:'pending'"` // pending|approved|rejected
	AutoPassed  bool       `json:"auto_passed"`
	ReviewerID  *uint      `json:"reviewer_id"`
	ReviewNote  string     `json:"review_note" gorm:"size:500"`
	ReviewedAt  *time.Time `json:"reviewed_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (AssetShareRequest) TableName() string { return "ink_asset_share_request" }

// AssetVersion tracks version history for personal-library assets (ink_asset_version).
type AssetVersion struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	AssetID      uint      `json:"asset_id" gorm:"index"`
	VersionNo    int       `json:"version_no"`
	StorageURL   string    `json:"storage_url" gorm:"type:text"`
	ThumbnailURL string    `json:"thumbnail_url" gorm:"type:text"`
	FileSize     int64     `json:"file_size"`
	ChangeNote   string    `json:"change_note" gorm:"size:500"`
	CreatedBy    uint      `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

func (AssetVersion) TableName() string { return "ink_asset_version" }

// AssetCollection groups assets (personal or public scope) (ink_asset_collection).
type AssetCollection struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	TenantID    uint      `json:"tenant_id" gorm:"index"`
	Scope       string    `json:"scope" gorm:"size:20;default:'personal'"`
	Name        string    `json:"name" gorm:"size:200"`
	Description string    `json:"description" gorm:"size:1000"`
	CoverURL    string    `json:"cover_url" gorm:"size:2000"`
	AssetCount  int       `json:"asset_count" gorm:"default:0"`
	CreatorID   uint      `json:"creator_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (AssetCollection) TableName() string { return "ink_asset_collection" }

// AssetCollectionItem is the join between collections and assets (ink_asset_collection_item).
type AssetCollectionItem struct {
	CollectionID uint      `gorm:"primaryKey;index"`
	AssetID      uint      `gorm:"primaryKey"`
	SortOrder    int       `gorm:"default:0"`
	AddedAt      time.Time
}

func (AssetCollectionItem) TableName() string { return "ink_asset_collection_item" }

// CrawlJob tracks crawler runs that import assets directly into the public library (ink_crawl_job).
type CrawlJob struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	TaskID      string     `json:"task_id" gorm:"size:50;index"`            // AsyncTask.TaskID for lifecycle management
	TenantID    uint       `json:"tenant_id" gorm:"index"`
	Source      string     `json:"source" gorm:"size:50"` // unsplash|pexels|pixabay|freesound|nasa|wikimedia|webpage
	Query       string     `json:"query" gorm:"size:500"` // search keyword, or URL when source=webpage
	AssetType   string     `json:"asset_type" gorm:"size:20"`
	License     string     `json:"license" gorm:"size:100"`
	Limit       int        `json:"limit"`
	CrawlDepth  int        `json:"crawl_depth" gorm:"default:0"`  // webpage: 0=single page, 1=follow links
	URLPattern  string     `json:"url_pattern" gorm:"size:500"`   // webpage: regex filter for followed links
	Status      string     `json:"status" gorm:"size:20;default:'pending'"` // pending|running|completed|failed
	TotalFound  int        `json:"total_found"`
	Imported    int        `json:"imported"`
	Skipped     int        `json:"skipped"`
	Failed      int        `json:"failed"`
	ErrorMsg    string     `json:"error_msg" gorm:"size:500"`
	CreatedBy   uint       `json:"created_by"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
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

// ShareLink enables no-login external sharing of assets or collections (ink_share_link).
type ShareLink struct {
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

func (ShareLink) TableName() string { return "ink_share_link" }

// AssetRequest is a community asset request board (ink_asset_request).
type AssetRequest struct {
	ID                uint      `json:"id" gorm:"primaryKey"`
	TenantID          uint      `json:"tenant_id" gorm:"index"`
	RequestedBy       uint      `json:"requested_by"`
	AssetType         string    `json:"asset_type" gorm:"size:20"`
	Description       string    `json:"description" gorm:"size:2000"`
	TagsWanted        string    `json:"tags_wanted" gorm:"size:500"` // JSON
	Priority          string    `json:"priority" gorm:"size:20;default:'medium'"`
	Status            string    `json:"status" gorm:"size:20;default:'open'"` // open|in_progress|fulfilled|closed
	FulfilledAssetIDs string    `json:"fulfilled_asset_ids" gorm:"size:1000"` // JSON uint[]
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (AssetRequest) TableName() string { return "ink_asset_request" }

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
	UpdatedAt          time.Time `json:"updated_at"`
}

func (AssetStorageQuota) TableName() string { return "ink_asset_storage_quota" }

// Deprecated: MediaAsset 已被 Asset（ink_asset）取代，仅保留向后兼容。新代码请使用 Asset。
type MediaAsset struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	TenantID    uint      `gorm:"index" json:"tenant_id"`
	NovelID     uint      `gorm:"index" json:"novel_id"`
	ChapterID   uint      `gorm:"index" json:"chapter_id"`
	MediaType   string    `gorm:"size:20;index" json:"media_type"` // image/audio/video/subtitle
	Filename    string    `gorm:"size:255" json:"filename"`
	ContentType string    `gorm:"size:100" json:"content_type"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

func (MediaAsset) TableName() string { return "ink_media_asset" }
