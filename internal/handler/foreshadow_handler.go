package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ForeshadowHandler 伏笔管理处理器
type ForeshadowHandler struct {
	svc *service.ForeshadowCRUDService
}

func NewForeshadowHandler(svc *service.ForeshadowCRUDService) *ForeshadowHandler {
	return &ForeshadowHandler{svc: svc}
}

// ListForeshadows GET /novels/:id/foreshadows
func (h *ForeshadowHandler) ListForeshadows(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.ListByNovel(c.Request.Context(), novelID)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] ListForeshadows: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to list foreshadows")
		return
	}
	respondOK(c, gin.H{"foreshadows": list, "total": len(list)})
}

// CreateForeshadow POST /novels/:id/foreshadows
func (h *ForeshadowHandler) CreateForeshadow(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var f model.Foreshadow
	if !bindJSON(c, &f) {
		return
	}
	if f.Title == "" {
		respondBadRequest(c, "title is required")
		return
	}
	f.NovelID = novelID
	if f.Status == "" {
		f.Status = "planted"
	}
	if f.Level == "" {
		f.Level = "sub"
	}
	if err := h.svc.Create(c.Request.Context(), &f); err != nil {
		logger.Errorf("[ForeshadowHandler] CreateForeshadow: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to create foreshadow")
		return
	}
	respondCreated(c, f)
}

// ListUnfulfilledForeshadows GET /novels/:id/foreshadows/unfulfilled
func (h *ForeshadowHandler) ListUnfulfilledForeshadows(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.ListUnfulfilled(c.Request.Context(), novelID)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] ListUnfulfilled: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to list unfulfilled foreshadows")
		return
	}
	respondOK(c, gin.H{"foreshadows": list, "total": len(list)})
}

// UpdateForeshadow PUT /novels/:id/foreshadows/:foreshadow_id
func (h *ForeshadowHandler) UpdateForeshadow(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	foreshadowID, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}
	var updates map[string]interface{}
	if !bindJSON(c, &updates) {
		return
	}
	f, err := h.svc.Update(c.Request.Context(), foreshadowID, updates)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] UpdateForeshadow: id=%d err=%v", foreshadowID, err)
		respondErr(c, http.StatusInternalServerError, "failed to update foreshadow")
		return
	}
	respondOK(c, f)
}

// AIExtractForeshadows POST /novels/:id/foreshadows/extract
func (h *ForeshadowHandler) AIExtractForeshadows(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	list, err := h.svc.AIExtractFromNovel(c.Request.Context(), tenantID, novelID)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] AIExtract: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "AI extraction failed: "+err.Error())
		return
	}
	respondOK(c, gin.H{"foreshadows": list, "total": len(list)})
}

// GetForeshadowStats GET /novels/:id/foreshadows/stats?current_chapter=N
func (h *ForeshadowHandler) GetForeshadowStats(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	currentChapterNo := 0
	if v := c.Query("current_chapter"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			currentChapterNo = n
		}
	}
	stats, err := h.svc.GetStats(c.Request.Context(), novelID, currentChapterNo)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] GetStats: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to get foreshadow stats")
		return
	}
	respondOK(c, stats)
}

// DeleteForeshadow DELETE /novels/:id/foreshadows/:foreshadow_id
func (h *ForeshadowHandler) DeleteForeshadow(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	foreshadowID, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), foreshadowID); err != nil {
		logger.Errorf("[ForeshadowHandler] DeleteForeshadow: id=%d err=%v", foreshadowID, err)
		respondErr(c, http.StatusInternalServerError, "failed to delete foreshadow")
		return
	}
	respondOK(c, gin.H{"deleted": true})
}

// AddReinforcement POST /novels/:id/foreshadows/:foreshadow_id/reinforce
func (h *ForeshadowHandler) AddReinforcement(c *gin.Context) {
	_, ok := parseID(c, "id")
	if !ok {
		return
	}
	foreshadowID, ok := parseID(c, "foreshadow_id")
	if !ok {
		return
	}
	var body struct {
		ChapterNo int    `json:"chapter_no" binding:"required"`
		Note      string `json:"note"`
	}
	if !bindJSON(c, &body) {
		return
	}
	f, err := h.svc.AddReinforcement(c.Request.Context(), foreshadowID, body.ChapterNo, body.Note)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] AddReinforcement: id=%d err=%v", foreshadowID, err)
		respondErr(c, http.StatusInternalServerError, "failed to add reinforcement")
		return
	}
	respondOK(c, f)
}

// GetForeshadowTree GET /novels/:id/foreshadows/tree
func (h *ForeshadowHandler) GetForeshadowTree(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tree, err := h.svc.GetTree(c.Request.Context(), novelID)
	if err != nil {
		logger.Errorf("[ForeshadowHandler] GetTree: novelID=%d err=%v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to get foreshadow tree")
		return
	}
	respondOK(c, gin.H{"tree": tree, "total": len(tree)})
}
