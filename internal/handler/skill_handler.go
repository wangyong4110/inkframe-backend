package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// SkillHandler 技能处理器
type SkillHandler struct {
	skillSvc *service.SkillService
	novelSvc *service.NovelService
	taskSvc  *service.TaskService
}

func NewSkillHandler(skillSvc *service.SkillService) *SkillHandler {
	return &SkillHandler{skillSvc: skillSvc}
}

func (h *SkillHandler) WithNovelService(svc *service.NovelService) *SkillHandler {
	h.novelSvc = svc
	return h
}

func (h *SkillHandler) WithTaskService(svc *service.TaskService) *SkillHandler {
	h.taskSvc = svc
	return h
}

// checkSkillTenant 校验技能归属当前租户（通过关联小说）。
// 返回 false 时已写入错误响应。
func (h *SkillHandler) checkSkillTenant(c *gin.Context, novelID uint) bool {
	if h.novelSvc == nil {
		return true
	}
	if _, err := h.novelSvc.GetNovel(novelID, getTenantID(c), getUserID(c)); err != nil {
		respondErr(c, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// ListSkills GET /novels/:id/skills
func (h *SkillHandler) ListSkills(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkSkillTenant(c, novelID) {
		return
	}
	p := parsePagination(c)
	skills, total, err := h.skillSvc.ListSkillsPaged(novelID, p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"items": skills, "total": total, "page": p.Page, "page_size": p.PageSize})
}

// CreateSkill POST /novels/:id/skills
func (h *SkillHandler) CreateSkill(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkSkillTenant(c, novelID) {
		return
	}
	var req model.CreateSkillRequest
	if !bindJSON(c, &req) {
		return
	}
	tenantID := getTenantID(c)
	skill, err := h.skillSvc.CreateSkill(tenantID, novelID, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondCreated(c, skill)
}

// GetSkill GET /skills/:id
func (h *SkillHandler) GetSkill(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	skill, err := h.skillSvc.GetSkill(id)
	if err != nil {
		respondErr(c, http.StatusNotFound, "skill not found")
		return
	}
	if !h.checkSkillTenant(c, skill.NovelID) {
		return
	}
	respondOK(c, skill)
}

// UpdateSkill PUT /skills/:id
func (h *SkillHandler) UpdateSkill(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.skillSvc.GetSkill(id)
	if err != nil {
		respondErr(c, http.StatusNotFound, "skill not found")
		return
	}
	if !h.checkSkillTenant(c, existing.NovelID) {
		return
	}
	var req model.UpdateSkillRequest
	if !bindJSON(c, &req) {
		return
	}
	skill, err := h.skillSvc.UpdateSkill(id, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, skill)
}

// DeleteSkill DELETE /skills/:id
func (h *SkillHandler) DeleteSkill(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.skillSvc.DeleteSkill(id, getTenantID(c)); err != nil {
		if err.Error() == "not found" {
			respondErr(c, http.StatusNotFound, "skill not found")
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "skill deleted"})
}

// BatchDeleteSkills 批量删除技能
// DELETE /api/v1/novels/:id/skills
func (h *SkillHandler) BatchDeleteSkills(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkSkillTenant(c, novelID) {
		return
	}
	var req struct {
		IDs []uint `json:"ids" binding:"required,min=1"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.skillSvc.BatchDeleteSkills(novelID, req.IDs); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"deleted": len(req.IDs)})
}

// GenerateSkills POST /novels/:id/skills/ai-generate
func (h *SkillHandler) GenerateSkills(c *gin.Context) {
	novelID, ok := parseID(c, "id")
	if !ok {
		return
	}
	if !h.checkSkillTenant(c, novelID) {
		return
	}
	tenantID := getTenantID(c)

	if h.taskSvc != nil {
		task, err := h.taskSvc.Create(tenantID, service.TaskTypeSkillGen, "AI生成技能体系", "novel", novelID)
		if err != nil {
			respondErr(c, http.StatusInternalServerError, "failed to create task")
			return
		}
		go func(taskID string) {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[SkillHandler] GenerateSkills task %s panic: %v", taskID, r)
					h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
				}
			}()
			h.taskSvc.SetRunning(taskID)         //nolint:errcheck
			h.taskSvc.UpdateProgress(taskID, 10) //nolint:errcheck
			skills, err := h.skillSvc.GenerateSkills(tenantID, novelID)
			if err != nil {
				logger.Errorf("[SkillHandler] GenerateSkills task %s failed: %v", taskID, err)
				h.taskSvc.Fail(taskID, err.Error()) //nolint:errcheck
			} else {
				h.taskSvc.Complete(taskID, map[string]interface{}{"skills": skills, "count": len(skills)}) //nolint:errcheck
			}
		}(task.TaskID)
		respondAccepted(c, task.TaskID, "技能生成任务已提交")
		return
	}

	skills, err := h.skillSvc.GenerateSkills(tenantID, novelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, skills)
}

// GenerateSkillEffect POST /skills/:id/effect
func (h *SkillHandler) GenerateSkillEffect(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	existing, err := h.skillSvc.GetSkill(id)
	if err != nil {
		respondErr(c, http.StatusNotFound, "skill not found")
		return
	}
	if !h.checkSkillTenant(c, existing.NovelID) {
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	_ = c.ShouldBindJSON(&req)
	tenantID := getTenantID(c)
	skill, err := h.skillSvc.GenerateSkillEffect(tenantID, id, req.Provider)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, skill)
}
