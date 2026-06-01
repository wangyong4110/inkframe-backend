package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// OutlineReviewHandler 章节大纲审查处理器
type OutlineReviewHandler struct {
	reviewSvc *service.OutlineReviewService
	taskSvc   *service.TaskService
}

func NewOutlineReviewHandler(reviewSvc *service.OutlineReviewService, taskSvc *service.TaskService) *OutlineReviewHandler {
	return &OutlineReviewHandler{reviewSvc: reviewSvc, taskSvc: taskSvc}
}

// ReviewChapter 审查单个章节大纲（异步）
// POST /api/v1/chapters/:id/outline-review
func (h *OutlineReviewHandler) ReviewChapter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterReview, "大纲审查", "chapter", id)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[OutlineReviewHandler] ReviewChapter panic: %v", r)
				h.taskSvc.Fail(taskID, "内部错误") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := c.Request.Context()
		review, err := h.reviewSvc.ReviewChapterOutline(ctx, tenantID, id)
		if err != nil {
			logger.Printf("[OutlineReviewHandler] ReviewChapter failed: chapterID=%d err=%v", id, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"review": review}) //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "大纲审查任务已提交")
}

// GetChapterReview 获取章节大纲审查结果
// GET /api/v1/chapters/:id/outline-review
func (h *OutlineReviewHandler) GetChapterReview(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	review, err := h.reviewSvc.GetReview(tenantID, id)
	if err != nil {
		respondErr(c, http.StatusNotFound, "no review found")
		return
	}
	respondOK(c, review)
}

// BatchReviewNovel 批量审查小说所有章节大纲（异步）
// POST /api/v1/novels/:id/outline-review/batch
func (h *OutlineReviewHandler) BatchReviewNovel(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, "outline_review_batch", "大纲批量审查", "novel", novelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[OutlineReviewHandler] BatchReview panic: %v", r)
				h.taskSvc.Fail(taskID, "内部错误") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := c.Request.Context()
		reviews, err := h.reviewSvc.BatchReviewNovel(ctx, tenantID, novelID, func(done, total int) {
			progress := int(float64(done) / float64(total) * 90)
			h.taskSvc.UpdateProgress(taskID, progress) //nolint:errcheck
		})
		if err != nil {
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"reviews": reviews, "count": len(reviews)}) //nolint:errcheck
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "大纲批量审查任务已提交")
}

// ListNovelReviews 获取小说所有章节审查结果
// GET /api/v1/novels/:id/outline-review
func (h *OutlineReviewHandler) ListNovelReviews(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	reviews, err := h.reviewSvc.ListNovelReviews(tenantID, novelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, reviews)
}
