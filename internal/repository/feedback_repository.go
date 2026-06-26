package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

type FeedbackRepository struct {
	db *gorm.DB
}

func NewFeedbackRepository(db *gorm.DB) *FeedbackRepository {
	return &FeedbackRepository{db: db}
}

func (r *FeedbackRepository) Create(f *model.UserFeedback) error {
	var maxSeqNo uint
	r.db.Raw("SELECT COALESCE(MAX(seq_no), 0) + 1 FROM ink_user_feedback").Scan(&maxSeqNo)
	f.SeqNo = maxSeqNo
	return r.db.Create(f).Error
}

func (r *FeedbackRepository) List(page, size int, status, typ, priority string) ([]*model.UserFeedback, int64, error) {
	var items []*model.UserFeedback
	var total int64
	q := r.db.Model(&model.UserFeedback{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if typ != "" {
		q = q.Where("type = ?", typ)
	}
	if priority != "" {
		q = q.Where("priority = ?", priority)
	}
	q.Count(&total)
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&items).Error
	return items, total, err
}

func (r *FeedbackRepository) ListByUser(userID uint, page, size int) ([]*model.UserFeedback, int64, error) {
	var items []*model.UserFeedback
	var total int64
	q := r.db.Model(&model.UserFeedback{}).Where("user_id = ?", userID)
	q.Count(&total)
	err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&items).Error
	return items, total, err
}

func (r *FeedbackRepository) GetByID(id uint) (*model.UserFeedback, error) {
	var f model.UserFeedback
	err := r.db.First(&f, id).Error
	return &f, err
}

func (r *FeedbackRepository) Update(f *model.UserFeedback) error {
	return r.db.Save(f).Error
}

func (r *FeedbackRepository) GetStats() (map[string]interface{}, error) {
	var total, pending, reviewing, resolved int64
	r.db.Model(&model.UserFeedback{}).Count(&total)
	r.db.Model(&model.UserFeedback{}).Where("status = ?", "pending").Count(&pending)
	r.db.Model(&model.UserFeedback{}).Where("status = ?", "reviewing").Count(&reviewing)
	r.db.Model(&model.UserFeedback{}).Where("status = ?", "resolved").Count(&resolved)

	type typeCount struct {
		Type  string
		Count int64
	}
	var typeCounts []typeCount
	r.db.Model(&model.UserFeedback{}).Select("type, COUNT(*) as count").Group("type").Scan(&typeCounts)
	byType := make(map[string]int64)
	for _, tc := range typeCounts {
		byType[tc.Type] = tc.Count
	}

	var avgRating float64
	r.db.Model(&model.UserFeedback{}).Where("rating > 0").Select("COALESCE(AVG(rating), 0)").Scan(&avgRating)

	type dailyCount struct {
		Date  string
		Count int64
	}
	var dailyCounts []dailyCount
	since := time.Now().AddDate(0, 0, -6)
	r.db.Model(&model.UserFeedback{}).
		Where("created_at >= ?", since).
		Select("DATE(created_at) as date, COUNT(*) as count").
		Group("DATE(created_at)").
		Order("date ASC").
		Scan(&dailyCounts)

	daily := make([]map[string]interface{}, len(dailyCounts))
	for i, d := range dailyCounts {
		daily[i] = map[string]interface{}{"date": d.Date, "count": d.Count}
	}

	return map[string]interface{}{
		"total":        total,
		"pending":      pending,
		"reviewing":    reviewing,
		"resolved":     resolved,
		"by_type":      byType,
		"avg_rating":   avgRating,
		"daily_counts": daily,
	}, nil
}
