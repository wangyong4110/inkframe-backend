package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log"
	"time"

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

// AsyncTask 统一异步任务（DB 持久化，页面刷新后仍可恢复）
// 索引说明：
//   - idx_task_tenant_status: 按租户+状态过滤（ListByTenant、cleanup）
//   - idx_task_tenant_type_status: 按租户+类型+状态组合查询
//   - idx_task_tenant_created: 按租户+创建时间过滤（per-tenant fairness 轮询）
type AsyncTask struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	TaskID     string         `json:"task_id" gorm:"uniqueIndex;size:64;not null"`
	TenantID   uint           `json:"tenant_id" gorm:"not null;default:1;index:idx_task_tenant_status;index:idx_task_tenant_type_status;index:idx_task_tenant_created,priority:1"`
	Type       string         `json:"type" gorm:"size:50;not null;index:idx_task_tenant_type_status"`
	Status     string         `json:"status" gorm:"size:20;not null;default:pending;index:idx_task_tenant_status;index:idx_task_tenant_type_status"`
	Title      string         `json:"title" gorm:"size:255"`
	EntityType string         `json:"entity_type" gorm:"size:50"`
	EntityID   uint           `json:"entity_id" gorm:"index"`
	ResultJSON string         `json:"-" gorm:"column:result;type:mediumtext"` // mediumtext 支持 16MB，避免大结果超 text 65KB 限制
	ParamsJSON string         `json:"-" gorm:"column:params;type:mediumtext"` // mediumtext 支持 16MB，避免大参数超 text 65KB 限制
	Error      string         `json:"error,omitempty" gorm:"type:text"`
	Progress   int            `json:"progress" gorm:"default:0"`
	// Dead-letter queue fields: track retries and accumulated failure logs.
	// Status "dead" means all retries exhausted and the task will not be retried.
	RetryCount int    `json:"retry_count" gorm:"not null;default:0"`
	MaxRetries int    `json:"max_retries" gorm:"not null;default:3"`
	FailureLog string `json:"failure_log,omitempty" gorm:"type:text"`
	CreatedAt  time.Time      `json:"created_at" gorm:"index:idx_task_tenant_created,priority:2"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `json:"-" gorm:"index"`
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
			log.Printf("[AsyncTask] task %s: ResultJSON unmarshal failed: %v", t.TaskID, err)
		} else {
			m["data"] = data
		}
	}
	return json.Marshal(m)
}

// Novel 小说
type Novel struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	TenantID    uint   `json:"tenant_id" gorm:"index;index:idx_novel_tenant_status,priority:1;not null;default:1"`
	Title       string `json:"title" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Genre       string `json:"genre" gorm:"size:50;index"`

	// 状态
	Status string `json:"status" gorm:"size:20;index;index:idx_novel_tenant_status,priority:2;default:planning"`
	// planning=规划中, writing=创作中, paused=暂停, completed=已完成, archived=已归档

	// 频道与分类（创作目标）
	Channel         string `json:"channel" gorm:"size:50"`             // female=女生原创, male=男生原创, publish=出版图书
	TargetWordCount int    `json:"target_word_count" gorm:"default:0"` // 目标字数（字，如 300000 = 30万字）
	TargetChapters  int    `json:"target_chapters" gorm:"default:0"`   // 目标章节数

	// 统计
	TotalWords     int `json:"total_words" gorm:"default:0"`
	ChapterCount   int `json:"chapter_count" gorm:"default:0"`
	PublishedCount int `json:"published_count" gorm:"default:0"`

	// 关联
	WorldviewID *uint      `json:"worldview_id"`
	Worldview   *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`
	CoverImage  string     `json:"cover_image" gorm:"size:500"`

	// AI配置（项目级，作为所有生成操作的默认参数）
	AIModel     string  `json:"ai_model" gorm:"size:100"`    // LLM 模型（章节生成等文本任务）
	ImageModel  string  `json:"image_model" gorm:"size:100"` // 图片生成模型
	VideoModel  string  `json:"video_model" gorm:"size:100"` // 视频生成模型
	TTSModel    string  `json:"tts_model" gorm:"size:100"`   // 语音合成模型
	Temperature    float64 `json:"temperature" gorm:"type:decimal(3,2);default:0.7"`
	TopP           float64 `json:"top_p" gorm:"type:decimal(3,2);default:0.9"`
	TopK           int     `json:"top_k" gorm:"default:40"`
	MaxTokens      int     `json:"max_tokens" gorm:"default:0"` // 0=不限制，由模型自身决定
	TimeoutSeconds int     `json:"timeout_seconds" gorm:"default:0"` // 0=使用系统默认(300s)
	StylePrompt string `json:"style_prompt" gorm:"type:text"`
	Outline     string `json:"outline,omitempty" gorm:"type:text"` // 大纲 JSON（章节列表）

	// 风格配置
	ImageStyle     string `json:"image_style" gorm:"size:50"`      // 视觉/图片风格，如 anime/realistic/ink_painting
	PromptLanguage string `json:"prompt_language" gorm:"size:10;default:zh"` // AI 提示词语言：zh（中文，默认）/ en（英文）

	// 视频/字幕配置（已迁移至 ink_novel_video_config，通过 VideoConfig 关联访问）
	VideoConfig *NovelVideoConfig `json:"video_config,omitempty" gorm:"foreignKey:NovelID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	// 广场社交字段
	IsPublished  bool       `json:"is_published" gorm:"default:false;index"`
	PublishedAt  *time.Time `json:"published_at"`
	Visibility   string     `json:"visibility" gorm:"size:20;default:'private'"` // private|unlisted|public
	ViewCount    int        `json:"view_count" gorm:"default:0"`
	LikeCount    int        `json:"like_count" gorm:"default:0"`
	CommentCount int        `json:"comment_count" gorm:"default:0"`
	HotScore     float64    `json:"hot_score" gorm:"default:0;index"`
	PlazaTags    string     `json:"plaza_tags" gorm:"type:text"` // JSON 数组，如 ["玄幻","古风"]

	// 内容审核
	ReviewStatus string     `json:"review_status" gorm:"size:20;default:'draft'"` // draft|pending_review|approved|rejected
	ReviewNote   string     `json:"review_note" gorm:"size:500"`                  // 审核不通过的原因
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy   uint       `json:"reviewed_by" gorm:"default:0"`

	// 文件去重（记录导入来源文件内容哈希，用于防止同一文件重复导入）
	SourceFileHash string `json:"source_file_hash,omitempty" gorm:"size:64;index"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Novel) TableName() string {
	return "ink_novel"
}

// VideoConf returns video/subtitle config with safe nil handling.
func (n *Novel) VideoConf() NovelVideoConfig {
	if n.VideoConfig != nil {
		return *n.VideoConfig
	}
	return NovelVideoConfig{
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

// MarshalJSON flattens VideoConfig fields into the top-level Novel JSON so the
// frontend can read them directly (e.g. novel.video_type instead of novel.video_config.video_type).
// Uses direct field mapping instead of triple JSON round-trip for better performance.
func (n Novel) MarshalJSON() ([]byte, error) {
	// Type alias prevents infinite recursion when calling json.Marshal below.
	type NovelAlias Novel
	base, err := json.Marshal(NovelAlias(n))
	if err != nil {
		return nil, err
	}
	if n.VideoConfig == nil {
		return base, nil
	}
	// Unmarshal base once, then inject VideoConfig fields directly — no second marshal of vc.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	vc := n.VideoConfig
	vcFields := map[string]any{
		"video_type":              vc.VideoType,
		"video_resolution":        vc.VideoResolution,
		"video_fps":               vc.VideoFPS,
		"video_aspect_ratio":      vc.VideoAspectRatio,
		"char_consistency_weight": vc.CharConsistencyWeight,
		"asset_export_path":       vc.AssetExportPath,
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
	return json.Marshal(m)
}

// Chapter 章节
type Chapter struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	NovelID  uint   `json:"novel_id" gorm:"index;uniqueIndex:idx_chapter_novel_no,priority:1;index:idx_chapter_novel_status,priority:1;not null"`
	TenantID uint   `json:"tenant_id" gorm:"index;not null;default:1"`
	Novel    *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID      string `json:"uuid" gorm:"uniqueIndex;size:36"`
	ChapterNo int    `json:"chapter_no" gorm:"uniqueIndex:idx_chapter_novel_no,priority:2;not null"`
	Title     string `json:"title" gorm:"size:255"`

	// 内容
	Content   string `json:"content" gorm:"type:text"`
	Summary   string `json:"summary" gorm:"type:text"`
	WordCount int    `json:"word_count" gorm:"default:0"`

	// 大纲与场景结构
	Outline      string `json:"outline" gorm:"type:text"`
	SceneOutline string `json:"scene_outline" gorm:"type:text"` // JSON: 场景级大纲（3-5个场景）

	// 叙事元数据（来自小说大纲）
	TensionLevel  int    `json:"tension_level" gorm:"default:0"` // 0-10 张力值
	ActNo         int    `json:"act_no" gorm:"default:0"`        // 所属幕次（1/2/3）
	EmotionalTone string `json:"emotional_tone" gorm:"size:50"`  // 情感基调
	HookType      string `json:"hook_type" gorm:"size:30"`       // 章末钩子类型
	ChapterHook   string `json:"chapter_hook" gorm:"type:text"`  // 章末钩子正文（供下一章生成时使用）

	// 内容状态（不含发布状态）
	Status string `json:"status" gorm:"size:20;index:idx_chapter_novel_status,priority:2;default:draft"`
	// draft=草稿, generating=生成中, completed=已完成

	// ContinuityBlocked 当连贯性检查发现 high/critical 级别问题时由异步后处理标记为 true。
	// 不阻塞章节返回，前端根据此字段决定是否提示用户审查。
	ContinuityBlocked bool `json:"continuity_blocked" gorm:"default:false"`

	// QualityStatus 质量状态：经质检+精修后仍不达标时标记为 "low"，默认 "ok"
	QualityStatus string `json:"quality_status" gorm:"size:20;default:'ok'"`
	// QualityIssues 质量问题 JSON 摘要（仅在 QualityStatus="low" 时写入）
	QualityIssues string `json:"quality_issues,omitempty" gorm:"type:text"`

	// 广场发布状态（与内容状态解耦）
	IsPublished bool       `json:"is_published" gorm:"default:false;index"`
	PublishedAt *time.Time `json:"published_at"`

	// 广场社交数据
	LikeCount int `json:"like_count" gorm:"default:0"`

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
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`

	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Genre       string `json:"genre" gorm:"size:50;index"`

	// 关联实体（用于 Preload，避免 N+1 查询）
	Entities []*WorldviewEntity `json:"entities,omitempty" gorm:"foreignKey:WorldviewID"`

	// 世界观元素
	MagicSystem string `json:"magic_system" gorm:"type:text"`
	Geography   string `json:"geography" gorm:"type:text"`
	History     string `json:"history" gorm:"type:text"`
	Culture     string `json:"culture" gorm:"type:text"`
	Technology  string `json:"technology" gorm:"type:text"`

	// 约束规则
	Rules string `json:"rules" gorm:"type:text"`

	// 金手指/系统（可选）
	CheatSystem string `json:"cheat_system" gorm:"type:text"`

	// 扩展世界观元素
	Factions            string `json:"factions" gorm:"type:text"`             // 势力格局
	CoreConflicts       string `json:"core_conflicts" gorm:"type:text"`       // 核心矛盾
	CharacterArchetypes string `json:"character_archetypes" gorm:"type:text"` // 典型人物原型
	Religion            string `json:"religion" gorm:"type:text"`             // 宗教与信仰
	Glossary            string `json:"glossary" gorm:"type:text"`             // 术语词汇表

	// 封面
	CoverImage string `json:"cover_image" gorm:"size:500"`

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
	WorldviewID uint       `json:"worldview_id" gorm:"index;not null"`
	Worldview   *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`

	Type string `json:"type" gorm:"size:50;index"`
	// location=地点, organization=组织, artifact=神器, race=种族, creature=生物

	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Attributes  string `json:"attributes" gorm:"type:text"` // JSON
	Relations   string `json:"relations" gorm:"type:text"`  // JSON
	ImageURL    string `json:"image_url" gorm:"size:500"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (WorldviewEntity) TableName() string {
	return "ink_worldview_entity"
}

