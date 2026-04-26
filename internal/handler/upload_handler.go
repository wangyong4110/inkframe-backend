package handler

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		respondBadRequest(c, "no file uploaded")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowed := map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
	}
	if !allowed[ext] {
		respondBadRequest(c, "only jpg/png/webp/gif images are allowed")
		return
	}

	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "image/jpeg"
	}

	objectKey := fmt.Sprintf("images/%s%s", uuid.New().String(), ext)
	url, err := h.storageSvc.Upload(c.Request.Context(), objectKey, file, header.Size, contentType)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "upload failed")
		return
	}

	respondOK(c, gin.H{"url": url})
}
