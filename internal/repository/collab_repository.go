package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// NovelMemberRepository
type NovelMemberRepository struct{ db *gorm.DB }

func NewNovelMemberRepository(db *gorm.DB) *NovelMemberRepository {
	return &NovelMemberRepository{db: db}
}

func (r *NovelMemberRepository) Create(m *model.NovelMember) error {
	return r.db.Create(m).Error
}

func (r *NovelMemberRepository) GetByNovelAndUser(novelID, userID uint) (*model.NovelMember, error) {
	var m model.NovelMember
	err := r.db.Where("novel_id = ? AND user_id = ?", novelID, userID).First(&m).Error
	return &m, err
}

func (r *NovelMemberRepository) GetByInviteToken(token string) (*model.NovelMember, error) {
	var m model.NovelMember
	err := r.db.Where("invite_token = ? AND status = 'pending'", token).First(&m).Error
	return &m, err
}

func (r *NovelMemberRepository) ListByNovel(novelID uint) ([]*model.NovelMember, error) {
	var list []*model.NovelMember
	err := r.db.Where("novel_id = ? AND status = 'active'", novelID).Find(&list).Error
	return list, err
}

func (r *NovelMemberRepository) Update(m *model.NovelMember) error {
	return r.db.Save(m).Error
}

func (r *NovelMemberRepository) Delete(novelID, userID uint) error {
	return r.db.Where("novel_id = ? AND user_id = ?", novelID, userID).Delete(&model.NovelMember{}).Error
}

func (r *NovelMemberRepository) CountByNovel(novelID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.NovelMember{}).Where("novel_id = ? AND status = 'active'", novelID).Count(&count).Error
	return count, err
}

// EditingLockRepository
type EditingLockRepository struct{ db *gorm.DB }

func NewEditingLockRepository(db *gorm.DB) *EditingLockRepository {
	return &EditingLockRepository{db: db}
}

// Acquire 尝试获取锁。若已有其他用户持有且未过期则返回 false。
func (r *EditingLockRepository) Acquire(entityType string, entityID, userID uint, userName string, ttl time.Duration) (*model.EditingLock, bool) {
	expires := time.Now().Add(ttl)
	var lock model.EditingLock
	result := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).First(&lock)
	if result.Error != nil {
		// 无锁，直接创建
		lock = model.EditingLock{
			EntityType:   entityType,
			EntityID:     entityID,
			LockedBy:     userID,
			LockedByName: userName,
			ExpiresAt:    expires,
		}
		if err := r.db.Create(&lock).Error; err != nil {
			return nil, false
		}
		return &lock, true
	}
	// 已有锁
	if lock.LockedBy != userID && time.Now().Before(lock.ExpiresAt) {
		return &lock, false // 他人持有有效锁
	}
	// 自己的锁或已过期，更新
	lock.LockedBy = userID
	lock.LockedByName = userName
	lock.ExpiresAt = expires
	if err := r.db.Save(&lock).Error; err != nil {
		return nil, false
	}
	return &lock, true
}

func (r *EditingLockRepository) Release(entityType string, entityID, userID uint) error {
	return r.db.Where("entity_type = ? AND entity_id = ? AND locked_by = ?", entityType, entityID, userID).
		Delete(&model.EditingLock{}).Error
}

func (r *EditingLockRepository) Refresh(entityType string, entityID, userID uint, ttl time.Duration) error {
	return r.db.Model(&model.EditingLock{}).
		Where("entity_type = ? AND entity_id = ? AND locked_by = ?", entityType, entityID, userID).
		Update("expires_at", time.Now().Add(ttl)).Error
}

func (r *EditingLockRepository) Get(entityType string, entityID uint) (*model.EditingLock, error) {
	var lock model.EditingLock
	err := r.db.Where("entity_type = ? AND entity_id = ?", entityType, entityID).First(&lock).Error
	if err != nil {
		return nil, err
	}
	if time.Now().After(lock.ExpiresAt) {
		r.db.Delete(&lock)
		return nil, gorm.ErrRecordNotFound
	}
	return &lock, nil
}

func (r *EditingLockRepository) CleanupExpired() {
	r.db.Where("expires_at < ?", time.Now()).Delete(&model.EditingLock{})
}
