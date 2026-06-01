package repository

import (
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
	var existing model.OutlineReview
	err := r.db.Where("chapter_id = ?", review.ChapterID).First(&existing).Error
	if err == nil {
		review.ID = existing.ID
		return r.db.Save(review).Error
	}
	return r.db.Create(review).Error
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
