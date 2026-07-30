package model

import (
	"time"

	"gorm.io/gorm"
)

// CharacterVoiceConfig 配音配置（JSON存储）
type CharacterVoiceConfig struct {
	VoiceID       string  `json:"voice_id"`
	VoiceModel    string  `json:"voice_model,omitempty"` // provider 名称，如 "doubao-speech" / "qianwen"
	VoiceSpeed    float64 `json:"voice_speed"`
	VoiceStyle    string  `json:"voice_style"`
	VoiceLanguage string  `json:"voice_language"`
	VoiceSample   string  `json:"voice_sample"`
	VoiceProfile  string  `json:"voice_profile"`
}

// CharacterMeta 角色基本属性（JSON存储）
type CharacterMeta struct {
	Gender           string `json:"gender"`
	Age              string `json:"age"`
	InnerConflict    string `json:"inner_conflict"`
	CoreDesire       string `json:"core_desire"`
	AppearancePrompt string `json:"appearance_prompt"`
}

// Character 角色
type Character struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	NovelID uint   `json:"novel_id" gorm:"index;not null;uniqueIndex:uniq_char_novel_name,priority:1"`
	Novel   *Novel `json:"novel,omitempty" gorm:"foreignKey:NovelID"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name string `json:"name" gorm:"size:100;not null;uniqueIndex:uniq_char_novel_name,priority:2"`
	Role string `json:"role" gorm:"size:50"` // protagonist/antagonist/supporting/minor

	// 统一描述字段（外貌、性格、背景、对话风格等所有描述性信息）
	Description string `json:"description" gorm:"type:text"`

	// JSON 合并字段（减少列数）
	VoiceConfig CharacterVoiceConfig `json:"voice_config" gorm:"column:voice_config;serializer:json;type:text"`
	Meta        CharacterMeta        `json:"meta" gorm:"column:character_meta;serializer:json;type:text"`

	// 默认形象 ID（指向 ink_character_look 主键；0 表示未设置）
	DefaultLookID uint `json:"default_look_id" gorm:"default:0"`

	// 默认形象完整对象（虚字段，不存库，由服务层批量注入）
	DefaultLook *CharacterLook `json:"default_look,omitempty" gorm:"-"`

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

// CharacterStateSnapshot 角色状态快照
type CharacterStateSnapshot struct {
	ID          uint `json:"id" gorm:"primaryKey"`
	NovelID     uint `json:"novel_id" gorm:"index;not null;default:0"`
	CharacterID uint `json:"character_id" gorm:"index;not null;uniqueIndex:uniq_snapshot_char_chapter,priority:1"`
	ChapterID   uint `json:"chapter_id" gorm:"index;uniqueIndex:uniq_snapshot_char_chapter,priority:2"`

	Health     string `json:"health" gorm:"size:50"` // healthy, injured, critical
	PowerLevel int    `json:"power_level"`
	Mood       string `json:"mood" gorm:"size:50"`
	Motivation string `json:"motivation" gorm:"size:200"`
	Location   string `json:"location" gorm:"size:200"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (CharacterStateSnapshot) TableName() string {
	return "ink_character_state_snapshot"
}

// Item 道具（项目级别，贯穿整部小说）
type Item struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	NovelID uint   `json:"novel_id" gorm:"index;not null;uniqueIndex:uniq_item_novel_name,priority:1"`
	UUID    string `json:"uuid" gorm:"uniqueIndex;size:36"`

	Name string `json:"name" gorm:"size:100;not null;uniqueIndex:uniq_item_novel_name,priority:2"`

	Location string `json:"location" gorm:"size:200"` // 当前/最后已知位置
	Owner    string `json:"owner" gorm:"size:100"`     // 当前持有者

	ImageURL          string `json:"image_url" gorm:"size:1000"`
	VisualPrompt      string `json:"visual_prompt" gorm:"type:text"`       // 用于 AI 图像生成的英文提示词
	ReferenceImageURL string `json:"reference_image_url" gorm:"size:1000"` // 参考图 URL（已上传到 OSS）

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (Item) TableName() string { return "ink_item" }

