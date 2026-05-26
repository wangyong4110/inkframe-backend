package handler

import (
	"context"
	"net/http"
	"strconv"
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
		Description:    c.Description,
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

// CreateCharacter 创建角色
// POST /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) CreateCharacter(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
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
	if tenantID := getTenantID(c); character.TenantID != tenantID {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	respondOK(c, characterResponse(character))
}

// ListCharacters 获取角色列表
// GET /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) ListCharacters(c *gin.Context) {
	novelId, ok := parseID(c, "id")
	if !ok {
		return
	}

	characters, err := h.characterService.ListCharacters(uint(novelId))
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

	existing, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if tenantID := getTenantID(c); existing.TenantID != tenantID {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	var req model.UpdateCharacterRequest
	if !bindJSON(c, &req) {
		return
	}

	character, err := h.characterService.UpdateCharacter(uint(id), &req)
	if err != nil {
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

	existing, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if tenantID := getTenantID(c); existing.TenantID != tenantID {
		respondErr(c, http.StatusForbidden, "forbidden")
		return
	}

	if err := h.characterService.DeleteCharacter(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, nil)
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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeThreeView, "角色三视图生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	novelTitle := h.characterService.GetNovelTitle(character.NovelID)

	go func(taskID string, charID uint, char *model.Character, style, provider string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		genCtx := context.Background()
		if novelTitle != "" {
			genCtx = service.WithImageStorageHint(genCtx, service.ImageStorageHint{NovelTitle: novelTitle})
		}
		// 生成三合一参考图（正视+侧视+背视放在同一张图中）
		img, err := h.imageGenService.GenerateThreeViewSheet(genCtx, tenantID, char.Name, char.Description, style, "", "", provider)
		if err != nil {
			logger.Printf("[CharacterHandler] GenerateThreeView task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, "generate three-view sheet failed: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 99) //nolint:errcheck

		updateReq := characterToUpdateReq(char)
		updateReq.ThreeViewSheet = img.URL // ThreeViewSheet 存储三合一参考图

		updated, err := h.characterService.UpdateCharacter(charID, updateReq)
		if err != nil {
			h.taskSvc.Fail(taskID, "save three-view sheet failed: "+err.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"character": updated,
			"generated": map[string]string{"sheet": img.URL},
		})
	}(task.TaskID, uint(id), character, req.Style, req.Provider)

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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeFaceCloseup, "角色面部特写生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	novelTitle := h.characterService.GetNovelTitle(character.NovelID)

	go func(taskID string, charID uint, char *model.Character, style, provider string) {
		h.taskSvc.SetRunning(taskID) //nolint:errcheck

		genCtx := context.Background()
		if novelTitle != "" {
			genCtx = service.WithImageStorageHint(genCtx, service.ImageStorageHint{NovelTitle: novelTitle})
		}
		// 使用肖像图作为参考（若有），保持面部一致性
		referenceImage := char.Portrait
		if referenceImage == "" {
			referenceImage = char.ThreeViewSheet
		}
		img, err := h.imageGenService.GenerateFaceCloseupImage(genCtx, tenantID, char.Name, char.Description, style, "", referenceImage, provider)
		if err != nil {
			logger.Printf("[CharacterHandler] GenerateFaceCloseup task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, "generate face closeup failed: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.UpdateProgress(taskID, 99) //nolint:errcheck

		updateReq := characterToUpdateReq(char)
		updateReq.FaceCloseup = img.URL

		updated, err := h.characterService.UpdateCharacter(charID, updateReq)
		if err != nil {
			h.taskSvc.Fail(taskID, "save face closeup failed: "+err.Error()) //nolint:errcheck
			return
		}

		h.taskSvc.Complete(taskID, map[string]interface{}{ //nolint:errcheck
			"character": updated,
			"generated": map[string]string{"face_closeup": img.URL},
		})
	}(task.TaskID, uint(id), character, req.Style, req.Provider)

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
	updated, err := h.characterService.UpdateCharacter(uint(id), updateReq)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update portrait")
		return
	}

	respondOK(c, gin.H{"url": portraitURL, "character": updated})
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
				logger.Printf("[CharacterHandler] AIBatchGenerate task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)         //nolint:errcheck
		h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
		chars, err := h.characterService.AIBatchGenerate(tenantID, uint(novelID))
		if err != nil {
			logger.Printf("[CharacterHandler] AIBatchGenerate task %s failed: %v", taskID, err)
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
	}
	_ = c.ShouldBindJSON(&req)
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeThreeView, "批量生成角色图片", "novel", uint(novelID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[CharacterHandler] BatchGenerateImages task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID)                                          //nolint:errcheck
		progressFn := func(pct int) { h.taskSvc.UpdateProgress(taskID, pct) } //nolint:errcheck
		succ, fail, err := h.characterService.BatchGenerateImages(tenantID, uint(novelID), req.Provider, progressFn)
		if err != nil {
			logger.Printf("[CharacterHandler] BatchGenerateImages task %s failed: %v", taskID, err)
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

	arc, err := h.arcService.GetCharacterArc(uint(novelId), uint(characterId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
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
	if !bindJSON(c, &req) {
		return
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
	respondOK(c, gin.H{"characters": chars, "count": len(chars)})
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
		respondErr(c, http.StatusInternalServerError, "voice generation failed: "+err.Error())
		return
	}

	// Store raw path for serving; return API endpoint if local file
	playURL := rawURL
	if len(rawURL) > 7 && rawURL[:7] == "file://" {
		playURL = "/api/v1/characters/" + c.Param("id") + "/voice/sample"
	}
	h.characterService.UpdateCharacter(uint(id), &model.UpdateCharacterRequest{ //nolint:errcheck
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
	filePath := character.VoiceSample
	if len(filePath) > 7 && filePath[:7] == "file://" {
		filePath = filePath[7:]
	}
	c.Header("Content-Type", "audio/mpeg")
	c.Header("Cache-Control", "public, max-age=86400")
	c.File(filePath)
}
