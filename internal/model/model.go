package model

import (
	"time"
)

// Novel 小说
type Novel struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	UUID        string    `json:"uuid" gorm:"uniqueIndex;size:36"`
	Title       string    `json:"title" gorm:"size:255;not null"`
	Description string    `json:"description" gorm:"type:text"`
	Genre       string    `json:"genre" gorm:"size:50;index"`

	// 状态
	Status      string    `json:"status" gorm:"size:20;index;default:planning"`
	// planning=规划中, writing=创作中, paused=暂停, completed=已完成, archived=已归档

	// 统计
	TotalWords   int       `json:"total_words" gorm:"default:0"`
	ChapterCount int       `json:"chapter_count" gorm:"default:0"`
	ViewCount    int       `json:"view_count" gorm:"default:0"`

	// 关联
	WorldviewID  *uint      `json:"worldview_id"`
	Worldview    *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`
	CoverImage   string     `json:"cover_image" gorm:"size:500"`

	// AI配置
	AIModel      string  `json:"ai_model" gorm:"size:100"`
	Temperature  float64 `json:"temperature" gorm:"type:decimal(3,2);default:0.7"`
	MaxTokens    int     `json:"max_tokens" gorm:"default:4096"`
	StylePrompt  string  `json:"style_prompt" gorm:"type:text"`

	// 时间戳
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Novel) TableName() string {
	return "ink_novel"
}

// Chapter 章节
type Chapter struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	NovelID   uint      `json:"novel_id" gorm:"index;not null"`
	Novel     *Novel    `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID      string    `json:"uuid" gorm:"uniqueIndex;size:36"`
	ChapterNo int       `json:"chapter_no" gorm:"not null"`
	Title     string    `json:"title" gorm:"size:255"`

	// 内容
	Content  string `json:"content" gorm:"type:text"`
	Summary  string `json:"summary" gorm:"type:text"`
	WordCount int   `json:"word_count" gorm:"default:0"`

	// 大纲
	Outline   string `json:"outline" gorm:"type:text"`
	PlotPoints string `json:"plot_points" gorm:"type:text"` // JSON数组

	// 状态
	Status   string `json:"status" gorm:"size:20;default:draft"`
	// draft=草稿, generating=生成中, completed=已完成, published=已发布

	// 关联
	PreviousChapterID *uint    `json:"previous_chapter_id"`
	NextChapterID     *uint    `json:"next_chapter_id"`

	// 质量评分
	QualityScore float64 `json:"quality_score" gorm:"type:decimal(5,4)"`

	// 时间戳
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

func (Chapter) TableName() string {
	return "ink_chapter"
}

// PlotPoint 剧情点
type PlotPoint struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	ChapterID   uint   `json:"chapter_id" gorm:"index;not null"`
	Chapter     *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	Type        string `json:"type" gorm:"size:50"`
	// conflict=冲突, climax=高潮, resolution=解决, twist=转折, foreshadow=伏笔

	Description string `json:"description" gorm:"type:text"`
	Characters  string `json:"characters" gorm:"type:text"` // JSON数组
	Locations   string `json:"locations" gorm:"type:text"`  // JSON数组

	IsResolved  bool   `json:"is_resolved" gorm:"default:false"`
	ResolvedIn  *uint  `json:"resolved_in"` // 解决这一剧情点的章节ID

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PlotPoint) TableName() string {
	return "ink_plot_point"
}

