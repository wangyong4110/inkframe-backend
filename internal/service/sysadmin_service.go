package service

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// SysAdminService provides system-wide administration capabilities.
// It operates across all tenants and bypasses tenant-scoped data isolation.
type SysAdminService struct {
	db        *gorm.DB
	jwtSecret string
	jwtExpiry time.Duration
}

// NewSysAdminService creates a new SysAdminService.
func NewSysAdminService(db *gorm.DB, jwtSecret string, jwtExpiry time.Duration) *SysAdminService {
	return &SysAdminService{db: db, jwtSecret: jwtSecret, jwtExpiry: jwtExpiry}
}

// SysOverview holds platform-wide statistics.
type SysOverview struct {
	TenantCount  int64 `json:"tenant_count"`
	UserCount    int64 `json:"user_count"`
	NovelCount   int64 `json:"novel_count"`
	ChapterCount int64 `json:"chapter_count"`
	VideoCount   int64 `json:"video_count"`
	ActiveTasks  int64 `json:"active_tasks"`
}

// GetOverview returns platform-wide statistics.
func (s *SysAdminService) GetOverview() (*SysOverview, error) {
	var o SysOverview
	s.db.Model(&model.Tenant{}).Count(&o.TenantCount)
	s.db.Model(&model.User{}).Where("role != ?", model.RoleSystemAdmin).Count(&o.UserCount)
	s.db.Table("ink_novel").Where("deleted_at IS NULL").Count(&o.NovelCount)
	s.db.Table("ink_chapter").Where("deleted_at IS NULL").Count(&o.ChapterCount)
	s.db.Table("ink_video").Where("deleted_at IS NULL").Count(&o.VideoCount)
	s.db.Model(&model.AsyncTask{}).Where("status IN ?", []string{"pending", "running"}).Count(&o.ActiveTasks)
	return &o, nil
}

// ── Tenant management ─────────────────────────────────────────────────────────

// ListTenants returns a paginated list of tenants with optional search/status filters.
func (s *SysAdminService) ListTenants(page, size int, search, status string) ([]*model.Tenant, int64, error) {
	q := s.db.Model(&model.Tenant{})
	if search != "" {
		q = q.Where("name LIKE ? OR code LIKE ?", "%"+search+"%", "%"+search+"%")
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	q.Count(&total)
	var tenants []*model.Tenant
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&tenants).Error
	return tenants, total, err
}

// GetTenant returns a single tenant by ID.
func (s *SysAdminService) GetTenant(id uint) (*model.Tenant, error) {
	var t model.Tenant
	err := s.db.First(&t, id).Error
	return &t, err
}

// UpdateTenantRequest holds mutable tenant fields for admin updates.
type UpdateTenantRequest struct {
	Status    string     `json:"status"`
	Plan      string     `json:"plan"`
	ExpiresAt *time.Time `json:"expires_at"`
	Quota     string     `json:"quota"`   // JSON string
	Profile   string     `json:"profile"` // JSON string
}

