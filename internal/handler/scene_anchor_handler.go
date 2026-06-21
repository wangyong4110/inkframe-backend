package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SceneAnchorHandler 场景锚点处理器
type SceneAnchorHandler struct {
	svc            *service.SceneAnchorService
	consistencySvc *service.SceneConsistencyService
	taskSvc        *service.TaskService
	chapterSvc     *service.ChapterService
	videoSvc       *service.VideoService
	novelSvc       *service.NovelService
	storageSvc     storage.Service
}

func NewSceneAnchorHandler(svc *service.SceneAnchorService, consistencySvc *service.SceneConsistencyService) *SceneAnchorHandler {
	return &SceneAnchorHandler{svc: svc, consistencySvc: consistencySvc}
}

func (h *SceneAnchorHandler) WithStorageService(svc storage.Service) *SceneAnchorHandler {
	h.storageSvc = svc
	return h
}

func (h *SceneAnchorHandler) WithTaskService(svc *service.TaskService) *SceneAnchorHandler {
	h.taskSvc = svc
	return h
}

func (h *SceneAnchorHandler) WithChapterService(svc *service.ChapterService) *SceneAnchorHandler {
	h.chapterSvc = svc
	return h
}

func (h *SceneAnchorHandler) WithVideoService(svc *service.VideoService) *SceneAnchorHandler {
	h.videoSvc = svc
	return h
}

// WithNovelService 注入小说服务（用于 ListSceneAnchors 时验证小说归属租户）
func (h *SceneAnchorHandler) WithNovelService(svc *service.NovelService) *SceneAnchorHandler {
	h.novelSvc = svc
	return h
}

// checkNovelTenant 校验小说归属当前租户。
// 返回 false 时已写入错误响应。
func (h *SceneAnchorHandler) checkNovelTenant(c *gin.Context, novelID uint) bool {
	if h.novelSvc == nil {
		return true // novelSvc 未注入时跳过检查（兼容测试）
	}
	if _, err := h.novelSvc.GetNovel(novelID, getTenantID(c), getUserIDFromCtx(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return false
	}
	return true
}

// GetSceneAnchor GET /scene-anchors/:id
func (h *SceneAnchorHandler) GetSceneAnchor(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	anchor, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, anchor.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	respondOK(c, anchor)
}

// ListSceneAnchors GET /novels/:id/scene-anchors
func (h *SceneAnchorHandler) ListSceneAnchors(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelTenant(c, uint(novelID)) {
		return
	}
	anchors, err := h.svc.ListByNovel(uint(novelID))
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] ListSceneAnchors novelID=%d: %v", novelID, err)
		respondErr(c, http.StatusInternalServerError, "failed to list scene anchors")
		return
	}
	respondOK(c, gin.H{"scene_anchors": anchors, "total": len(anchors)})
}

// CreateSceneAnchor POST /novels/:id/scene-anchors
func (h *SceneAnchorHandler) CreateSceneAnchor(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req service.CreateSceneAnchorReq
	if !bindJSON(c, &req) {
		return
	}
	anchor, err := h.svc.Create(getTenantID(c), uint(novelID), req)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] CreateSceneAnchor novelID=%d name=%q: %v", novelID, req.Name, err)
		respondErr(c, http.StatusInternalServerError, "failed to create scene anchor")
		return
	}
	respondCreated(c, anchor)
}

// UpdateSceneAnchor PUT /scene-anchors/:id
func (h *SceneAnchorHandler) UpdateSceneAnchor(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var req service.UpdateSceneAnchorReq
	if !bindJSON(c, &req) {
		return
	}
	logger.Printf("[SceneAnchorHandler] UpdateSceneAnchor id=%d: HTTP body parsed: description_len=%d description_preview=%.120q", id, len(req.Description), req.Description)
	anchor, err := h.svc.Update(uint(id), req)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] UpdateSceneAnchor id=%d: %v", id, err)
		respondErr(c, http.StatusInternalServerError, "failed to update scene anchor")
		return
	}
	logger.Printf("[SceneAnchorHandler] UpdateSceneAnchor id=%d: response description_len=%d", id, len(anchor.Description))
	respondOK(c, anchor)
}

// DeleteSceneAnchor DELETE /scene-anchors/:id
func (h *SceneAnchorHandler) DeleteSceneAnchor(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.svc.Delete(uint(id)); err != nil {
		logger.Errorf("[SceneAnchorHandler] DeleteSceneAnchor id=%d: %v", id, err)
		respondErr(c, http.StatusInternalServerError, "failed to delete scene anchor")
		return
	}
	respondOK(c, nil)
}

