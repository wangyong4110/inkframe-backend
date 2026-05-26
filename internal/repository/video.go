package repository

import (
	"errors"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// VideoRepository 视频仓库
type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

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
// 优先使用 ink_videos.tenant_id 直接过滤（无需 JOIN）；
// tenant_id=0 的旧记录视为公共数据，任意租户均可访问（兼容迁移前数据）。
func (r *VideoRepository) GetByIDAndTenant(id, tenantID uint) (*model.Video, error) {
	var video model.Video
	err := r.db.
		Where("id = ? AND (tenant_id = ? OR tenant_id = 0)", id, tenantID).
		First(&video).Error
	if err != nil {
		return nil, err
	}
	return &video, nil
}

// List 获取视频列表
func (r *VideoRepository) List(novelID *uint, chapterID *uint, tenantID uint, page, pageSize int) ([]*model.Video, int64, error) {
	var videos []*model.Video
	var total int64

	query := r.db.Model(&model.Video{}).Session(&gorm.Session{})
	if tenantID > 0 {
		query = query.Where("tenant_id = ? OR tenant_id = 0", tenantID)
	}
	if novelID != nil {
		query = query.Where("novel_id = ?", *novelID)
	}
	if chapterID != nil {
		query = query.Where("chapter_id = ?", *chapterID)
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
	base := r.db.Model(&model.Video{}).Where("is_published = ? AND visibility = ?", true, "public")
	if q != "" {
		base = base.Where("title LIKE ? OR description LIKE ?", "%"+q+"%", "%"+q+"%")
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
	err := r.db.Where("id = ? AND is_published = ? AND visibility = ?", id, true, "public").First(&v).Error
	return &v, err
}

// IncrViewCount 视频播放量+1
func (r *VideoRepository) IncrViewCount(id uint) error {
	return r.db.Model(&model.Video{}).Where("id = ?", id).UpdateColumn("view_count", gorm.Expr("view_count + 1")).Error
}

// IncrLikeCount 点赞数 delta（+1 或 -1），递减时防止低于 0
func (r *VideoRepository) IncrLikeCount(id uint, delta int) error {
	if delta >= 0 {
		return r.db.Model(&model.Video{}).Where("id = ?", id).
			UpdateColumn("like_count", gorm.Expr("like_count + ?", delta)).Error
	}
	// 递减：WHERE like_count > 0 防止下溢
	return r.db.Model(&model.Video{}).Where("id = ? AND like_count > 0", id).
		UpdateColumn("like_count", gorm.Expr("like_count + ?", delta)).Error
}

// IncrCommentCount 评论数 delta（+1 或 -1），递减时防止低于 0
func (r *VideoRepository) IncrCommentCount(id uint, delta int) error {
	if delta >= 0 {
		return r.db.Model(&model.Video{}).Where("id = ?", id).
			UpdateColumn("comment_count", gorm.Expr("comment_count + ?", delta)).Error
	}
	// 递减：WHERE comment_count > 0 防止下溢
	return r.db.Model(&model.Video{}).Where("id = ? AND comment_count > 0", id).
		UpdateColumn("comment_count", gorm.Expr("comment_count + ?", delta)).Error
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
		for _, id := range batch {
			if err := r.db.Model(&model.Video{}).Where("id = ?", id).
				UpdateColumn("hot_score", updates[id]).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// ListPublicForHotCalc 列出所有公开视频用于热度分批量计算
func (r *VideoRepository) ListPublicForHotCalc() ([]*model.Video, error) {
	var videos []*model.Video
	err := r.db.Model(&model.Video{}).Where("is_published = ? AND visibility = ?", true, "public").
		Select("id, view_count, like_count, comment_count, published_at").Find(&videos).Error
	return videos, err
}

// ListPublicForHotCalcPaged 分页列出公开视频用于热度分批量计算（避免全量加载）
func (r *VideoRepository) ListPublicForHotCalcPaged(limit, offset int) ([]*model.Video, error) {
	var videos []*model.Video
	err := r.db.Model(&model.Video{}).Where("is_published = ? AND visibility = ?", true, "public").
		Select("id, view_count, like_count, comment_count, published_at").
		Order("id ASC").Limit(limit).Offset(offset).Find(&videos).Error
	return videos, err
}

// ─── VideoLikeRepository ────────────────────────────────────────────────────

type VideoLikeRepository struct{ db *gorm.DB }

func NewVideoLikeRepository(db *gorm.DB) *VideoLikeRepository {
	return &VideoLikeRepository{db: db}
}

// Toggle 点赞/取消，返回最终状态（事务内执行，防止并发竞态）
func (r *VideoLikeRepository) Toggle(videoID, userID uint) (liked bool, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var like model.VideoLike
		result := tx.Where("video_id = ? AND user_id = ?", videoID, userID).First(&like)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			liked = true
			return tx.Create(&model.VideoLike{VideoID: videoID, UserID: userID}).Error
		}
		if result.Error != nil {
			return result.Error
		}
		liked = false
		return tx.Delete(&like).Error
	})
	return
}

// Exists 检查是否已点赞
func (r *VideoLikeRepository) Exists(videoID, userID uint) (bool, error) {
	var count int64
	err := r.db.Model(&model.VideoLike{}).Where("video_id = ? AND user_id = ?", videoID, userID).Count(&count).Error
	return count > 0, err
}

// ─── VideoCommentRepository ──────────────────────────────────────────────────

type VideoCommentRepository struct{ db *gorm.DB }

func NewVideoCommentRepository(db *gorm.DB) *VideoCommentRepository {
	return &VideoCommentRepository{db: db}
}

func (r *VideoCommentRepository) Create(c *model.VideoComment) error {
	return r.db.Create(c).Error
}

func (r *VideoCommentRepository) ListByVideo(videoID uint, page, size int) ([]*model.VideoComment, int64, error) {
	var list []*model.VideoComment
	var total int64
	base := r.db.Model(&model.VideoComment{}).Where("video_id = ?", videoID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * size
	if err := base.Order("created_at DESC").Offset(offset).Limit(size).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (r *VideoCommentRepository) GetByID(id uint) (*model.VideoComment, error) {
	var c model.VideoComment
	return &c, r.db.First(&c, id).Error
}

func (r *VideoCommentRepository) Delete(id uint) error {
	return r.db.Delete(&model.VideoComment{}, id).Error
}
