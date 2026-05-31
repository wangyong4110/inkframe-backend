package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
	"gorm.io/gorm"
)

// RewriteHandler handles novel rewriting project endpoints
type RewriteHandler struct {
	rewriteSvc *service.RewriteService
}

// NewRewriteHandler creates a new RewriteHandler
func NewRewriteHandler(rewriteSvc *service.RewriteService) *RewriteHandler {
	return &RewriteHandler{rewriteSvc: rewriteSvc}
}

// getProjectForTenant 提取租户鉴权公共逻辑。返回 false 时已写入错误响应。
func (h *RewriteHandler) getProjectForTenant(c *gin.Context, id uint) (*model.RewriteProject, bool) {
	project, err := h.rewriteSvc.GetProject(id)
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return nil, false
	}
	if project.TenantID != c.GetUint("tenant_id") {
		respondErr(c, http.StatusForbidden, "forbidden")
		return nil, false
	}
	return project, true
}

// ListProjects GET /rewrite/projects
func (h *RewriteHandler) ListProjects(c *gin.Context) {
	tenantID := c.GetUint("tenant_id")
	p := parsePagination(c)
	projects, total, err := h.rewriteSvc.ListProjects(tenantID, p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": projects, "total": total, "page": p.Page, "page_size": p.PageSize})
}

// CreateProject POST /rewrite/projects
func (h *RewriteHandler) CreateProject(c *gin.Context) {
	tenantID := c.GetUint("tenant_id")
	var req struct {
		NovelID uint   `json:"novel_id" binding:"required"`
		Name    string `json:"name" binding:"required"`
		Level   int    `json:"level"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if req.Level < 1 || req.Level > 5 {
		req.Level = 2
	}
	project, err := h.rewriteSvc.CreateProject(tenantID, req.NovelID, req.Name, req.Level)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, project)
}

// GetProject GET /rewrite/projects/:id
func (h *RewriteHandler) GetProject(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	project, ok := h.getProjectForTenant(c, uint(id))
	if !ok {
		return
	}
	respondOK(c, project)
}

// DeleteProject DELETE /rewrite/projects/:id
func (h *RewriteHandler) DeleteProject(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	if err := h.rewriteSvc.DeleteProject(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

// StartAnalysis POST /rewrite/projects/:id/analyze
// Returns 202 Accepted with task_id; poll GET /api/v1/tasks/:task_id for progress.
func (h *RewriteHandler) StartAnalysis(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	tenantID := c.GetUint("tenant_id")
	taskID, err := h.rewriteSvc.StartAnalysis(tenantID, uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondAccepted(c, taskID, "文学分析任务已提交")
}

// StartRewriting POST /rewrite/projects/:id/rewrite
// Returns 202 Accepted with task_id; poll GET /api/v1/tasks/:task_id for progress.
func (h *RewriteHandler) StartRewriting(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	tenantID := c.GetUint("tenant_id")
	taskID, err := h.rewriteSvc.StartRewriting(tenantID, uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondAccepted(c, taskID, "章节改写任务已提交")
}

// GetAnalysis GET /rewrite/projects/:id/analysis
func (h *RewriteHandler) GetAnalysis(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	analysis, err := h.rewriteSvc.GetAnalysis(uint(id))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondOK(c, nil)
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, analysis)
}

// GetBible GET /rewrite/projects/:id/bible
func (h *RewriteHandler) GetBible(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	bible, err := h.rewriteSvc.GetBible(uint(id))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respondOK(c, nil)
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, bible)
}

// ListChapterTasks GET /rewrite/projects/:id/chapters
func (h *RewriteHandler) ListChapterTasks(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	tasks, err := h.rewriteSvc.ListChapterTasks(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": tasks, "total": len(tasks)})
}

// GetChapterTask GET /rewrite/projects/:id/chapters/:task_id
func (h *RewriteHandler) GetChapterTask(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	taskID, err := strconv.ParseUint(c.Param("task_id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid task_id")
		return
	}
	task, err := h.rewriteSvc.GetChapterTask(uint(taskID))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	if task.ProjectID != uint(id) {
		respondErr(c, http.StatusForbidden, "task does not belong to this project")
		return
	}
	respondOK(c, task)
}

// ApproveChapter POST /rewrite/projects/:id/chapters/:task_id/approve
func (h *RewriteHandler) ApproveChapter(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	taskID, err := strconv.ParseUint(c.Param("task_id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid task_id")
		return
	}
	task, err := h.rewriteSvc.GetChapterTask(uint(taskID))
	if err != nil {
		respondErr(c, http.StatusNotFound, err.Error())
		return
	}
	if task.ProjectID != uint(id) {
		respondErr(c, http.StatusForbidden, "task does not belong to this project")
		return
	}
	if err := h.rewriteSvc.ApproveChapter(uint(taskID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "approved"})
}

// GetComplianceReport GET /rewrite/projects/:id/compliance-report
func (h *RewriteHandler) GetComplianceReport(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	report, err := h.rewriteSvc.GetComplianceReport(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, report)
}

// UpdateBible PUT /rewrite/projects/:id/bible
func (h *RewriteHandler) UpdateBible(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if _, ok := h.getProjectForTenant(c, uint(id)); !ok {
		return
	}
	var req service.UpdateBibleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.rewriteSvc.UpdateBible(uint(id), req); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "updated"})
}