// UpdateTenant applies admin-initiated changes to a tenant record.
func (s *SysAdminService) UpdateTenant(id uint, req *UpdateTenantRequest) (*model.Tenant, error) {
	updates := map[string]interface{}{}
	if req.Status != "" {
		updates["status"] = req.Status
	}
	if req.Plan != "" {
		updates["plan"] = req.Plan
	}
	if req.ExpiresAt != nil {
		updates["expires_at"] = req.ExpiresAt
	}
	if req.Quota != "" {
		updates["quota"] = req.Quota
	}
	if req.Profile != "" {
		updates["profile"] = req.Profile
	}
	if err := s.db.Model(&model.Tenant{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetTenant(id)
}

// DeleteTenant soft-deletes a tenant.
func (s *SysAdminService) DeleteTenant(id uint) error {
	return s.db.Delete(&model.Tenant{}, id).Error
}

// ── User management ──────────────────────────────────────────────────────────

// ListUsers returns a paginated list of users with optional search/role filters.
func (s *SysAdminService) ListUsers(page, size int, search, role string) ([]*model.User, int64, error) {
	q := s.db.Model(&model.User{})
	if search != "" {
		q = q.Where("username LIKE ? OR email LIKE ?", "%"+search+"%", "%"+search+"%")
	}
	if role != "" {
		q = q.Where("role = ?", role)
	}
	var total int64
	q.Count(&total)
	var users []*model.User
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&users).Error
	return users, total, err
}

// GetUser returns a single user by ID.
func (s *SysAdminService) GetUser(id uint) (*model.User, error) {
	var u model.User
	err := s.db.First(&u, id).Error
	return &u, err
}

// UpdateUserRequest holds mutable user fields for admin updates.
type UpdateUserRequest struct {
	Role   string `json:"role"`
	Status string `json:"status"`
}

// UpdateUser applies admin-initiated changes to a user record.
func (s *SysAdminService) UpdateUser(id uint, req *UpdateUserRequest) (*model.User, error) {
	updates := map[string]interface{}{}
	if req.Role != "" {
		updates["role"] = req.Role
	}
	if req.Status != "" {
		updates["status"] = req.Status
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetUser(id)
}

// ImpersonateUser generates a short-lived JWT scoped to the target user's first tenant.
// Intended for debugging purposes only.
func (s *SysAdminService) ImpersonateUser(targetUserID uint) (string, error) {
	var u model.User
	if err := s.db.First(&u, targetUserID).Error; err != nil {
		return "", err
	}
	var tu model.TenantUser
	if err := s.db.Where("user_id = ?", targetUserID).Order("id ASC").First(&tu).Error; err != nil {
		return "", fmt.Errorf("no tenant found for user %d", targetUserID)
	}
	expiresAt := time.Now().Add(1 * time.Hour) // short-lived
	jti := uuid.New().String()
	claims := &middleware.JWTClaims{
		UserID: u.ID, TenantID: tu.TenantID, Role: tu.Role, JTI: jti,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        jti,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.jwtSecret))
}

// ResetUserPassword resets a user's password to the provided plaintext value (bcrypt-hashed before storage).
func (s *SysAdminService) ResetUserPassword(userID uint, newPassword string) error {
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
	if err != nil {
		return err
	}
	return s.db.Model(&model.User{}).Where("id = ?", userID).Update("password", string(hashed)).Error
}

// ChangeAdminPassword changes the system admin's own password.
func (s *SysAdminService) ChangeAdminPassword(adminUserID uint, newPassword string) error {
	return s.ResetUserPassword(adminUserID, newPassword)
}

// ── Task management ───────────────────────────────────────────────────────────

// ListTasks returns a paginated list of async tasks with optional status filter.
func (s *SysAdminService) ListTasks(page, size int, status string) ([]*model.AsyncTask, int64, error) {
	q := s.db.Model(&model.AsyncTask{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	q.Count(&total)
	var tasks []*model.AsyncTask
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&tasks).Error
	return tasks, total, err
}

// CancelTask sets a task's status to "cancelled".
func (s *SysAdminService) CancelTask(taskID uint) error {
	return s.db.Model(&model.AsyncTask{}).Where("id = ?", taskID).Update("status", "cancelled").Error
}

// ── Audit logs ────────────────────────────────────────────────────────────────

// AuditLogItem is a serialisable view of an audit log row.
type AuditLogItem struct {
	ID         uint      `json:"id"`
	UserID     uint      `json:"user_id"`
	TenantID   uint      `json:"tenant_id"`
	Action     string    `json:"action"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	Details    string    `json:"details"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListAuditLogs queries the ink_audit_log table directly with optional filters.
func (s *SysAdminService) ListAuditLogs(page, size int, entityType string, userID uint) ([]AuditLogItem, int64, error) {
	type rawLog struct {
		ID         uint
		UserID     uint
		TenantID   uint
		Action     string
		EntityType string
		EntityID   string
		Details    string
		CreatedAt  time.Time
	}
	q := s.db.Table("ink_audit_log")
	if entityType != "" {
		q = q.Where("entity_type = ?", entityType)
	}
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	var total int64
	q.Count(&total)
	var rows []rawLog
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Scan(&rows).Error
	result := make([]AuditLogItem, len(rows))
	for i, r := range rows {
		result[i] = AuditLogItem{
			ID: r.ID, UserID: r.UserID, TenantID: r.TenantID,
			Action: r.Action, EntityType: r.EntityType, EntityID: r.EntityID,
			Details: r.Details, CreatedAt: r.CreatedAt,
		}
	}
	return result, total, err
}

// ── System settings ───────────────────────────────────────────────────────────

// ListSettings returns all system settings as a key→value map.
func (s *SysAdminService) ListSettings() (map[string]string, error) {
	var rows []struct {
		Key   string
		Value string
	}
	err := s.db.Table("ink_system_setting").Scan(&rows).Error
	result := make(map[string]string, len(rows))
	for _, r := range rows {
		result[r.Key] = r.Value
	}
	return result, err
}

// UpdateSettings upserts multiple system settings.
func (s *SysAdminService) UpdateSettings(settings map[string]string) error {
	for k, v := range settings {
		s.db.Exec(
			"INSERT INTO ink_system_setting (`key`, `value`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `value` = ?",
			k, v, v,
		)
	}
	return nil
}

// ── Content review ────────────────────────────────────────────────────────────

// ListNovels returns a paginated list of novels across all tenants.
func (s *SysAdminService) ListNovels(page, size int, search string) ([]map[string]interface{}, int64, error) {
	q := s.db.Table("ink_novel").Where("deleted_at IS NULL")
	if search != "" {
		q = q.Where("title LIKE ?", "%"+search+"%")
	}
	var total int64
	q.Count(&total)
	var rows []map[string]interface{}
	err := q.Select("id, title, status, tenant_id, created_at, updated_at").
		Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	return rows, total, err
}

// ── Asset governance ──────────────────────────────────────────────────────────

// TenantStorageInfo holds per-tenant asset usage statistics.
type TenantStorageInfo struct {
	TenantID   uint    `json:"tenant_id"`
	TenantName string  `json:"tenant_name"`
	UsedMB     float64 `json:"used_mb"`
	AssetCount int64   `json:"asset_count"`
}

// GetAssetGovernance returns storage usage grouped by tenant.
func (s *SysAdminService) GetAssetGovernance() ([]TenantStorageInfo, error) {
	var result []TenantStorageInfo
	err := s.db.Raw(`
		SELECT t.id AS tenant_id, t.name AS tenant_name,
		       COALESCE(SUM(a.file_size)/1048576.0, 0) AS used_mb,
		       COUNT(a.id) AS asset_count
		FROM tenants t
		LEFT JOIN ink_asset a ON a.tenant_id = t.id AND a.deleted_at IS NULL
		WHERE t.deleted_at IS NULL
		GROUP BY t.id, t.name
		ORDER BY used_mb DESC
	`).Scan(&result).Error
	return result, err
}

// ── AI infra stats ────────────────────────────────────────────────────────────

// AIInfraStats holds counts of registered AI providers and models.
type AIInfraStats struct {
	ProviderCount int64 `json:"provider_count"`
	ModelCount    int64 `json:"model_count"`
}

// GetAIInfraStats returns counts of active AI providers and models.
func (s *SysAdminService) GetAIInfraStats() (*AIInfraStats, error) {
	var stats AIInfraStats
	s.db.Table("ink_model_provider").Where("deleted_at IS NULL").Count(&stats.ProviderCount)
	s.db.Table("ink_ai_model").Where("deleted_at IS NULL").Count(&stats.ModelCount)
	return &stats, nil
}

// ── Notifications ─────────────────────────────────────────────────────────────

// BroadcastNotification sends a system notification to all active non-admin users.
func (s *SysAdminService) BroadcastNotification(title, content string) error {
	var userIDs []uint
	s.db.Table("users").Where("status = ? AND role != ? AND deleted_at IS NULL", "active", model.RoleSystemAdmin).Pluck("id", &userIDs)
	for _, uid := range userIDs {
		s.db.Exec(
			"INSERT INTO ink_notification (user_id, type, title, content, is_read, created_at, updated_at) VALUES (?, ?, ?, ?, 0, NOW(), NOW())",
			uid, "system", title, content,
		)
	}
	return nil
}

// NotifyTenant sends a system notification to all active users in a specific tenant.
func (s *SysAdminService) NotifyTenant(tenantID uint, title, content string) error {
	var userIDs []uint
	s.db.Table("tenant_users").Where("tenant_id = ? AND status = ? AND deleted_at IS NULL", tenantID, "active").Pluck("user_id", &userIDs)
	for _, uid := range userIDs {
		s.db.Exec(
			"INSERT INTO ink_notification (user_id, type, title, content, is_read, created_at, updated_at) VALUES (?, ?, ?, ?, 0, NOW(), NOW())",
			uid, "system", title, content,
		)
	}
	return nil
}

// ── Experiments ───────────────────────────────────────────────────────────────

// ListExperiments returns a paginated list of AI comparison experiments.
func (s *SysAdminService) ListExperiments(page, size int) ([]map[string]interface{}, int64, error) {
	q := s.db.Table("ink_model_comparison_experiment").Where("deleted_at IS NULL")
	var total int64
	q.Count(&total)
	var rows []map[string]interface{}
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&rows).Error
	return rows, total, err
}
