package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// characterToUpdateReq copies string fields from a Character into an
// UpdateCharacterRequest, preserving existing values before a partial update.
func characterToUpdateReq(c *model.Character) *model.UpdateCharacterRequest {
	return &model.UpdateCharacterRequest{
		Name:          c.Name,
		Role:          c.Role,
		Gender:        c.Meta.Gender,
		Age:           c.Meta.Age,
		Description:   c.Description,
		InnerConflict: c.Meta.InnerConflict,
		CoreDesire:    c.Meta.CoreDesire,
		VoiceID:       c.VoiceConfig.VoiceID,
		VoiceSpeed:    &c.VoiceConfig.VoiceSpeed,
		VoiceStyle:    c.VoiceConfig.VoiceStyle,
	}
}

// CharacterResponse converts a Character model to a response map. Exported so
// cmd/server/task_resume.go can shape a task's Complete() result the same way the HTTP
// handlers do (e.g. char_reanalyze), instead of a differently-shaped ad hoc map.
func CharacterResponse(c *model.Character) gin.H {
	return gin.H{
		"id":               c.ID,
		"novel_id":         c.NovelID,
		"uuid":             c.UUID,
		"name":             c.Name,
		"role":             c.Role,
		"gender":           c.Meta.Gender,
		"age":              c.Meta.Age,
		"description":        c.Description,
		"default_look_id": c.DefaultLookID,
		"default_look":    c.DefaultLook,
		"voice_id":         c.VoiceConfig.VoiceID,
		"voice_speed":      c.VoiceConfig.VoiceSpeed,
		"voice_style":      c.VoiceConfig.VoiceStyle,
		"voice_language":   c.VoiceConfig.VoiceLanguage,
		"voice_sample":     c.VoiceConfig.VoiceSample,
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
	auditSvc         *service.AuditService
}

func (h *CharacterHandler) WithAuditService(svc *service.AuditService) *CharacterHandler {
	h.auditSvc = svc
	return h
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
	if _, err := h.novelService.GetNovel(novelID, getTenantID(c), getUserID(c)); err != nil {
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

	if !requireNovelEditorRole(c, h.novelService, uint(novelId)) {
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

	respondCreated(c, CharacterResponse(character))
}

// charBelongsToTenant verifies character ownership via novel (char → novel → tenant).
// Falls back to allow when novelService is not wired (internal/batch calls).
func (h *CharacterHandler) charBelongsToTenant(char *model.Character, c *gin.Context) bool {
	if h.novelService == nil {
		return true
	}
	_, err := h.novelService.GetNovel(char.NovelID, getTenantID(c), getUserID(c))
	return err == nil
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
	if !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	respondOK(c, CharacterResponse(character))
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

	h.characterService.InjectDefaultLooks(characters)

	resp := make([]gin.H, 0, len(characters))
	for _, ch := range characters {
		resp = append(resp, CharacterResponse(ch))
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
		if isDuplicateKeyError(err) {
			respondErr(c, http.StatusConflict, err.Error())
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, CharacterResponse(character))
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
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c),
			Action: "character.delete", ResourceType: "character", ResourceID: uint(id),
			IP: c.ClientIP(),
		})
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
	if !requireNovelEditorRole(c, h.novelService, uint(novelId)) {
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
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: getTenantID(c), UserID: getUserID(c), NovelID: uint(novelId),
			Action: "character.batch_delete", ResourceType: "novel", ResourceID: uint(novelId),
			Details: map[string]any{"ids": req.IDs, "count": len(req.IDs)}, IP: c.ClientIP(),
		})
	}
	respondOK(c, gin.H{"deleted": len(req.IDs)})
}

