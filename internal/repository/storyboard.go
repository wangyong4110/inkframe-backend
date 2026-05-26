package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// StoryboardRepository 分镜仓库
type StoryboardRepository struct {
	db *gorm.DB
}

func NewStoryboardRepository(db *gorm.DB) *StoryboardRepository {
	return &StoryboardRepository{db: db}
}

// Create 创建分镜
func (r *StoryboardRepository) Create(shot *model.StoryboardShot) error {
	return r.db.Create(shot).Error
}

// BatchCreate 批量插入分镜（单次 SQL，避免 N 次往返）
func (r *StoryboardRepository) BatchCreate(shots []*model.StoryboardShot) error {
	if len(shots) == 0 {
		return nil
	}
	return r.db.CreateInBatches(shots, 100).Error
}

// GetByID 根据ID获取分镜
func (r *StoryboardRepository) GetByID(id uint) (*model.StoryboardShot, error) {
	var shot model.StoryboardShot
	if err := r.db.First(&shot, id).Error; err != nil {
		return nil, err
	}
	return &shot, nil
}

// ListByVideo 获取视频的所有分镜
func (r *StoryboardRepository) ListByVideo(videoID uint) ([]*model.StoryboardShot, error) {
	var shots []*model.StoryboardShot
	if err := r.db.Where("video_id = ?", videoID).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// ListByVideoAndStatus 按视频ID和状态获取分镜
func (r *StoryboardRepository) ListByVideoAndStatus(videoID uint, status string) ([]*model.StoryboardShot, error) {
	var shots []*model.StoryboardShot
	if err := r.db.Where("video_id = ? AND status = ?", videoID, status).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// Update 更新分镜
func (r *StoryboardRepository) Update(shot *model.StoryboardShot) error {
	return r.db.Save(shot).Error
}

// UpdateFields 按 map 部分更新分镜字段（空字符串也会写入，支持清空字段）
func (r *StoryboardRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", id).Updates(fields).Error
}

// BatchGetByIDs 批量获取分镜（单次 IN 查询）
func (r *StoryboardRepository) BatchGetByIDs(ids []uint) ([]*model.StoryboardShot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var shots []*model.StoryboardShot
	if err := r.db.Where("id IN ?", ids).Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// UpdateSFXTags 仅更新分镜的 sfx_tags 字段，不修改 sfx_url 和 sfx_volume
func (r *StoryboardRepository) UpdateSFXTags(shotID uint, sfxTags string) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", shotID).Update("sfx_tags", sfxTags).Error
}

// UpdateSFX 更新单个分镜的音效字段（URL、标签、混音音量）
func (r *StoryboardRepository) UpdateSFX(shotID uint, sfxURL, sfxTags string, sfxVolume float64) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", shotID).Updates(map[string]interface{}{
		"sfx_url":    sfxURL,
		"sfx_tags":   sfxTags,
		"sfx_volume": sfxVolume,
	}).Error
}

// DeleteByVideoID 硬删除视频的所有分镜（重新生成时使用）
// 必须用 Unscoped() 物理删除，否则软删除的行仍触发 uk_video_shot 唯一键冲突。
func (r *StoryboardRepository) DeleteByVideoID(videoID uint) error {
	return r.db.Unscoped().Where("video_id = ?", videoID).Delete(&model.StoryboardShot{}).Error
}

// Delete 硬删除单个分镜
func (r *StoryboardRepository) Delete(shotID uint) error {
	return r.db.Unscoped().Delete(&model.StoryboardShot{}, shotID).Error
}

// MaxShotNo 返回视频中最大的 shot_no（无分镜时返回 0）
func (r *StoryboardRepository) MaxShotNo(videoID uint) (int, error) {
	var max int
	err := r.db.Model(&model.StoryboardShot{}).
		Where("video_id = ? AND deleted_at IS NULL", videoID).
		Select("COALESCE(MAX(shot_no), 0)").Scan(&max).Error
	return max, err
}

