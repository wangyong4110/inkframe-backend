package model

import (
	"time"

	"gorm.io/gorm"
)

// NovelVideoConfigData 视频配置数据（序列化为 JSON 存储）
type NovelVideoConfigData struct {
	VideoType             string  `json:"video_type"`
	VideoResolution       string  `json:"video_resolution"`
	VideoFPS              int     `json:"video_fps"`
	VideoAspectRatio      string  `json:"video_aspect_ratio"`
	CharConsistencyWeight float64 `json:"char_consistency_weight"`
	NarrationVoice        string  `json:"narration_voice"`
	SubtitleEnabled       bool    `json:"subtitle_enabled"`
	SubtitlePosition      string  `json:"subtitle_position"`
	SubtitleFontSize      int     `json:"subtitle_font_size"`
	SubtitleColor         string  `json:"subtitle_color"`
	SubtitleBgStyle       string  `json:"subtitle_bg_style"`
	ColorGrade            string  `json:"color_grade"`
	ContrastLevel         float64 `json:"contrast_level"`
	Saturation            float64 `json:"saturation"`
	FilmGrain             bool    `json:"film_grain"`
	Vignette              bool    `json:"vignette"`
	ChromaticAberration   bool    `json:"chromatic_aberration"`
	KlingProForAction     bool    `json:"kling_pro_for_action"`
	KlingModel            string  `json:"kling_model"`
	ThreeDEnabled         bool    `json:"three_d_enabled"`
	SubtitleStyle         string  `json:"subtitle_style"`
	SubtitleFont          string  `json:"subtitle_font"`
}

// NovelVideoConfig 小说视频/字幕配置（1:1 with Novel，独立表）
// 所有配置数据存储到 Config (JSON) 字段中以减少列数。
type NovelVideoConfig struct {
	ID        uint                 `json:"id" gorm:"primaryKey"`
	NovelID   uint                 `json:"novel_id" gorm:"uniqueIndex;not null"`
	Config    NovelVideoConfigData `json:"config" gorm:"column:config;serializer:json;type:text"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func (NovelVideoConfig) TableName() string { return "ink_novel_video_config" }

// VideoRenderConfig 视频渲染参数（合并存储为 JSON）
type VideoRenderConfig struct {
	Type           string `json:"type"`
	Resolution     string `json:"resolution"`
	FrameRate      int    `json:"frame_rate"`
	AspectRatio    string `json:"aspect_ratio"`
	ArtStyle       string `json:"art_style"`
	Pacing         string `json:"pacing"`
	TargetDuration int    `json:"target_duration"`
	QualityTier    string `json:"quality_tier"`
	VisualMode     string `json:"visual_mode"`
	ThreeDStyle    string `json:"three_d_style"`
}

// VideoPublishMeta 发布与展示元数据（JSON存储）
// 注意：PublishedAt、Visibility、HotScore 已迁移为 Video 独立列，不再存此结构体。
type VideoPublishMeta struct {
	Description   string  `json:"description"`
	Tags          string  `json:"tags"`
	Thumbnail     string  `json:"thumbnail"`
	CoverURL      string  `json:"cover_url"`
	FinalVideoURL string  `json:"final_video_url"`
	TotalShots    int     `json:"total_shots"`
	Duration      float64 `json:"duration"`
	ReviewStatus  string  `json:"review_status"`
}

// VideoTaskMeta 异步任务状态（JSON存储）
type VideoTaskMeta struct {
	ProviderName string  `json:"provider_name"`
	TaskID       string  `json:"task_id"`
	ErrorMessage string  `json:"error_message"`
	RetryCount   int     `json:"retry_count"`
	Progress     float64 `json:"progress"`
	VideoPath    string  `json:"video_path"`
	ScriptStatus string  `json:"script_status"`
}

// Video 视频
type Video struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	UUID      string `json:"uuid" gorm:"uniqueIndex;size:36"`
	NovelID   uint   `json:"novel_id" gorm:"index;not null"`
	Novel     *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	ChapterID *uint  `json:"chapter_id,omitempty" gorm:"index"`

	Title string `json:"title" gorm:"size:255;not null"`

	Mode string `json:"mode" gorm:"size:20;default:'video'"`
	// video=AI视频生成（Kling/Seedance）, slideshow=图片解说（图片+Ken Burns效果）

	// 状态
	Status string `json:"status" gorm:"size:20;default:planning"`

	// 发布状态
	IsPublished bool `json:"is_published" gorm:"default:false;index"`

	// 独立列（用于 WHERE / ORDER BY 索引查询）
	Visibility  string     `json:"visibility" gorm:"size:20;default:'private'"`
	PublishedAt *time.Time `json:"published_at" gorm:"index"`
	HotScore    float64    `json:"hot_score" gorm:"default:0;index"`

	// 短剧分集参数（0=不启用）
	EpisodeCount       int `json:"episode_count" gorm:"default:0"`        // 总集数
	EpisodeDurationSec int `json:"episode_duration_sec" gorm:"default:0"` // 每集目标时长（秒）

	// 统计计数（不存 ink_video 主表，从 ink_content_stats 加载）
	ViewCount    int `json:"view_count" gorm:"-"`
	LikeCount    int `json:"like_count" gorm:"-"`
	CommentCount int `json:"comment_count" gorm:"-"`

	// 渲染参数（JSON）
	RenderConfig VideoRenderConfig `json:"render_config" gorm:"column:render_config;serializer:json;type:text"`

	// JSON 合并字段（减少列数）
	PublishMeta VideoPublishMeta `json:"publish_meta" gorm:"column:publish_meta;serializer:json;type:text"`
	TaskMeta    VideoTaskMeta    `json:"task_meta" gorm:"column:task_meta;serializer:json;type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Video) TableName() string {
	return "ink_video"
}

