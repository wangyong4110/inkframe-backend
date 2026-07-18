package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// StoryboardShotVersionRepository 分镜历史版本仓库（整视频一份快照，见 model.StoryboardShotVersion 注释）
type StoryboardShotVersionRepository struct {
	db *gorm.DB
}

func NewStoryboardShotVersionRepository(db *gorm.DB) *StoryboardShotVersionRepository {
	return &StoryboardShotVersionRepository{db: db}
}

// CreateAtomic 在事务内用 SELECT MAX FOR UPDATE 原子分配 version_no 后插入版本记录。
func (r *StoryboardShotVersionRepository) CreateAtomic(version *model.StoryboardShotVersion) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var maxNo struct{ V *int }
		if err := tx.Raw(
			"SELECT MAX(version_no) AS v FROM ink_storyboard_shot_version WHERE video_id = ? FOR UPDATE",
			version.VideoID,
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

// List 获取某视频的所有历史版本，按版本号倒序
func (r *StoryboardShotVersionRepository) List(videoID uint) ([]*model.StoryboardShotVersion, error) {
	var versions []*model.StoryboardShotVersion
	if err := r.db.Where("video_id = ?", videoID).
		Order("version_no DESC").
		Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

// GetVersion 获取视频的指定版本
func (r *StoryboardShotVersionRepository) GetVersion(videoID uint, versionNo int) (*model.StoryboardShotVersion, error) {
	var version model.StoryboardShotVersion
	if err := r.db.Where("video_id = ? AND version_no = ?", videoID, versionNo).First(&version).Error; err != nil {
		return nil, err
	}
	return &version, nil
}

// DeleteByVideo 删除某视频的所有历史版本（视频删除时级联调用）
func (r *StoryboardShotVersionRepository) DeleteByVideo(videoID uint) error {
	return r.db.Where("video_id = ?", videoID).Delete(&model.StoryboardShotVersion{}).Error
}
