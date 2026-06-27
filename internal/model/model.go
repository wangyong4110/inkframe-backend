package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"gorm.io/gorm"
)

// JSONUintSlice 存储为 JSON 字符串，序列化为 JSON 数组（供 character_ids 等字段使用）
type JSONUintSlice []uint

func (s JSONUintSlice) Value() (driver.Value, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal([]uint(s))
	return string(b), err
}

func (s *JSONUintSlice) Scan(value interface{}) error {
	*s = nil
	if value == nil {
		return nil
	}
	var str string
	switch v := value.(type) {
	case []byte:
		str = string(v)
	case string:
		str = v
	default:
		return fmt.Errorf("JSONUintSlice: unsupported type %T", value)
	}
	if str == "" || str == "null" || str == "[]" {
		return nil
	}
	return json.Unmarshal([]byte(str), s)
}

// AsyncTaskDLQ 死信队列重试元数据（JSON存储）
type AsyncTaskDLQ struct {
	MaxRetries int    `json:"max_retries"`
	FailureLog string `json:"failure_log"`
}

// AsyncTask 统一异步任务（DB 持久化，页面刷新后仍可恢复）
// 索引说明：
//   - idx_task_tenant_status: 按租户+状态过滤（ListByTenant、cleanup）
//   - idx_task_tenant_type_status: 按租户+类型+状态组合查询
//   - idx_task_tenant_created: 按租户+创建时间过滤（per-tenant fairness 轮询）
type AsyncTask struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	TaskID     string `json:"task_id" gorm:"uniqueIndex;size:64;not null"`
	TenantID   uint   `json:"tenant_id" gorm:"not null;default:1;index:idx_task_tenant_status;index:idx_task_tenant_type_status;index:idx_task_tenant_created,priority:1"`
	Type       string `json:"type" gorm:"size:50;not null;index:idx_task_tenant_type_status"`
	Status     string `json:"status" gorm:"size:20;not null;default:pending;index:idx_task_tenant_status;index:idx_task_tenant_type_status"`
	Title      string `json:"title" gorm:"size:255"`
	EntityType string `json:"entity_type" gorm:"size:50"`
	EntityID   uint   `json:"entity_id" gorm:"index"`
	ResultJSON string `json:"-" gorm:"column:result;type:mediumtext"` // mediumtext 支持 16MB，避免大结果超 text 65KB 限制
	ParamsJSON string `json:"-" gorm:"column:params;type:mediumtext"` // mediumtext 支持 16MB，避免大参数超 text 65KB 限制
	Error      string `json:"error,omitempty" gorm:"type:text"`
	Progress   int    `json:"progress" gorm:"default:0"`
	RetryCount int    `json:"retry_count" gorm:"not null;default:0"`

	// JSON 合并字段（减少列数）
	DLQ AsyncTaskDLQ `json:"dlq" gorm:"column:task_dlq;serializer:json;type:text"`

	CreatedAt time.Time      `json:"created_at" gorm:"index:idx_task_tenant_created,priority:2"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (AsyncTask) TableName() string { return "ink_async_task" }

// MarshalJSON exposes ResultJSON as 'data' in the API response.
func (t AsyncTask) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{
		"id":          t.ID,
		"task_id":     t.TaskID,
		"tenant_id":   t.TenantID,
		"type":        t.Type,
		"status":      t.Status,
		"title":       t.Title,
		"entity_type": t.EntityType,
		"entity_id":   t.EntityID,
		"progress":    t.Progress,
		"retry_count": t.RetryCount,
		"created_at":  t.CreatedAt,
		"updated_at":  t.UpdatedAt,
	}
	if t.Error != "" {
		m["error"] = t.Error
	}
	if t.ResultJSON != "" {
		var data interface{}
		if err := json.Unmarshal([]byte(t.ResultJSON), &data); err != nil {
			// ResultJSON 损坏时记录警告，避免静默丢失数据
			logger.Errorf("[AsyncTask] task %s: ResultJSON unmarshal failed: %v", t.TaskID, err)
		} else {
			m["data"] = data
		}
	}
	return json.Marshal(m)
}

// NovelMeta 小说基本元数据（JSON存储，减少列数）
type NovelMeta struct {
	Description     string     `json:"description"`
	Genre           string     `json:"genre"`
	Channel         string     `json:"channel"`
	TargetWordCount int        `json:"target_word_count"`
	TargetChapters  int        `json:"target_chapters"`
	CoverImage      string     `json:"cover_image"`
	CoreTheme       string     `json:"core_theme"`
	PlazaTags       string     `json:"plaza_tags"`
	PublishedAt     *time.Time `json:"published_at"`
	Visibility      string     `json:"visibility"`
}

// NovelAIConfig AI生成配置（JSON存储）
type NovelAIConfig struct {
	AIModel            string  `json:"ai_model"`
	Temperature        float64 `json:"temperature"`
	TopP               float64 `json:"top_p"`
	MaxTokens          int     `json:"max_tokens"`
	TimeoutSeconds     int     `json:"timeout_seconds"`
	StylePrompt        string  `json:"style_prompt"`
	ImageStyle         string  `json:"image_style"`
	PromptLanguage     string  `json:"prompt_language"`
	ChapterMode        string  `json:"chapter_mode"`
	AutoReviewRounds   int     `json:"auto_review_rounds"`
	AutoReviewMinScore float64 `json:"auto_review_min_score"`
}

// NovelReviewMeta 内容审核元数据（JSON存储）
type NovelReviewMeta struct {
	ReviewStatus string     `json:"review_status"`
	ReviewNote   string     `json:"review_note"`
	ReviewedAt   *time.Time `json:"reviewed_at"`
	ReviewedBy   uint       `json:"reviewed_by"`
}

// Novel 小说
type Novel struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	UUID     string `json:"uuid" gorm:"uniqueIndex;size:36"`
	TenantID uint   `json:"tenant_id" gorm:"index;index:idx_novel_tenant_status,priority:1;not null;default:1"`
	Title    string `json:"title" gorm:"size:255;not null"`

	// 状态
	Status string `json:"status" gorm:"size:20;index;index:idx_novel_tenant_status,priority:2;default:planning"`
	// planning=规划中, writing=创作中, paused=暂停, completed=已完成, archived=已归档

	// 统计
	TotalWords     int `json:"total_words" gorm:"default:0"`
	ChapterCount   int `json:"chapter_count" gorm:"default:0"`
	PublishedCount int `json:"published_count" gorm:"default:0"`

	// 关联
	WorldviewID *uint      `json:"worldview_id"`
	Worldview   *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`

	Outline string `json:"outline,omitempty" gorm:"type:longtext"` // 大纲 JSON（章节列表）；300章时可超 64KB，必须用 longtext

	// JSON 合并字段（减少列数）
	Meta       NovelMeta       `json:"meta" gorm:"column:novel_meta;serializer:json;type:text"`
	AIConfig   NovelAIConfig   `json:"ai_config" gorm:"column:ai_config;serializer:json;type:text"`
	ReviewMeta NovelReviewMeta `json:"review_meta" gorm:"column:review_meta;serializer:json;type:text"`

	// 视频/字幕配置（已迁移至 ink_novel_video_config，通过 VideoConfig 关联访问）
	VideoConfig *NovelVideoConfig `json:"video_config,omitempty" gorm:"foreignKey:NovelID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	// 广场社交字段
	IsPublished bool `json:"is_published" gorm:"default:false;index"`
	// 统计计数（不存 ink_novel 主表，从 ink_content_stats 加载）
	ViewCount    int     `json:"view_count" gorm:"-"`
	LikeCount    int     `json:"like_count" gorm:"-"`
	CommentCount int     `json:"comment_count" gorm:"-"`
	HotScore     float64 `json:"hot_score" gorm:"default:0;index"`

	// 文件去重（记录导入来源文件内容哈希，用于防止同一文件重复导入）
	SourceFileHash string `json:"source_file_hash,omitempty" gorm:"size:64;index"`

	// 创建者（协作权限快速判断用）
	CreatedBy uint `json:"created_by" gorm:"index;not null;default:0"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Novel) TableName() string {
	return "ink_novel"
}

