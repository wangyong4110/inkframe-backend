package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // PNG 解码支持（合成参考图时可能遇到 PNG 格式）
	"net/http"
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
	reviewRecordRepo   *repository.StoryboardReviewRecordRepository
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

// charListEntry is a TTL-bounded cache entry for ListByNovel results.
type charListEntry struct {
	chars     []*model.Character
	expiresAt time.Time
}

// startCharListCacheCleanup 启动 charListCache 的后台定期清理（每 5 分钟扫描一次，删除已过期条目）。
// 应在 VideoService 构造时调用一次，防止长期运行后缓存无限膨胀。
func (s *VideoService) startCharListCacheCleanup() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				s.charListCache.Range(func(k, v interface{}) bool {
					if entry, ok := v.(*charListEntry); ok && now.After(entry.expiresAt) {
						s.charListCache.Delete(k)
					}
					return true
				})
			case <-s.stopCh:
				return
			}
		}
	}()
}

// listCharsByNovelCached returns the character list for a novel, using a 60-second
// in-process cache to avoid repeated DB calls during batch voice generation.
func (s *VideoService) listCharsByNovelCached(novelID uint) ([]*model.Character, error) {
	if v, ok := s.charListCache.Load(novelID); ok {
		if entry := v.(*charListEntry); time.Now().Before(entry.expiresAt) {
			return entry.chars, nil
		}
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	s.charListCache.Store(novelID, &charListEntry{
		chars:     chars,
		expiresAt: time.Now().Add(60 * time.Second),
	})
	return chars, nil
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

func (s *VideoService) WithReviewRecordRepo(r *repository.StoryboardReviewRecordRepository) *VideoService {
	s.reviewRecordRepo = r
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

	// 选择 provider：优先 kling，其次 seedance，均无则返回错误
	providerName := "kling"
	provider, ok := s.videoProviders[providerName]
	if !ok {
		providerName = "seedance"
		provider, ok = s.videoProviders[providerName]
	}
	if !ok {
		// 无可用 provider：标记失败并返回错误
		if updErr := s.videoRepo.UpdateFields(video.ID, map[string]interface{}{
			"status": "failed", "error_message": "no video provider configured (set KLING_API_KEY or SEEDANCE_API_KEY)",
		}); updErr != nil {
			logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, updErr)
		}
		return "", fmt.Errorf("no video provider configured")
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
func (s *VideoService) ListVideoProviders() []VideoProvider {
	result := make([]VideoProvider, 0, len(s.videoProviders))
	for name := range s.videoProviders {
		result = append(result, VideoProvider{Name: name, DisplayName: capableProviderDisplayName(name, "")})
	}
	return result
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
	if len(s.videoProviders) == 0 {
		logger.Printf("GenerateSingleShot: no video provider available, falling back to slideshow for shot %d (video %d)", shotID, videoID)
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	return shot, s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
}

// maxConcurrentShots 限制同时提交给视频提供商的并发数，防止触发 API 429
const maxConcurrentShots = 3

// downloadHTTPClient 用于下载生成的图片/视频文件。
// 设置 5 分钟超时，防止 CDN 接受连接后挂起导致 goroutine 永久阻塞（批量生成卡在 99% 的根本原因）。
var downloadHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// BatchGenerateShots 批量触发指定分镜生成（同步等待所有完成，支持并发限制）
// 图片解说模式(Mode=="slideshow")只生成图片，不生成 Ken Burns 短片。
func (s *VideoService) BatchGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string, progressFn func(int), provider ...string) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	if qualityTierOverride != "" {
		video.QualityTier = qualityTierOverride
	}

	// Resolve effective provider and aspect ratio from novel config
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

	mode := video.Mode
	if mode == "" {
		mode = "video"
	}
	logger.Printf("BatchGenerateShots: videoID=%d total=%d mode=%s provider=%s aspectRatio=%s", videoID, len(shotIDs), mode, effectiveProvider, aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShots, batchErr := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErr != nil {
		return nil, batchErr
	}
	shotMap := make(map[uint]*model.StoryboardShot, len(allShots))
	for _, sh := range allShots {
		shotMap[sh.ID] = sh
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32
	for _, sid := range shotIDs {
		shot, ok := shotMap[sid]
		if !ok || shot.VideoID != videoID {
			if progressFn != nil && total > 0 {
				pct := int(done.Add(1)) * 99 / total
				progressFn(pct)
			}
			continue
		}
		shot.Status = "generating"
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", shot.ShotNo, err)
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				n := int(done.Add(1))
				if progressFn != nil && total > 0 {
					pct := n * 99 / total
					progressFn(pct)
				}
				logger.Printf("BatchGenerateShots: shot %d done (%d/%d)", sh.ShotNo, n, total)
			}()
			logger.Printf("BatchGenerateShots: shot %d start (mode=%s)", sh.ShotNo, mode)
			const maxRetries = 3
			var genErr error
			if video.Mode == "slideshow" || len(s.videoProviders) == 0 {
				// ── 两阶段异步模式 ──────────────────────────────────────────────────
				// 阶段一（同步，占用 sem）：AI 图片生成 → 下载到本地
				// 阶段二（异步，释放 sem 后后台执行）：Ken Burns 编码 → OSS 上传，支持自动重试
				var localImage string
				var clipDur float64
				for attempt := 1; attempt <= maxRetries; attempt++ {
					localImage, clipDur, genErr = s.generateShotImageOnly(sh, aspectRatio)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d image attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr == nil {
					// 图片就绪：立即标记 completed（progress=50）供前端展示
					if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "completed", "progress": 50,
					}); err != nil {
						logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", sh.ShotNo, err)
					}
					// Ken Burns + OSS 在后台异步执行，完成后 progress=100
					go func(shotID uint, imgPath string, dur float64, ar string) {
						bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Minute)
						defer bgCancel()
						s.generateClipAndUploadWithRetry(bgCtx, shotID, imgPath, dur, ar)
					}(sh.ID, localImage, clipDur, aspectRatio)
					logger.Printf("BatchGenerateShots: shot %d image ready, clip async", sh.ShotNo)
				} else {
					logger.Printf("BatchGenerateShots: shot %d image failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				}
			} else {
				// ── AI 视频模式：原有同步逻辑（提交 → provider 轮询）──────────────
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateShotVideo(sh, aspectRatio, effectiveProvider)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr != nil {
					logger.Printf("BatchGenerateShots: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				} else {
					logger.Printf("BatchGenerateShots: shot %d submitted successfully (taskID=%s)", sh.ShotNo, sh.ShotTaskID)
				}
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShots: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// BatchGenerateShotImages 批量为分镜生成参考图片（幂等：已有 ImageURL 的分镜自动跳过）。
// 只执行阶段一（AI 图片生成），不启动 Ken Burns 编码。
func (s *VideoService) BatchGenerateShotImages(videoID uint, shotIDs []uint, progressFn func(int)) ([]*model.StoryboardShot, error) {
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

	logger.Printf("BatchGenerateShotImages: videoID=%d total=%d aspectRatio=%s", videoID, len(shotIDs), aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShotsImg, batchErrImg := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErrImg != nil {
		return nil, batchErrImg
	}
	shotMapImg := make(map[uint]*model.StoryboardShot, len(allShotsImg))
	for _, sh := range allShotsImg {
		shotMapImg[sh.ID] = sh
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
		shot, ok := shotMapImg[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.ImageURL != "" {
			// Already has image — skip (idempotent).
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
				logger.Printf("BatchGenerateShotImages: shot %d done", sh.ShotNo)
			}()
			const maxRetries = 3
			var localImage string
			var genErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				localImage, _, genErr = s.generateShotImageOnly(sh, aspectRatio)
				if genErr == nil {
					break
				}
				logger.Printf("BatchGenerateShotImages: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if localImage != "" {
				os.Remove(localImage) //nolint:errcheck  // temp file not needed; ImageURL is in DB
			}
			if genErr == nil {
				if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
					"status": "completed", "progress": 50,
				}); err != nil {
					logger.Printf("[VideoService] BatchGenerateShotImages: failed to update shot %d status: %v", sh.ShotNo, err)
				}
				logger.Printf("BatchGenerateShotImages: shot %d image ready", sh.ShotNo)
			} else {
				logger.Printf("BatchGenerateShotImages: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotImages: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
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

// GetStatus 获取视频生成状态（从 provider 同步最新进度）

// generateShotReferenceImage 为分镜生成参考帧图像，返回图片URL和错误。
// ─── 参考图合成辅助函数 ─────────────────────────────────────────────────────

const (
	maxCompositeImages    = 4   // 最多合成张数（角色最多3张 + 场景1张）
	compositeTargetHeight = 512 // 等高缩放目标高度（px）
)

// compositeRefImages 将多张参考图等高缩放后横向拼接为一张，上传到 OSS（或降级为临时文件），返回 URL。
// 若只有一张图，直接返回原 URL 不做处理。
func (s *VideoService) compositeRefImages(ctx context.Context, imageURLs []string, tenantID uint) (string, error) {
	if len(imageURLs) == 0 {
		return "", fmt.Errorf("compositeRefImages: no images")
	}
	if len(imageURLs) == 1 {
		return imageURLs[0], nil
	}
	if len(imageURLs) > maxCompositeImages {
		imageURLs = imageURLs[:maxCompositeImages]
	}

	type imgEntry struct {
		img image.Image
		url string
	}
	var decoded []imgEntry
	for _, u := range imageURLs {
		localPath, dlErr := downloadToTemp(u, "inkframe-ref-", ".jpg")
		if dlErr != nil {
			logger.Printf("compositeRefImages: download failed (%s): %v", u, dlErr)
			continue
		}
		f, openErr := os.Open(localPath)
		if openErr != nil {
			os.Remove(localPath) //nolint:errcheck
			continue
		}
		img, _, decErr := image.Decode(f)
		f.Close()
		os.Remove(localPath) //nolint:errcheck
		if decErr != nil {
			logger.Printf("compositeRefImages: decode failed (%s): %v", u, decErr)
			continue
		}
		decoded = append(decoded, imgEntry{img: img, url: u})
	}

	if len(decoded) == 0 {
		return "", fmt.Errorf("compositeRefImages: all images failed to load")
	}
	if len(decoded) == 1 {
		return decoded[0].url, nil // 只有一张解码成功，直接复用原 URL
	}

	// 等高缩放到 compositeTargetHeight，按宽高比计算各图缩放后宽度
	const H = compositeTargetHeight
	totalW := 0
	widths := make([]int, len(decoded))
	for i, e := range decoded {
		b := e.img.Bounds()
		if b.Dy() > 0 && b.Dx() > 0 {
			widths[i] = b.Dx() * H / b.Dy()
		}
		if widths[i] < 1 {
			widths[i] = H
		}
		totalW += widths[i]
	}

	// 创建横向拼接画布（白色背景）
	canvas := image.NewRGBA(image.Rect(0, 0, totalW, H))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	x := 0
	for i, e := range decoded {
		dstRect := image.Rect(x, 0, x+widths[i], H)
		refCompositeDrawScaled(canvas, dstRect, e.img)
		x += widths[i]
	}

	// 编码为 JPEG
	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 88}); encErr != nil {
		return "", fmt.Errorf("compositeRefImages: encode: %w", encErr)
	}

	// 上传到 OSS（若配置了 storageSvc）
	if s.storageSvc != nil {
		key := fmt.Sprintf("composites/%d/ref-%d.jpg", tenantID, time.Now().UnixNano())
		ossURL, upErr := s.storageSvc.Upload(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "image/jpeg")
		if upErr == nil {
			return ossURL, nil
		}
		logger.Printf("compositeRefImages: OSS upload failed (falling back to temp file): %v", upErr)
	}

	// 降级：保存为临时文件，返回 file:// URL
	tmp, err := os.CreateTemp("", "inkframe-composite-*.jpg")
	if err != nil {
		return "", fmt.Errorf("compositeRefImages: create temp: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return "", fmt.Errorf("compositeRefImages: write temp: %w", err)
	}
	tmp.Close()
	return "file://" + tmp.Name(), nil
}

// refCompositeDrawScaled 最近邻缩放，将 src 绘制到 dst 的 dstRect 区域。
func refCompositeDrawScaled(dst draw.Image, dstRect image.Rectangle, src image.Image) {
	srcBounds := src.Bounds()
	srcW, srcH := srcBounds.Dx(), srcBounds.Dy()
	dstW, dstH := dstRect.Dx(), dstRect.Dy()
	if srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 {
		return
	}
	for dy := 0; dy < dstH; dy++ {
		sy := dy*srcH/dstH + srcBounds.Min.Y
		for dx := 0; dx < dstW; dx++ {
			sx := dx*srcW/dstW + srcBounds.Min.X
			dst.Set(dstRect.Min.X+dx, dstRect.Min.Y+dy, src.At(sx, sy))
		}
	}
}

// ─── 分镜参考图生成 ──────────────────────────────────────────────────────────

func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// charBestImage 返回角色的最佳参考图 URL。
	// 优先级：ThreeViewSheet（三视图合图，一致性最强）> Portrait（肖像）
	charBestImage := func(c *model.Character) string {
		if c.ThreeViewSheet != "" {
			return c.ThreeViewSheet
		}
		return c.Portrait
	}

	// 精准匹配：批量加载 shot.CharacterIDs 中的所有角色参考图（最多 maxCompositeImages-1 张，留槽给场景锚点）
	const maxCharRefs = maxCompositeImages - 1
	var characterPortraits []string // 可能包含多个角色的图
	var refSources []string
	if len(shot.CharacterIDs) > 0 {
		ids := []uint(shot.CharacterIDs)
		batchChars, batchErr := s.characterRepo.ListByIDs(ids)
		if batchErr == nil {
			for _, char := range batchChars {
				if len(characterPortraits) >= maxCharRefs {
					break
				}
				if char.ThreeViewSheet != "" {
					characterPortraits = append(characterPortraits, char.ThreeViewSheet)
					refSources = append(refSources, fmt.Sprintf("charID=%d ThreeViewSheet", char.ID))
				} else if char.Portrait != "" {
					characterPortraits = append(characterPortraits, char.Portrait)
					refSources = append(refSources, fmt.Sprintf("charID=%d Portrait", char.ID))
				}
			}
		}
	}

	// cachedNovelChars 延迟加载：降级一、降级二共用，避免重复 ListByNovel 查询
	var cachedNovelChars []*model.Character

	// 降级一：若 CharacterIDs 未命中，从 shot.Characters JSON 内联名称匹配
	// （CharacterIDs 由 autoMatchShotCharacters 在分镜生成时设置，若名称有偏差则可能为空）
	if len(characterPortraits) == 0 && shot.Characters != "" {
		var shotChars []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err == nil && len(shotChars) > 0 {
			if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil && video.NovelID > 0 {
				if cachedNovelChars == nil {
					cachedNovelChars, _ = s.characterRepo.ListByNovel(video.NovelID)
				}
				if len(cachedNovelChars) > 0 {
					nameMap := make(map[string]*model.Character, len(cachedNovelChars))
					for _, c := range cachedNovelChars {
						nameMap[strings.ToLower(c.Name)] = c
					}
					for _, sc := range shotChars {
						if len(characterPortraits) >= maxCharRefs {
							break
						}
						nameLow := strings.ToLower(sc.Name)
						char, ok := nameMap[nameLow]
						if !ok {
							for n, c := range nameMap {
								if strings.Contains(nameLow, n) || strings.Contains(n, nameLow) {
									char = c
									ok = true
									break
								}
							}
						}
						if ok && char != nil {
							if char.ThreeViewSheet != "" {
								characterPortraits = append(characterPortraits, char.ThreeViewSheet)
								refSources = append(refSources, fmt.Sprintf("inline name=%q ThreeViewSheet", sc.Name))
							} else if char.Portrait != "" {
								characterPortraits = append(characterPortraits, char.Portrait)
								refSources = append(refSources, fmt.Sprintf("inline name=%q Portrait", sc.Name))
							}
						}
					}
				}
			}
		}
	}

	// 降级二：通过 ChapterID 找到 NovelID，取第一个有参考图的角色；同时获取章节序号用于 OSS 路径
	var chapterNo int
	if shot.ChapterID != nil && s.chapterRepo != nil {
		if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
			chapterNo = chapter.ChapterNo
			if len(characterPortraits) == 0 {
				if cachedNovelChars == nil {
					cachedNovelChars, _ = s.characterRepo.ListByNovel(chapter.NovelID)
				}
				for _, c := range cachedNovelChars {
					img := charBestImage(c)
					if img != "" {
						characterPortraits = append(characterPortraits, img)
						refSources = append(refSources, fmt.Sprintf("novel first char=%q", c.Name))
						break
					}
				}
			}
		}
	}
	logger.Printf("generateShotReferenceImage: shot %d charIDs=%v sources=%v portraits=%d",
		shot.ShotNo, shot.CharacterIDs, refSources, len(characterPortraits))

	promptText := shot.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	// 场景锚点：注入锁定词，并收集场景参考图
	var sceneRefImage string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
		}
	}

	// 合并所有参考图 URL（角色图 + 场景锚点图），稍后在 tenantID 确定后执行合成
	allRefImages := make([]string, 0, len(characterPortraits)+1)
	allRefImages = append(allRefImages, characterPortraits...)
	if sceneRefImage != "" {
		allRefImages = append(allRefImages, sceneRefImage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 获取视频的 ArtStyle、TenantID、质量档位和角色一致性权重
	artStyle := ""
	var tenantID uint
	charConsistencyWeight := 1.0 // 默认严格一致
	qualityTier := "preview"     // 默认质量档位
	if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
		artStyle = video.ArtStyle
		tenantID = video.TenantID
		if video.QualityTier != "" {
			qualityTier = video.QualityTier
		}
		if video.NovelID > 0 && s.novelRepo != nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				if tenantID == 0 {
					tenantID = novel.TenantID
				}
				if novel.VideoConf().CharConsistencyWeight > 0 {
					charConsistencyWeight = novel.VideoConf().CharConsistencyWeight
				}
				// 降级：视频未设置画面风格时使用项目设置中的画面风格
				if artStyle == "" && novel.ImageStyle != "" {
					artStyle = novel.ImageStyle
				}
				// 注入 OSS 路径提示（项目名+章节序号）
				if novel.Title != "" {
					ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title, ChapterNo: chapterNo})
				}
			}
		}
	}

	// 合成参考图：多张时横向拼接为单一图（角色一致性 + 场景锚点同时作为参考）
	var refImage string
	switch len(allRefImages) {
	case 0:
		// 无任何参考图，纯文本生成
	case 1:
		refImage = allRefImages[0]
	default:
		composited, compErr := s.compositeRefImages(ctx, allRefImages, tenantID)
		if compErr != nil {
			logger.Printf("generateShotReferenceImage: shot %d composite failed (%v), using first image", shot.ShotNo, compErr)
			refImage = allRefImages[0]
		} else {
			refImage = composited
			logger.Printf("generateShotReferenceImage: shot %d composited %d ref images → %s", shot.ShotNo, len(allRefImages), composited)
		}
	}

	// 根据质量档位注入分辨率提示，引导图像提供者选择适当尺寸。
	// 对于支持 size 参数的提供者（如 DALL-E 3），此 hint 也作为备注说明。
	imgWidth, _, _ := qualityTierImageParams(qualityTier)
	if imgWidth > 0 {
		promptText = fmt.Sprintf("--w %d ", imgWidth) + promptText
	}
	logger.Printf("generateShotReferenceImage: shot %d qualityTier=%s imgWidth=%d", shot.ShotNo, qualityTier, imgWidth)

	// 镜头类型注解：根据景别选择光学特征，提升图像构图的电影感
	lensTypeMap := map[string]string{
		"extreme_close_up": "macro lens 100mm, extreme shallow DOF, bokeh",
		"close_up":         "portrait lens 85mm, shallow depth of field, subject isolation",
		"medium":           "standard lens 50mm, natural perspective",
		"wide":             "wide angle lens 24mm, deep focus, environmental context",
		"extreme_wide":     "ultra wide lens 16mm, expansive environment, dramatic perspective",
	}
	lensType := lensTypeMap[shot.ShotSize]
	if lensType == "" {
		lensType = "standard lens 50mm"
	}
	cinematicImgPrefix := "cinematic film photography, 35mm anamorphic lens, professional lighting setup, " + lensType + ", "
	promptText = cinematicImgPrefix + promptText

	imageURL, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", promptText, refImage, artStyle, "", charConsistencyWeight)
	if err != nil {
		logger.Printf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		logger.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	// 首图锁定：场景锚点无参考图时，将本次生成结果存为参考图
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if err := s.sceneAnchorSvc.AutoSetRefImage(*shot.SceneAnchorID, imageURL); err != nil {
			logger.Printf("[VideoService] AutoSetRefImage: %v", err)
		}
	}

	return imageURL, nil
}


// 成功后自动更新 DB 中的 ImageURL 并返回新 URL。
func (s *VideoService) RefineShotImage(shotID uint, suggestion string) (string, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return "", fmt.Errorf("shot %d not found: %w", shotID, err)
	}

	// 构建含修改建议的提示词（操作副本，不改 DB 原始字段）
	shotCopy := *shot
	basePrompt := shot.Prompt
	if basePrompt == "" {
		basePrompt = shot.Description
	}
	if suggestion != "" {
		shotCopy.Prompt = basePrompt + ". Modification: " + suggestion
	} else {
		shotCopy.Prompt = basePrompt
	}

	newURL, err := s.generateShotReferenceImage(&shotCopy)
	if err != nil {
		return "", fmt.Errorf("refine image for shot %d: %w", shotID, err)
	}

	// 持久化新图片 URL
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"image_url": newURL}); err != nil {
		logger.Printf("[VideoService] RefineShot: failed to update shot %d image URL: %v", shotID, err)
	}
	return newURL, nil
}

