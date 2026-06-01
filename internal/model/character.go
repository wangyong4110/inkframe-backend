package model

import (
	"time"

	"gorm.io/gorm"
)

// Character 角色
type Character struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	NovelID  uint   `json:"novel_id" gorm:"index;index:idx_char_novel_tenant,priority:1;not null"`
	TenantID uint   `json:"tenant_id" gorm:"index;index:idx_char_novel_tenant,priority:2;not null;default:1"`
	Novel    *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name string `json:"name" gorm:"size:100;not null"`
	Role string `json:"role" gorm:"size:50"` // protagonist/antagonist/supporting/minor

	// 统一描述字段（外貌、性格、背景、对话风格等所有描述性信息）
	Description string `json:"description" gorm:"type:text"`

	// 人物深层动机（驱动角色行为的内在逻辑，用于生成更立体的角色表现）
	InnerConflict string `json:"inner_conflict,omitempty" gorm:"column:inner_conflict;type:text"` // 内在矛盾/恐惧（如：渴望自由却害怕失去家人）
	CoreDesire    string `json:"core_desire,omitempty" gorm:"column:core_desire;type:text"`    // 核心渴望（如：被认可、复仇、保护所爱之人）

	// 角色弧光设计（规划角色在全书中的心理成长阶段）
	// JSON格式: [{"stage":"起点","desc":"...","target_range":[1,20]},{"stage":"考验","desc":"...","target_range":[21,60]},...]
	// stage可选: 起点/考验/最低点/转折/终点
	ArcDesign       string `json:"arc_design,omitempty" gorm:"type:text"`
	CurrentArcStage string `json:"current_arc_stage,omitempty" gorm:"size:50"` // 当前所处弧光阶段（自动更新）

	// AI 图像生成英文提示词（由 extract_characters 生成，用于三视图/头像生成）
	VisualPrompt string `json:"visual_prompt" gorm:"type:text"`

	// 三视图合一参考图（一张图展示正/侧/背三个视角）
	ThreeViewSheet string `json:"three_view_sheet" gorm:"column:three_view_front;size:1000"`

	// 面部特写图
	FaceCloseup string `json:"face_closeup" gorm:"size:1000"`

	// 头像
	Portrait string `json:"portrait" gorm:"size:1000"`

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

// Item 物品（项目级别，贯穿整部小说）
type Item struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	NovelID uint   `json:"novel_id" gorm:"index;not null"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name string `json:"name" gorm:"size:100;not null"`

	Description string `json:"description" gorm:"type:text"` // 统一描述（含类别、外观等所有描述性信息）
	Location    string `json:"location" gorm:"size:200"`      // 当前/最后已知位置
	Owner    string `json:"owner" gorm:"size:100"`    // 当前持有者

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

// ─── Character / Item / ChapterCharacter DTOs ─────────────────────────────────

type CreateCharacterRequest struct {
	Name        string `json:"name" binding:"required"`
	Role        string `json:"role"`
	Description string `json:"description"`
}

type UpdateCharacterRequest struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Description   string `json:"description"`
	InnerConflict   string `json:"inner_conflict"`    // 内在矛盾（如：渴望自由却害怕失去家人）
	CoreDesire      string `json:"core_desire"`       // 核心渴望（如：被认可、复仇、保护所爱之人）
	ArcDesign       string `json:"arc_design"`        // 弧光设计 JSON（各阶段描述+章节范围）
	CurrentArcStage string `json:"current_arc_stage"` // 当前弧光阶段（起点/考验/最低点/转折/终点）
	VisualPrompt    string `json:"visual_prompt"`     // AI 图像生成英文提示词
	ThreeViewSheet string   `json:"three_view_sheet"`
	FaceCloseup    string   `json:"face_closeup"`
	Portrait       string   `json:"portrait"`
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

type CreateItemRequest struct {
	Name         string `json:"name" binding:"required"`
	Description  string `json:"description"`
	Location     string `json:"location"`
	Owner        string `json:"owner"`
	VisualPrompt string `json:"visual_prompt"`
	Status       string `json:"status"`
}

type UpdateItemRequest struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	Location          string `json:"location"`
	Owner             string `json:"owner"`
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

type UpsertChapterCharacterRequest struct {
	Appearance  string `json:"appearance"`
	Personality string `json:"personality"`
	Status      string `json:"status"`
	Location    string `json:"location"`
	Notes       string `json:"notes"`
}
