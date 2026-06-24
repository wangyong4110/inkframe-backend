package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// MediaHandler 媒体素材下载端点（DB 存储后端已废弃）
type MediaHandler struct{ db *gorm.DB }

func NewMediaHandler(db *gorm.DB) *MediaHandler {
	return &MediaHandler{db: db}
}

// ServeMedia 媒体素材已迁移至 OSS，此端点不再提供二进制数据，请通过 OSS 直链访问。
// GET /api/v1/media/:id
func (h *MediaHandler) ServeMedia(c *gin.Context) {
	respondErr(c, http.StatusNotFound, "媒体文件存储已迁移至 OSS，请通过直链访问")
}