// VideoConf returns video/subtitle config with safe nil handling.
func (n *Novel) VideoConf() NovelVideoConfigData {
	if n.VideoConfig != nil {
		return n.VideoConfig.Config
	}
	return NovelVideoConfigData{
		VideoType:             "animation",
		VideoResolution:       "1080p",
		VideoFPS:              30,
		VideoAspectRatio:      "16:9",
		CharConsistencyWeight: 1.0,
		SubtitleEnabled:       true,
		SubtitlePosition:      "bottom",
		SubtitleFontSize:      48,
		SubtitleColor:         "#FFFFFF",
		SubtitleBgStyle:       "shadow",
	}
}

// EnsureVideoConfig initializes VideoConfig if nil (for mutation).
func (n *Novel) EnsureVideoConfig() *NovelVideoConfig {
	if n.VideoConfig == nil {
		n.VideoConfig = &NovelVideoConfig{NovelID: n.ID}
	}
	return n.VideoConfig
}

// MarshalJSON flattens VideoConfig + Meta + AIConfig + ReviewMeta fields into
// the top-level Novel JSON so the frontend can read them directly
// (e.g. novel.video_type instead of novel.video_config.video_type).
func (n Novel) MarshalJSON() ([]byte, error) {
	// Type alias prevents infinite recursion when calling json.Marshal below.
	type NovelAlias Novel
	base, err := json.Marshal(NovelAlias(n))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	// Flatten Meta / AIConfig / ReviewMeta
	metaFields := map[string]any{
		"description":           n.Meta.Description,
		"genre":                 n.Meta.Genre,
		"channel":               n.Meta.Channel,
		"target_word_count":     n.Meta.TargetWordCount,
		"target_chapters":       n.Meta.TargetChapters,
		"cover_image":           n.Meta.CoverImage,
		"core_theme":            n.Meta.CoreTheme,
		"plaza_tags":            n.Meta.PlazaTags,
		"published_at":          n.Meta.PublishedAt,
		"visibility":            n.Meta.Visibility,
		"ai_model":              n.AIConfig.AIModel,
		"temperature":           n.AIConfig.Temperature,
		"top_p":                 n.AIConfig.TopP,
		"max_tokens":            n.AIConfig.MaxTokens,
		"timeout_seconds":       n.AIConfig.TimeoutSeconds,
		"style_prompt":          n.AIConfig.StylePrompt,
		"image_style":           n.AIConfig.ImageStyle,
		"prompt_language":       n.AIConfig.PromptLanguage,
		"chapter_mode":          n.AIConfig.ChapterMode,
		"auto_review_rounds":    n.AIConfig.AutoReviewRounds,
		"auto_review_min_score": n.AIConfig.AutoReviewMinScore,
		"review_status":         n.ReviewMeta.ReviewStatus,
		"review_note":           n.ReviewMeta.ReviewNote,
		"reviewed_at":           n.ReviewMeta.ReviewedAt,
		"reviewed_by":           n.ReviewMeta.ReviewedBy,
	}
	for k, v := range metaFields {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	if n.VideoConfig != nil {
		vc := n.VideoConfig.Config
		vcFields := map[string]any{
			"video_type":              vc.VideoType,
			"video_resolution":        vc.VideoResolution,
			"video_fps":               vc.VideoFPS,
			"video_aspect_ratio":      vc.VideoAspectRatio,
			"char_consistency_weight": vc.CharConsistencyWeight,
			"narration_voice":         vc.NarrationVoice,
			"subtitle_enabled":        vc.SubtitleEnabled,
			"subtitle_position":       vc.SubtitlePosition,
			"subtitle_font_size":      vc.SubtitleFontSize,
			"subtitle_color":          vc.SubtitleColor,
			"subtitle_bg_style":       vc.SubtitleBgStyle,
			"subtitle_font":           vc.SubtitleFont,
			"color_grade":             vc.ColorGrade,
			"contrast_level":          vc.ContrastLevel,
			"saturation":              vc.Saturation,
			"film_grain":              vc.FilmGrain,
			"vignette":                vc.Vignette,
			"chromatic_aberration":    vc.ChromaticAberration,
			"kling_pro_for_action":    vc.KlingProForAction,
		}
		for k, v := range vcFields {
			b, _ := json.Marshal(v)
			m[k] = b
		}
	}
	return json.Marshal(m)
}

// ChapterNarrativeMeta 叙事元数据（JSON存储）
type ChapterNarrativeMeta struct {
	Outline            string `json:"outline"`
	SceneOutline       string `json:"scene_outline"`
	TensionLevel       int    `json:"tension_level"`
	ActNo              int    `json:"act_no"`
	EmotionalTone      string `json:"emotional_tone"`
	HookType           string `json:"hook_type"`
	ChapterHook        string `json:"chapter_hook"`
	ReaderExpectations string `json:"reader_expectations"`
	ChapterEndState    string `json:"chapter_end_state"`
}

// ChapterQualityMeta 质量与发布元数据（JSON存储）
type ChapterQualityMeta struct {
	PublishedAt       *time.Time `json:"published_at"`
	ContinuityBlocked bool       `json:"continuity_blocked"`
	QualityStatus     string     `json:"quality_status"`
}

// Chapter 章节
type Chapter struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	TenantID  uint   `json:"tenant_id" gorm:"index;not null;default:0"` // 冗余租户 ID，避免多租户查询 JOIN ink_novel
	NovelID   uint   `json:"novel_id" gorm:"index;uniqueIndex:idx_chapter_novel_no,priority:1;index:idx_chapter_novel_status,priority:1;not null"`
	Novel     *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID      string `json:"uuid" gorm:"uniqueIndex;size:36"`
	ChapterNo int    `json:"chapter_no" gorm:"uniqueIndex:idx_chapter_novel_no,priority:2;not null"`
	Title     string `json:"title" gorm:"size:255"`

	// 内容
	Content   string `json:"content" gorm:"type:mediumtext"` // 章节正文，mediumtext 支持 16MB 防截断
	Summary   string `json:"summary" gorm:"type:text"`
	WordCount int    `json:"word_count" gorm:"default:0"`

	// 内容状态（不含发布状态）
	Status string `json:"status" gorm:"size:20;index:idx_chapter_novel_status,priority:2;default:draft"`
	// draft=草稿, generating=生成中, completed=已完成

	// 广场发布状态（与内容状态解耦）
	IsPublished bool `json:"is_published" gorm:"default:false;index"`

	// JSON 合并字段（减少列数）
	NarrativeMeta ChapterNarrativeMeta `json:"narrative_meta" gorm:"column:narrative_meta;serializer:json;type:text"`
	QualityMeta   ChapterQualityMeta   `json:"quality_meta" gorm:"column:quality_meta;serializer:json;type:text"`

	// 乐观锁版本号（协作编辑冲突检测）
	ContentVersion uint `json:"content_version" gorm:"default:1"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Chapter) TableName() string {
	return "ink_chapter"
}

// Worldview 世界观
type Worldview struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:0"`

	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Genre       string `json:"genre" gorm:"size:50;index"`

	// 关联实体（用于 Preload，避免 N+1 查询）
	Entities []*WorldviewEntity `json:"entities,omitempty" gorm:"foreignKey:WorldviewID"`

	// 世界观元素（mediumtext：玄幻/仙侠题材内容可能超过 64KB）
	MagicSystem string `json:"magic_system" gorm:"type:mediumtext"`
	Geography   string `json:"geography" gorm:"type:mediumtext"` // 地理格局，含文明发展水平
	History     string `json:"history" gorm:"type:mediumtext"`
	Culture     string `json:"culture" gorm:"type:mediumtext"` // 文化习俗，含宗教信仰

	// 约束规则
	Rules string `json:"rules" gorm:"type:mediumtext"`

	// 势力与术语
	Factions string `json:"factions" gorm:"type:mediumtext"` // 主要势力格局
	Glossary string `json:"glossary" gorm:"type:mediumtext"` // 世界专属术语表

	// 使用统计
	UsedCount int `json:"used_count" gorm:"default:0"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Worldview) TableName() string {
	return "ink_worldview"
}

// WorldviewEntity 世界观实体
type WorldviewEntity struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	TenantID    uint       `json:"tenant_id" gorm:"index;not null;default:0"` // 冗余租户 ID，避免跨租户查询 JOIN
	WorldviewID uint       `json:"worldview_id" gorm:"index;not null;uniqueIndex:idx_we_name_type,priority:1"`
	Worldview   *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`

	Type string `json:"type" gorm:"size:50;index;uniqueIndex:idx_we_name_type,priority:3"`
	// location=地点, organization=组织, artifact=神器, race=种族, creature=生物

	Name        string `json:"name" gorm:"size:255;not null;uniqueIndex:idx_we_name_type,priority:2"`
	Description string `json:"description" gorm:"type:text"`
	ImageURL    string `json:"image_url" gorm:"size:500"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (WorldviewEntity) TableName() string {
	return "ink_worldview_entity"
}

// ChapterVersion 章节版本
type ChapterVersion struct {
	ID        uint     `json:"id" gorm:"primaryKey"`
	NovelID   uint     `json:"novel_id" gorm:"index;not null;default:0"` // 冗余，便于级联删除和小说维度查询
	ChapterID uint     `json:"chapter_id" gorm:"uniqueIndex:idx_version_chapter,priority:1;not null"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	VersionNo int      `json:"version_no" gorm:"uniqueIndex:idx_version_chapter,priority:2;not null"`

	Content string `json:"content" gorm:"type:mediumtext"` // 正文副本，mediumtext 防截断

	ChangeType string `json:"change_type" gorm:"size:50"`
	// generation=AI生成, manual_edit=手动编辑, ai_revision=AI修改, rollback=回滚

	ChangeDescription string `json:"change_description" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (ChapterVersion) TableName() string {
	return "ink_chapter_version"
}

// ArcSummary 弧光摘要（每10章自动生成一次，用于长篇小说的层次化记忆）
// arc 1 = chapters 1-10, arc 2 = chapters 11-20, ...
type ArcSummary struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TenantID uint   `json:"tenant_id" gorm:"index;not null;default:0"` // 冗余租户 ID
	NovelID  uint   `json:"novel_id" gorm:"index;not null;uniqueIndex:idx_arc_novel_no"`
	Novel    *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ArcNo    int    `json:"arc_no" gorm:"not null;uniqueIndex:idx_arc_novel_no"` // 1, 2, 3...

	StartChapter int `json:"start_chapter" gorm:"not null"` // 起始章节号
	EndChapter   int `json:"end_chapter" gorm:"not null"`   // 结束章节号

	// 摘要内容（~200字，供后续章节生成使用）
	Summary string `json:"summary" gorm:"type:text"`

	// 关键事件 JSON: [{"chapter": N, "event": "..."}]
	KeyEvents string `json:"key_events" gorm:"type:text"`

	// 角色变化 JSON: {"角色名": "变化描述"}
	CharacterChanges string `json:"character_changes" gorm:"type:text"`

	// 未解决的伏笔 JSON: ["伏笔描述"]
	OpenForeshadows string `json:"open_foreshadows" gorm:"type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"` // 软删除，随小说删除时同步清理
}

func (ArcSummary) TableName() string {
	return "ink_arc_summary"
}

// NovelOutlineVersion 小说大纲历史版本（每次重新生成大纲前自动快照）
type NovelOutlineVersion struct {
	gorm.Model
	NovelID uint   `json:"novel_id" gorm:"not null;index;uniqueIndex:idx_outline_novel_ver,priority:1"`
	Version int    `json:"version" gorm:"not null;uniqueIndex:idx_outline_novel_ver,priority:2"`
	Outline string `json:"outline" gorm:"type:longtext"`
	Prompt  string `json:"prompt" gorm:"type:text"`
}

func (NovelOutlineVersion) TableName() string {
	return "ink_novel_outline_version"
}

// ReferenceNovel 参考小说
type ReferenceNovel struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TenantID uint   `json:"tenant_id" gorm:"index;not null;default:0"` // 0=全局共享，>0=租户私有
	Title    string `json:"title" gorm:"size:255;not null"`
	Author   string `json:"author" gorm:"size:100"`

	SourceURL  string `json:"source_url" gorm:"size:500;uniqueIndex:idx_ref_novel_url_site,priority:1"`
	SourceSite string `json:"source_site" gorm:"size:50;uniqueIndex:idx_ref_novel_url_site,priority:2"`
	// qidian=起点, jjwxc=晋江, zongheng=纵横

	Genre string `json:"genre" gorm:"size:50;index"`

	// 统计
	TotalChapters int `json:"total_chapters" gorm:"default:0"`

	// 状态
	Status       string `json:"status" gorm:"size:20;default:crawling"`
	ErrorMessage string `json:"error_message,omitempty" gorm:"type:text"` // 失败原因，status=failed 时记录
	// crawling=爬取中, completed=已完成, failed=失败

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ReferenceNovel) TableName() string {
	return "ink_reference_novel"
}

// ReferenceChapter 参考小说章节
type ReferenceChapter struct {
	ID        uint            `json:"id" gorm:"primaryKey"`
	TenantID  uint            `json:"tenant_id" gorm:"index;not null;default:0"` // 与 ReferenceNovel.TenantID 一致
	NovelID   uint            `json:"novel_id" gorm:"index;not null;uniqueIndex:idx_ref_chapter_novel_no,priority:1"`
	Novel     *ReferenceNovel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterNo int             `json:"chapter_no" gorm:"not null;uniqueIndex:idx_ref_chapter_novel_no,priority:2"`
	Title     string          `json:"title" gorm:"size:255"`
	Content   string          `json:"content" gorm:"type:mediumtext"` // mediumtext 防爬取内容截断

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ReferenceChapter) TableName() string {
	return "ink_reference_chapter"
}

// SystemSetting 系统全局配置（key-value）
type SystemSetting struct {
	Key         string    `gorm:"primaryKey;type:varchar(64)" json:"key"`
	Value       string    `gorm:"type:text"                   json:"value"`
	Description string    `gorm:"type:varchar(255)"           json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (SystemSetting) TableName() string { return "ink_system_setting" }

// ============================================
// Request / Response types (used by handlers)
// ============================================

type CreateNovelRequest struct {
	Title           string `json:"title" binding:"required"`
	Description     string `json:"description"`
	Genre           string `json:"genre" binding:"required"`
	WorldviewID     *uint  `json:"worldview_id"`
	CoverImage      string `json:"cover_image"`
	Channel         string `json:"channel"`
	TargetWordCount int    `json:"target_word_count"`
	TargetChapters  int    `json:"target_chapters"`
	ChapterMode     string `json:"chapter_mode"` // sequential / independent
	TenantID        uint   `json:"-"`
	UserID          uint   `json:"-"`
}

type UpdateNovelRequest struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Genre       string   `json:"genre"`
	Status      string   `json:"status"`
	WorldviewID *uint    `json:"worldview_id"`
	CoverImage  string   `json:"cover_image"`
	AIModel     string   `json:"ai_model"`
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	MaxTokens   *int     `json:"max_tokens"`
	StylePrompt    string `json:"style_prompt"`
	ImageStyle     string `json:"image_style"`
	PromptLanguage string `json:"prompt_language"`
	ChapterMode    string `json:"chapter_mode"` // sequential / independent
	CoreTheme      string `json:"core_theme"` // 全书核心主题
	// 自动审查
	AutoReviewRounds   *int     `json:"auto_review_rounds"`
	AutoReviewMinScore *float64 `json:"auto_review_min_score"`
	// 创作目标
	TargetWordCount *int `json:"target_word_count"`
	TargetChapters  *int `json:"target_chapters"`
	// 视频配置
	VideoType             string   `json:"video_type"`
	VideoResolution       string   `json:"video_resolution"`
	VideoFPS              *int     `json:"video_fps"`
	VideoAspectRatio      string   `json:"video_aspect_ratio"`
	CharConsistencyWeight *float64 `json:"char_consistency_weight"`
	NarrationVoice        string   `json:"narration_voice"`

	// 字幕配置
	SubtitleEnabled  *bool  `json:"subtitle_enabled"`
	SubtitlePosition string `json:"subtitle_position"`
	SubtitleFontSize *int   `json:"subtitle_font_size"`
	SubtitleColor    string `json:"subtitle_color"`
	SubtitleBgStyle  string `json:"subtitle_bg_style"`
	SubtitleFont     string `json:"subtitle_font"`

	// 超时
	TimeoutSeconds *int `json:"timeout_seconds"`

	// 色彩调色
	ColorGrade    string   `json:"color_grade"`
	ContrastLevel *float64 `json:"contrast_level"`
	Saturation    *float64 `json:"saturation"`

	// 镜头特效（bool 用指针，false 也要能写入）
	FilmGrain           *bool `json:"film_grain"`
	Vignette            *bool `json:"vignette"`
	ChromaticAberration *bool `json:"chromatic_aberration"`
	KlingProForAction   *bool `json:"kling_pro_for_action"`
}

