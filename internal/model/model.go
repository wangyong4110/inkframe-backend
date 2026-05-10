package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"time"
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
type AsyncTask struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	TaskID     string         `json:"task_id" gorm:"uniqueIndex;size:64;not null"`
	TenantID   uint           `json:"tenant_id" gorm:"index;not null;default:1"`
	Type       string         `json:"type" gorm:"size:50;index;not null"`
	Status     string         `json:"status" gorm:"size:20;index;not null;default:pending"`
	Title      string         `json:"title" gorm:"size:255"`
	EntityType string         `json:"entity_type" gorm:"size:50"`
	EntityID   uint           `json:"entity_id" gorm:"index"`
	ResultJSON string         `json:"-" gorm:"column:result;type:mediumtext"` // mediumtext 支持 16MB，避免大结果超 text 65KB 限制
	Error      string         `json:"error,omitempty" gorm:"type:text"`
	Progress   int            `json:"progress" gorm:"default:0"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `json:"-" gorm:"index"`
}

func (AsyncTask) TableName() string { return "ink_async_task" }

// MediaAsset 媒体素材（图片/音频/视频/字幕），OSS 未配置时存 DB
type MediaAsset struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	TenantID    uint      `gorm:"index" json:"tenant_id"`
	NovelID     uint      `gorm:"index" json:"novel_id"`
	ChapterID   uint      `gorm:"index" json:"chapter_id"`
	MediaType   string    `gorm:"size:20;index" json:"media_type"` // image/audio/video/subtitle
	Filename    string    `gorm:"size:255" json:"filename"`
	ContentType string    `gorm:"size:100" json:"content_type"`
	Size        int64     `json:"size"`
	Data        []byte    `gorm:"type:longblob" json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

func (MediaAsset) TableName() string { return "ink_media_asset" }

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
		"created_at":  t.CreatedAt,
		"updated_at":  t.UpdatedAt,
	}
	if t.Error != "" {
		m["error"] = t.Error
	}
	if t.ResultJSON != "" {
		var data interface{}
		if err := json.Unmarshal([]byte(t.ResultJSON), &data); err == nil {
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
	TargetWordCount int    `json:"target_word_count" gorm:"default:0"` // 目标字数（万字）
	TargetChapters  int    `json:"target_chapters" gorm:"default:0"`   // 目标章节数

	// 统计
	TotalWords   int `json:"total_words" gorm:"default:0"`
	ChapterCount int `json:"chapter_count" gorm:"default:0"`

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
	StylePrompt    string  `json:"style_prompt" gorm:"type:text"`

	// 风格配置
	ImageStyle string `json:"image_style" gorm:"size:50"` // 视觉/图片风格，如 anime/realistic/ink_painting

	// 视频/字幕配置（已迁移至 ink_novel_video_config，通过 VideoConfig 关联访问）
	VideoConfig *NovelVideoConfig `json:"video_config,omitempty" gorm:"foreignKey:NovelID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

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

// NovelVideoConfig 小说视频/字幕配置（1:1 with Novel，独立表）
type NovelVideoConfig struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	NovelID uint `json:"novel_id" gorm:"uniqueIndex;not null"`

	// 视频生成配置
	VideoType             string  `json:"video_type" gorm:"size:20;default:'animation'"`
	VideoResolution       string  `json:"video_resolution" gorm:"size:20;default:'1080p'"`
	VideoFPS              int     `json:"video_fps" gorm:"default:30"`
	VideoAspectRatio      string  `json:"video_aspect_ratio" gorm:"size:10;default:'16:9'"`
	CharConsistencyWeight float64 `json:"char_consistency_weight" gorm:"type:decimal(3,2);default:1.0"`
	AssetExportPath       string  `json:"asset_export_path" gorm:"size:500"`
	NarrationVoice        string  `json:"narration_voice" gorm:"size:200"`

	// 字幕配置
	SubtitleEnabled  bool   `json:"subtitle_enabled" gorm:"default:true"`
	SubtitlePosition string `json:"subtitle_position" gorm:"size:20;default:'bottom'"`
	SubtitleFontSize int    `json:"subtitle_font_size" gorm:"default:48"`
	SubtitleColor    string `json:"subtitle_color" gorm:"size:20;default:'#FFFFFF'"`
	SubtitleBgStyle  string `json:"subtitle_bg_style" gorm:"size:20;default:'shadow'"`

	// 色彩调色配置（Color Grading）
	ColorGrade    string  `json:"color_grade" gorm:"size:50;default:'none'"`          // none/cinematic/warm/cool/teal_orange/vintage/noir
	ContrastLevel float64 `json:"contrast_level" gorm:"type:decimal(3,2);default:0"`   // -1.0 to 1.0, 0=no change
	Saturation    float64 `json:"saturation" gorm:"type:decimal(3,2);default:1.0"`     // 0.0=grayscale, 1.0=normal, 2.0=vivid

	// 镜头特效（Lens FX）
	FilmGrain           bool `json:"film_grain" gorm:"default:false"`            // 胶片颗粒
	Vignette            bool `json:"vignette" gorm:"default:false"`              // 镜头暗角
	ChromaticAberration bool `json:"chromatic_aberration" gorm:"default:false"` // 色差效果

	// Kling 专业模式
	KlingProForAction bool `json:"kling_pro_for_action" gorm:"default:true"` // 动作/史诗镜头自动使用 pro 模式

	// 字幕样式
	SubtitleStyle string `json:"subtitle_style" gorm:"size:20;default:'none'"`                  // none/basic/cinematic/anime
	SubtitleFont  string `json:"subtitle_font" gorm:"size:100;default:'Noto Sans CJK SC'"` // 字体名称

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (NovelVideoConfig) TableName() string { return "ink_novel_video_config" }

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

	// 状态
	Status string `json:"status" gorm:"size:20;index:idx_chapter_novel_status,priority:2;default:draft"`
	// draft=草稿, generating=生成中, completed=已完成, published=已发布

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Chapter) TableName() string {
	return "ink_chapter"
}

// PlotPoint 剧情点
type PlotPoint struct {
	ID        uint     `json:"id" gorm:"primaryKey"`
	TenantID  uint     `json:"tenant_id" gorm:"index"`
	NovelID   uint     `json:"novel_id" gorm:"index:idx_plotpoint_novel_resolved,priority:1;not null"`
	ChapterID uint     `json:"chapter_id" gorm:"index;not null"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	Type      string   `json:"type" gorm:"size:50"`
	// conflict=冲突, climax=高潮, resolution=解决, twist=转折, foreshadow=伏笔

	Description string `json:"description" gorm:"type:text"`
	Characters  string `json:"characters" gorm:"type:text"` // JSON数组
	Locations   string `json:"locations" gorm:"type:text"`  // JSON数组

	IsResolved bool  `json:"is_resolved" gorm:"index:idx_plotpoint_novel_resolved,priority:2;default:false"`
	ResolvedIn *uint `json:"resolved_in"` // 解决这一剧情点的章节ID

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpdatePlotPointRequest 更新剧情点请求
type UpdatePlotPointRequest struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Characters  []string `json:"characters"`
	Locations   []string `json:"locations"`
	IsResolved  *bool    `json:"is_resolved"`
	ResolvedIn  *uint    `json:"resolved_in"`
}

func (PlotPoint) TableName() string {
	return "ink_plot_point"
}

// Worldview 世界观
type Worldview struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Genre       string `json:"genre" gorm:"size:50;index"`

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

// Character 角色
type Character struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	NovelID  uint   `json:"novel_id" gorm:"index;not null"`
	TenantID uint   `json:"tenant_id" gorm:"index;not null;default:1"`
	Novel    *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name   string `json:"name" gorm:"size:100;not null"`
	Role   string `json:"role" gorm:"size:50"`
	// protagonist=主角, antagonist=反派, supporting=配角, minor=龙套
	Gender string `json:"gender" gorm:"size:20"` // "male" | "female" | "neutral"

	Archetype string `json:"archetype" gorm:"size:50"` // 角色原型

	// 外貌与性格
	Appearance  string `json:"appearance" gorm:"type:text"`
	Personality string `json:"personality" gorm:"type:text"`
	Background  string `json:"background" gorm:"type:text"`

	// 能力与属性
	Abilities       string `json:"abilities" gorm:"type:text"`        // JSON array [{name,level,description}]
	PersonalityTags string `json:"personality_tags" gorm:"type:text"` // JSON array of tag strings
	Attributes      string `json:"attributes" gorm:"type:text"`       // JSON

	// 角色关系（JSON）
	Relations string `json:"relations" gorm:"type:text"`

	// 角色弧光
	CharacterArc string `json:"character_arc" gorm:"type:text"`

	// 对话风格（AI提取的说话习惯、用词偏好、禁忌表达）
	DialogueStyle string `json:"dialogue_style" gorm:"type:text"` // JSON: {patterns, vocabulary_level, speech_habits, forbidden_phrases}

	// 视觉设计
	VisualDesign string `json:"visual_design" gorm:"type:text"` // JSON: 包含图像URL、表情库等

	// 三视图（正面、侧面、背面参考图）
	ThreeViewFront string `json:"three_view_front" gorm:"size:1000"`
	ThreeViewSide  string `json:"three_view_side" gorm:"size:1000"`
	ThreeViewBack  string `json:"three_view_back" gorm:"size:1000"`

	// 封面 / 头像
	Portrait   string `json:"portrait" gorm:"size:1000"`
	CoverImage string `json:"cover_image" gorm:"size:500"`

	// 配音设置
	VoiceID       string  `json:"voice_id" gorm:"size:100"`                         // 声音ID（如 alloy/echo/nova 等）
	VoiceSpeed    float64 `json:"voice_speed" gorm:"type:decimal(4,2);default:1.0"` // 语速 0.25–4.0
	VoiceStyle    string  `json:"voice_style" gorm:"size:100"`                      // 语音风格（如 calm/excited/sad）
	VoiceLanguage string  `json:"voice_language" gorm:"size:20"`                    // 语言+方言（如 zh / zh-yue / en / ja）
	VoiceSample   string  `json:"voice_sample" gorm:"size:1000"`                    // 试听样本 URL

	// 状态
	Status string `json:"status" gorm:"size:20;default:active"`

	// 时间戳
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Character) TableName() string {
	return "ink_character"
}

