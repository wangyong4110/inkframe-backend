package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// PlotPointRepository 剧情点仓库
type PlotPointRepository struct {
	db *gorm.DB
}

func NewPlotPointRepository(db *gorm.DB) *PlotPointRepository {
	return &PlotPointRepository{db: db}
}

func (r *PlotPointRepository) Create(pp *model.PlotPoint) error {
	return r.db.Create(pp).Error
}

func (r *PlotPointRepository) Update(pp *model.PlotPoint) error {
	return r.db.Save(pp).Error
}

func (r *PlotPointRepository) Delete(id uint) error {
	return r.db.Delete(&model.PlotPoint{}, id).Error
}

func (r *PlotPointRepository) GetByID(id uint) (*model.PlotPoint, error) {
	var pp model.PlotPoint
	if err := r.db.First(&pp, id).Error; err != nil {
		return nil, err
	}
	return &pp, nil
}

func (r *PlotPointRepository) ListByChapter(chapterID uint) ([]*model.PlotPoint, error) {
	var pps []*model.PlotPoint
	if err := r.db.Where("chapter_id = ?", chapterID).Order("created_at ASC").Find(&pps).Error; err != nil {
		return nil, err
	}
	return pps, nil
}

func (r *PlotPointRepository) ListByNovel(novelID uint, ppType string, onlyUnresolved bool) ([]*model.PlotPoint, error) {
	q := r.db.Where("novel_id = ?", novelID)
	if ppType != "" {
		q = q.Where("type = ?", ppType)
	}
	if onlyUnresolved {
		q = q.Where("is_resolved = ?", false)
	}
	var pps []*model.PlotPoint
	if err := q.Order("chapter_id ASC, created_at ASC").Find(&pps).Error; err != nil {
		return nil, err
	}
	return pps, nil
}

func (r *PlotPointRepository) BatchCreate(pps []*model.PlotPoint) error {
	if len(pps) == 0 {
		return nil
	}
	return r.db.Create(&pps).Error
}
