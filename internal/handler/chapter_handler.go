package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ChapterHandler 章节处理器
type ChapterHandler struct {
	chapterService    *service.ChapterService
	versionService    *service.ChapterVersionService
	qualityService    *service.QualityControlService
	taskSvc           *service.TaskService
	novelService      *service.NovelService
	continuityService *service.ContinuityService
	auditSvc          *service.AuditService
}

func (h *ChapterHandler) WithAuditService(svc *service.AuditService) *ChapterHandler {
	h.auditSvc = svc
	return h
}

func NewChapterHandler(
	chapterService *service.ChapterService,
	versionService *service.ChapterVersionService,
	qualityService *service.QualityControlService,
	taskSvc *service.TaskService,
) *ChapterHandler {
	return &ChapterHandler{
		chapterService: chapterService,
		versionService: versionService,
		qualityService: qualityService,
		taskSvc:        taskSvc,
	}
}

// WithContinuityService injects the ContinuityService for report listing.
func (h *ChapterHandler) WithContinuityService(cs *service.ContinuityService) *ChapterHandler {
	h.continuityService = cs
	return h
}

// WithNovelService injects the NovelService for novel ownership checks.
func (h *ChapterHandler) WithNovelService(ns *service.NovelService) *ChapterHandler {
	h.novelService = ns
	return h
}

// checkNovelOwnership verifies the novel exists and belongs to the caller's tenant.
// Returns true on success; writes an error response and returns false on failure.
func (h *ChapterHandler) checkNovelOwnership(c *gin.Context, novelId uint) bool {
	if h.novelService == nil {
		return true // service not wired — skip check (backward-compat)
	}
	if _, err := h.novelService.GetNovel(novelId, getTenantID(c), getUserID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return false
	}
	return true
}

// CreateChapter 创建章节
// POST /api/v1/novels/:novel_id/chapters
func (h *ChapterHandler) CreateChapter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}

	var req model.CreateChapterRequest
	if !bindJSON(c, &req) {
		return
	}

	if len([]rune(req.Title)) > 200 {
		respondBadRequest(c, "chapter title exceeds 200 characters")
		return
	}
	if len([]rune(req.Content)) > 100000 {
		respondBadRequest(c, "chapter content exceeds 100,000 characters")
		return
	}

	chapter, err := h.chapterService.CreateChapter(uint(novelId), &req)
	if err != nil {
		logger.Errorf("[ChapterHandler] CreateChapter: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c), NovelID: uint(novelId),
			Action: "chapter.create", ResourceType: "chapter",
			ResourceID: chapter.ID, ResourceName: chapter.Title, IP: c.ClientIP(),
		})
	}
	respondCreated(c, chapter)
}

