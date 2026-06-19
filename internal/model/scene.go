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

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// KnowledgeBase 知识库
type KnowledgeBase struct {
	ID   uint   `json:"id" gorm:"primaryKey"`
	Type string `json:"type" gorm:"size:50;index;index:idx_kb_novel_type,priority:2"`
	// character_fact=角色事实, lore=世界观知识, writing_technique=写作技巧

	Title   string `json:"title" gorm:"size:255;not null"`
	Content string `json:"content" gorm:"type:text"`

	Tags string `json:"tags" gorm:"type:text"` // JSON数组

	// 关联
	NovelID         *uint           `json:"novel_id,omitempty" gorm:"index;index:idx_kb_novel_type,priority:1"`
	Novel           *Novel          `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	SourceChapterID *uint           `json:"source_chapter_id,omitempty" gorm:"index"`
	ReferenceID     *uint           `json:"reference_id,omitempty" gorm:"index"`
	Reference       *ReferenceNovel `json:"reference,omitempty" gorm:"foreignKey:ReferenceID"`

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
	ForeshadowID    *uint  `json:"foreshadow_id,omitempty" gorm:"index"`      // 关联伏笔 ID
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
	Type        string `json:"type" gorm:"size:50"` // interior/exterior/imaginary
	Description string `json:"description" gorm:"type:text"`
	PromptLock  string `json:"prompt_lock" gorm:"type:text"`   // 锁定关键词（逗号分隔，含风格/光照等）
	RefImageURL string `json:"ref_image_url" gorm:"size:1000"` // 首次生成后存参考图URL

	// 扩展字段（一致性评分相关）
	RefImageLockedAt *time.Time `json:"ref_image_locked_at,omitempty" gorm:"index"`
	UsageCount       int        `json:"usage_count" gorm:"default:0"`
	AvgConsScore     float64    `json:"avg_cons_score" gorm:"type:decimal(4,3);default:0"`
	ParentAnchorID   *uint      `json:"parent_anchor_id,omitempty" gorm:"index"`
	Variant          string     `json:"variant" gorm:"size:50"` // day/night/winter/battle

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
	TimeScore    float64 `gorm:"type:decimal(4,3);default:1" json:"time_score"`
	Issues       string  `gorm:"type:json" json:"issues"`
	// SuggestedFix 由 LLM 生成的下次图像生成 prompt 修正关键词（供重试时使用）
	SuggestedFix string  `gorm:"type:text" json:"suggested_fix,omitempty"`
	IPWeight     float64 `json:"ip_weight"`
	Passed       bool    `json:"passed"`
	CreatedAt    time.Time `json:"created_at"`
}

func (SceneConsistencyLog) TableName() string { return "ink_scene_consistency_log" }
