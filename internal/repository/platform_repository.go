package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// PlatformAccountRepository handles platform account data
type PlatformAccountRepository struct {
	db *gorm.DB
}

func NewPlatformAccountRepository(db *gorm.DB) *PlatformAccountRepository {
	return &PlatformAccountRepository{db: db}
}

func (r *PlatformAccountRepository) Create(a *model.PlatformAccount) error {
	return r.db.Create(a).Error
}

func (r *PlatformAccountRepository) GetByID(id uint) (*model.PlatformAccount, error) {
	var a model.PlatformAccount
	if err := r.db.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PlatformAccountRepository) ListByTenant(tenantID uint) ([]*model.PlatformAccount, error) {
	var accounts []*model.PlatformAccount
	err := r.db.Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&accounts).Error
	return accounts, err
}

func (r *PlatformAccountRepository) UpdateStatus(id uint, status string) error {
	return r.db.Model(&model.PlatformAccount{}).Where("id = ?", id).Update("status", status).Error
}

func (r *PlatformAccountRepository) UpdateTokens(id uint, accessToken, refreshToken string, expiresAt *time.Time) error {
	// Encrypt tokens before persisting (BeforeSave hook does not fire on map-based Updates).
	encAccess, _ := model.EncryptField(accessToken)
	encRefresh, _ := model.EncryptField(refreshToken)
	return r.db.Model(&model.PlatformAccount{}).Where("id = ?", id).Updates(map[string]interface{}{
		"access_token":  encAccess,
		"refresh_token": encRefresh,
		"expires_at":    expiresAt,
		"status":        "active",
	}).Error
}

func (r *PlatformAccountRepository) Delete(id uint) error {
	return r.db.Delete(&model.PlatformAccount{}, id).Error
}

// VideoPublishRecordRepository handles video publish record data
type VideoPublishRecordRepository struct {
	db *gorm.DB
}

func NewVideoPublishRecordRepository(db *gorm.DB) *VideoPublishRecordRepository {
	return &VideoPublishRecordRepository{db: db}
}

func (r *VideoPublishRecordRepository) Create(rec *model.VideoPublishRecord) error {
	return r.db.Create(rec).Error
}

func (r *VideoPublishRecordRepository) GetByID(id uint) (*model.VideoPublishRecord, error) {
	var rec model.VideoPublishRecord
	if err := r.db.First(&rec, id).Error; err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *VideoPublishRecordRepository) ListByVideo(videoID uint) ([]*model.VideoPublishRecord, error) {
	var records []*model.VideoPublishRecord
	err := r.db.Where("video_id = ?", videoID).Order("created_at DESC").Find(&records).Error
	return records, err
}

func (r *VideoPublishRecordRepository) UpdateStatus(id uint, status, errMsg, externalID, externalURL string) error {
	updates := map[string]interface{}{
		"status":   status,
		"err_msg":  errMsg,
	}
	if externalID != "" {
		updates["external_id"] = externalID
	}
	if externalURL != "" {
		updates["external_url"] = externalURL
	}
	if status == "published" {
		now := time.Now()
		updates["published_at"] = &now
	}
	return r.db.Model(&model.VideoPublishRecord{}).Where("id = ?", id).Updates(updates).Error
}