// CharacterAppearance 角色出现记录
type CharacterAppearance struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	CharacterID uint       `json:"character_id" gorm:"uniqueIndex:idx_char_app_char_chapter,priority:1;not null"`
	Character   *Character `json:"character,omitempty" gorm:"foreignKey:CharacterID"`
	ChapterID   uint       `json:"chapter_id" gorm:"uniqueIndex:idx_char_app_char_chapter,priority:2;not null"`
	Chapter     *Chapter   `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	RoleInChapter string `json:"role_in_chapter" gorm:"size:50"`
	// main=主要出场, supporting=辅助出场, mentioned=被提及

	Action string `json:"action" gorm:"type:text"` // 本章动作
	Change string `json:"change" gorm:"type:text"` // 本章变化

	CreatedAt time.Time `json:"created_at"`
}

func (CharacterAppearance) TableName() string {
	return "ink_character_appearance"
}

// CharacterStateSnapshot 角色状态快照
type CharacterStateSnapshot struct {
	ID          uint `json:"id" gorm:"primaryKey"`
	CharacterID uint `json:"character_id" gorm:"index;not null"`
	ChapterID   uint `json:"chapter_id" gorm:"index"`

	// 物理状态
	Age      float64 `json:"age"`
	Height   float64 `json:"height"`                    // 单位：米
	Weight   float64 `json:"weight"`                    // 单位：公斤
	Health   string  `json:"health"`                    // healthy, injured, critical
	Injuries string  `json:"injuries" gorm:"type:text"` // JSON: [{part, severity, description}]

	// 能力状态
	PowerLevel int    `json:"power_level"`
	Abilities  string `json:"abilities" gorm:"type:text"` // JSON
	Equipment  string `json:"equipment" gorm:"type:text"` // JSON

	// 心理状态
	Mood       string `json:"mood"`
	Motivation string `json:"motivation"`
	Goals      string `json:"goals" gorm:"type:text"` // JSON
	Fears      string `json:"fears" gorm:"type:text"` // JSON

	// 位置状态
	Location       string `json:"location"`
	KnownLocations string `json:"known_locations" gorm:"type:text"` // JSON

	// 关系状态
	Relations string `json:"relations" gorm:"type:text"` // JSON: [{character_id, attitude, recent_interaction}]

	SnapshotTime time.Time `json:"snapshot_time"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (CharacterStateSnapshot) TableName() string {
	return "ink_character_state_snapshot"
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

// KnowledgeBase 知识库
type KnowledgeBase struct {
	ID   uint   `json:"id" gorm:"primaryKey"`
	Type string `json:"type" gorm:"size:50;index;index:idx_kb_novel_type,priority:2"`
	// character_fact=角色事实, lore=世界观知识, writing_technique=写作技巧

	Title   string `json:"title" gorm:"size:255;not null"`
	Content string `json:"content" gorm:"type:text"`

	Tags string `json:"tags" gorm:"type:text"` // JSON数组

	// 关联
	NovelID     *uint           `json:"novel_id,omitempty" gorm:"index;index:idx_kb_novel_type,priority:1"`
	Novel       *Novel          `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ReferenceID *uint           `json:"reference_id,omitempty" gorm:"index"`
	Reference   *ReferenceNovel `json:"reference,omitempty" gorm:"foreignKey:ReferenceID"`

	// 向量信息
	VectorID   string `json:"vector_id" gorm:"size:100"`
	VectorHash string `json:"vector_hash" gorm:"size:64"`

	// 统计
	UsageCount int `json:"usage_count" gorm:"default:0"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (KnowledgeBase) TableName() string {
	return "ink_knowledge_base"
}

// ModelProvider 模型提供商
type ModelProvider struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	TenantID    uint   `json:"tenant_id" gorm:"index;default:0;comment:0=系统级,>0=租户私有;uniqueIndex:idx_provider_name_tenant"`
	Name        string `json:"name" gorm:"size:50;not null;uniqueIndex:idx_provider_name_tenant"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Type        string `json:"type" gorm:"size:20"`
	// cloud=云端, local=本地

	// API配置
	APIEndpoint  string `json:"api_endpoint" gorm:"size:500"`
	APIKey       string `json:"api_key" gorm:"type:text"`
	APISecretKey string `json:"api_secret_key" gorm:"type:text"` // AK/SK 鉴权的 SecretKey（如火山引擎即梦AI）
	APIVersion   string `json:"api_version" gorm:"size:50"`      // 也用于存储默认模型名称

	// 限制
	RateLimit int     `json:"rate_limit"` // 请求/分钟
	MaxTokens int     `json:"max_tokens"`
	CostPer1K float64 `json:"cost_per_1k_tokens"`
	Timeout   int     `json:"timeout" gorm:"default:0"` // HTTP 请求超时（秒），0=使用默认值300s

	// 元数据（系统级模板字段，由 seedAIModels 写入，用户无需填写）
	NeedsSecretKey bool   `json:"needs_secret_key" gorm:"default:false"` // 是否需要 AK/SK 双密钥鉴权
	StaticModels   string `json:"static_models,omitempty" gorm:"type:text"` // JSON 字符串，不支持 /models 端点时的内置模型列表

	// 状态
	IsActive    bool   `json:"is_active" gorm:"default:true"`
	HealthCheck string `json:"health_check" gorm:"size:20;default:ok"`
	// ok=正常, degraded=降级, down=宕机

	LastChecked *time.Time `json:"last_checked"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ModelProvider) TableName() string {
	return "ink_model_provider"
}

// AIModel AI模型
type AIModel struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	ProviderID uint           `json:"provider_id" gorm:"index;not null"`
	Provider   *ModelProvider `json:"provider,omitempty" gorm:"foreignKey:ProviderID"`

	Name        string `json:"name" gorm:"size:100;not null"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Version     string `json:"version" gorm:"size:50"`
	Type        string `json:"type" gorm:"size:50;default:''"` // e.g. chat/image/voice/embedding

	// 能力
	Capabilities string `json:"capabilities" gorm:"type:text"` // JSON

	// 性能指标
	MaxTokens     int     `json:"max_tokens"`
	ContextWindow int     `json:"context_window"`
	Speed         float64 `json:"speed"`   // tokens/秒
	Quality       float64 `json:"quality"` // 0.0-1.0
	CostPer1K     float64 `json:"cost_per_1k_tokens"`

	// 适用任务
	SuitableTasks string `json:"suitable_tasks" gorm:"type:text"` // JSON数组

	// 默认参数
	DefaultTemperature float64 `json:"default_temperature" gorm:"type:decimal(3,2)"`
	DefaultTopP        float64 `json:"default_top_p" gorm:"type:decimal(3,2)"`
	DefaultTopK        int     `json:"default_top_k"`

	// 状态
	IsActive    bool `json:"is_active" gorm:"default:true"`
	IsAvailable bool `json:"is_available" gorm:"default:true"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (AIModel) TableName() string {
	return "ink_ai_model"
}

// TaskModelConfig 任务模型配置
type TaskModelConfig struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TaskType string `json:"task_type" gorm:"size:50;uniqueIndex;not null"`

	PrimaryModelID   uint     `json:"primary_model_id"`
	PrimaryModel     *AIModel `json:"primary_model,omitempty" gorm:"foreignKey:PrimaryModelID"`
	FallbackModelIDs string   `json:"fallback_model_ids" gorm:"type:text"` // JSON数组

	// 参数
	Temperature    float64 `json:"temperature" gorm:"type:decimal(3,2)"`
	TopP           float64 `json:"top_p" gorm:"type:decimal(3,2)"`
	TopK           int     `json:"top_k"`
	MaxTokens      int     `json:"max_tokens"`
	TimeoutSeconds int     `json:"timeout_seconds" gorm:"default:0"` // 0=使用硬编码默认值(300s)
	SystemPrompt   string  `json:"system_prompt" gorm:"type:text"`

	// 限制
	MaxCostPerTask float64 `json:"max_cost_per_task"`

	// 质量要求
	MinQualityScore float64 `json:"min_quality_score" gorm:"type:decimal(3,2)"`

	// 策略
	Strategy string `json:"strategy" gorm:"size:20;default:balanced"`
	// quality_first=质量优先, cost_first=成本优先, balanced=平衡, custom=自定义

	IsActive bool `json:"is_active" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (TaskModelConfig) TableName() string {
	return "ink_task_model_config"
}

// ModelComparisonExperiment 模型对比实验
type ModelComparisonExperiment struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`

	TaskType  string `json:"task_type" gorm:"size:50;index"`
	InputData string `json:"input_data" gorm:"type:text"` // JSON

	ModelIDs   string `json:"model_ids" gorm:"type:text"`  // JSON数组
	Parameters string `json:"parameters" gorm:"type:text"` // JSON

	// 状态
	Status   string  `json:"status" gorm:"size:20;default:pending"`
	Progress float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`

	// 结果
	Results       string `json:"results" gorm:"type:text"` // JSON
	WinnerModelID *uint  `json:"winner_model_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ModelComparisonExperiment) TableName() string {
	return "ink_model_comparison_experiment"
}

