package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
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
	if _, err := h.novelService.GetNovel(novelId, getTenantID(c)); err != nil {
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

	// Fix 4: 验证 novel 归属当前租户，防止跨租户写入
	if !h.checkNovelOwnership(c, novelId) {
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
		logger.Printf("[ChapterHandler] CreateChapter: novelID=%d err=%v", novelId, err)
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
			logger.Printf("[ChapterHandler] ListChapters: novelID=%d err=%v", novelId, err)
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
		logger.Printf("[ChapterHandler] ListChapters: novelID=%d err=%v", novelId, err)
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
		logger.Printf("[ChapterHandler] UpdateChapter: chapterID=%d err=%v", id, err)
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
		logger.Printf("[ChapterHandler] DeleteChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, nil)
}

// GenerateChapter 生成章节内容
// POST /api/v1/chapters/generate
func (h *ChapterHandler) GenerateChapter(c *gin.Context) {
	var req model.GenerateChapterRequest
	if !bindJSON(c, &req) {
		return
	}
	logger.Printf("[ChapterHandler] GenerateChapter: novelID=%d chapterNo=%d", req.NovelID, req.ChapterNo)

	if !h.checkNovelOwnership(c, req.NovelID) {
		return
	}

	// 支持通过 Header 临时覆盖 AI 模型/provider
	if override := c.GetHeader("X-Model-Override"); override != "" && req.ModelOverride == "" {
		req.ModelOverride = override
	}

	chapter, err := h.chapterService.GenerateChapter(getTenantID(c), req.NovelID, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	modelUsed := req.ModelOverride
	if modelUsed == "" {
		modelUsed = h.chapterService.GetDefaultProviderName()
	}

	c.JSON(http.StatusOK, gin.H{
		"code":       0,
		"message":    "success",
		"data":       chapter,
		"model_used": modelUsed,
	})
}

// RegenerateChapter 重新生成章节
// POST /api/v1/chapters/:id/regenerate
func (h *ChapterHandler) RegenerateChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if !bindJSON(c, &req) {
		return
	}

	chapter, err := h.chapterService.RegenerateChapter(getTenantID(c), uint(id), req.Prompt)
	if err != nil {
		logger.Printf("[ChapterHandler] RegenerateChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapter)
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
		logger.Printf("[ChapterHandler] GetVersions: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, versions)
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
		logger.Printf("[ChapterHandler] RestoreVersion: chapterID=%d versionNo=%d err=%v", id, versionNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, chapter)
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

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	var req model.UpdateChapterRequest
	if !bindJSON(c, &req) {
		return
	}

	chapter, err := h.chapterService.UpdateChapterByNo(uint(novelId), chapterNo, &req)
	if err != nil {
		logger.Printf("[ChapterHandler] UpdateChapterByNo: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
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

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	if err := h.chapterService.DeleteChapterByNo(uint(novelId), chapterNo); err != nil {
		logger.Printf("[ChapterHandler] DeleteChapterByNo: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
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

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	chapter, err := h.chapterService.PublishChapter(uint(novelId), chapterNo)
	if err != nil {
		logger.Printf("[ChapterHandler] PublishChapter: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
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

	if !h.checkNovelOwnership(c, novelId) {
		return
	}

	chapter, err := h.chapterService.UnpublishChapter(uint(novelId), chapterNo)
	if err != nil {
		logger.Printf("[ChapterHandler] UnpublishChapter: novelID=%d chapterNo=%d err=%v", novelId, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
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
		logger.Printf("[ChapterHandler] BatchPublishChapters: novelID=%d err=%v", novelId, err)
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
		logger.Printf("[ChapterHandler] GenerateChapterOutline: novelID=%d chapterNo=%d err=%v", novelID, chapterNo, err)
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
		logger.Printf("[ChapterHandler] GetQualityReport: chapterID=%d err=%v", id, err)
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
		logger.Printf("[ChapterHandler] QualityCheck: chapterID=%d err=%v", id, err)
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
		logger.Printf("[ChapterHandler] RefineChapter: chapterID=%d err=%v", id, err)
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
		logger.Printf("[ChapterHandler] ApproveChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
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
		logger.Printf("[ChapterHandler] RejectChapter: chapterID=%d err=%v", id, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
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
				logger.Printf("[ChapterHandler] BatchSummarizeChapters task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		count, err := h.chapterService.BatchGenerateSummaries(tenantID, uint(novelID), progressFn)
		if err != nil {
			logger.Printf("[ChapterHandler] BatchGenerateSummaries task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"count": count}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "章节摘要批量生成任务已提交")
}

// ─── Chapter AI Review Handlers ──────────────────────────────────────────────

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
				logger.Printf("[ChapterHandler] ReviewChapter task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck

		review, reviewErr := h.qualityService.ReviewChapter(context.Background(), uint(id), req.Provider)
		if reviewErr != nil {
			logger.Printf("[ChapterHandler] ReviewChapter task %s failed: chapterID=%d err=%v", taskID, id, reviewErr)
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

