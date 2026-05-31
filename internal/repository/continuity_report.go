package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ContinuityReportRepository handles persistence of continuity check results.
type ContinuityReportRepository struct {
	db *gorm.DB
}

func NewContinuityReportRepository(db *gorm.DB) *ContinuityReportRepository {
	return &ContinuityReportRepository{db: db}
}

func (r *ContinuityReportRepository) Create(rec *model.ContinuityReportRecord) error {
	return r.db.Create(rec).Error
}

func (r *ContinuityReportRepository) ListByChapter(chapterID uint) ([]*model.ContinuityReportRecord, error) {
	var recs []*model.ContinuityReportRecord
	return recs, r.db.Where("chapter_id = ?", chapterID).Order("created_at desc").Find(&recs).Error
}

func (r *ContinuityReportRepository) LatestByChapter(chapterID uint) (*model.ContinuityReportRecord, error) {
	var rec model.ContinuityReportRecord
	return &rec, r.db.Where("chapter_id = ?", chapterID).Order("created_at desc").First(&rec).Error
}
