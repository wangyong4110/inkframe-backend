package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

type DramaTemplateRepository struct {
	db *gorm.DB
}

func NewDramaTemplateRepository(db *gorm.DB) *DramaTemplateRepository {
	return &DramaTemplateRepository{db: db}
}

func (r *DramaTemplateRepository) List() ([]*model.DramaTemplate, error) {
	var templates []*model.DramaTemplate
	err := r.db.Order("is_builtin DESC, id ASC").Find(&templates).Error
	return templates, err
}

func (r *DramaTemplateRepository) GetByID(id uint) (*model.DramaTemplate, error) {
	var t model.DramaTemplate
	err := r.db.First(&t, id).Error
	return &t, err
}

func (r *DramaTemplateRepository) Create(t *model.DramaTemplate) error {
	return r.db.Create(t).Error
}

func (r *DramaTemplateRepository) Update(t *model.DramaTemplate) error {
	return r.db.Save(t).Error
}

func (r *DramaTemplateRepository) Delete(id uint) error {
	return r.db.Delete(&model.DramaTemplate{}, id).Error
}

func (r *DramaTemplateRepository) Upsert(t *model.DramaTemplate) error {
	var existing model.DramaTemplate
	if err := r.db.Where("name = ?", t.Name).First(&existing).Error; err == nil {
		t.ID = existing.ID
		return r.db.Save(t).Error
	}
	return r.db.Create(t).Error
}