// ExperimentResult 实验结果
type ExperimentResult struct {
	ID           uint                       `json:"id" gorm:"primaryKey"`
	ExperimentID uint                       `json:"experiment_id" gorm:"index;not null"`
	Experiment   *ModelComparisonExperiment `json:"experiment,omitempty" gorm:"foreignKey:ExperimentID"`
	ModelID      uint                       `json:"model_id" gorm:"index;not null"`
	Model        *AIModel                   `json:"model,omitempty" gorm:"foreignKey:ModelID"`

	Output string `json:"output" gorm:"type:text"`

	// 质量评分
	QualityScore     float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	RelevanceScore   float64 `json:"relevance_score" gorm:"type:decimal(5,4)"`
	CreativityScore  float64 `json:"creativity_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`

	// 成本
	TokensUsed int     `json:"tokens_used"`
	Cost       float64 `json:"cost"`
	Latency    float64 `json:"latency"` // 秒

	// 用户评价
	UserRating  *int   `json:"user_rating,omitempty"` // 1-5
	UserComment string `json:"user_comment" gorm:"type:text"`

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (ExperimentResult) TableName() string {
	return "ink_experiment_result"
}

// ModelUsageLog 模型使用记录
// ModelID 仅作为软引用（无外键约束），允许关联的 AIModel 被删除而不级联报错。
type ModelUsageLog struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	ModelID  uint   `json:"model_id" gorm:"index:idx_usage_model_time,priority:1;default:0"` // 软引用，0=未关联
	TaskType string `json:"task_type" gorm:"size:50;index"`

	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	Cost         float64 `json:"cost"`
	Latency      float64 `json:"latency"` // 秒

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	QualityScore *float64 `json:"quality_score,omitempty"`

	CreatedAt time.Time `json:"created_at" gorm:"index:idx_usage_model_time,priority:2"`
}

func (ModelUsageLog) TableName() string {
	return "ink_model_usage_log"
}

