package service

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// NotificationService 站内通知服务
type NotificationService struct {
	repo *repository.NotificationRepository
}

// NewNotificationService 创建通知服务
func NewNotificationService(repo *repository.NotificationRepository) *NotificationService {
	return &NotificationService{repo: repo}
}

// Send 创建一条通知（核心发送方法，供其他 service 调用）
// 当 s == nil 时静默跳过，其他 service 可安全调用无需 nil 检查。
func (s *NotificationService) Send(tenantID, userID uint, eventType, title, body, entityType string, entityID uint, linkPath string) error {
	if s == nil || s.repo == nil {
		return nil
	}
	return s.repo.Create(&model.Notification{
		TenantID:   tenantID,
		UserID:     userID,
		EventType:  eventType,
		Title:      title,
		Body:       body,
		EntityType: entityType,
		EntityID:   entityID,
		LinkPath:   linkPath,
	})
}

// List 分页查询用户通知
func (s *NotificationService) List(userID, tenantID uint, onlyUnread bool, page, size int) ([]*model.Notification, int64, error) {
	return s.repo.List(userID, tenantID, onlyUnread, page, size)
}

// UnreadCount 未读数
func (s *NotificationService) UnreadCount(userID, tenantID uint) (int64, error) {
	return s.repo.UnreadCount(userID, tenantID)
}

// MarkRead 单条已读
func (s *NotificationService) MarkRead(id, userID uint) error {
	return s.repo.MarkRead(id, userID)
}

// MarkAllRead 全部已读
func (s *NotificationService) MarkAllRead(userID, tenantID uint) error {
	return s.repo.MarkAllRead(userID, tenantID)
}

// Delete 删除通知
func (s *NotificationService) Delete(id, userID uint) error {
	return s.repo.Delete(id, userID)
}
