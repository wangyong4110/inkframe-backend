package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type VideoService struct {
	videoRepo          *repository.VideoRepository
	storyboardRepo     *repository.StoryboardRepository
	chapterRepo        *repository.ChapterRepository
	characterRepo      *repository.CharacterRepository
	novelRepo          *repository.NovelRepository
	tenantRepo         *repository.TenantRepository
	aiService          *AIService
	videoProviders     map[string]ai.VideoProvider
	consistencyService *CharacterConsistencyService
	bgmService         *BGMService
	bgmRepo            *repository.VideoBGMSegmentRepository
	sfxService         *SFXService
	storageSvc         storage.Service
	sceneAnchorSvc           *SceneAnchorService
	sceneConsistencySvc      *SceneConsistencyService
	chapterCharacterRepo     *repository.ChapterCharacterRepository
	plotPointRepo        *repository.PlotPointRepository
	systemSettingRepo  *repository.SystemSettingRepository
	segmentRepo        *repository.ShotVoiceSegmentRepository
	reviewRecordRepo      *repository.ReviewRecordRepository
	ignoredSuggestionRepo *repository.IgnoredReviewIssueRepository
	lookRepo              *repository.CharacterLookRepository
	taskSvc            *TaskService
	videoSem           chan struct{} // nil = unlimited; set via WithVideoConcurrency
	videoSemMu         sync.RWMutex  // protects videoSem replacement
	audioSem           chan struct{} // limits concurrent TTS calls; nil = unlimited
	charListCache      sync.Map      // novelID → *charListEntry (short-lived cache for batch voice gen)
	// 广场社交
	videoLikeRepo    *repository.VideoLikeRepository
	videoCommentRepo *repository.VideoCommentRepository
	viewDedupCache   sync.Map     // fallback in-process dedup when Redis unavailable
	cache            *redis.Client // optional: cross-instance view dedup
	cleanupOnce      sync.Once
	stopCh           chan struct{} // closed by Shutdown() to stop background goroutines
	activePoll            sync.Map     // videoID → struct{} (prevents duplicate PollAndStitchVideo goroutines)
	generatingStoryboard  sync.Map     // videoID → context.CancelFunc (cancels in-progress GenerateStoryboard)
	backendBaseURL        string       // e.g. "http://192.168.1.10:8080"; used to resolve relative /api/v1/media/* URLs
	dbMediaReader         storage.Service // DB-backed storage for reading legacy /api/v1/media/* assets
}

// GetNovelByID 通过 novelRepo 加载小说（供 handler 传递给 CapCutService 等下游服务）
func (s *VideoService) GetNovelByID(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
}

// videoBelongsToTenant verifies video ownership via novel.TenantID (novel-based ownership).
func (s *VideoService) videoBelongsToTenant(video *model.Video, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(video.NovelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
}

// videoTenantID returns the tenantID for a video via its parent novel.
func (s *VideoService) videoTenantID(video *model.Video) uint {
	if s.novelRepo == nil {
		return 0
	}
	novel, err := s.novelRepo.GetByID(video.NovelID)
	if err != nil {
		return 0
	}
	return novel.TenantID
}

// GetNovelVideoConfig 获取小说的视频配置（供 handler 层使用）
func (s *VideoService) GetNovelVideoConfig(novelID uint) *model.NovelVideoConfig {
	if s.novelRepo == nil {
		return nil
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil || novel == nil {
		return nil
	}
	return novel.VideoConfig
}

func (s *VideoService) WithSystemSettingRepo(r *repository.SystemSettingRepository) *VideoService {
	s.systemSettingRepo = r
	return s
}

// WithVideoConcurrency 设置视频生成的最大并发数（启动时调用）。
func (s *VideoService) WithVideoConcurrency(n int) *VideoService {
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	}
	return s
}

// WithAudioConcurrency 设置 TTS 音频生成的最大并发数。
// 默认不限制；推荐设置为 3，防止批量生成时触发 API 限速（429）。
func (s *VideoService) WithAudioConcurrency(n int) *VideoService {
	if n > 0 {
		s.audioSem = make(chan struct{}, n)
	}
	return s
}

// SetVideoConcurrency 运行时动态调整视频并发度（线程安全）。
func (s *VideoService) SetVideoConcurrency(n int) {
	s.videoSemMu.Lock()
	defer s.videoSemMu.Unlock()
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	} else {
		s.videoSem = nil
	}
}

func (s *VideoService) WithLookRepo(r *repository.CharacterLookRepository) *VideoService {
	s.lookRepo = r
	return s
}

