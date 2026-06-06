package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// AuditService records audit events for tenant actions.
type AuditService struct {
	db *gorm.DB
}

// NewAuditService creates a new AuditService.
func NewAuditService(db *gorm.DB) *AuditService {
	return &AuditService{db: db}
}

// Log records an audit event asynchronously (non-blocking).
func (s *AuditService) Log(tenantID, userID uint, action, entityType string, entityID uint, ip, ua string, detail interface{}) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		detailJSON, err := json.Marshal(detail)
		if err != nil {
			logger.Errorf("[AuditService] marshal detail: %v", err)
			detailJSON = []byte("{}")
		}
		entry := &model.AuditLog{
			TenantID:   tenantID,
			UserID:     userID,
			Action:     action,
			EntityType: entityType,
			EntityID:   entityID,
			IPAddress:  ip,
			UserAgent:  ua,
			Detail:     string(detailJSON),
		}
		if err := s.db.WithContext(ctx).Create(entry).Error; err != nil {
			logger.Errorf("[AuditService] create log: %v", err)
		}
	}()
}

// List returns audit logs for a tenant, paginated, optionally filtered by action.
func (s *AuditService) List(tenantID uint, action string, page, pageSize int) ([]*model.AuditLog, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}

	query := s.db.Model(&model.AuditLog{}).Where("tenant_id = ?", tenantID)
	if action != "" {
		query = query.Where("action = ?", action)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}

	var logs []*model.AuditLog
	offset := (page - 1) * pageSize
	if err := query.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, 0, fmt.Errorf("list audit logs: %w", err)
	}
	return logs, total, nil
}
