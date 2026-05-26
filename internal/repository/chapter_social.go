package repository

import (
	"errors"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ─── ChapterLikeRepository ───────────────────────────────────────────────────

type ChapterLikeRepository struct{ db *gorm.DB }

func NewChapterLikeRepository(db *gorm.DB) *ChapterLikeRepository {
	return &ChapterLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态及最新 like_count
func (r *ChapterLikeRepository) Toggle(chapterID, novelID, userID uint) (liked bool, likeCount int, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var existing model.ChapterLike
		result := tx.Where("chapter_id = ? AND user_id = ?", chapterID, userID).First(&existing)
		if result.Error != nil {
			// Not found → create
			if err2 := tx.Create(&model.ChapterLike{ChapterID: chapterID, UserID: userID, NovelID: novelID}).Error; err2 != nil {
				return err2
			}
			liked = true
			return tx.Model(&model.Chapter{}).Where("id = ?", chapterID).
				UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error
		}
		// Found → delete (unlike)
		if err2 := tx.Delete(&model.ChapterLike{}, "chapter_id = ? AND user_id = ?", chapterID, userID).Error; err2 != nil {
			return err2
		}
		liked = false
		return tx.Model(&model.Chapter{}).Where("id = ?", chapterID).
			UpdateColumn("like_count", gorm.Expr("GREATEST(0, like_count - 1)")).Error
	})
	if err != nil {
		return
	}
	var ch model.Chapter
	if e := r.db.Select("like_count").First(&ch, chapterID).Error; e == nil {
		likeCount = ch.LikeCount
	}
	return
}

// Exists 是否已点赞
func (r *ChapterLikeRepository) Exists(chapterID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.ChapterLike{}).
		Where("chapter_id = ? AND user_id = ?", chapterID, userID).Count(&count).Error
	return count > 0, err
}

// ─── ChapterCommentRepository ─────────────────────────────────────────────────

type ChapterCommentRepository struct{ db *gorm.DB }

func NewChapterCommentRepository(db *gorm.DB) *ChapterCommentRepository {
	return &ChapterCommentRepository{db: db}
}

func (r *ChapterCommentRepository) Create(c *model.ChapterComment) error {
	return r.db.Create(c).Error
}

func (r *ChapterCommentRepository) ListByChapter(chapterID uint, page, size int) ([]*model.ChapterComment, int64, error) {
	var list []*model.ChapterComment
	var total int64
	base := r.db.Model(&model.ChapterComment{}).Where("chapter_id = ?", chapterID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	err := base.Order("created_at ASC").Offset(offset).Limit(size).Find(&list).Error
	return list, total, err
}

func (r *ChapterCommentRepository) GetByID(id uint) (*model.ChapterComment, error) {
	var c model.ChapterComment
	err := r.db.First(&c, id).Error
	return &c, err
}

func (r *ChapterCommentRepository) Delete(id uint) error {
	return r.db.Delete(&model.ChapterComment{}, id).Error
}

// ─── ReadingProgressRepository ───────────────────────────────────────────────

type ReadingProgressRepository struct{ db *gorm.DB }

func NewReadingProgressRepository(db *gorm.DB) *ReadingProgressRepository {
	return &ReadingProgressRepository{db: db}
}

// Upsert 保存或更新阅读进度（仅在新 chapterNo 更大，或同章节时才更新 scroll）
func (r *ReadingProgressRepository) Upsert(userID, novelID uint, chapterNo int, chapterID uint, scrollPct int) error {
	now := time.Now()
	prog := model.ReadingProgress{
		UserID: userID, NovelID: novelID,
		ChapterNo: chapterNo, ChapterID: chapterID,
		ScrollPct: scrollPct, UpdatedAt: now, CreatedAt: now,
	}
	return r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "novel_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"chapter_no": gorm.Expr("CASE WHEN excluded.chapter_no >= chapter_no THEN excluded.chapter_no ELSE chapter_no END"),
			"chapter_id": gorm.Expr("CASE WHEN excluded.chapter_no >= chapter_no THEN excluded.chapter_id ELSE chapter_id END"),
			"scroll_pct": gorm.Expr("CASE WHEN excluded.chapter_no > chapter_no THEN excluded.scroll_pct WHEN excluded.chapter_no = chapter_no THEN excluded.scroll_pct ELSE scroll_pct END"),
			"updated_at": now,
		}),
	}).Create(&prog).Error
}

// Get 获取用户对某小说的阅读进度（nil = 未读过）
func (r *ReadingProgressRepository) Get(userID, novelID uint) (*model.ReadingProgress, error) {
	var p model.ReadingProgress
	err := r.db.Where("user_id = ? AND novel_id = ?", userID, novelID).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

// ListByUser 获取用户的阅读历史（按最近阅读时间排序）
func (r *ReadingProgressRepository) ListByUser(userID uint, page, size int) ([]*model.ReadingProgress, int64, error) {
	var list []*model.ReadingProgress
	var total int64
	base := r.db.Model(&model.ReadingProgress{}).Where("user_id = ?", userID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	err := base.Order("updated_at DESC").Offset(offset).Limit(size).Find(&list).Error
	return list, total, err
}

// ─── ChapterReadRecordRepository ─────────────────────────────────────────────

type ChapterReadRecordRepository struct{ db *gorm.DB }

func NewChapterReadRecordRepository(db *gorm.DB) *ChapterReadRecordRepository {
	return &ChapterReadRecordRepository{db: db}
}

// MarkRead 标记章节已读（幂等）
func (r *ChapterReadRecordRepository) MarkRead(userID, chapterID, novelID uint) error {
	rec := model.ChapterReadRecord{UserID: userID, ChapterID: chapterID, NovelID: novelID, ReadAt: time.Now()}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec).Error
}

// ListReadChapterIDs 获取用户在某小说中已读的章节 ID 列表
func (r *ChapterReadRecordRepository) ListReadChapterIDs(userID, novelID uint) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&model.ChapterReadRecord{}).
		Where("user_id = ? AND novel_id = ?", userID, novelID).
		Pluck("chapter_id", &ids).Error
	return ids, err
}
