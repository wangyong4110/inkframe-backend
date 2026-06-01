package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// NovelHandler 小说处理器
type NovelHandler struct {
	novelService          *service.NovelService
	chapterService        *service.ChapterService
	foreshadowService     *service.ForeshadowService
	timelineService       *service.TimelineService
	qualityControlService *service.QualityControlService
	taskSvc               *service.TaskService
	modelService          *service.ModelService
	notifSvc              *service.NotificationService // 可选
	analysisSvc           *service.NovelAnalysisService // 可选
}

func NewNovelHandler(
	novelService *service.NovelService,
	chapterService *service.ChapterService,
	foreshadowService *service.ForeshadowService,
	timelineService *service.TimelineService,
	qualityControlService *service.QualityControlService,
) *NovelHandler {
	return &NovelHandler{
		novelService:          novelService,
		chapterService:        chapterService,
		foreshadowService:     foreshadowService,
		timelineService:       timelineService,
		qualityControlService: qualityControlService,
	}
}

func (h *NovelHandler) WithTaskService(svc *service.TaskService) *NovelHandler {
	h.taskSvc = svc
	return h
}

func (h *NovelHandler) WithModelService(svc *service.ModelService) *NovelHandler {
	h.modelService = svc
	return h
}

// WithNotificationService 注入通知服务（可选）
func (h *NovelHandler) WithNotificationService(svc *service.NotificationService) *NovelHandler {
	h.notifSvc = svc
	return h
}

// WithAnalysisService 注入小说分析服务（可选，用于分析状态查询）
func (h *NovelHandler) WithAnalysisService(svc *service.NovelAnalysisService) *NovelHandler {
	h.analysisSvc = svc
	return h
}