// GenerateCharacterImage 生成角色图像（异步任务）
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
	if !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeCharImageGen, "角色图片生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎。
	// cmd/server/task_resume.go 里 service.TaskTypeCharImageGen 的执行函数会用 t.EntityID 重新
	// 查一次角色拿 Name/Description（这两个字段不进 params，因为它们是角色数据不是请求参数），
	// 再反序列化下面存的 type/emotion/action/style 调用同一个 h.imageGenService.GenerateCharacterImage。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"type":    req.Type,
		"emotion": req.Emotion,
		"action":  req.Action,
		"style":   req.Style,
	})
	respondAccepted(c, task.TaskID, "角色图片生成任务已提交")
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

	if !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeThreeView, "角色三视图生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	// 优先使用请求中的 style；未指定时降级到小说项目设置的 image_style。这个降级必须在这里做
	// （而不是留给执行函数），因为 image_style 是"生成这一刻"小说项目设置的快照，存进 params 后
	// 执行函数只管读，不需要也不应该重新解析一次。
	resolvedStyle := req.Style
	if resolvedStyle == "" {
		resolvedStyle = h.characterService.GetNovelImageStyle(character.NovelID)
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎。cmd/server/task_resume.go 里
	// service.TaskTypeThreeView 的执行函数（entity_type=="character" 分支）逻辑与本函数曾经
	// 内联的调用完全一致：用 t.EntityID 重新查角色/默认形象，反序列化 provider/style 调用
	// GenerateThreeViewSheet。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
		"style":    resolvedStyle,
	})

	respondAccepted(c, task.TaskID, "三视图生成任务已提交")
}

// UploadCharacterImage 上传角色图片到指定字段
// POST /api/v1/characters/:id/image/upload?type=three_view
// three_view 会写入默认形象（CharacterLook）。
func (h *CharacterHandler) UploadCharacterImage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	imgURL, ok := receiveAndUpload(c, "character-images", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp"})
	if !ok {
		return
	}
	imgType := c.Query("type")
	switch imgType {
	case "three_view":
		// 写入默认形象（不存在时自动创建）
		defaultLook, _ := h.characterService.GetDefaultLook(uint(id))
		lookReq := &model.UpdateCharacterLookRequest{ThreeViewSheet: &imgURL}
		var updatedLook *model.CharacterLook
		if defaultLook != nil {
			updatedLook, err = h.characterService.UpdateLook(defaultLook.ID, lookReq)
		} else {
			updatedLook, err = h.characterService.CreateLook(uint(id), character.NovelID, &model.CreateCharacterLookRequest{
				Label: "默认形象", SetAsDefault: true, ChapterFrom: 1,
			})
			if err == nil {
				updatedLook, err = h.characterService.UpdateLook(updatedLook.ID, lookReq)
			}
		}
		if err != nil {
			respondErr(c, http.StatusInternalServerError, "failed to save image to look")
			return
		}
		respondOK(c, gin.H{"url": imgURL, "look": updatedLook})
	default:
		respondErr(c, http.StatusBadRequest, "type must be 'three_view'")
	}
}

// UploadCharacterLookImage 上传角色形象图片到指定形象
// POST /api/v1/characters/:id/looks/:look_id/upload?type=portrait|three_view
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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
	default: // "portrait" or empty
		updateReq.Portrait = &imgURL
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
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID: tenantID, UserID: getUserID(c), NovelID: uint(novelID),
			Action: "character.ai_batch_generate", ResourceType: "novel", ResourceID: uint(novelID),
			IP: c.ClientIP(),
		})
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeCharGen 的执行函数
	// 在 cmd/server/task_resume.go，只依赖 t.TenantID/t.EntityID，不需要额外 SetParams）。
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
	// 执行逻辑不在这里——只创建任务记录，执行权完全交给任务引擎（internal/service/task_engine.go）。
	// 引擎会调用 cmd/server/task_resume.go 里为 service.TaskTypeThreeView 注册的执行函数
	// （entity_type=="novel" 分支），其逻辑与本函数曾经内联的调用完全一致：反序列化下面存的
	// provider/force，调用 h.characterService.BatchGenerateImages。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"provider": req.Provider,
		"force":    req.Force,
	})
	respondAccepted(c, task.TaskID, "角色图片批量生成任务已提交")
}