// ReorderChapters 批量调整章节顺序
// PUT /api/v1/novels/:id/chapters/reorder
func (h *ChapterHandler) ReorderChapters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}
	var req struct {
		Orders []struct {
			ChapterID int `json:"chapter_id"`
			ChapterNo int `json:"chapter_no"`
		} `json:"orders" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	orders := make([]repository.ChapterOrder, 0, len(req.Orders))
	for _, o := range req.Orders {
		orders = append(orders, repository.ChapterOrder{ID: uint(o.ChapterID), ChapterNo: o.ChapterNo})
	}
	if err := h.chapterService.ReorderChapters(uint(novelId), orders); err != nil {
		logger.Errorf("[ChapterHandler] ReorderChapters: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, nil)
}

// InsertChapter 在指定章节后插入新章节
// POST /api/v1/novels/:id/chapters/insert
func (h *ChapterHandler) InsertChapter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}
	var req struct {
		AfterChapterNo int `json:"after_chapter_no"`
	}
	if !bindJSON(c, &req) {
		return
	}
	chapter, err := h.chapterService.InsertChapterAfter(uint(novelId), req.AfterChapterNo)
	if err != nil {
		logger.Errorf("[ChapterHandler] InsertChapter: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, chapter)
}

// GetChapter 获取章节详情
// GET /api/v1/chapters/:id
func (h *ChapterHandler) GetChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	chapter, err := h.chapterService.GetChapter(uint(id), getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	respondOK(c, chapter)
}

// ListChapters 获取章节列表
// GET /api/v1/novels/:novel_id/chapters
// 支持可选分页：?page=1&page_size=20。无分页参数时返回全量列表（向后兼容）。
func (h *ChapterHandler) ListChapters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	// If pagination params are provided, use paginated query.
	if c.Query("page") != "" || c.Query("page_size") != "" {
		p := parsePagination(c)
		chapters, total, err := h.chapterService.ListChaptersPaged(uint(novelId), p.Page, p.PageSize)
		if err != nil {
			logger.Errorf("[ChapterHandler] ListChapters: novelID=%d err=%v", novelId, err)
			respondErr(c, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(c, gin.H{
			"items":      chapters,
			"total":      total,
			"page":       p.Page,
			"page_size":  p.PageSize,
			"total_page": (total + int64(p.PageSize) - 1) / int64(p.PageSize),
		})
		return
	}

	chapters, err := h.chapterService.ListChapters(uint(novelId))
	if err != nil {
		logger.Errorf("[ChapterHandler] ListChapters: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapters)
}

// UpdateChapter 更新章节
// PUT /api/v1/chapters/:id
func (h *ChapterHandler) UpdateChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.UpdateChapterRequest
	if !bindJSON(c, &req) {
		return
	}

	if req.Title != "" && len([]rune(req.Title)) > 200 {
		respondBadRequest(c, "chapter title exceeds 200 characters")
		return
	}
	if req.Content != "" && len([]rune(req.Content)) > 100000 {
		respondBadRequest(c, "chapter content exceeds 100,000 characters")
		return
	}

	chapter, err := h.chapterService.UpdateChapter(uint(id), getTenantID(c), &req)
	if err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "chapter not found")
			return
		}
		logger.Errorf("[ChapterHandler] UpdateChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapter)
}

// DeleteChapter 删除章节
// DELETE /api/v1/chapters/:id
func (h *ChapterHandler) DeleteChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if err := h.chapterService.DeleteChapter(uint(id), getTenantID(c)); err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "chapter not found")
			return
		}
		logger.Errorf("[ChapterHandler] DeleteChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c),
			Action: "chapter.delete", ResourceType: "chapter", ResourceID: uint(id),
			IP: c.ClientIP(),
		})
	}
	respondOK(c, nil)
}

// GenerateChapter 生成章节内容（异步任务）
// POST /api/v1/chapters/generate
// Returns HTTP 202 with {task_id, status:"pending"} immediately.
// Poll GET /api/v1/tasks/:id to track progress; on completion result.data.chapter contains the chapter.
func (h *ChapterHandler) GenerateChapter(c *gin.Context) {
	var req model.GenerateChapterRequest
	if !bindJSON(c, &req) {
		return
	}
	logger.Printf("[ChapterHandler] GenerateChapter: novelID=%d chapterNo=%d", req.NovelID, req.ChapterNo)

	if !requireNovelEditorRole(c, h.novelService, req.NovelID) {
		return
	}

	// 支持通过 Header 临时覆盖 AI 模型/provider
	if override := c.GetHeader("X-Model-Override"); override != "" && req.ModelOverride == "" {
		req.ModelOverride = override
	}

	tenantID := getTenantID(c)

	// Cancel any active chapter_gen task for this novel to avoid duplicate runs.
	h.taskSvc.CancelActiveByEntity("novel", req.NovelID, service.TaskTypeChapterGen)

	// Create an async task and return immediately (HTTP 202).
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterGen, "章节生成", "novel", req.NovelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	// Persist request parameters so the task can be resumed after a server restart.
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"novel_id": req.NovelID,
		"req":      req,
	})

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] GenerateChapter task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)        //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5) //nolint:errcheck

		chapter, genErr := h.chapterService.GenerateChapter(tenantID, req.NovelID, &req)
		if genErr != nil {
			logger.Errorf("[ChapterHandler] GenerateChapter task %s failed: novelID=%d err=%v", taskID, req.NovelID, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90)                                                      //nolint:errcheck
		h.taskSvc.Complete(taskID, map[string]interface{}{"chapter": chapter}) //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "章节生成任务已提交")
}

// RegenerateChapter 重新生成章节内容（异步）
// POST /api/v1/chapters/:id/regenerate
// Body: {"prompt":"...","word_count":0,"max_tokens":0,"model":"","temperature":0,"timeout_seconds":0,"web_search":false,"wiki_search":false,"use_story_pattern":false}
// Returns HTTP 202 with {task_id, status:"pending"}
func (h *ChapterHandler) RegenerateChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// Optional overrides — all fields are optional
	var req model.GenerateChapterRequest
	_ = c.ShouldBindJSON(&req) // ignore parse errors — all fields optional

	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterGen, "章节重新生成", "chapter", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] RegenerateChapter task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)        //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 5) //nolint:errcheck

		chapter, genErr := h.chapterService.RegenerateChapter(tenantID, uint(id), &req)
		if genErr != nil {
			logger.Errorf("[ChapterHandler] RegenerateChapter task %s failed: chapterID=%d err=%v", taskID, id, genErr)
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90)                                                      //nolint:errcheck
		h.taskSvc.Complete(taskID, map[string]interface{}{"chapter": chapter}) //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "章节重新生成任务已提交")
}

// GetVersions 获取章节版本历史
// GET /api/v1/chapters/:id/versions
func (h *ChapterHandler) GetVersions(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// Fix 5: 验证章节归属当前租户，防止跨租户信息泄露
	if _, err := h.chapterService.GetChapter(uint(id), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	versions, err := h.versionService.GetVersions(uint(id))
	if err != nil {
		logger.Errorf("[ChapterHandler] GetVersions: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{"versions": versions})
}

// RestoreVersion 恢复版本
// POST /api/v1/chapters/:id/versions/:version_no/restore
func (h *ChapterHandler) RestoreVersion(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	versionNo, err := strconv.Atoi(c.Param("version_no"))
	if err != nil {
		respondBadRequest(c, "invalid version no")
		return
	}

	chapter, err := h.versionService.RestoreVersion(uint(id), versionNo)
	if err != nil {
		logger.Errorf("[ChapterHandler] RestoreVersion: chapterID=%d versionNo=%d err=%v", id, versionNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapter)
}

// GetVersionContent 获取指定章节版本的完整内容（供客户端 diff）
// GET /api/v1/chapters/:id/versions/:version_id/content
func (h *ChapterHandler) GetVersionContent(c *gin.Context) {
	chapterID, ok := parseID(c, "id")
	if !ok {
		return
	}
	versionID, ok := parseID(c, "version_id")
	if !ok {
		return
	}

	// Verify chapter belongs to tenant.
	if _, err := h.chapterService.GetChapter(uint(chapterID), getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	version, err := h.versionService.GetChapterVersion(uint(chapterID), uint(versionID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "version not found")
		return
	}
	respondOK(c, version)
}

// GetChapterByNo 根据章节号获取章节
// GET /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) GetChapterByNo(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	chapter, err := h.chapterService.GetChapterByNo(uint(novelId), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	respondOK(c, chapter)
}

// UpdateChapterByNo 根据章节号更新章节
// PUT /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) UpdateChapterByNo(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}

	var req model.UpdateChapterRequest
	if !bindJSON(c, &req) {
		return
	}

	chapter, err := h.chapterService.UpdateChapterByNo(uint(novelId), chapterNo, &req)
	if err != nil {
		logger.Errorf("[ChapterHandler] UpdateChapterByNo: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapter)
}

// DeleteChapterByNo 根据章节号删除章节
// DELETE /api/v1/novels/:novel_id/chapters/:chapter_no
func (h *ChapterHandler) DeleteChapterByNo(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}

	if err := h.chapterService.DeleteChapterByNo(uint(novelId), chapterNo); err != nil {
		logger.Errorf("[ChapterHandler] DeleteChapterByNo: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, nil)
}

// PublishChapter 发布章节到广场
// POST /api/v1/novels/:id/chapters/:chapter_no/publish
func (h *ChapterHandler) PublishChapter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}

	// Fetch the chapter to check continuity_blocked before publishing.
	existing, err := h.chapterService.GetChapterByNo(uint(novelId), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if existing.ContinuityBlocked {
		c.JSON(http.StatusConflict, gin.H{
			"error": "chapter has unresolved continuity issues and cannot be published",
			"code":  "continuity_blocked",
		})
		return
	}

	chapter, err := h.chapterService.PublishChapter(uint(novelId), chapterNo)
	if err != nil {
		logger.Errorf("[ChapterHandler] PublishChapter: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c), NovelID: uint(novelId),
			Action: "chapter.publish", ResourceType: "chapter",
			ResourceID: chapter.ID, ResourceName: chapter.Title, IP: c.ClientIP(),
		})
	}
	respondOK(c, chapter)
}

// UnpublishChapter 取消章节发布
// POST /api/v1/novels/:id/chapters/:chapter_no/unpublish
func (h *ChapterHandler) UnpublishChapter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelId) {
		return
	}

	chapter, err := h.chapterService.UnpublishChapter(uint(novelId), chapterNo)
	if err != nil {
		logger.Errorf("[ChapterHandler] UnpublishChapter: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c), NovelID: uint(novelId),
			Action: "chapter.unpublish", ResourceType: "chapter",
			ResourceID: chapter.ID, ResourceName: chapter.Title, IP: c.ClientIP(),
		})
	}
	respondOK(c, chapter)
}

// BatchPublishChapters 批量发布小说所有章节到广场
// POST /api/v1/novels/:id/chapters/batch-publish
func (h *ChapterHandler) BatchPublishChapters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	count, err := h.chapterService.BatchPublishChapters(uint(novelId))
	if err != nil {
		logger.Errorf("[ChapterHandler] BatchPublishChapters: novelID=%d err=%v", novelId, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"published_count": count})
}

// GenerateChapterOutline 为章节生成 AI 大纲
// POST /api/v1/novels/:id/chapters/:chapter_no/outline
func (h *ChapterHandler) GenerateChapterOutline(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter no")
		return
	}
	if chapterNo <= 0 {
		respondBadRequest(c, "chapter_no must be a positive integer")
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
	}
	// Fix 7: 校验 JSON 绑定错误，避免静默忽略非法请求体
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}

	chapter, err := h.chapterService.GenerateChapterOutline(getTenantID(c), uint(novelID), chapterNo, req.Prompt)
	if err != nil {
		logger.Errorf("[ChapterHandler] GenerateChapterOutline: novelID=%d chapterNo=%d err=%v", novelID, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, chapter)
}

// GetQualityReport 获取质量报告
// GET /api/v1/chapters/:id/quality-report
func (h *ChapterHandler) GetQualityReport(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	report, err := h.qualityService.CheckChapter(uint(id))
	if err != nil {
		logger.Errorf("[ChapterHandler] GetQualityReport: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, report)
}

// QualityCheck 质量检查
// POST /api/v1/chapters/:id/quality-check
func (h *ChapterHandler) QualityCheck(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	report, err := h.qualityService.CheckChapter(uint(id))
	if err != nil {
		logger.Errorf("[ChapterHandler] QualityCheck: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, report)
}

// RefineChapter 应用改进建议精修章节（返回改进后内容，不自动保存）
// POST /api/v1/chapters/:id/improve
func (h *ChapterHandler) RefineChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Suggestions []string `json:"suggestions"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Suggestions) == 0 {
		respondBadRequest(c, "suggestions required")
		return
	}

	content, err := h.qualityService.RefineWithSuggestions(uint(id), req.Suggestions)
	if err != nil {
		logger.Errorf("[ChapterHandler] RefineChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{"content": content})
}

// ApproveChapter 审核通过章节
// POST /api/v1/chapters/:id/approve
func (h *ChapterHandler) ApproveChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Comment string `json:"comment"`
	}
	_ = c.ShouldBindJSON(&req) // comment is optional; ignore bind errors

	if err := h.chapterService.ApproveChapter(uint(id), req.Comment); err != nil {
		logger.Errorf("[ChapterHandler] ApproveChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, nil)
}

// RejectChapter 驳回章节
// POST /api/v1/chapters/:id/reject
func (h *ChapterHandler) RejectChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if err := h.chapterService.RejectChapter(uint(id), req.Reason); err != nil {
		logger.Errorf("[ChapterHandler] RejectChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, nil)
}

// BatchSummarizeChapters 批量生成章节摘要（异步任务）
// POST /api/v1/novels/:id/chapters/batch-summarize
func (h *ChapterHandler) BatchSummarizeChapters(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterSummaryBatch, "批量生成章节摘要", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	logger.Printf("[ChapterHandler] BatchSummarizeChapters: tenantID=%d novelID=%d taskID=%s", tenantID, novelID, task.TaskID)
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] BatchSummarizeChapters task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		count, err := h.chapterService.BatchGenerateSummaries(tenantID, uint(novelID), progressFn)
		if err != nil {
			logger.Errorf("[ChapterHandler] BatchGenerateSummaries task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"count": count}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "章节摘要批量生成任务已提交")
}

// ─── Chapter AI Review Handlers ──────────────────────────────────────────────

// BatchReviewChapters 批量启动所有章节的正文 AI 审查（异步任务）
// POST /api/v1/novels/:id/chapters/batch-review
func (h *ChapterHandler) BatchReviewChapters(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterReviewBatch, "批量章节正文审查", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] BatchReviewChapters task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		progressFn := func(done, total int) {
			if total > 0 {
				h.taskSvc.UpdateProgress(taskID, done*100/total) //nolint:errcheck
			}
		}
		if err := h.qualityService.BatchReviewNovelChapters(context.Background(), tenantID, uint(novelID), progressFn); err != nil {
			logger.Errorf("[ChapterHandler] BatchReviewChapters task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"novel_id": novelID}) //nolint:errcheck
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "章节正文批量审查任务已提交")
}

// ReviewChapter 启动章节 AI 审查（异步任务）
// POST /api/v1/chapters/:id/review
func (h *ChapterHandler) ReviewChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&req)

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterReview, "章节 AI 审查", "chapter", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
	})

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] ReviewChapter task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		// Intentionally use context.Background(): this goroutine runs after respondAccepted
		// returns the HTTP response. c.Request.Context() would be cancelled at that point,
		// which would abort the long-running AI review call.
		review, reviewErr := h.qualityService.ReviewChapter(context.Background(), uint(id), req.Provider)
		if reviewErr != nil {
			logger.Errorf("[ChapterHandler] ReviewChapter task %s failed: chapterID=%d err=%v", taskID, id, reviewErr)
			h.taskSvc.Fail(taskID, reviewErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 90)           //nolint:errcheck
		h.taskSvc.Complete(taskID, review)              //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "章节审查任务已提交")
}