// SetShotAnchor PUT /videos/:video_id/shots/:shot_id/anchor
func (h *SceneAnchorHandler) SetShotAnchor(c *gin.Context) {
	videoID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if h.videoSvc != nil {
		if _, err := h.videoSvc.GetVideoByTenant(uint(videoID), getTenantID(c)); err != nil {
			respondErr(c, http.StatusNotFound, "video not found")
			return
		}
	}
	shotID, ok := parseID(c, "shot_id")
	if !ok {
		return
	}
	var body struct {
		AnchorID *uint `json:"anchor_id"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if err := h.svc.SetShotAnchor(uint(shotID), body.AnchorID); err != nil {
		logger.Errorf("[SceneAnchorHandler] SetShotAnchor shotID=%d anchorID=%v: %v", shotID, body.AnchorID, err)
		respondErr(c, http.StatusInternalServerError, "failed to set shot anchor")
		return
	}
	respondOK(c, nil)
}

// ExtractSceneAnchors POST /novels/:id/scene-anchors/extract
func (h *SceneAnchorHandler) ExtractSceneAnchors(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var body struct {
		ChapterContent string `json:"chapter_content" binding:"required"`
		NovelTitle     string `json:"novel_title"`
	}
	if !bindJSON(c, &body) {
		return
	}
	anchors, err := h.svc.ExtractFromChapter(c.Request.Context(), getTenantID(c), uint(novelID), body.NovelTitle, body.ChapterContent, 0, "")
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] ExtractSceneAnchors error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to extract scene anchors")
		return
	}
	respondOK(c, gin.H{"scene_anchors": anchors, "total": len(anchors)})
}

// LockRefImage PUT /scene-anchors/:id/ref-image
func (h *SceneAnchorHandler) LockRefImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		ImageURL string `json:"image_url" binding:"required"`
		ShotID   *uint  `json:"shot_id"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if err := h.svc.SetRefImage(uint(id), body.ImageURL, body.ShotID); err != nil {
		logger.Errorf("[SceneAnchorHandler] LockRefImage id=%d: %v", id, err)
		respondErr(c, http.StatusInternalServerError, "failed to lock ref image")
		return
	}
	respondOK(c, nil)
}

// UploadRefImage POST /scene-anchors/:id/ref-image/upload
// 上传本地图片作为场景参考图，上传后自动调用 SetRefImage 锁定。
func (h *SceneAnchorHandler) UploadRefImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	imgURL, ok := receiveAndUpload(c, "scene-ref-images", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp"})
	if !ok {
		return
	}
	if err := h.svc.SetRefImage(uint(id), imgURL, nil); err != nil {
		logger.Errorf("[SceneAnchorHandler] UploadRefImage id=%d: %v", id, err)
		respondErr(c, http.StatusInternalServerError, "failed to save ref image")
		return
	}
	updated, _ := h.svc.Get(uint(id))
	respondOK(c, gin.H{"url": imgURL, "anchor": updated})
}

// AIAnalyzeSceneAnchor POST /scene-anchors/:id/ai-analyze
// 使用 AI 分析场景，返回建议的 type / description / variant，不自动保存。
func (h *SceneAnchorHandler) AIAnalyzeSceneAnchor(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	result, err := h.svc.AIAnalyze(c.Request.Context(), getTenantID(c), uint(id))
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] AIAnalyzeSceneAnchor id=%d: %v", id, err)
		respondErr(c, http.StatusInternalServerError, "AI分析失败："+err.Error())
		return
	}
	respondOK(c, result)
}

// GenerateRefImage POST /scene-anchors/:id/generate-ref-image
func (h *SceneAnchorHandler) GenerateRefImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body) // optional body
	anchor, err := h.svc.GenerateRefImage(c.Request.Context(), getTenantID(c), uint(id), body.Provider)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] GenerateRefImage error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to generate ref image")
		return
	}
	respondOK(c, anchor)
}

// EditRefImage POST /scene-anchors/:id/edit-ref-image
func (h *SceneAnchorHandler) EditRefImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, existing.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	var body struct {
		Instruction string `json:"instruction" binding:"required"`
	}
	if !bindJSON(c, &body) {
		return
	}
	anchor, err := h.svc.EditRefImageWithInstruction(c.Request.Context(), getTenantID(c), uint(id), body.Instruction)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] EditRefImage error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to edit ref image")
		return
	}
	respondOK(c, anchor)
}

