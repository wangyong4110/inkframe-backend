package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ScreenplayHandler 分场剧本处理器
type ScreenplayHandler struct {
	svc            *service.ScreenplayService
	chapterSvc     *service.ChapterService
	novelSvc       *service.NovelService
	videoSvc       *service.VideoService
	taskSvc        *service.TaskService
	characterSvc   *service.CharacterService
	itemSvc        *service.ItemService
	sceneAnchorSvc *service.SceneAnchorService
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

func (h *ScreenplayHandler) WithCharacterService(svc *service.CharacterService) *ScreenplayHandler {
	h.characterSvc = svc
	return h
}

func (h *ScreenplayHandler) WithItemService(svc *service.ItemService) *ScreenplayHandler {
	h.itemSvc = svc
	return h
}

func (h *ScreenplayHandler) WithSceneAnchorService(svc *service.SceneAnchorService) *ScreenplayHandler {
	h.sceneAnchorSvc = svc
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

// GenerateScreenplayFull POST /chapters/:id/screenplay/generate-full
// "生成剧本"按钮的一键管线：提取并绑定角色/道具/场景 → 重新生成分场剧本 → 异步生成分镜脚本。
// 提取步骤是 best-effort（失败只记日志不中断，它们只是丰富绑定数据，核心是后面两步）；
// 分镜生成本身耗时较长，沿用现有异步任务 + 轮询机制，不做成同步等待。
func (h *ScreenplayHandler) GenerateScreenplayFull(c *gin.Context) {
	if h.videoSvc == nil || h.taskSvc == nil {
		respondErr(c, http.StatusInternalServerError, "storyboard generation not available")
		return
	}
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkChapterTenant(c, uint(id)) {
		return
	}
	tenantID := getTenantID(c)
	chapterID := uint(id)
	chapter, err := h.chapterSvc.GetChapter(chapterID, tenantID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	var body struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&body)

	ctx := c.Request.Context()
	if h.characterSvc != nil {
		if _, err := h.characterSvc.AIExtractMinorChars(ctx, tenantID, chapter.NovelID, chapterID, ""); err != nil {
			reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplayFull: extract characters failed: %v", err)
		}
	}
	if h.itemSvc != nil {
		if _, err := h.itemSvc.AIExtractChapterItems(tenantID, chapter.NovelID, chapterID, ""); err != nil {
			reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplayFull: extract items failed: %v", err)
		}
	}
	if h.sceneAnchorSvc != nil {
		if _, err := h.sceneAnchorSvc.ExtractFromChapter(ctx, tenantID, chapter.NovelID, "", chapter.Content, chapterID, ""); err != nil {
			reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplayFull: extract scene anchors failed: %v", err)
		}
	}

	scenes, err := h.svc.GenerateScreenplayScenes(tenantID, chapterID, body.Provider, false)
	if err != nil {
		reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplayFull: generate screenplay failed: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to generate screenplay")
		return
	}

	video, err := h.videoSvc.GetVideoByChapterID(tenantID, chapterID)
	if err != nil {
		reqLogger(c).Errorf("[ScreenplayHandler] GenerateScreenplayFull: resolve video failed: %v", err)
		respondErr(c, http.StatusInternalServerError, "failed to resolve video project")
		return
	}

	h.taskSvc.CancelActiveByEntityAndInvoke("video", video.ID, service.TaskTypeStoryboardGen)
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeStoryboardGen, "分镜脚本生成", "video", video.ID, map[string]interface{}{
		"chapter_id": chapterID,
		"provider":   body.Provider,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create storyboard task")
		return
	}

	respondOK(c, gin.H{
		"scenes":             scenes,
		"video_id":           video.ID,
		"storyboard_task_id": task.TaskID,
	})
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

// GetSceneVersions GET /screenplay-scenes/:id/versions
func (h *ScreenplayHandler) GetSceneVersions(c *gin.Context) {
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
	versions, err := h.svc.GetSceneVersions(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, versions)
}

// RestoreSceneVersion POST /screenplay-scenes/:id/versions/:version_no/restore
func (h *ScreenplayHandler) RestoreSceneVersion(c *gin.Context) {
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
	versionNo, err := strconv.Atoi(c.Param("version_no"))
	if err != nil {
		respondBadRequest(c, "invalid version_no")
		return
	}
	updated, err := h.svc.RestoreSceneVersion(uint(id), versionNo)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, updated)
}