// ShotCamDir 摄像机方向与风格配置（合并存储为 JSON）
type ShotCamDir struct {
	CameraType    string `json:"camera_type"`
	CameraAngle   string `json:"camera_angle"`
	ShotSize      string `json:"shot_size"`
	EmotionalTone string `json:"emotional_tone"`
	Transition    string `json:"transition"`
	TransitionOut string `json:"transition_out"`
	TransitionIn  string `json:"transition_in"`
}

// ShotGenMeta AI生成元数据（合并存储为 JSON）
type ShotGenMeta struct {
	Prompt            string  `json:"prompt"`
	NegativePrompt    string  `json:"negative_prompt"`
	MotionPrompt      string  `json:"motion_prompt"`
	Characters        string  `json:"characters"`
	Scene             string  `json:"scene"`
	GenerationMode    string  `json:"generation_mode"`
	ConsistencyScore  float64 `json:"consistency_score"`
	ReferenceImageURL string  `json:"reference_image_url"`
	SFXTags           string  `json:"sfx_tags"`
	Dialogue          string  `json:"dialogue"`
	Subtitle          string  `json:"subtitle"`
}

// ShotTaskMeta 任务状态与时间轴（JSON存储）
type ShotTaskMeta struct {
	Progress            float64 `json:"progress"`
	ErrorMessage        string  `json:"error_message"`
	ClipPath            string  `json:"clip_path"`
	AudioPath           string  `json:"audio_path"`
	ShotTaskID          string  `json:"shot_task_id"`
	ShotProviderName    string  `json:"shot_provider_name"`
	RetryCount          int     `json:"retry_count"`
	ActualVideoDuration float64 `json:"actual_video_duration"`
	TimelineStart       float64 `json:"timeline_start"`
	VoiceDelay          float64 `json:"voice_delay"`
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

	Duration float64 `json:"duration" gorm:"type:decimal(5,2);default:5.0"`

	// 状态
	Status       string `json:"status" gorm:"size:20;index:idx_shot_video_status,priority:2;default:pending"`
	ErrorMessage string `json:"error_message" gorm:"size:1000"` // 最后一次失败原因，供前端展示

	// AI 生成结果 URL（前端展示用）
	ImageURL string `json:"image_url" gorm:"size:1000"` // AI生成图片URL
	VideoURL string `json:"video_url" gorm:"size:1000"` // AI生成视频URL

	// 场景锚点
	SceneAnchorID *uint `json:"scene_anchor_id,omitempty" gorm:"index"`

	// 角色绑定（序列化为 JSON 数组，前端直接收到 [1,2,3]）
	CharacterIDs JSONUintSlice `json:"character_ids" gorm:"type:json"`

	// 摄像机方向与风格（JSON）
	CamDir ShotCamDir `json:"cam_dir" gorm:"column:cam_dir;serializer:json;type:text"`
	// AI生成元数据（JSON）
	GenMeta ShotGenMeta `json:"gen_meta" gorm:"column:gen_meta;serializer:json;type:text"`
	// 任务状态与时间轴（JSON）
	TaskMeta ShotTaskMeta `json:"task_meta" gorm:"column:shot_task_meta;serializer:json;type:text"`

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

	Text     string `json:"text" gorm:"type:text"`      // TTS 朗读文本（旁白或台词内容）
	Speaker  string `json:"speaker" gorm:"size:100"`    // 空串=旁白，"角色名"=对白
	Emotion  string `json:"emotion" gorm:"size:50"`     // 情绪标签（平静/温馨/激动等）
	Language string `json:"language" gorm:"size:20"`    // 方言/语言（空串=普通话；zh-yue=粤语；zh-scu=四川话；en=英语等）
	VoiceID  string `json:"voice_id" gorm:"size:100"`   // TTS 声音 ID（覆盖默认值）

	AudioPath    string  `json:"audio_path" gorm:"size:1000"`                   // 生成的音频文件路径/URL
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3);default:0"` // 音频时长（秒）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ShotVoiceSegment) TableName() string { return "ink_shot_voice_segment" }