// GetConsistencyLogs GET /scene-anchors/:id/consistency-logs
func (h *SceneAnchorHandler) GetConsistencyLogs(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	anchor, err := h.svc.Get(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "scene anchor not found")
		return
	}
	if !h.checkNovelTenant(c, anchor.NovelID) {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}
	logs, err := h.consistencySvc.GetLogsByAnchorID(uint(id))
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] GetConsistencyLogs anchorID=%d: %v", id, err)
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
	novelID, ok := parseID(c, "id")
	if !ok {
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

	var body struct {
		UserPrompt string `json:"user_prompt"`
	}
	_ = c.ShouldBindJSON(&body)

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterSceneExtract, "场景分析", "chapter", chapter.ID)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] AIExtractChapterAnchors create task novelID=%d chapterNo=%d: %v", novelID, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"novel_id":   novelID,
		"chapter_no": chapterNo,
		"content":    content,
	})

	go func(taskID string, tID, nID, chapID uint, chContent, userPrompt string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[SceneAnchorHandler] AIExtractChapterAnchors task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		logger.Printf("[SceneAnchorHandler] AIExtractChapterAnchors task %s started: novelID=%d chapterID=%d contentLen=%d", taskID, nID, chapID, len(chContent))
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		anchors, err := h.svc.ExtractFromChapter(ctx, tID, nID, "", chContent, chapID, userPrompt)
		if err != nil {
			logger.Errorf("[SceneAnchorHandler] AIExtractChapterAnchors task %s failed: novelID=%d chapterID=%d err=%v", taskID, nID, chapID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			return
		}
		logger.Printf("[SceneAnchorHandler] AIExtractChapterAnchors task %s completed: novelID=%d chapterID=%d newAnchors=%d", taskID, nID, chapID, len(anchors))
		h.taskSvc.Complete(taskID, map[string]interface{}{"new_count": len(anchors)}) //nolint:errcheck
	}(task.TaskID, tenantID, uint(novelID), chapter.ID, content, body.UserPrompt)

	respondAccepted(c, task.TaskID, "场景分析任务已提交")
}

// ListChapterAnchors GET /novels/:id/chapters/:chapter_no/scene-anchors
func (h *SceneAnchorHandler) ListChapterAnchors(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
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
	anchors, err := h.svc.ListChapterAnchors(uint(novelID), chapter.ID)
	if err != nil {
		logger.Errorf("[SceneAnchorHandler] ListChapterAnchors novelID=%d chapterNo=%d: %v", novelID, chapterNo, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, anchors)
}

// BindChapterAnchor PUT /novels/:id/chapters/:chapter_no/scene-anchors/:anchor_id
func (h *SceneAnchorHandler) BindChapterAnchor(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	anchorID, ok2 := parseID(c, "anchor_id")
	if !ok2 {
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if err := h.svc.BindChapterAnchor(chapter.ID, uint(novelID), uint(anchorID)); err != nil {
		logger.Errorf("[SceneAnchorHandler] BindChapterAnchor chapterID=%d novelID=%d anchorID=%d: %v", chapter.ID, novelID, anchorID, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// UnbindChapterAnchor DELETE /novels/:id/chapters/:chapter_no/scene-anchors/:anchor_id
func (h *SceneAnchorHandler) UnbindChapterAnchor(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	anchorID, ok2 := parseID(c, "anchor_id")
	if !ok2 {
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if err := h.svc.UnbindChapterAnchor(chapter.ID, uint(anchorID)); err != nil {
		logger.Errorf("[SceneAnchorHandler] UnbindChapterAnchor chapterID=%d anchorID=%d: %v", chapter.ID, anchorID, err)
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// BatchGenerateRefImages POST /novels/:id/scene-anchors/batch-ref-images
// 批量为小说所有场景锚点生成参考图（跳过已有参考图的锚点，异步任务）
func (h *SceneAnchorHandler) BatchGenerateRefImages(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Force    bool   `json:"force"` // true=强制重新生成（风格变更时使用）
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
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"source":   "scene_anchor_batch",
		"provider": body.Provider,
		"force":    body.Force,
	})
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[SceneAnchorHandler] BatchGenerateRefImages task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                           //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		succ, fail, err := h.svc.BatchGenerateRefImages(context.Background(), tenantID, uint(novelID), body.Provider, body.Force, progressFn)
		if err != nil {
			logger.Errorf("[SceneAnchorHandler] BatchGenerateRefImages task %s failed: %v", taskID, err)
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
	novelID, ok := parseID(c, "id")
	if !ok {
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
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[SceneAnchorHandler] AIExtractFromNovel task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		anchors, err := h.svc.AIExtractAllFromNovel(context.Background(), tenantID, uint(novelID), progressFn)
		if err != nil {
			logger.Errorf("[SceneAnchorHandler] AIExtractFromNovel: %v", err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"scene_anchors": anchors, "count": len(anchors)}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "场景锚点提取任务已提交")
}