// resolveArtStyle 返回视频的画面风格：优先用 video.ArtStyle，降级到 novel.ImageStyle
func (s *VideoService) resolveArtStyle(videoID uint) string {
	if s.videoRepo == nil {
		return ""
	}
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return ""
	}
	if video.ArtStyle != "" {
		return video.ArtStyle
	}
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
			return novel.ImageStyle
		}
	}
	return ""
}

// extractLastFrame 使用 FFmpeg 提取视频最后一帧，返回本地 jpeg 路径
func (s *VideoService) extractLastFrame(clipPath string) (string, error) {
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(clipPath, "file://")

	tmpJpeg := fmt.Sprintf("%s/inkframe-lastframe-%d.jpg", inkframeTempDir(), time.Now().UnixNano())
	if _, err := runFFmpegCtx(context.Background(), "-y",
		"-sseof", "-0.1",
		"-i", localPath,
		"-vframes", "1",
		"-f", "image2",
		tmpJpeg,
	); err != nil {
		return "", fmt.Errorf("extractLastFrame failed: %w", err)
	}
	return tmpJpeg, nil
}

// emotionToKlingParams 根据情绪/摄像机类型映射最优的 Kling 生成参数。
// 动作/史诗场景使用 pro 模式 + 10 秒时长，获得更高画质；
// 风景/全景使用高 CFG + 10 秒；对话/温情使用 5 秒防止内容填充。
func emotionToKlingParams(emotion, cameraType string) (mode string, cfgScale float64, duration float64) {
	// 将情绪标签规范化到英文
	e := strings.ToLower(emotion)
	ct := strings.ToLower(cameraType)

	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") ||
		strings.Contains(e, "action") || strings.Contains(e, "fight"):
		return "pro", 0.45, 10

	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "壮观") ||
		strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "pro", 0.5, 10

	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") ||
		strings.Contains(e, "危险") || strings.Contains(e, "恐惧"):
		return "std", 0.7, 5

	case strings.Contains(e, "landscape") || strings.Contains(e, "scenery") ||
		strings.Contains(e, "风景") || strings.Contains(e, "空镜") ||
		ct == "crane" || (ct == "pan" && strings.Contains(e, "wide")):
		return "std", 0.8, 10

	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "std", 0.6, 5

	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "离别") || strings.Contains(e, "grief"):
		return "std", 0.65, 5

	default:
		return "std", 0.5, 5
	}
}

