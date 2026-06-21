package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── SceneAnchorRepository 场景锚点仓库 ──────────────────────────────────────

type SceneAnchorRepository struct{ db *gorm.DB }

func NewSceneAnchorRepository(db *gorm.DB) *SceneAnchorRepository {
	return &SceneAnchorRepository{db: db}
}

func (r *SceneAnchorRepository) Create(a *model.SceneAnchor) error {
	return r.db.Create(a).Error
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

func (r *ChapterSceneAnchorRepository) DeleteByAnchor(sceneAnchorID uint) error {
	return r.db.Where("scene_anchor_id = ?", sceneAnchorID).Delete(&model.ChapterSceneAnchor{}).Error
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