// ListChapterReviews 获取章节审查历史列表
// GET /api/v1/chapters/:id/reviews
func (h *ChapterHandler) ListChapterReviews(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	records, err := h.qualityService.ListReviewRecords(uint(id))
	if err != nil {
		respondOK(c, []struct{}{})
		return
	}

	type recordResp struct {
		ID           uint                 `json:"id"`
		CreatedAt    string               `json:"created_at"`
		OverallScore float64              `json:"overall_score"`
		Status       string               `json:"status"`
		AppliedAt    *string              `json:"applied_at,omitempty"`
		Review       *model.ChapterReview `json:"review,omitempty"`
	}
	resp := make([]recordResp, 0, len(records))
	for _, rec := range records {
		r := recordResp{
			ID:           rec.ID,
			CreatedAt:    rec.CreatedAt.Format("2006-01-02 15:04:05"),
			OverallScore: rec.OverallScore,
			Status:       rec.Status,
		}
		if rec.AppliedAt != nil {
			s := rec.AppliedAt.Format("2006-01-02 15:04:05")
			r.AppliedAt = &s
		}
		if rec.ReviewJSON != "" {
			var rv model.ChapterReview
			if err := json.Unmarshal([]byte(rec.ReviewJSON), &rv); err == nil {
				r.Review = &rv
			}
		}
		resp = append(resp, r)
	}
	respondOK(c, resp)
}