// GenerateShotVideo 为单个分镜提交视频生成任务
func (s *VideoService) GenerateShotVideo(shot *model.StoryboardShot, videoAspectRatio string, providerOverride ...string) error {
	// 并发限流：若配置了 video_concurrency，则在此处等待令牌
	s.videoSemMu.RLock()
	vsem := s.videoSem
	s.videoSemMu.RUnlock()
	if vsem != nil {
		vsem <- struct{}{}
		defer func() { <-vsem }()
	}

	providerName := "kling"
	if len(providerOverride) > 0 && providerOverride[0] != "" {
		providerName = providerOverride[0]
	}
	provider, ok := s.videoProviders[providerName]
	if !ok {
		// fallback to any available
		for name, p := range s.videoProviders {
			providerName = name
			provider = p
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("no video provider configured")
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	logger.Printf("GenerateShotVideo: shot %d provider=%s aspect=%s duration=%ds", shot.ShotNo, providerName, videoAspectRatio, shot.Duration)

	// 图片优先策略：先确保 shot.ImageURL 已有图片，再用其作为视频参考图（image-to-video）。
	// 若 ImageURL 已存在则直接复用，否则先生成并持久化，保证前端可见且视频有参考帧。
	referenceImage := shot.ReferenceImageURL
	if shot.ImageURL != "" {
		// 已有正式镜头图，直接复用，无需再次生成
		referenceImage = shot.ImageURL
		shot.FrameImageURL = shot.ImageURL
		logger.Printf("GenerateShotVideo: shot %d reusing existing ImageURL as reference: %s", shot.ShotNo, shot.ImageURL)
	} else {
		// 先生成图片：使用 shot.Prompt（image_prompt）+ 角色参考图 → 完整场景图
		logger.Printf("GenerateShotVideo: shot %d ImageURL empty, generating image first (charIDs=%v)", shot.ShotNo, shot.CharacterIDs)
		frameURL, frameErr := s.generateShotReferenceImage(shot)
		if frameErr != nil {
			logger.Printf("GenerateShotVideo: shot %d image generation failed (non-fatal, continuing without reference): %v", shot.ShotNo, frameErr)
		} else {
			logger.Printf("GenerateShotVideo: shot %d image generated: %s", shot.ShotNo, frameURL)
		}
		if frameURL != "" {
			shot.ImageURL = frameURL
			shot.FrameImageURL = frameURL
			referenceImage = frameURL
			// 立即持久化图片 URL，确保视频生成失败时图片不丢失
			if updateErr := s.storyboardRepo.Update(shot); updateErr != nil {
				logger.Printf("GenerateShotVideo: shot %d failed to persist ImageURL: %v", shot.ShotNo, updateErr)
			}
		}
	}

	// 场景锚点：将锁定词注入视频生成 prompt
	// 优先使用运镜提示词（MotionPrompt），若为空则降级到静态画面描述（Prompt）
	videoPrompt := shot.MotionPrompt
	if videoPrompt == "" {
		videoPrompt = shot.Prompt
	}
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}

	// 画面风格：注入视频 prompt（video.ArtStyle 优先，降级到 novel.ImageStyle）
	if videoArtStyle := s.resolveArtStyle(shot.VideoID); videoArtStyle != "" {
		videoPrompt = videoArtStyle + " style, " + videoPrompt
	}

	// TTS 对齐：若分镜有配音，确保视频时长不短于音频时长+缓冲。
	// alignShotDurationToTTS 仅返回调整值，不持久化到 DB。
	shotDuration := alignShotDurationToTTS(shot)

	// 动态 Kling 参数（根据情绪和摄像机类型选择最优配置）
	klingMode, klingCFG, klingDefaultDur := emotionToKlingParams(shot.EmotionalTone, shot.CameraType)
	if shotDuration <= 0 {
		shotDuration = klingDefaultDur
	}

	// 检查项目配置：KlingProForAction、HD、3D
	var hdEnabled, threeDEnabled bool
	var threeDStyle, klingModelOverride string
	if vid, vidErr := s.videoRepo.GetByID(shot.VideoID); vidErr == nil && vid.NovelID > 0 && s.novelRepo != nil {
		if novel, novelErr := s.novelRepo.GetByID(vid.NovelID); novelErr == nil {
			vc := novel.VideoConf()
			if klingMode == "pro" && !vc.KlingProForAction {
				klingMode = "std"
			}
			hdEnabled = vc.HDEnabled || strings.Contains(vid.VisualMode, "hd")
			threeDEnabled = vc.ThreeDEnabled || strings.Contains(vid.VisualMode, "3d")
			threeDStyle = vid.ThreeDStyle
			klingModelOverride = vc.KlingModel
		}
	}
	if threeDStyle == "" {
		threeDStyle = "cg"
	}
	// HD 模式：升级为更高清的模型并强制 pro
	if hdEnabled {
		if klingModelOverride == "" || klingModelOverride == "kling-v1" {
			klingModelOverride = "kling-v1-6"
		}
		klingMode = "pro"
	}

	// 电影级动态前缀——注入运镜词+情绪氛围词，移除 "film still" 静态词避免抑制视频动态感
	cinematicPrefix := buildCinematicPrefix(shot.CameraType, shot.EmotionalTone)
	// 3D 风格前缀
	if threeDEnabled {
		cinematicPrefix = resolve3DStylePrefix(threeDStyle) + ", " + cinematicPrefix
	}
	// 视频生成专属负向词：补充 static/still/frozen/slideshow 防止模型生成静止画面
	negativeBase := "blurry, low quality, watermark, text overlay, deformed, ugly, " +
		"bad anatomy, duplicate, morbid, mutilated, out of frame, extra limbs, " +
		"gross proportions, malformed limbs, " +
		"static image, still frame, frozen, no motion, slideshow, photo, " +
		"flickering, temporal inconsistency, abrupt scene change, jump cut"

	videoPromptFinal := cinematicPrefix + videoPrompt
	negativePrompt := negativeBase
	if shot.NegativePrompt != "" {
		negativePrompt = negativeBase + ", " + shot.NegativePrompt
	}

	// Seedance / Kling 均支持多张参考图：在主参考图（scene image）之外追加角色三视图和场景锚点图，
	// 提升角色一致性和场景一致性。
	var extraRefImages []string
	multiImageProviders := map[string]bool{"seedance": true, "kling": true}
	if multiImageProviders[providerName] && s.characterRepo != nil && len(shot.CharacterIDs) > 0 {
		if chars, charErr := s.characterRepo.ListByIDs([]uint(shot.CharacterIDs)); charErr == nil {
			for _, c := range chars {
				var img string
				if c.ThreeViewSheet != "" {
					img = c.ThreeViewSheet
				} else if c.Portrait != "" {
					img = c.Portrait
				}
				if img != "" && img != referenceImage {
					extraRefImages = append(extraRefImages, img)
				}
			}
		}
	}
	if multiImageProviders[providerName] && s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if _, anchorRefURL, anchorErr := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); anchorErr == nil && anchorRefURL != "" && anchorRefURL != referenceImage {
			extraRefImages = append(extraRefImages, anchorRefURL)
		}
	}

	req := &ai.VideoGenerateRequest{
		Prompt:         videoPromptFinal,
		NegativePrompt: negativePrompt,
		Duration:       shotDuration,
		AspectRatio:    videoAspectRatio,
		ImageURL:       referenceImage, // 主参考图（生成的场景图），image-to-video；空时退化为 text-to-video
		ImageURLs:      extraRefImages, // 额外参考图（Seedance 多图支持）
		CFGScale:       klingCFG,
		Mode:           klingMode,
		Model:          klingModelOverride,
	}

	logger.Printf("GenerateShotVideo: shot %d submitting to %s (hasRef=%v extraRefs=%d mode=%s cfg=%.2f prompt=%q)", shot.ShotNo, providerName, referenceImage != "", len(extraRefImages), klingMode, klingCFG, videoPromptFinal)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		logger.Printf("GenerateShotVideo: shot %d submit failed: %v", shot.ShotNo, err)
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	logger.Printf("GenerateShotVideo: shot %d submitted taskID=%s", shot.ShotNo, task.TaskID)
	shot.ShotTaskID = task.TaskID
	shot.ShotProviderName = providerName
	shot.Status = "processing"
	return s.storyboardRepo.Update(shot)
}

