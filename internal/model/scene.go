package model

import (
	"time"

	"gorm.io/gorm"
)

// PlotPoint 剧情点
type PlotPoint struct {
	ID      uint     `json:"id" gorm:"primaryKey"`
	NovelID uint     `json:"novel_id" gorm:"index:idx_plotpoint_novel_resolved,priority:1;index;not null"`
	ChapterID uint     `json:"chapter_id" gorm:"index;not null"`
	Chapter   *Chapter `json:"chapter,omitempty" gorm:"foreignKey:ChapterID"`
	Type      string   `json:"type" gorm:"size:50"`
	// conflict=冲突, climax=高潮, resolution=解决, twist=转折, foreshadow=伏笔

	Description string `json:"description" gorm:"type:text"`
	Characters  string `json:"characters" gorm:"type:text"` // JSON数组
	Locations   string `json:"locations" gorm:"type:text"`  // JSON数组

	IsResolved bool  `json:"is_resolved" gorm:"index:idx_plotpoint_novel_resolved,priority:2;default:false"`
	ResolvedIn *uint `json:"resolved_in"` // 解决这一剧情点的章节ID

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (PlotPoint) TableName() string {
	return "ink_plot_point"
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

// HookChain 钩子链（章末悬念/情感/谜题/威胁/承诺）
type HookChain struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	NovelID uint `json:"novel_id" gorm:"index;index:idx_hook_novel_fulfilled,priority:1;not null"`

	Type        string `json:"type" gorm:"size:50;not null"`
	// chapter_end/emotional/mystery/threat/promise/revelation/decision/action
	Description     string `json:"description" gorm:"type:text;not null"`
	PlantedAt       int    `json:"planted_at" gorm:"not null"`                // 埋下章节号
	PlannedPayoffAt int    `json:"planned_payoff_at" gorm:"default:0"`        // 计划兑现章节号（0=未规划）
	ActualPayoffAt  int    `json:"actual_payoff_at" gorm:"default:0"`         // 实际兑现章节号
	Intensity       int    `json:"intensity" gorm:"not null;default:5"`       // 1-10
	IsFulfilled     bool   `json:"is_fulfilled" gorm:"index:idx_hook_novel_fulfilled,priority:2;default:false"`
	Notes           string `json:"notes" gorm:"type:text"`
	PayoffQuality   int    `json:"payoff_quality" gorm:"default:0"` // 1-5兑现质量评分
	PayoffNotes     string `json:"payoff_notes" gorm:"type:text"`   // 兑现质量说明

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (HookChain) TableName() string { return "ink_hook_chain" }

// SatisfactionPoint 爽点（打脸/突破/揭秘/重逢/复仇/认可/其他）
type SatisfactionPoint struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	NovelID uint `json:"novel_id" gorm:"index;not null"`

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
	ID      uint `json:"id" gorm:"primaryKey"`
	NovelID uint `json:"novel_id" gorm:"index;not null"`

	Title        string `json:"title" gorm:"size:255;not null"`
	Type         string `json:"type" gorm:"size:50;not null"`
	// internal/interpersonal/social
	Description  string `json:"description" gorm:"type:text"`
	Antagonist   string `json:"antagonist" gorm:"size:255"`
	StartChapter int    `json:"start_chapter" gorm:"default:0"`
	PeakChapter  int    `json:"peak_chapter" gorm:"default:0"`  // 预计高潮章节
	EndChapter   int    `json:"end_chapter" gorm:"default:0"`   // 预计解决章节（0=未规划）
	CurrentPhase string `json:"current_phase" gorm:"size:30;default:setup"`
	// setup/ignition/escalation/turning_point/climax/aftershock (三幕六阶段)
	IsResolved    bool   `json:"is_resolved" gorm:"default:false"`
	Notes         string `json:"notes" gorm:"type:text"`
	TensionLevels string `json:"tension_levels" gorm:"type:text"` // JSON: {"setup":3,"ignition":6,...} 各阶段张力值1-10

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ConflictArc) TableName() string { return "ink_conflict_arc" }

// SceneAnchor 场景锚点（固定命名场景的视觉描述，确保分镜跨镜头布景一致）
type SceneAnchor struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	NovelID uint `json:"novel_id" gorm:"uniqueIndex:idx_scene_anchor_novel_name;index;not null"`

	Name        string `json:"name" gorm:"size:255;not null;uniqueIndex:idx_scene_anchor_novel_name"`
	Description string `json:"description" gorm:"type:text"`
	RefImageURL string `json:"ref_image_url" gorm:"size:1000"` // 首次生成后存参考图URL

	// 扩展字段（一致性评分相关）
	RefImageLockedAt *time.Time `json:"ref_image_locked_at,omitempty" gorm:"index"`
	AvgConsScore     float64    `json:"avg_cons_score" gorm:"type:decimal(4,3);default:0"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (SceneAnchor) TableName() string { return "ink_scene_anchor" }

// ChapterSceneAnchor 章节与场景锚点的绑定关系
type ChapterSceneAnchor struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	ChapterID     uint      `gorm:"uniqueIndex:idx_chapter_scene_anchor;not null" json:"chapter_id"`
	SceneAnchorID uint      `gorm:"uniqueIndex:idx_chapter_scene_anchor;not null" json:"scene_anchor_id"`
	NovelID       uint      `gorm:"index;not null" json:"novel_id"`
	CreatedAt     time.Time `json:"created_at"`
}

func (ChapterSceneAnchor) TableName() string { return "ink_chapter_scene_anchor" }

// ScreenplayScene 分场剧本：一章拆分为多场，每场再拆分为多个分镜（StoryboardShot.ScreenplaySceneID）。
// 分场依据：地点变化 / 时间跳跃 / POV 切换，而非按字数硬切。
type ScreenplayScene struct {
	ID      uint `json:"id" gorm:"primaryKey"`
	ChapterID uint `json:"chapter_id" gorm:"index:idx_screenplay_scene_chapter_no,priority:1;not null"`
	NovelID   uint `json:"novel_id" gorm:"index;not null"`

	SceneNo int    `json:"scene_no" gorm:"index:idx_screenplay_scene_chapter_no,priority:2;not null"` // 本章内第几场，从1开始
	Heading string `json:"heading" gorm:"size:255"`                                                   // slugline，如"内景·客厅·日"

	SceneAnchorID *uint `json:"scene_anchor_id,omitempty" gorm:"index"` // 关联现有场景锚点，保证地点视觉一致性

	Synopsis      string `json:"synopsis" gorm:"type:text"`
	EmotionalTone string `json:"emotional_tone" gorm:"size:100"`

	// 本场内的叙事节拍：纯文本，每行一条；对话行格式为"角色名：台词"，其余行为动作/描写
	// （原为结构化 []ScreenplayBeat 数组，改为纯文本以匹配前端合并单文本框编辑的方式）
	Beats string `json:"beats" gorm:"column:beats;type:text"`

	EstimatedShotCount int `json:"estimated_shot_count" gorm:"default:0"` // 分镜生成时按场次镜头数规划，替代按章节字数估算

	// 人工审校确认后锁定：重新生成分镜时跳过本场剧本内容的重新生成，只重跑分镜
	Locked bool `json:"locked" gorm:"default:false"`

	// 是否被人工编辑过：一旦用户编辑过 heading/synopsis/beats 就标记为 true（供前端展示"已编辑"提示；
	// 覆盖保护仍以 Locked 为准，"重新生成剧本"会覆盖未锁定场次，即使已被编辑过）。
	Edited bool `json:"edited" gorm:"default:false"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ScreenplayScene) TableName() string { return "ink_screenplay_scene" }

