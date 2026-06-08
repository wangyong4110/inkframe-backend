package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// characterToUpdateReq copies string fields from a Character into an
// UpdateCharacterRequest, preserving existing values before a partial update.
func characterToUpdateReq(c *model.Character) *model.UpdateCharacterRequest {
	return &model.UpdateCharacterRequest{
		Name:           c.Name,
		Role:           c.Role,
		Gender:         c.Gender,
		Age:            c.Age,
		Description:    c.Description,
		InnerConflict:  c.InnerConflict,
		CoreDesire:     c.CoreDesire,
		VisualPrompt:   c.VisualPrompt,
		Portrait:       c.Portrait,
		ThreeViewSheet: c.ThreeViewSheet,
		FaceCloseup:    c.FaceCloseup,
		VoiceID:        c.VoiceID,
		VoiceSpeed:     &c.VoiceSpeed,
		VoiceStyle:     c.VoiceStyle,
	}
}

// characterResponse converts a Character model to a response map.
func characterResponse(c *model.Character) gin.H {
	return gin.H{
		"id":               c.ID,
		"novel_id":         c.NovelID,
		"uuid":             c.UUID,
		"name":             c.Name,
		"role":             c.Role,
		"gender":           c.Gender,
		"age":              c.Age,
		"description":      c.Description,
		"visual_prompt":    c.VisualPrompt,
		"three_view_sheet": c.ThreeViewSheet,
		"face_closeup":     c.FaceCloseup,
		"portrait":         c.Portrait,
		"voice_id":         c.VoiceID,
		"voice_speed":      c.VoiceSpeed,
		"voice_style":      c.VoiceStyle,
		"voice_language":   c.VoiceLanguage,
		"voice_sample":     c.VoiceSample,
		"status":           c.Status,
		"created_at":       c.CreatedAt,
		"updated_at":       c.UpdatedAt,
	}
}

// CharacterHandler 角色处理器
type CharacterHandler struct {
	characterService *service.CharacterService
	arcService       *service.CharacterArcService
	imageGenService  *service.ImageGenerationService
	chapterSvc       *service.ChapterService
	storageSvc       storage.Service
	taskSvc          *service.TaskService
	aiService        *service.AIService
	novelService     *service.NovelService
	narrativeSvc     *service.NarrativeMemoryService
}

func NewCharacterHandler(
	characterService *service.CharacterService,
	arcService *service.CharacterArcService,
	imageGenService *service.ImageGenerationService,
) *CharacterHandler {
	return &CharacterHandler{
		characterService: characterService,
		arcService:       arcService,
		imageGenService:  imageGenService,
	}
}

func (h *CharacterHandler) WithAIService(svc *service.AIService) *CharacterHandler {
	h.aiService = svc
	return h
}

func (h *CharacterHandler) WithStorage(svc storage.Service) *CharacterHandler {
	h.storageSvc = svc
	return h
}

func (h *CharacterHandler) WithTaskService(svc *service.TaskService) *CharacterHandler {
	h.taskSvc = svc
	return h
}

func (h *CharacterHandler) WithChapterService(svc *service.ChapterService) *CharacterHandler {
	h.chapterSvc = svc
	return h
}

func (h *CharacterHandler) WithNovelService(svc *service.NovelService) *CharacterHandler {
	h.novelService = svc
	return h
}

func (h *CharacterHandler) WithNarrativeService(svc *service.NarrativeMemoryService) *CharacterHandler {
	h.narrativeSvc = svc
	return h
}

// checkNovelAccess verifies the novel exists and belongs to the current tenant.
func (h *CharacterHandler) checkNovelAccess(c *gin.Context, novelID uint) bool {
	if h.novelService == nil {
		return true // fallback: no service wired, allow (should not happen in production)
	}
	if _, err := h.novelService.GetNovel(novelID, getTenantID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "novel not found")
		return false
	}
	return true
}