// buildCinematicPrefix 根据摄像机类型和情绪生成动态电影级 prompt 前缀。
// 刻意移除了 "film still"（静帧含义），改用 "cinematic sequence" 强化动态感。
func buildCinematicPrefix(cameraType, emotionalTone string) string {
	motion := cameraMotionToken(cameraType)
	atmos := emotionAtmosphereToken(emotionalTone)
	base := "cinematic sequence, professional cinematography, anamorphic lens, natural film grain, high dynamic range"
	if motion != "" {
		base = motion + ", " + base
	}
	if atmos != "" {
		base += ", " + atmos
	}
	return base + ", "
}

// cameraMotionToken 把 CameraType 映射为视频 prompt 运镜描述词。
func cameraMotionToken(cameraType string) string {
	switch strings.ToLower(cameraType) {
	case "pan":
		return "smooth camera pan"
	case "tilt":
		return "camera tilt movement"
	case "zoom":
		return "cinematic zoom"
	case "dolly":
		return "dolly shot, camera pushing forward"
	case "tracking", "track":
		return "smooth tracking shot following subject"
	case "crane", "crane_up":
		return "crane shot, camera rising dramatically"
	case "crane_down":
		return "crane shot, camera descending"
	case "arc":
		return "arc shot, camera orbiting subject"
	case "handheld":
		return "handheld camera, subtle natural shake"
	case "whip_pan":
		return "whip pan transition, fast swipe"
	default: // "static" or unknown — no motion token
		return ""
	}
}

