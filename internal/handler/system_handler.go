package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SystemHandler 系统配置控制器
type SystemHandler struct {
	repo     *repository.SystemSettingRepository
	onChange map[string]func(value string) // key → callback（设置变更时触发）
}

func NewSystemHandler(repo *repository.SystemSettingRepository) *SystemHandler {
	return &SystemHandler{
		repo:     repo,
		onChange: make(map[string]func(string)),
	}
}

// RegisterOnChange 注册某个 key 变更时的回调（由 main.go 在启动时注册）。
func (h *SystemHandler) RegisterOnChange(key string, fn func(value string)) {
	h.onChange[key] = fn
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
	if !bindJSON(c, &body) {
		return
	}
	if err := h.repo.Set(key, body.Value, body.Description); err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to save setting")
		return
	}
	// 触发变更回调（不阻塞请求）
	if fn, ok := h.onChange[key]; ok {
		go fn(body.Value)
	}
	respondOK(c, nil)
}