// GetChapterReview 获取单条审查记录详情（含完整 ReviewJSON）
// GET /api/v1/chapters/:id/reviews/:rid
func (h *ChapterHandler) GetChapterReview(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	rid, ok := parseID(c, "rid")
	if !ok {
		return
	}

	rec, err := h.qualityService.GetReviewRecord(uint(rid), getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "record not found")
		return
	}
	// Ensure the review record belongs to the requested chapter.
	if rec.EntityID != uint(id) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	type resp struct {
		ID           uint                 `json:"id"`
		CreatedAt    string               `json:"created_at"`
		OverallScore float64              `json:"overall_score"`
		Status       string               `json:"status"`
		AppliedAt    *string              `json:"applied_at,omitempty"`
		Review       *model.ChapterReview `json:"review,omitempty"`
	}
	r := resp{
		ID:           rec.ID,
		CreatedAt:    rec.CreatedAt.Format("2006-01-02 15:04:05"),
		OverallScore: rec.OverallScore,
		Status:       rec.Status,
	}
	if rec.AppliedAt != nil {
		s := rec.AppliedAt.Format("2006-01-02 15:04:05")
		r.AppliedAt = &s
	}
	if rec.ReviewJSON != "" {
		var rv model.ChapterReview
		if err := json.Unmarshal([]byte(rec.ReviewJSON), &rv); err == nil {
			r.Review = &rv
		}
	}
	respondOK(c, r)
}

