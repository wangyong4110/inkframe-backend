package model

import (
	"time"

	"gorm.io/gorm"
)

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

	// 高清 & 3D 视频生成（项目全局）
	KlingModel    string `json:"kling_model" gorm:"size:50;default:'kling-v1'"`    // kling-v1/kling-v1-6/kling-v2
	HDEnabled     bool   `json:"hd_enabled" gorm:"default:false"`                  // 开启高清输出（自动升级为 kling-v1-6 + pro）
	ThreeDEnabled bool   `json:"three_d_enabled" gorm:"default:false"`              // 开启 3D 动画风格（项目全局默认）

	// 字幕样式
	SubtitleStyle string `json:"subtitle_style" gorm:"size:20;default:'none'"`                  // none/basic/cinematic/anime
	SubtitleFont  string `json:"subtitle_font" gorm:"size:100;default:'Noto Sans CJK SC'"` // 字体名称

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (NovelVideoConfig) TableName() string { return "ink_novel_video_config" }

// Video 视频
type Video struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;default:0"`
	UUID     string `json:"uuid" gorm:"uniqueIndex;size:36"`
	NovelID  uint   `json:"novel_id" gorm:"index;not null"`
	Novel     *Novel   `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID *uint    `json:"chapter_id,omitempty" gorm:"index"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"-"`

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
	Progress     float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`
	ReviewStatus string  `json:"review_status" gorm:"size:20;default:'none'"` // none/pending/reviewed

	// 质量档位
	QualityTier string `json:"quality_tier" gorm:"size:20;default:preview"`
	// draft=草稿(静图+Pan), preview=预览(720p短片), final=正式(1080p+)

	// 高清 & 3D 视觉模式
	VisualMode  string `json:"visual_mode" gorm:"size:30;default:'standard'"` // standard/hd/3d/hd_3d
	ThreeDStyle string `json:"three_d_style" gorm:"size:50;default:'cg'"`     // cg/pixar/anime3d/realistic3d

	// 成本
	GenerationCost float64 `json:"generation_cost" gorm:"type:decimal(10,2)"`

	// 异步任务追踪
	ProviderName string `json:"provider_name" gorm:"size:50"`             // kling/seedance
	TaskID       string `json:"task_id" gorm:"size:255;index"`            // 外部 API 任务 ID
	ErrorMessage string `json:"error_message,omitempty" gorm:"type:text"` // 生成失败原因
	RetryCount   int    `json:"retry_count" gorm:"default:0"`             // 已重试次数

	// 合成与发布
	FinalVideoURL string     `json:"final_video_url" gorm:"size:2000"`
	CoverURL      string     `json:"cover_url" gorm:"size:2000"`
	IsPublished   bool       `json:"is_published" gorm:"default:false;index"`
	PublishedAt   *time.Time `json:"published_at"`
	ViewCount     int        `json:"view_count" gorm:"default:0"`
	Visibility    string     `json:"visibility" gorm:"size:20;default:'private'"` // private|unlisted|public

	// 广场社交字段
	LikeCount    int     `json:"like_count" gorm:"default:0"`
	CommentCount int     `json:"comment_count" gorm:"default:0"`
	HotScore     float64 `json:"hot_score" gorm:"default:0;index"` // 热度分，定时计算
	Tags         string  `json:"tags" gorm:"size:500"`             // JSON 数组，如 ["玄幻","古风"]

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
	ShotID uint `json:"shot_id" gorm:"not null;index:idx_seg_shot_seq,unique,priority:1"`
	SeqNo  int  `json:"seq_no" gorm:"not null;default:1;index:idx_seg_shot_seq,unique,priority:2"`

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

	// 音效分类与播放行为（v2）
	SFXType     string `json:"sfx_type" gorm:"size:20;default:'action'"` // action / ambient / emotion
	LoopEnabled bool   `json:"loop_enabled" gorm:"default:false"`         // true=循环播放直到镜头结束
	FadeInMs    int    `json:"fade_in_ms" gorm:"default:0"`               // 淡入时长（毫秒）
	FadeOutMs   int    `json:"fade_out_ms" gorm:"default:200"`            // 淡出时长（毫秒）

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

// ─── 分镜脚本 AI 审查 ──────────────────────────────────────────────────────────