// emotionAtmosphereToken 把情绪基调映射为氛围关键词，注入 prompt 以影响画面色调与动态能量。
func emotionAtmosphereToken(emotion string) string {
	e := strings.ToLower(emotion)
	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") || strings.Contains(e, "action"):
		return "intense action atmosphere, dynamic motion blur, adrenaline energy"
	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "epic grand atmosphere, sweeping cinematic motion, heroic scale"
	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") || strings.Contains(e, "tension"):
		return "dramatic tense atmosphere, deep shadows, ominous mood"
	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "soft romantic atmosphere, warm golden bokeh, intimate mood"
	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "grief") || strings.Contains(e, "离别") || strings.Contains(e, "melancholy"):
		return "melancholic somber atmosphere, cool desaturated tones, slow motion feel"
	case strings.Contains(e, "landscape") || strings.Contains(e, "风景") ||
		strings.Contains(e, "scenery") || strings.Contains(e, "空镜"):
		return "breathtaking scenic vista, sweeping majestic atmosphere"
	case strings.Contains(e, "peaceful") || strings.Contains(e, "平静") || strings.Contains(e, "calm"):
		return "serene tranquil atmosphere, soft diffused light, gentle motion"
	case strings.Contains(e, "funny") || strings.Contains(e, "humorous") || strings.Contains(e, "幽默"):
		return "lively energetic atmosphere, bright warm tones"
	default:
		return ""
	}
}

