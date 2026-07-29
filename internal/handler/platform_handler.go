package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// PlatformHandler 视频广场、小说广场与外部平台发布处理器
type PlatformHandler struct {
	novelService   *service.NovelService
	videoService   *service.VideoService
	chapterService *service.ChapterService
	readingService *service.ReadingService
}

func NewPlatformHandler(novelSvc *service.NovelService, videoSvc *service.VideoService) *PlatformHandler {
	return &PlatformHandler{
		novelService: novelSvc,
		videoService: videoSvc,
	}
}

func (h *PlatformHandler) WithChapterService(svc *service.ChapterService) *PlatformHandler {
	h.chapterService = svc
	return h
}

func (h *PlatformHandler) WithReadingService(svc *service.ReadingService) *PlatformHandler {
	h.readingService = svc
	return h
}

// ─── 小说广场 ─────────────────────────────────────────────────────────────────

// GetPlatformNovels 小说广场（公开，无需 JWT）
// GET /api/v1/platform/novels?sort=hot|latest|words|favorites&q=关键词&channel=female|male|publish
//
//	&genre=romance&word_min=300000&word_max=1000000&updated_days=7&is_completed=1
//	&page=1&page_size=12
func (h *PlatformHandler) GetPlatformNovels(c *gin.Context) {
	p := parsePagination(c)
	wordMin, _ := strconv.Atoi(c.Query("word_min"))
	wordMax, _ := strconv.Atoi(c.Query("word_max"))
	updatedDays, _ := strconv.Atoi(c.Query("updated_days"))

	// Whitelist allowed sort values to prevent injection/unexpected behavior
	allowedSortValues := map[string]bool{
		"hot": true, "latest": true, "words": true, "favorites": true,
	}
	sortVal := c.DefaultQuery("sort", "hot")
	if !allowedSortValues[sortVal] {
		sortVal = "hot"
	}

	f := repository.NovelPublicFilter{
		Sort:        sortVal,
		Q:           c.Query("q"),
		Channel:     c.Query("channel"),
		Genre:       c.Query("genre"),
		WordMin:     wordMin,
		WordMax:     wordMax,
		UpdatedDays: updatedDays,
		IsCompleted: c.Query("is_completed"),
		Page:        p.Page,
		PageSize:    p.PageSize,
	}

	novels, total, err := h.novelService.ListPublicNovels(f)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list novels")
		return
	}
	respondOK(c, gin.H{
		"items":      novels,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (total + int64(p.PageSize) - 1) / int64(p.PageSize),
	})
}

// GetNovelRanking 小说排行榜（公开，无需 JWT）
// GET /api/v1/platform/novels/ranking?type=hot|new|completed|favorites|updated&gender=male|female
func (h *PlatformHandler) GetNovelRanking(c *gin.Context) {
	rankType := c.DefaultQuery("type", "hot")
	gender := c.Query("gender")
	novels, err := h.novelService.GetNovelRanking(rankType, gender)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to get ranking")
		return
	}
	respondOK(c, gin.H{"items": novels, "total": len(novels)})
}

// GetPlatformNovel 获取公开小说详情（可选 JWT：已登录则附带 is_liked）
// GET /api/v1/platform/novels/:id
func (h *PlatformHandler) GetPlatformNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	novel, err := h.novelService.GetPublicNovel(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "novel not found or not published")
		return
	}
	isLiked := false
	if uid := getUserID(c); uid > 0 {
		isLiked = h.novelService.IsNovelLiked(uint(id), uid)
	}
	respondOK(c, gin.H{
		"novel":    novel,
		"is_liked": isLiked,
	})
}

// RecordNovelView 记录小说浏览量（防刷）
// POST /api/v1/platform/novels/:id/view
func (h *PlatformHandler) RecordNovelView(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	_ = h.novelService.RecordNovelViewDeduped(uint(id), c.ClientIP())
	respondOK(c, gin.H{"recorded": true})
}