// QualityReport 质量报告
type QualityReport struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`

	UUID       string `json:"uuid" gorm:"uniqueIndex;size:36"`
	TargetType string `json:"target_type" gorm:"size:50;index:idx_quality_target,priority:1"`
	// novel=小说, chapter=章节, video=视频

	TargetID uint `json:"target_id" gorm:"index:idx_quality_target,priority:2;not null"`

	// 整体评分
	OverallScore     float64 `json:"overall_score" gorm:"type:decimal(5,4)"`
	QualityScore     float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`
	CreativityScore  float64 `json:"creativity_score" gorm:"type:decimal(5,4)"`

	// 问题统计
	TotalIssues    int `json:"total_issues"`
	HighPriority   int `json:"high_priority"`
	MediumPriority int `json:"medium_priority"`
	LowPriority    int `json:"low_priority"`

	// 详细报告（JSON）
	Issues      string `json:"issues" gorm:"type:text"`
	Suggestions string `json:"suggestions" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (QualityReport) TableName() string {
	return "ink_quality_report"
}

// ChapterVersion 章节版本
type ChapterVersion struct {
	ID        uint     `json:"id" gorm:"primaryKey"`
	ChapterID uint     `json:"chapter_id" gorm:"uniqueIndex:idx_version_chapter,priority:1;not null"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	VersionNo int      `json:"version_no" gorm:"uniqueIndex:idx_version_chapter,priority:2;not null"`

	Content string `json:"content" gorm:"type:text"`

	ChangeType string `json:"change_type" gorm:"size:50"`
	// generation=AI生成, manual_edit=手动编辑, ai_revision=AI修改, rollback=回滚

	ChangeDescription string `json:"change_description" gorm:"type:text"`
	ChangeAuthorID    *uint  `json:"change_author_id,omitempty"`

	QualityScore     float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`

	CreatedAt time.Time `json:"created_at"`
}

func (ChapterVersion) TableName() string {
	return "ink_chapter_version"
}

// ArcSummary 弧光摘要（每10章自动生成一次，用于长篇小说的层次化记忆）
// arc 1 = chapters 1-10, arc 2 = chapters 11-20, ...
type ArcSummary struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	NovelID uint   `json:"novel_id" gorm:"index;not null;uniqueIndex:idx_arc_novel_no"`
	Novel   *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ArcNo   int    `json:"arc_no" gorm:"not null;uniqueIndex:idx_arc_novel_no"` // 1, 2, 3...

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

	// 张力曲线（本弧最高/最低张力点）
	PeakTension int `json:"peak_tension" gorm:"default:0"`
	LowTension  int `json:"low_tension" gorm:"default:0"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ArcSummary) TableName() string {
	return "ink_arc_summary"
}

