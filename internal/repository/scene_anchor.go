package repository

import (
	"errors"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── SceneAnchorRepository 场景锚点仓库 ──────────────────────────────────────

type SceneAnchorRepository struct{ db *gorm.DB }

func NewSceneAnchorRepository(db *gorm.DB) *SceneAnchorRepository {
	return &SceneAnchorRepository{db: db}
}

func (r *SceneAnchorRepository) DB() *gorm.DB { return r.db }

func (r *SceneAnchorRepository) Create(a *model.SceneAnchor) error {
	return r.db.Create(a).Error
}

// FindByNovelAndNameUnscoped 包含软删除记录的查找（用于判断 idx_scene_anchor_novel_name
// 唯一索引是否被软删除记录占用）。返回 nil, nil 表示完全不存在。
func (r *SceneAnchorRepository) FindByNovelAndNameUnscoped(novelID uint, name string) (*model.SceneAnchor, error) {
	var a model.SceneAnchor
	err := r.db.Unscoped().Where("novel_id = ? AND name = ?", novelID, name).First(&a).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// RestoreByID 恢复软删除的场景锚点（清空 deleted_at）。
func (r *SceneAnchorRepository) RestoreByID(id uint) error {
	return r.db.Unscoped().Model(&model.SceneAnchor{}).
		Where("id = ?", id).
		Update("deleted_at", nil).Error
}

func (r *SceneAnchorRepository) GetByID(id uint) (*model.SceneAnchor, error) {
	var a model.SceneAnchor
	if err := r.db.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *SceneAnchorRepository) Update(a *model.SceneAnchor) error {
	return r.db.Save(a).Error
}

// UpdateFields 只更新指定字段，不影响其他列（防止全量读-改-写导致的 lost-update 并发问题）
func (r *SceneAnchorRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.SceneAnchor{}).Where("id = ?", id).Updates(fields).Error
}

func (r *SceneAnchorRepository) Delete(id uint) error {
	return r.db.Delete(&model.SceneAnchor{}, id).Error
}

func (r *SceneAnchorRepository) ListByNovel(novelID uint) ([]*model.SceneAnchor, error) {
	var items []*model.SceneAnchor
	err := r.db.Where("novel_id = ?", novelID).Order("created_at ASC").Find(&items).Error
	return items, err
}

// ─── ChapterSceneAnchorRepository 章节-场景绑定仓库 ──────────────────────────

type ChapterSceneAnchorRepository struct{ db *gorm.DB }

func NewChapterSceneAnchorRepository(db *gorm.DB) *ChapterSceneAnchorRepository {
	return &ChapterSceneAnchorRepository{db: db}
}

func (r *ChapterSceneAnchorRepository) Upsert(csa *model.ChapterSceneAnchor) error {
	return r.db.Where(model.ChapterSceneAnchor{ChapterID: csa.ChapterID, SceneAnchorID: csa.SceneAnchorID}).
		FirstOrCreate(csa).Error
}

func (r *ChapterSceneAnchorRepository) ListByChapter(chapterID uint) ([]*model.ChapterSceneAnchor, error) {
	var items []*model.ChapterSceneAnchor
	err := r.db.Where("chapter_id = ?", chapterID).Find(&items).Error
	return items, err
}

func (r *ChapterSceneAnchorRepository) Delete(chapterID, sceneAnchorID uint) error {
	return r.db.Where("chapter_id = ? AND scene_anchor_id = ?", chapterID, sceneAnchorID).
		Delete(&model.ChapterSceneAnchor{}).Error
}

// ─── SceneConsistencyLogRepository 场景一致性日志仓库 ────────────────────────

type SceneConsistencyLogRepository struct{ db *gorm.DB }

func NewSceneConsistencyLogRepository(db *gorm.DB) *SceneConsistencyLogRepository {
	return &SceneConsistencyLogRepository{db: db}
}

func (r *SceneConsistencyLogRepository) Create(log *model.SceneConsistencyLog) error {
	return r.db.Create(log).Error
}

func (r *SceneConsistencyLogRepository) ListByShotID(shotID uint) ([]*model.SceneConsistencyLog, error) {
	var items []*model.SceneConsistencyLog
	err := r.db.Where("shot_id = ?", shotID).Order("created_at DESC").Find(&items).Error
	return items, err
}

func (r *SceneConsistencyLogRepository) ListByAnchorID(anchorID uint) ([]*model.SceneConsistencyLog, error) {
	var items []*model.SceneConsistencyLog
	err := r.db.Where("anchor_id = ?", anchorID).Order("created_at DESC").Find(&items).Error
	return items, err
}

// ─── ScreenplaySceneRepository 分场剧本仓库 ──────────────────────────────────

type ScreenplaySceneRepository struct{ db *gorm.DB }

func NewScreenplaySceneRepository(db *gorm.DB) *ScreenplaySceneRepository {
	return &ScreenplaySceneRepository{db: db}
}

func (r *ScreenplaySceneRepository) Create(s *model.ScreenplayScene) error {
	return r.db.Create(s).Error
}

func (r *ScreenplaySceneRepository) GetByID(id uint) (*model.ScreenplayScene, error) {
	var s model.ScreenplayScene
	if err := r.db.First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// ListByChapter 按场次编号顺序返回一章的全部分场剧本。
func (r *ScreenplaySceneRepository) ListByChapter(chapterID uint) ([]*model.ScreenplayScene, error) {
	var items []*model.ScreenplayScene
	err := r.db.Where("chapter_id = ?", chapterID).Order("scene_no ASC").Find(&items).Error
	return items, err
}

// ListByNovel 返回一部小说下的全部分场剧本（按章节+场次顺序），单次查询，供剧集列表等
// 需要展示多章节场次明细的场景使用，避免逐章调用 ListByChapter 造成 N+1。
func (r *ScreenplaySceneRepository) ListByNovel(novelID uint) ([]*model.ScreenplayScene, error) {
	var items []*model.ScreenplayScene
	err := r.db.Where("novel_id = ?", novelID).Order("chapter_id ASC, scene_no ASC").Find(&items).Error
	return items, err
}

func (r *ScreenplaySceneRepository) Update(s *model.ScreenplayScene) error {
	return r.db.Save(s).Error
}

// UpdateFields 只更新指定字段，避免全量 Save 覆盖其他字段（如已生成的 Beats）。
func (r *ScreenplaySceneRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.ScreenplayScene{}).Where("id = ?", id).Updates(fields).Error
}

// DeleteByChapter 删除一章的全部分场剧本（重新生成剧本前清空旧数据，仅针对未锁定场次的调用方需自行过滤）。
func (r *ScreenplaySceneRepository) DeleteByChapter(chapterID uint) error {
	return r.db.Where("chapter_id = ?", chapterID).Delete(&model.ScreenplayScene{}).Error
}

func (r *ScreenplaySceneRepository) Delete(id uint) error {
	return r.db.Delete(&model.ScreenplayScene{}, id).Error
}

