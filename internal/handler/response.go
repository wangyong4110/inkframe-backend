package handler

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

func respondOK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": data})
}

func respondCreated(c *gin.Context, data interface{}) {
	c.JSON(http.StatusCreated, gin.H{"code": 0, "message": "success", "data": data})
}

func respondBadRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": msg})
}

func respondErr(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// respondAccepted writes a 202 Accepted response with a task_id payload.
// Used by all async AI endpoints that submit background tasks.
func respondAccepted(c *gin.Context, taskID, message string) {
	c.JSON(http.StatusAccepted, gin.H{
		"code":    0,
		"message": message,
		"data":    gin.H{"task_id": taskID},
	})
}

// receiveAndUpload validates the uploaded image file and stores it under keyPrefix.
// On success it returns (url, true); on failure it writes the HTTP response and returns ("", false).
func receiveAndUpload(c *gin.Context, keyPrefix string, storageSvc storage.Service) (string, bool) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		respondBadRequest(c, "no file uploaded")
		return "", false
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}[ext] {
		respondBadRequest(c, "only jpg/png/webp images are allowed")
		return "", false
	}

	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "image/jpeg"
	}

	objectKey := fmt.Sprintf("%s/%s%s", keyPrefix, uuid.New().String(), ext)
	url, err := storageSvc.Upload(c.Request.Context(), objectKey, file, header.Size, ct)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to upload image")
		return "", false
	}
	return url, true
}

// PaginationParams holds parsed pagination query parameters.
type PaginationParams struct{ Page, PageSize int }

func parsePagination(c *gin.Context) PaginationParams {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 1
	} else if size > 100 {
		size = 100
	}
	return PaginationParams{page, size}
}