// Video 视频
type Video struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;default:0"`
	UUID     string `json:"uuid" gorm:"uniqueIndex;size:36"`
	NovelID  uint   `json:"novel_id" gorm:"index;not null"`
	Novel     *Novel   `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID *uint    `json:"chapter_id,omitempty" gorm:"index"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	Title       string `json:"title" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`

	// 配置
	Type string `json:"type" gorm:"size:50;default:image_sequence"`
	// image_sequence=图片序列, animation=动画, live_action=真人

	Mode string `json:"mode" gorm:"size:20;default:'video'"`
	// video=AI视频生成（Kling/Seedance）, slideshow=图片解说（图片+Ken Burns效果）

	Resolution  string `json:"resolution" gorm:"size:20;default:1080p"`
	FrameRate   int    `json:"frame_rate" gorm:"default:24"`
	AspectRatio string `json:"aspect_ratio" gorm:"size:10;default:16:9"`
	ArtStyle       string `json:"art_style" gorm:"size:50"`
	Pacing         string `json:"pacing" gorm:"size:20;default:'normal'"`   // slow/normal/fast
	TargetDuration int    `json:"target_duration" gorm:"default:0"`          // 目标时长（秒），0=自动估算

	// 统计
	Duration    float64 `json:"duration"` // 秒
	TotalShots  int     `json:"total_shots" gorm:"default:0"`
	TotalFrames int     `json:"total_frames" gorm:"default:0"`
	TotalWords  int     `json:"total_words" gorm:"default:0"`

	// 文件
	VideoPath string `json:"video_path" gorm:"size:500"`
	Thumbnail string `json:"thumbnail" gorm:"size:500"`

	// 状态
	Status       string `json:"status" gorm:"size:20;default:planning"`
	ScriptStatus string `json:"script_status" gorm:"size:20;default:draft"`
	// draft=脚本草稿（可编辑），confirmed=脚本已确认（可生成素材）
	Progress float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`

	// 质量档位
	QualityTier string `json:"quality_tier" gorm:"size:20;default:preview"`
	// draft=草稿(静图+Pan), preview=预览(720p短片), final=正式(1080p+)

	// 成本
	GenerationCost float64 `json:"generation_cost" gorm:"type:decimal(10,2)"`

	// 异步任务追踪
	ProviderName string `json:"provider_name" gorm:"size:50"`             // kling/seedance
	TaskID       string `json:"task_id" gorm:"size:255;index"`            // 外部 API 任务 ID
	ErrorMessage string `json:"error_message,omitempty" gorm:"type:text"` // 生成失败原因
	RetryCount   int    `json:"retry_count" gorm:"default:0"`             // 已重试次数

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Video) TableName() string {
	return "ink_video"
}

// StoryboardShot 分镜
type StoryboardShot struct {
	ID        uint     `json:"id" gorm:"primaryKey"`
	VideoID   uint     `json:"video_id" gorm:"index;index:idx_shot_video_status,priority:1;index:idx_shot_video_no,priority:1;not null"`
	Video     *Video   `json:"video,omitempty" gorm:"foreignKey:VideoID"`
	UUID      string   `json:"uuid" gorm:"uniqueIndex;size:36"`
	ShotNo    int      `json:"shot_no" gorm:"not null;index:idx_shot_video_no,priority:2"`
	ChapterID *uint    `json:"chapter_id,omitempty" gorm:"index"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	Description string `json:"description" gorm:"type:text"` // 英文画面描述，供AI图片/视频生成使用
	Narration   string `json:"narration" gorm:"type:text"`   // 中文旁白文案，供TTS朗读和字幕显示使用
	Dialogue    string `json:"dialogue" gorm:"type:text"`    // 角色台词（格式："角色名：台词"），有对话时填写
	Subtitle    string `json:"subtitle" gorm:"type:text"`    // 字幕覆盖文本，非空时优先用于SRT/VTT导出，不影响TTS朗读

	// 摄像机配置
	CameraType string `json:"camera_type" gorm:"size:50;default:static"`
	// static=静态, pan=平移, zoom=缩放, tracking=跟踪, dolly=移动

	CameraAngle string `json:"camera_angle" gorm:"size:50;default:eye_level"`
	// eye_level=平视, low=俯, high=仰, dutch=荷兰角

	ShotSize string `json:"shot_size" gorm:"size:50;default:medium"`
	// wide=远景, medium=中景, close_up=近景, extreme_close_up=特写

	Duration      float64 `json:"duration" gorm:"type:decimal(5,2);default:5.0"`
	EmotionalTone string  `json:"emotional_tone" gorm:"size:100"` // 情绪基调，如：紧张、浪漫、压抑→释怀
	Transition    string  `json:"transition" gorm:"size:50;default:cut"` // 过渡方式：cut/j-cut/l-cut/fade/dissolve/dip-black/dip-white/wipe/push/slide/zoom/whip-pan/spin/flash/glitch/blur/morph

	// 角色和场景（JSON）
	Characters string `json:"characters" gorm:"type:text"`
	Scene      string `json:"scene" gorm:"type:text"`

	// AI生成
	Prompt         string `json:"prompt" gorm:"type:text"`
	NegativePrompt string `json:"negative_prompt" gorm:"type:text"`
	MotionPrompt   string `json:"motion_prompt" gorm:"type:text"` // 视频运镜描述（供Kling/Seedance使用）
	Frames         string `json:"frames" gorm:"type:text"` // JSON数组

	// 状态
	Status       string `json:"status" gorm:"size:20;index:idx_shot_video_status,priority:2;default:pending"`
	Progress     float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`
	ErrorMessage string  `json:"error_message,omitempty" gorm:"type:text"` // 失败原因（供前端展示）

	// 文件
	ClipPath string `json:"clip_path" gorm:"size:2000"`

	// per-shot 视频生成
	ShotTaskID       string  `json:"shot_task_id" gorm:"size:255;index"`
	ShotProviderName string  `json:"shot_provider_name" gorm:"size:50"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(4,3)"`
	AudioPath        string  `json:"audio_path" gorm:"size:2000"`
	RetryCount       int     `json:"retry_count" gorm:"default:0"`

	// 生成模式
	GenerationMode string `json:"generation_mode" gorm:"size:20;default:static"`
	// static=静图+Ken Burns效果, video=AI视频生成

	// AI 生成结果 URL（前端展示用）
	ImageURL string `json:"image_url" gorm:"size:1000"` // AI生成图片URL
	VideoURL string `json:"video_url" gorm:"size:1000"` // AI生成视频URL

	// 时序连贯与参考帧
	ReferenceImageURL string `json:"reference_image_url" gorm:"size:500"` // 前一镜头最后一帧URL，用于时序连贯
	FrameImageURL     string `json:"frame_image_url" gorm:"size:500"`     // 本镜头AI图像生成结果URL，传给Kling image-to-video

	// 场景锚点
	SceneAnchorID *uint `json:"scene_anchor_id,omitempty" gorm:"index"`

	// 角色绑定（序列化为 JSON 数组，前端直接收到 [1,2,3]）
	CharacterIDs JSONUintSlice `json:"character_ids" gorm:"type:json"`

	// 音效（SFX）
	SFXURL    string  `json:"sfx_url" gorm:"size:1000"`    // 音效文件URL（本地/OSS/Freesound预览）
	SFXTags   string  `json:"sfx_tags" gorm:"size:2000"`   // LLM提取的音效标签（JSON对象或数组字符串）
	SFXVolume float64 `json:"sfx_volume" gorm:"type:decimal(4,2);default:0"` // 混音音量（0=自动，>0=覆盖）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (StoryboardShot) TableName() string {
	return "ink_storyboard_shot"
}

// ShotVoiceSegment 分镜语音段落（一个分镜可包含多条语音/字幕段落）
type ShotVoiceSegment struct {
	ID     uint `json:"id" gorm:"primaryKey"`
	ShotID uint `json:"shot_id" gorm:"not null;index:idx_seg_shot_seq,priority:1"`
	SeqNo  int  `json:"seq_no" gorm:"not null;default:1;index:idx_seg_shot_seq,priority:2"`

	Text     string `json:"text" gorm:"type:text"`    // TTS 朗读文本（旁白或台词内容）
	Speaker  string `json:"speaker" gorm:"size:100"`  // 空串=旁白，"角色名"=对白
	VoiceID  string `json:"voice_id" gorm:"size:100"` // TTS 声音 ID（覆盖默认值）

	AudioPath    string  `json:"audio_path" gorm:"size:1000"`                   // 生成的音频文件路径/URL
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3);default:0"` // 音频时长（秒）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ShotVoiceSegment) TableName() string { return "ink_shot_voice_segment" }

// ShotSFXItem 分镜音效条目（一个分镜可包含多条独立音效）
type ShotSFXItem struct {
	ID     uint `json:"id" gorm:"primaryKey"`
	ShotID uint `json:"shot_id" gorm:"not null;index:idx_sfx_shot_seq,priority:1"`
	SeqNo  int  `json:"seq_no" gorm:"not null;default:1;index:idx_sfx_shot_seq,priority:2"`

	Tag          string  `json:"tag" gorm:"size:200"`                          // 音效搜索词，如 "heavy rain bamboo forest"
	URL          string  `json:"url" gorm:"size:1000"`                         // 音效文件 URL
	Volume       float64 `json:"volume" gorm:"type:decimal(4,2);default:0.4"` // 混音音量（0.1–1.0）
	Source       string  `json:"source" gorm:"size:20"`                        // local/freesound/elevenlabs
	Disabled     bool    `json:"disabled" gorm:"default:false"`                // 禁用后不参与合成/预览
	StartOffset  float64 `json:"start_offset" gorm:"type:decimal(8,3);default:0"`  // 在分镜中的开始时间（秒，0=分镜起始）
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3);default:0"` // 音效时长（秒，0=未知）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ShotSFXItem) TableName() string { return "ink_shot_sfx_item" }

