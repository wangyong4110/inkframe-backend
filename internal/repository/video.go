package repository

import (
	"errors"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VideoRepository 视频仓库
type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

// DB exposes the underlying *gorm.DB for use in transactions.
func (r *VideoRepository) DB() *gorm.DB { return r.db }

// Create 创建视频
func (r *VideoRepository) Create(video *model.Video) error {
	// Normalize chapter_id=0 to NULL to avoid FK constraint failures
	if video.ChapterID != nil && *video.ChapterID == 0 {
		video.ChapterID = nil
	}
	return r.db.Create(video).Error
}

// GetByID 根据ID获取视频
func (r *VideoRepository) GetByID(id uint) (*model.Video, error) {
	var video model.Video
	if err := r.db.First(&video, id).Error; err != nil {
		return nil, err
	}
	return &video, nil
}

// GetByIDAndTenant 根据ID和租户获取视频（防止越权访问）
// 通过 novel.tenant_id 校验归属（novel_id → ink_novel.tenant_id）。
// tenant_id=0 的小说视为公共数据，任意租户均可访问。
func (r *VideoRepository) GetByIDAndTenant(id, tenantID uint) (*model.Video, error) {
	var video model.Video
	err := r.db.
		Where("id = ? AND novel_id IN (SELECT id FROM ink_novel WHERE (tenant_id = ? OR tenant_id = 0) AND deleted_at IS NULL)", id, tenantID).
		First(&video).Error
	if err != nil {
		return nil, err
	}
	return &video, nil
}

// List 获取视频列表
func (r *VideoRepository) List(novelID *uint, chapterID *uint, status string, tenantID uint, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64

	query := r.db.Model(&model.Video{}).Session(&gorm.Session{})
	if tenantID > 0 {
		query = query.Where("novel_id IN (SELECT id FROM ink_novel WHERE (tenant_id = ? OR tenant_id = 0) AND deleted_at IS NULL)", tenantID)
	}
	if novelID != nil {
		query = query.Where("novel_id = ?", *novelID)
	}
	if chapterID != nil {
		query = query.Where("chapter_id = ?", *chapterID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&videos).Error; err != nil {
		return nil, 0, err
	}

	return videos, total, nil
}

// Update 更新视频
func (r *VideoRepository) Update(video *model.Video) error {
	return r.db.Save(video).Error
}

// DeleteByID 删除视频
func (r *VideoRepository) DeleteByID(id uint) error {
	return r.db.Delete(&model.Video{}, id).Error
}

// UpdateFields 更新视频任意字段
func (r *VideoRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).Updates(fields).Error
}

// ListPublic 列出公开发布的视频（用于广场）
func (r *VideoRepository) ListPublic(page, pageSize int) ([]*model.Video, int64, error) {
	return r.ListPublicSorted("hot", "", page, pageSize)
}

// ListPublicSorted 排序列出公开视频（sort: latest|hot；q: 关键词搜索）
func (r *VideoRepository) ListPublicSorted(sort, q string, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64
	base := r.db.Model(&model.Video{}).
		Where("is_published = ? AND visibility = ?", true, "public")
	if q != "" {
		base = base.Where("title LIKE ?", "%"+q+"%")
	}
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "hot_score DESC, published_at DESC"
	if sort == "latest" {
		order = "published_at DESC, created_at DESC"
	}
	offset := (page - 1) * pageSize
	if err := base.Order(order).Offset(offset).Limit(pageSize).Find(&videos).Error; err != nil {
		return nil, 0, err
	}
	return videos, total, nil
}

// GetPublicByID 获取单条公开视频（不需要 tenantID）
func (r *VideoRepository) GetPublicByID(id uint) (*model.Video, error) {
	var v model.Video
	err := r.db.Where("id = ? AND is_published = ? AND visibility = ?", id, true, "public").
		First(&v).Error
	return &v, err
}

// incrStat 通用统计字段原子递增（ON DUPLICATE KEY UPDATE via GORM clause）
func (r *VideoRepository) incrStat(id uint, field string, delta interface{}) {
	r.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			field:        delta,
			"updated_at": gorm.Expr("NOW()"),
		}),
	}).Create(&model.ContentStats{EntityType: "video", EntityID: id})
}

