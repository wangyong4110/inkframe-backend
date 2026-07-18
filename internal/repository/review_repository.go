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

// DeletePendingByEntity 硬删除某实体下所有待处理（未应用）的审查记录。
// 用于单场次分镜重新生成后：后续场次的 shot_no 会整体位移，遗留的待处理审查建议
// （按旧 shot_no 记录）会指向错误的分镜，必须作废，而不能留着误导用户。
// 已应用（applied）或已回滚（rolled_back）的记录代表历史事实，不受影响。
func (r *ReviewRecordRepository) DeletePendingByEntity(entityType string, entityID uint) error {
	return r.db.Unscoped().Where("entity_type = ? AND entity_id = ? AND status = 'pending'", entityType, entityID).
		Delete(&model.ReviewRecord{}).Error
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
	// 硬删除：避免 (entity_type, entity_id, issue_hash) 唯一索引与软删除残留行冲突，
	// 导致用户取消忽略后无法再次忽略同一问题。
	return r.db.Unscoped().Delete(&model.IgnoredReviewIssue{}, id).Error
}