// ShiftShotNos 将 video_id 下所有 shot_no >= fromShotNo 的分镜的 shot_no 加 delta（delta 通常为 1）。
// 使用两阶段更新避免唯一键冲突：先整体偏移到无冲突区间，再还原到目标值。
func (r *StoryboardRepository) ShiftShotNos(videoID uint, fromShotNo, delta int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Phase 1: move into a temporary collision-free range
		if err := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			tempOffset, videoID, fromShotNo,
		).Error; err != nil {
			return err
		}
		// Phase 2: shift back to the intended final position
		return tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			tempOffset, delta, videoID, fromShotNo+tempOffset,
		).Error
	})
}

// CompactShotNosAfter 将 video_id 下 shot_no > deletedShotNo 的分镜 shot_no 减 1（删除后紧凑化）。
// 同样使用两阶段更新。
func (r *StoryboardRepository) CompactShotNosAfter(videoID uint, deletedShotNo int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
			tempOffset, videoID, deletedShotNo,
		).Error; err != nil {
			return err
		}
		return tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? - 1 WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
			tempOffset, videoID, deletedShotNo+tempOffset,
		).Error
	})
}

// ShotVoiceSegmentRepository 分镜语音段落仓库
type ShotVoiceSegmentRepository struct {
	db *gorm.DB
}

func NewShotVoiceSegmentRepository(db *gorm.DB) *ShotVoiceSegmentRepository {
	return &ShotVoiceSegmentRepository{db: db}
}

// ListByShotID 获取分镜的所有语音段落，按 seq_no 升序
func (r *ShotVoiceSegmentRepository) ListByShotID(shotID uint) ([]*model.ShotVoiceSegment, error) {
	var segs []*model.ShotVoiceSegment
	err := r.db.Where("shot_id = ?", shotID).Order("seq_no ASC").Find(&segs).Error
	return segs, err
}

func (r *ShotVoiceSegmentRepository) GetByID(id uint) (*model.ShotVoiceSegment, error) {
	var seg model.ShotVoiceSegment
	if err := r.db.First(&seg, id).Error; err != nil {
		return nil, err
	}
	return &seg, nil
}

func (r *ShotVoiceSegmentRepository) Create(seg *model.ShotVoiceSegment) error {
	return r.db.Create(seg).Error
}

func (r *ShotVoiceSegmentRepository) Update(seg *model.ShotVoiceSegment) error {
	return r.db.Save(seg).Error
}

func (r *ShotVoiceSegmentRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.ShotVoiceSegment{}).Where("id = ?", id).Updates(fields).Error
}

func (r *ShotVoiceSegmentRepository) Delete(id uint) error {
	return r.db.Unscoped().Delete(&model.ShotVoiceSegment{}, id).Error
}

// MaxSeqNo 返回分镜中最大的 seq_no（无段落时返回 0）
func (r *ShotVoiceSegmentRepository) MaxSeqNo(shotID uint) (int, error) {
	var max int
	err := r.db.Model(&model.ShotVoiceSegment{}).
		Where("shot_id = ? AND deleted_at IS NULL", shotID).
		Select("COALESCE(MAX(seq_no), 0)").Scan(&max).Error
	return max, err
}

// ShiftSeqNos 将 shot_id 下所有 seq_no >= fromSeqNo 的段落的 seq_no 加 1（为插入腾出位置）
func (r *ShotVoiceSegmentRepository) ShiftSeqNos(shotID uint, fromSeqNo int) error {
	return r.db.Exec(
		"UPDATE ink_shot_voice_segment SET seq_no = seq_no + 1 WHERE shot_id = ? AND seq_no >= ? AND deleted_at IS NULL",
		shotID, fromSeqNo,
	).Error
}

// CompactSeqNosAfter 将 shot_id 下 seq_no > deletedSeqNo 的段落 seq_no 减 1（删除后紧凑化）
func (r *ShotVoiceSegmentRepository) CompactSeqNosAfter(shotID uint, deletedSeqNo int) error {
	return r.db.Exec(
		"UPDATE ink_shot_voice_segment SET seq_no = seq_no - 1 WHERE shot_id = ? AND seq_no > ? AND deleted_at IS NULL",
		shotID, deletedSeqNo,
	).Error
}

