package repository

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	if err := r.db.Where("invite_token = ? AND status = 'pending'", token).First(&m).Error; err != nil {
		return nil, err
	}
	if m.InviteExpiresAt != nil && m.InviteExpiresAt.Before(time.Now()) {
		return nil, gorm.ErrRecordNotFound
	}
	return &m, nil
}

func (r *NovelMemberRepository) ListByNovel(novelID uint) ([]*model.NovelMember, error) {
	var list []*model.NovelMember
	// Include active members plus non-expired pending invites so inviter can track them.
	err := r.db.Where(
		"novel_id = ? AND (status = 'active' OR (status = 'pending' AND (invite_expires_at IS NULL OR invite_expires_at > ?)))",
		novelID, time.Now(),
	).Find(&list).Error
	return list, err
}

// EnsureOwner creates the owner record if it doesn't already exist (upsert-style, race-safe).
func (r *NovelMemberRepository) EnsureOwner(novelID, ownerUserID uint, joinedAt time.Time) error {
	m := &model.NovelMember{
		NovelID:  novelID,
		UserID:   ownerUserID,
		Role:     "owner",
		Status:   "active",
		JoinedAt: &joinedAt,
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(m).Error
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

// Acquire 原子性地获取锁。使用事务+SELECT FOR UPDATE 防止并发竞态。
// 若已有其他用户持有且未过期则返回 (existingLock, false)。
func (r *EditingLockRepository) Acquire(entityType string, entityID, userID uint, userName string, ttl time.Duration) (*model.EditingLock, bool) {
	expires := time.Now().Add(ttl)
	var resultLock model.EditingLock
	acquired := false

	_ = r.db.Transaction(func(tx *gorm.DB) error {
		var lock model.EditingLock
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("entity_type = ? AND entity_id = ?", entityType, entityID).
			First(&lock).Error

		if err != nil {
			// 无锁记录，创建新锁
			lock = model.EditingLock{
				EntityType:   entityType,
				EntityID:     entityID,
				LockedBy:     userID,
				LockedByName: userName,
				ExpiresAt:    expires,
			}
			if cErr := tx.Create(&lock).Error; cErr != nil {
				return cErr
			}
			resultLock = lock
			acquired = true
			return nil
		}

		resultLock = lock

		// 他人持有有效锁
		if lock.LockedBy != userID && time.Now().Before(lock.ExpiresAt) {
			acquired = false
			return nil
		}

		// 自己的锁或已过期，续约/重置
		lock.LockedBy = userID
		lock.LockedByName = userName
		lock.ExpiresAt = expires
		if sErr := tx.Save(&lock).Error; sErr != nil {
			return sErr
		}
		resultLock = lock
		acquired = true
		return nil
	})

	return &resultLock, acquired
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