// CreateNovel 创建小说
// POST /api/v1/novels
func (h *NovelHandler) CreateNovel(c *gin.Context) {
	tenantID := getTenantID(c)

	// 前置检查：要求至少配置一个有效的 LLM 提供商
	if h.modelService != nil {
		capable, err := h.modelService.ListCapableProviders(tenantID, "llm")
		if err == nil && len(capable) == 0 {
			respondErr(c, http.StatusUnprocessableEntity,
				"请先前往「模型管理」页面为至少一个文本生成（LLM）提供商配置 API Key，再创建小说项目")
			return
		}
	}

	var req model.CreateNovelRequest
	if !bindJSON(c, &req) {
		return
	}
	req.TenantID = tenantID

	novel, err := h.novelService.CreateNovel(&req)
	if err != nil {
		logger.Printf("[NovelHandler] CreateNovel: tenantID=%d err=%v", tenantID, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, novel)
}

// GetNovel 获取小说详情
// GET /api/v1/novels/:id
func (h *NovelHandler) GetNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	novel, err := h.novelService.GetNovel(uint(id), getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	respondOK(c, novel)
}

// ListNovels 获取小说列表
// GET /api/v1/novels?sort=updated_at|created_at|title|status&order=asc|desc
func (h *NovelHandler) ListNovels(c *gin.Context) {
	p := parsePagination(c)

	filters := map[string]interface{}{}
	if tenantID, ok := c.Get("tenant_id"); ok {
		filters["tenant_id"] = tenantID
	}
	if status := c.Query("status"); status != "" {
		filters["status"] = status
	}
	if genre := c.Query("genre"); genre != "" {
		filters["genre"] = genre
	}

	// Sort params with whitelist validation (also validated in repository)
	allowedSortFields := map[string]bool{
		"created_at": true, "updated_at": true, "title": true, "status": true,
	}
	sortField := c.DefaultQuery("sort", "updated_at")
	if !allowedSortFields[sortField] {
		sortField = "updated_at"
	}
	filters["sort"] = sortField

	sortOrder := c.DefaultQuery("order", "desc")
	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "desc"
	}
	filters["order"] = sortOrder

	novels, total, err := h.novelService.ListNovelsFiltered(p.Page, p.PageSize, filters)
	if err != nil {
		logger.Printf("[NovelHandler] ListNovels: err=%v", err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{
		"items":      novels,
		"total":      total,
		"page":       p.Page,
		"page_size":  p.PageSize,
		"total_page": (int(total) + p.PageSize - 1) / p.PageSize,
	})
}

// UpdateNovel 更新小说
// PUT /api/v1/novels/:id
func (h *NovelHandler) UpdateNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.UpdateNovelRequest
	if !bindJSON(c, &req) {
		return
	}

	novel, err := h.novelService.UpdateNovel(uint(id), getTenantID(c), &req)
	if err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
		logger.Printf("[NovelHandler] UpdateNovel: novelID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, novel)
}

// DeleteNovel 删除小说
// DELETE /api/v1/novels/:id
func (h *NovelHandler) DeleteNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if err := h.novelService.DeleteNovel(uint(id), getTenantID(c)); err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
		logger.Printf("[NovelHandler] DeleteNovel: novelID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateChapter 生成章节（异步任务）
// POST /api/v1/novels/:id/chapters/generate
// 立即返回 202 + task_id，轮询 GET /:id/chapters/generate/:task_id 获取结果
func (h *NovelHandler) GenerateChapter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.GenerateChapterRequest
	if !bindJSON(c, &req) {
		return
	}
	// NovelID 从 URL 路径参数注入（body 中可不传）
	req.NovelID = uint(novelId)

	// 支持通过 Header 临时覆盖 AI 模型/provider
	if override := c.GetHeader("X-Model-Override"); override != "" && req.ModelOverride == "" {
		req.ModelOverride = override
	}

	tenantID := getTenantID(c)
	// 验证小说归属当前租户
	if _, err := h.novelService.GetNovel(uint(novelId), tenantID); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}
	userID, _ := c.Get("user_id")
	callerUserID, _ := userID.(uint)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterGen, "章节生成", "chapter", 0)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"novel_id": uint(novelId),
		"req":      &req,
	})

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[NovelHandler] GenerateChapter task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)        //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5) //nolint:errcheck

		chapter, err := h.chapterService.GenerateChapter(tenantID, uint(novelId), &req)
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			logger.Printf("[NovelHandler] GenerateChapter task %s failed: %v", taskID, err)
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90) //nolint:errcheck

		modelUsed := req.ModelOverride
		if modelUsed == "" {
			modelUsed = h.novelService.GetAIService().GetDefaultProviderName()
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"chapter": chapter, "model_used": modelUsed}) //nolint:errcheck

		// 站内通知：章节生成完成
		if h.notifSvc != nil && callerUserID > 0 {
			_ = h.notifSvc.Send(
				tenantID, callerUserID,
				"chapter_done",
				fmt.Sprintf("第%d章生成完成", chapter.ChapterNo),
				chapter.Title,
				"chapter", chapter.ID,
				fmt.Sprintf("/novel/%d/chapter/%d", chapter.NovelID, chapter.ChapterNo),
			)
		}

		// 后处理：伏笔提取 + 质量检查（非阻塞）
		go func(ch *model.Chapter, tid uint) {
			if _, err := h.foreshadowService.ExtractForeshadows(ch, tid, ch.NovelID); err != nil {
				logger.Printf("[NovelHandler] GenerateChapter: foreshadow extraction failed (ch %d): %v", ch.ID, err)
			}
		}(chapter, tenantID)
		go func(chID uint) {
			if _, err := h.qualityControlService.CheckChapter(chID); err != nil {
				logger.Printf("[NovelHandler] GenerateChapter: quality check failed (ch %d): %v", chID, err)
			}
		}(chapter.ID)
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "章节生成任务已提交",
		"data":    gin.H{"task_id": task.TaskID},
	})
}

