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

// Toggle 点赞/取消，返回最终状态及最新 like_count，同时原子更新 ink_content_stats。
func (r *ChapterLikeRepository) Toggle(chapterID, novelID, userID uint) (liked bool, likeCount int, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var existing model.EntityLike
		result := tx.Where("entity_type = 'chapter' AND entity_id = ? AND user_id = ?", chapterID, userID).First(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			if err2 := tx.Create(&model.EntityLike{EntityType: "chapter", EntityID: chapterID, UserID: userID, NovelID: novelID}).Error; err2 != nil {
				return err2
			}
			liked = true
			if err2 := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
				DoUpdates: clause.Assignments(map[string]interface{}{
					"like_count": gorm.Expr("like_count + 1"),
					"updated_at": gorm.Expr("NOW()"),
				}),
			}).Create(&model.ContentStats{EntityType: "chapter", EntityID: chapterID}).Error; err2 != nil {
				return err2
			}
		} else if result.Error != nil {
			return result.Error
		} else {
			if err2 := tx.Where("entity_type = 'chapter' AND entity_id = ? AND user_id = ?", chapterID, userID).Delete(&model.EntityLike{}).Error; err2 != nil {
				return err2
			}
			liked = false
			if err2 := tx.Model(&model.ContentStats{}).
				Where("entity_type = 'chapter' AND entity_id = ?", chapterID).
				Updates(map[string]interface{}{
					"like_count": gorm.Expr("GREATEST(0, like_count - 1)"),
					"updated_at": gorm.Expr("NOW()"),
				}).Error; err2 != nil {
				return err2
			}
		}
		var cnt int64
		tx.Model(&model.EntityLike{}).Where("entity_type = 'chapter' AND entity_id = ?", chapterID).Count(&cnt)
		likeCount = int(cnt)
		return nil
	})
	return
}

// Exists 是否已点赞
func (r *ChapterLikeRepository) Exists(chapterID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.EntityLike{}).
		Where("entity_type = 'chapter' AND entity_id = ? AND user_id = ?", chapterID, userID).Count(&count).Error
	return count > 0, err
}

// ─── ChapterCommentRepository ─────────────────────────────────────────────────

type ChapterCommentRepository struct{ db *gorm.DB }

func NewChapterCommentRepository(db *gorm.DB) *ChapterCommentRepository {
	return &ChapterCommentRepository{db: db}
}

func (r *ChapterCommentRepository) Create(c *model.ChapterComment) error {
	ec := &model.EntityComment{
		EntityType: "chapter", EntityID: c.ChapterID, NovelID: c.NovelID,
		UserID: c.UserID, Content: c.Content, ParentID: c.ParentID,
	}
	if err := r.db.Create(ec).Error; err != nil {
		return err
	}
	c.ID = ec.ID
	c.CreatedAt = ec.CreatedAt
	c.UpdatedAt = ec.UpdatedAt
	return nil
}

func (r *ChapterCommentRepository) ListByChapter(chapterID uint, page, size int) ([]*model.ChapterComment, int64, error) {
	var list []*model.EntityComment
	var total int64
	base := r.db.Model(&model.EntityComment{}).Where("entity_type = 'chapter' AND entity_id = ?", chapterID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	if err := base.Order("created_at ASC").Offset(offset).Limit(size).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*model.ChapterComment, len(list))
	for i, ec := range list {
		out[i] = &model.ChapterComment{ID: ec.ID, ChapterID: ec.EntityID, NovelID: ec.NovelID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}
	}
	return out, total, nil
}

func (r *ChapterCommentRepository) GetByID(id uint) (*model.ChapterComment, error) {
	var ec model.EntityComment
	if err := r.db.Where("entity_type = 'chapter' AND id = ?", id).First(&ec).Error; err != nil {
		return nil, err
	}
	return &model.ChapterComment{ID: ec.ID, ChapterID: ec.EntityID, NovelID: ec.NovelID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}, nil
}

func (r *ChapterCommentRepository) Delete(id uint) error {
	return r.db.Where("entity_type = 'chapter' AND id = ?", id).Delete(&model.EntityComment{}).Error
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
