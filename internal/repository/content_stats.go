package repository

import (
	"errors"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ContentStatsRepository 内容统计仓库（独立表 ink_content_stats）
type ContentStatsRepository struct {
	db *gorm.DB
}

func NewContentStatsRepository(db *gorm.DB) *ContentStatsRepository {
	return &ContentStatsRepository{db: db}
}

// upsertWith 用 ON DUPLICATE KEY UPDATE 原子更新指定列，避免拼接字段名。
func (r *ContentStatsRepository) upsertWith(db *gorm.DB, entityType string, entityID uint, updates map[string]interface{}) error {
	cs := &model.ContentStats{EntityType: entityType, EntityID: entityID}
	updates["updated_at"] = gorm.Expr("NOW()")
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(cs).Error
}

// Ensure 确保统计行存在（幂等）
func (r *ContentStatsRepository) Ensure(entityType string, entityID uint) error {
	cs := &model.ContentStats{EntityType: entityType, EntityID: entityID}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(cs).Error
}

// IncrView 浏览量 +1
func (r *ContentStatsRepository) IncrView(entityType string, entityID uint) error {
	return r.upsertWith(r.db, entityType, entityID, map[string]interface{}{
		"view_count": gorm.Expr("view_count + 1"),
	})
}

// IncrLike 点赞数 delta（+1 或 -1，GREATEST 防下溢）
func (r *ContentStatsRepository) IncrLike(entityType string, entityID uint, delta int) error {
	return r.upsertWith(r.db, entityType, entityID, map[string]interface{}{
		"like_count": gorm.Expr("GREATEST(0, like_count + ?)", delta),
	})
}

// IncrComment 评论数 delta（GREATEST 防下溢）
func (r *ContentStatsRepository) IncrComment(entityType string, entityID uint, delta int) error {
	return r.upsertWith(r.db, entityType, entityID, map[string]interface{}{
		"comment_count": gorm.Expr("GREATEST(0, comment_count + ?)", delta),
	})
}

// IncrViewTx 在事务中浏览量 +1
func (r *ContentStatsRepository) IncrViewTx(tx *gorm.DB, entityType string, entityID uint) error {
	return r.upsertWith(tx, entityType, entityID, map[string]interface{}{
		"view_count": gorm.Expr("view_count + 1"),
	})
}

// IncrLikeTx 在事务中点赞数 delta
func (r *ContentStatsRepository) IncrLikeTx(tx *gorm.DB, entityType string, entityID uint, delta int) error {
	return r.upsertWith(tx, entityType, entityID, map[string]interface{}{
		"like_count": gorm.Expr("GREATEST(0, like_count + ?)", delta),
	})
}

// IncrCommentTx 在事务中评论数 delta
func (r *ContentStatsRepository) IncrCommentTx(tx *gorm.DB, entityType string, entityID uint, delta int) error {
	return r.upsertWith(tx, entityType, entityID, map[string]interface{}{
		"comment_count": gorm.Expr("GREATEST(0, comment_count + ?)", delta),
	})
}

// Get 获取单条统计记录，不存在时返回零值结构体
func (r *ContentStatsRepository) Get(entityType string, entityID uint) *model.ContentStats {
	var cs model.ContentStats
	if err := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).First(&cs).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.Errorf("[ContentStatsRepository] Get %s/%d: %v", entityType, entityID, err)
	}
	if cs.EntityID == 0 {
		return &model.ContentStats{EntityType: entityType, EntityID: entityID}
	}
	return &cs
}

// BatchGet 批量获取统计记录，返回 map[entityID]*ContentStats
func (r *ContentStatsRepository) BatchGet(entityType string, entityIDs []uint) map[uint]*model.ContentStats {
	result := make(map[uint]*model.ContentStats, len(entityIDs))
	if len(entityIDs) == 0 {
		return result
	}
	var rows []*model.ContentStats
	if err := r.db.Where("entity_type = ? AND entity_id IN ?", entityType, entityIDs).Find(&rows).Error; err != nil {
		logger.Errorf("[ContentStatsRepository] BatchGet %s: %v", entityType, err)
		return result
	}
	for _, row := range rows {
		result[row.EntityID] = row
	}
	return result
}

// GetForRecalc 返回给定实体类型的全部统计行（用于热度分批量重算）
func (r *ContentStatsRepository) GetForRecalc(entityType string) ([]*model.ContentStats, error) {
	var rows []*model.ContentStats
	err := r.db.Where("entity_type = ?", entityType).Find(&rows).Error
	return rows, err
}