// CreateCharacter 创建角色
// POST /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) CreateCharacter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !h.checkNovelAccess(c, uint(novelId)) {
		return
	}

	var req model.CreateCharacterRequest
	if !bindJSON(c, &req) {
		return
	}

	character, err := h.characterService.CreateCharacter(uint(novelId), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, characterResponse(character))
}

// GetCharacter 获取角色详情
// GET /api/v1/characters/:id
func (h *CharacterHandler) GetCharacter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	tenantID := getTenantID(c)
	if character.TenantID != tenantID {
		// 自愈：tenant_id=0 是历史数据缺陷，通过 novel 归属验证后修复
		if character.TenantID == 0 && h.novelService != nil {
			if _, e := h.novelService.GetNovel(character.NovelID, tenantID); e == nil {
				go h.characterService.FixTenantID(character.ID, tenantID)
				character.TenantID = tenantID
			} else {
				respondErr(c, http.StatusNotFound, "character not found")
				return
			}
		} else {
			respondErr(c, http.StatusNotFound, "character not found")
			return
		}
	}

	respondOK(c, characterResponse(character))
}

// ListCharacters 获取角色列表
// GET /api/v1/novels/:novel_id/characters
// 可选查询参数 role: protagonist | antagonist | supporting | extra
func (h *CharacterHandler) ListCharacters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if !h.checkNovelAccess(c, uint(novelId)) {
		return
	}

	role := c.Query("role")
	var (
		characters []*model.Character
		err        error
	)
	if role != "" {
		characters, err = h.characterService.ListByNovelFiltered(c.Request.Context(), uint(novelId), role)
	} else {
		characters, err = h.characterService.ListCharacters(uint(novelId))
	}
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]gin.H, 0, len(characters))
	for _, ch := range characters {
		resp = append(resp, characterResponse(ch))
	}
	respondOK(c, resp)
}

// UpdateCharacter 更新角色
// PUT /api/v1/characters/:id
func (h *CharacterHandler) UpdateCharacter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.UpdateCharacterRequest
	if !bindJSON(c, &req) {
		return
	}

	character, err := h.characterService.UpdateCharacter(uint(id), getTenantID(c), &req)
	if err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "character not found")
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, characterResponse(character))
}