// ShotReviewFeedback 单个镜头的审查反馈
type ShotReviewFeedback struct {
	ShotNo               int      `json:"shot_no"`
	Issues               []string `json:"issues"`
	Suggestion           string   `json:"suggestion"`
	Severity             string   `json:"severity"` // info / warning / error
	SuggestedNarration   string   `json:"suggested_narration,omitempty"`
	SuggestedDescription string   `json:"suggested_description,omitempty"`
}

// ─── 统一审查记录（章节/分镜共用）────────────────────────────────────────────────

const (
	ReviewEntityChapter    = "chapter"
	ReviewEntityStoryboard = "storyboard"
)

// ReviewRecord 统一审查历史记录（章节/分镜共用同一张表）
type ReviewRecord struct {
	gorm.Model
	TenantID         uint       `json:"tenant_id" gorm:"index;not null;default:1"`
	EntityType       string     `json:"entity_type" gorm:"size:20;index"`           // "chapter" | "storyboard"
	EntityID         uint       `json:"entity_id" gorm:"index"`                     // chapter_id 或 video_id
	OverallScore     float64    `json:"overall_score"`
	ReviewJSON       string     `json:"-" gorm:"column:review_json;type:text"`      // ChapterReview 或 StoryboardReview JSON
	Status           string     `json:"status" gorm:"size:20;default:'pending'"`    // pending|applied|rolled_back
	AppliedAt        *time.Time `json:"applied_at,omitempty"`
	AppliedDiffsJSON string     `json:"-" gorm:"column:applied_diffs;type:text"`    // 已应用变更 JSON
	SnapshotJSON     string     `json:"-" gorm:"column:snapshot_json;type:text"`    // 回滚快照 JSON
}

func (ReviewRecord) TableName() string { return "ink_review_record" }

// IgnoredReviewIssue 统一已忽略的审查问题（章节/分镜共用同一张表）
type IgnoredReviewIssue struct {
	gorm.Model
	TenantID    uint   `json:"tenant_id" gorm:"index;not null;default:1"`
	EntityType  string `json:"entity_type" gorm:"size:20;index"`
	EntityID    uint   `json:"entity_id" gorm:"index"`
	IssueText   string `json:"issue_text" gorm:"type:text"`
	IssueHash   string `json:"issue_hash,omitempty" gorm:"size:64;index"`
	ContextJSON string `json:"-" gorm:"column:context_json;type:text"` // 分镜: {"shot_no":3}
	Note        string `json:"note,omitempty" gorm:"type:text"`
}

func (IgnoredReviewIssue) TableName() string { return "ink_ignored_review_issue" }

// ShotRollbackItem 回滚快照中的单镜原始内容
type ShotRollbackItem struct {
	ShotID      uint   `json:"shot_id"`
	ShotNo      int    `json:"shot_no"`
	Narration   string `json:"narration"`
	Description string `json:"description"`
}

// ShotInsertSuggestion AI 审查建议插入的新分镜
type ShotInsertSuggestion struct {
	AfterShotNo int     `json:"after_shot_no"` // 在此编号镜头之后插入；0=插入到最前
	Narration   string  `json:"narration"`
	Description string  `json:"description"`
	Duration    float64 `json:"duration"`
	ShotSize    string  `json:"shot_size,omitempty"`
	CameraType  string  `json:"camera_type,omitempty"`
	Reason      string  `json:"reason"` // 插入原因（引用章节文本依据）
}

// ShotDeleteSuggestion AI 审查建议删除的分镜
type ShotDeleteSuggestion struct {
	ShotNo int    `json:"shot_no"`
	Reason string `json:"reason"` // 删除原因
}