func (s *VideoService) WithReviewRecordRepo(r *repository.ReviewRecordRepository) *VideoService {
	s.reviewRecordRepo = r
	return s
}

func (s *VideoService) WithIgnoredIssueRepo(r *repository.IgnoredReviewIssueRepository) *VideoService {
	s.ignoredSuggestionRepo = r
	return s
}

func (s *VideoService) WithSegmentRepo(r *repository.ShotVoiceSegmentRepository) *VideoService {
	s.segmentRepo = r
	return s
}

func (s *VideoService) WithTaskService(svc *TaskService) *VideoService {
	s.taskSvc = svc
	return s
}

func (s *VideoService) WithSceneAnchorService(svc *SceneAnchorService) *VideoService {
	s.sceneAnchorSvc = svc
	return s
}

func (s *VideoService) WithSceneConsistencyService(svc *SceneConsistencyService) *VideoService {
	s.sceneConsistencySvc = svc
	return s
}

func (s *VideoService) WithPlotPointRepo(repo *repository.PlotPointRepository) *VideoService {
	s.plotPointRepo = repo
	return s
}

func (s *VideoService) WithChapterCharacterRepo(r *repository.ChapterCharacterRepository) *VideoService {
	s.chapterCharacterRepo = r
	return s
}

func NewVideoService(
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	novelRepo *repository.NovelRepository,
	tenantRepo *repository.TenantRepository,
	aiService *AIService,
	videoProviders map[string]ai.VideoProvider,
) *VideoService {
	svc := &VideoService{
		videoRepo:      videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		characterRepo:  characterRepo,
		novelRepo:      novelRepo,
		tenantRepo:     tenantRepo,
		aiService:      aiService,
		videoProviders: videoProviders,
		stopCh:         make(chan struct{}),
	}
	svc.startCharListCacheCleanup()
	return svc
}

// Shutdown 停止所有后台 goroutine（优雅关闭时调用）。
func (s *VideoService) Shutdown() {
	close(s.stopCh)
}

// WithConsistencyService 设置一致性服务（选填）
func (s *VideoService) WithConsistencyService(cs *CharacterConsistencyService) *VideoService {
	s.consistencyService = cs
	return s
}

// WithBGMService 设置BGM服务（选填）
func (s *VideoService) WithBGMService(bgm *BGMService) *VideoService {
	s.bgmService = bgm
	return s
}

// WithBGMSegmentRepo 注入 BGM 分段仓库（选填；用于合成前覆盖率校验）
func (s *VideoService) WithBGMSegmentRepo(r *repository.VideoBGMSegmentRepository) *VideoService {
	s.bgmRepo = r
	return s
}

// WithRedis enables cross-instance view deduplication via Redis SETNX.
func (s *VideoService) WithRedis(c *redis.Client) *VideoService {
	s.cache = c
	return s
}

// WithSFXService 设置音效服务（选填）
func (s *VideoService) WithSFXService(sfx *SFXService) *VideoService {
	s.sfxService = sfx
	return s
}

// WithStorage 注入媒体存储服务（选填；配置 OSS 时上传至 OSS，否则存 DB）
func (s *VideoService) WithStorage(svc storage.Service) *VideoService {
	s.storageSvc = svc
	return s
}

// WithBackendBaseURL 设置后端对外可访问的根 URL（如 "http://192.168.1.10:8080"），
// 用于将 /api/v1/media/* 相对路径解析为第三方 API 可访问的完整 URL。
// 仅在既未配置 OSS 又无 dbMediaReader 时作为最后回退。
func (s *VideoService) WithBackendBaseURL(baseURL string) *VideoService {
	s.backendBaseURL = strings.TrimRight(baseURL, "/")
	return s
}

// WithDBMediaReader 注入专门用于读取 DB 存储（/api/v1/media/*）媒体数据的 storage.Service。
// 当主 storageSvc 为 OSS 时，OSS 的 Get() 无法处理相对路径，需要用此单独读取。
func (s *VideoService) WithDBMediaReader(svc storage.Service) *VideoService {
	s.dbMediaReader = svc
	return s
}

