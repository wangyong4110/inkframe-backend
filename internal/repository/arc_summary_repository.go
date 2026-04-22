package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ArcSummaryRepository 弧光摘要仓库
type ArcSummaryRepository struct {
	db *gorm.DB
}

func NewArcSummaryRepository(db *gorm.DB) *ArcSummaryRepository {
	return &ArcSummaryRepository{db: db}
}

// Create 创建弧光摘要
func (r *ArcSummaryRepository) Create(arc *model.ArcSummary) error {
	return r.db.Create(arc).Error
}

// Update 更新弧光摘要
func (r *ArcSummaryRepository) Update(arc *model.ArcSummary) error {
	return r.db.Save(arc).Error
}

// GetByNovelAndArcNo 获取指定小说的指定弧摘要
func (r *ArcSummaryRepository) GetByNovelAndArcNo(novelID uint, arcNo int) (*model.ArcSummary, error) {
	var arc model.ArcSummary
	if err := r.db.Where("novel_id = ? AND arc_no = ?", novelID, arcNo).First(&arc).Error; err != nil {
		return nil, err
	}
	return &arc, nil
}

// ListByNovel 获取小说的所有弧摘要（按 arc_no 升序）
func (r *ArcSummaryRepository) ListByNovel(novelID uint) ([]*model.ArcSummary, error) {
	var arcs []*model.ArcSummary
	if err := r.db.Where("novel_id = ?", novelID).Order("arc_no ASC").Find(&arcs).Error; err != nil {
		return nil, err
	}
	return arcs, nil
}
