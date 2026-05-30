package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
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
	sfxService         *SFXService
	storageSvc         storage.Service
	sceneAnchorSvc     *SceneAnchorService
	plotPointRepo      *repository.PlotPointRepository
	systemSettingRepo  *repository.SystemSettingRepository
	segmentRepo        *repository.ShotVoiceSegmentRepository
	reviewRecordRepo      *repository.ReviewRecordRepository
	ignoredSuggestionRepo *repository.IgnoredReviewIssueRepository
	taskSvc            *TaskService
	videoSem           chan struct{} // nil = unlimited; set via WithVideoConcurrency
	videoSemMu         sync.RWMutex  // protects videoSem replacement
	audioSem           chan struct{} // limits concurrent TTS calls; nil = unlimited
	charListCache      sync.Map      // novelID → *charListEntry (short-lived cache for batch voice gen)
	// 广场社交
	videoLikeRepo    *repository.VideoLikeRepository
	videoCommentRepo *repository.VideoCommentRepository
	viewDedupCache   sync.Map     // key "ip:id" → expiry time.Time（防刷播放量）
	cleanupOnce      sync.Once
	stopCh           chan struct{} // closed by Shutdown() to stop background goroutines
}

// GetNovelByID 通过 novelRepo 加载小说（供 handler 传递给 CapCutService 等下游服务）
func (s *VideoService) GetNovelByID(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
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

func (s *VideoService) WithPlotPointRepo(repo *repository.PlotPointRepository) *VideoService {
	s.plotPointRepo = repo
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
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		video.TenantID = novel.TenantID
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
	video.TenantID = novel.TenantID
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
	videos, total, err := s.videoRepo.List(novelId, chapterID, tenantID, page, pageSize)
	return videos, int(total), err
}

// UpdateVideo 更新视频
func (s *VideoService) UpdateVideo(id uint, req *model.UpdateVideoRequest) (*model.Video, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
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
func (s *VideoService) RecordViewDeduped(id uint, clientIP string) error {
	key := fmt.Sprintf("%s:%d", clientIP, id)
	if v, ok := s.viewDedupCache.Load(key); ok {
		if expiry, ok2 := v.(time.Time); ok2 && time.Now().Before(expiry) {
			return nil // 已记录，跳过
		}
	}
	s.viewDedupCache.Store(key, time.Now().Add(time.Hour))
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
func (s *VideoService) AddVideoComment(videoID, userID uint, nickname, content string, parentID *uint) (*model.VideoComment, error) {
	if s.videoCommentRepo == nil {
		return nil, fmt.Errorf("comment feature not available")
	}
	c := &model.VideoComment{
		VideoID:  videoID,
		UserID:   userID,
		Nickname: nickname,
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
			logger.Printf("RecalcVideoHotScores: batch update failed: %v", err)
		}
		offset += batchSize
	}
	return nil
}

// DeleteVideo 删除视频
func (s *VideoService) DeleteVideo(id uint) error {
	return s.videoRepo.DeleteByID(id)
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
			logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", err
	}

	// 选择 provider：优先 kling，其次 seedance，静态 map 或 DB 均可
	provider, providerName, provErr := s.resolveVideoProvider(video.TenantID, "")
	if provErr != nil {
		if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
			"status": "failed", "error_message": "no video provider configured",
		}); updErr != nil {
			logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
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
			logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", fmt.Errorf("video generation start failed: %w", err)
	}

	// 持久化任务 ID 和 provider 信息
	if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
		"status": "generating", "provider_name": providerName, "task_id": task.TaskID, "error_message": "",
	}); updErr != nil {
		logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
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
		for _, name := range []string{"kling", "seedance"} {
			if _, err := s.aiService.GetTenantVideoProvider(1, name); err == nil {
				return []VideoProvider{{Name: name, DisplayName: capableProviderDisplayName(name, "")}}
			}
		}
	}
	return nil
}

// resolveVideoProvider 选择视频生成提供商：优先静态 map，其次 DB 租户配置。
// preferredName 为空时按 kling→seedance 顺序尝试。
func (s *VideoService) resolveVideoProvider(tenantID uint, preferredName string) (ai.VideoProvider, string, error) {
	names := []string{"kling", "seedance"}
	if preferredName != "" {
		names = []string{preferredName, "kling", "seedance"}
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
			if effectiveProvider == "" && novel.VideoModel != "" {
				effectiveProvider = novel.VideoModel
			}
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] failed to update shot status to generating: %v", err)
	}
	if video.Mode == "slideshow" {
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	// AI 视频模式：若没有可用的视频提供商，自动降级为图片解说模式
	if !s.hasVideoProvider(video.TenantID) {
		logger.Printf("GenerateSingleShot: no video provider available, falling back to slideshow for shot %d (video %d)", shotID, videoID)
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	return shot, s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
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
			localImage, dlErr := downloadToTemp(sh.ImageURL, fmt.Sprintf("inkframe-img-%d-", sh.ID), ".jpg")
			if dlErr != nil {
				logger.Printf("BatchGenerateShotClips: shot %d download failed: %v", sh.ShotNo, dlErr)
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
				logger.Printf("BatchGenerateShotClips: shot %d attempt %d/%d: %v", sh.ShotNo, attempt, maxClipRetries, lastErr)
				if attempt < maxClipRetries {
					time.Sleep(time.Duration(attempt*5) * time.Second)
				}
			}

			fields := map[string]interface{}{"progress": 100}
			if lastErr != nil {
				logger.Printf("BatchGenerateShotClips: shot %d clip failed: %v", sh.ShotNo, lastErr)
			} else if ossURL := s.uploadClipToStorage(context.Background(), sh, clipPath); ossURL != "" {
				fields["video_url"] = ossURL
				fields["clip_path"] = ""
				os.Remove(clipPath) //nolint:errcheck
				logger.Printf("BatchGenerateShotClips: shot %d clip → %s", sh.ShotNo, ossURL)
			} else {
				fields["clip_path"] = "file://" + clipPath
			}
			if err := s.storyboardRepo.UpdateFields(sh.ID, fields); err != nil {
				logger.Printf("[VideoService] BatchGenerateShotClips: failed to update shot %d fields: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotClips: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}