// migrateLocalImageToPublic 将本地 DB 存储的图片（/api/v1/media/*）迁移到 OSS。
// 若已是公网 URL 则直接返回原值。迁移失败时返回原值并打印错误日志，由调用方决定如何处理。
func (s *VideoService) migrateLocalImageToPublic(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if !strings.HasPrefix(u, "/api/v1/media/") {
		return u
	}
	if s.dbMediaReader == nil {
		logger.Errorf("[migrateLocalImage] dbMediaReader is nil, cannot read %q", u)
		return u
	}
	data, err := s.dbMediaReader.Get(context.Background(), u)
	if err != nil {
		logger.Errorf("[migrateLocalImage] read DB failed for %q: %v", u, err)
		return u
	}
	if len(data) == 0 {
		logger.Errorf("[migrateLocalImage] DB returned empty data for %q", u)
		return u
	}
	if s.storageSvc == nil {
		logger.Errorf("[migrateLocalImage] storageSvc is nil, cannot upload %q to OSS", u)
		return u
	}
	key := fmt.Sprintf("images/migrated-%s.jpg", uuid.New().String())
	ossURL, uploadErr := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(data), int64(len(data)), "image/jpeg")
	if uploadErr != nil {
		logger.Errorf("[migrateLocalImage] OSS upload failed for %q: %v", u, uploadErr)
		return u
	}
	if !strings.HasPrefix(ossURL, "http://") && !strings.HasPrefix(ossURL, "https://") {
		logger.Errorf("[migrateLocalImage] OSS returned non-public URL %q for %q (OSS not configured?)", ossURL, u)
		return u
	}
	return ossURL
}

// resolveAbsURL 将相对路径转换为绝对 URL，供批量处理额外参考图使用。
// 主参考图请使用 migrateLocalImageToPublic（带永久 DB 更新）。
// 若迁移失败且未配置 PublicURL，返回空字符串（调用方应跳过该图，禁止使用 127.0.0.1）。
func (s *VideoService) resolveAbsURL(u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	migrated := s.migrateLocalImageToPublic(u)
	if strings.HasPrefix(migrated, "http://") || strings.HasPrefix(migrated, "https://") {
		return migrated
	}
	// 仅在明确配置了 PublicURL 时才拼接（严禁 127.0.0.1）
	if s.backendBaseURL != "" {
		return s.backendBaseURL + u
	}
	logger.Errorf("[resolveAbsURL] cannot resolve %q to a public URL (OSS migration failed, no PublicURL configured)", u)
	return ""
}

// CreateVideoFromChapter 从章节创建视频
func (s *VideoService) CreateVideoFromChapter(novelID uint, chapterID *uint) (*model.Video, error) {
	if chapterID != nil && *chapterID == 0 {
		chapterID = nil
	}
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   chapterID,
		Title:       "新视频",
		Status:      "planning",
		FrameRate:   24,
		Resolution:  "1080p",
		AspectRatio: "16:9",
	}
	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// CreateVideo 创建视频（接受请求对象）
func (s *VideoService) CreateVideo(novelID uint, req *model.CreateVideoRequest, callerTenantID uint) (*model.Video, error) {
	return s.CreateVideoFromReq(novelID, req, callerTenantID)
}



func (s *VideoService) CreateVideoFromReq(novelID uint, req *model.CreateVideoRequest, callerTenantID uint) (*model.Video, error) {
	// Treat chapter_id=0 as absent (frontend may send 0 instead of omitting the field)
	chapterID := req.ChapterID
	if chapterID != nil && *chapterID == 0 {
		chapterID = nil
	}
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   chapterID,
		Title:       req.Title,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:    req.ArtStyle,
		QualityTier: req.QualityTier,
		Mode:        req.Mode,
		VisualMode:  req.VisualMode,
		ThreeDStyle: req.ThreeDStyle,
		Status:      "planning",
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, fmt.Errorf("novel %d not found: %w", novelID, err)
	}
	// 校验调用方租户权限：防止跨租户创建视频
	if callerTenantID != 0 && novel.TenantID != 0 && novel.TenantID != callerTenantID {
		return nil, fmt.Errorf("novel %d does not belong to tenant %d", novelID, callerTenantID)
	}
	// 默认画面风格：继承项目设置中的画面风格
	if video.ArtStyle == "" && novel.ImageStyle != "" {
		video.ArtStyle = novel.ImageStyle
	}
	if video.FrameRate == 0 {
		video.FrameRate = 24
	}
	if video.Resolution == "" {
		video.Resolution = "1080p"
	}
	if video.AspectRatio == "" {
		video.AspectRatio = "16:9"
	}
	if video.QualityTier == "" {
		video.QualityTier = "preview"
	}
	if video.Mode == "" {
		video.Mode = "slideshow"
	}
	return video, s.videoRepo.Create(video)
}

