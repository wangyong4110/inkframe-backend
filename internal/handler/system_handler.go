package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SystemHandler 系统配置控制器
type SystemHandler struct {
	repo *repository.SystemSettingRepository
}

func NewSystemHandler(repo *repository.SystemSettingRepository) *SystemHandler {
	return &SystemHandler{repo: repo}
}

// ListSettings GET /api/v1/system/settings
func (h *SystemHandler) ListSettings(c *gin.Context) {
	items, err := h.repo.List()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list settings")
		return
	}
	respondOK(c, items)
}

// UpdateSetting PUT /api/v1/system/settings/:key
func (h *SystemHandler) UpdateSetting(c *gin.Context) {
	key := c.Param("key")
	var body struct {
		Value       string `json:"value"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if err := h.repo.Set(key, body.Value, body.Description); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save setting")
		return
	}
	respondOK(c, nil)
}
