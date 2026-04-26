package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// PlotPointHandler 剧情点处理器
type PlotPointHandler struct {
	svc        *service.PlotPointService
	chapterSvc *service.ChapterService
}

func NewPlotPointHandler(svc *service.PlotPointService) *PlotPointHandler {
	return &PlotPointHandler{svc: svc}
}

// WithChapterService 注入章节服务（用于 ExtractFromChapter 时服务端加载章节内容）
func (h *PlotPointHandler) WithChapterService(svc *service.ChapterService) *PlotPointHandler {
	h.chapterSvc = svc
	return h
}

// ListByChapter GET /chapters/:id/plot-points
func (h *PlotPointHandler) ListByChapter(c *gin.Context) {
	chapterID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid chapter id")
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
	chapterID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid chapter id")
		return
	}
	var pp model.PlotPoint
	if err := c.ShouldBindJSON(&pp); err != nil {
		respondBadRequest(c, err.Error())
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
	chapterID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid chapter id")
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
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	ppType := c.Query("type")
	onlyUnresolved := c.Query("unresolved") == "true" || c.Query("unresolved") == "1"
	pps, err := h.svc.ListByNovel(uint(novelID), ppType, onlyUnresolved)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list plot points")
		return
	}
	respondOK(c, gin.H{"plot_points": pps, "total": len(pps)})
}

// Update PUT /plot-points/:id
func (h *PlotPointHandler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid plot point id")
		return
	}
	var req model.UpdatePlotPointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid plot point id")
		return
	}
	var body struct {
		ResolvedInChapterID uint `json:"resolved_in_chapter_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid plot point id")
		return
	}
	if err := h.svc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete plot point")
		return
	}
	respondOK(c, nil)
}