// VideoBGMSegment 视频背景音乐分段（一段BGM可跨越多个分镜，AI智能分组）
type VideoBGMSegment struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	VideoID uint `json:"video_id" gorm:"index;index:idx_bgm_video_seq,priority:1;not null"`
	SeqNo   int  `json:"seq_no" gorm:"index:idx_bgm_video_seq,priority:2;not null;default:1"` // 分段顺序
	StartShotNo int     `json:"start_shot_no" gorm:"not null;default:1"`
	EndShotNo   int     `json:"end_shot_no" gorm:"not null;default:1"`
	Mood        string  `json:"mood" gorm:"size:200"`         // 情绪/氛围描述，如 "史诗战斗""温情别离"
	Tempo       string  `json:"tempo" gorm:"size:20"`         // fast/medium/slow
	SearchQueries string `json:"search_queries" gorm:"type:text"` // JSON 字符串，自然语言搜索词列表
	URL         string  `json:"url" gorm:"size:1000"`          // 匹配到的 BGM 音频 URL
	Volume      float64 `json:"volume" gorm:"type:decimal(4,2);default:0.3"`
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3)"`
	TrackName   string  `json:"track_name" gorm:"size:255"`
	TrackArtist string  `json:"track_artist" gorm:"size:255"`
	Source      string  `json:"source" gorm:"size:20"`   // jamendo/pixabay/local/none
	Disabled    bool    `json:"disabled" gorm:"default:false"` // 禁用后不参与合成/预览

	// 人声闪避（Audio Ducking）
	DuckingEnabled bool    `json:"ducking_enabled" gorm:"default:true"`                    // true=检测到人声时自动压低BGM
	DuckingLevel   float64 `json:"ducking_level" gorm:"type:decimal(3,2);default:0.15"`   // 闪避后的目标音量（0.1-0.5，默认0.15）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (VideoBGMSegment) TableName() string { return "ink_video_bgm_segment" }

// QualityReport 质量报告
type QualityReport struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
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

// Item 物品（项目级别，贯穿整部小说）
type Item struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	NovelID uint   `json:"novel_id" gorm:"index;not null"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name     string `json:"name" gorm:"size:100;not null"`
	Category string `json:"category" gorm:"size:50"` // weapon/treasure/tool/document/artifact/other

	Description  string `json:"description" gorm:"type:text"`
	Appearance   string `json:"appearance" gorm:"type:text"`   // 外观描述
	Location     string `json:"location" gorm:"size:200"`      // 当前/最后已知位置
	Owner        string `json:"owner" gorm:"size:100"`         // 当前持有者
	Significance string `json:"significance" gorm:"type:text"` // 在故事中的重要性
	Abilities    string `json:"abilities" gorm:"type:text"`    // JSON: [{name, description}]

	ImageURL          string `json:"image_url" gorm:"size:1000"`
	VisualPrompt      string `json:"visual_prompt" gorm:"type:text"`       // 用于 AI 图像生成的英文提示词
	ReferenceImageURL string `json:"reference_image_url" gorm:"size:1000"` // 参考图 URL（已上传到 OSS）

	Status string `json:"status" gorm:"size:20;default:active"` // active/lost/destroyed/unknown

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Item) TableName() string { return "ink_item" }

// ChapterItem 章节级别的物品状态（覆盖项目级别）
type ChapterItem struct {
	ID        uint `json:"id" gorm:"primaryKey"`
	ItemID    uint `json:"item_id" gorm:"uniqueIndex:uniq_chapter_item;not null"`
	ChapterID uint `json:"chapter_id" gorm:"uniqueIndex:uniq_chapter_item;not null"`
	NovelID   uint `json:"novel_id" gorm:"index;not null"`

	Location  string `json:"location" gorm:"size:200"` // 本章节中物品所在位置（覆盖项目级）
	Owner     string `json:"owner" gorm:"size:100"`    // 本章节中持有者（覆盖项目级）
	Condition string `json:"condition" gorm:"size:50"` // intact/damaged/broken/destroyed
	Notes     string `json:"notes" gorm:"type:text"`   // 本章节备注

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ChapterItem) TableName() string { return "ink_chapter_item" }

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
	StylePrompt string   `json:"style_prompt"`
	ImageStyle  string   `json:"image_style"`
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

type GenerateChapterRequest struct {
	NovelID        uint    `json:"novel_id"`
	ChapterNo      int     `json:"chapter_no" binding:"required,min=1"`
	Prompt         string  `json:"prompt"`
	MaxTokens      int     `json:"max_tokens"`
	Temperature    float64 `json:"temperature,omitempty"`    // 0=使用项目配置或系统默认
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"` // 0=使用项目配置或系统默认
	ModelOverride  string  `json:"model,omitempty"` // 可选：指定使用的 AI 模型/provider
	IsStandalone   bool    `json:"is_standalone"`   // true=最终章，要求故事完整收尾；可显式传入，也会由系统根据 chapter_no >= target_chapters 自动推断
}

type CreateCharacterRequest struct {
	Name        string `json:"name" binding:"required"`
	Gender      string `json:"gender"`   // "male" | "female" | "neutral"
	Role        string `json:"role"`
	Archetype   string `json:"archetype"`
	Background  string `json:"background"`
	Appearance  string `json:"appearance"`
	Personality string `json:"personality"`
}

type UpdateCharacterRequest struct {
	Name         string `json:"name"`
	Gender       string `json:"gender"`    // "male" | "female" | "neutral"
	Role         string `json:"role"`
	Archetype    string `json:"archetype"`
	Background   string `json:"background"`
	Appearance   string `json:"appearance"`
	Personality  string `json:"personality"`
	CharacterArc string `json:"character_arc"`
	// nil = field absent (don't update); non-nil empty = clear; non-empty = update
	PersonalityTags []string      `json:"personality_tags"`
	Abilities       []interface{} `json:"abilities"` // [{name,level,description}]
	ThreeViewFront  string        `json:"three_view_front"`
	ThreeViewSide   string        `json:"three_view_side"`
	ThreeViewBack   string        `json:"three_view_back"`
	Portrait        string        `json:"portrait"`
	CoverImage      string        `json:"cover_image"`
	// 配音设置
	VoiceID       string   `json:"voice_id"`
	VoiceSpeed    *float64 `json:"voice_speed"`    // nil = absent (don't update)
	VoiceStyle    string   `json:"voice_style"`
	VoiceLanguage string   `json:"voice_language"` // 语言+方言（如 zh / zh-yue / en / ja）
	VoiceSample   string   `json:"voice_sample"`   // 试听样本存储路径（file:// 或 URL）
}

type GenerateImageRequest struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Emotion     string `json:"emotion"`
	Action      string `json:"action"`
	Style       string `json:"style"`
}

type CreateVideoRequest struct {
	Title       string `json:"title"`
	Resolution  string `json:"resolution"`
	FrameRate   int    `json:"frame_rate"`
	AspectRatio string `json:"aspect_ratio"`
	ArtStyle    string `json:"art_style"`
	QualityTier string `json:"quality_tier"` // draft/preview/final
	ChapterID   *uint  `json:"chapter_id"`
	Mode        string `json:"mode"` // video/slideshow
}

type UpdateVideoRequest struct {
	Title        string `json:"title"`
	Resolution   string `json:"resolution"`
	FrameRate    int    `json:"frame_rate"`
	AspectRatio  string `json:"aspect_ratio"`
	ArtStyle     string `json:"art_style"`
	ScriptStatus string `json:"script_status"` // draft/confirmed
	Mode         string `json:"mode"`           // video/slideshow
}

type EnhancementConfig struct {
	Type      string  `json:"type"`
	Enabled   bool    `json:"enabled"`
	Intensity float64 `json:"intensity,omitempty"`
}

type CreateModelProviderRequest struct {
	Name         string `json:"name" binding:"required"`
	DisplayName  string `json:"display_name"`
	Type         string `json:"type" binding:"required"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     bool   `json:"is_active"`
	Timeout      int    `json:"timeout"` // HTTP 超时秒数，0=默认300s
}

type UpdateModelProviderRequest struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Type         string `json:"type"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     *bool  `json:"is_active"`
	Timeout      *int   `json:"timeout"` // HTTP 超时秒数，0=默认300s；nil=不修改
}

