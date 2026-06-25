package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

func timeNow() time.Time { return time.Now() }

// VideoHandler 视频处理器
type VideoHandler struct {
	videoService       *service.VideoService
	storyboardService  *service.StoryboardService
	enhancementService *service.VideoEnhancementService
	consistencyService *service.CharacterConsistencyService
	capcutService      *service.CapCutService
	taskSvc            *service.TaskService
	sfxSvc             *service.SFXService
	sfxItemRepo        *repository.ShotSFXItemRepository
	bgmSvc             *service.BGMService
	bgmRepo            *repository.VideoBGMSegmentRepository
	subtitleSvc        *service.SubtitleService
	storageSvc         storage.Service
	assetRepo          *repository.AssetRepository
}

func (h *VideoHandler) WithStorage(svc storage.Service) *VideoHandler {
	h.storageSvc = svc
	return h
}

// WithServerBaseURL 将服务器自身 base URL 注入 CapCutService，用于解析本地存储/DB 存储的相对路径媒体 URL。
func (h *VideoHandler) WithServerBaseURL(u string) *VideoHandler {
	h.capcutService.WithServerBaseURL(u)
	return h
}

func (h *VideoHandler) WithAssetRepo(r *repository.AssetRepository) *VideoHandler {
	h.assetRepo = r
	return h
}

// WithCapCutSegmentRepo 将 VoiceSegment 仓库注入 CapCutService，使导出包含多段配音音频（P1-2）。
func (h *VideoHandler) WithCapCutSegmentRepo(r *repository.ShotVoiceSegmentRepository) *VideoHandler {
	h.capcutService.WithSegmentRepo(r)
	return h
}

func (h *VideoHandler) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *VideoHandler {
	h.sfxItemRepo = r
	h.capcutService.WithSFXItemRepo(r) // 同时注入 CapCut 导出服务，使多条 SFX item 能正确导出
	return h
}

func (h *VideoHandler) WithBGMService(svc *service.BGMService) *VideoHandler {
	h.bgmSvc = svc
	return h
}

func (h *VideoHandler) WithBGMRepo(r *repository.VideoBGMSegmentRepository) *VideoHandler {
	h.bgmRepo = r
	return h
}

func (h *VideoHandler) WithSubtitleService(svc *service.SubtitleService) *VideoHandler {
	h.subtitleSvc = svc
	return h
}

// ExportSubtitles 导出视频字幕（ASS 格式）
// POST /api/v1/videos/:id/subtitles/export
func (h *VideoHandler) ExportSubtitles(c *gin.Context) {
	if h.subtitleSvc == nil {
		respondErr(c, http.StatusNotImplemented, "subtitle service not configured")
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}
	shots, err := h.videoService.GetStoryboard(video.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to load storyboard")
		return
	}

	// 获取字体配置
	fontName := "Noto Sans CJK SC"
	if vc := h.videoService.GetNovelVideoConfig(video.NovelID); vc != nil && vc.SubtitleFont != "" {
		fontName = vc.SubtitleFont
	}

	// 将 []*model.StoryboardShot 转换为 []model.StoryboardShot
	shotSlice := make([]model.StoryboardShot, len(shots))
	for i, s := range shots {
		if s != nil {
			shotSlice[i] = *s
		}
	}

	content := h.subtitleSvc.GenerateASS(shotSlice, fontName)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"video_%d_subtitles.ass\"", id))
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, content)
}

func NewVideoHandler(
	videoService *service.VideoService,
	storyboardService *service.StoryboardService,
	enhancementService *service.VideoEnhancementService,
	consistencyService *service.CharacterConsistencyService,
) *VideoHandler {
	return &VideoHandler{
		videoService:       videoService,
		storyboardService:  storyboardService,
		enhancementService: enhancementService,
		consistencyService: consistencyService,
		capcutService:      service.NewCapCutService(),
	}
}

func (h *VideoHandler) WithTaskService(svc *service.TaskService) *VideoHandler {
	h.taskSvc = svc
	return h
}

func (h *VideoHandler) WithSFXService(svc *service.SFXService) *VideoHandler {
	h.sfxSvc = svc
	return h
}

// getVideoForTenant 提取租户鉴权公共逻辑，减少重复代码。
// 返回 false 时已向 c 写入错误响应，调用方直接 return。
func (h *VideoHandler) getVideoForTenant(c *gin.Context, id uint) (*model.Video, bool) {
	video, err := h.videoService.GetVideoByTenant(id, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found")
		return nil, false
	}
	return video, true
}

// ListVideoProviders 列出已配置的视频生成提供者
// GET /api/v1/videos/providers
func (h *VideoHandler) ListVideoProviders(c *gin.Context) {
	respondOK(c, h.videoService.ListVideoProviders())
}