// NovelCrawlJob 小说章节爬取任务（持久化进度，服务重启后可恢复）
type NovelCrawlJob struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	TenantID    uint       `json:"tenant_id" gorm:"index;not null;default:0"`
	NovelID     uint       `json:"novel_id" gorm:"index;not null"`
	Platform    string     `json:"platform" gorm:"size:30"`                 // qidian|jjwxc|zongheng
	SourceURL   string     `json:"source_url" gorm:"size:500"`              // 爬取起始 URL
	Status      string     `json:"status" gorm:"size:20;default:'running'"` // running|completed|partial|failed|paused
	Progress    int        `json:"progress" gorm:"default:0"`              // 已成功爬取章节数
	TotalChaps  int        `json:"total_chaps" gorm:"default:0"`           // 总章节数
	FailedCount int        `json:"failed_count" gorm:"default:0"`          // 失败章节数
	ErrorMessage string    `json:"error_message,omitempty" gorm:"type:text"` // 失败原因（status=failed 时记录）
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (NovelCrawlJob) TableName() string { return "ink_novel_crawl_job" }

// ContinuityReportRecord 连续性检查记录（持久化，保留历史记录）
type ContinuityReportRecord struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	NovelID    uint           `json:"novel_id" gorm:"index:idx_continuity_novel_chapter,priority:1;index;not null"`
	ChapterID  uint           `json:"chapter_id" gorm:"index:idx_continuity_novel_chapter,priority:2;index;not null"`
	ReportJSON string         `json:"report_json" gorm:"type:text"` // JSON of ContinuityReport
	IssueCount int            `json:"issue_count"`
	Passed     bool           `json:"passed"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ContinuityReportRecord) TableName() string { return "ink_continuity_report" }

// ForeshadowMeta 伏笔元数据（JSON存储）
type ForeshadowMeta struct {
	ForeshadowType        string `json:"foreshadow_type"`
	Importance            string `json:"importance"`
	Confidence            string `json:"confidence"`
	LinkedHookID          *uint  `json:"linked_hook_id"`
	LinkedArcID           *uint  `json:"linked_arc_id"`
	CharacterIDs          string `json:"character_ids"`
	ReinforcementChapters string `json:"reinforcement_chapters"`
	PayoffQuality         int    `json:"payoff_quality"`
	PayoffNotes           string `json:"payoff_notes"`
	ActualPayoffChapterID *uint  `json:"actual_payoff_chapter_id"`
	Tags                  string `json:"tags"`
	Description           string `json:"description"`
}

// Foreshadow 伏笔/预兆（专用表，替代通过 KnowledgeBase tag 存储的方案）
type Foreshadow struct {
	gorm.Model
	NovelID          uint   `json:"novel_id" gorm:"not null;index"`
	Title            string `json:"title" gorm:"size:200;not null"`
	PlantedChapterID *uint  `json:"planted_chapter_id,omitempty" gorm:"index"`
	PayoffChapterID  *uint  `json:"payoff_chapter_id,omitempty" gorm:"index"`
	Status           string `json:"status" gorm:"size:20;default:'planted'"` // planted, ripening, paid_off, abandoned

	// 生命周期增强字段
	PlantedChapterNo      int    `json:"planted_chapter_no" gorm:"default:0"`        // 种下的章节序号
	PayoffChapterNo       int    `json:"payoff_chapter_no" gorm:"default:0"`         // 预期回收章节序号（0=未规划）
	ActualPayoffChapterNo int    `json:"actual_payoff_chapter_no" gorm:"default:0"`  // 实际兑现章节序号（0=未兑现）
	Level                 string `json:"level" gorm:"size:20;default:'sub'"`         // main/sub/detail
	ParentID              *uint  `json:"parent_id,omitempty" gorm:"index"`           // 父伏笔 ID（支持层叠关系）

	// JSON 合并字段（减少列数）
	Meta ForeshadowMeta `json:"meta" gorm:"column:foreshadow_meta;serializer:json;type:text"`
}

func (Foreshadow) TableName() string { return "ink_foreshadow" }

// WebhookSubscription 用户配置的 Webhook 订阅
type WebhookSubscription struct {
	gorm.Model
	TenantID  uint   `gorm:"not null;index"`
	URL       string `gorm:"size:500;not null"`
	Secret    string `gorm:"size:200"` // for HMAC-SHA256 signing
	Events    string `gorm:"type:text"` // JSON array of event types
	IsActive  bool   `gorm:"default:true"`
	LastError string `gorm:"type:text"`
	FailCount int    `gorm:"default:0"`
}

func (WebhookSubscription) TableName() string { return "ink_webhook_subscription" }

// WebhookDelivery 投递记录
type WebhookDelivery struct {
	gorm.Model
	SubscriptionID uint   `gorm:"not null;index"`
	EventType      string `gorm:"size:100"`
	Payload        string `gorm:"type:text"`
	StatusCode     int
	ResponseBody   string `gorm:"type:text"`
	Attempt        int    `gorm:"default:1"`
	Success        bool   `gorm:"default:false"`
}

func (WebhookDelivery) TableName() string { return "ink_webhook_delivery" }

// AuditLog 审计日志
type AuditLog struct {
	ID         uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt    time.Time `json:"created_at" gorm:"index"`
	TenantID     uint      `json:"tenant_id" gorm:"index"`
	UserID       uint      `json:"user_id" gorm:"index"`
	NovelID      uint      `json:"novel_id" gorm:"index;default:0"` // 0=用户级，>0=项目级
	Action       string    `json:"action" gorm:"size:60;not null"`  // e.g. "novel.create"
	ResourceType string    `json:"resource_type" gorm:"size:50"`
	ResourceID   uint      `json:"resource_id" gorm:"default:0"`
	ResourceName string    `json:"resource_name" gorm:"size:200"`
	Details      string    `json:"details,omitempty" gorm:"type:text"` // JSON
	IP           string    `json:"ip" gorm:"size:64"`
	Status       string    `json:"status" gorm:"size:20;default:'ok'"` // ok / fail
	// Legacy fields kept for backward compat
	EntityType string `json:"entity_type,omitempty" gorm:"size:50"`
	EntityID   uint   `json:"entity_id,omitempty"`
	IPAddress  string `json:"ip_address,omitempty" gorm:"size:50"`
	UserAgent  string `json:"user_agent,omitempty" gorm:"size:500"`
	Detail     string `json:"detail,omitempty" gorm:"type:text"`
}

func (AuditLog) TableName() string { return "ink_audit_log" }

// OutlineScores 分项得分（JSON存储）
type OutlineScores struct {
	StructureScore  float64 `json:"structure_score"`
	PacingScore     float64 `json:"pacing_score"`
	ContinuityScore float64 `json:"continuity_score"`
	CharacterScore  float64 `json:"character_score"`
	ConflictScore   float64 `json:"conflict_score"`
	HookScore       float64 `json:"hook_score"`
}

// OutlineReviewContent 审查内容（JSON存储）
type OutlineReviewContent struct {
	IssuesJSON     string `json:"issues_json"`
	HighlightsJSON string `json:"highlights_json"`
	Suggestion     string `json:"suggestion"`
}

// OutlineReview 章节大纲审查结果
type OutlineReview struct {
	gorm.Model
	NovelID      uint       `json:"novel_id" gorm:"index"`
	ChapterID    uint       `json:"chapter_id" gorm:"uniqueIndex"` // one latest review per chapter
	ChapterNo    int        `json:"chapter_no"`
	Status       string     `json:"status" gorm:"size:20;default:'pending'"` // pending/reviewing/passed/warning/failed
	OverallScore float64    `json:"overall_score"`                           // 0-100

	// JSON 合并字段（减少列数）
	Scores  OutlineScores        `json:"scores" gorm:"column:review_scores;serializer:json;type:text"`
	Content OutlineReviewContent `json:"content" gorm:"column:review_content;serializer:json;type:text"`

	ReviewedAt *time.Time `json:"reviewed_at"`
}

func (OutlineReview) TableName() string { return "ink_outline_review" }

// NovelOutlineSynthesis 小说整体大纲批量审查综合报告（每部小说一条，按 novel_id 唯一）
// updated_at（来自 gorm.Model）即代表最后一次综合分析时间，无需单独 synthesized_at 字段。
type NovelOutlineSynthesis struct {
	gorm.Model
	NovelID uint `json:"novel_id" gorm:"uniqueIndex"`
	TotalChapters        int     `json:"total_chapters"`   // novel.Outline 中规划的总章数
	ReviewedCount        int     `json:"reviewed_count"`   // 实际完成审查的章数
	PassedCount          int     `json:"passed_count"`
	WarningCount         int     `json:"warning_count"`
	FailedCount          int     `json:"failed_count"`
	AvgScore             float64 `json:"avg_score"`
	ArcBalanceJSON       string  `json:"arc_balance_json" gorm:"type:text"`      // 三幕结构分析 JSON
	TensionCurveJSON     string  `json:"tension_curve_json" gorm:"type:text"`    // 逐章张力曲线 [{chapter_no,tension,score}]
	RecurringIssuesJSON  string  `json:"recurring_issues_json" gorm:"type:text"` // []OutlineIssue 高频共性问题
	ChapterAdvicesJSON   string  `json:"chapter_advices_json" gorm:"type:text"`  // []ChapterAdvice 逐章改进建议
	GlobalSuggestion     string  `json:"global_suggestion" gorm:"type:text"`
	Status               string  `json:"status" gorm:"size:20"` // completed/partial
}

func (NovelOutlineSynthesis) TableName() string { return "ink_outline_synthesis" }

// ChapterAdvice 批量审查中单章的改进建议摘要
type ChapterAdvice struct {
	ChapterNo  int     `json:"chapter_no"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Status     string  `json:"status"` // passed/warning/failed/skipped
	KeyIssue   string  `json:"key_issue"`
	Suggestion string  `json:"suggestion"`
}