// Worldview 世界观
type Worldview struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	UUID        string    `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string    `json:"name" gorm:"size:255;not null"`
	Description string    `json:"description" gorm:"type:text"`
	Genre       string    `json:"genre" gorm:"size:50;index"`

	// 世界观元素
	MagicSystem string `json:"magic_system" gorm:"type:text"`
	Geography   string `json:"geography" gorm:"type:text"`
	History     string `json:"history" gorm:"type:text"`
	Culture     string `json:"culture" gorm:"type:text"`
	Technology  string `json:"technology" gorm:"type:text"`

	// 约束规则（JSON）
	Rules string `json:"rules" gorm:"type:text"`

	// 封面
	CoverImage string `json:"cover_image" gorm:"size:500"`

	// 使用统计
	UsedCount int `json:"used_count" gorm:"default:0"`

	// 时间戳
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Worldview) TableName() string {
	return "ink_worldview"
}

// WorldviewEntity 世界观实体
type WorldviewEntity struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	WorldviewID uint   `json:"worldview_id" gorm:"index;not null"`
	Worldview   *Worldview `json:"worldview,omitempty" gorm:"foreignKey:WorldviewID"`

	Type        string `json:"type" gorm:"size:50;index"`
	// location=地点, organization=组织, artifact=神器, race=种族, creature=生物

	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`
	Attributes  string `json:"attributes" gorm:"type:text"` // JSON
	Relations   string `json:"relations" gorm:"type:text"`   // JSON
	ImageURL    string `json:"image_url" gorm:"size:500"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (WorldviewEntity) TableName() string {
	return "ink_worldview_entity"
}

// Character 角色
type Character struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	NovelID     uint      `json:"novel_id" gorm:"index;not null"`
	Novel       *Novel    `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID        string    `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name        string `json:"name" gorm:"size:100;not null"`
	Role        string `json:"role" gorm:"size:50"`
	// protagonist=主角, antagonist=反派, supporting=配角, minor=龙套

	Archetype   string `json:"archetype" gorm:"size:50"` // 角色原型

	// 外貌与性格
	Appearance  string `json:"appearance" gorm:"type:text"`
	Personality string `json:"personality" gorm:"type:text"`
	Background  string `json:"background" gorm:"type:text"`

	// 能力与属性
	Abilities   string `json:"abilities" gorm:"type:text"`   // JSON
	Attributes  string `json:"attributes" gorm:"type:text"` // JSON

	// 角色关系（JSON）
	Relations   string `json:"relations" gorm:"type:text"`

	// 角色弧光
	CharacterArc string `json:"character_arc" gorm:"type:text"`

	// 视觉设计
	VisualDesign string `json:"visual_design" gorm:"type:text"` // JSON: 包含图像URL、表情库等

	// 封面
	CoverImage string `json:"cover_image" gorm:"size:500"`

	// 状态
	Status string `json:"status" gorm:"size:20;default:active"`

	// 时间戳
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Character) TableName() string {
	return "ink_character"
}

// CharacterAppearance 角色出现记录
type CharacterAppearance struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	CharacterID   uint   `json:"character_id" gorm:"index;not null"`
	Character     *Character `json:"character,omitempty" gorm:"foreignKey:CharacterID"`
	ChapterID    uint   `json:"chapter_id" gorm:"index;not null"`
	Chapter       *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

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
	ID          uint      `json:"id" gorm:"primaryKey"`
	CharacterID uint      `json:"character_id" gorm:"index;not null"`
	ChapterID   uint      `json:"chapter_id" gorm:"index"`

	// 物理状态
	Age         float64   `json:"age"`
	Height      float64   `json:"height"` // 单位：米
	Weight      float64   `json:"weight"` // 单位：公斤
	Health      string    `json:"health"` // healthy, injured, critical
	Injuries    string    `json:"injuries" gorm:"type:text"` // JSON: [{part, severity, description}]

	// 能力状态
	PowerLevel  int       `json:"power_level"`
	Abilities   string    `json:"abilities" gorm:"type:text"` // JSON
	Equipment   string    `json:"equipment" gorm:"type:text"` // JSON

	// 心理状态
	Mood        string    `json:"mood"`
	Motivation  string    `json:"motivation"`
	Goals       string    `json:"goals" gorm:"type:text"` // JSON
	Fears       string    `json:"fears" gorm:"type:text"` // JSON

	// 位置状态
	Location    string    `json:"location"`
	KnownLocations string `json:"known_locations" gorm:"type:text"` // JSON

	// 关系状态
	Relations   string    `json:"relations" gorm:"type:text"` // JSON: [{character_id, attitude, recent_interaction}]

	SnapshotTime time.Time `json:"snapshot_time"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (CharacterStateSnapshot) TableName() string {
	return "ink_character_state_snapshot"
}

