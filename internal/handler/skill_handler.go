package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SkillHandler HTTP 技能管理处理器
type SkillHandler struct {
	skillService *service.SkillService
	chapterSvc   *service.ChapterService
}

func NewSkillHandler(skillService *service.SkillService) *SkillHandler {
	return &SkillHandler{skillService: skillService}
}

func (h *SkillHandler) WithChapterService(svc *service.ChapterService) *SkillHandler {
	h.chapterSvc = svc
	return h
}

// ListSkills GET /novels/:id/skills
func (h *SkillHandler) ListSkills(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	opts := repository.ListSkillsOpts{
		Category: c.Query("category"),
		Status:   c.Query("status"),
	}
	if cidStr := c.Query("character_id"); cidStr != "" {
		if cid, err := strconv.ParseUint(cidStr, 10, 64); err == nil {
			u := uint(cid)
			opts.CharacterID = &u
		}
	}
	skills, err := h.skillService.ListSkills(uint(novelID), opts)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list skills")
		return
	}
	respondOK(c, gin.H{"skills": skills, "total": len(skills)})
}

// CreateSkill POST /novels/:id/skills
func (h *SkillHandler) CreateSkill(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.CreateSkillRequest
	if !bindJSON(c, &req) {
		return
	}
	skill, err := h.skillService.CreateSkill(uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create skill")
		return
	}
	respondCreated(c, skill)
}

// GetSkill GET /skills/:skillId
func (h *SkillHandler) GetSkill(c *gin.Context) {
	id, ok := parseID(c, "skillId")
	if !ok {
		return
	}
	skill, err := h.skillService.GetSkill(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "skill not found")
		return
	}
	respondOK(c, skill)
}

// UpdateSkill PUT /skills/:skillId
func (h *SkillHandler) UpdateSkill(c *gin.Context) {
	id, ok := parseID(c, "skillId")
	if !ok {
		return
	}
	var req model.UpdateSkillRequest
	if !bindJSON(c, &req) {
		return
	}
	skill, err := h.skillService.UpdateSkill(uint(id), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update skill")
		return
	}
	respondOK(c, skill)
}

// DeleteSkill DELETE /skills/:skillId
func (h *SkillHandler) DeleteSkill(c *gin.Context) {
	id, ok := parseID(c, "skillId")
	if !ok {
		return
	}
	if err := h.skillService.DeleteSkill(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete skill")
		return
	}
	respondOK(c, gin.H{"message": "skill deleted"})
}

// GenerateSkills POST /novels/:id/skills/generate
func (h *SkillHandler) GenerateSkills(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req model.GenerateSkillsRequest
	if !bindJSON(c, &req) {
		return
	}
	skills, err := h.skillService.GenerateSkills(getTenantID(c), uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to generate skills: "+err.Error())
		return
	}
	respondCreated(c, gin.H{"skills": skills, "count": len(skills)})
}

// AIExtractChapterSkills POST /novels/:id/chapters/:chapter_no/skills/ai-extract
func (h *SkillHandler) AIExtractChapterSkills(c *gin.Context) {
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
		respondErr(c, http.StatusBadRequest, "invalid chapter_no")
		return
	}
	chapter, err := h.chapterSvc.GetChapterByNo(uint(novelID), chapterNo)
	if err != nil {
		respondErr(c, http.StatusNotFound, "chapter not found")
		return
	}
	skills, err := h.skillService.AIExtractChapterSkills(getTenantID(c), uint(novelID), chapter.ID, chapterNo)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to extract chapter skills: "+err.Error())
		return
	}
	respondOK(c, gin.H{"skills": skills, "count": len(skills)})
}

// GenerateSkillEffect POST /skills/:skillId/effect-image
func (h *SkillHandler) GenerateSkillEffect(c *gin.Context) {
	id, ok := parseID(c, "skillId")
	if !ok {
		return
	}
	skill, err := h.skillService.GenerateSkillEffect(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, skill)
}
