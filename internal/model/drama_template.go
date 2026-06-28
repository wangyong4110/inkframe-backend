package model

import "time"

// DramaTemplate 短剧爆款类型模板
// 内置4种经典类型（霸总逆袭、替嫁甜宠、重生复仇、赘婿觉醒），支持用户自定义扩展。
type DramaTemplate struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	Name        string `json:"name" gorm:"size:100;not null;uniqueIndex"`
	Genre       string `json:"genre" gorm:"size:50"`      // 都市/古风/穿越/现代
	CoreHook    string `json:"core_hook" gorm:"size:200"` // 核心钩子：如"身份落差→认知反转"
	Description string `json:"description" gorm:"type:text"`

	// 三幕六转折骨架（JSON）
	ThreeActBeats string `json:"three_act_beats" gorm:"type:text"`

	// 角色原型建议（JSON: {"protagonist":"...","antagonist":"...","love_interest":"..."}）
	CharacterArchetypes string `json:"character_archetypes" gorm:"type:text"`

	// 情绪曲线模板（JSON数组，每10%进度的期望情绪强度 0-10）
	EmotionCurveTemplate string `json:"emotion_curve_template" gorm:"type:text"`

	// 爆款关键词（逗号分隔，用于触发对应场景设计）
	KeyTriggers string `json:"key_triggers" gorm:"size:500"`

	IsBuiltin bool `json:"is_builtin" gorm:"default:false"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (DramaTemplate) TableName() string {
	return "ink_drama_template"
}

// DramaTemplateArchetypes 角色原型 JSON 结构
type DramaTemplateArchetypes struct {
	Protagonist  string `json:"protagonist"`
	Antagonist   string `json:"antagonist"`
	LoveInterest string `json:"love_interest,omitempty"`
	Sidekick     string `json:"sidekick,omitempty"`
}