// ArcBalance 三幕结构均衡分析
type ArcBalance struct {
	Act1Count     int     `json:"act1_count"`
	Act2Count     int     `json:"act2_count"`
	Act3Count     int     `json:"act3_count"`
	Act1AvgScore  float64 `json:"act1_avg_score"`
	Act2AvgScore  float64 `json:"act2_avg_score"`
	Act3AvgScore  float64 `json:"act3_avg_score"`
	Assessment    string  `json:"assessment"` // AI 对幕次分配的评价
	Suggestion    string  `json:"suggestion"`
}

// TensionPoint 单章张力点（用于曲线图）
type TensionPoint struct {
	ChapterNo    int     `json:"chapter_no"`
	PlannedLevel int     `json:"planned_level"` // 大纲规划张力 0-10
	Score        float64 `json:"score"`         // 审查得分 0-100（-1 表示未审查）
	Status       string  `json:"status"`
}

// OutlineIssue 大纲审查问题条目
type OutlineIssue struct {
	Dimension   string `json:"dimension"`   // structure/pacing/continuity/character/conflict/hook
	Severity    string `json:"severity"`    // error/warning/info
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
}

type CreateChapterRequest struct {
	ChapterNo int    `json:"chapter_no"`
	Title     string `json:"title"`
	Content   string `json:"content"`
}

