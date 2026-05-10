package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// PlatformHandler 视频广场与外部平台发布处理器
type PlatformHandler struct {
	videoService    *service.VideoService
	publishService  *service.PlatformPublishService
}

func NewPlatformHandler(videoSvc *service.VideoService, publishSvc *service.PlatformPublishService) *PlatformHandler {
	return &PlatformHandler{
		videoService:   videoSvc,
		publishService: publishSvc,
	}
}

// GetPlatformFeed 视频广场（公开，无需 JWT）
// GET /api/v1/platform/videos
func (h *PlatformHandler) GetPlatformFeed(c *gin.Context) {
	p := parsePagination(c)
	videos, total, err := h.videoService.ListPublicVideos(p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list videos")
		return
	}
	respondOK(c, gin.H{
		"items":      videos,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (total + int64(p.PageSize) - 1) / int64(p.PageSize),
	})
}

// GetPlatformVideo 获取公开视频详情（JWT 保护）
// GET /api/v1/platform/videos/:id
func (h *PlatformHandler) GetPlatformVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	video, err := h.videoService.GetVideoByTenant(uint(id), 0) // 0 = 任意租户（公开视频）
	if err != nil || !video.IsPublished {
		respondErr(c, http.StatusNotFound, "video not found or not published")
		return
	}
	respondOK(c, video)
}

// RecordView 记录视频播放量（公开）
// POST /api/v1/platform/videos/:id/view
func (h *PlatformHandler) RecordView(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	_ = h.videoService.IncrVideoViewCount(uint(id))
	respondOK(c, gin.H{"recorded": true})
}

// ListAccounts 列出平台账号（JWT 保护）
// GET /api/v1/platform/accounts
func (h *PlatformHandler) ListAccounts(c *gin.Context) {
	accounts, err := h.publishService.ListAccounts(getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list accounts")
		return
	}
	respondOK(c, accounts)
}

// ConnectAccount OAuth 跳转（JWT 保护）
// GET /api/v1/platform/accounts/oauth/:platform → 302
func (h *PlatformHandler) ConnectAccount(c *gin.Context) {
	platform := c.Param("platform")
	redirectURI := c.Query("redirect_uri")
	state := c.Query("state")
	if redirectURI == "" {
		respondErr(c, http.StatusBadRequest, "redirect_uri is required")
		return
	}
	url, err := h.publishService.GetAuthURL(platform, redirectURI, state)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	if url == "" {
		respondErr(c, http.StatusNotImplemented, "platform not configured")
		return
	}
	c.Redirect(http.StatusFound, url)
}

// OAuthCallback OAuth 回调（JWT 保护）
// GET /api/v1/platform/accounts/callback/:platform
func (h *PlatformHandler) OAuthCallback(c *gin.Context) {
	platform := c.Param("platform")
	code := c.Query("code")
	redirectURI := c.Query("redirect_uri")
	if code == "" {
		respondErr(c, http.StatusBadRequest, "code is required")
		return
	}
	account, err := h.publishService.ConnectAccount(c.Request.Context(), platform, code, redirectURI, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, account)
}

// DisconnectAccount 解绑平台账号（JWT 保护）
// DELETE /api/v1/platform/accounts/:id
func (h *PlatformHandler) DisconnectAccount(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.publishService.DisconnectAccount(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"disconnected": true})
}

// PublishToExternal 向外部平台发布视频（JWT 保护）
// POST /api/v1/videos/:id/publish-external
func (h *PlatformHandler) PublishToExternal(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	video, err := h.videoService.GetVideoByTenant(uint(id), tenantID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found")
		return
	}

	var req struct {
		AccountIDs  []uint   `json:"account_ids"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		IsPublic    bool     `json:"is_public"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.AccountIDs) == 0 {
		respondErr(c, http.StatusBadRequest, "account_ids is required")
		return
	}

	opts := service.PublishOptions{
		Title:       req.Title,
		Description: req.Description,
		Tags:        req.Tags,
		IsPublic:    req.IsPublic,
	}
	taskID, err := h.publishService.PublishToExternal(c.Request.Context(), video, req.AccountIDs, opts, tenantID)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"code": 0, "data": gin.H{"task_id": taskID}})
}

// ListPublishRecords 列出视频发布记录（JWT 保护）
// GET /api/v1/videos/:id/publish-records
func (h *PlatformHandler) ListPublishRecords(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	records, err := h.publishService.ListPublishRecords(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list records")
		return
	}
	respondOK(c, records)
}