// GenerateCharacterProfile AI生成角色档案（异步任务）
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

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeCharProfileGen, "角色档案生成", "novel", uint(novelId))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeCharProfileGen
	// 的执行函数在 cmd/server/task_resume.go，反序列化下面存的 description 调用同一个
	// h.characterService.GenerateProfile）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"description": req.Description,
	})
	respondAccepted(c, task.TaskID, "角色档案生成任务已提交")
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
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c), getUserID(c)); err != nil {
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
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c), getUserID(c)); err != nil {
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
		if _, err := h.novelService.GetNovel(uint(novelId), getTenantID(c), getUserID(c)); err != nil {
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

	var body struct {
		UserPrompt string `json:"user_prompt"`
	}
	_ = c.ShouldBindJSON(&body)

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeChapterCharExtract, "角色分析", "chapter", chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeChapterCharExtract
	// 的执行函数在 cmd/server/task_resume.go，chapter_id 从 t.EntityID 取，novel_id/user_prompt
	// 反序列化下面存的字段）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"novel_id":    novelID,
		"chapter_no":  chapterNo,
		"user_prompt": body.UserPrompt,
	})
	respondAccepted(c, task.TaskID, "角色分析任务已提交")
}

// ReanalyzeCharacter POST /api/v1/characters/:id/reanalyze
func (h *CharacterHandler) ReanalyzeCharacter(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeCharReanalyze, "角色重新分析", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeCharReanalyze
	// 的执行函数在 cmd/server/task_resume.go，只依赖 t.TenantID/t.EntityID，无需额外 SetParams）。
	respondAccepted(c, task.TaskID, "重新分析任务已提交")
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
	if !h.charBelongsToTenant(character, c) {
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
		reqLogger(c).Errorf("[CharacterHandler] ExtractCharacterVoice: save voice style for char %d: %v", id, err)
		// Non-fatal: return the extracted style even if persisting failed.
		respondOK(c, gin.H{"voice_style": voiceStyle, "character_id": id, "saved": false})
		return
	}

	respondOK(c, gin.H{"voice_style": voiceStyle, "character_id": id, "saved": true, "character": CharacterResponse(updated)})
}

// PreviewVoice 试听角色声音（异步任务）
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
	if !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	tenantID := getTenantID(c)

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
		voice = character.VoiceConfig.VoiceID
	}
	if voice == "" {
		voice = "alloy"
	}
	speed := 1.0
	if req.VoiceSpeed != nil {
		speed = *req.VoiceSpeed
	} else if character.VoiceConfig.VoiceSpeed > 0 {
		speed = character.VoiceConfig.VoiceSpeed
	}
	style := req.VoiceStyle
	if style == "" {
		style = character.VoiceConfig.VoiceStyle
	}
	lang := req.VoiceLanguage
	if lang == "" {
		lang = character.VoiceConfig.VoiceLanguage
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeVoicePreview, "语音试听生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeVoicePreview
	// 的执行函数在 cmd/server/task_resume.go，反序列化下面存的字段调用同一个
	// h.aiService.AudioGenerateWithOptions + h.characterService.UpdateCharacter）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"text":        req.Text,
		"voice_id":    voice,
		"voice_speed": speed,
		"voice_style": style,
		"voice_lang":  lang,
		"char_name":   character.Name,
	})
	respondAccepted(c, task.TaskID, "语音试听生成任务已提交")
}

