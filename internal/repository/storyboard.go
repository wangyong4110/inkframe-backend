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

func (r *StoryboardRepository) DB() *gorm.DB { return r.db }

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

// GetByVideoAndShotNo 根据视频ID和镜头序号精确查询单个分镜
func (r *StoryboardRepository) GetByVideoAndShotNo(videoID uint, shotNo int) (*model.StoryboardShot, error) {
	var shot model.StoryboardShot
	err := r.db.Where("video_id = ? AND shot_no = ?", videoID, shotNo).First(&shot).Error
	if err != nil {
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
		Where("video_id = ?", videoID).
		Select("COALESCE(MAX(shot_no), 0)").Scan(&max).Error
	return max, err
}

// ShiftShotNos 将 video_id 下所有 shot_no >= fromShotNo 的分镜的 shot_no 加 delta（delta 通常为 1）。
// 使用两阶段更新避免唯一键冲突：先整体偏移到无冲突区间，再还原到目标值。
func (r *StoryboardRepository) ShiftShotNos(videoID uint, fromShotNo, delta int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		base := tx.Model(&model.StoryboardShot{}).Where("video_id = ?", videoID)
		if err := base.Where("shot_no >= ?", fromShotNo).
			UpdateColumn("shot_no", gorm.Expr("shot_no + ?", tempOffset)).Error; err != nil {
			return err
		}
		return base.Where("shot_no >= ?", fromShotNo+tempOffset).
			UpdateColumn("shot_no", gorm.Expr("shot_no - ? + ?", tempOffset, delta)).Error
	})
}

// CompactShotNosAfter 将 video_id 下 shot_no > deletedShotNo 的分镜 shot_no 减 1（删除后紧凑化）。
// 同样使用两阶段更新。
func (r *StoryboardRepository) CompactShotNosAfter(videoID uint, deletedShotNo int) error {
	const tempOffset = 100000
	return r.db.Transaction(func(tx *gorm.DB) error {
		base := tx.Model(&model.StoryboardShot{}).Where("video_id = ?", videoID)
		if err := base.Where("shot_no > ?", deletedShotNo).
			UpdateColumn("shot_no", gorm.Expr("shot_no + ?", tempOffset)).Error; err != nil {
			return err
		}
		return base.Where("shot_no > ?", deletedShotNo+tempOffset).
			UpdateColumn("shot_no", gorm.Expr("shot_no - ? - 1", tempOffset)).Error
	})
}

// ShotVoiceSegmentRepository 分镜语音段落仓库
type ShotVoiceSegmentRepository struct {
	db *gorm.DB
}

func NewShotVoiceSegmentRepository(db *gorm.DB) *ShotVoiceSegmentRepository {
	return &ShotVoiceSegmentRepository{db: db}
}

func (r *ShotVoiceSegmentRepository) DB() *gorm.DB { return r.db }

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
		Where("shot_id = ?", shotID).
		Select("COALESCE(MAX(seq_no), 0)").Scan(&max).Error
	return max, err
}

// AppendAtomic assigns the next seq_no and creates the segment in a single transaction,
// eliminating the read-then-write race under concurrent appends.
func (r *ShotVoiceSegmentRepository) AppendAtomic(seg *model.ShotVoiceSegment) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var maxSeq int
		if err := tx.Raw(
			"SELECT COALESCE(MAX(seq_no), 0) FROM ink_shot_voice_segment WHERE shot_id = ? AND deleted_at IS NULL FOR UPDATE",
			seg.ShotID,
		).Scan(&maxSeq).Error; err != nil {
			return err
		}
		seg.SeqNo = maxSeq + 1
		return tx.Create(seg).Error
	})
}

// ShiftSeqNos 将 shot_id 下所有 seq_no >= fromSeqNo 的段落的 seq_no 加 1（为插入腾出位置）
func (r *ShotVoiceSegmentRepository) ShiftSeqNos(shotID uint, fromSeqNo int) error {
	return r.db.Model(&model.ShotVoiceSegment{}).
		Where("shot_id = ? AND seq_no >= ?", shotID, fromSeqNo).
		UpdateColumn("seq_no", gorm.Expr("seq_no + 1")).Error
}

// CompactSeqNosAfter 将 shot_id 下 seq_no > deletedSeqNo 的段落 seq_no 减 1（删除后紧凑化）
func (r *ShotVoiceSegmentRepository) CompactSeqNosAfter(shotID uint, deletedSeqNo int) error {
	return r.db.Model(&model.ShotVoiceSegment{}).
		Where("shot_id = ? AND seq_no > ?", shotID, deletedSeqNo).
		UpdateColumn("seq_no", gorm.Expr("seq_no - 1")).Error
}

// GetFirstAudioByShotIDs returns a map of shotID → first segment audio_path for a list of shots.
func (r *ShotVoiceSegmentRepository) GetFirstAudioByShotIDs(shotIDs []uint) map[uint]string {
	if len(shotIDs) == 0 {
		return nil
	}
	var segs []*model.ShotVoiceSegment
	r.db.Where("shot_id IN ? AND audio_path != ''", shotIDs).
		Order("shot_id, seq_no").Find(&segs)
	result := make(map[uint]string, len(shotIDs))
	for _, seg := range segs {
		if _, ok := result[seg.ShotID]; !ok {
			result[seg.ShotID] = seg.AudioPath
		}
	}
	return result
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

// Create 创建单条音效条目
func (r *ShotSFXItemRepository) Create(item *model.ShotSFXItem) error {
	return r.db.Create(item).Error
}

// AppendAtomic atomically assigns seq_no = MAX(seq_no)+1 inside a transaction to prevent
// duplicate seq_no when multiple instances append simultaneously.
func (r *ShotSFXItemRepository) AppendAtomic(item *model.ShotSFXItem) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var maxSeq int
		if err := tx.Raw("SELECT COALESCE(MAX(seq_no), 0) FROM ink_shot_sfx_item WHERE shot_id = ? FOR UPDATE", item.ShotID).Scan(&maxSeq).Error; err != nil {
			return err
		}
		item.SeqNo = maxSeq + 1
		return tx.Create(item).Error
	})
}

// MaxSeqNo 返回分镜中最大的 seq_no（无条目时返回 0）
func (r *ShotSFXItemRepository) MaxSeqNo(shotID uint) (int, error) {
	var max int
	err := r.db.Model(&model.ShotSFXItem{}).Where("shot_id = ?", shotID).
		Select("COALESCE(MAX(seq_no), 0)").Scan(&max).Error
	return max, err
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

// UpdateFields 部分更新 ShotSFXItem 指定字段，避免 Save 覆盖未传字段为零值。
func (r *ShotSFXItemRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.ShotSFXItem{}).Where("id = ?", id).Updates(fields).Error
}

func (r *ShotSFXItemRepository) UpdateDisabled(id uint, disabled bool) error {
	return r.db.Model(&model.ShotSFXItem{}).Where("id = ?", id).Update("disabled", disabled).Error
}

// Delete 物理删除单条音效条目
func (r *ShotSFXItemRepository) GetByID(id uint) (*model.ShotSFXItem, error) {
	var item model.ShotSFXItem
	if err := r.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

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

// StoryboardReviewRecordRepository 和 IgnoredSuggestionRepository 已合并为
// ReviewRecordRepository 和 IgnoredReviewIssueRepository（review_repository.go）。