// IncrViewCount 视频播放量+1（写入 ink_content_stats）
func (r *VideoRepository) IncrViewCount(id uint) error {
	r.incrStat(id, "view_count", gorm.Expr("view_count + 1"))
	return nil
}

// IncrLikeCount 点赞数 delta（+1 或 -1，写入 ink_content_stats）
func (r *VideoRepository) IncrLikeCount(id uint, delta int) error {
	r.incrStat(id, "like_count", gorm.Expr("GREATEST(0, like_count + ?)", delta))
	return nil
}

// IncrCommentCount 评论数 delta（写入 ink_content_stats）
func (r *VideoRepository) IncrCommentCount(id uint, delta int) error {
	r.incrStat(id, "comment_count", gorm.Expr("GREATEST(0, comment_count + ?)", delta))
	return nil
}

// UpdateHotScore 更新热度分
func (r *VideoRepository) UpdateHotScore(id uint, score float64) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).Update("hot_score", score).Error
}

// BatchUpdateHotScores 批量更新热度分（分批次处理，防止单次 SQL 过大）
func (r *VideoRepository) BatchUpdateHotScores(updates map[uint]float64) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(updates))
	for id := range updates {
		ids = append(ids, id)
	}
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		if err := r.db.Transaction(func(tx *gorm.DB) error {
			for _, id := range batch {
				if err := tx.Model(&model.Video{}).Where("id = ?", id).
					UpdateColumn("hot_score", updates[id]).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// VideoHotCalcRow 热度分计算辅助行（JOIN content_stats 结果）
type VideoHotCalcRow struct {
	ID           uint
	PublishedAt  *time.Time
	ViewCount    int
	LikeCount    int
	CommentCount int
}

// ListPublicForHotCalc 列出所有公开视频用于热度分批量计算（JOIN ink_content_stats）
func (r *VideoRepository) ListPublicForHotCalc() ([]VideoHotCalcRow, error) {
	var rows []VideoHotCalcRow
	err := r.db.Raw(`SELECT v.id,
		v.published_at,
		COALESCE(cs.view_count, 0) AS view_count,
		COALESCE(cs.like_count, 0) AS like_count,
		COALESCE(cs.comment_count, 0) AS comment_count
		FROM ink_video v
		LEFT JOIN ink_content_stats cs ON cs.entity_type = 'video' AND cs.entity_id = v.id
		WHERE v.is_published = 1
		  AND v.visibility = 'public'
		  AND v.deleted_at IS NULL
		LIMIT 10000`).
		Scan(&rows).Error
	return rows, err
}

// ListPublicForHotCalcPaged 分页列出公开视频用于热度分批量计算（JOIN ink_content_stats，避免全量加载）
func (r *VideoRepository) ListPublicForHotCalcPaged(limit, offset int) ([]VideoHotCalcRow, error) {
	var rows []VideoHotCalcRow
	err := r.db.Raw(`SELECT v.id,
		v.published_at,
		COALESCE(cs.view_count, 0) AS view_count,
		COALESCE(cs.like_count, 0) AS like_count,
		COALESCE(cs.comment_count, 0) AS comment_count
		FROM ink_video v
		LEFT JOIN ink_content_stats cs ON cs.entity_type = 'video' AND cs.entity_id = v.id
		WHERE v.is_published = 1
		  AND v.visibility = 'public'
		  AND v.deleted_at IS NULL
		ORDER BY v.id ASC LIMIT ? OFFSET ?`, limit, offset).
		Scan(&rows).Error
	return rows, err
}

// HydrateVideoStats 批量填充视频统计字段（ViewCount/LikeCount/CommentCount）
func (r *VideoRepository) HydrateVideoStats(videos []*model.Video) {
	if len(videos) == 0 {
		return
	}
	ids := make([]uint, 0, len(videos))
	for _, v := range videos {
		ids = append(ids, v.ID)
	}
	var rows []struct {
		EntityID     uint
		ViewCount    int
		LikeCount    int
		CommentCount int
	}
	r.db.Raw(`SELECT entity_id, view_count, like_count, comment_count
		FROM ink_content_stats WHERE entity_type = 'video' AND entity_id IN ?`, ids).Scan(&rows)
	statsMap := make(map[uint]struct{ v, l, c int }, len(rows))
	for _, row := range rows {
		statsMap[row.EntityID] = struct{ v, l, c int }{row.ViewCount, row.LikeCount, row.CommentCount}
	}
	for _, vid := range videos {
		if s, ok := statsMap[vid.ID]; ok {
			vid.ViewCount = s.v
			vid.LikeCount = s.l
			vid.CommentCount = s.c
		}
	}
}

// ─── VideoLikeRepository ────────────────────────────────────────────────────

type VideoLikeRepository struct{ db *gorm.DB }

func NewVideoLikeRepository(db *gorm.DB) *VideoLikeRepository {
	return &VideoLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态。
func (r *VideoLikeRepository) Toggle(videoID, userID uint) (liked bool, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var like model.EntityLike
		result := tx.Where("entity_type = 'video' AND entity_id = ? AND user_id = ?", videoID, userID).First(&like)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			liked = true
			if err2 := tx.Create(&model.EntityLike{EntityType: "video", EntityID: videoID, UserID: userID}).Error; err2 != nil {
				return err2
			}
			return tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "entity_type"}, {Name: "entity_id"}},
				DoUpdates: clause.Assignments(map[string]interface{}{
					"like_count": gorm.Expr("like_count + 1"),
					"updated_at": gorm.Expr("NOW()"),
				}),
			}).Create(&model.ContentStats{EntityType: "video", EntityID: videoID}).Error
		}
		if result.Error != nil {
			return result.Error
		}
		liked = false
		if err2 := tx.Delete(&like).Error; err2 != nil {
			return err2
		}
		return tx.Model(&model.ContentStats{}).
			Where("entity_type = 'video' AND entity_id = ?", videoID).
			Updates(map[string]interface{}{
				"like_count": gorm.Expr("GREATEST(0, like_count - 1)"),
				"updated_at": gorm.Expr("NOW()"),
			}).Error
	})
	return
}