// ScreenplaySceneVersion 分场剧本历史快照：每次"生成剧本"覆盖某场次前，把该场次覆盖前的完整
// 内容存一条版本记录，供用户在"历史版本"里查看/恢复（Content 是该场次覆盖前字段的 JSON 快照）。
type ScreenplaySceneVersion struct {
	ID                uint      `json:"id" gorm:"primaryKey"`
	ScreenplaySceneID uint      `json:"screenplay_scene_id" gorm:"uniqueIndex:idx_scene_version,priority:1;not null"`
	ChapterID         uint      `json:"chapter_id" gorm:"index;not null"`
	NovelID           uint      `json:"novel_id" gorm:"index;not null"`
	VersionNo         int       `json:"version_no" gorm:"uniqueIndex:idx_scene_version,priority:2;not null"`
	Content           string    `json:"content" gorm:"type:json"`
	ChangeType        string    `json:"change_type" gorm:"size:50"`
	CreatedAt         time.Time `json:"created_at"`
}

func (ScreenplaySceneVersion) TableName() string { return "ink_screenplay_scene_version" }

// SceneConsistencyLog 场景一致性评分日志
type SceneConsistencyLog struct {
	ID       uint `gorm:"primaryKey" json:"id"`
	NovelID  uint `gorm:"index;not null;default:0" json:"novel_id"`
	ShotID   uint `gorm:"index;not null" json:"shot_id"`
	AnchorID uint `gorm:"index;not null" json:"anchor_id"`
	Attempt      int     `json:"attempt"`
	OverallScore float64 `gorm:"type:decimal(4,3)" json:"overall_score"`
	ArchScore    float64 `gorm:"type:decimal(4,3)" json:"arch_score"`
	LightScore   float64 `gorm:"type:decimal(4,3)" json:"light_score"`
	AtmoScore    float64 `gorm:"type:decimal(4,3)" json:"atmo_score"`
	PropScore    float64 `gorm:"type:decimal(4,3)" json:"prop_score"`
	TimeScore    float64 `gorm:"type:decimal(4,3);default:1" json:"time_score"`
	Issues       string  `gorm:"type:json" json:"issues"`
	// SuggestedFix 由 LLM 生成的下次图像生成 prompt 修正关键词（供重试时使用）
	SuggestedFix string  `gorm:"type:text" json:"suggested_fix,omitempty"`
	IPWeight     float64 `json:"ip_weight"`
	Passed       bool    `json:"passed"`
	CreatedAt    time.Time `json:"created_at"`
}

func (SceneConsistencyLog) TableName() string { return "ink_scene_consistency_log" }