// ToggleNovelLike 点赞/取消（需 JWT）
// POST /api/v1/platform/novels/:id/like
func (h *PlatformHandler) ToggleNovelLike(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	liked, err := h.novelService.ToggleNovelLike(uint(id), uid)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"liked": liked})
}

// ListNovelComments 获取评论列表（公开）
// GET /api/v1/platform/novels/:id/comments?page=1&page_size=20
func (h *PlatformHandler) ListNovelComments(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	pg := parsePagination(c)
	comments, total, err := h.novelService.ListNovelComments(uint(id), pg.Page, pg.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list comments")
		return
	}
	respondOK(c, gin.H{
		"items":      comments,
		"total":      total,
		"page":       pg.Page,
		"page_size":  pg.PageSize,
		"total_page": (total + int64(pg.PageSize) - 1) / int64(pg.PageSize),
	})
}

// GetNovelChapters 获取已发布小说的章节列表（公开）
// GET /api/v1/platform/novels/:id/chapters
func (h *PlatformHandler) GetNovelChapters(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.novelService.GetPublicNovel(uint(id)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found or not published")
		return
	}
	if h.chapterService == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not available")
		return
	}
	chapters, err := h.chapterService.ListPublishedChapters(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list chapters")
		return
	}
	respondOK(c, gin.H{"items": chapters, "total": len(chapters)})
}

// GetPublishedChapter 获取已发布小说的单章内容（公开）
// GET /api/v1/platform/novels/:id/chapters/:chapter_no
func (h *PlatformHandler) GetPublishedChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNoStr := c.Param("chapter_no")
	chapterNo, err := strconv.Atoi(chapterNoStr)
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	if _, err := h.novelService.GetPublicNovel(uint(id)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found or not published")
		return
	}
	if h.chapterService == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not available")
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(id), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if !chapter.IsPublished {
		respondErr(c, http.StatusNotFound, "chapter not published")
		return
	}
	respondOK(c, chapter)
}

// AddNovelComment 发表评论（需 JWT）
// POST /api/v1/platform/novels/:id/comments
func (h *PlatformHandler) AddNovelComment(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	var req struct {
		Content  string `json:"content" binding:"required"`
		ParentID *uint  `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, "content is required")
		return
	}
	comment, err := h.novelService.AddNovelComment(uint(id), uid, req.Content, req.ParentID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": comment})
}

// DeleteNovelComment 删除评论（作者本人）
// DELETE /api/v1/platform/novels/:id/comments/:cid
func (h *PlatformHandler) DeleteNovelComment(c *gin.Context) {
	cid, ok := parseID(c, "cid")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if err := h.novelService.DeleteNovelComment(uint(cid), uid); err != nil {
		if errors.Is(err, service.ErrPermissionDenied) {
			respondErr(c, http.StatusForbidden, "permission denied")
		} else {
			respondErr(c, http.StatusInternalServerError, err.Error())
		}
		return
	}
	respondOK(c, gin.H{"deleted": true})
}

// GetPlatformFeed 视频广场（公开，无需 JWT）
// GET /api/v1/platform/videos?sort=latest|hot&q=关键词&page=1&page_size=12
func (h *PlatformHandler) GetPlatformFeed(c *gin.Context) {
	p := parsePagination(c)
	sort := c.DefaultQuery("sort", "hot")
	q := c.Query("q")

	videos, total, err := h.videoService.ListPublicVideos(sort, q, p.Page, p.PageSize)
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

// GetPlatformVideo 获取公开视频详情（可选 JWT：已登录则附带 is_liked）
// GET /api/v1/platform/videos/:id
func (h *PlatformHandler) GetPlatformVideo(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	video, err := h.videoService.GetPublicVideo(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "video not found or not published")
		return
	}

	// 附带当前用户是否已点赞（登录时有效）
	isLiked := false
	if uid := getUserID(c); uid > 0 {
		isLiked = h.videoService.IsVideoLiked(uint(id), uid)
	}

	respondOK(c, gin.H{
		"video":    video,
		"is_liked": isLiked,
	})
}

// RecordView 记录视频播放量（防刷，同 IP 1 小时内只计一次）
// POST /api/v1/platform/videos/:id/view
func (h *PlatformHandler) RecordView(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	clientIP := c.ClientIP()
	_ = h.videoService.RecordViewDeduped(uint(id), clientIP)
	respondOK(c, gin.H{"recorded": true})
}

// ToggleLike 点赞/取消（需 JWT）
// POST /api/v1/platform/videos/:id/like
func (h *PlatformHandler) ToggleLike(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	liked, err := h.videoService.ToggleVideoLike(uint(id), uid)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"liked": liked})
}

// ListComments 获取评论列表（公开）
// GET /api/v1/platform/videos/:id/comments?page=1&page_size=20
func (h *PlatformHandler) ListComments(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	pg := parsePagination(c)

	comments, total, err := h.videoService.ListVideoComments(uint(id), pg.Page, pg.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list comments")
		return
	}
	respondOK(c, gin.H{
		"items":      comments,
		"total":      total,
		"page":       pg.Page,
		"page_size":  pg.PageSize,
		"total_page": (total + int64(pg.PageSize) - 1) / int64(pg.PageSize),
	})
}

// AddComment 发表评论（需 JWT）
// POST /api/v1/platform/videos/:id/comments
func (h *PlatformHandler) AddComment(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}

	var req struct {
		Content  string `json:"content" binding:"required"`
		ParentID *uint  `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, "content is required")
		return
	}

	comment, err := h.videoService.AddVideoComment(uint(id), uid, req.Content, req.ParentID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": comment})
}

