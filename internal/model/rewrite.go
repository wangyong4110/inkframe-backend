package model

import "time"

// RewriteProject represents a novel rewriting project
type RewriteProject struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	TenantID      uint      `json:"tenant_id" gorm:"index;not null;default:0"`
	NovelID       uint      `json:"novel_id" gorm:"index"`
	Name          string    `json:"name" gorm:"size:200"`
	Level         int       `json:"level" gorm:"default:1"` // 1=字词润色 2=文学精炼 3=情节调整 4=结构重构 5=精神蒸馏
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
	ID                 uint      `json:"id" gorm:"primaryKey"`
	ProjectID          uint      `json:"project_id" gorm:"uniqueIndex"`
	VoiceFingerprint   string    `json:"voice_fingerprint" gorm:"type:text"`   // JSON: narrator_style, sentence_patterns, distinctive_phrases
	SceneArchitecture  string    `json:"scene_architecture" gorm:"type:text"`  // JSON: typical_scene_structure, pacing, transitions
	CharacterPsych     string    `json:"character_psych" gorm:"type:text"`     // JSON: character archetypes, relationship topology
	ThemeCore          string    `json:"theme_core" gorm:"type:text"`          // JSON: themes, motifs, symbols
	WorldLogic         string    `json:"world_logic" gorm:"type:text"`         // JSON: world rules, power systems, geography
	HighRiskMarkers    string    `json:"high_risk_markers" gorm:"type:text"`   // JSON: unique phrases, plot sequences, signature elements
	RhythmPattern      string    `json:"rhythm_pattern" gorm:"type:text"`      // JSON: emotional peak interval, chapter-end hook style
	ImagerySystem      string    `json:"imagery_system" gorm:"type:text"`      // JSON: core symbols and their recurrence pattern
	InterChapterHooks  string    `json:"inter_chapter_hooks" gorm:"type:text"` // JSON: foreshadow style, avg reveal distance in chapters
	CreatedAt          time.Time `json:"created_at"`
}

func (LiteraryAnalysis) TableName() string { return "ink_literary_analysis" }

// RewriteBible is the strategic rewriting guide
type RewriteBible struct {
	ID                 uint      `json:"id" gorm:"primaryKey"`
	ProjectID          uint      `json:"project_id" gorm:"uniqueIndex"`
	NewWorldName       string    `json:"new_world_name" gorm:"size:200"`
	NewCharNames       string    `json:"new_char_names" gorm:"type:text"`       // JSON: map[oldName]newName (proper names only)
	NamingStyle        string    `json:"naming_style" gorm:"type:text"`         // cultural/naming convention guide for consistency
	PlotTransform      string    `json:"plot_transform" gorm:"type:text"`       // JSON: transformation rules for main plot beats
	PropsTransform     string    `json:"props_transform" gorm:"type:text"`      // JSON: map[originalProp]newProp for iconic items
	VoiceStrategy      string    `json:"voice_strategy" gorm:"type:text"`       // JSON: narrator voice guidelines
	StyleGuide         string    `json:"style_guide" gorm:"type:text"`          // JSON: sentence structure, vocabulary register, pacing rules
	ForbiddenElems     string    `json:"forbidden_elems" gorm:"type:text"`      // JSON: legacy mixed-type array (kept for backward compat)
	ForbiddenPhrases   string    `json:"forbidden_phrases" gorm:"type:text"`    // JSON: []string — phrases strictly forbidden in output
	ForbiddenDialogues string    `json:"forbidden_dialogues" gorm:"type:text"`  // JSON: []ForbiddenDialogue — signature dialogues with rewrite guides
	ImageryTransform   string    `json:"imagery_transform" gorm:"type:text"`    // JSON: map[originalSymbol]newSymbol for core imagery
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (RewriteBible) TableName() string { return "ink_rewrite_bible" }

// ChapterRewriteTask tracks rewriting progress per chapter
type ChapterRewriteTask struct {
	ID                uint      `json:"id" gorm:"primaryKey"`
	ProjectID         uint      `json:"project_id" gorm:"index"`
	ChapterID         uint      `json:"chapter_id" gorm:"index"`
	ChapterNo         int       `json:"chapter_no"`
	Status            string    `json:"status" gorm:"size:20;default:'pending'"` // pending, rewriting, reviewing, completed, failed
	OriginalContent   string    `json:"original_content" gorm:"type:longtext"`
	AttemptContent    string    `json:"attempt_content" gorm:"type:longtext"`  // in-flight result; not shown until accepted
	RewrittenContent  string    `json:"rewritten_content" gorm:"type:longtext"` // accepted final result
	SimilarityScore   float64   `json:"similarity_score" gorm:"default:0"`
	LexicalSim        float64   `json:"lexical_sim" gorm:"default:0"`
	SemanticSim       float64   `json:"semantic_sim" gorm:"default:0"`
	StructuralSim     float64   `json:"structural_sim" gorm:"default:0"`
	Passed            bool      `json:"passed" gorm:"default:false"`
	RetryCount        int       `json:"retry_count" gorm:"default:0"`
	ErrorMsg          string    `json:"error_msg" gorm:"size:500"`
	QualityScore      float64   `json:"quality_score" gorm:"default:0"`
	DeaiApplied       bool      `json:"deai_applied" gorm:"default:false"`
	ConsistencyIssues string    `json:"consistency_issues" gorm:"size:1000"`
	SummaryWritten    bool      `json:"summary_written" gorm:"default:false"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (ChapterRewriteTask) TableName() string { return "ink_chapter_rewrite_task" }

// RewriteContinuityIndex tracks all confirmed entity replacements across chapters.
// Built incrementally: Bible names are pre-seeded, new names discovered per chapter are appended.
type RewriteContinuityIndex struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	ProjectID  uint      `json:"project_id" gorm:"uniqueIndex:idx_rci_proj_key"`
	EntityKey  string    `json:"entity_key" gorm:"size:200;uniqueIndex:idx_rci_proj_key"` // original name/phrase
	EntityType string    `json:"entity_type" gorm:"size:30"`                              // char | location | prop | concept
	NewName    string    `json:"new_name" gorm:"size:200"`                                // replacement in the rewritten world
	FirstSeen  int       `json:"first_seen"`                                              // chapter_no where first confirmed
	UpdatedAt  time.Time `json:"updated_at"`
}

func (RewriteContinuityIndex) TableName() string { return "ink_rewrite_continuity_index" }

// RewriteChapterSummary stores an AI-generated semantic summary of each completed chapter.
// Used as rolling context for subsequent chapters (richer than opening/closing excerpts).
type RewriteChapterSummary struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	ProjectID     uint      `json:"project_id" gorm:"uniqueIndex:idx_rcs_proj_ch"`
	ChapterNo     int       `json:"chapter_no" gorm:"uniqueIndex:idx_rcs_proj_ch"`
	Summary       string    `json:"summary" gorm:"type:text"`      // 100-150 char semantic summary
	CharStateSnap string    `json:"char_state_snap" gorm:"type:text"` // JSON map[charName]stateDesc at chapter end
	CreatedAt     time.Time `json:"created_at"`
}

func (RewriteChapterSummary) TableName() string { return "ink_rewrite_chapter_summary" }
