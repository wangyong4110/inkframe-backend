package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ScreenplayHandler 分场剧本处理器
type ScreenplayHandler struct {
	svc        *service.ScreenplayService
	chapterSvc *service.ChapterService
	novelSvc   *service.NovelService
	videoSvc   *service.VideoService
	taskSvc    *service.TaskService
}

func NewScreenplayHandler(svc *service.ScreenplayService, chapterSvc *service.ChapterService, novelSvc *service.NovelService) *ScreenplayHandler {
	return &ScreenplayHandler{svc: svc, chapterSvc: chapterSvc, novelSvc: novelSvc}
}

func (h *ScreenplayHandler) WithVideoService(svc *service.VideoService) *ScreenplayHandler {
	h.videoSvc = svc
	return h
}

func (h *ScreenplayHandler) WithTaskService(svc *service.TaskService) *ScreenplayHandler {
	h.taskSvc = svc
	return h
}

// checkChapterTenant 校验章节所属小说归属当前租户。返回 false 时已写入错误响应。
func (h *ScreenplayHandler) checkChapterTenant(c *gin.Context, chapterID uint) bool {
	if h.chapterSvc == nil || h.novelSvc == nil {
		return true // 未注入时跳过检查（兼容测试）
	}
	chapter, err := h.chapterSvc.GetChapter(chapterID, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return false
	}
	if _, err := h.novelSvc.GetNovel(chapter.NovelID, getTenantID(c), getUserID(c)); err != nil {
		respondErr(c, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// GenerateScreenplay POST /chapters/:id/screenplay/generate
func (h *ScreenplayHandler) GenerateScreenplay(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkChapterTenant(c, uint(id)) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body)
	// 用户在界面上显式点击"重新生成剧本"：preserveEdited=false，只保留锁定场次，
	// 未锁定场次（即使已手动编辑过）会被覆盖——这是用户主动要求的操作，语义上就该覆盖。
	scenes, err := h.svc.GenerateScreenplayScenes(getTenantID(c), uint(id), body.Provider, false)
	if err != nil {
		reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplay error: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to generate screenplay")
		return
	}
	respondOK(c, scenes)
}

// ListScreenplayScenes GET /chapters/:id/screenplay
func (h *ScreenplayHandler) ListScreenplayScenes(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkChapterTenant(c, uint(id)) {
		return
	}
	scenes, err := h.svc.ListScenes(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, scenes)
}

// UpdateScreenplayScene PUT /screenplay-scenes/:id
func (h *ScreenplayHandler) UpdateScreenplayScene(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	scene, err := h.svc.GetScene(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "screenplay scene not found")
		return
	}
	if !h.checkChapterTenant(c, scene.ChapterID) {
		return
	}
	var body struct {
		Heading       *string `json:"heading"`
		Synopsis      *string `json:"synopsis"`
		EmotionalTone *string `json:"emotional_tone"`
		Beats         *string `json:"beats"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "invalid body")
		return
	}
	fields := map[string]interface{}{}
	if body.Heading != nil {
		fields["heading"] = *body.Heading
	}
	if body.Synopsis != nil {
		fields["synopsis"] = *body.Synopsis
	}
	if body.EmotionalTone != nil {
		fields["emotional_tone"] = *body.EmotionalTone
	}
	if body.Beats != nil {
		fields["beats"] = *body.Beats
	}
	updated, err := h.svc.UpdateScene(uint(id), fields)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, updated)
}

// LockScreenplayScene PUT /screenplay-scenes/:id/lock
func (h *ScreenplayHandler) LockScreenplayScene(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	scene, err := h.svc.GetScene(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "screenplay scene not found")
		return
	}
	if !h.checkChapterTenant(c, scene.ChapterID) {
		return
	}
	var body struct {
		Locked bool `json:"locked"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, "invalid body")
		return
	}
	updated, err := h.svc.SetLocked(uint(id), body.Locked)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, updated)
}

// RegenerateSceneStoryboard POST /screenplay-scenes/:id/regenerate-storyboard
// 只重新生成本场次对应的分镜（不影响本视频其它场次），保存分场剧本时若勾选
// "更新分镜脚本" 由前端调用；异步任务，执行体见 cmd/server/task_resume.go 里
// service.TaskTypeStoryboardSceneRegen 对应的 resume handler。
func (h *ScreenplayHandler) RegenerateSceneStoryboard(c *gin.Context) {
	if h.videoSvc == nil || h.taskSvc == nil {
		respondErr(c, http.StatusInternalServerError, "storyboard regeneration not available")
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	sceneID := uint(id)
	scene, err := h.svc.GetScene(sceneID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "screenplay scene not found")
		return
	}
	if !h.checkChapterTenant(c, scene.ChapterID) {
		return
	}

	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body)

	tenantID := getTenantID(c)
	videoID, err := h.videoSvc.GetVideoIDForScreenplayScene(tenantID, sceneID)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}

	h.taskSvc.CancelActiveByEntityAndInvoke("video", videoID, service.TaskTypeStoryboardSceneRegen)
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeStoryboardSceneRegen, "单场次分镜重新生成", "video", videoID, map[string]interface{}{
		"scene_id": sceneID,
		"provider": body.Provider,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	respondAccepted(c, task.TaskID, "分镜重新生成任务已提交")
}

// DeleteScreenplayScene DELETE /screenplay-scenes/:id
func (h *ScreenplayHandler) DeleteScreenplayScene(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	scene, err := h.svc.GetScene(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "screenplay scene not found")
		return
	}
	if !h.checkChapterTenant(c, scene.ChapterID) {
		return
	}
	if err := h.svc.DeleteScene(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}