// ReferenceNovel 参考小说
type ReferenceNovel struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	UUID       string    `json:"uuid" gorm:"uniqueIndex;size:36"`
	Title      string    `json:"title" gorm:"size:255;not null"`
	Author     string    `json:"author" gorm:"size:100"`

	SourceURL  string `json:"source_url" gorm:"size:500"`
	SourceSite string `json:"source_site" gorm:"size:50"`
	// qidian=起点, jjwxc=晋江, zongheng=纵横

	Genre      string `json:"genre" gorm:"size:50;index"`

	// 统计
	TotalChapters int `json:"total_chapters" gorm:"default:0"`
	TotalWords    int `json:"total_words" gorm:"default:0"`

	// 状态
	Status string `json:"status" gorm:"size:20;default:crawling"`
	// crawling=爬取中, completed=已完成, failed=失败

	// 分析结果
	StyleAnalysis string `json:"style_analysis" gorm:"type:text"` // JSON
	Keywords      string `json:"keywords" gorm:"type:text"`        // JSON数组
	SimilarNovels string `json:"similar_novels" gorm:"type:text"` // JSON数组

	// 封面
	CoverImage string `json:"cover_image" gorm:"size:500"`

	// 时间戳
	CrawledAt time.Time `json:"crawled_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (ReferenceNovel) TableName() string {
	return "ink_reference_novel"
}

// ReferenceChapter 参考小说章节
type ReferenceChapter struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	NovelID   uint      `json:"novel_id" gorm:"index;not null"`
	Novel     *ReferenceNovel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID      string    `json:"uuid" gorm:"uniqueIndex;size:36"`
	ChapterNo int       `json:"chapter_no" gorm:"not null"`
	Title     string    `json:"title" gorm:"size:255"`
	Content   string    `json:"content" gorm:"type:text"`
	Summary   string    `json:"summary" gorm:"type:text"`
	WordCount int       `json:"word_count" gorm:"default:0"`

	CreatedAt time.Time `json:"created_at"`
}

func (ReferenceChapter) TableName() string {
	return "ink_reference_chapter"
}

// KnowledgeBase 知识库
type KnowledgeBase struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	Type   string `json:"type" gorm:"size:50;index"`
	// character_fact=角色事实, lore=世界观知识, writing_technique=写作技巧

	Title   string `json:"title" gorm:"size:255;not null"`
	Content string `json:"content" gorm:"type:text"`

	Tags    string `json:"tags" gorm:"type:text"` // JSON数组

	// 关联
	NovelID     *uint `json:"novel_id,omitempty" gorm:"index"`
	Novel       *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ReferenceID *uint `json:"reference_id,omitempty" gorm:"index"`
	Reference   *ReferenceNovel `json:"reference,omitempty" gorm:"foreignKey:ReferenceID"`

	// 向量信息
	VectorID   string `json:"vector_id" gorm:"size:100"`
	VectorHash string `json:"vector_hash" gorm:"size:64"`

	// 统计
	UsageCount int `json:"usage_count" gorm:"default:0"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (KnowledgeBase) TableName() string {
	return "ink_knowledge_base"
}