// GetVideo 获取视频（内部调用，无租户隔离）
func (s *VideoService) GetVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetByID(id)
}

// GetVideoByTenant 获取视频（带租户隔离，防止越权访问）
func (s *VideoService) GetVideoByTenant(id, tenantID uint) (*model.Video, error) {
	return s.videoRepo.GetByIDAndTenant(id, tenantID)
}

// ListVideos 获取视频列表
func (s *VideoService) ListVideos(novelId *uint, chapterID *uint, status string, tenantID uint, page, pageSize int) ([]*model.Video, int, error) {
	videos, total, err := s.videoRepo.List(novelId, chapterID, status, tenantID, page, pageSize)
	return videos, int(total), err
}

// UpdateVideo 更新视频（带租户隔离校验）
func (s *VideoService) UpdateVideo(id, tenantID uint, req *model.UpdateVideoRequest) (*model.Video, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if tenantID != 0 && !s.videoBelongsToTenant(video, tenantID) {
		return nil, fmt.Errorf("video not found or access denied")
	}
	if req.Title != "" {
		video.Title = req.Title
	}
	if req.Resolution != "" {
		video.Resolution = req.Resolution
	}
	if req.FrameRate != 0 {
		video.FrameRate = req.FrameRate
	}
	if req.AspectRatio != "" {
		video.AspectRatio = req.AspectRatio
	}
	if req.ArtStyle != "" {
		video.ArtStyle = req.ArtStyle
	}
	if req.ScriptStatus != "" {
		video.ScriptStatus = req.ScriptStatus
	}
	if req.Mode != "" {
		video.Mode = req.Mode
	}
	if req.VisualMode != "" {
		video.VisualMode = req.VisualMode
	}
	if req.ThreeDStyle != "" {
		video.ThreeDStyle = req.ThreeDStyle
	}
	return video, s.videoRepo.Update(video)
}

// UpdatePacingConfig 更新视频的节奏和目标时长配置（供分镜生成前调用）
func (s *VideoService) UpdatePacingConfig(id uint, pacing string, targetDuration int) error {
	fields := map[string]interface{}{"target_duration": targetDuration}
	if pacing != "" {
		fields["pacing"] = pacing
	}
	return s.videoRepo.UpdateFields(id, fields)
}

// UpdateVideoFields 更新视频任意字段（用于发布状态更新）
func (s *VideoService) UpdateVideoFields(id uint, fields map[string]interface{}) error {
	return s.videoRepo.UpdateFields(id, fields)
}

// WithVideoSocial 注入广场社交仓库
func (s *VideoService) WithVideoSocial(likeRepo *repository.VideoLikeRepository, commentRepo *repository.VideoCommentRepository) *VideoService {
	s.videoLikeRepo = likeRepo
	s.videoCommentRepo = commentRepo
	s.cleanupOnce.Do(func() {
		go s.cleanupViewDedup()
	})
	return s
}

// cleanupViewDedup 每小时扫描并清除已过期的去重条目，防止 sync.Map 无限增长。
func (s *VideoService) cleanupViewDedup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.viewDedupCache.Range(func(k, v any) bool {
				if expiry, ok := v.(time.Time); ok && now.After(expiry) {
					s.viewDedupCache.Delete(k)
				}
				return true
			})
		case <-s.stopCh:
			return
		}
	}
}

// IncrVideoViewCount 增加视频播放量
func (s *VideoService) IncrVideoViewCount(id uint) error {
	return s.videoRepo.IncrViewCount(id)
}

// RecordViewDeduped 防刷播放量（同一 IP 对同一视频 1 小时内只计一次）
// Redis 可用时跨实例去重；否则降级为进程内去重。
func (s *VideoService) RecordViewDeduped(id uint, clientIP string) error {
	localKey := fmt.Sprintf("%s:%d", clientIP, id)
	if s.cache != nil {
		redisKey := fmt.Sprintf("view:video:%d:%s", id, clientIP)
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", time.Hour).Result()
		if err != nil {
			logger.Errorf("[VideoService] Redis view dedup error: %v, fallback to local", err)
			// fall through to local dedup
		} else if !ok {
			return nil // 已由某实例记录
		} else {
			return s.videoRepo.IncrViewCount(id)
		}
	}
	// 进程内兜底
	if v, ok := s.viewDedupCache.Load(localKey); ok {
		if expiry, ok2 := v.(time.Time); ok2 && time.Now().Before(expiry) {
			return nil
		}
	}
	s.viewDedupCache.Store(localKey, time.Now().Add(time.Hour))
	return s.videoRepo.IncrViewCount(id)
}

