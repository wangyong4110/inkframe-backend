package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── ChapterReviewRecordRepository ───────────────────────────────────────────

type ChapterReviewRecordRepository struct{ db *gorm.DB }

func NewChapterReviewRecordRepository(db *gorm.DB) *ChapterReviewRecordRepository {
	return &ChapterReviewRecordRepository{db: db}
}

func (r *ChapterReviewRecordRepository) Create(rec *model.ChapterReviewRecord) error {
	return r.db.Create(rec).Error
}

func (r *ChapterReviewRecordRepository) ListByChapter(chapterID uint) ([]*model.ChapterReviewRecord, error) {
	var list []*model.ChapterReviewRecord
	err := r.db.Where("chapter_id = ?", chapterID).Order("created_at DESC").Find(&list).Error
	return list, err
}

func (r *ChapterReviewRecordRepository) GetByID(id uint) (*model.ChapterReviewRecord, error) {
	var rec model.ChapterReviewRecord
	err := r.db.First(&rec, id).Error
	return &rec, err
}

func (r *ChapterReviewRecordRepository) Update(rec *model.ChapterReviewRecord) error {
	return r.db.Save(rec).Error
}

// ─── ChapterIgnoredIssueRepository ───────────────────────────────────────────

type ChapterIgnoredIssueRepository struct{ db *gorm.DB }

func NewChapterIgnoredIssueRepository(db *gorm.DB) *ChapterIgnoredIssueRepository {
	return &ChapterIgnoredIssueRepository{db: db}
}

func (r *ChapterIgnoredIssueRepository) Create(item *model.ChapterIgnoredIssue) error {
	return r.db.Where(model.ChapterIgnoredIssue{
		ChapterID: item.ChapterID,
		IssueHash: item.IssueHash,
	}).FirstOrCreate(item).Error
}

func (r *ChapterIgnoredIssueRepository) ListByChapter(chapterID uint) ([]*model.ChapterIgnoredIssue, error) {
	var list []*model.ChapterIgnoredIssue
	err := r.db.Where("chapter_id = ?", chapterID).Order("id ASC").Find(&list).Error
	return list, err
}

func (r *ChapterIgnoredIssueRepository) DeleteByID(id uint) error {
	return r.db.Delete(&model.ChapterIgnoredIssue{}, id).Error
}

func (r *ChapterIgnoredIssueRepository) ExistsByHash(chapterID uint, hash string) bool {
	var count int64
	r.db.Model(&model.ChapterIgnoredIssue{}).
		Where("chapter_id = ? AND issue_hash = ?", chapterID, hash).
		Count(&count)
	return count > 0
}