// BatchGenerateChapters 批量生成小说所有章节正文（顺序执行，保证叙事连贯性）
// POST /api/v1/novels/:id/chapters/batch-generate
// Body: {"skip_existing":true,"word_count":0,"start_chapter_no":0,"end_chapter_no":0,"model":""}
// Returns HTTP 202 with {task_id}
func (h *NovelHandler) BatchGenerateChapters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.BatchGenerateChaptersRequest
	req.SkipExisting = true // default: skip chapters that already have content
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	tenantID := getTenantID(c)
	if _, err := h.novelService.GetNovel(uint(novelId), tenantID); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	chapters, err := h.chapterService.ListChapters(uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "list chapters failed: "+err.Error())
		return
	}

	// Filter chapters to process
	var toGenerate []*model.Chapter
	for _, ch := range chapters {
		if req.StartChapterNo > 0 && ch.ChapterNo < req.StartChapterNo {
			continue
		}
		if req.EndChapterNo > 0 && ch.ChapterNo > req.EndChapterNo {
			continue
		}
		if req.SkipExisting && strings.TrimSpace(ch.Content) != "" {
			continue
		}
		toGenerate = append(toGenerate, ch)
	}

	if len(toGenerate) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"code":    0,
			"message": "所有章节已有内容，无需生成",
			"data":    gin.H{"total": 0, "generated": 0},
		})
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeBatchChapterGen,
		fmt.Sprintf("批量生成章节内容（共%d章）", len(toGenerate)), "novel", uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[BatchGenerate] task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		total := len(toGenerate)
		var generated, failed int
		var failedChapters []int
		for i, ch := range toGenerate {
			progress := (i*90)/total + 5
			h.taskSvc.UpdateProgressAndTitle(taskID, progress, //nolint:errcheck
				fmt.Sprintf("正在生成第%d章《%s》（%d/%d）", ch.ChapterNo, ch.Title, i+1, total))
			genReq := &model.GenerateChapterRequest{
				NovelID:       uint(novelId),
				ChapterNo:     ch.ChapterNo,
				WordCount:     req.WordCount,
				MaxTokens:     req.MaxTokens,
				ModelOverride: req.ModelOverride,
			}
			if _, genErr := h.chapterService.GenerateChapter(tenantID, uint(novelId), genReq); genErr != nil {
				logger.Printf("[BatchGenerate] chapter %d failed: %v", ch.ChapterNo, genErr)
				failed++
				failedChapters = append(failedChapters, ch.ChapterNo)
			} else {
				generated++
			}
		}
		resultTitle := fmt.Sprintf("批量生成完成：成功%d章", generated)
		if failed > 0 {
			resultTitle += fmt.Sprintf("，失败%d章", failed)
		}
		h.taskSvc.UpdateProgressAndTitle(taskID, 99, resultTitle) //nolint:errcheck
		h.taskSvc.Complete(taskID, map[string]interface{}{        //nolint:errcheck
			"total":           total,
			"generated":       generated,
			"failed":          failed,
			"failed_chapters": failedChapters,
		})
	}(task.TaskID)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": fmt.Sprintf("批量生成任务已提交，共%d章", len(toGenerate)),
		"data":    gin.H{"task_id": task.TaskID, "total": len(toGenerate)},
	})
}

// GenerateOutline 生成大纲
// POST /api/v1/novels/:id/outline
func (h *NovelHandler) GenerateOutline(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		ChapterNum     int      `json:"chapter_num"`
		Prompt         string   `json:"prompt"`
		Keywords       []string `json:"keywords"`
		MaxTokens      int      `json:"max_tokens,omitempty"`
		Temperature    float64  `json:"temperature,omitempty"`
		TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request body: "+err.Error())
		return
	}

	result, err := h.novelService.GenerateOutline(getTenantID(c), &service.GenerateOutlineRequest{
		NovelID:        uint(novelId),
		ChapterNum:     req.ChapterNum,
		Prompt:         req.Prompt,
		Keywords:       req.Keywords,
		MaxTokens:      req.MaxTokens,
		Temperature:    req.Temperature,
		TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		logger.Printf("[NovelHandler] GenerateOutline: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// ListOutlineVersions 获取小说大纲历史版本列表
// GET /api/v1/novels/:id/outline-versions
func (h *NovelHandler) ListOutlineVersions(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}
	versions, err := h.novelService.ListOutlineVersions(uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, versions)
}

// GetForeshadows 获取伏笔列表
// GET /api/v1/novels/:id/foreshadows
func (h *NovelHandler) GetForeshadows(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	chapterNo, _ := strconv.Atoi(c.Query("chapter_no"))

	foreshadows, err := h.foreshadowService.GetForeshadows(uint(novelId), chapterNo)
	if err != nil {
		logger.Printf("[NovelHandler] GetForeshadows: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, foreshadows)
}

// MarkForeshadowFulfilled 标记伏笔已回收
// POST /api/v1/novels/:id/foreshadows/:foreshadow_id/fulfill
func (h *NovelHandler) MarkForeshadowFulfilled(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	foreshadowId, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}

	var req struct {
		ChapterID uint `json:"chapter_id"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if err := h.foreshadowService.MarkFulfilledByID(uint(novelId), uint(foreshadowId), req.ChapterID); err != nil {
		logger.Printf("[NovelHandler] MarkForeshadowFulfilled: novelID=%d foreshadowID=%d err=%v", novelId, foreshadowId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GetTimeline 获取时间线
// GET /api/v1/novels/:id/timeline
func (h *NovelHandler) GetTimeline(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	timeline, err := h.timelineService.GetTimeline(uint(novelId))
	if err != nil {
		logger.Printf("[NovelHandler] GetTimeline: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, timeline)
}

// BuildTimeline 构建时间线
// POST /api/v1/novels/:id/timeline/build
func (h *NovelHandler) BuildTimeline(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	timeline, err := h.timelineService.BuildTimeline(uint(novelId))
	if err != nil {
		logger.Printf("[NovelHandler] BuildTimeline: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, timeline)
}

// PublishNovel 发布小说到广场
// POST /api/v1/novels/:id/publish
func (h *NovelHandler) PublishNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Visibility string `json:"visibility"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Visibility == "" {
		req.Visibility = "public"
	}
	updated, err := h.novelService.PublishNovel(uint(id), getTenantID(c), req.Visibility)
	if err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, updated)
}

// ReviewNovel 审核小说（管理员操作）
// PUT /api/v1/novels/:id/review
func (h *NovelHandler) ReviewNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	role, _ := c.Get("user_role")
	roleStr, _ := role.(string)
	if roleStr != "admin" && roleStr != "owner" {
		respondErr(c, http.StatusForbidden, "admin access required")
		return
	}
	var req service.ReviewNovelRequest
	if !bindJSON(c, &req) {
		return
	}
	reviewerID, _ := c.Get("user_id")
	uid, _ := reviewerID.(uint)
	novel, err := h.novelService.ReviewNovel(uint(id), uid, getTenantID(c), req)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, novel)
}

// UnpublishNovel 取消发布小说
// POST /api/v1/novels/:id/unpublish
func (h *NovelHandler) UnpublishNovel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.novelService.UnpublishNovel(uint(id), getTenantID(c)); err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"unpublished": true})
}

