package repository

import (
	"errors"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// OutlineReviewRepository 章节大纲审查结果仓库
type OutlineReviewRepository struct {
	db *gorm.DB
}

func NewOutlineReviewRepository(db *gorm.DB) *OutlineReviewRepository {
	return &OutlineReviewRepository{db: db}
}

// Upsert 按章节 ID 更新（存在则覆盖，否则创建）
func (r *OutlineReviewRepository) Upsert(review *model.OutlineReview) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var existing model.OutlineReview
		err := tx.Where("chapter_id = ?", review.ChapterID).First(&existing).Error
		if err == nil {
			review.ID = existing.ID
			review.CreatedAt = existing.CreatedAt
			return tx.Save(review).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(review).Error
	})
}

// GetByChapter 获取章节最新审查结果
func (r *OutlineReviewRepository) GetByChapter(chapterID uint) (*model.OutlineReview, error) {
	var review model.OutlineReview
	return &review, r.db.Where("chapter_id = ?", chapterID).First(&review).Error
}

// ListByNovel 获取小说所有章节审查结果（按章节号升序）
func (r *OutlineReviewRepository) ListByNovel(novelID uint) ([]*model.OutlineReview, error) {
	var reviews []*model.OutlineReview
	return reviews, r.db.Where("novel_id = ?", novelID).Order("chapter_no ASC").Find(&reviews).Error
}

// DeleteByNovel 删除小说所有章节审查结果
func (r *OutlineReviewRepository) DeleteByNovel(novelID uint) error {
	return r.db.Where("novel_id = ?", novelID).Delete(&model.OutlineReview{}).Error
}

// NovelOutlineSynthesisRepository 小说整体大纲综合报告仓库
type NovelOutlineSynthesisRepository struct {
	db *gorm.DB
}

func NewNovelOutlineSynthesisRepository(db *gorm.DB) *NovelOutlineSynthesisRepository {
	return &NovelOutlineSynthesisRepository{db: db}
}

// Upsert 按 novel_id 唯一，存在则全量覆盖，不存在则创建
func (r *NovelOutlineSynthesisRepository) Upsert(s *model.NovelOutlineSynthesis) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var existing model.NovelOutlineSynthesis
		err := tx.Where("novel_id = ?", s.NovelID).First(&existing).Error
		if err == nil {
			s.ID = existing.ID
			s.CreatedAt = existing.CreatedAt
			return tx.Save(s).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(s).Error
	})
}

// GetByNovel 获取小说最新综合报告
func (r *NovelOutlineSynthesisRepository) GetByNovel(novelID uint) (*model.NovelOutlineSynthesis, error) {
	var s model.NovelOutlineSynthesis
	return &s, r.db.Where("novel_id = ?", novelID).First(&s).Error
}