// PromptTemplate 提示词模板
type PromptTemplate struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	UUID   string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name   string `json:"name" gorm:"size:100;not null"`
	Genre  string `json:"genre" gorm:"size:50;index"`
	Stage  string `json:"stage" gorm:"size:50"`
	// outline=大纲, chapter=章节, dialogue=对话, description=描写

	// 模板内容
	Template string `json:"template" gorm:"type:text;not null"`

	// AI参数
	SystemPrompt string  `json:"system_prompt" gorm:"type:text"`
	Temperature  float64 `json:"temperature" gorm:"type:decimal(3,2)"`
	MaxTokens    int     `json:"max_tokens" gorm:"default:4096"`
	TopP         float64 `json:"top_p" gorm:"type:decimal(3,2)"`
	TopK         int     `json:"top_k" gorm:"default:40"`

	// 使用统计
	UsageCount int `json:"usage_count" gorm:"default:0"`

	// 状态
	IsDefault bool `json:"is_default" gorm:"default:false"`
	IsActive   bool `json:"is_active" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PromptTemplate) TableName() string {
	return "ink_prompt_template"
}

// ModelProvider 模型提供商
type ModelProvider struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Name        string `json:"name" gorm:"size:50;uniqueIndex;not null"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Type        string `json:"type" gorm:"size:20"`
	// cloud=云端, local=本地

	// API配置
	APIEndpoint string `json:"api_endpoint" gorm:"size:500"`
	APIKey      string `json:"api_key" gorm:"type:text"`
	APIVersion  string `json:"api_version" gorm:"size:50"`

	// 限制
	RateLimit  int     `json:"rate_limit"` // 请求/分钟
	MaxTokens  int     `json:"max_tokens"`
	CostPer1K  float64 `json:"cost_per_1k_tokens"`

	// 状态
	IsActive    bool   `json:"is_active" gorm:"default:true"`
	HealthCheck string `json:"health_check" gorm:"size:20;default:ok"`
	// ok=正常, degraded=降级, down=宕机

	LastChecked time.Time `json:"last_checked"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ModelProvider) TableName() string {
	return "ink_model_provider"
}

// AIModel AI模型
type AIModel struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	ProviderID uint   `json:"provider_id" gorm:"index;not null"`
	Provider   *ModelProvider `json:"provider,omitempty" gorm:"foreignKey:ProviderID"`

	Name        string `json:"name" gorm:"size:100;not null"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Version     string `json:"version" gorm:"size:50"`

	// 能力
	Capabilities string `json:"capabilities" gorm:"type:text"` // JSON

	// 性能指标
	MaxTokens     int     `json:"max_tokens"`
	ContextWindow int     `json:"context_window"`
	Speed         float64 `json:"speed"` // tokens/秒
	Quality       float64 `json:"quality"` // 0.0-1.0
	CostPer1K     float64 `json:"cost_per_1k_tokens"`

	// 适用任务
	SuitableTasks string `json:"suitable_tasks" gorm:"type:text"` // JSON数组

	// 默认参数
	DefaultTemperature float64 `json:"default_temperature" gorm:"type:decimal(3,2)"`
	DefaultTopP        float64 `json:"default_top_p" gorm:"type:decimal(3,2)"`
	DefaultTopK        int     `json:"default_top_k"`

	// 状态
	IsActive   bool `json:"is_active" gorm:"default:true"`
	IsAvailable bool `json:"is_available" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AIModel) TableName() string {
	return "ink_ai_model"
}

// TaskModelConfig 任务模型配置
type TaskModelConfig struct {
	ID     uint `json:"id" gorm:"primaryKey"`
	TaskType string `json:"task_type" gorm:"size:50;uniqueIndex;not null"`

	PrimaryModelID   uint   `json:"primary_model_id"`
	PrimaryModel     *AIModel `json:"primary_model,omitempty" gorm:"foreignKey:PrimaryModelID"`
	FallbackModelIDs string `json:"fallback_model_ids" gorm:"type:text"` // JSON数组

	// 参数
	Temperature float64 `json:"temperature" gorm:"type:decimal(3,2)"`
	TopP        float64 `json:"top_p" gorm:"type:decimal(3,2)"`
	TopK        int     `json:"top_k"`
	MaxTokens   int     `json:"max_tokens"`
	SystemPrompt string `json:"system_prompt" gorm:"type:text"`

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

	ModelIDs    string `json:"model_ids" gorm:"type:text"` // JSON数组
	Parameters string `json:"parameters" gorm:"type:text"`  // JSON

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
	ID           uint   `json:"id" gorm:"primaryKey"`
	ExperimentID uint   `json:"experiment_id" gorm:"index;not null"`
	Experiment   *ModelComparisonExperiment `json:"experiment,omitempty" gorm:"foreignKey:ExperimentID"`
	ModelID     uint   `json:"model_id" gorm:"index;not null"`
	Model       *AIModel `json:"model,omitempty" gorm:"foreignKey:ModelID"`

	Output string `json:"output" gorm:"type:text"`

	// 质量评分
	QualityScore       float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	RelevanceScore     float64 `json:"relevance_score" gorm:"type:decimal(5,4)"`
	CreativityScore    float64 `json:"creativity_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore   float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`

	// 成本
	TokensUsed int     `json:"tokens_used"`
	Cost      float64 `json:"cost"`
	Latency   float64 `json:"latency"` // 秒

	// 用户评价
	UserRating  *int    `json:"user_rating,omitempty"` // 1-5
	UserComment string  `json:"user_comment" gorm:"type:text"`

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (ExperimentResult) TableName() string {
	return "ink_experiment_result"
}