// ServeVoiceSample 播放角色声音样本（file:// 路径转 HTTP 流）
// GET /api/v1/characters/:id/voice/sample
func (h *CharacterHandler) ServeVoiceSample(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil || character.VoiceConfig.VoiceSample == "" {
		respondErr(c, http.StatusNotFound, "no voice sample available")
		return
	}
	if !h.charBelongsToTenant(character, c) {
		respondErr(c, http.StatusNotFound, "no voice sample available")
		return
	}
	filePath := character.VoiceConfig.VoiceSample
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
	if err != nil || !h.charBelongsToTenant(character, c) {
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
	if err != nil || !h.charBelongsToTenant(character, c) {
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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
	if err != nil || !h.charBelongsToTenant(char, c) {
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

// GenerateLookVisualPrompt POST /characters/:id/looks/generate-prompt（异步任务）
func (h *CharacterHandler) GenerateLookVisualPrompt(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || !h.charBelongsToTenant(char, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	var req struct {
		Description string `json:"description"`
	}
	_ = c.ShouldBindJSON(&req)
	description := req.Description
	if description == "" {
		description = char.Description
	}
	if description == "" {
		respondBadRequest(c, "角色描述不能为空，请先填写角色描述")
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeLookPromptGen, "形象提示词生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeLookPromptGen
	// 的执行函数在 cmd/server/task_resume.go，kind="prompt"（默认）分支反序列化下面存的
	// description 调用同一个 h.characterService.GenerateLookVisualPrompt；GenerateAppearanceDesign
	// 用的是 kind="design" 分支）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"kind":        "prompt",
		"description": description,
	})
	respondAccepted(c, task.TaskID, "形象提示词生成任务已提交")
}

// GenerateAppearanceDesign POST /characters/:id/generate-appearance（异步任务）
// AI 根据角色描述+世界观生成时代准确的统一英文形象提示词（含人种），存入 AppearancePromptEN 并更新默认 look。
func (h *CharacterHandler) GenerateAppearanceDesign(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || !h.charBelongsToTenant(char, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if char.Description == "" {
		respondBadRequest(c, "角色描述为空，请先填写角色描述")
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeLookPromptGen, "形象设计生成", "character", uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎。GenerateCostumeDesign 只需要
	// charID（已是 t.EntityID），不依赖请求体，但仍需 kind="design" 区分于同 taskType 下的
	// GenerateLookVisualPrompt 分支（见 cmd/server/task_resume.go）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"kind": "design",
	})
	respondAccepted(c, task.TaskID, "形象设计生成任务已提交")
}

// GenerateLookImages POST /characters/:id/looks/:look_id/images（异步任务）
func (h *CharacterHandler) GenerateLookImages(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	lookID, ok := parseID(c, "look_id")
	if !ok {
		return
	}
	tenantID := getTenantID(c)
	char, err := h.characterService.GetCharacter(uint(id))
	if err != nil || !h.charBelongsToTenant(char, c) {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}
	if _, err := h.characterService.GetLook(uint(lookID)); err != nil {
		respondErr(c, http.StatusNotFound, "look not found")
		return
	}
	var req struct {
		Type     string `json:"type"`     // "three_view" | "portrait"
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	task, err := h.taskSvc.Create(tenantID, service.TaskTypeLookImageGen, "形象图片生成", "look", uint(lookID))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeLookImageGen
	// 的执行函数在 cmd/server/task_resume.go，用 t.EntityID(=lookID) + char_id 重新查
	// look/character 拿 visualPrompt/style/charName/currentPortrait，反序列化 type/provider
	// 调用同一套 GeneratePortrait/GenerateThreeViewSheet）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"type":     req.Type,
		"char_id":  id,
		"provider": req.Provider,
	})
	respondAccepted(c, task.TaskID, "形象图片生成任务已提交")
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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeCharImageGen, "章节角色形象生成", "chapter", chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	// 执行逻辑不在这里——只创建任务记录，执行权交给任务引擎（service.TaskTypeCharImageGen
	// 的执行函数在 cmd/server/task_resume.go，entity_type=="chapter" 分支用 t.EntityID
	// 重新查章节，反序列化下面存的 novel_id/character_ids/provider 调用同一个
	// h.characterService.GenerateChapterImages）。
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"novel_id":      novelID,
		"character_ids": req.CharacterIDs,
		"provider":      req.Provider,
	})
	respondAccepted(c, task.TaskID, "角色形象生成任务已提交")
}