// RollbackChapterReview 回滚章节内容到审查快照
// POST /api/v1/chapters/:id/reviews/:rid/rollback
func (h *ChapterHandler) RollbackChapterReview(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	rid, ok := parseID(c, "rid")
	if !ok {
		return
	}

	if err := h.qualityService.RollbackReview(uint(rid), uint(id), getTenantID(c)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"rolled_back": true})
}

// ApplyChapterReviewDiffs 应用选中的段落改写
// POST /api/v1/chapters/:id/review/apply-diffs
func (h *ChapterHandler) ApplyChapterReviewDiffs(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Diffs    []service.ParagraphDiff `json:"diffs" binding:"required"`
		RecordID uint                    `json:"record_id"`
	}
	if !bindJSON(c, &req) {
		return
	}

	count, err := h.qualityService.ApplyDiffs(uint(id), req.Diffs, req.RecordID, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"updated_paragraphs": count})
}

// ListChapterIgnoredIssues 列出已忽略的审查问题
// GET /api/v1/chapters/:id/ignored-issues
func (h *ChapterHandler) ListChapterIgnoredIssues(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	items, err := h.qualityService.ListIgnoredIssues(uint(id))
	if err != nil {
		respondOK(c, []struct{}{})
		return
	}
	respondOK(c, items)
}