// CreateVideo 创建视频项目
// POST /api/v1/novels/:novel_id/videos
func (h *VideoHandler) CreateVideo(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.CreateVideoRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	video, err := h.videoService.CreateVideo(uint(novelId), &req, tenantID)
	if err != nil {
		logger.Errorf("[VideoHandler] CreateVideo: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, video)
}

// GetVideo 获取视频详情
// GET /api/v1/videos/:id
func (h *VideoHandler) GetVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	respondOK(c, video)
}

// ListVideos 获取视频列表
// GET /api/v1/videos?novel_id=&chapter_id=&status=
// GET /api/v1/novels/:id/videos?chapter_id=&status=
func (h *VideoHandler) ListVideos(c *gin.Context) {
	// novel_id: 优先读路径参数（来自 /novels/:id/videos 路由），其次读查询参数
	var novelId *uint
	if pathId := c.Param("id"); pathId != "" && pathId != "/" {
		if id, err := strconv.ParseUint(pathId, 10, 32); err == nil {
			novelId = new(uint)
			*novelId = uint(id)
		}
	}
	if novelId == nil {
		if q := c.Query("novel_id"); q != "" {
			if id, err := strconv.ParseUint(q, 10, 32); err == nil {
				novelId = new(uint)
				*novelId = uint(id)
			}
		}
	}

	var chapterID *uint
	if q := c.Query("chapter_id"); q != "" {
		if id, err := strconv.ParseUint(q, 10, 32); err == nil {
			chapterID = new(uint)
			*chapterID = uint(id)
		}
	}

	status := c.Query("status")
	validStatuses := map[string]bool{"": true, "pending": true, "generating": true, "done": true, "failed": true}
	if !validStatuses[status] {
		respondBadRequest(c, "invalid status value")
		return
	}
	p := parsePagination(c)

	videos, total, err := h.videoService.ListVideos(novelId, chapterID, status, getTenantID(c), p.Page, p.PageSize)
	if err != nil {
		logger.Errorf("[VideoHandler] ListVideos: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"items":      videos,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (total + p.PageSize - 1) / p.PageSize,
	})
}

// UpdateVideo 更新视频
// PUT /api/v1/videos/:id
func (h *VideoHandler) UpdateVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if _, ok := h.getVideoForTenant(c, uint(id)); !ok {
		return
	}

	var req model.UpdateVideoRequest
	if !bindJSON(c, &req) {
		return
	}

	video, err := h.videoService.UpdateVideo(uint(id), getTenantID(c), &req)
	if err != nil {
		logger.Errorf("[VideoHandler] UpdateVideo: id=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, video)
}

// DeleteVideo 删除视频
// DELETE /api/v1/videos/:id
func (h *VideoHandler) DeleteVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if _, ok := h.getVideoForTenant(c, uint(id)); !ok {
		return
	}

	if err := h.videoService.DeleteVideo(uint(id), getTenantID(c)); err != nil {
		logger.Errorf("[VideoHandler] DeleteVideo: id=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// SynthesizeVideo 完整合成流水线（拼接→BGM→字幕→上传OSS）
// POST /api/v1/videos/:id/synthesize
func (h *VideoHandler) SynthesizeVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}
	taskID, err := h.videoService.SynthesizeVideo(c.Request.Context(), video.ID, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"code": 0, "data": gin.H{"task_id": taskID}})
}

// PublishVideo 站内发布（设置可见性）
// POST /api/v1/videos/:id/publish
func (h *VideoHandler) PublishVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}
	var req struct {
		Visibility string `json:"visibility"` // private|unlisted|public
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Visibility == "" {
		req.Visibility = "public"
	}
	switch req.Visibility {
	case "private", "unlisted", "public":
		// valid
	default:
		respondBadRequest(c, "visibility must be one of: private, unlisted, public")
		return
	}
	now := timeNow()
	video.IsPublished = true
	video.PublishedAt = &now
	video.Visibility = req.Visibility
	if err := h.videoService.UpdateVideoFields(video.ID, map[string]interface{}{
		"is_published": true,
		"published_at": &now,
		"visibility":   req.Visibility,
	}); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, video)
}

// UnpublishVideo 取消站内发布
// POST /api/v1/videos/:id/unpublish
func (h *VideoHandler) UnpublishVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	_, ok = h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}
	if err := h.videoService.UpdateVideoFields(uint(id), map[string]interface{}{
		"is_published": false,
		"visibility":   "private",
		"published_at": nil,
	}); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"unpublished": true})
}

// GetVideoProgress GET /api/v1/videos/:id/progress
// Returns generation progress (0-100) based on how many shots have an image URL.
func (h *VideoHandler) GetVideoProgress(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	video, ok := h.getVideoForTenant(c, uint(id))
	if !ok {
		return
	}

	shots, err := h.videoService.GetStoryboard(video.ID)
	if err != nil || len(shots) == 0 {
		respondOK(c, gin.H{"progress": 0, "status": video.Status})
		return
	}

	var done int
	for _, s := range shots {
		if s.ImageURL != "" {
			done++
		}
	}
	progress := done * 100 / len(shots)
	respondOK(c, gin.H{
		"progress":    progress,
		"done_shots":  done,
		"total_shots": len(shots),
		"status":      video.Status,
	})
}

// GenerateStoryboard 生成分镜（异步任务）
// POST /api/v1/videos/:id/storyboard/generate
// 立即返回 202 + task_id，轮询 GET /:id/storyboard/generate/:task_id 获取结果