// ModelUsageLog 模型使用记录
type ModelUsageLog struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	ModelID uint `json:"model_id" gorm:"index;not null"`
	Model   *AIModel `json:"model,omitempty" gorm:"foreignKey:ModelID"`
	TaskType string `json:"task_type" gorm:"size:50;index"`

	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens int     `json:"total_tokens"`
	Cost        float64 `json:"cost"`
	Latency     float64 `json:"latency"` // 秒

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	QualityScore *float64 `json:"quality_score,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (ModelUsageLog) TableName() string {
	return "ink_model_usage_log"
}

// Video 视频
type Video struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	UUID       string `json:"uuid" gorm:"uniqueIndex;size:36"`
	NovelID    uint   `json:"novel_id" gorm:"index;not null"`
	Novel      *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID  *uint  `json:"chapter_id,omitempty" gorm:"index"`
	Chapter    *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	Title       string `json:"title" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`

	// 配置
	Type        string `json:"type" gorm:"size:50;default:image_sequence"`
	// image_sequence=图片序列, animation=动画, live_action=真人

	Resolution  string `json:"resolution" gorm:"size:20;default:1080p"`
	FrameRate   int    `json:"frame_rate" gorm:"default:24"`
	AspectRatio string `json:"aspect_ratio" gorm:"size:10;default:16:9"`
	ArtStyle   string `json:"art_style" gorm:"size:50"`

	// 统计
	Duration      float64 `json:"duration"` // 秒
	TotalShots   int     `json:"total_shots" gorm:"default:0"`
	TotalFrames  int     `json:"total_frames" gorm:"default:0"`
	TotalWords   int     `json:"total_words" gorm:"default:0"`

	// 文件
	VideoPath  string `json:"video_path" gorm:"size:500"`
	Thumbnail string `json:"thumbnail" gorm:"size:500"`

	// 状态
	Status   string  `json:"status" gorm:"size:20;default:planning"`
	Progress float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`

	// 成本
	GenerationCost float64 `json:"generation_cost" gorm:"type:decimal(10,2)"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Video) TableName() string {
	return "ink_video"
}

// StoryboardShot 分镜
type StoryboardShot struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	VideoID    uint   `json:"video_id" gorm:"index;not null"`
	Video      *Video `json:"video,omitempty" gorm:"foreignKey:VideoID"`
	UUID       string `json:"uuid" gorm:"uniqueIndex;size:36"`
	ShotNo     int    `json:"shot_no" gorm:"not null"`
	ChapterID  *uint  `json:"chapter_id,omitempty" gorm:"index"`
	Chapter    *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	Description string `json:"description" gorm:"type:text"`
	Dialogue    string `json:"dialogue" gorm:"type:text"`

	// 摄像机配置
	CameraType  string `json:"camera_type" gorm:"size:50;default:static"`
	// static=静态, pan=平移, zoom=缩放, tracking=跟踪, dolly=移动

	CameraAngle string `json:"camera_angle" gorm:"size:50;default:eye_level"`
	// eye_level=平视, low=俯, high=仰, dutch=荷兰角

	ShotSize    string `json:"shot_size" gorm:"size:50;default:medium"`
	// wide=远景, medium=中景, close_up=近景, extreme_close_up=特写

	Duration    float64 `json:"duration" gorm:"type:decimal(5,2);default:5.0"`

	// 角色和场景（JSON）
	Characters string `json:"characters" gorm:"type:text"`
	Scene      string `json:"scene" gorm:"type:text"`

	// AI生成
	Prompt        string `json:"prompt" gorm:"type:text"`
	NegativePrompt string `json:"negative_prompt" gorm:"type:text"`
	Frames        string `json:"frames" gorm:"type:text"` // JSON数组

	// 状态
	Status   string  `json:"status" gorm:"size:20;default:pending"`
	Progress float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`

	// 文件
	ClipPath string `json:"clip_path" gorm:"size:500"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (StoryboardShot) TableName() string {
	return "ink_storyboard_shot"
}

// CharacterVisualDesign 角色视觉设计
type CharacterVisualDesign struct {
	ID          uint `json:"id" gorm:"primaryKey"`
	CharacterID uint `json:"character_id" gorm:"index;unique;not null"`
	Character   *Character `json:"character,omitempty" gorm:"foreignKey:CharacterID"`

	AppearanceDescription string `json:"appearance_description" gorm:"type:text"`

	// 视觉特征
	FacialFeatures string `json:"facial_features" gorm:"type:text"`
	HairStyle     string `json:"hair_style" gorm:"type:text"`
	SkinTone      string `json:"skin_tone" gorm:"size:50"`
	BodyType      string `json:"body_type" gorm:"size:50"`
	Age           int    `json:"age"`
	Gender        string `json:"gender" gorm:"size:20"`

	// 服装装备
	Outfit       string `json:"outfit" gorm:"type:text"`
	Accessories  string `json:"accessories" gorm:"type:text"`
	Weapons      string `json:"weapons" gorm:"type:text"`

	// 风格
	ArtStyle     string `json:"art_style" gorm:"size:50;default:realistic"`
	ColorPalette string `json:"color_palette" gorm:"type:text"`

	// 参考图
	ReferenceImageURLs string `json:"reference_image_urls" gorm:"type:text"`
	GeneratedImages    string `json:"generated_images" gorm:"type:text"`

	// LoRA
	LoraModelID string  `json:"lora_model_id" gorm:"size:100"`
	LoraWeight  float64 `json:"lora_weight" gorm:"type:decimal(3,2);default:0.7"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (CharacterVisualDesign) TableName() string {
	return "ink_character_visual_design"
}