// GetPublicVideo 获取单条公开视频（无需 tenantID）
func (s *VideoService) GetPublicVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetPublicByID(id)
}

// ListPublicVideos 列出公开视频（支持排序 latest|hot 和关键词搜索）
func (s *VideoService) ListPublicVideos(sort, q string, page, pageSize int) ([]*model.Video, int64, error) {
	if sort == "" {
		sort = "hot"
	}
	return s.videoRepo.ListPublicSorted(sort, q, page, pageSize)
}

// ToggleVideoLike 点赞/取消点赞，返回最终状态
func (s *VideoService) ToggleVideoLike(videoID, userID uint) (liked bool, err error) {
	if s.videoLikeRepo == nil {
		return false, fmt.Errorf("like feature not available")
	}
	liked, err = s.videoLikeRepo.Toggle(videoID, userID)
	if err != nil {
		return false, err
	}
	delta := 1
	if !liked {
		delta = -1
	}
	_ = s.videoRepo.IncrLikeCount(videoID, delta)
	return liked, nil
}

// IsVideoLiked 检查用户是否已点赞
func (s *VideoService) IsVideoLiked(videoID, userID uint) bool {
	if s.videoLikeRepo == nil {
		return false
	}
	exists, _ := s.videoLikeRepo.Exists(videoID, userID)
	return exists
}

// ListVideoComments 获取评论列表
func (s *VideoService) ListVideoComments(videoID uint, page, size int) ([]*model.VideoComment, int64, error) {
	if s.videoCommentRepo == nil {
		return []*model.VideoComment{}, 0, nil
	}
	return s.videoCommentRepo.ListByVideo(videoID, page, size)
}

// AddVideoComment 发表评论
func (s *VideoService) AddVideoComment(videoID, userID uint, content string, parentID *uint) (*model.VideoComment, error) {
	if s.videoCommentRepo == nil {
		return nil, fmt.Errorf("comment feature not available")
	}
	c := &model.VideoComment{
		VideoID:  videoID,
		UserID:   userID,
		Content:  content,
		ParentID: parentID,
	}
	if err := s.videoCommentRepo.Create(c); err != nil {
		return nil, err
	}
	_ = s.videoRepo.IncrCommentCount(videoID, 1)
	return c, nil
}

// DeleteVideoComment 删除评论（只允许作者删除）
func (s *VideoService) DeleteVideoComment(commentID, callerID uint) error {
	if s.videoCommentRepo == nil {
		return fmt.Errorf("comment feature not available")
	}
	c, err := s.videoCommentRepo.GetByID(commentID)
	if err != nil {
		return err
	}
	if c.UserID != callerID {
		return ErrPermissionDenied
	}
	if err := s.videoCommentRepo.Delete(commentID); err != nil {
		return err
	}
	_ = s.videoRepo.IncrCommentCount(c.VideoID, -1)
	return nil
}

// RecalcVideoHotScores 重新计算所有公开视频热度分（定时任务调用）
// HotScore = view_count×0.5 + like_count×0.3 + comment_count×0.2，叠加时间衰减
// 使用 BatchUpdateHotScores 批量写入，避免 N+1 问题。
func (s *VideoService) RecalcVideoHotScores() error {
	const batchSize = 500
	offset := 0
	now := time.Now()
	for {
		videos, err := s.videoRepo.ListPublicForHotCalcPaged(batchSize, offset)
		if err != nil || len(videos) == 0 {
			break
		}
		updates := make(map[uint]float64, len(videos))
		for _, v := range videos {
			ageDays := 1.0
			if v.PublishedAt != nil {
				ageDays = now.Sub(*v.PublishedAt).Hours()/24 + 1
			}
			// 简单时间衰减：分母随天数增长
			decay := 1.0 / (1.0 + ageDays*0.1)
			updates[v.ID] = (float64(v.ViewCount)*0.5 + float64(v.LikeCount)*0.3 + float64(v.CommentCount)*0.2) * decay
		}
		if err := s.videoRepo.BatchUpdateHotScores(updates); err != nil {
			logger.Errorf("RecalcVideoHotScores: batch update failed: %v", err)
		}
		offset += batchSize
	}
	return nil
}

