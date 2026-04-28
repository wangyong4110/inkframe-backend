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
}

func NewSkillHandler(skillService *service.SkillService) *SkillHandler {
	return &SkillHandler{skillService: skillService}
}

// ListSkills GET /novels/:id/skills
func (h *SkillHandler) ListSkills(c *gin.Context) {
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid novel id")
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
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid novel id")
		return
	}
	var req model.CreateSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
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
	id, err := strconv.ParseUint(c.Param("skillId"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid skill id")
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
	id, err := strconv.ParseUint(c.Param("skillId"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid skill id")
		return
	}
	var req model.UpdateSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
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
	id, err := strconv.ParseUint(c.Param("skillId"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid skill id")
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
	novelID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid novel id")
		return
	}
	var req model.GenerateSkillsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	skills, err := h.skillService.GenerateSkills(getTenantID(c), uint(novelID), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to generate skills: "+err.Error())
		return
	}
	respondCreated(c, gin.H{"skills": skills, "count": len(skills)})
}

// GenerateSkillEffect POST /skills/:skillId/effect-image
func (h *SkillHandler) GenerateSkillEffect(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("skillId"), 10, 64)
	if err != nil {
		respondErr(c, http.StatusBadRequest, "invalid skill id")
		return
	}
	skill, err := h.skillService.GenerateSkillEffect(uint(id))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, skill)
}