type CreateAIModelRequest struct {
	ProviderID uint    `json:"provider_id" binding:"required"`
	ModelID    string  `json:"model_id" binding:"required"`
	Name       string  `json:"name" binding:"required"`
	TaskTypes  string  `json:"task_types"`
	MaxTokens  int     `json:"max_tokens"`
	CostPer1K  float64 `json:"cost_per_1k"`
	IsDefault  bool    `json:"is_default"`
}

type UpdateAIModelRequest struct {
	Name      string  `json:"name"`
	TaskTypes string  `json:"task_types"`
	MaxTokens int     `json:"max_tokens"`
	CostPer1K float64 `json:"cost_per_1k"`
	IsDefault *bool   `json:"is_default"`
}

type UpdateTaskConfigRequest struct {
	PrimaryModelID   uint    `json:"primary_model_id"`
	FallbackModelIDs string  `json:"fallback_model_ids"`
	MaxTokens        int     `json:"max_tokens"`
	Temperature      float64 `json:"temperature"`
	TopP             float64 `json:"top_p"`
}

type CreateModelComparisonRequest struct {
	Name       string `json:"name" binding:"required"`
	TaskType   string `json:"task_type" binding:"required"`
	ModelIDs   []uint `json:"model_ids"`
	TestPrompt string `json:"test_prompt"`
	Iterations int    `json:"iterations"`
}

// ─── MCP Tools ────────────────────────────────────────────────────────────────

// McpTool MCP 工具配置
type McpTool struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	TenantID      uint   `json:"tenant_id" gorm:"index;not null;default:1"`
	Name          string `json:"name" gorm:"size:100;uniqueIndex"`
	DisplayName   string `json:"display_name" gorm:"size:100"`
	Description   string `json:"description" gorm:"type:text"`
	TransportType string `json:"transport_type" gorm:"size:20"` // http, sse, stdio
	Endpoint      string `json:"endpoint" gorm:"size:500"`
	Headers       string `json:"headers" gorm:"type:text"` // JSON map[string]string
	Env           string `json:"env" gorm:"type:text"`     // JSON map[string]string (stdio only)
	Timeout       int    `json:"timeout" gorm:"default:30"`
	IsActive      bool   `json:"is_active" gorm:"default:true"`
	IsSystem      bool   `json:"is_system" gorm:"default:false"` // 系统内置工具不可删除
	Schema        string `json:"schema" gorm:"type:text"`        // JSON 工具能力描述

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (McpTool) TableName() string {
	return "ink_mcp_tool"
}

