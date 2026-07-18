package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

type ImageStylePresetRepository struct {
	db *gorm.DB
}

func NewImageStylePresetRepository(db *gorm.DB) *ImageStylePresetRepository {
	return &ImageStylePresetRepository{db: db}
}

func (r *ImageStylePresetRepository) List() ([]*model.ImageStylePreset, error) {
	var presets []*model.ImageStylePreset
	err := r.db.Order("sort_order ASC, id ASC").Find(&presets).Error
	return presets, err
}

func (r *ImageStylePresetRepository) GetByID(id uint) (*model.ImageStylePreset, error) {
	var p model.ImageStylePreset
	err := r.db.First(&p, id).Error
	return &p, err
}

func (r *ImageStylePresetRepository) GetByStyleID(styleID string) (*model.ImageStylePreset, error) {
	var p model.ImageStylePreset
	err := r.db.Where("style_id = ?", styleID).First(&p).Error
	return &p, err
}

func (r *ImageStylePresetRepository) Create(p *model.ImageStylePreset) error {
	return r.db.Create(p).Error
}

func (r *ImageStylePresetRepository) Update(p *model.ImageStylePreset) error {
	return r.db.Save(p).Error
}

func (r *ImageStylePresetRepository) Delete(id uint) error {
	return r.db.Delete(&model.ImageStylePreset{}, id).Error
}

func (r *ImageStylePresetRepository) Upsert(p *model.ImageStylePreset) error {
	var existing model.ImageStylePreset
	if err := r.db.Where("style_id = ?", p.StyleID).First(&existing).Error; err == nil {
		p.ID = existing.ID
		p.CreatedAt = existing.CreatedAt
		return r.db.Save(p).Error
	}
	return r.db.Create(p).Error
}

// DeleteBuiltinNotIn 删除不在 styleIDs 中的内置(is_builtin=true)预设，用于种子目录整体替换时
// 清理旧版本遗留的内置预设；管理员手动创建的非内置预设不受影响。
func (r *ImageStylePresetRepository) DeleteBuiltinNotIn(styleIDs []string) error {
	if len(styleIDs) == 0 {
		return r.db.Where("is_builtin = ?", true).Delete(&model.ImageStylePreset{}).Error
	}
	return r.db.Where("is_builtin = ? AND style_id NOT IN ?", true, styleIDs).Delete(&model.ImageStylePreset{}).Error
}
