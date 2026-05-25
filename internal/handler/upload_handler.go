package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// UploadHandler 通用文件上传处理器
type UploadHandler struct {
	storageSvc storage.Service
}

func NewUploadHandler(svc storage.Service) *UploadHandler {
	return &UploadHandler{storageSvc: svc}
}

// UploadImage 上传图片，返回可访问的公开 URL
// POST /api/v1/upload/image
// multipart/form-data: file (jpg/png/webp/gif)
func (h *UploadHandler) UploadImage(c *gin.Context) {
	url, ok := receiveAndUpload(c, "images", h.storageSvc, []string{".jpg", ".jpeg", ".png", ".webp", ".gif"})
	if !ok {
		return
	}
	respondOK(c, gin.H{"url": url})
}