// GenerateCoverImage 使用 AI 为小说生成封面（异步不适用，直接同步返回 URL）
// POST /api/v1/novels/:id/cover/generate
func (h *NovelHandler) GenerateCoverImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.novelService.GetNovel(uint(id), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}
	var req struct {
		Suggestion string `json:"suggestion"`
	}
	_ = c.ShouldBindJSON(&req) // 可选 body，忽略解析错误
	// 封面生成是长耗时操作（30-120s），使用独立 context 避免受 HTTP WriteTimeout 影响
	genCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	url, err := h.novelService.GenerateCoverImage(genCtx, getTenantID(c), uint(id), req.Suggestion)
	if err != nil {
		logger.Printf("[NovelHandler] GenerateCoverImage: novelID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"url": url})
}

// SyncCharacterSnapshots 同步章节角色状态快照
// POST /api/v1/novels/:id/chapters/:chapter_no/character-snapshots
func (h *NovelHandler) SyncCharacterSnapshots(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}

	var req struct {
		CharacterIDs  []uint `json:"character_ids"`
		ReusePrevious bool   `json:"reuse_previous"`
	}
	if !bindJSON(c, &req) {
		return
	}

	chapter, err := h.chapterService.GetChapterByNo(uint(novelId), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	if err := h.novelService.SyncCharacterSnapshots(
		getTenantID(c), chapter, req.CharacterIDs, req.ReusePrevious,
	); err != nil {
		logger.Printf("[NovelHandler] SyncCharacterSnapshots: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{"message": "character snapshots synced"})
}

// GetAnalysisStatus 查询小说分析进度
// GET /api/v1/novels/:id/analysis/status
func (h *NovelHandler) GetAnalysisStatus(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	// Verify novel belongs to tenant
	if _, err := h.novelService.GetNovel(uint(id), tenantID); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}
	if h.analysisSvc == nil {
		respondOK(c, gin.H{"novel_id": id, "status": "not_started"})
		return
	}
	status, err := h.analysisSvc.GetAnalysisStatus(uint(id))
	if err != nil {
		logger.Printf("[NovelHandler] GetAnalysisStatus: novelID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, status)
}

// ExportNovel 导出小说全文（TXT 格式）
// GET /api/v1/novels/:id/export?format=txt
func (h *NovelHandler) ExportNovel(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	format := c.DefaultQuery("format", "txt")
	if format != "txt" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only format=txt supported currently"})
		return
	}

	novel, err := h.novelService.GetNovel(uint(novelID), tenantID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return
	}

	chapters, err := h.chapterService.ListChapters(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to fetch chapters")
		return
	}

	var buf strings.Builder
	buf.WriteString(novel.Title + "\n")
	buf.WriteString(strings.Repeat("=", len([]rune(novel.Title))) + "\n\n")
	if novel.Description != "" {
		buf.WriteString(novel.Description + "\n\n")
	}

	for _, ch := range chapters {
		title := ch.Title
		if title == "" {
			title = fmt.Sprintf("第%d章", ch.ChapterNo)
		}
		buf.WriteString(title + "\n")
		buf.WriteString(strings.Repeat("-", len([]rune(title))) + "\n\n")
		buf.WriteString(ch.Content + "\n\n")
	}

	filename := fmt.Sprintf("%s.txt", novel.Title)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(buf.String()))
}