// NovelOutlineVersion 小说大纲历史版本（每次重新生成大纲前自动快照）
type NovelOutlineVersion struct {
	gorm.Model
	TenantID uint   `json:"tenant_id" gorm:"index"`
	NovelID  uint   `json:"novel_id" gorm:"not null;index"`
	Version  int    `json:"version" gorm:"not null"`
	Outline  string `json:"outline" gorm:"type:longtext"`
	Prompt   string `json:"prompt" gorm:"type:text"`
}

func (NovelOutlineVersion) TableName() string {
	return "ink_novel_outline_version"
}

// ReferenceNovel 参考小说
type ReferenceNovel struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	UUID   string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Title  string `json:"title" gorm:"size:255;not null"`
	Author string `json:"author" gorm:"size:100"`

	SourceURL  string `json:"source_url" gorm:"size:500"`
	SourceSite string `json:"source_site" gorm:"size:50"`
	// qidian=起点, jjwxc=晋江, zongheng=纵横

	Genre string `json:"genre" gorm:"size:50;index"`

	// 统计
	TotalChapters int `json:"total_chapters" gorm:"default:0"`
	TotalWords    int `json:"total_words" gorm:"default:0"`

	// 状态
	Status string `json:"status" gorm:"size:20;default:crawling"`
	// crawling=爬取中, completed=已完成, failed=失败

	// 封面
	CoverImage string `json:"cover_image" gorm:"size:500"`

	// 时间戳
	CrawledAt *time.Time `json:"crawled_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (ReferenceNovel) TableName() string {
	return "ink_reference_novel"
}

// ReferenceChapter 参考小说章节
type ReferenceChapter struct {
	ID        uint            `json:"id" gorm:"primaryKey"`
	NovelID   uint            `json:"novel_id" gorm:"index;not null"`
	Novel     *ReferenceNovel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID      string          `json:"uuid" gorm:"uniqueIndex;size:36"`
	ChapterNo int             `json:"chapter_no" gorm:"not null"`
	Title     string          `json:"title" gorm:"size:255"`
	Content   string          `json:"content" gorm:"type:text"`
	Summary   string          `json:"summary" gorm:"type:text"`
	WordCount int             `json:"word_count" gorm:"default:0"`

	CreatedAt time.Time `json:"created_at"`
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
	TenantID        uint   `json:"-"`
}

type UpdateNovelRequest struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Genre       string   `json:"genre"`
	Status      string   `json:"status"`
	WorldviewID *uint    `json:"worldview_id"`
	CoverImage  string   `json:"cover_image"`
	AIModel     string   `json:"ai_model"`
	ImageModel  string   `json:"image_model"`
	VideoModel  string   `json:"video_model"`
	TTSModel    string   `json:"tts_model"`
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	TopK        *int     `json:"top_k"`
	MaxTokens   *int     `json:"max_tokens"`
	StylePrompt    string `json:"style_prompt"`
	ImageStyle     string `json:"image_style"`
	PromptLanguage string `json:"prompt_language"`
	// 创作目标
	TargetWordCount *int `json:"target_word_count"`
	TargetChapters  *int `json:"target_chapters"`
	// 视频配置
	VideoType             string   `json:"video_type"`
	VideoResolution       string   `json:"video_resolution"`
	VideoFPS              *int     `json:"video_fps"`
	VideoAspectRatio      string   `json:"video_aspect_ratio"`
	CharConsistencyWeight *float64 `json:"char_consistency_weight"`
	AssetExportPath       string   `json:"asset_export_path"`
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
	ID         uint       `json:"id" gorm:"primaryKey"`
	NovelID    uint       `json:"novel_id" gorm:"index;not null"`
	Status     string     `json:"status" gorm:"size:20;default:'running'"` // running|completed|partial|failed|paused
	Progress   int        `json:"progress" gorm:"default:0"`              // 已成功爬取章节数
	TotalChaps int        `json:"total_chaps" gorm:"default:0"`           // 总章节数
	FailedCount int       `json:"failed_count" gorm:"default:0"`          // 失败章节数
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (NovelCrawlJob) TableName() string { return "ink_novel_crawl_job" }

// ContinuityReportRecord 连续性检查记录（持久化）
type ContinuityReportRecord struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	NovelID    uint           `json:"novel_id" gorm:"index;not null"`
	ChapterID  uint           `json:"chapter_id" gorm:"index;not null"`
	TenantID   uint           `json:"tenant_id" gorm:"index;not null"`
	ReportJSON string         `json:"report_json" gorm:"type:text"` // JSON of ContinuityReport
	IssueCount int            `json:"issue_count"`
	Passed     bool           `json:"passed"`
	CreatedAt  time.Time      `json:"created_at"`
	DeletedAt  gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ContinuityReportRecord) TableName() string { return "ink_continuity_report" }

// Foreshadow 伏笔/预兆（专用表，替代通过 KnowledgeBase tag 存储的方案）
type Foreshadow struct {
	gorm.Model
	TenantID         uint   `json:"tenant_id" gorm:"index"`
	NovelID          uint   `json:"novel_id" gorm:"not null;index"`
	Title            string `json:"title" gorm:"size:200;not null"`
	Description      string `json:"description" gorm:"type:text"`
	PlantedChapterID *uint  `json:"planted_chapter_id,omitempty" gorm:"index"`
	PayoffChapterID  *uint  `json:"payoff_chapter_id,omitempty" gorm:"index"`
	Status           string `json:"status" gorm:"size:20;default:'planted'"` // planted, paid_off, abandoned
	Tags             string `json:"tags" gorm:"size:500"`
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
	CreatedAt  time.Time `json:"created_at" gorm:"index"`
	TenantID   uint      `json:"tenant_id" gorm:"index"`
	UserID     uint      `json:"user_id" gorm:"index"`
	Action     string    `json:"action" gorm:"size:100;not null"` // e.g. "novel.create", "chapter.delete"
	EntityType string    `json:"entity_type" gorm:"size:50"`
	EntityID   uint      `json:"entity_id"`
	IPAddress  string    `json:"ip_address" gorm:"size:50"`
	UserAgent  string    `json:"user_agent" gorm:"size:500"`
	Detail     string    `json:"detail" gorm:"type:text"` // JSON extras
}

func (AuditLog) TableName() string { return "ink_audit_log" }

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

type GenerateChapterRequest struct {
	NovelID        uint    `json:"novel_id"`
	ChapterNo      int     `json:"chapter_no" binding:"required,min=1"`
	Prompt         string  `json:"prompt"`
	WordCount      int     `json:"word_count"`      // 章节目标字数；0=从小说配置推算
	MaxTokens      int     `json:"max_tokens"`      // LLM max tokens；0=自动；不影响目标字数
	Temperature    float64 `json:"temperature,omitempty"`    // 0=使用项目配置或系统默认
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"` // 0=使用项目配置或系统默认
	ModelOverride  string  `json:"model,omitempty"` // 可选：指定使用的 AI 模型/provider
	IsStandalone    bool    `json:"is_standalone"`    // true=最终章，要求故事完整收尾；可显式传入，也会由系统根据 chapter_no >= target_chapters 自动推断
	WebSearch       bool    `json:"web_search"`       // true=启用联网参考，搜索相关故事片段注入 prompt
	WikiSearch      bool    `json:"wiki_search"`      // true=启用百科知识查询，注入世界观准确信息
	UseStoryPattern bool    `json:"use_story_pattern"` // true=启用情节模板，注入叙事结构参考
}
