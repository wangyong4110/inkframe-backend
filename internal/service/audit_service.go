package service

import (
	"encoding/json"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"gorm.io/gorm"
)

// AuditService records audit events for tenant actions.
type AuditService struct {
	db   *gorm.DB
	repo *repository.AuditLogRepository
}

// NewAuditService creates a new AuditService.
func NewAuditService(db *gorm.DB) *AuditService {
	return &AuditService{
		db:   db,
		repo: repository.NewAuditLogRepository(db),
	}
}

// AuditEntry holds all fields for a single audit event.
type AuditEntry struct {
	TenantID     uint
	UserID       uint
	NovelID      uint   // 0 = user-level, >0 = project-level
	Action       string // e.g. "novel.create"
	ResourceType string
	ResourceID   uint
	ResourceName string
	Details      map[string]any
	IP           string
	Status       string // "ok" or "fail"
}

// LogEntry records an audit event asynchronously (non-blocking).
// This is the new preferred API used by handlers.
func (s *AuditService) LogEntry(entry AuditEntry) {
	if s == nil || s.repo == nil {
		return
	}
	go func() {
		if entry.Status == "" {
			entry.Status = "ok"
		}
		var detailsJSON string
		if len(entry.Details) > 0 {
			if b, err := json.Marshal(entry.Details); err == nil {
				detailsJSON = string(b)
			}
		}
		_ = s.repo.Create(&model.AuditLog{
			TenantID:     entry.TenantID,
			UserID:       entry.UserID,
			NovelID:      entry.NovelID,
			Action:       entry.Action,
			ResourceType: entry.ResourceType,
			ResourceID:   entry.ResourceID,
			ResourceName: entry.ResourceName,
			Details:      detailsJSON,
			IP:           entry.IP,
			Status:       entry.Status,
		})
	}()
}

// List returns audit logs for a tenant, paginated, optionally filtered by action.
// Legacy method for the existing audit handler.
func (s *AuditService) List(tenantID uint, action string, page, pageSize int) ([]*repository.AuditLogItem, int64, error) {
	return s.repo.ListByTenant(tenantID, action, page, pageSize)
}

// ListByNovel returns project-level audit logs for a specific novel.
func (s *AuditService) ListByNovel(novelID uint, tenantID uint, page, pageSize int) ([]*repository.AuditLogItem, int64, error) {
	return s.repo.ListByNovel(novelID, tenantID, page, pageSize)
}

// ListByUser returns user-level audit logs (novel_id=0) for a specific user.
func (s *AuditService) ListByUser(userID uint, tenantID uint, page, pageSize int) ([]*repository.AuditLogItem, int64, error) {
	return s.repo.ListByUser(userID, tenantID, page, pageSize)
}