// IgnoreChapterIssue 永久忽略某条审查建议
// POST /api/v1/chapters/:id/ignored-issues
func (h *ChapterHandler) IgnoreChapterIssue(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		IssueText string `json:"issue_text" binding:"required"`
		Note      string `json:"note"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if err := h.qualityService.IgnoreIssue(uint(id), req.IssueText, req.Note); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"ignored": true})
}

// UnignoreChapterIssue 取消忽略
// DELETE /api/v1/chapters/:id/ignored-issues/:iid
func (h *ChapterHandler) UnignoreChapterIssue(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	iid, ok := parseID(c, "iid")
	if !ok {
		return
	}

	if err := h.qualityService.UnignoreIssue(uint(iid)); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, nil)
}

// ListContinuityReports 获取章节的连续性检查记录列表
// GET /api/v1/chapters/:id/continuity-reports
func (h *ChapterHandler) ListContinuityReports(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if h.continuityService == nil {
		respondErr(c, http.StatusServiceUnavailable, "continuity service not available")
		return
	}

	records, err := h.continuityService.ListReports(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, records)
}

// BatchDeleteChapters 批量删除章节
// DELETE /api/v1/novels/:id/chapters
// Body: {"chapter_ids": [1,2,3]}
func (h *ChapterHandler) BatchDeleteChapters(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !requireNovelEditorRole(c, h.novelService, novelID) {
		return
	}

	var req struct {
		ChapterIDs []uint `json:"chapter_ids" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}
	if len(req.ChapterIDs) == 0 {
		respondBadRequest(c, "chapter_ids must not be empty")
		return
	}

	tenantID := getTenantID(c)
	if err := h.chapterService.BatchDeleteChapters(c.Request.Context(), novelID, tenantID, req.ChapterIDs); err != nil {
		logger.Errorf("[ChapterHandler] BatchDeleteChapters: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "delete failed")
		return
	}
	respondOK(c, gin.H{"deleted": len(req.ChapterIDs)})
}

