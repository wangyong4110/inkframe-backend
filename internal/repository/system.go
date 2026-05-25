package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── SystemSettingRepository ────────────────────────────────────────────────

type SystemSettingRepository struct{ db *gorm.DB }

func NewSystemSettingRepository(db *gorm.DB) *SystemSettingRepository {
	return &SystemSettingRepository{db: db}
}

func (r *SystemSettingRepository) Get(key string) (string, error) {
	var s model.SystemSetting
	if err := r.db.First(&s, "key = ?", key).Error; err != nil {
		return "", err
	}
	return s.Value, nil
}

func (r *SystemSettingRepository) Set(key, value, description string) error {
	return r.db.Save(&model.SystemSetting{Key: key, Value: value, Description: description}).Error
}

func (r *SystemSettingRepository) List() ([]model.SystemSetting, error) {
	var items []model.SystemSetting
	err := r.db.Order("key ASC").Find(&items).Error
	return items, err
}

// ─── HookChain / SatisfactionPoint / ConflictArc ─────────────────────────────

// HookChainRepository 钩子链仓库
type HookChainRepository struct{ db *gorm.DB }

func NewHookChainRepository(db *gorm.DB) *HookChainRepository {
	return &HookChainRepository{db: db}
}

func (r *HookChainRepository) Create(h *model.HookChain) error {
	return r.db.Create(h).Error
}

func (r *HookChainRepository) GetByID(id uint) (*model.HookChain, error) {
	var h model.HookChain
	if err := r.db.First(&h, id).Error; err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *HookChainRepository) Update(h *model.HookChain) error {
	return r.db.Save(h).Error
}

func (r *HookChainRepository) Delete(id uint) error {
	return r.db.Delete(&model.HookChain{}, id).Error
}

func (r *HookChainRepository) ListByNovel(novelID uint) ([]*model.HookChain, error) {
	var items []*model.HookChain
	err := r.db.Where("novel_id = ?", novelID).Order("planted_at ASC").Find(&items).Error
	return items, err
}

// ListPending 返回未兑现的钩子
func (r *HookChainRepository) ListPending(novelID uint) ([]*model.HookChain, error) {
	var items []*model.HookChain
	err := r.db.Where("novel_id = ? AND is_fulfilled = false", novelID).Order("planted_at ASC").Find(&items).Error
	return items, err
}

// SatisfactionPointRepository 爽点仓库
type SatisfactionPointRepository struct{ db *gorm.DB }

func NewSatisfactionPointRepository(db *gorm.DB) *SatisfactionPointRepository {
	return &SatisfactionPointRepository{db: db}
}

func (r *SatisfactionPointRepository) Create(sp *model.SatisfactionPoint) error {
	return r.db.Create(sp).Error
}

func (r *SatisfactionPointRepository) GetByID(id uint) (*model.SatisfactionPoint, error) {
	var sp model.SatisfactionPoint
	if err := r.db.First(&sp, id).Error; err != nil {
		return nil, err
	}
	return &sp, nil
}

func (r *SatisfactionPointRepository) Update(sp *model.SatisfactionPoint) error {
	return r.db.Save(sp).Error
}

func (r *SatisfactionPointRepository) Delete(id uint) error {
	return r.db.Delete(&model.SatisfactionPoint{}, id).Error
}

func (r *SatisfactionPointRepository) ListByNovel(novelID uint) ([]*model.SatisfactionPoint, error) {
	var items []*model.SatisfactionPoint
	err := r.db.Where("novel_id = ?", novelID).Order("planned_chapter ASC").Find(&items).Error
	return items, err
}

// ListRecentFulfilled 返回最近N章内已发生的爽点（用于节奏健康检测）
func (r *SatisfactionPointRepository) ListRecentFulfilled(novelID uint, fromChapter int) ([]*model.SatisfactionPoint, error) {
	var items []*model.SatisfactionPoint
	err := r.db.Where("novel_id = ? AND is_planned = false AND planned_chapter >= ?", novelID, fromChapter).
		Find(&items).Error
	return items, err
}

// ConflictArcRepository 冲突弧仓库
type ConflictArcRepository struct{ db *gorm.DB }

func NewConflictArcRepository(db *gorm.DB) *ConflictArcRepository {
	return &ConflictArcRepository{db: db}
}

func (r *ConflictArcRepository) Create(arc *model.ConflictArc) error {
	return r.db.Create(arc).Error
}

func (r *ConflictArcRepository) GetByID(id uint) (*model.ConflictArc, error) {
	var arc model.ConflictArc
	if err := r.db.First(&arc, id).Error; err != nil {
		return nil, err
	}
	return &arc, nil
}

func (r *ConflictArcRepository) Update(arc *model.ConflictArc) error {
	return r.db.Save(arc).Error
}

func (r *ConflictArcRepository) Delete(id uint) error {
	return r.db.Delete(&model.ConflictArc{}, id).Error
}

func (r *ConflictArcRepository) ListByNovel(novelID uint) ([]*model.ConflictArc, error) {
	var items []*model.ConflictArc
	err := r.db.Where("novel_id = ?", novelID).Order("start_chapter ASC").Find(&items).Error
	return items, err
}
