package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ForeshadowRepository 伏笔仓库
type ForeshadowRepository struct {
	db *gorm.DB
}

func NewForeshadowRepository(db *gorm.DB) *ForeshadowRepository {
	return &ForeshadowRepository{db: db}
}

func (r *ForeshadowRepository) Create(f *model.Foreshadow) error {
	return r.db.Create(f).Error
}

func (r *ForeshadowRepository) GetByID(id uint) (*model.Foreshadow, error) {
	var f model.Foreshadow
	return &f, r.db.First(&f, id).Error
}

func (r *ForeshadowRepository) ListByNovel(novelID uint) ([]*model.Foreshadow, error) {
	var list []*model.Foreshadow
	return list, r.db.Where("novel_id = ?", novelID).Order("created_at DESC").Find(&list).Error
}

func (r *ForeshadowRepository) ListUnfulfilled(novelID uint) ([]*model.Foreshadow, error) {
	var list []*model.Foreshadow
	return list, r.db.Where("novel_id = ? AND status = 'planted'", novelID).Order("created_at DESC").Find(&list).Error
}

func (r *ForeshadowRepository) Update(f *model.Foreshadow) error {
	return r.db.Save(f).Error
}

func (r *ForeshadowRepository) Delete(id uint) error {
	return r.db.Delete(&model.Foreshadow{}, id).Error
}