// DeleteCharacter 删除角色
// DELETE /api/v1/characters/:id
func (h *CharacterHandler) DeleteCharacter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if err := h.characterService.DeleteCharacter(uint(id), getTenantID(c)); err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "character not found")
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// BatchDeleteCharacters 批量删除角色
// DELETE /api/v1/novels/:id/characters
func (h *CharacterHandler) BatchDeleteCharacters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkNovelAccess(c, uint(novelId)) {
		return
	}
	var req struct {
		IDs []uint `json:"ids" binding:"required,min=1"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.characterService.BatchDeleteCharacters(c.Request.Context(), uint(novelId), req.IDs); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"deleted": len(req.IDs)})
}

// GenerateCharacterImage 生成角色图像
// POST /api/v1/characters/:id/images
func (h *CharacterHandler) GenerateCharacterImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Type    string `json:"type"` // portrait, expression, pose
		Emotion string `json:"emotion,omitempty"`
		Action  string `json:"action,omitempty"`
		Style   string `json:"style,omitempty"`
	}
	if !bindJSON(c, &req) {
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	image, err := h.imageGenService.GenerateCharacterImage(&model.GenerateImageRequest{
		Subject:     character.Name,
		Description: character.Description,
		Type:        req.Type,
		Emotion:     req.Emotion,
		Action:      req.Action,
		Style:       req.Style,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, image)
}

// GenerateThreeView AI生成角色三视图合图（正视/侧视/背视放在同一张图中，异步任务）
// POST /api/v1/characters/:id/three-view
// 立即返回 202 + task_id，轮询任务接口获取结果
func (h *CharacterHandler) GenerateThreeView(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Style    string `json:"style,omitempty"`
		Provider string `json:"provider,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	tenantID := getTenantID(c)
	if character.TenantID != tenantID {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeThreeView, "角色三视图生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	novelTitle := h.characterService.GetNovelTitle(character.NovelID)
	// 优先使用请求中的 style；未指定时降级到小说项目设置的 image_style
	resolvedStyle := req.Style
	if resolvedStyle == "" {
		resolvedStyle = h.characterService.GetNovelImageStyle(character.NovelID)
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
		"style":    resolvedStyle,
	})

	go func(taskID string, charID uint, char *model.Character, style, provider string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		genCtx := context.Background()
		if novelTitle != "" {
			genCtx = service.WithImageStorageHint(genCtx, service.ImageStorageHint{NovelTitle: novelTitle})
		}
		// 生成三合一参考图（正视+侧视+背视放在同一张图中）
		// 优先使用 visual_prompt（图像生成专用提示词，含质量标签），降级使用 description
		sheetAppearance := char.VisualPrompt
		if sheetAppearance == "" {
			sheetAppearance = char.Description
		}
		// 三视图不传参考图：DreamO 模型会优先保留参考图的画风（包括旧风格），
		// 导致 style 变更后仍渲染旧风格。去掉参考图后走 Text2ImgV3 纯文本生成，
		// prompt 中的 %s风格 指令能完整生效。角色外形由 VisualPrompt 文本描述保障。
		sheetRef := ""
		sheetGender := service.InferGenderTag(char.VisualPrompt, char.Description)
		img, err := h.imageGenService.GenerateThreeViewSheet(genCtx, tenantID, char.Name, sheetAppearance, style, sheetGender, sheetRef, provider)
		if err != nil {
			logger.Errorf("[CharacterHandler] GenerateThreeView task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, "generate three-view sheet failed: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 99) //nolint:errcheck

		updateReq := characterToUpdateReq(char)
		updateReq.ThreeViewSheet = img.URL // ThreeViewSheet 存储三合一参考图

		updated, err := h.characterService.UpdateCharacter(charID, tenantID, updateReq)
		if err != nil {
			h.taskSvc.Fail(taskID, "save three-view sheet failed: "+err.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"character": updated,
			"generated": map[string]string{"sheet": img.URL},
		})
	}(task.TaskID, uint(id), character, resolvedStyle, req.Provider)

	respondAccepted(c, task.TaskID, "三视图生成任务已提交")
}

// GenerateFaceCloseup AI生成角色面部特写图（异步任务）
// POST /api/v1/characters/:id/face-closeup
func (h *CharacterHandler) GenerateFaceCloseup(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Style    string `json:"style,omitempty"`
		Provider string `json:"provider,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	tenantID := getTenantID(c)
	if character.TenantID != tenantID {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeFaceCloseup, "角色面部特写生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	novelTitle := h.characterService.GetNovelTitle(character.NovelID)
	// 优先使用请求中的 style；未指定时降级到小说项目设置的 image_style
	faceStyle := req.Style
	if faceStyle == "" {
		faceStyle = h.characterService.GetNovelImageStyle(character.NovelID)
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
		"style":    faceStyle,
	})

	go func(taskID string, charID uint, char *model.Character, style, provider string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		genCtx := context.Background()
		if novelTitle != "" {
			genCtx = service.WithImageStorageHint(genCtx, service.ImageStorageHint{NovelTitle: novelTitle})
		}
		// 使用肖像图作为参考（若有），保持面部一致性；推断性别以锁定性别 token
		referenceImage := char.Portrait
		if referenceImage == "" {
			referenceImage = char.ThreeViewSheet
		}
		// 优先使用 visual_prompt（图像生成专用提示词，含质量标签），降级使用 description
		faceAppearance := char.VisualPrompt
		if faceAppearance == "" {
			faceAppearance = char.Description
		}
		faceGender := service.InferGenderTag(char.VisualPrompt, char.Description)
		img, err := h.imageGenService.GenerateFaceCloseupImage(genCtx, tenantID, char.Name, faceAppearance, style, faceGender, referenceImage, provider)
		if err != nil {
			logger.Errorf("[CharacterHandler] GenerateFaceCloseup task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, "generate face closeup failed: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 99) //nolint:errcheck

		updateReq := characterToUpdateReq(char)
		updateReq.FaceCloseup = img.URL
		updateReq.Portrait = img.URL // face closeup doubles as portrait/avatar

		updated, err := h.characterService.UpdateCharacter(charID, tenantID, updateReq)
		if err != nil {
			h.taskSvc.Fail(taskID, "save face closeup failed: "+err.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"character": updated,
			"generated": map[string]string{"face_closeup": img.URL},
		})
	}(task.TaskID, uint(id), character, faceStyle, req.Provider)

	respondAccepted(c, task.TaskID, "面部特写生成任务已提交")
}

// UploadPortrait 上传角色肖像图片（远端 OSS 或本地存储兜底），用作三视图生成参考图
// POST /api/v1/characters/:id/portrait/upload
func (h *CharacterHandler) UploadPortrait(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	portraitURL, ok := receiveAndUpload(c, "portraits", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp"})
	if !ok {
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	updateReq := characterToUpdateReq(character)
	updateReq.Portrait = portraitURL
	updated, err := h.characterService.UpdateCharacter(uint(id), getTenantID(c), updateReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update portrait")
		return
	}

	respondOK(c, gin.H{"url": portraitURL, "character": updated})
}

// UploadCharacterImage 上传角色图片到指定字段
// POST /api/v1/characters/:id/image/upload?type=portrait|three_view|face_closeup
func (h *CharacterHandler) UploadCharacterImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	imgURL, ok := receiveAndUpload(c, "character-images", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp"})
	if !ok {
		return
	}
	updateReq := characterToUpdateReq(character)
	imgType := c.Query("type")
	switch imgType {
	case "three_view":
		updateReq.ThreeViewSheet = imgURL
	case "face_closeup":
		updateReq.FaceCloseup = imgURL
		updateReq.Portrait = imgURL
	default: // "portrait" or empty
		updateReq.Portrait = imgURL
	}
	updated, err := h.characterService.UpdateCharacter(uint(id), getTenantID(c), updateReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save image")
		return
	}
	respondOK(c, gin.H{"url": imgURL, "character": updated})
}

// UploadCharacterLookImage 上传角色形象图片到指定形象
// POST /api/v1/characters/:id/looks/:look_id/upload?type=portrait|three_view|face_closeup
func (h *CharacterHandler) UploadCharacterLookImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	lookID, ok := parseID(c, "look_id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	imgURL, ok := receiveAndUpload(c, "character-look-images", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp"})
	if !ok {
		return
	}
	imgType := c.Query("type")
	updateReq := &model.UpdateCharacterLookRequest{}
	switch imgType {
	case "three_view":
		updateReq.ThreeViewSheet = &imgURL
	case "face_closeup":
		updateReq.FaceCloseup = &imgURL
		updateReq.Portrait = &imgURL
	default: // "portrait" or empty
		updateReq.Portrait = &imgURL
		updateReq.FaceCloseup = &imgURL
	}
	look, err := h.characterService.UpdateLook(uint(lookID), updateReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save look image")
		return
	}
	respondOK(c, gin.H{"url": imgURL, "look": look})
}

// AIBatchGenerate AI批量生成/更新角色（异步任务）
// POST /api/v1/novels/:id/characters/ai-batch
func (h *CharacterHandler) AIBatchGenerate(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeCharGen, "批量生成角色", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[CharacterHandler] AIBatchGenerate task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		chars, err := h.characterService.AIBatchGenerate(tenantID, uint(novelID))
		if err != nil {
			logger.Errorf("[CharacterHandler] AIBatchGenerate task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"characters": chars, "count": len(chars)}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "角色批量生成任务已提交")
}

// BatchGenerateImages 批量为小说所有角色生成三视图合图图（异步任务）
// POST /api/v1/novels/:id/characters/batch-images
func (h *CharacterHandler) BatchGenerateImages(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Provider string `json:"provider"` // 可选：指定图像生成提供者
		Force    bool   `json:"force"`    // true=强制重新生成（风格变更时使用）
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, "invalid request: "+err.Error())
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeThreeView, "批量生成角色图片", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
		"force":    req.Force,
	})
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[CharacterHandler] BatchGenerateImages task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                           //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		succ, fail, err := h.characterService.BatchGenerateImages(tenantID, uint(novelID), req.Provider, req.Force, progressFn)
		if err != nil {
			logger.Errorf("[CharacterHandler] BatchGenerateImages task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
		} else {
			h.taskSvc.Complete(taskID, map[string]interface{}{"succeeded": succ, "failed": fail}) //nolint:errcheck
		}
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "角色图片批量生成任务已提交")
}

// GenerateCharacterProfile AI生成角色档案
// POST /api/v1/novels/:novel_id/characters/generate
func (h *CharacterHandler) GenerateCharacterProfile(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}

	character, err := h.characterService.GenerateProfile(getTenantID(c), uint(novelId), req.Description)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, character)
}

// GetCharacterArc 获取角色弧光
// GET /api/v1/novels/:novel_id/character-arcs/:character_id
func (h *CharacterHandler) GetCharacterArc(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	characterId, ok := parseID(c, "character_id")
	if !ok {
		return
	}

	if h.novelService != nil {
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
	}

	arc, err := h.arcService.GetCharacterArc(uint(novelId), uint(characterId))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character arc not found")
		return
	}

	respondOK(c, arc)
}

// GetAllCharacterArcs 获取所有角色弧光
// GET /api/v1/novels/:novel_id/character-arcs
func (h *CharacterHandler) GetAllCharacterArcs(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	if h.novelService != nil {
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
	}

	arcs, err := h.arcService.GetAllArcs(uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, arcs)
}

// UpdateCharacterArc 更新角色弧光
// PUT /api/v1/novels/:novel_id/character-arcs/:character_id
func (h *CharacterHandler) UpdateCharacterArc(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}
	characterId, ok := parseID(c, "character_id")
	if !ok {
		return
	}

	if h.novelService != nil {
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c)); err != nil {
			respondErr(c, http.StatusNotFound, "novel not found")
			return
		}
	}

	var req struct {
		CurrentStage int    `json:"current_stage"`
		Note         string `json:"note,omitempty"`
	}
	if !bindJSON(c, &req) {
		return
	}

	arc, err := h.arcService.UpdateArc(uint(novelId), uint(characterId), req.CurrentStage, req.Note)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, arc)
}

// AnalyzeCharacterConsistency 分析角色一致性
// POST /api/v1/characters/:id/analyze-consistency
func (h *CharacterHandler) AnalyzeCharacterConsistency(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req struct {
		Images []string `json:"images" binding:"required,min=1"`
	}
	if !bindJSON(c, &req) {
		return
	}

	result, err := h.characterService.AnalyzeConsistency(uint(id), req.Images)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, result)
}

// ListEffectiveCharacters GET /novels/:id/chapters/:chapter_no/characters
func (h *CharacterHandler) ListEffectiveCharacters(c *gin.Context) {
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
	chars, err := h.characterService.ListEffectiveCharacters(uint(novelID), chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, chars)
}

// UpsertChapterCharacter POST /novels/:id/chapters/:chapter_no/characters/:character_id
func (h *CharacterHandler) UpsertChapterCharacter(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	characterID, ok := parseID(c, "character_id")
	if !ok {
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	var req model.UpsertChapterCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if e := err.Error(); e != "EOF" && !strings.HasPrefix(e, "unexpected end") {
			respondBadRequest(c, "invalid request: "+e)
			return
		}
	}
	cc, err := h.characterService.UpsertChapterCharacter(uint(novelID), chapter.ID, uint(characterID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, cc)
}

// DeleteChapterCharacter DELETE /novels/:id/chapters/:chapter_no/characters/:character_id
func (h *CharacterHandler) DeleteChapterCharacter(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}
	characterID, ok := parseID(c, "character_id")
	if !ok {
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	if err := h.characterService.DeleteChapterCharacter(chapter.ID, uint(characterID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
}

// AIExtractMinorCharacters POST /novels/:id/chapters/:chapter_no/characters/ai-extract
func (h *CharacterHandler) AIExtractMinorCharacters(c *gin.Context) {
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
	chars, err := h.characterService.AIExtractMinorChars(getTenantID(c), uint(novelID), chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to extract minor characters: "+err.Error())
		return
	}
	respondOK(c, gin.H{"characters": chars, "new_count": len(chars)})
}

// ReanalyzeCharacter POST /api/v1/characters/:id/reanalyze
func (h *CharacterHandler) ReanalyzeCharacter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	char, err := h.characterService.ReanalyzeCharacter(getTenantID(c), uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "reanalyze failed: "+err.Error())
		return
	}
	respondOK(c, characterResponse(char))
}

// ExtractCharacterVoice 从小说章节中提取角色对话风格并写回角色的 VoiceStyle 字段
// POST /api/v1/characters/:id/extract-voice
func (h *CharacterHandler) ExtractCharacterVoice(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if h.narrativeSvc == nil {
		respondErr(c, http.StatusServiceUnavailable, "narrative service not configured")
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	var req struct {
		NovelID uint `json:"novel_id" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}

	voiceStyle, err := h.narrativeSvc.ExtractCharacterVoice(getTenantID(c), character, req.NovelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Persist the extracted voice style to the character's VoiceStyle field.
	updateReq := characterToUpdateReq(character)
	updateReq.VoiceStyle = voiceStyle
	updated, err := h.characterService.UpdateCharacter(uint(id), getTenantID(c), updateReq)
	if err != nil {
		logger.Errorf("[CharacterHandler] ExtractCharacterVoice: save voice style for char %d: %v", id, err)
		// Non-fatal: return the extracted style even if persisting failed.
		respondOK(c, gin.H{"voice_style": voiceStyle, "character_id": id, "saved": false})
		return
	}

	respondOK(c, gin.H{"voice_style": voiceStyle, "character_id": id, "saved": true, "character": characterResponse(updated)})
}

// PreviewVoice 试听角色声音
// POST /api/v1/characters/:id/voice/preview
func (h *CharacterHandler) PreviewVoice(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if h.aiService == nil {
		respondErr(c, http.StatusServiceUnavailable, "AI service not available")
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	var req struct {
		Text          string   `json:"text"`
		VoiceID       string   `json:"voice_id"`
		VoiceSpeed    *float64 `json:"voice_speed"`
		VoiceStyle    string   `json:"voice_style"`
		VoiceLanguage string   `json:"voice_language"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.Text == "" {
		req.Text = "大家好，我是" + character.Name + "，很高兴认识你们。"
	}

	// Use request params if provided; fall back to saved character values
	voice := req.VoiceID
	if voice == "" {
		voice = character.VoiceID
	}
	if voice == "" {
		voice = "alloy"
	}
	speed := 1.0
	if req.VoiceSpeed != nil {
		speed = *req.VoiceSpeed
	} else if character.VoiceSpeed > 0 {
		speed = character.VoiceSpeed
	}
	style := req.VoiceStyle
	if style == "" {
		style = character.VoiceStyle
	}
	lang := req.VoiceLanguage
	if lang == "" {
		lang = character.VoiceLanguage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rawURL, err := h.aiService.AudioGenerateWithOptions(ctx, getTenantID(c), req.Text, voice, speed, style, lang)
	if err != nil {
		logger.Errorf("PreviewVoice: TTS generation failed for character %d voice=%q: %v", id, voice, err)
		respondErr(c, http.StatusInternalServerError, "voice generation failed: "+err.Error())
		return
	}

	// For local file:// URLs, encode as base64 data URL so the browser plays it
	// inline without depending on the tmp file persisting across requests.
	// For remote URLs (CDN), pass them through directly.
	playURL := rawURL
	if len(rawURL) > 7 && rawURL[:7] == "file://" {
		filePath := rawURL[7:]
		if data, readErr := os.ReadFile(filePath); readErr == nil && len(data) > 0 {
			playURL = "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(data)
		} else {
			// Fallback to sample endpoint if file cannot be read.
			playURL = "/api/v1/characters/" + c.Param("id") + "/voice/sample?t=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		}
	}
	h.characterService.UpdateCharacter(uint(id), getTenantID(c), &model.UpdateCharacterRequest{ //nolint:errcheck
		Name:        character.Name,
		VoiceSample: rawURL,
	})

	respondOK(c, gin.H{"audio_url": playURL, "voice_id": voice, "voice_speed": speed})
}

// ServeVoiceSample 播放角色声音样本（file:// 路径转 HTTP 流）
// GET /api/v1/characters/:id/voice/sample
func (h *CharacterHandler) ServeVoiceSample(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || character.VoiceSample == "" {
		respondErr(c, http.StatusNotFound, "no voice sample available")
		return
	}
	if character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "no voice sample available")
		return
	}
	filePath := character.VoiceSample
	if len(filePath) > 7 && filePath[:7] == "file://" {
		filePath = filePath[7:]
	}
	c.Header("Content-Type", "audio/mpeg")
	c.Header("Cache-Control", "public, max-age=86400")
	c.File(filePath)
}

// ListCharacterSnapshots GET /characters/:id/snapshots
func (h *CharacterHandler) ListCharacterSnapshots(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	snapshots, err := h.characterService.ListCharacterSnapshots(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"snapshots": snapshots, "total": len(snapshots)})
}

// CreateCharacterSnapshot POST /characters/:id/snapshots
func (h *CharacterHandler) CreateCharacterSnapshot(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || character.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	var req struct {
		Motivation string `json:"motivation"`
		Mood       string `json:"mood"`
	}
	_ = c.ShouldBindJSON(&req)
	snap, err := h.characterService.CreateCharacterSnapshot(uint(id), req.Motivation, req.Mood)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, snap)
}

// ─── CharacterLook handlers ───────────────────────────────────────────────────

// ListCharacterLooks GET /characters/:id/looks
func (h *CharacterHandler) ListCharacterLooks(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	looks, err := h.characterService.ListLooks(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"looks": looks, "total": len(looks)})
}

// CreateCharacterLook POST /characters/:id/looks
func (h *CharacterHandler) CreateCharacterLook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	var req model.CreateCharacterLookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	look, err := h.characterService.CreateLook(uint(id), char.NovelID, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, look)
}

// UpdateCharacterLook PUT /characters/:id/looks/:look_id
func (h *CharacterHandler) UpdateCharacterLook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	lookID, ok := parseID(c, "look_id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	var req model.UpdateCharacterLookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	look, err := h.characterService.UpdateLook(uint(lookID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, look)
}

// DeleteCharacterLook DELETE /characters/:id/looks/:look_id
func (h *CharacterHandler) DeleteCharacterLook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	lookID, ok := parseID(c, "look_id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	_ = char
	if err := h.characterService.DeleteLook(uint(lookID)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "deleted"})
}

// GetActiveLook GET /characters/:id/looks/active?chapter_no=N
func (h *CharacterHandler) GetActiveLook(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	chapterNo, _ := strconv.Atoi(c.Query("chapter_no"))
	look, err := h.characterService.GetActiveLook(uint(id), chapterNo)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if look == nil {
		respondOK(c, gin.H{"look": nil})
		return
	}
	respondOK(c, gin.H{"look": look})
}

// GenerateLookVisualPrompt POST /characters/:id/looks/generate-prompt
func (h *CharacterHandler) GenerateLookVisualPrompt(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	prompt, err := h.characterService.GenerateLookVisualPrompt(getTenantID(c), uint(id), req.Description)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"visual_prompt": prompt})
}

// GenerateLookImages POST /characters/:id/looks/:look_id/images
func (h *CharacterHandler) GenerateLookImages(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	lookID, ok := parseID(c, "look_id")
	if !ok {
		return
	}
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || char.TenantID != getTenantID(c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	look, err := h.characterService.GetLook(uint(lookID))
	if err != nil {
		respondErr(c, http.StatusNotFound, "look not found")
		return
	}
	var req struct {
		Type     string `json:"type"`     // "three_view" | "face_closeup" | "portrait"
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	tenantID := getTenantID(c)
	visualPrompt := look.VisualPrompt
	if visualPrompt == "" {
		visualPrompt = char.VisualPrompt
	}

	style := h.characterService.GetNovelImageStyle(char.NovelID)

	var imageURL string
	switch req.Type {
	case "face_closeup", "portrait", "":
		img, err := h.imageGenService.GenerateFaceCloseupImage(c.Request.Context(), tenantID, char.Name, visualPrompt, style, "", look.Portrait, req.Provider)
		if err != nil {
			respondErr(c, http.StatusInternalServerError, err.Error())
			return
		}
		imageURL = img.URL
		updateReq := &model.UpdateCharacterLookRequest{FaceCloseup: &imageURL, Portrait: &imageURL}
		look, _ = h.characterService.UpdateLook(uint(lookID), updateReq)
	case "three_view":
		img, err := h.imageGenService.GenerateThreeViewSheet(c.Request.Context(), tenantID, char.Name, visualPrompt, style, "", look.ThreeViewSheet, req.Provider)
		if err != nil {
			respondErr(c, http.StatusInternalServerError, err.Error())
			return
		}
		imageURL = img.URL
		updateReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &imageURL}
		look, _ = h.characterService.UpdateLook(uint(lookID), updateReq)
	default:
		respondBadRequest(c, "type must be 'three_view', 'face_closeup', or 'portrait'")
		return
	}
	respondOK(c, look)
}

// GenerateChapterCharacterImages POST /api/v1/novels/:id/chapters/:chapter_no/characters/generate-images
// 根据章节内容为选定角色生成形象图（三视图），先用 AI 生成章节外形补充说明再合并生成。
// 异步任务，立即返回 task_id。
func (h *CharacterHandler) GenerateChapterCharacterImages(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	chapterNo, err := strconv.Atoi(c.Param("chapter_no"))
	if err != nil {
		respondBadRequest(c, "invalid chapter_no")
		return
	}

	var req struct {
		CharacterIDs []uint `json:"character_ids"`
		Provider     string `json:"provider,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}
	if len(req.CharacterIDs) == 0 {
		respondBadRequest(c, "character_ids is required")
		return
	}

	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterReview, "章节角色形象生成", "chapter", chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	go func(taskID string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		ctx := context.Background()
		succeeded, failed, genErr := h.characterService.GenerateChapterImages(
			ctx, tenantID, uint(novelID), chapter, req.CharacterIDs, req.Provider,
			func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) }, //nolint:errcheck
		)
		if genErr != nil {
			h.taskSvc.Fail(taskID, genErr.Error()) //nolint:errcheck
			return
		}
		if failed > 0 && succeeded == 0 {
			h.taskSvc.Fail(taskID, fmt.Sprintf("all %d character image generations failed", failed)) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"succeeded": succeeded,
			"failed":    failed,
		})
	}(task.TaskID)

	respondAccepted(c, task.TaskID, "角色形象生成任务已提交")
}