// ModelMcpBinding 模型 <-> MCP 工具绑定关系
type ModelMcpBinding struct {
	ID        uint `json:"id" gorm:"primaryKey"`
	ModelID   uint `json:"model_id" gorm:"index;uniqueIndex:idx_model_mcp,priority:1;not null"`
	McpToolID uint `json:"tool_id" gorm:"index;uniqueIndex:idx_model_mcp,priority:2;not null"`
	Enabled   bool `json:"enabled" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
}

func (ModelMcpBinding) TableName() string {
	return "ink_model_mcp_binding"
}

// ─── MCP Request/Response DTOs ────────────────────────────────────────────────

type CreateMcpToolRequest struct {
	Name          string            `json:"name" binding:"required"`
	DisplayName   string            `json:"display_name"`
	Description   string            `json:"description"`
	TransportType string            `json:"transport_type" binding:"required"` // http/sse/stdio
	Endpoint      string            `json:"endpoint" binding:"required"`
	Headers       map[string]string `json:"headers"`
	Env           map[string]string `json:"env"`
	Timeout       int               `json:"timeout"`
	IsActive      bool              `json:"is_active"`
}

type UpdateMcpToolRequest struct {
	DisplayName   string            `json:"display_name"`
	Description   string            `json:"description"`
	TransportType string            `json:"transport_type"`
	Endpoint      string            `json:"endpoint"`
	Headers       map[string]string `json:"headers"`
	Env           map[string]string `json:"env"`
	Timeout       int               `json:"timeout"`
	IsActive      *bool             `json:"is_active"`
}

// ─── Item DTOs ─────────────────────────────────────────────────────────────────

type CreateItemRequest struct {
	Name         string `json:"name" binding:"required"`
	Category     string `json:"category"`
	Description  string `json:"description"`
	Appearance   string `json:"appearance"`
	Location     string `json:"location"`
	Owner        string `json:"owner"`
	Significance string `json:"significance"`
	Abilities    string `json:"abilities"`
	VisualPrompt string `json:"visual_prompt"`
	Status       string `json:"status"`
}

type UpdateItemRequest struct {
	Name              string `json:"name"`
	Category          string `json:"category"`
	Description       string `json:"description"`
	Appearance        string `json:"appearance"`
	Location          string `json:"location"`
	Owner             string `json:"owner"`
	Significance      string `json:"significance"`
	Abilities         string `json:"abilities"`
	VisualPrompt      string `json:"visual_prompt"`
	ImageURL          string `json:"image_url"`
	ReferenceImageURL string `json:"reference_image_url"`
	Status            string `json:"status"`
}

type UpsertChapterItemRequest struct {
	Location  string `json:"location"`
	Owner     string `json:"owner"`
	Condition string `json:"condition"`
	Notes     string `json:"notes"`
}

// ChapterCharacter 章节级角色状态覆盖
type ChapterCharacter struct {
	ID          uint `json:"id" gorm:"primaryKey"`
	CharacterID uint `json:"character_id" gorm:"uniqueIndex:uniq_chapter_char;not null"`
	ChapterID   uint `json:"chapter_id" gorm:"uniqueIndex:uniq_chapter_char;not null"`
	NovelID     uint `json:"novel_id" gorm:"index;not null"`

	Appearance  string `json:"appearance" gorm:"type:text"`  // 本章外观（覆盖项目级）
	Personality string `json:"personality" gorm:"type:text"` // 本章性格变化
	Status      string `json:"status" gorm:"size:50"`        // alive/dead/missing/injured/imprisoned
	Location    string `json:"location" gorm:"size:200"`     // 本章所在位置
	Notes       string `json:"notes" gorm:"type:text"`       // 本章备注

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ChapterCharacter) TableName() string { return "ink_chapter_character" }

type UpsertChapterCharacterRequest struct {
	Appearance  string `json:"appearance"`
	Personality string `json:"personality"`
	Status      string `json:"status"`
	Location    string `json:"location"`
	Notes       string `json:"notes"`
}

// ─── Skill 技能 ─────────────────────────────────────────────────────────────────

// Skill 技能（归属于角色，或作为世界级别的公共技能）
// category: 武技/法术/身法/心法/阵法/神通/秘法/特性
// skill_type: active(主动)/passive(被动)/toggle(切换)/ultimate(绝技)
// status: active/sealed(封印)/lost(失传)/disabled(禁用)
type Skill struct {
	ID          uint  `json:"id" gorm:"primaryKey"`
	NovelID     uint  `json:"novel_id" gorm:"index;index:idx_skill_novel_char,priority:1;not null"`
	CharacterID *uint `json:"character_id" gorm:"index;index:idx_skill_novel_char,priority:2"` // nil = 世界/未分配技能
	ParentID    *uint `json:"parent_id" gorm:"index"`    // 前置技能（技能树）

	Name      string `json:"name" gorm:"size:100;not null"`
	Category  string `json:"category" gorm:"size:50"`   // 武技/法术/身法/心法/阵法/神通/秘法/特性
	SkillType string `json:"skill_type" gorm:"size:30"` // active/passive/toggle/ultimate
	Level     int    `json:"level" gorm:"default:1"`
	MaxLevel  int    `json:"max_level" gorm:"default:10"`
	Realm     string `json:"realm" gorm:"size:50"` // 修炼境界要求：练气/筑基/金丹/元婴…

	Description string `json:"description" gorm:"type:text"` // 技能概述
	Effect      string `json:"effect" gorm:"type:text"`      // 效果详情
	FlavorText  string `json:"flavor_text" gorm:"type:text"` // 世界观文字（小说内描述）

	Cost     string `json:"cost" gorm:"size:100"`    // 消耗（法力/灵力/体力等）
	Cooldown string `json:"cooldown" gorm:"size:50"` // 冷却时间
	Tags     string `json:"tags" gorm:"size:200"`    // 逗号分隔标签

	AcquiredChapterNo *int   `json:"acquired_chapter_no"`            // 获得技能的章节号
	AcquiredDesc      string `json:"acquired_desc" gorm:"type:text"` // 获得方式描述

	Status string `json:"status" gorm:"size:20;default:active"` // active/sealed/lost/disabled
	Notes  string `json:"notes" gorm:"type:text"`               // 作者内部备注

	EffectImageURL     string `json:"effect_image_url" gorm:"size:1000"`     // AI 生成的技能特效图片
	EffectVisualPrompt string `json:"effect_visual_prompt" gorm:"type:text"` // 特效图片生成提示词

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Skill) TableName() string { return "ink_skill" }

// ─── Skill DTOs ────────────────────────────────────────────────────────────────

type CreateSkillRequest struct {
	CharacterID       *uint  `json:"character_id"`
	ParentID          *uint  `json:"parent_id"`
	Name              string `json:"name" binding:"required"`
	Category          string `json:"category"`
	SkillType         string `json:"skill_type"`
	Level             int    `json:"level"`
	MaxLevel          int    `json:"max_level"`
	Realm             string `json:"realm"`
	Description       string `json:"description"`
	Effect            string `json:"effect"`
	FlavorText        string `json:"flavor_text"`
	Cost              string `json:"cost"`
	Cooldown          string `json:"cooldown"`
	Tags              string `json:"tags"`
	AcquiredChapterNo *int   `json:"acquired_chapter_no"`
	AcquiredDesc      string `json:"acquired_desc"`
	Notes             string `json:"notes"`
}

type UpdateSkillRequest struct {
	CharacterID        *uint  `json:"character_id"`
	ParentID           *uint  `json:"parent_id"`
	Name               string `json:"name"`
	Category           string `json:"category"`
	SkillType          string `json:"skill_type"`
	Level              int    `json:"level"`
	MaxLevel           int    `json:"max_level"`
	Realm              string `json:"realm"`
	Description        string `json:"description"`
	Effect             string `json:"effect"`
	FlavorText         string `json:"flavor_text"`
	Cost               string `json:"cost"`
	Cooldown           string `json:"cooldown"`
	Tags               string `json:"tags"`
	AcquiredChapterNo  *int   `json:"acquired_chapter_no"`
	AcquiredDesc       string `json:"acquired_desc"`
	Status             string `json:"status"`
	Notes              string `json:"notes"`
	EffectVisualPrompt string `json:"effect_visual_prompt"`
}

type GenerateSkillsRequest struct {
	CharacterID *uint  `json:"character_id"`
	Count       int    `json:"count"` // 生成数量，默认3，最大10
	Hints       string `json:"hints"` // 额外生成提示
}

// ─── Per-shot generation DTOs ─────────────────────────────────────────────────

type BatchGenerateShotsRequest struct {
	ShotIDs     []uint `json:"shot_ids" binding:"required"`
	QualityTier string `json:"quality_tier"` // override; empty = use video's quality_tier
	Provider    string `json:"provider"`     // video provider override (e.g. "kling", "seedance")
}

// ─── 分镜脚本 AI 审查 ──────────────────────────────────────────────────────────

// ShotReviewFeedback 单个镜头的审查反馈
type ShotReviewFeedback struct {
	ShotNo     int      `json:"shot_no"`
	Issues     []string `json:"issues"`
	Suggestion string   `json:"suggestion"`
	Severity   string   `json:"severity"` // info / warning / error
}

// StoryboardReview AI 分镜脚本审查报告
type StoryboardReview struct {
	OverallScore      float64              `json:"overall_score"`      // 综合得分 0-10
	NarrativeScore    float64              `json:"narrative_score"`    // 叙事连贯性
	VisualScore       float64              `json:"visual_score"`       // 视觉多样性
	PacingScore       float64              `json:"pacing_score"`       // 节奏控制
	NarrationScore    float64              `json:"narration_score"`    // 旁白质量
	Summary           string               `json:"summary"`            // 综合评价
	Strengths         []string             `json:"strengths"`          // 亮点
	Weaknesses        []string             `json:"weaknesses"`         // 主要问题
	GlobalSuggestions []string             `json:"global_suggestions"` // 整体改进建议
	ShotFeedback      []ShotReviewFeedback `json:"shot_feedback"`      // 逐镜反馈（仅有问题的镜头）
}

// ─── 戏剧张力管理模型 ──────────────────────────────────────────────────────────

// HookChain 钩子链（章末悬念/情感/谜题/威胁/承诺）
type HookChain struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`
	NovelID  uint `json:"novel_id" gorm:"index;index:idx_hook_novel_fulfilled,priority:1;not null"`

	Type        string `json:"type" gorm:"size:50;not null"`
	// chapter_end/emotional/mystery/threat/promise
	Description     string `json:"description" gorm:"type:text;not null"`
	PlantedAt       int    `json:"planted_at" gorm:"not null"`                // 埋下章节号
	PlannedPayoffAt int    `json:"planned_payoff_at" gorm:"default:0"`        // 计划兑现章节号（0=未规划）
	ActualPayoffAt  int    `json:"actual_payoff_at" gorm:"default:0"`         // 实际兑现章节号
	Intensity       int    `json:"intensity" gorm:"not null;default:5"`       // 1-10
	IsFulfilled     bool   `json:"is_fulfilled" gorm:"index:idx_hook_novel_fulfilled,priority:2;default:false"`
	Notes           string `json:"notes" gorm:"type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (HookChain) TableName() string { return "ink_hook_chain" }

// SatisfactionPoint 爽点（打脸/突破/揭秘/重逢/复仇/认可/其他）
type SatisfactionPoint struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`
	NovelID  uint `json:"novel_id" gorm:"index;not null"`

	ChapterID      *uint  `json:"chapter_id" gorm:"index"` // 实际发生章节（nil=仅计划）
	PlannedChapter int    `json:"planned_chapter" gorm:"default:0"` // 计划发生章节号
	Type           string `json:"type" gorm:"size:50;not null"`
	// face_slap/breakthrough/reveal/reunion/revenge/recognition/other
	Description     string `json:"description" gorm:"type:text;not null"`
	BuildupStart    int    `json:"buildup_start" gorm:"default:0"`        // 铺垫从第几章开始
	IntensityTarget int    `json:"intensity_target" gorm:"default:7"`     // 1-10
	IsPlanned       bool   `json:"is_planned" gorm:"default:true"`        // false=已发生
	Notes           string `json:"notes" gorm:"type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (SatisfactionPoint) TableName() string { return "ink_satisfaction_point" }

// ConflictArc 冲突弧（内部/人际/社会）
type ConflictArc struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`
	NovelID  uint `json:"novel_id" gorm:"index;not null"`

	Title        string `json:"title" gorm:"size:255;not null"`
	Type         string `json:"type" gorm:"size:50;not null"`
	// internal/interpersonal/social
	Description  string `json:"description" gorm:"type:text"`
	Antagonist   string `json:"antagonist" gorm:"size:255"`
	StartChapter int    `json:"start_chapter" gorm:"default:0"`
	PeakChapter  int    `json:"peak_chapter" gorm:"default:0"`  // 预计高潮章节
	EndChapter   int    `json:"end_chapter" gorm:"default:0"`   // 预计解决章节（0=未规划）
	CurrentPhase string `json:"current_phase" gorm:"size:30;default:setup"`
	// setup/escalation/climax/resolution
	IsResolved bool   `json:"is_resolved" gorm:"default:false"`
	Notes      string `json:"notes" gorm:"type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ConflictArc) TableName() string { return "ink_conflict_arc" }

// SceneAnchor 场景锚点（固定命名场景的视觉描述，确保分镜跨镜头布景一致）
type SceneAnchor struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:1"`
	NovelID  uint `json:"novel_id" gorm:"uniqueIndex:idx_scene_anchor_novel_name;not null"`

	Name        string `json:"name" gorm:"size:255;not null;uniqueIndex:idx_scene_anchor_novel_name"`
	Type        string `json:"type" gorm:"size:50"` // interior/exterior/imaginary
	Description string `json:"description" gorm:"type:text"`
	PromptLock  string `json:"prompt_lock" gorm:"type:text"`   // 锁定关键词（逗号分隔）
	StyleTokens string `json:"style_tokens" gorm:"size:500"`   // 风格标签
	RefImageURL string `json:"ref_image_url" gorm:"size:1000"` // 首次生成后存参考图URL
	Notes       string `json:"notes" gorm:"type:text"`

	// 扩展字段（一致性评分相关）
	RefImageLockedAt *time.Time `json:"ref_image_locked_at,omitempty" gorm:"index"`
	RefImageShotID   *uint      `json:"ref_image_shot_id,omitempty"`
	UsageCount       int        `json:"usage_count" gorm:"default:0"`
	AvgConsScore     float64    `json:"avg_cons_score" gorm:"type:decimal(4,3);default:0"`
	ParentAnchorID   *uint      `json:"parent_anchor_id,omitempty" gorm:"index"`
	Variant          string     `json:"variant" gorm:"size:50"` // day/night/winter/battle

	// 光照锁定（确保同一场景跨镜头光照一致）
	LightingKeywords string `json:"lighting_keywords" gorm:"size:500"` // 逗号分隔关键词，如 "golden hour, warm backlight, soft shadows"
	TimeOfDay        string `json:"time_of_day" gorm:"size:50"`        // morning/afternoon/evening/night
	Weather          string `json:"weather" gorm:"size:50"`            // clear/cloudy/rainy/foggy/snowy

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (SceneAnchor) TableName() string { return "ink_scene_anchor" }

// SceneConsistencyLog 场景一致性评分日志
type SceneConsistencyLog struct {
	ID           uint    `gorm:"primaryKey" json:"id"`
	ShotID       uint    `gorm:"index;not null" json:"shot_id"`
	AnchorID     uint    `gorm:"index;not null" json:"anchor_id"`
	Attempt      int     `json:"attempt"`
	OverallScore float64 `gorm:"type:decimal(4,3)" json:"overall_score"`
	ArchScore    float64 `gorm:"type:decimal(4,3)" json:"arch_score"`
	LightScore   float64 `gorm:"type:decimal(4,3)" json:"light_score"`
	AtmoScore    float64 `gorm:"type:decimal(4,3)" json:"atmo_score"`
	PropScore    float64 `gorm:"type:decimal(4,3)" json:"prop_score"`
	Issues       string  `gorm:"type:json" json:"issues"`
	IPWeight     float64 `json:"ip_weight"`
	Passed       bool    `json:"passed"`
	CreatedAt    time.Time `json:"created_at"`
}

func (SceneConsistencyLog) TableName() string { return "ink_scene_consistency_log" }

// SystemSetting 系统全局配置（key-value）
type SystemSetting struct {
	Key         string    `gorm:"primaryKey;type:varchar(64)" json:"key"`
	Value       string    `gorm:"type:text"                   json:"value"`
	Description string    `gorm:"type:varchar(255)"           json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (SystemSetting) TableName() string { return "ink_system_setting" }

// RewriteProject represents a novel rewriting project
type RewriteProject struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	TenantID      uint      `json:"tenant_id" gorm:"index"`
	NovelID       uint      `json:"novel_id" gorm:"index"`
	Name          string    `json:"name" gorm:"size:200"`
	Level         int       `json:"level" gorm:"default:1"` // 1=Literary Refinement, 2=Structural Reconstruction, 3=Spirit Distillation
	Status        string    `json:"status" gorm:"size:20;default:'pending'"` // pending, analyzing, bible_ready, rewriting, reviewing, completed, failed
	Progress      int       `json:"progress" gorm:"default:0"` // 0-100
	TotalChapters int       `json:"total_chapters" gorm:"default:0"`
	DoneChapters  int       `json:"done_chapters" gorm:"default:0"`
	ErrorMsg      string    `json:"error_msg" gorm:"size:500"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (RewriteProject) TableName() string { return "ink_rewrite_project" }

// LiteraryAnalysis holds the AI analysis of the original novel
type LiteraryAnalysis struct {
	ID                uint      `json:"id" gorm:"primaryKey"`
	ProjectID         uint      `json:"project_id" gorm:"uniqueIndex"`
	VoiceFingerprint  string    `json:"voice_fingerprint" gorm:"type:text"` // JSON: narrator_style, sentence_patterns, distinctive_phrases
	SceneArchitecture string    `json:"scene_architecture" gorm:"type:text"` // JSON: typical_scene_structure, pacing, transitions
	CharacterPsych    string    `json:"character_psych" gorm:"type:text"` // JSON: character archetypes, relationship topology
	ThemeCore         string    `json:"theme_core" gorm:"type:text"` // JSON: themes, motifs, symbols
	WorldLogic        string    `json:"world_logic" gorm:"type:text"` // JSON: world rules, power systems, geography
	HighRiskMarkers   string    `json:"high_risk_markers" gorm:"type:text"` // JSON: unique phrases, plot sequences, signature elements
	CreatedAt         time.Time `json:"created_at"`
}

func (LiteraryAnalysis) TableName() string { return "ink_literary_analysis" }

// RewriteBible is the strategic rewriting guide
type RewriteBible struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	ProjectID      uint      `json:"project_id" gorm:"uniqueIndex"`
	NewWorldName   string    `json:"new_world_name" gorm:"size:200"`
	NewCharNames   string    `json:"new_char_names" gorm:"type:text"` // JSON: map[oldName]newName
	PlotTransform  string    `json:"plot_transform" gorm:"type:text"` // JSON: transformation rules for main plot beats
	VoiceStrategy  string    `json:"voice_strategy" gorm:"type:text"` // JSON: narrator voice guidelines
	StyleGuide     string    `json:"style_guide" gorm:"type:text"` // JSON: sentence structure, vocabulary register, pacing rules
	ForbiddenElems string    `json:"forbidden_elems" gorm:"type:text"` // JSON: elements to avoid (high-risk markers)
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (RewriteBible) TableName() string { return "ink_rewrite_bible" }

// ChapterRewriteTask tracks rewriting progress per chapter
type ChapterRewriteTask struct {
	ID               uint      `json:"id" gorm:"primaryKey"`
	ProjectID        uint      `json:"project_id" gorm:"index"`
	ChapterID        uint      `json:"chapter_id" gorm:"index"`
	ChapterNo        int       `json:"chapter_no"`
	Status           string    `json:"status" gorm:"size:20;default:'pending'"` // pending, rewriting, reviewing, completed, failed
	OriginalContent  string    `json:"original_content" gorm:"type:longtext"`
	RewrittenContent string    `json:"rewritten_content" gorm:"type:longtext"`
	SimilarityScore  float64   `json:"similarity_score" gorm:"default:0"` // 0-1, lower is better (more different)
	LexicalSim       float64   `json:"lexical_sim" gorm:"default:0"`
	SemanticSim      float64   `json:"semantic_sim" gorm:"default:0"`
	StructuralSim    float64   `json:"structural_sim" gorm:"default:0"`
	Passed           bool      `json:"passed" gorm:"default:false"`
	RetryCount       int       `json:"retry_count" gorm:"default:0"`
	ErrorMsg         string    `json:"error_msg" gorm:"size:500"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (ChapterRewriteTask) TableName() string { return "ink_chapter_rewrite_task" }