type UpdateChapterRequest struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	ChapterHook string `json:"chapter_hook"`
	Outline     string `json:"outline"`
}

// BatchGenerateChaptersRequest 批量生成章节正文请求
type BatchGenerateChaptersRequest struct {
	SkipExisting   bool   `json:"skip_existing"`    // true=跳过已有正文的章节（默认 true）
	WordCount      int    `json:"word_count"`       // 每章目标字数，0=自动推算
	MaxTokens      int    `json:"max_tokens"`       // LLM max tokens，0=自动
	StartChapterNo int    `json:"start_chapter_no"` // 从第几章开始，0=全部
	EndChapterNo   int    `json:"end_chapter_no"`   // 到第几章结束，0=全部
	ModelOverride  string `json:"model"`            // 可选：指定 AI 模型/provider
}

type GenerateChapterRequest struct {
	NovelID        uint    `json:"novel_id"`
	ChapterNo      int     `json:"chapter_no" binding:"required,min=1"`
	Prompt         string  `json:"prompt"`
	WordCount      int     `json:"word_count"`      // 章节目标字数；0=从小说配置推算
	MaxTokens      int     `json:"max_tokens"`      // LLM max tokens；0=自动；不影响目标字数
	Temperature    float64 `json:"temperature,omitempty"`    // 0=使用项目配置或系统默认
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"` // 0=使用项目配置或系统默认
	ModelOverride  string  `json:"model,omitempty"` // 可选：指定使用的 AI 模型/provider
	IsStandalone    bool     `json:"is_standalone"`    // true=最终章，要求故事完整收尾；可显式传入，也会由系统根据 chapter_no >= target_chapters 自动推断
	WebSearch       bool     `json:"web_search"`       // true=启用联网参考，搜索相关故事片段注入 prompt
	WikiSearch      bool     `json:"wiki_search"`      // true=启用百科知识查询，注入世界观准确信息
	UseStoryPattern bool     `json:"use_story_pattern"` // true=启用情节模板，注入叙事结构参考
	ReviewHints     *ReviewHintsPayload `json:"review_hints,omitempty"` // AI审查反馈，重生成时注入
}

// ReviewHintsPayload 审查反馈摘要，用于指导重新生成
type ReviewHintsPayload struct {
	Weaknesses      []string `json:"weaknesses"`       // 整体不足列表（issue + suggestion 合并）
	ParagraphIssues []string `json:"paragraph_issues"` // 选中段落的问题描述
	ExistingContent string   `json:"-"`                // 当前章节正文（由 RegenerateChapter 自动填充，不从请求读取）
}

// SensitiveWordRule 敏感词替换规则（DB 可配置，管理员增删改）。
// 图像生成 prompt 在发往 AI API 前，PromptFilter 会依次应用所有启用的规则。
type SensitiveWordRule struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	TenantID    uint      `gorm:"index" json:"tenant_id"`
	Word        string    `gorm:"size:200;uniqueIndex:idx_sw_tenant_word" json:"word"`
	Replacement string    `gorm:"size:200" json:"replacement"`
	Category    string    `gorm:"size:50" json:"category"` // violence / adult / political / other
	Enabled     bool      `gorm:"default:true" json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