// DeleteVideo 删除视频（级联删除所有子记录：分镜、语音段、音效、BGM段）
// tenantID: 调用方租户 ID；传 0 则跳过租户校验（内部调用）。
// 所有 DB 操作在同一事务中执行，任何步骤失败均回滚。
// OSS 文件删除在事务提交前收集 URL，事务成功后执行（best-effort，失败仅记录日志）。
func (s *VideoService) DeleteVideo(id, tenantID uint) error {
	// 租户归属校验：确认视频属于调用方租户
	if tenantID != 0 {
		v, err := s.videoRepo.GetByID(id)
		if err != nil {
			return fmt.Errorf("video not found or access denied")
		}
		if !s.videoBelongsToTenant(v, tenantID) {
			return fmt.Errorf("video not found or access denied")
		}
	}

	// 事务前收集所有需要清理的 OSS URL（读操作，不加锁）
	var urlsToDelete []string
	var video model.Video
	if err := s.videoRepo.DB().Unscoped().First(&video, id).Error; err == nil {
		if video.FinalVideoURL != "" {
			urlsToDelete = append(urlsToDelete, video.FinalVideoURL)
		}
		if video.CoverURL != "" {
			urlsToDelete = append(urlsToDelete, video.CoverURL)
		}
	}
	var shots []*model.StoryboardShot
	if err := s.videoRepo.DB().Unscoped().Where("video_id = ?", id).Find(&shots).Error; err == nil {
		for _, shot := range shots {
			if shot.ImageURL != "" {
				urlsToDelete = append(urlsToDelete, shot.ImageURL)
			}
			if shot.VideoURL != "" {
				urlsToDelete = append(urlsToDelete, shot.VideoURL)
			}
			if shot.ClipPath != "" {
				urlsToDelete = append(urlsToDelete, shot.ClipPath)
			}
		}
	}

	if err := s.videoRepo.DB().Transaction(func(tx *gorm.DB) error {
		// 1. 获取所有 shot IDs（含软删除记录，确保级联彻底）
		var shotIDs []uint
		if err := tx.Unscoped().Model(&model.StoryboardShot{}).
			Where("video_id = ?", id).
			Pluck("id", &shotIDs).Error; err != nil {
			return fmt.Errorf("DeleteVideo: pluck shot ids: %w", err)
		}

		if len(shotIDs) > 0 {
			// 2. 删除语音段（硬删除）
			if err := tx.Unscoped().Where("shot_id IN ?", shotIDs).
				Delete(&model.ShotVoiceSegment{}).Error; err != nil {
				return fmt.Errorf("DeleteVideo: delete voice segments: %w", err)
			}
			// 3. 删除音效条目（硬删除）
			if err := tx.Unscoped().Where("shot_id IN ?", shotIDs).
				Delete(&model.ShotSFXItem{}).Error; err != nil {
				return fmt.Errorf("DeleteVideo: delete sfx items: %w", err)
			}
		}
		// 4. 删除 BGM 分段
		if err := tx.Unscoped().Where("video_id = ?", id).
			Delete(&model.VideoBGMSegment{}).Error; err != nil {
			return fmt.Errorf("DeleteVideo: delete bgm segments: %w", err)
		}
		// 5. 删除分镜（硬删除）
		if err := tx.Unscoped().Where("video_id = ?", id).
			Delete(&model.StoryboardShot{}).Error; err != nil {
			return fmt.Errorf("DeleteVideo: delete shots: %w", err)
		}
		// 6. 删除视频本体
		if err := tx.Unscoped().Delete(&model.Video{}, id).Error; err != nil {
			return fmt.Errorf("DeleteVideo: delete video: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// DB 事务成功后，best-effort 清理 OSS 文件（最多重试3次，指数退避；失败只记录日志，不影响返回值）
	// 已分享到公共素材库的文件不删除，保证素材库链接持续有效。
	if len(urlsToDelete) > 0 {
		var sharedURLs []string
		s.videoRepo.DB().Raw(
			"SELECT storage_url FROM ink_asset WHERE storage_url IN ? AND status != 'trash' AND deleted_at IS NULL",
			urlsToDelete,
		).Scan(&sharedURLs)
		sharedSet := make(map[string]bool, len(sharedURLs))
		for _, u := range sharedURLs {
			sharedSet[u] = true
		}
		filtered := urlsToDelete[:0]
		for _, u := range urlsToDelete {
			if !sharedSet[u] {
				filtered = append(filtered, u)
			}
		}
		urlsToDelete = filtered
	}
	if s.storageSvc != nil {
		for _, u := range urlsToDelete {
			if u == "" {
				continue
			}
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				if err := s.storageSvc.Delete(context.Background(), u); err == nil {
					lastErr = nil
					break
				} else {
					lastErr = err
					if attempt < 2 {
						time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
					}
				}
			}
			if lastErr != nil {
				logger.Errorf("ALERT: orphaned OSS file not deleted after 3 attempts: %s — %v", u, lastErr)
			}
		}
	}

	// 清理视图去重缓存，避免已删除视频的去重记录残留。
	suffix := fmt.Sprintf(":%d", id)
	s.viewDedupCache.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && strings.HasSuffix(key, suffix) {
			s.viewDedupCache.Delete(k)
		}
		return true
	})
	if s.cache != nil {
		pattern := fmt.Sprintf("view:video:%d:*", id)
		if keys, err := s.cache.Keys(context.Background(), pattern).Result(); err == nil && len(keys) > 0 {
			s.cache.Del(context.Background(), keys...)
		}
	}
	return nil
}

// StartGeneration 开始生成视频（调用真实视频 Provider）
func (s *VideoService) StartGeneration(id uint) (string, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return "", err
	}

	// 租户状态校验
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
			"status": "failed", "error_message": err.Error(),
		}); updErr != nil {
			logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", err
	}

	// 选择 provider：优先 kling，其次 seedance，静态 map 或 DB 均可
	provider, providerName, provErr := s.resolveVideoProvider(s.videoTenantID(video), "")
	if provErr != nil {
		if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
			"status": "failed", "error_message": "no video provider configured",
		}); updErr != nil {
			logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", provErr
	}

	// 构建生成请求
	req := &ai.VideoGenerateRequest{
		Prompt:      fmt.Sprintf("%s — cinematic, high quality", video.Title),
		AspectRatio: video.AspectRatio,
		Duration:    defaultShotDurationSecs,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
			"status": "failed", "error_message": err.Error(), "retry_count": gorm.Expr("retry_count + 1"),
		}); updErr != nil {
			logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", fmt.Errorf("video generation start failed: %w", err)
	}

	// 持久化任务 ID 和 provider 信息
	if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
		"status": "generating", "provider_name": providerName, "task_id": task.TaskID, "error_message": "",
	}); updErr != nil {
		logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
	}

	return task.TaskID, nil
}

