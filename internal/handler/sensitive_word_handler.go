package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SensitiveWordHandler 敏感词规则管理（管理员接口）
type SensitiveWordHandler struct {
	repo *repository.SensitiveWordRuleRepository
}

func NewSensitiveWordHandler(repo *repository.SensitiveWordRuleRepository) *SensitiveWordHandler {
	return &SensitiveWordHandler{repo: repo}
}

// List GET /api/v1/admin/sensitive-words
func (h *SensitiveWordHandler) List(c *gin.Context) {
	pp := parsePagination(c)
	rules, total, err := h.repo.List(getTenantID(c), pp.Page, pp.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list rules")
		return
	}
	respondOK(c, gin.H{"total": total, "items": rules})
}

// Create POST /api/v1/admin/sensitive-words
func (h *SensitiveWordHandler) Create(c *gin.Context) {
	var req struct {
		Word        string `json:"word" binding:"required"`
		Replacement string `json:"replacement"`
		Category    string `json:"category"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule := &model.SensitiveWordRule{
		TenantID:    getTenantID(c),
		Word:        req.Word,
		Replacement: req.Replacement,
		Category:    req.Category,
		Enabled:     enabled,
	}
	if err := h.repo.Create(rule); err != nil {
		if isDuplicateKeyError(err) {
			respondBadRequest(c, "word already exists")
			return
		}
		respondErr(c, http.StatusInternalServerError, "failed to create rule")
		return
	}
	respondCreated(c, rule)
}

// Update PUT /api/v1/admin/sensitive-words/:id
func (h *SensitiveWordHandler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	rule, err := h.repo.GetByID(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "rule not found")
		return
	}

	var req struct {
		Word        *string `json:"word"`
		Replacement *string `json:"replacement"`
		Category    *string `json:"category"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if req.Word != nil {
		rule.Word = *req.Word
	}
	if req.Replacement != nil {
		rule.Replacement = *req.Replacement
	}
	if req.Category != nil {
		rule.Category = *req.Category
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if err := h.repo.Update(rule); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to update rule")
		return
	}
	respondOK(c, rule)
}

// Delete DELETE /api/v1/admin/sensitive-words/:id
func (h *SensitiveWordHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid id")
		return
	}
	if err := h.repo.Delete(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to delete rule")
		return
	}
	respondOK(c, nil)
}