// ShotSFXItemRepository 分镜音效条目仓库
type ShotSFXItemRepository struct {
	db *gorm.DB
}

func NewShotSFXItemRepository(db *gorm.DB) *ShotSFXItemRepository {
	return &ShotSFXItemRepository{db: db}
}

// ListByShotID 获取分镜的所有音效条目，按 seq_no 升序
func (r *ShotSFXItemRepository) ListByShotID(shotID uint) ([]*model.ShotSFXItem, error) {
	var items []*model.ShotSFXItem
	err := r.db.Where("shot_id = ?", shotID).Order("seq_no").Find(&items).Error
	return items, err
}

// CountByShotID 统计分镜已有音效数量（幂等检测）
func (r *ShotSFXItemRepository) CountByShotID(shotID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.ShotSFXItem{}).Where("shot_id = ?", shotID).Count(&count).Error
	return count, err
}

// BatchCreate 批量创建音效条目
func (r *ShotSFXItemRepository) BatchCreate(items []*model.ShotSFXItem) error {
	if len(items) == 0 {
		return nil
	}
	return r.db.Create(&items).Error
}

// Update 更新音效条目（通常只更新 volume）
func (r *ShotSFXItemRepository) Update(item *model.ShotSFXItem) error {
	return r.db.Save(item).Error
}

func (r *ShotSFXItemRepository) UpdateDisabled(id uint, disabled bool) error {
	return r.db.Model(&model.ShotSFXItem{}).Where("id = ?", id).Update("disabled", disabled).Error
}

// Delete 物理删除单条音效条目
func (r *ShotSFXItemRepository) Delete(id uint) error {
	return r.db.Unscoped().Delete(&model.ShotSFXItem{}, id).Error
}

// DeleteByShotID 物理删除分镜的所有音效条目（重新生成时使用）
func (r *ShotSFXItemRepository) DeleteByShotID(shotID uint) error {
	return r.db.Unscoped().Where("shot_id = ?", shotID).Delete(&model.ShotSFXItem{}).Error
}

// ─── VideoBGMSegmentRepository ────────────────────────────────────────────────

// VideoBGMSegmentRepository 视频BGM分段仓库
type VideoBGMSegmentRepository struct {
	db *gorm.DB
}

func NewVideoBGMSegmentRepository(db *gorm.DB) *VideoBGMSegmentRepository {
	return &VideoBGMSegmentRepository{db: db}
}

// ListByVideoID 获取视频的所有BGM分段，按 seq_no 升序
func (r *VideoBGMSegmentRepository) ListByVideoID(videoID uint) ([]*model.VideoBGMSegment, error) {
	var segs []*model.VideoBGMSegment
	err := r.db.Where("video_id = ?", videoID).Order("seq_no").Find(&segs).Error
	return segs, err
}

// DeleteByVideoID 删除视频的所有BGM分段（重新分析时清空）
func (r *VideoBGMSegmentRepository) DeleteByVideoID(videoID uint) error {
	return r.db.Unscoped().Where("video_id = ?", videoID).Delete(&model.VideoBGMSegment{}).Error
}

// BatchCreate 批量创建BGM分段
func (r *VideoBGMSegmentRepository) BatchCreate(segs []*model.VideoBGMSegment) error {
	if len(segs) == 0 {
		return nil
	}
	return r.db.Create(&segs).Error
}

// Update 更新BGM分段（用于更新URL/Volume等）
func (r *VideoBGMSegmentRepository) GetByID(id uint) (*model.VideoBGMSegment, error) {
	var seg model.VideoBGMSegment
	if err := r.db.First(&seg, id).Error; err != nil {
		return nil, err
	}
	return &seg, nil
}

func (r *VideoBGMSegmentRepository) Update(seg *model.VideoBGMSegment) error {
	return r.db.Save(seg).Error
}

func (r *VideoBGMSegmentRepository) UpdateDisabled(id uint, disabled bool) error {
	return r.db.Model(&model.VideoBGMSegment{}).Where("id = ?", id).Update("disabled", disabled).Error
}