// RewriteChapterByInstruction POST /api/v1/chapters/:id/rewrite
// 根据用户自然语言指令异步重写章节内容，完成后作为新版本保存。
func (h *ChapterHandler) RewriteChapterByInstruction(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Instruction string `json:"instruction"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.Instruction) == "" {
		respondBadRequest(c, "instruction is required")
		return
	}

	tenantID := getTenantID(c)
	// Verify ownership
	chapter, err := h.chapterService.GetChapter(uint(id), tenantID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if chapter.Content == "" {
		respondErr(c, http.StatusBadRequest, "chapter has no content to rewrite")
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterRewriteInstr, "按指令修改章节", "chapter", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	instruction := req.Instruction
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ChapterHandler] RewriteByInstruction task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		newContent, rewriteErr := h.qualityService.RewriteByInstruction(context.Background(), uint(id), instruction)
		if rewriteErr != nil {
			logger.Errorf("[ChapterHandler] RewriteByInstruction task %s failed: chapterID=%d err=%v", taskID, id, rewriteErr)
			h.taskSvc.Fail(taskID, rewriteErr.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 80) //nolint:errcheck

		// Save current content as a version before overwriting
		if err2 := h.chapterService.ArchiveVersionBeforeRewrite(uint(id), instruction); err2 != nil {
			logger.Errorf("[ChapterHandler] RewriteByInstruction archive version failed: %v", err2)
		}

		// Apply new content
		updated, applyErr := h.chapterService.ApplyRewrittenContent(uint(id), newContent)
		if applyErr != nil {
			logger.Errorf("[ChapterHandler] RewriteByInstruction apply content failed: %v", applyErr)
			h.taskSvc.Fail(taskID, "保存修改内容失败: "+applyErr.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, map[string]interface{}{"chapter": updated}) //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "按指令修改任务已提交")
}

// RefineSelection POST /api/v1/chapters/:id/refine-selection
// 对选中片段按指令同步精修，返回改写后的文字（不保存）
func (h *ChapterHandler) RefineSelection(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		SelectedText string `json:"selected_text"`
		Instruction  string `json:"instruction"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.SelectedText) == "" {
		respondBadRequest(c, "selected_text is required")
		return
	}
	if strings.TrimSpace(req.Instruction) == "" {
		respondBadRequest(c, "instruction is required")
		return
	}

	tenantID := getTenantID(c)
	if _, err := h.chapterService.GetChapter(uint(id), tenantID); err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	refinedText, err := h.qualityService.RefineSelection(c.Request.Context(), uint(id), req.SelectedText, req.Instruction)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, gin.H{"refined_text": refinedText})
}

