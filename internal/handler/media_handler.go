package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// MediaHandler 媒体素材下载端点（DB 存储后端使用）
type MediaHandler struct{ db *gorm.DB }

func NewMediaHandler(db *gorm.DB) *MediaHandler {
	return &MediaHandler{db: db}
}

// ServeMedia 返回 DB 中存储的媒体素材二进制数据
// GET /api/v1/media/:id
func (h *MediaHandler) ServeMedia(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var asset model.MediaAsset
	if err := h.db.First(&asset, id).Error; err != nil {
		respondErr(c, http.StatusNotFound, "not found")
		return
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, asset.Filename))
	c.Data(http.StatusOK, asset.ContentType, asset.Data)
}
