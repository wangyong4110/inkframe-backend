package handler

import (
	"fmt"
	"net/http"
	"strconv"

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
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondBadRequest(c, "invalid media id")
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
