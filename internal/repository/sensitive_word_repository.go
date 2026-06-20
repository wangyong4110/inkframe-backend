package repository

import (
	"context"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

type SensitiveWordRuleRepository struct {
	db *gorm.DB
}

func NewSensitiveWordRuleRepository(db *gorm.DB) *SensitiveWordRuleRepository {
	return &SensitiveWordRuleRepository{db: db}
}

func (r *SensitiveWordRuleRepository) ListEnabled(ctx context.Context) ([]model.SensitiveWordRule, error) {
	var rules []model.SensitiveWordRule
	err := r.db.WithContext(ctx).Where("enabled = ?", true).Find(&rules).Error
	return rules, err
}

func (r *SensitiveWordRuleRepository) List(tenantID uint, page, pageSize int) ([]model.SensitiveWordRule, int64, error) {
	var rules []model.SensitiveWordRule
	var total int64
	q := r.db.Model(&model.SensitiveWordRule{}).Where("tenant_id = ?", tenantID)
	q.Count(&total)
	err := q.Offset((page - 1) * pageSize).Limit(pageSize).Order("id DESC").Find(&rules).Error
	return rules, total, err
}

func (r *SensitiveWordRuleRepository) GetByID(id uint) (*model.SensitiveWordRule, error) {
	var rule model.SensitiveWordRule
	err := r.db.First(&rule, id).Error
	return &rule, err
}

func (r *SensitiveWordRuleRepository) Create(rule *model.SensitiveWordRule) error {
	return r.db.Create(rule).Error
}

func (r *SensitiveWordRuleRepository) Update(rule *model.SensitiveWordRule) error {
	return r.db.Save(rule).Error
}

func (r *SensitiveWordRuleRepository) Delete(id uint) error {
	return r.db.Delete(&model.SensitiveWordRule{}, id).Error
}
