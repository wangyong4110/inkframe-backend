package handler

import (
	"net/http"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// PlotPointHandler 剧情点处理器
type PlotPointHandler struct {
	svc        *service.PlotPointService
	chapterSvc *service.ChapterService
	taskSvc    *service.TaskService
}

func NewPlotPointHandler(svc *service.PlotPointService) *PlotPointHandler {
	return &PlotPointHandler{svc: svc}
}

// WithChapterService 注入章节服务（用于 ExtractFromChapter 时服务端加载章节内容）
func (h *PlotPointHandler) WithChapterService(svc *service.ChapterService) *PlotPointHandler {
	h.chapterSvc = svc
	return h
}

func (h *PlotPointHandler) WithTaskService(svc *service.TaskService) *PlotPointHandler {
	h.taskSvc = svc
	return h
}

// ListByChapter GET /chapters/:id/plot-points
func (h *PlotPointHandler) ListByChapter(c *gin.Context) {
	chapterID, ok := parseID(c, "id")
	if !ok {
		return
	}
	pps, err := h.svc.List(uint(chapterID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list plot points")
		return
	}
	respondOK(c, gin.H{"plot_points": pps, "total": len(pps)})
}

// Create POST /chapters/:id/plot-points
func (h *PlotPointHandler) Create(c *gin.Context) {
	chapterID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var pp model.PlotPoint
	if !bindJSON(c, &pp) {
		return
	}
	pp.ChapterID = uint(chapterID)
	if err := h.svc.Create(&pp); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create plot point")
		return
	}
	respondCreated(c, pp)
}

// ExtractFromChapter POST /chapters/:id/plot-points/extract
func (h *PlotPointHandler) ExtractFromChapter(c *gin.Context) {
	chapterID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if h.chapterSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not configured")
		return
	}
	chapter, err := h.chapterSvc.GetChapter(uint(chapterID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	pps, err := h.svc.ExtractFromChapter(getTenantID(c), chapter)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to extract plot points: "+err.Error())
		return
	}
	respondOK(c, gin.H{"plot_points": pps, "total": len(pps)})
}

// ListByNovel GET /novels/:id/plot-points
func (h *PlotPointHandler) ListByNovel(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	ppType := c.Query("type")
	onlyUnresolved := c.Query("unresolved") == "true" || c.Query("unresolved") == "1"
	p := parsePagination(c)
	pps, total, err := h.svc.ListByNovelPaged(uint(novelID), ppType, onlyUnresolved, p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list plot points")
		return
	}
	respondOK(c, gin.H{"plot_points": pps, "total": total, "page": p.Page, "page_size": p.PageSize})
}

// Update PUT /plot-points/:id
func (h *PlotPointHandler) Update(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "plot point not found")
		return
	}
	if existing.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var req model.UpdatePlotPointRequest
	if !bindJSON(c, &req) {
		return
	}
	pp, err := h.svc.Update(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update plot point")
		return
	}
	respondOK(c, pp)
}

// MarkResolved PUT /plot-points/:id/resolve
func (h *PlotPointHandler) MarkResolved(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "plot point not found")
		return
	}
	if existing.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		ResolvedInChapterID uint `json:"resolved_in_chapter_id" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	pp, err := h.svc.MarkResolved(uint(id), body.ResolvedInChapterID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to mark plot point resolved")
		return
	}
	respondOK(c, pp)
}

// Delete DELETE /plot-points/:id
func (h *PlotPointHandler) Delete(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "plot point not found")
		return
	}
	if existing.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.svc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete plot point")
		return
	}
	respondOK(c, nil)
}

// AIExtractFromNovel POST /novels/:id/plot-points/ai-extract
func (h *PlotPointHandler) AIExtractFromNovel(c *gin.Context) {
	if h.taskSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "task service not configured")
		return
	}
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypePlotExtract, "AI提取剧情点", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[PlotPointHandler] AIExtractFromNovel task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		pps, err := h.svc.AIExtractFromNovel(tenantID, uint(novelID))
		if err != nil {
			logger.Printf("[PlotPointHandler] AIExtractFromNovel task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"plot_points": pps, "count": len(pps)}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "剧情点提取任务已提交")
}