// SceneVisualDesign 场景视觉设计
type SceneVisualDesign struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	SceneID uint `json:"scene_id" gorm:"index;unique;not null"`

	SceneDescription string `json:"scene_description" gorm:"type:text"`

	// 场景元素
	Environment  string `json:"environment" gorm:"type:text"`
	Architecture string `json:"architecture" gorm:"type:text"`
	Props        string `json:"props" gorm:"type:text"`
	Lighting     string `json:"lighting" gorm:"type:text"`
	Atmosphere   string `json:"atmosphere" gorm:"size:50"`

	// 风格
	ArtStyle     string `json:"art_style" gorm:"size:50;default:realistic"`
	ColorPalette string `json:"color_palette" gorm:"type:text"`
	Mood         string `json:"mood" gorm:"size:50"`

	// 参考图
	ReferenceImageURLs string `json:"reference_image_urls" gorm:"type:text"`
	GeneratedImages    string `json:"generated_images" gorm:"type:text"`

	// LoRA
	LoraModelID string  `json:"lora_model_id" gorm:"size:100"`
	LoraWeight  float64 `json:"lora_weight" gorm:"type:decimal(3,2);default:0.7"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (SceneVisualDesign) TableName() string {
	return "ink_scene_visual_design"
}

// QualityReport 质量报告
type QualityReport struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	UUID       string `json:"uuid" gorm:"uniqueIndex;size:36"`
	TargetType string `json:"target_type" gorm:"size:50;index"`
	// novel=小说, chapter=章节, video=视频

	TargetID uint `json:"target_id" gorm:"index;not null"`

	// 整体评分
	OverallScore float64 `json:"overall_score" gorm:"type:decimal(5,4)"`
	QualityScore float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`
	CreativityScore float64 `json:"creativity_score" gorm:"type:decimal(5,4)"`

	// 问题统计
	TotalIssues    int `json:"total_issues"`
	HighPriority   int `json:"high_priority"`
	MediumPriority int `json:"medium_priority"`
	LowPriority    int `json:"low_priority"`

	// 详细报告（JSON）
	Issues  string `json:"issues" gorm:"type:text"`
	Suggestions string `json:"suggestions" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (QualityReport) TableName() string {
	return "ink_quality_report"
}

// ReviewTask 审核任务
type ReviewTask struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	UUID       string `json:"uuid" gorm:"uniqueIndex;size:36"`
	NovelID    uint   `json:"novel_id" gorm:"index;not null"`
	Novel      *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID  *uint  `json:"chapter_id,omitempty" gorm:"index"`
	Chapter    *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	Type     string `json:"type" gorm:"size:50;index"`
	Priority string `json:"priority" gorm:"size:20;default:medium"`
	// high=高, medium=中, low=低

	Issues  string `json:"issues" gorm:"type:text"` // JSON

	Status      string    `json:"status" gorm:"size:20;default:pending"`
	// pending=待处理, in_progress=处理中, completed=已完成, rejected=已驳回

	AssignedTo *uint  `json:"assigned_to,omitempty"`
	ReviewerNote string `json:"reviewer_note" gorm:"type:text"`

	CompletedAt *time.Time `json:"completed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ReviewTask) TableName() string {
	return "ink_review_task"
}