// UpdateTrack 更新BGM分段的曲目信息（手动选曲后调用）
func (r *VideoBGMSegmentRepository) UpdateTrack(id uint, url, name, artist, source string) error {
	return r.db.Model(&model.VideoBGMSegment{}).Where("id = ?", id).Updates(map[string]interface{}{
		"url":          url,
		"track_name":   name,
		"track_artist": artist,
		"source":       source,
	}).Error
}

// ReplaceForVideo 在单个事务内原子替换视频的所有 BGM 分段：先建新再删旧，避免数据丢失。
func (r *VideoBGMSegmentRepository) ReplaceForVideo(videoID uint, segs []*model.VideoBGMSegment) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if len(segs) > 0 {
			if err := tx.Create(&segs).Error; err != nil {
				return err
			}
		}
		return tx.Unscoped().Where("video_id = ? AND id NOT IN (?)",
			videoID, collectIDs(segs)).Delete(&model.VideoBGMSegment{}).Error
	})
}

// collectIDs 提取记录 ID 列表；若空则返回 []uint{0}（避免 NOT IN 空集合语法错误）
func collectIDs(segs []*model.VideoBGMSegment) []uint {
	if len(segs) == 0 {
		return []uint{0}
	}
	ids := make([]uint, len(segs))
	for i, s := range segs {
		ids[i] = s.ID
	}
	return ids
}

// ─── StoryboardReviewRecordRepository ───────────────────────────────────────

type StoryboardReviewRecordRepository struct{ db *gorm.DB }

func NewStoryboardReviewRecordRepository(db *gorm.DB) *StoryboardReviewRecordRepository {
	return &StoryboardReviewRecordRepository{db: db}
}

func (r *StoryboardReviewRecordRepository) Create(rec *model.StoryboardReviewRecord) error {
	return r.db.Create(rec).Error
}

func (r *StoryboardReviewRecordRepository) ListByVideo(videoID uint) ([]*model.StoryboardReviewRecord, error) {
	var list []*model.StoryboardReviewRecord
	err := r.db.Where("video_id = ?", videoID).Order("created_at DESC").Find(&list).Error
	return list, err
}

func (r *StoryboardReviewRecordRepository) GetByID(id uint) (*model.StoryboardReviewRecord, error) {
	var rec model.StoryboardReviewRecord
	err := r.db.First(&rec, id).Error
	return &rec, err
}

func (r *StoryboardReviewRecordRepository) Update(rec *model.StoryboardReviewRecord) error {
	return r.db.Save(rec).Error
}

// ─── IgnoredSuggestionRepository ─────────────────────────────────────────────

type IgnoredSuggestionRepository struct{ db *gorm.DB }

func NewIgnoredSuggestionRepository(db *gorm.DB) *IgnoredSuggestionRepository {
	return &IgnoredSuggestionRepository{db: db}
}

func (r *IgnoredSuggestionRepository) Create(item *model.IgnoredSuggestion) error {
	return r.db.Where(model.IgnoredSuggestion{
		VideoID:   item.VideoID,
		ShotNo:    item.ShotNo,
		IssueHash: item.IssueHash,
	}).FirstOrCreate(item).Error
}

func (r *IgnoredSuggestionRepository) ListByVideo(videoID uint) ([]*model.IgnoredSuggestion, error) {
	var list []*model.IgnoredSuggestion
	err := r.db.Where("video_id = ?", videoID).Order("shot_no ASC, id ASC").Find(&list).Error
	return list, err
}

func (r *IgnoredSuggestionRepository) Delete(id uint) error {
	return r.db.Delete(&model.IgnoredSuggestion{}, id).Error
}

// GetLatestApplied 获取该视频最近一条已应用（status=applied）的审查记录
func (r *StoryboardReviewRecordRepository) GetLatestApplied(videoID uint) (*model.StoryboardReviewRecord, error) {
	var rec model.StoryboardReviewRecord
	err := r.db.Where("video_id = ? AND status = ?", videoID, "applied").
		Order("id DESC").First(&rec).Error
	return &rec, err
}
