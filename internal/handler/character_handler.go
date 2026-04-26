package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// characterToUpdateReq copies string fields from a Character into an
// UpdateCharacterRequest, preserving existing values before a partial update.
// Slice fields (PersonalityTags, Abilities) are left nil so the service
// leaves them unchanged.
func characterToUpdateReq(c *model.Character) *model.UpdateCharacterRequest {
	return &model.UpdateCharacterRequest{
		Name:           c.Name,
		Role:           c.Role,
		Archetype:      c.Archetype,
		Appearance:     c.Appearance,
		Personality:    c.Personality,
		Background:     c.Background,
		CharacterArc:   c.CharacterArc,
		Portrait:       c.Portrait,
		ThreeViewFront: c.ThreeViewFront,
		ThreeViewSide:  c.ThreeViewSide,
		ThreeViewBack:  c.ThreeViewBack,
		CoverImage:     c.CoverImage,
	}
}

// characterResponse converts a Character model to a response map, parsing JSON
// string fields (abilities, personality_tags) into proper JSON arrays so the
// frontend receives typed data rather than raw JSON strings.
func characterResponse(c *model.Character) gin.H {
	resp := gin.H{
		"id":               c.ID,
		"novel_id":         c.NovelID,
		"uuid":             c.UUID,
		"name":             c.Name,
		"role":             c.Role,
		"archetype":        c.Archetype,
		"appearance":       c.Appearance,
		"personality":      c.Personality,
		"background":       c.Background,
		"character_arc":    c.CharacterArc,
		"three_view_front": c.ThreeViewFront,
		"three_view_side":  c.ThreeViewSide,
		"three_view_back":  c.ThreeViewBack,
		"portrait":         c.Portrait,
		"cover_image":      c.CoverImage,
		"status":           c.Status,
		"created_at":       c.CreatedAt,
		"updated_at":       c.UpdatedAt,
	}
	// Parse JSON-stored array fields
	if c.Abilities != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.Abilities), &v); err == nil {
			resp["abilities"] = v
		}
	}
	if c.PersonalityTags != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.PersonalityTags), &v); err == nil {
			resp["personality_tags"] = v
		}
	}
	if c.VisualDesign != "" {
		var v interface{}
		if err := json.Unmarshal([]byte(c.VisualDesign), &v); err == nil {
			resp["visual_design"] = v
		}
	}
	return resp
}

// CharacterHandler 角色处理器
type CharacterHandler struct {
	characterService *service.CharacterService
	arcService       *service.CharacterArcService
	imageGenService  *service.ImageGenerationService
	chapterSvc       *service.ChapterService
	storageSvc       storage.Service
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

func (h *CharacterHandler) WithStorage(svc storage.Service) *CharacterHandler {
	h.storageSvc = svc
	return h
}

func (h *CharacterHandler) WithChapterService(svc *service.ChapterService) *CharacterHandler {
	h.chapterSvc = svc
	return h
}

// CreateCharacter 创建角色
// POST /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) CreateCharacter(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	var req model.CreateCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	respondOK(c, characterResponse(character))
}