// DeleteComment 删除评论（作者本人）
// DELETE /api/v1/platform/videos/:id/comments/:cid
func (h *PlatformHandler) DeleteComment(c *gin.Context) {
	cid, ok := parseID(c, "cid")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if err := h.videoService.DeleteVideoComment(uint(cid), uid); err != nil {
		if errors.Is(err, service.ErrPermissionDenied) {
			respondErr(c, http.StatusForbidden, "permission denied")
		} else {
			respondErr(c, http.StatusInternalServerError, err.Error())
		}
		return
	}
	respondOK(c, gin.H{"deleted": true})
}

// ─── Chapter Social: Like / Comment ──────────────────────────────────────────

// ToggleChapterLike 章节点赞/取消（需 JWT）
// POST /api/v1/platform/novels/:id/chapters/:chapter_no/like
func (h *PlatformHandler) ToggleChapterLike(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if h.chapterService == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not available")
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil || !chapter.IsPublished {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	liked, likeCount, err := h.readingService.ToggleChapterLike(chapter.ID, uint(novelID), uid, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"liked": liked, "like_count": likeCount})
}

// ListChapterComments 获取章节评论列表（公开）
// GET /api/v1/platform/novels/:id/chapters/:chapter_no/comments
func (h *PlatformHandler) ListChapterComments(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	if h.chapterService == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not available")
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil || !chapter.IsPublished {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	pg := parsePagination(c)
	page, pageSize := pg.Page, pg.PageSize
	list, total, err := h.readingService.ListChapterComments(chapter.ID, page, pageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": list, "total": total, "page": page, "page_size": pageSize})
}

// AddChapterComment 发表章节评论（需 JWT）
// POST /api/v1/platform/novels/:id/chapters/:chapter_no/comments
func (h *PlatformHandler) AddChapterComment(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if h.chapterService == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not available")
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil || !chapter.IsPublished {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	var req struct {
		Content  string `json:"content" binding:"required"`
		ParentID *uint  `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "content is required")
		return
	}
	comment, err := h.readingService.AddChapterComment(chapter.ID, uint(novelID), uid, getTenantID(c), req.Content, req.ParentID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": comment})
}

// DeleteChapterComment 删除章节评论（作者本人）
// DELETE /api/v1/platform/novels/:id/chapters/:chapter_no/comments/:cid
func (h *PlatformHandler) DeleteChapterComment(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	cid, ok := parseID(c, "cid")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if err := h.readingService.DeleteChapterComment(uint(cid), uid); err != nil {
		if errors.Is(err, service.ErrChapterCommentPermission) {
			respondErr(c, http.StatusForbidden, "permission denied")
		} else {
			respondErr(c, http.StatusInternalServerError, err.Error())
		}
		return
	}
	respondOK(c, gin.H{"deleted": true})
}

// ─── Reading Progress ─────────────────────────────────────────────────────────

// SaveReadingProgress 保存阅读进度（需 JWT）
// PUT /api/v1/platform/novels/:id/progress
func (h *PlatformHandler) SaveReadingProgress(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	var req struct {
		ChapterNo int  `json:"chapter_no" binding:"required,min=1"`
		ChapterID uint `json:"chapter_id" binding:"required"`
		ScrollPct int  `json:"scroll_pct"` // 0-100
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if req.ScrollPct < 0 {
		req.ScrollPct = 0
	}
	if req.ScrollPct > 100 {
		req.ScrollPct = 100
	}
	if err := h.readingService.SaveProgress(uid, uint(novelID), req.ChapterNo, req.ChapterID, req.ScrollPct); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"saved": true})
}

// GetReadingProgress 获取阅读进度（需 JWT）
// GET /api/v1/platform/novels/:id/progress
func (h *PlatformHandler) GetReadingProgress(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	prog, err := h.readingService.GetProgress(uid, uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, prog) // nil → null in JSON
}

// GetReadChapters 获取用户在某小说中已读的章节 ID 列表（需 JWT）
// GET /api/v1/platform/novels/:id/read-chapters
func (h *PlatformHandler) GetReadChapters(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	ids, err := h.readingService.GetReadChapterIDs(uid, uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if ids == nil {
		ids = []uint{}
	}
	respondOK(c, gin.H{"chapter_ids": ids})
}

// GetReadingHistory 获取当前用户阅读历史（需 JWT）
// GET /api/v1/platform/me/reading-history
func (h *PlatformHandler) GetReadingHistory(c *gin.Context) {
	if h.readingService == nil {
		respondErr(c, http.StatusServiceUnavailable, "reading service not available")
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	pg := parsePagination(c)
	page, pageSize := pg.Page, pg.PageSize
	list, total, err := h.readingService.GetReadHistory(uid, page, pageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": list, "total": total, "page": page, "page_size": pageSize})
}

// MarkChapterRead 标记章节已读（需 JWT，打开章节时调用）
// POST /api/v1/platform/novels/:id/chapters/:chapter_no/read
func (h *PlatformHandler) MarkChapterRead(c *gin.Context) {
	if h.readingService == nil {
		respondOK(c, gin.H{"marked": false})
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondErr(c, http.StatusUnauthorized, "login required")
		return
	}
	if h.chapterService == nil {
		respondOK(c, gin.H{"marked": false})
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil || !chapter.IsPublished {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	_ = h.readingService.MarkChapterRead(uid, chapter.ID, uint(novelID))
	respondOK(c, gin.H{"marked": true})
}

// GetChapterIsLiked 获取当前用户是否已点赞某章节（需 JWT）
// GET /api/v1/platform/novels/:id/chapters/:chapter_no/like
func (h *PlatformHandler) GetChapterIsLiked(c *gin.Context) {
	if h.readingService == nil {
		respondOK(c, gin.H{"liked": false})
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil || chapterNo < 1 {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	uid := getUserID(c)
	if uid == 0 {
		respondOK(c, gin.H{"liked": false})
		return
	}
	if h.chapterService == nil {
		respondOK(c, gin.H{"liked": false})
		return
	}
	chapter, err := h.chapterService.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondOK(c, gin.H{"liked": false})
		return
	}
	liked := h.readingService.IsChapterLiked(chapter.ID, uid)
	respondOK(c, gin.H{"liked": liked})
}
