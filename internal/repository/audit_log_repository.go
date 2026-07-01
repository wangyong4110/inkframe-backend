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

// AuditLogItem is kept as a type alias so callers don't need to change signatures.
type AuditLogItem = model.AuditLog

// Create inserts a new audit log record.
func (r *AuditLogRepository) Create(log *model.AuditLog) error {
	return r.db.Create(log).Error
}

// enrichWithUsernames does a single IN-query to attach Username/Nickname to a slice of logs.
func (r *AuditLogRepository) enrichWithUsernames(logs []*model.AuditLog) {
	if len(logs) == 0 {
		return
	}
	seen := make(map[uint]bool)
	ids := make([]uint, 0, len(logs))
	for _, l := range logs {
		if l.UserID > 0 && !seen[l.UserID] {
			seen[l.UserID] = true
			ids = append(ids, l.UserID)
		}
	}
	if len(ids) == 0 {
		return
	}

	var rows []struct {
		ID       uint   `gorm:"column:id"`
		Username string `gorm:"column:username"`
		Nickname string `gorm:"column:nickname"`
	}
	r.db.Table("users").
		Select("id, username, nickname").
		Where("id IN ?", ids).
		Find(&rows)

	um := make(map[uint]struct{ Username, Nickname string }, len(rows))
	for _, u := range rows {
		um[u.ID] = struct{ Username, Nickname string }{u.Username, u.Nickname}
	}
	for _, l := range logs {
		if u, ok := um[l.UserID]; ok {
			l.Username = u.Username
			l.Nickname = u.Nickname
		}
	}
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

	base := r.db.Model(&model.AuditLog{}).Where("novel_id = ? AND tenant_id = ?", novelID, tenantID)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := base.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	r.enrichWithUsernames(logs)
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

	base := r.db.Model(&model.AuditLog{}).
		Where("user_id = ? AND tenant_id = ? AND novel_id = 0", userID, tenantID)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := base.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	r.enrichWithUsernames(logs)
	return logs, total, nil
}

// ListByTenant returns paginated audit logs for a tenant (legacy).
func (r *AuditLogRepository) ListByTenant(tenantID uint, action string, page, pageSize int) ([]*model.AuditLog, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	base := r.db.Model(&model.AuditLog{}).Where("tenant_id = ?", tenantID)
	if action != "" {
		base = base.Where("action = ?", action)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := base.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	r.enrichWithUsernames(logs)
	return logs, total, nil
}
