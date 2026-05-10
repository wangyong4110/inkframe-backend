package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// DramaticHandler 戏剧张力管理处理器
type DramaticHandler struct {
	hookSvc   *service.HookChainService
	spSvc     *service.SatisfactionPointService
	arcSvc    *service.ConflictArcService
	pacingSvc *service.PacingService
}

func NewDramaticHandler(
	hookSvc *service.HookChainService,
	spSvc *service.SatisfactionPointService,
	arcSvc *service.ConflictArcService,
	pacingSvc *service.PacingService,
) *DramaticHandler {
	return &DramaticHandler{hookSvc: hookSvc, spSvc: spSvc, arcSvc: arcSvc, pacingSvc: pacingSvc}
}

// ─── 节奏曲线 ──────────────────────────────────────────────────────────────────

// GetPacingCurve GET /novels/:id/pacing-curve
func (h *DramaticHandler) GetPacingCurve(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	points, err := h.pacingSvc.GetCurve(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to get pacing curve")
		return
	}
	respondOK(c, gin.H{"points": points, "total": len(points)})
}

// GetPacingHealth GET /novels/:id/pacing-health
func (h *DramaticHandler) GetPacingHealth(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	health, err := h.pacingSvc.GetHealth(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to get pacing health")
		return
	}
	respondOK(c, health)
}

// ─── 钩子链 ────────────────────────────────────────────────────────────────────

// ListHooks GET /novels/:id/hooks
func (h *DramaticHandler) ListHooks(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	hooks, err := h.hookSvc.ListByNovel(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list hooks")
		return
	}
	respondOK(c, gin.H{"hooks": hooks, "total": len(hooks)})
}

// CreateHook POST /novels/:id/hooks
func (h *DramaticHandler) CreateHook(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.HookChain
	if !bindJSON(c, &req) {
		return
	}
	hook, err := h.hookSvc.Create(getTenantID(c), uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create hook")
		return
	}
	respondCreated(c, hook)
}

// UpdateHook PUT /hooks/:id
func (h *DramaticHandler) UpdateHook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.HookChain
	if !bindJSON(c, &req) {
		return
	}
	hook, err := h.hookSvc.Update(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update hook")
		return
	}
	respondOK(c, hook)
}

// DeleteHook DELETE /hooks/:id
func (h *DramaticHandler) DeleteHook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.hookSvc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete hook")
		return
	}
	respondOK(c, nil)
}

// FulfillHook PUT /hooks/:id/fulfill
func (h *DramaticHandler) FulfillHook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var body struct {
		ActualChapter int `json:"actual_chapter" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	hook, err := h.hookSvc.Fulfill(uint(id), body.ActualChapter)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to fulfill hook")
		return
	}
	respondOK(c, hook)
}

// ─── 爽点 ──────────────────────────────────────────────────────────────────────

// ListSatisfactionPoints GET /novels/:id/satisfaction-points
func (h *DramaticHandler) ListSatisfactionPoints(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	sps, err := h.spSvc.ListByNovel(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list satisfaction points")
		return
	}
	respondOK(c, gin.H{"satisfaction_points": sps, "total": len(sps)})
}

// CreateSatisfactionPoint POST /novels/:id/satisfaction-points
func (h *DramaticHandler) CreateSatisfactionPoint(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.SatisfactionPoint
	if !bindJSON(c, &req) {
		return
	}
	sp, err := h.spSvc.Create(getTenantID(c), uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create satisfaction point")
		return
	}
	respondCreated(c, sp)
}

// UpdateSatisfactionPoint PUT /satisfaction-points/:id
func (h *DramaticHandler) UpdateSatisfactionPoint(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.SatisfactionPoint
	if !bindJSON(c, &req) {
		return
	}
	sp, err := h.spSvc.Update(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update satisfaction point")
		return
	}
	respondOK(c, sp)
}

// DeleteSatisfactionPoint DELETE /satisfaction-points/:id
func (h *DramaticHandler) DeleteSatisfactionPoint(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.spSvc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete satisfaction point")
		return
	}
	respondOK(c, nil)
}

// ─── 冲突弧 ────────────────────────────────────────────────────────────────────

// ListConflictArcs GET /novels/:id/conflict-arcs
func (h *DramaticHandler) ListConflictArcs(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	arcs, err := h.arcSvc.ListByNovel(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list conflict arcs")
		return
	}
	respondOK(c, gin.H{"conflict_arcs": arcs, "total": len(arcs)})
}

// CreateConflictArc POST /novels/:id/conflict-arcs
func (h *DramaticHandler) CreateConflictArc(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.ConflictArc
	if !bindJSON(c, &req) {
		return
	}
	arc, err := h.arcSvc.Create(getTenantID(c), uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create conflict arc")
		return
	}
	respondCreated(c, arc)
}

// UpdateConflictArc PUT /conflict-arcs/:id
func (h *DramaticHandler) UpdateConflictArc(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.ConflictArc
	if !bindJSON(c, &req) {
		return
	}
	arc, err := h.arcSvc.Update(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update conflict arc")
		return
	}
	respondOK(c, arc)
}

// DeleteConflictArc DELETE /conflict-arcs/:id
func (h *DramaticHandler) DeleteConflictArc(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.arcSvc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete conflict arc")
		return
	}
	respondOK(c, nil)
}

// AdvancePhase PUT /conflict-arcs/:id/advance-phase
func (h *DramaticHandler) AdvancePhase(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	arc, err := h.arcSvc.AdvancePhase(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to advance phase")
		return
	}
	respondOK(c, arc)
}
