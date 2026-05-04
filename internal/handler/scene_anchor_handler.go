package handler

import (
	"context"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SceneAnchorHandler 场景锚点处理器
type SceneAnchorHandler struct {
	svc            *service.SceneAnchorService
	consistencySvc *service.SceneConsistencyService
	taskSvc        *service.TaskService
	chapterSvc     *service.ChapterService
}

func NewSceneAnchorHandler(svc *service.SceneAnchorService, consistencySvc *service.SceneConsistencyService) *SceneAnchorHandler {
	return &SceneAnchorHandler{svc: svc, consistencySvc: consistencySvc}
}

func (h *SceneAnchorHandler) WithTaskService(svc *service.TaskService) *SceneAnchorHandler {
	h.taskSvc = svc
	return h
}

func (h *SceneAnchorHandler) WithChapterService(svc *service.ChapterService) *SceneAnchorHandler {
	h.chapterSvc = svc
	return h
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

// AIExtractChapterAnchors POST /novels/:id/chapters/:chapter_no/scene-anchors/ai-extract
func (h *SceneAnchorHandler) AIExtractChapterAnchors(c *gin.Context) {
	if h.chapterSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "chapter service not configured")
		return
	}
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	content := chapter.Content
	if content == "" {
		content = chapter.Summary
	}
	if content == "" {
		respondBadRequest(c, "chapter has no content")
		return
	}
	anchors, err := h.svc.ExtractFromChapter(context.Background(), getTenantID(c), uint(novelID), "", content)
	if err != nil {
		log.Printf("[SceneAnchorHandler] AIExtractChapterAnchors: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to extract chapter scene anchors")
		return
	}
	respondOK(c, gin.H{"scene_anchors": anchors, "total": len(anchors)})
}

// BatchGenerateRefImages POST /novels/:id/scene-anchors/batch-ref-images
// 批量为小说所有场景锚点生成参考图（跳过已有参考图的锚点，异步任务）
func (h *SceneAnchorHandler) BatchGenerateRefImages(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body)
	tenantID := getTenantID(c)
	if h.taskSvc == nil {
		respondErr(c, http.StatusInternalServerError, "task service not configured")
		return
	}
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImageGen, "批量生成场景参考图", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		succ, fail, err := h.svc.BatchGenerateRefImages(context.Background(), tenantID, uint(novelID), body.Provider, progressFn)
		if err != nil {
			log.Printf("[SceneAnchorHandler] BatchGenerateRefImages task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "场景参考图批量生成任务已提交")
}

// AIExtractFromNovel POST /novels/:id/scene-anchors/ai-extract
// 异步批量提取小说所有章节的场景锚点
func (h *SceneAnchorHandler) AIExtractFromNovel(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}
	tenantID := getTenantID(c)
	if h.taskSvc == nil {
		respondErr(c, http.StatusInternalServerError, "task service not configured")
		return
	}
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeSceneAnchorExtract, "AI提取场景锚点", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		anchors, err := h.svc.AIExtractAllFromNovel(tenantID, uint(novelID), progressFn)
		if err != nil {
			log.Printf("[SceneAnchorHandler] AIExtractFromNovel: %v", err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"scene_anchors": anchors, "count": len(anchors)}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "场景锚点提取任务已提交")
}