// VideoProvider is a minimal video provider descriptor used by the listing endpoint.
type VideoProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// ListVideoProviders returns configured video providers with display names.
// Falls back to DB lookup for tenant 1 when static map is empty.
func (s *VideoService) ListVideoProviders() []VideoProvider {
	if len(s.videoProviders) > 0 {
		result := make([]VideoProvider, 0, len(s.videoProviders))
		for name := range s.videoProviders {
			result = append(result, VideoProvider{Name: name, DisplayName: capableProviderDisplayName(name, "")})
		}
		return result
	}
	if s.aiService != nil {
		for _, name := range []string{"jimeng-video", "kling", "seedance"} {
			if _, err := s.aiService.GetTenantVideoProvider(1, name); err == nil {
				return []VideoProvider{{Name: name, DisplayName: capableProviderDisplayName(name, "")}}
			}
		}
	}
	return nil
}

// resolveVideoProvider 选择视频生成提供商：优先静态 map，其次 DB 租户配置。
// preferredName 为空时按 jimeng-video→kling→seedance 顺序尝试。
func (s *VideoService) resolveVideoProvider(tenantID uint, preferredName string) (ai.VideoProvider, string, error) {
	names := []string{"jimeng-video", "kling", "seedance"}
	if preferredName != "" {
		names = []string{preferredName, "jimeng-video", "kling", "seedance"}
	}
	// 先查静态 map
	for _, name := range names {
		if p, ok := s.videoProviders[name]; ok {
			return p, name, nil
		}
	}
	// 再查 DB
	if s.aiService != nil {
		for _, name := range names {
			if p, err := s.aiService.GetTenantVideoProvider(tenantID, name); err == nil {
				return p, name, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no video provider configured")
}

// hasVideoProvider 判断当前租户是否存在可用的视频生成提供商（静态或 DB）。
func (s *VideoService) hasVideoProvider(tenantID uint) bool {
	if len(s.videoProviders) > 0 {
		return true
	}
	if s.aiService != nil {
		_, err := s.aiService.GetTenantVideoProvider(tenantID, "")
		return err == nil
	}
	return false
}

// GetStoryboard 获取分镜列表


// GenerateSingleShot 触发单个分镜生成（异步）

func (s *VideoService) GenerateSingleShot(videoID, shotID uint, provider ...string) (*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}

	// Resolve provider and aspect ratio from novel project config (caller override wins)
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] failed to update shot status to generating: %v", err)
	}
	tenantID := s.videoTenantID(video)
	hasProvider := s.hasVideoProvider(tenantID)
	logger.Printf("GenerateSingleShot: shot %d videoID=%d mode=%q tenantID=%d hasVideoProvider=%v effectiveProvider=%q aspectRatio=%s",
		shotID, videoID, video.Mode, tenantID, hasProvider, effectiveProvider, aspectRatio)
	// 有视频提供商时优先调用 AI 视频模型，无论 video.Mode 如何
	if hasProvider {
		logger.Printf("GenerateSingleShot: shot %d → AI video mode (provider=%q, videoMode=%q)", shotID, effectiveProvider, video.Mode)
		return shot, s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
	}
	// 无视频提供商：降级为图片解说 + Ken Burns
	logger.Printf("GenerateSingleShot: shot %d → slideshow fallback (no video provider for tenantID=%d, videoMode=%q)", shotID, tenantID, video.Mode)
	return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
}