// ChapterChatStream POST /api/v1/chapters/:id/chat/stream
// 与 AI 编辑对话，修改章节内容（SSE 流式）
func (h *ChapterHandler) ChapterChatStream(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Messages) == 0 {
		respondBadRequest(c, "messages is required")
		return
	}

	tenantID := getTenantID(c)
	chapter, err := h.chapterService.GetChapter(uint(id), tenantID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	if h.novelService == nil {
		respondErr(c, http.StatusInternalServerError, "service not initialized")
		return
	}

	systemPrompt := buildChapterChatSystemPrompt(chapter)
	msgs := buildChatMessages(req.Messages)

	aiSvc := h.novelService.GetAIService()
	ch, streamErr := aiSvc.StreamWithMessagesCtx(c.Request.Context(), tenantID, "novel_chat", msgs, systemPrompt)
	if streamErr != nil {
		logger.Errorf("[ChapterChatStream] chapterID=%d err=%v", id, streamErr)
		respondErr(c, http.StatusInternalServerError, "AI 响应失败: "+streamErr.Error())
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	var fullContent strings.Builder
	for chunk := range ch {
		if chunk.Error != "" {
			payload, _ := json.Marshal(map[string]string{"error": chunk.Error})
			fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
			c.Writer.Flush()
			return
		}
		if chunk.Content == "" {
			continue
		}
		fullContent.WriteString(chunk.Content)
		payload, _ := json.Marshal(map[string]string{"delta": chunk.Content})
		fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		c.Writer.Flush()
	}

	modifiedContent := extractChapterModifiedContent(fullContent.String())
	donePayload, _ := json.Marshal(map[string]interface{}{
		"done":             true,
		"modified_content": modifiedContent,
	})
	fmt.Fprintf(c.Writer, "data: %s\n\n", donePayload)
	c.Writer.Flush()
}

func buildChapterChatSystemPrompt(chapter *model.Chapter) string {
	content := chapter.Content
	runes := []rune(content)
	if len(runes) > 6000 {
		content = string(runes[:6000]) + "\n...（正文过长已截断）"
	}

	var sb strings.Builder
	sb.WriteString("你是一位专业小说编辑，正在协助作者修改章节内容。\n\n")
	fmt.Fprintf(&sb, "当前章节：第 %d 章「%s」\n\n", chapter.ChapterNo, chapter.Title)
	if chapter.Outline != "" {
		fmt.Fprintf(&sb, "章节大纲：%s\n\n", chapter.Outline)
	}
	sb.WriteString("当前章节正文：\n---\n")
	sb.WriteString(content)
	sb.WriteString("\n---\n\n")
	sb.WriteString("工作规则：\n")
	sb.WriteString("1. 针对用户的修改需求，给出专业、具体的改写方案\n")
	sb.WriteString("2. 段落级别的局部修改直接在回复中给出修改内容即可\n")
	sb.WriteString("3. 若用户要求对整章进行重大修改，将修改后的完整正文放在以下标记之间（标记各占独立一行）：\n")
	sb.WriteString("   【REVISED_CONTENT_START】\n   （修改后全文）\n   【REVISED_CONTENT_END】\n")
	sb.WriteString("4. 保持原文写作风格和叙事连贯性，除非用户明确要求改变\n")
	sb.WriteString("5. 用中文回复")
	return sb.String()
}

func extractChapterModifiedContent(text string) string {
	const startMark = "【REVISED_CONTENT_START】"
	const endMark = "【REVISED_CONTENT_END】"
	s := strings.Index(text, startMark)
	if s == -1 {
		return ""
	}
	e := strings.Index(text, endMark)
	if e == -1 || e <= s {
		return ""
	}
	return strings.TrimSpace(text[s+len(startMark) : e])
}

