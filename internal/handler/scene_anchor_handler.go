package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SceneAnchorHandler 场景锚点处理器
type SceneAnchorHandler struct {
	svc              *service.SceneAnchorService
	consistencySvc   *service.SceneConsistencyService
}

func NewSceneAnchorHandler(svc *service.SceneAnchorService, consistencySvc *service.SceneConsistencyService) *SceneAnchorHandler {
	return &SceneAnchorHandler{svc: svc, consistencySvc: consistencySvc}
}

// GetSceneAnchor GET /scene-anchors/:id
func (h *SceneAnchorHandler) GetSceneAnchor(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	anchor, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	respondOK(c, anchor)
}

// ListSceneAnchors GET /novels/:id/scene-anchors
func (h *SceneAnchorHandler) ListSceneAnchors(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	anchors, err := h.svc.ListByNovel(uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list scene anchors")
		return
	}
	respondOK(c, gin.H{"scene_anchors": anchors, "total": len(anchors)})
}

// CreateSceneAnchor POST /novels/:id/scene-anchors
func (h *SceneAnchorHandler) CreateSceneAnchor(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	var req service.CreateSceneAnchorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	anchor, err := h.svc.Create(getTenantID(c), uint(novelID), req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create scene anchor")
		return
	}
	respondCreated(c, anchor)
}

// UpdateSceneAnchor PUT /scene-anchors/:id
func (h *SceneAnchorHandler) UpdateSceneAnchor(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	var req service.UpdateSceneAnchorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	anchor, err := h.svc.Update(uint(id), req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update scene anchor")
		return
	}
	respondOK(c, anchor)
}

// DeleteSceneAnchor DELETE /scene-anchors/:id
func (h *SceneAnchorHandler) DeleteSceneAnchor(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	if err := h.svc.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete scene anchor")
		return
	}
	respondOK(c, nil)
}

// SetShotAnchor PUT /videos/:video_id/shots/:shot_id/anchor
func (h *SceneAnchorHandler) SetShotAnchor(c *gin.Context) {
	shotID, err := strconv.ParseUint(c.Param("shot_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid shot id")
		return
	}
	var body struct {
		AnchorID *uint `json:"anchor_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.SetShotAnchor(uint(shotID), body.AnchorID); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to set shot anchor")
		return
	}
	respondOK(c, nil)
}

// ExtractSceneAnchors POST /novels/:id/scene-anchors/extract
func (h *SceneAnchorHandler) ExtractSceneAnchors(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	var body struct {
		ChapterContent string `json:"chapter_content" binding:"required"`
		NovelTitle     string `json:"novel_title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	anchors, err := h.svc.ExtractFromChapter(c.Request.Context(), getTenantID(c), uint(novelID), body.NovelTitle, body.ChapterContent)
	if err != nil {
		log.Printf("[SceneAnchorHandler] ExtractSceneAnchors error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to extract scene anchors")
		return
	}
	respondOK(c, gin.H{"scene_anchors": anchors, "total": len(anchors)})
}

// LockRefImage PUT /scene-anchors/:id/ref-image
func (h *SceneAnchorHandler) LockRefImage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	var body struct {
		ImageURL string `json:"image_url" binding:"required"`
		ShotID   *uint  `json:"shot_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.svc.SetRefImage(uint(id), body.ImageURL, body.ShotID); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to lock ref image")
		return
	}
	respondOK(c, nil)
}

// GenerateRefImage POST /scene-anchors/:id/generate-ref-image
func (h *SceneAnchorHandler) GenerateRefImage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body) // optional body
	anchor, err := h.svc.GenerateRefImage(c.Request.Context(), getTenantID(c), uint(id), body.Provider)
	if err != nil {
		log.Printf("[SceneAnchorHandler] GenerateRefImage error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to generate ref image")
		return
	}
	respondOK(c, anchor)
}

// GetConsistencyLogs GET /scene-anchors/:id/consistency-logs
func (h *SceneAnchorHandler) GetConsistencyLogs(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid scene anchor id")
		return
	}
	logs, err := h.consistencySvc.GetLogsByAnchorID(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to get consistency logs")
		return
	}
	respondOK(c, gin.H{"logs": logs, "total": len(logs)})
}