// ChapterVersion 章节版本
type ChapterVersion struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	ChapterID  uint   `json:"chapter_id" gorm:"index;not null"`
	Chapter    *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	VersionNo  int    `json:"version_no" gorm:"not null"`

	Content string `json:"content" gorm:"type:text"`

	ChangeType        string `json:"change_type" gorm:"size:50"`
	// generation=AI生成, manual_edit=手动编辑, ai_revision=AI修改, rollback=回滚

	ChangeDescription string `json:"change_description" gorm:"type:text"`
	ChangeAuthorID    *uint  `json:"change_author_id,omitempty"`

	QualityScore       float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore   float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`

	CreatedAt time.Time `json:"created_at"`
}

func (ChapterVersion) TableName() string {
	return "ink_chapter_version"
}

// FeedbackRecord 反馈记录
type FeedbackRecord struct {
	ID         uint   `json:"id" gorm:"primaryKey"`
	NovelID    uint   `json:"novel_id" gorm:"index;not null"`
	Novel      *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID  *uint  `json:"chapter_id,omitempty" gorm:"index"`
	Chapter    *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`

	FeedbackType string `json:"feedback_type" gorm:"size:50;index"`
	// consistency_issue=一致性问题, quality_issue=质量问题, user_suggestion=用户建议

	IssueType   string `json:"issue_type" gorm:"size:50"`
	Description string `json:"description" gorm:"type:text"`

	UserRating  *int    `json:"user_rating,omitempty"` // 1-5
	UserComment string  `json:"user_comment" gorm:"type:text"`

	AIResponse  string `json:"ai_response" gorm:"type:text"`
	WasHelpful  *bool  `json:"was_helpful,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (FeedbackRecord) TableName() string {
	return "ink_feedback_record"
}