// resolve3DStylePrefix 返回对应 3D 风格的提示词前缀。
func resolve3DStylePrefix(style string) string {
	switch style {
	case "pixar":
		return "Pixar-style 3D animation, stylized characters, warm appealing lighting, Disney Pixar quality render"
	case "anime3d":
		return "3D anime style, cel-shaded 3D, vibrant colors, smooth 3D animation, Japanese anime 3D render"
	case "realistic3d":
		return "ultra-realistic 3D render, Unreal Engine 5, ray tracing global illumination, cinematic 3D, 8K 3D rendering"
	default: // "cg"
		return "3D CGI animation, ray tracing, volumetric lighting, subsurface scattering, photorealistic 3D render, high-fidelity 3D"
	}
}

// PollShotStatus 轮询单个分镜视频生成状态



// generateKenBurnsClip 使用 FFmpeg zoompan 滤镜将静图制作成 Ken Burns 动效短片
// generateStillFrameClip 用 FFmpeg 将静态图片编码为固定时长的视频（无动效，Ken Burns 降级方案）。
func (s *VideoService) generateStillFrameClip(imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}
	parts := strings.SplitN(resolution, ":", 2)
	w, h := parts[0], parts[1]
	vf := fmt.Sprintf("scale=%s:%s:force_original_aspect_ratio=decrease,pad=%s:%s:(ow-iw)/2:(oh-ih)/2,setsar=1", w, h, w, h)
	outPath := fmt.Sprintf("%s/inkframe-still-%s.mp4", inkframeTempDir(), uuid.New().String()[:8])
	logger.Printf("generateStillFrameClip: start image=%s duration=%.1fs res=%s → %s", imagePath, duration, resolution, outPath)
	encStart := time.Now()
	// 使用 goroutine 超时而非 context 超时：wazero 在密集计算时不响应 ctx 取消。
	// -preset ultrafast -tune stillimage 大幅降低 WASM x264 编码时间（静止帧全为 P 帧）。
	out, err := runFFmpegWithGoroutineTimeout(90*time.Second,
		"-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		"-r", "24",
		outPath,
	)
	if err != nil {
		logger.Printf("generateStillFrameClip: failed after %.1fs: %v\noutput: %s", time.Since(encStart).Seconds(), err, string(out))
		return "", fmt.Errorf("ffmpeg still frame: %w", err)
	}
	logger.Printf("generateStillFrameClip: done in %.1fs → %s", time.Since(encStart).Seconds(), outPath)
	return outPath, nil
}