// ListCharacters 获取角色列表
// GET /api/v1/novels/:novel_id/characters
func (h *CharacterHandler) ListCharacters(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	var req model.UpdateCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	if err := h.characterService.DeleteCharacter(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// GenerateCharacterImage 生成角色图像
// POST /api/v1/characters/:id/images
func (h *CharacterHandler) GenerateCharacterImage(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	var req struct {
		Type     string `json:"type"` // portrait, expression, pose
		Emotion  string `json:"emotion,omitempty"`
		Action   string `json:"action,omitempty"`
		Style    string `json:"style,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	image, err := h.imageGenService.GenerateCharacterImage(&model.GenerateImageRequest{
		Subject:     character.Name,
		Description: character.Appearance,
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

// GenerateThreeView AI生成角色三视图（异步任务）
// POST /api/v1/characters/:id/three-view
// 立即返回 202 + task_id，轮询 GET /characters/:id/three-view/:task_id 获取结果
func (h *CharacterHandler) GenerateThreeView(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	var req struct {
		ViewType string `json:"view_type"` // "front" | "side" | "back" | "all"
		Style    string `json:"style,omitempty"`
		Provider string `json:"provider,omitempty"` // 指定图像生成提供者，可为空
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		respondBadRequest(c, err.Error())
		return
	}
	if req.ViewType == "" {
		req.ViewType = "all"
	}

	character, err := h.characterService.GetCharacter(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "character not found")
		return
	}

	taskID := newTaskID("tv")
	task := &AsyncTask{TaskID: taskID, Status: taskStatusPending, CreatedAt: time.Now().Unix()}
	threeViewTasks.store(task)

	tenantID := getTenantID(c)
	go func(charID uint, char *model.Character, viewType, style, provider string) {
		task.Status = taskStatusRunning
		threeViewTasks.store(task)

		views := []string{viewType}
		if viewType == "all" {
			views = []string{"front", "side", "back"}
		}

		// Generate all views in parallel.
		type viewResult struct {
			view string
			url  string
			err  error
		}
		resultCh := make(chan viewResult, len(views))
		var wg sync.WaitGroup
		for _, v := range views {
			wg.Add(1)
			go func(v string) {
				defer wg.Done()
				img, err := h.imageGenService.GenerateThreeViewImage(tenantID, char.Name, char.Appearance, v, style, char.Portrait, provider)
				if err != nil {
					resultCh <- viewResult{view: v, err: err}
					return
				}
				resultCh <- viewResult{view: v, url: img.URL}
			}(v)
		}
		wg.Wait()
		close(resultCh)

		generated := map[string]string{}
		updateReq := characterToUpdateReq(char)
		for r := range resultCh {
			if r.err != nil {
				task.Status = taskStatusFailed
				task.Error = "generate " + r.view + " view failed: " + r.err.Error()
				threeViewTasks.store(task)
				return
			}
			generated[r.view] = r.url
			switch r.view {
			case "front":
				updateReq.ThreeViewFront = r.url
			case "side":
				updateReq.ThreeViewSide = r.url
			case "back":
				updateReq.ThreeViewBack = r.url
			}
		}

		updated, err := h.characterService.UpdateCharacter(charID, updateReq)
		if err != nil {
			task.Status = taskStatusFailed
			task.Error = "save three view failed: " + err.Error()
			threeViewTasks.store(task)
			return
		}

		task.Status = taskStatusCompleted
		task.Data = map[string]interface{}{"character": updated, "generated": generated}
		threeViewTasks.store(task)
	}(uint(id), character, req.ViewType, req.Style, req.Provider)

	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": "三视图生成任务已提交",
		"data":    gin.H{"task_id": taskID},
	})
}

// GetThreeViewTaskStatus 查询三视图生成任务状态
// GET /api/v1/characters/:id/three-view/:task_id
func (h *CharacterHandler) GetThreeViewTaskStatus(c *gin.Context) {
	taskID := c.Param("task_id")
	task, ok := threeViewTasks.load(taskID)
	if !ok {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}
	respondOK(c, task)
}

// UploadPortrait 上传角色肖像图片（远端 OSS 或本地存储兜底），用作三视图生成参考图
// POST /api/v1/characters/:id/portrait/upload
func (h *CharacterHandler) UploadPortrait(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	portraitURL, ok := receiveAndUpload(c, "portraits", h.storageSvc)
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

// GenerateCharacterProfile AI生成角色档案
// POST /api/v1/novels/:novel_id/characters/generate
func (h *CharacterHandler) GenerateCharacterProfile(c *gin.Context) {
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
		return
	}

	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	novelId, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	characterId, _ := strconv.ParseUint(c.Param("character_id"), 10, 32)

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
	novelId, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid novel id")
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
	novelId, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	characterId, _ := strconv.ParseUint(c.Param("character_id"), 10, 32)

	var req struct {
		CurrentStage int    `json:"current_stage"`
		Note         string `json:"note,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}

	var req struct {
		Images []string `json:"images" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	chars, err := h.characterService.ListEffectiveCharacters(uint(novelID), chapter.ID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, chars)
}

// UpsertChapterCharacter POST /novels/:id/chapters/:chapter_no/characters/:character_id
func (h *CharacterHandler) UpsertChapterCharacter(c *gin.Context) {
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
	characterID, err := strconv.ParseUint(c.Param("character_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	var req model.UpsertChapterCharacterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
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
	characterID, err := strconv.ParseUint(c.Param("character_id"), 10, 32)
	if err != nil {
		respondBadRequest(c, "invalid character id")
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
	_ = novelID
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success"})
}
