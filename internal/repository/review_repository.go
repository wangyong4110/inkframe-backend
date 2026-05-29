package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── ReviewRecordRepository ───────────────────────────────────────────────────

type ReviewRecordRepository struct{ db *gorm.DB }

func NewReviewRecordRepository(db *gorm.DB) *ReviewRecordRepository {
	return &ReviewRecordRepository{db: db}
}

func (r *ReviewRecordRepository) Create(rec *model.ReviewRecord) error {
	return r.db.Create(rec).Error
}

func (r *ReviewRecordRepository) ListByEntity(entityType string, entityID uint) ([]*model.ReviewRecord, error) {
	var list []*model.ReviewRecord
	err := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).
		Order("created_at DESC").Find(&list).Error
	return list, err
}

func (r *ReviewRecordRepository) GetByID(id uint) (*model.ReviewRecord, error) {
	var rec model.ReviewRecord
	err := r.db.First(&rec, id).Error
	return &rec, err
}

func (r *ReviewRecordRepository) Update(rec *model.ReviewRecord) error {
	return r.db.Save(rec).Error
}

func (r *ReviewRecordRepository) GetLatestApplied(entityType string, entityID uint) (*model.ReviewRecord, error) {
	var rec model.ReviewRecord
	err := r.db.Where("entity_type = ? AND entity_id = ? AND status = 'applied'", entityType, entityID).
		Order("applied_at DESC").First(&rec).Error
	return &rec, err
}

// ─── IgnoredReviewIssueRepository ────────────────────────────────────────────

type IgnoredReviewIssueRepository struct{ db *gorm.DB }

func NewIgnoredReviewIssueRepository(db *gorm.DB) *IgnoredReviewIssueRepository {
	return &IgnoredReviewIssueRepository{db: db}
}

func (r *IgnoredReviewIssueRepository) Create(item *model.IgnoredReviewIssue) error {
	return r.db.Where(model.IgnoredReviewIssue{
		EntityType: item.EntityType,
		EntityID:   item.EntityID,
		IssueHash:  item.IssueHash,
	}).FirstOrCreate(item).Error
}

func (r *IgnoredReviewIssueRepository) ListByEntity(entityType string, entityID uint) ([]*model.IgnoredReviewIssue, error) {
	var list []*model.IgnoredReviewIssue
	err := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).
		Order("id ASC").Find(&list).Error
	return list, err
}

func (r *IgnoredReviewIssueRepository) Delete(id uint) error {
	return r.db.Delete(&model.IgnoredReviewIssue{}, id).Error
}

func (r *IgnoredReviewIssueRepository) ExistsByHash(entityType string, entityID uint, hash string) bool {
	var count int64
	r.db.Model(&model.IgnoredReviewIssue{}).
		Where("entity_type = ? AND entity_id = ? AND issue_hash = ?", entityType, entityID, hash).
		Count(&count)
	return count > 0
}