func (s *VideoService) generateKenBurnsClip(shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	fps := 30
	totalFrames := int(duration * float64(fps))

	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}

	// 根据摄像机类型选择 zoompan 动效
	var zoompan string
	switch shot.CameraType {
	case "zoom", "push":
		// 推镜/变焦：明显放大，模拟向前推进
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.002,1.5)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	case "pull":
		// 拉镜：缩小，模拟向后拉远（从1.4缩到1.0）
		zoompan = fmt.Sprintf("zoompan=z='max(1.4-t*0.4/%.1f,1.0)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", duration, totalFrames)
	case "pan", "track":
		// 摇镜/移镜：水平平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f))':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	case "crane_up":
		// 升镜：向上平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(ih-(ih/zoom)-t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "crane_down":
		// 降镜：向下平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "whip_pan":
		// 甩镜：快速水平扫过
		zoompan = fmt.Sprintf("zoompan=z=1.2:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f)*2)':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	default:
		// static / follow / arc / tilt / 旧值：默认轻微放大（Ken Burns 经典效果）
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.0008,1.2)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	}

	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%s.mp4", inkframeTempDir(), shot.ID, uuid.New().String()[:8])
	// pre-scale 到恰好等于输出分辨率：zoompan 的 zoom≤1.2 只需输入≥输出即可，更大对效果无益
	// 但会让 WASM 每帧计算量成倍增加（3840 vs 1920 = 4x 像素量）。
	// 1920:-1 for 16:9, 1080:-1 for 9:16/1:1 — 均与最终输出宽度对齐。
	preScale := "1920:-1"
	if aspectRatio == "9:16" || aspectRatio == "1:1" {
		preScale = "1080:-1"
	}
	vf := fmt.Sprintf("scale=%s,%s,scale=%s,setsar=1", preScale, zoompan, resolution)

	// 30s：以 1920:-1 输入跑 zoompan，普通 CPU 通常在 10-25s 内完成；超时则快速降级 still frame。
	const kenBurnsTimeout = 30 * time.Second
	kenCtx, kenCancel := context.WithTimeout(context.Background(), kenBurnsTimeout)
	defer kenCancel()
	if _, err := runFFmpegCtx(kenCtx, "-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", fps),
		"-threads", "1",
		outPath,
	); err != nil {
		return "", fmt.Errorf("ffmpeg ken burns: %w", err)
	}
	return outPath, nil
}

