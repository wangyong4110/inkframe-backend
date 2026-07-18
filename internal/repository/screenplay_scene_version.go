package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ScreenplaySceneVersionRepository 分场剧本历史版本仓库
type ScreenplaySceneVersionRepository struct {
	db *gorm.DB
}

func NewScreenplaySceneVersionRepository(db *gorm.DB) *ScreenplaySceneVersionRepository {
	return &ScreenplaySceneVersionRepository{db: db}
}

// CreateAtomic 在事务内用 SELECT MAX FOR UPDATE 原子分配 version_no 后插入版本记录，
// 与 ChapterVersionRepository.CreateAtomic 同样的模式。
func (r *ScreenplaySceneVersionRepository) CreateAtomic(version *model.ScreenplaySceneVersion) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var maxNo struct{ V *int }
		if err := tx.Raw(
			"SELECT MAX(version_no) AS v FROM ink_screenplay_scene_version WHERE screenplay_scene_id = ? FOR UPDATE",
			version.ScreenplaySceneID,
		).Scan(&maxNo).Error; err != nil {
			return err
		}
		if maxNo.V == nil {
			version.VersionNo = 1
		} else {
			version.VersionNo = *maxNo.V + 1
		}
		return tx.Create(version).Error
	})
}

// List 获取某场次的所有历史版本，按版本号倒序
func (r *ScreenplaySceneVersionRepository) List(sceneID uint) ([]*model.ScreenplaySceneVersion, error) {
	var versions []*model.ScreenplaySceneVersion
	if err := r.db.Where("screenplay_scene_id = ?", sceneID).
		Order("version_no DESC").
		Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

// GetVersion 获取场次的指定版本
func (r *ScreenplaySceneVersionRepository) GetVersion(sceneID uint, versionNo int) (*model.ScreenplaySceneVersion, error) {
	var version model.ScreenplaySceneVersion
	if err := r.db.Where("screenplay_scene_id = ? AND version_no = ?", sceneID, versionNo).First(&version).Error; err != nil {
		return nil, err
	}
	return &version, nil
}

// DeleteByScene 删除某场次的所有历史版本（场次删除时级联调用）
func (r *ScreenplaySceneVersionRepository) DeleteByScene(sceneID uint) error {
	return r.db.Where("screenplay_scene_id = ?", sceneID).Delete(&model.ScreenplaySceneVersion{}).Error
}
