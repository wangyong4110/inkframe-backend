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

// ListByVideoAndScene 按视频ID+剧本场次ID获取分镜（供前端按场次查看分镜脚本时使用，
// 避免一次性拉取整个视频的全部分镜——description 字段现在是完整的 AI 生图提示词，
// 单条体积明显变大，按场次过滤能显著减少首次加载的数据量）。
func (r *StoryboardRepository) ListByVideoAndScene(videoID, sceneID uint) ([]*model.StoryboardShot, error) {
	var shots []*model.StoryboardShot
	if err := r.db.Where("video_id = ? AND screenplay_scene_id = ?", videoID, sceneID).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// ShotSummary 分镜轻量汇总，供场次侧边栏/时间轴等只需聚合信息（时长、缩略图、场次归属）
// 的场景使用，不含 description/gen_meta/cam_dir 等大字段。
type ShotSummary struct {
	ID                uint    `json:"id"`
	ShotNo            int     `json:"shot_no"`
	Duration          float64 `json:"duration"`
	ImageURL          string  `json:"image_url"`
	ScreenplaySceneID *uint   `json:"screenplay_scene_id,omitempty"`
}

// ListSummaryByVideo 获取视频分镜的轻量汇总。description 现在是完整的 AI 生图提示词，
// 聚合展示（场次时长/缩略图分组）不需要这部分大字段，用 Select 在 SQL 层直接裁剪列，
// 避免大字段随全量分镜一起传输。
func (r *StoryboardRepository) ListSummaryByVideo(videoID uint) ([]ShotSummary, error) {
	var out []ShotSummary
	if err := r.db.Model(&model.StoryboardShot{}).
		Select("id, shot_no, duration, image_url, screenplay_scene_id").
		Where("video_id = ?", videoID).
		Order("shot_no ASC").
		Scan(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Update 更新分镜
func (r *StoryboardRepository) Update(shot *model.StoryboardShot) error {
	return r.db.Save(shot).Error
}

// UpdateFields 按 map 部分更新分镜字段（空字符串也会写入，支持清空字段）
func (r *StoryboardRepository) UpdateFields(id uint, fields map[string]interface{}) error {
	return r.db.Model(&model.StoryboardShot{}).Where("id = ?", id).Updates(fields).Error
}

// BatchGetByIDs 批量获取分镜（单次 IN 查询），按 shot_no 升序返回。
func (r *StoryboardRepository) BatchGetByIDs(ids []uint) ([]*model.StoryboardShot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var shots []*model.StoryboardShot
	if err := r.db.Where("id IN ?", ids).Order("shot_no ASC").Find(&shots).Error; err != nil {
		return nil, err
	}
	return shots, nil
}

// ShotCounts 单个视频的分镜总数与已生成视频的分镜数。
type ShotCounts struct {
	Total     int
	WithVideo int
}

// CountShotsGroupedByVideo 按 video_id 分组统计分镜总数和已生成视频的分镜数（单次 SQL，
// 避免调用方按视频逐个查询分镜列表再本地计数——用于剧集列表等需要展示多个视频完成度的场景）。
func (r *StoryboardRepository) CountShotsGroupedByVideo(videoIDs []uint) (map[uint]ShotCounts, error) {
	result := make(map[uint]ShotCounts, len(videoIDs))
	if len(videoIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		VideoID   uint
		Total     int
		WithVideo int
	}
	if err := r.db.Model(&model.StoryboardShot{}).
		Select("video_id, COUNT(*) AS total, SUM(CASE WHEN video_url != '' THEN 1 ELSE 0 END) AS with_video").
		Where("video_id IN ?", videoIDs).
		Group("video_id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.VideoID] = ShotCounts{Total: row.Total, WithVideo: row.WithVideo}
	}
	return result, nil
}

// Delete 硬删除单个分镜
func (r *StoryboardRepository) Delete(shotID uint) error {
	return r.db.Unscoped().Delete(&model.StoryboardShot{}, shotID).Error
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