// StoryboardReview AI 分镜脚本审查报告
type StoryboardReview struct {
	OverallScore      float64                `json:"overall_score"`       // 综合得分 0-100
	NarrativeScore    float64                `json:"narrative_score"`     // 叙事连贯性
	VisualScore       float64                `json:"visual_score"`        // 视觉多样性
	PacingScore       float64                `json:"pacing_score"`        // 节奏控制
	VoiceoverScore    float64                `json:"voiceover_score"`     // 旁白质量
	Summary           string                 `json:"summary"`             // 综合评价
	Strengths         []string               `json:"strengths"`           // 亮点
	Weaknesses        []string               `json:"weaknesses"`          // 主要问题
	GlobalSuggestions []string               `json:"global_suggestions"`  // 整体改进建议
	ShotFeedback      []ShotReviewFeedback   `json:"shot_feedback"`       // 逐镜反馈（仅有问题的镜头）
	SuggestedInserts  []ShotInsertSuggestion `json:"suggested_inserts,omitempty"` // 建议插入的新镜头
	SuggestedDeletes  []ShotDeleteSuggestion `json:"suggested_deletes,omitempty"` // 建议删除的镜头
}

// ─── 章节 AI 审查 ──────────────────────────────────────────────────────────────

// ParagraphFeedback 段落级审查反馈（嵌入 JSON，不单独建表）
type ParagraphFeedback struct {
	Index            int      `json:"index"`             // 段落序号(0起)
	OrigText         string   `json:"orig_text"`         // 原文摘要(前80字)
	Issues           []string `json:"issues"`
	Suggestion       string   `json:"suggestion"`
	Action           string   `json:"action"`            // "rewrite"(默认) | "delete"
	SuggestedRewrite string   `json:"suggested_rewrite"` // 改写后正文；action=delete 时为空
	Severity         string   `json:"severity"`          // info/warning/error
}

// WeaknessItem 不足条目，带对应改进建议
type WeaknessItem struct {
	Issue      string `json:"issue"`      // 问题描述
	Suggestion string `json:"suggestion"` // 对应改进建议
}

// ChapterReview AI章节审查报告
type ChapterReview struct {
	OverallScore      float64             `json:"overall_score"`
	NarrativeScore    float64             `json:"narrative_score"`
	CharacterScore    float64             `json:"character_score"`
	WritingScore      float64             `json:"writing_score"`
	PacingScore       float64             `json:"pacing_score"`
	DramaticScore     float64             `json:"dramatic_score"`   // 戏剧张力
	VisualPotential   float64             `json:"visual_potential"` // 画面感/可视化潜力
	Summary           string              `json:"summary"`
	Strengths         []string            `json:"strengths"`
	Weaknesses        []WeaknessItem      `json:"weaknesses"`
	GlobalSuggestions []string            `json:"global_suggestions"`
	ParagraphFeedback []ParagraphFeedback `json:"paragraph_feedback"`
	RecordID          uint                `json:"record_id,omitempty"`
}


// ─── Video / Storyboard DTOs ───────────────────────────────────────────────────

type CreateVideoRequest struct {
	Title       string `json:"title"`
	Resolution  string `json:"resolution"`
	FrameRate   int    `json:"frame_rate"`
	AspectRatio string `json:"aspect_ratio"`
	ArtStyle    string `json:"art_style"`
	QualityTier string `json:"quality_tier"` // draft/preview/final
	ChapterID   *uint  `json:"chapter_id"`
	Mode        string `json:"mode"`        // video/slideshow
	VisualMode  string `json:"visual_mode"` // standard/hd/3d/hd_3d
	ThreeDStyle string `json:"three_d_style"` // cg/pixar/anime3d/realistic3d
}

type UpdateVideoRequest struct {
	Title        string `json:"title"`
	Resolution   string `json:"resolution"`
	FrameRate    int    `json:"frame_rate"`
	AspectRatio  string `json:"aspect_ratio"`
	ArtStyle     string `json:"art_style"`
	ScriptStatus string `json:"script_status"` // draft/confirmed
	Mode         string `json:"mode"`           // video/slideshow
	VisualMode   string `json:"visual_mode"`    // standard/hd/3d/hd_3d
	ThreeDStyle  string `json:"three_d_style"`  // cg/pixar/anime3d/realistic3d
}

type EnhancementConfig struct {
	Type      string  `json:"type"`
	Enabled   bool    `json:"enabled"`
	Intensity float64 `json:"intensity,omitempty"`
}

type BatchGenerateShotsRequest struct {
	ShotIDs     []uint `json:"shot_ids" binding:"required"`
	QualityTier string `json:"quality_tier"` // override; empty = use video's quality_tier
	Provider    string `json:"provider"`     // video provider override (e.g. "kling", "seedance")
}