// generateShotImageOnly 执行图片解说模式的第一阶段：生成图片 + 下载到本地临时文件。
// 返回本地文件路径和实际视频时长；调用方负责在使用完毕后删除该文件。
// shot.Status 会在此函数内被设置为 "generating"；完成后调用方应更新为 "completed"。
func (s *VideoService) generateShotImageOnly(shot *model.StoryboardShot, aspectRatio string) (localImage string, duration float64, err error) {
	duration = shot.Duration
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	// 视频时长不能低于音频时长
	if shot.AudioPath != "" {
		if data, readErr := readLocalOrRemoteFile(shot.AudioPath); readErr == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				if audioDur := float64(micros) / 1_000_000; audioDur > duration {
					logger.Printf("generateShotImageOnly: shot %d extending duration %.2f→%.2fs to cover audio", shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}
	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return "", 0, fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return "", 0, fmt.Errorf("image generation failed for shot %d (empty URL)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	localImage, err = downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		return "", 0, fmt.Errorf("download image for shot %d: %w", shot.ShotNo, err)
	}
	return localImage, duration, nil
}

// generateClipAndUploadWithRetry 在后台 goroutine 中执行 Ken Burns 编码 + OSS 上传，
// 支持最多 maxClipRetries 次自动重试（指数退避）。
// 无论成功与否，最终均将 progress 更新为 100，并清理本地临时文件。
const maxClipRetries = 3

func (s *VideoService) generateClipAndUploadWithRetry(ctx context.Context, shotID uint, localImage string, duration float64, aspectRatio string) {
	defer os.Remove(localImage)

	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		logger.Printf("generateClipAndUploadWithRetry: shot %d not found: %v", shotID, err)
		return
	}

	var clipPath string
	var lastErr error

	for attempt := 1; attempt <= maxClipRetries; attempt++ {
		// 优先纯 Go Ken Burns；失败时降级为静止画面
		clipPath, lastErr = s.generateKenBurnsPureGo(ctx, shot, localImage, duration, aspectRatio)
		if lastErr != nil {
			logger.Printf("generateClipAndUploadWithRetry: shot %d ken burns attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
			clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
		}
		if lastErr == nil {
			break
		}
		logger.Printf("generateClipAndUploadWithRetry: shot %d still frame attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
		if attempt < maxClipRetries {
			select {
			case <-time.After(time.Duration(attempt*5) * time.Second):
			case <-ctx.Done():
				logger.Printf("[VideoService] generateClipAndUploadWithRetry: context cancelled for shot %d, stopping retries", shotID)
				return
			}
		}
	}

	fields := map[string]interface{}{"progress": 100}
	if lastErr != nil {
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip failed after %d attempts, keeping image-only: %v", shot.ShotNo, maxClipRetries, lastErr)
	} else if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		fields["video_url"] = ossURL
		fields["clip_path"] = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip → %s", shot.ShotNo, ossURL)
	} else {
		fields["clip_path"] = "file://" + clipPath
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip done (local only)", shot.ShotNo)
	}
	if err := s.storyboardRepo.UpdateFields(shotID, fields); err != nil {
		logger.Printf("[VideoService] generateClipAndUploadWithRetry: failed to update shot %d fields: %v", shotID, err)
	}
}

// GenerateSlideshowShotVideo 为单个分镜生成图片并应用 Ken Burns 动效（图片解说模式）
// 此函数保持同步语义，供 runSlideshowPipeline 的顺序流水线使用。
// BatchGenerateShots 中的批量生成改用 generateShotImageOnly + generateClipAndUploadWithRetry 两阶段异步模式。
func (s *VideoService) GenerateSlideshowShotVideo(shot *model.StoryboardShot, aspectRatio string) error {
	duration := shot.Duration
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}

	// 视频时长不能低于音频时长：读取已生成的 TTS 音频，若音频更长则扩展 duration。
	if shot.AudioPath != "" {
		if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				audioDur := float64(micros) / 1_000_000
				if audioDur > duration {
					logger.Printf("GenerateSlideshowShotVideo: shot %d extending duration %.2f→%.2fs to cover audio",
						shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}

	logger.Printf("GenerateSlideshowShotVideo: shot %d aspect=%s duration=%.1fs", shot.ShotNo, aspectRatio, duration)

	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	// 1. 生成图片
	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		logger.Printf("GenerateSlideshowShotVideo: image gen failed for shot %d: %s", shot.ShotNo, errMsg)
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return fmt.Errorf("image generation failed for shot %d (empty URL returned)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	logger.Printf("GenerateSlideshowShotVideo: shot %d storing image_url=%q (len=%d)", shot.ShotNo, imageURL, len(imageURL))
	// 保存图片 URL（后续步骤失败时图片仍可用）
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// 2. 下载图片到本地（Volcengine 返回的 URL 后缀为 .image，FFmpeg 无法识别格式，需重命名为 .jpg）
	logger.Printf("GenerateSlideshowShotVideo: shot %d downloading image for ffmpeg", shot.ShotNo)
	localImage, err := downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		logger.Printf("GenerateSlideshowShotVideo: download image failed for shot %d, marking completed with image only: %v", shot.ShotNo, err)
		shot.Status = "completed"
		shot.Progress = 100
		shot.ErrorMessage = fmt.Sprintf("ken burns skipped: %v", err)
		return s.storyboardRepo.Update(shot)
	}
	defer os.Remove(localImage)

	// 3. Ken Burns 动效（缓慢推拉/平移，让静态图更生动）；失败时降级为静止画面
	// 首选纯 Go 实现（逐帧计算 + WASM 编码）：速度快、可被 context 取消。
	// 若失败则降级为静止画面（跳过旧的 WASM zoompan 方案以避免长时间阻塞）。
	logger.Printf("GenerateSlideshowShotVideo: shot %d starting ken burns (pure Go)", shot.ShotNo)
	clipPath, err := s.generateKenBurnsPureGo(context.Background(), shot, localImage, duration, aspectRatio) // background ctx is intentional: decoupled from HTTP request
	if err != nil {
		logger.Printf("GenerateSlideshowShotVideo: ken burns failed for shot %d, falling back to still frame: %v", shot.ShotNo, err)
		// 降级：用静止画面代替 Ken Burns
		clipPath, err = s.generateStillFrameClip(localImage, duration, aspectRatio)
		if err != nil {
			logger.Printf("GenerateSlideshowShotVideo: still frame fallback also failed for shot %d, completing with image only: %v", shot.ShotNo, err)
			shot.Status = "completed"
			shot.Progress = 100
			shot.ErrorMessage = fmt.Sprintf("ffmpeg unavailable: %v", err)
			return s.storyboardRepo.Update(shot)
		}
		shot.ErrorMessage = "ken burns skipped, used still frame"
	}

	// 优先上传到 OSS（持久存储），成功后清除本地 file:// 路径并删除临时文件。
	// 失败时降级保留 file:// 路径（本地可访问但重启后失效）。
	if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		shot.VideoURL = ossURL
		shot.ClipPath = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("GenerateSlideshowShotVideo: shot %d clip uploaded → %s", shot.ShotNo, ossURL)
	} else {
		shot.ClipPath = "file://" + clipPath
		logger.Printf("GenerateSlideshowShotVideo: shot %d completed clip=%s (local only)", shot.ShotNo, clipPath)
	}
	shot.Status = "completed"
	shot.Progress = 100
	return s.storyboardRepo.Update(shot)
}

// uploadClipToStorage 将本地 MP4 文件上传到持久存储（OSS），返回持久 URL。
// storageSvc 为 nil 或上传失败时返回 ""（调用方保留 file:// 本地路径）。
// OSS key 格式：novels/{title}/chapters/{no}/videos/{uuid}.mp4
//
//	章节 ID 未知时降级：videos/{uuid}.mp4

// runSlideshowPipeline 异步处理图片解说模式的所有分镜，完成后自动拼接
func (s *VideoService) runSlideshowPipeline(videoID uint) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Printf("runSlideshowPipeline: get video %d failed: %v", videoID, err)
		return
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil || len(shots) == 0 {
		logger.Printf("runSlideshowPipeline: no pending shots for video %d", videoID)
		return
	}

	var audioWg sync.WaitGroup
	for _, shot := range shots {
		if err := s.GenerateSlideshowShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("runSlideshowPipeline: shot %d failed: %v", shot.ShotNo, err)
		}
		audioWg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer audioWg.Done()
			if err := s.GenerateShotAudio(sh, video.TenantID, ""); err != nil {
				logger.Printf("runSlideshowPipeline: audio gen failed for shot %d: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	audioWg.Wait()

	// 拼接
	if _, err := s.StitchVideo(videoID); err != nil {
		logger.Printf("runSlideshowPipeline: stitch video %d failed: %v", videoID, err)
		if v, _ := s.videoRepo.GetByID(videoID); v != nil {
			v.Status = "failed"
			v.ErrorMessage = err.Error()
			if updErr := s.videoRepo.Update(v); updErr != nil {
				logger.Printf("[VideoService] runSlideshowPipeline: failed to update video %d status to failed: %v", videoID, updErr)
			}
		}
	}
}

// GenerateAllShotVideos 提交所有待生成分镜的视频任务
func (s *VideoService) GenerateAllShotVideos(videoID uint) error {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	// 图片解说模式：异步生成图片，完成后自动拼接
	if video.Mode == "slideshow" {
		shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if err != nil || len(shots) == 0 {
			return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
		}
		video.Status = "generating"
		video.ErrorMessage = ""
		if err := s.videoRepo.Update(video); err != nil {
			logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
		}
		go s.runSlideshowPipeline(videoID)
		return nil
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil {
		return err
	}
	if len(shots) == 0 {
		return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
	}

	// 更新状态，让用户可以通过 GetStatus 感知进度
	video.Status = "generating"
	video.ErrorMessage = ""
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
	}

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(shot, video.TenantID, "") //nolint:errcheck
	}
	return nil
}