// ChapterItem 章节级别的道具状态（覆盖项目级别）
type ChapterItem struct {
	ID        uint `json:"id" gorm:"primaryKey"`
	ItemID    uint `json:"item_id" gorm:"uniqueIndex:uniq_chapter_item;not null"`
	ChapterID uint `json:"chapter_id" gorm:"uniqueIndex:uniq_chapter_item;not null"`
	NovelID   uint `json:"novel_id" gorm:"index;not null"`

	Location  string `json:"location" gorm:"size:200"` // 本章节中道具所在位置（覆盖项目级）
	Owner     string `json:"owner" gorm:"size:100"`    // 本章节中持有者（覆盖项目级）
	Condition string `json:"condition" gorm:"size:50"` // intact/damaged/broken/destroyed
	Notes     string `json:"notes" gorm:"type:text"`   // 本章节备注

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
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

	// 从 ink_character_appearance 迁入：出场信息
	RoleInChapter string `json:"role_in_chapter" gorm:"size:50"` // main/supporting/mentioned
	Action        string `json:"action" gorm:"type:text"`        // 本章动作
	Change        string `json:"change" gorm:"type:text"`        // 本章变化

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ChapterCharacter) TableName() string { return "ink_chapter_character" }

// CharacterLook 角色形象。每个角色可以有多个形象记录，通过 Character.DefaultLookID 指定当前使用的形象。
type CharacterLook struct {
	ID          uint `json:"id" gorm:"primaryKey"`
	CharacterID uint `json:"character_id" gorm:"index:idx_look_char_novel,priority:1;not null"`
	NovelID     uint `json:"novel_id" gorm:"index:idx_look_char_novel,priority:2;not null"`

	Label string `json:"label" gorm:"size:100"` // 形象名称，如"少年时期""伪装成书生""觉醒后"

	// 外观描述（中文，供用户阅读和编辑）
	Description string `json:"description" gorm:"type:text"`
	// AI 图像生成提示词：完整外观（含服装/鞋履/配饰/姿态），用于三视图生成
	VisualPrompt string `json:"visual_prompt" gorm:"type:text"`

	// 该形象的参考图像：正面/侧面/背面/面部特写合图
	ThreeViewSheet string `json:"three_view_sheet" gorm:"size:1000"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (CharacterLook) TableName() string { return "ink_character_look" }

// ─── Character / Item / ChapterCharacter DTOs ─────────────────────────────────

type CreateCharacterRequest struct {
	Name        string `json:"name" binding:"required"`
	Role        string `json:"role"`
	Gender      string `json:"gender"`
	Age         string `json:"age"`
	Description string `json:"description"`
}

type UpdateCharacterRequest struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Gender        string `json:"gender"`
	Age           string `json:"age"`
	Description   string `json:"description"`
	InnerConflict string `json:"inner_conflict"` // 内在矛盾（如：渴望自由却害怕失去家人）
	CoreDesire    string `json:"core_desire"`    // 核心渴望（如：被认可、复仇、保护所爱之人）
	// 配音设置
	VoiceID       string   `json:"voice_id"`
	VoiceModel    string   `json:"voice_model"`      // provider 名称，空=不更新
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
	Location     string `json:"location"`
	Owner        string `json:"owner"`
	VisualPrompt string `json:"visual_prompt"`
}

type UpdateItemRequest struct {
	Name              string `json:"name"`
	Location          string `json:"location"`
	Owner             string `json:"owner"`
	VisualPrompt      string `json:"visual_prompt"`
	ImageURL          string `json:"image_url"`
	ReferenceImageURL string `json:"reference_image_url"`
}

type UpsertChapterItemRequest struct {
	Location  string `json:"location"`
	Owner     string `json:"owner"`
	Condition string `json:"condition"`
	Notes     string `json:"notes"`
}

type UpsertChapterCharacterRequest struct {
	Appearance    string `json:"appearance"`
	Personality   string `json:"personality"`
	Status        string `json:"status"`
	Location      string `json:"location"`
	Notes         string `json:"notes"`
	RoleInChapter string `json:"role_in_chapter"` // main/supporting/mentioned
	Action        string `json:"action"`          // 本章动作
	Change        string `json:"change"`          // 本章变化
}

// CreateCharacterLookRequest 创建角色形象请求
type CreateCharacterLookRequest struct {
	Label          string `json:"label"`
	SetAsDefault   bool   `json:"set_as_default"` // 是否将此形象设为默认
	Description    string `json:"description"`
	VisualPrompt   string `json:"visual_prompt"`
	ThreeViewSheet string `json:"three_view_sheet"`
}

// UpdateCharacterLookRequest 更新角色形象请求
type UpdateCharacterLookRequest struct {
	Label          *string `json:"label"`
	SetAsDefault   *bool   `json:"set_as_default"` // 是否将此形象设为默认
	Description    *string `json:"description"`
	VisualPrompt   *string `json:"visual_prompt"`
	ThreeViewSheet *string `json:"three_view_sheet"`
}
