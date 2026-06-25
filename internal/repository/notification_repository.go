package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// NotificationRepository 站内通知仓库
type NotificationRepository struct{ db *gorm.DB }

// NewNotificationRepository 创建通知仓库
func NewNotificationRepository(db *gorm.DB) *NotificationRepository {
	return &NotificationRepository{db: db}
}

// Create 创建通知
func (r *NotificationRepository) Create(n *model.Notification) error {
	return r.db.Create(n).Error
}

// List 分页查询用户通知（最新在前）。
// 仅按 user_id 过滤，跨租户协作邀请通知也可正常显示。
func (r *NotificationRepository) List(userID, tenantID uint, onlyUnread bool, page, size int) ([]*model.Notification, int64, error) {
	var total int64
	var items []*model.Notification
	q := r.db.Model(&model.Notification{}).Where("user_id = ?", userID)
	if onlyUnread {
		q = q.Where("is_read = ?", false)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	err := q.Order("created_at DESC").Offset((page-1)*size).Limit(size).Find(&items).Error
	return items, total, err
}

// UnreadCount 未读数（按 user_id 过滤）。
func (r *NotificationRepository) UnreadCount(userID, tenantID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.Notification{}).
		Where("user_id = ? AND is_read = ?", userID, false).
		Count(&count).Error
	return count, err
}

// MarkRead 标记单条已读（同时记录已读时间戳）
func (r *NotificationRepository) MarkRead(id, userID uint) error {
	now := time.Now()
	return r.db.Model(&model.Notification{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]interface{}{"is_read": true, "read_at": now}).Error
}

// MarkAllRead 标记全部已读（同时记录已读时间戳）
func (r *NotificationRepository) MarkAllRead(userID, tenantID uint) error {
	now := time.Now()
	return r.db.Model(&model.Notification{}).
		Where("user_id = ? AND is_read = ?", userID, false).
		Updates(map[string]interface{}{"is_read": true, "read_at": now}).Error
}

// Delete 删除单条（软删除）
func (r *NotificationRepository) Delete(id, userID uint) error {
	return r.db.Where("id = ? AND user_id = ?", id, userID).Delete(&model.Notification{}).Error
}
