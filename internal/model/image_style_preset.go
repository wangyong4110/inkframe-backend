package model

import "time"

// ImageStylePreset 画风预设（风格库页面用）。内置预设随服务启动幂等 seed，
// 用户可在管理页新增自定义预设；StyleID 对应 Novel.AIConfig.ImageStyle /
// character、video 等模型上 art_style 字段取的值。
type ImageStylePreset struct {
	ID              uint   `json:"id" gorm:"primaryKey"`
	StyleID         string `json:"style_id" gorm:"size:50;not null;uniqueIndex"`
	Name            string `json:"name" gorm:"size:100;not null"`
	Description     string `json:"description" gorm:"type:text"`
	Tags            string `json:"tags" gorm:"type:text"`           // JSON 字符串数组
	Category        string `json:"category" gorm:"size:20;index"`   // live_action | anime，前端 Tab 分组用
	PromptCategory  string `json:"prompt_category" gorm:"size:24"`  // realistic|anime|classic_illustration|dark_stylized|pixel|render_3d，供质量提升词/冲突清理词选择，不用于前端展示
	PreviewColors   string `json:"preview_colors" gorm:"type:text"` // JSON 字符串数组，无示例图时用于卡片渐变底色
	PreviewImageURL string `json:"preview_image_url" gorm:"size:1000"`
	Prompt          string `json:"prompt" gorm:"type:text"` // 注入 image_prompt 的英文风格提示词

	SortOrder int  `json:"sort_order" gorm:"default:0"`
	IsBuiltin bool `json:"is_builtin" gorm:"default:false"`
	Enabled   bool `json:"enabled" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ImageStylePreset) TableName() string { return "ink_image_style_preset" }