// Exists 检查是否已点赞
func (r *VideoLikeRepository) Exists(videoID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.EntityLike{}).Where("entity_type = 'video' AND entity_id = ? AND user_id = ?", videoID, userID).Count(&count).Error
	return count > 0, err
}

// ─── VideoCommentRepository ──────────────────────────────────────────────────

type VideoCommentRepository struct{ db *gorm.DB }

func NewVideoCommentRepository(db *gorm.DB) *VideoCommentRepository {
	return &VideoCommentRepository{db: db}
}

func (r *VideoCommentRepository) Create(c *model.VideoComment) error {
	ec := &model.EntityComment{
		EntityType: "video", EntityID: c.VideoID,
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

func (r *VideoCommentRepository) ListByVideo(videoID uint, page, size int) ([]*model.VideoComment, int64, error) {
	var list []*model.EntityComment
	var total int64
	base := r.db.Model(&model.EntityComment{}).Where("entity_type = 'video' AND entity_id = ?", videoID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	if err := base.Order("created_at DESC").Offset(offset).Limit(size).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*model.VideoComment, len(list))
	for i, ec := range list {
		out[i] = &model.VideoComment{ID: ec.ID, VideoID: ec.EntityID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}
	}
	return out, total, nil
}

func (r *VideoCommentRepository) GetByID(id uint) (*model.VideoComment, error) {
	var ec model.EntityComment
	if err := r.db.Where("entity_type = 'video' AND id = ?", id).First(&ec).Error; err != nil {
		return nil, err
	}
	return &model.VideoComment{ID: ec.ID, VideoID: ec.EntityID, UserID: ec.UserID, Content: ec.Content, ParentID: ec.ParentID, CreatedAt: ec.CreatedAt, UpdatedAt: ec.UpdatedAt}, nil
}

func (r *VideoCommentRepository) Delete(id uint) error {
	return r.db.Where("entity_type = 'video' AND id = ?", id).Delete(&model.EntityComment{}).Error
}