// BatchGenerateShotClips 批量为已有图片的分镜生成 Ken Burns 动效视频（幂等：已有 VideoURL 的分镜自动跳过）。
// 只执行阶段二（Ken Burns 编码 + OSS 上传），不重新生成图片。
// 分镜必须已有 ImageURL；若没有图片则跳过并记录日志。
func (s *VideoService) BatchGenerateShotClips(videoID uint, shotIDs []uint, progressFn func(int)) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil && novel.VideoConf().VideoAspectRatio != "" {
			aspectRatio = novel.VideoConf().VideoAspectRatio
		}
	}

	logger.Printf("BatchGenerateShotClips: videoID=%d total=%d aspectRatio=%s", videoID, len(shotIDs), aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShotsClip, batchErrClip := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErrClip != nil {
		return nil, batchErrClip
	}
	shotMapClip := make(map[uint]*model.StoryboardShot, len(allShotsClip))
	for _, sh := range allShotsClip {
		shotMapClip[sh.ID] = sh
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32

	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	for _, sid := range shotIDs {
		shot, ok := shotMapClip[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.VideoURL != "" {
			// Already has clip — skip (idempotent).
			advanceProgress()
			continue
		}
		if shot.ImageURL == "" {
			logger.Printf("BatchGenerateShotClips: shot %d skipped — no image", shot.ShotNo)
			advanceProgress()
			continue
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShotClips: shot %d done", sh.ShotNo)
			}()
			duration := sh.Duration
			if duration <= 0 {
				duration = defaultShotDurationSecs
			}
			localImage, dlErr := s.resolveImageURLToLocalFile(sh.ImageURL, fmt.Sprintf("inkframe-img-%d-", sh.ID))
			if dlErr != nil {
				logger.Errorf("BatchGenerateShotClips: shot %d resolve image failed: %v", sh.ShotNo, dlErr)
				return
			}
			defer os.Remove(localImage)

			// Ken Burns encode (with still-frame fallback), same logic as generateClipAndUploadWithRetry.
			var clipPath string
			var lastErr error
			for attempt := 1; attempt <= maxClipRetries; attempt++ {
				clipPath, lastErr = s.generateKenBurnsPureGo(context.Background(), sh, localImage, duration, aspectRatio) // background ctx is intentional: decoupled from HTTP request
				if lastErr != nil {
					clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
				}
				if lastErr == nil {
					break
				}
				logger.Errorf("BatchGenerateShotClips: shot %d attempt %d/%d: %v", sh.ShotNo, attempt, maxClipRetries, lastErr)
				if attempt < maxClipRetries {
					time.Sleep(time.Duration(attempt*5) * time.Second)
				}
			}

			fields := map[string]interface{}{"progress": 100}
			if lastErr != nil {
				logger.Errorf("BatchGenerateShotClips: shot %d clip failed: %v", sh.ShotNo, lastErr)
			} else if ossURL := s.uploadClipToStorage(context.Background(), sh, clipPath); ossURL != "" {
				fields["video_url"] = ossURL
				fields["clip_path"] = ""
				os.Remove(clipPath) //nolint:errcheck
				logger.Printf("BatchGenerateShotClips: shot %d clip → %s", sh.ShotNo, ossURL)
			} else {
				fields["clip_path"] = "file://" + clipPath
			}
			if err := s.storyboardRepo.UpdateFields(sh.ID, fields); err != nil {
				logger.Errorf("[VideoService] BatchGenerateShotClips: failed to update shot %d fields: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotClips: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}
