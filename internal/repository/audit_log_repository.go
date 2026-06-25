package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// AuditLogRepository provides data access for audit logs.
type AuditLogRepository struct {
	db *gorm.DB
}

// NewAuditLogRepository creates a new AuditLogRepository.
func NewAuditLogRepository(db *gorm.DB) *AuditLogRepository {
	return &AuditLogRepository{db: db}
}

// Create inserts a new audit log record.
func (r *AuditLogRepository) Create(log *model.AuditLog) error {
	return r.db.Create(log).Error
}

// ListByNovel returns paginated audit logs for a specific novel (project-level logs).
func (r *AuditLogRepository) ListByNovel(novelID uint, tenantID uint, page, pageSize int) ([]*model.AuditLog, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	query := r.db.Model(&model.AuditLog{}).
		Where("novel_id = ? AND tenant_id = ?", novelID, tenantID)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := query.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// ListByUser returns paginated audit logs for a specific user (user-level logs, novel_id=0).
func (r *AuditLogRepository) ListByUser(userID uint, tenantID uint, page, pageSize int) ([]*model.AuditLog, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	query := r.db.Model(&model.AuditLog{}).
		Where("user_id = ? AND tenant_id = ? AND novel_id = 0", userID, tenantID)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := query.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}
