package repository

import (
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// validStatsFields 允许的统计字段白名单，防止 SQL 注入
var validStatsFields = map[string]bool{
	"view_count":    true,
	"like_count":    true,
	"comment_count": true,
}

// ContentStatsRepository 内容统计仓库（独立表 ink_content_stats）
type ContentStatsRepository struct {
	db *gorm.DB
}

func NewContentStatsRepository(db *gorm.DB) *ContentStatsRepository {
	return &ContentStatsRepository{db: db}
}

// Ensure 确保统计行存在（INSERT IGNORE 幂等）
func (r *ContentStatsRepository) Ensure(entityType string, entityID uint) error {
	return r.db.Exec(
		`INSERT IGNORE INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		 VALUES (?, ?, 0, 0, 0, NOW())`,
		entityType, entityID,
	).Error
}

// Incr 原子递增/递减指定字段，递减时使用 GREATEST(0, field+delta) 防止下溢
func (r *ContentStatsRepository) Incr(entityType string, entityID uint, field string, delta int) error {
	if !validStatsFields[field] {
		return fmt.Errorf("content_stats: invalid field %q", field)
	}
	return r.db.Exec(
		`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		 VALUES (?, ?, 0, 0, 0, NOW())
		 ON DUPLICATE KEY UPDATE `+field+` = GREATEST(0, `+field+` + ?), updated_at = NOW()`,
		entityType, entityID, delta,
	).Error
}

// IncrTx 在给定事务中原子递增/递减指定字段
func (r *ContentStatsRepository) IncrTx(tx *gorm.DB, entityType string, entityID uint, field string, delta int) error {
	if !validStatsFields[field] {
		return fmt.Errorf("content_stats: invalid field %q", field)
	}
	return tx.Exec(
		`INSERT INTO ink_content_stats (entity_type, entity_id, view_count, like_count, comment_count, updated_at)
		 VALUES (?, ?, 0, 0, 0, NOW())
		 ON DUPLICATE KEY UPDATE `+field+` = GREATEST(0, `+field+` + ?), updated_at = NOW()`,
		entityType, entityID, delta,
	).Error
}

// Get 获取单条统计记录，不存在时返回零值结构体
func (r *ContentStatsRepository) Get(entityType string, entityID uint) *model.ContentStats {
	var cs model.ContentStats
	r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).First(&cs)
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
	r.db.Where("entity_type = ? AND entity_id IN ?", entityType, entityIDs).Find(&rows)
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