// SFXPlayback 播放行为配置（JSON存储）
type SFXPlayback struct {
	FadeInMs  int `json:"fade_in_ms"`
	FadeOutMs int `json:"fade_out_ms"`
	PlayCount int `json:"play_count"`
}

// ShotSFXItem 分镜音效条目（一个分镜可包含多条独立音效）
type ShotSFXItem struct {
	ID     uint `json:"id" gorm:"primaryKey"`
	ShotID uint `json:"shot_id" gorm:"not null;index:idx_sfx_shot_seq,priority:1"`
	SeqNo  int  `json:"seq_no" gorm:"not null;default:1;index:idx_sfx_shot_seq,priority:2"`

	Tag          string  `json:"tag" gorm:"size:200"`                              // 音效搜索词
	URL          string  `json:"url" gorm:"size:1000"`                             // 音效文件 URL
	Volume       float64 `json:"volume" gorm:"type:decimal(4,2);default:0.4"`     // 混音音量
	Source       string  `json:"source" gorm:"size:20"`                            // local/freesound/elevenlabs
	Disabled     bool    `json:"disabled" gorm:"default:false"`                    // 禁用后不参与合成/预览
	StartOffset  float64 `json:"start_offset" gorm:"type:decimal(8,3);default:0"`  // 在分镜中的开始时间
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3);default:0"` // 音效时长

	// 音效分类
	SFXType     string `json:"sfx_type" gorm:"size:20;default:'action'"`
	LoopEnabled bool   `json:"loop_enabled" gorm:"default:false"`

	// 播放行为（JSON）
	Playback SFXPlayback `json:"playback" gorm:"column:sfx_playback;serializer:json;type:text"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ShotSFXItem) TableName() string { return "ink_shot_sfx_item" }

// BGMTrackMeta 音轨元数据（JSON存储）
type BGMTrackMeta struct {
	SearchQueries string `json:"search_queries"`
	TrackName     string `json:"track_name"`
	TrackArtist   string `json:"track_artist"`
	Source        string `json:"source"`
	Mood          string `json:"mood"`
	Tempo         string `json:"tempo"`
}

// BGMDucking 人声闪避配置（JSON存储）
type BGMDucking struct {
	Enabled bool    `json:"enabled"`
	Level   float64 `json:"level"`
}

// VideoBGMSegment 视频背景音乐分段（一段BGM可跨越多个分镜，AI智能分组）
type VideoBGMSegment struct {
	ID           uint    `json:"id" gorm:"primaryKey"`
	VideoID      uint    `json:"video_id" gorm:"index;index:idx_bgm_video_seq,priority:1;not null"`
	SeqNo        int     `json:"seq_no" gorm:"index:idx_bgm_video_seq,priority:2;not null;default:1"`
	StartShotNo  int     `json:"start_shot_no" gorm:"not null;default:1"`
	EndShotNo    int     `json:"end_shot_no" gorm:"not null;default:1"`
	URL          string  `json:"url" gorm:"size:1000"`
	Volume       float64 `json:"volume" gorm:"type:decimal(4,2);default:0.3"`
	DurationSecs float64 `json:"duration_secs" gorm:"type:decimal(8,3)"`
	Disabled     bool    `json:"disabled" gorm:"default:false"`

	// JSON 合并字段（减少列数）
	TrackMeta BGMTrackMeta `json:"track_meta" gorm:"column:bgm_track_meta;serializer:json;type:text"`
	Ducking   BGMDucking   `json:"ducking" gorm:"column:bgm_ducking;serializer:json;type:text"`

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
	SuggestedDialogue    string   `json:"suggested_dialogue,omitempty"`    // 建议对白（替换 dialogue 字段，同时清空 narration）
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
	NovelID          uint       `json:"novel_id" gorm:"index;not null;default:0"`
	EntityType       string     `json:"entity_type" gorm:"size:20;index:idx_review_entity,priority:1"` // "chapter" | "storyboard"
	EntityID         uint       `json:"entity_id" gorm:"index:idx_review_entity,priority:2"`           // chapter_id 或 video_id
	OverallScore     float64    `json:"overall_score"`
	ReviewJSON       string     `json:"-" gorm:"column:review_json;type:text"`   // ChapterReview 或 StoryboardReview JSON
	Status           string     `json:"status" gorm:"size:20;default:'pending'"` // pending|applied|rolled_back
	AppliedAt        *time.Time `json:"applied_at,omitempty"`
	AppliedDiffsJSON string     `json:"-" gorm:"column:applied_diffs;type:text"` // 已应用变更 JSON
	SnapshotJSON     string     `json:"-" gorm:"column:snapshot_json;type:text"` // 回滚快照 JSON
}

func (ReviewRecord) TableName() string { return "ink_review_record" }

// IgnoredReviewIssue 统一已忽略的审查问题（章节/分镜共用同一张表）
type IgnoredReviewIssue struct {
	gorm.Model
	NovelID     uint   `json:"novel_id" gorm:"index;not null;default:0"`
	EntityType  string `json:"entity_type" gorm:"size:20;index:idx_ignored_entity,priority:1;uniqueIndex:uniq_ignored_issue,priority:1"`
	EntityID    uint   `json:"entity_id" gorm:"index:idx_ignored_entity,priority:2;uniqueIndex:uniq_ignored_issue,priority:2"`
	IssueText   string `json:"issue_text" gorm:"type:text"`
	IssueHash   string `json:"issue_hash,omitempty" gorm:"size:64;index;uniqueIndex:uniq_ignored_issue,priority:3"`
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
	Dialogue    string  `json:"dialogue,omitempty"` // 对白镜头：与 narration 互斥，同时出现时 dialogue 优先
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
	DialogueScore     float64                `json:"dialogue_score"`      // 台词质量（潜台词/戏剧功能）
	ArcScore          float64                `json:"arc_score"`           // 情感节奏曲线完整性
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
	Index             int      `json:"index"`              // 段落序号(0起)
	OrigText          string   `json:"orig_text"`          // 原文摘要(前80字)
	Issues            []string `json:"issues"`
	Suggestion        string   `json:"suggestion"`
	Action            string   `json:"action"`             // "rewrite" | "delete" | "restructure"
	SuggestedRewrite  string   `json:"suggested_rewrite"`  // 改写后正文；action=delete 时为空
	Severity          string   `json:"severity"`           // info/warning/error
	NarrativeImpact   string   `json:"narrative_impact"`   // "plot_critical"|"quality"|"style"
	PreservedFunction string   `json:"preserved_function"` // restructure/delete 时说明原文叙事功能
}

// WeaknessItem 不足条目，带对应改进建议
type WeaknessItem struct {
	Issue      string `json:"issue"`      // 问题描述
	Suggestion string `json:"suggestion"` // 对应改进建议
}

// SceneAnalysisItem 场景层 3C 结构分析（嵌入 JSON）
type SceneAnalysisItem struct {
	SceneNo    int    `json:"scene_no"`
	StartIndex int    `json:"start_index"` // 场景起始段落序号
	EndIndex   int    `json:"end_index"`   // 场景结束段落序号（含）
	Goal       string `json:"goal"`        // 场景目标
	Conflict   string `json:"conflict"`    // 场景冲突/阻力
	Change     string `json:"change"`      // 场景结束后的变化
	C3Score    int    `json:"c3_score"`    // 3C 结构完整度 0-100
	Note       string `json:"note"`        // 编辑意见（可选）
}

// HookAnalysis 章末钩子专项评估（嵌入 JSON）
type HookAnalysis struct {
	Type             string `json:"type"`               // cliffhanger|emotional|action|reversal|none
	Strength         int    `json:"strength"`            // 0-100
	HookText         string `json:"hook_text"`           // 章末关键句原文引用
	NextChapterSetup string `json:"next_chapter_setup"` // 读者最想知道的问题
}

// ChapterReview AI章节审查报告
type ChapterReview struct {
	OverallScore       float64             `json:"overall_score"`
	NarrativeScore     float64             `json:"narrative_score"`
	CharacterScore     float64             `json:"character_score"`
	WritingScore       float64             `json:"writing_score"`
	PacingScore        float64             `json:"pacing_score"`
	DramaticScore      float64             `json:"dramatic_score"`       // 戏剧张力
	NarrativeNecessity float64             `json:"narrative_necessity"`  // 叙事必要性
	EmotionalResonance float64             `json:"emotional_resonance"`  // 情感共鸣
	VisualPotential    float64             `json:"visual_potential"`     // 画面感/可视化潜力
	Summary            string              `json:"summary"`
	Strengths          []string            `json:"strengths"`
	Weaknesses         []WeaknessItem      `json:"weaknesses"`
	GlobalSuggestions  []string            `json:"global_suggestions"`
	HookAnalysis       *HookAnalysis       `json:"hook_analysis,omitempty"`
	SceneAnalysis      []SceneAnalysisItem `json:"scene_analysis,omitempty"`
	ParagraphFeedback  []ParagraphFeedback `json:"paragraph_feedback"`
	RecordID           uint                `json:"record_id,omitempty"`
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
	Description  string `json:"description"`
	Tags         string `json:"tags"`
	Resolution   string `json:"resolution"`
	FrameRate    int    `json:"frame_rate"`
	AspectRatio  string `json:"aspect_ratio"`
	ArtStyle     string `json:"art_style"`
	Mode         string `json:"mode"`           // video/slideshow
	VisualMode   string `json:"visual_mode"`    // standard/hd/3d/hd_3d
	ThreeDStyle  string `json:"three_d_style"`  // cg/pixar/anime3d/realistic3d
	QualityTier  string `json:"quality_tier"`   // draft/preview/final/production
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
	Force       bool   `json:"force"`        // true = regenerate even if image already exists
	Sequential  bool   `json:"sequential"`   // true = 顺序生成（I2V 链接保障），慢但连贯
	VoiceFirst  bool   `json:"voice_first"`  // true = 先生成 TTS，以配音时长决定视频时长，保证声画同步
}
