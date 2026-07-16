package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ArcSummaryRepository 弧光摘要仓库
type ArcSummaryRepository struct {
	db *gorm.DB
}

func NewArcSummaryRepository(db *gorm.DB) *ArcSummaryRepository {
	return &ArcSummaryRepository{db: db}
}

// Upsert 原子写入弧摘要：若 (novel_id, arc_no) 已存在则更新，否则插入。
// 利用数据库唯一索引 idx_arc_novel_no 做 ON DUPLICATE KEY UPDATE，
// 多实例并发时只有一条记录，不会产生重复行。
func (r *ArcSummaryRepository) Upsert(arc *model.ArcSummary) error {
	return r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "novel_id"}, {Name: "arc_no"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"summary", "key_events", "character_changes", "open_foreshadows",
			"start_chapter", "end_chapter", "updated_at", "deleted_at",
		}),
	}).Create(arc).Error
}

// ListByNovel 获取小说的所有弧摘要（按 arc_no 升序）
func (r *ArcSummaryRepository) ListByNovel(novelID uint) ([]*model.ArcSummary, error) {
	var arcs []*model.ArcSummary
	if err := r.db.Where("novel_id = ?", novelID).Order("arc_no ASC").Find(&arcs).Error; err != nil {
		return nil, err
	}
	return arcs, nil
}
