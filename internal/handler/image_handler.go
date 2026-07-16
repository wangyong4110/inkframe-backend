package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ImageHandler handles generic image operations.
type ImageHandler struct {
	aiSvc   *service.AIService
	taskSvc *service.TaskService
}

func NewImageHandler(aiSvc *service.AIService) *ImageHandler {
	return &ImageHandler{aiSvc: aiSvc}
}

// WithTaskService injects the TaskService for async task management.
func (h *ImageHandler) WithTaskService(svc *service.TaskService) *ImageHandler {
	h.taskSvc = svc
	return h
}

// EditImage POST /images/edit（异步任务）
// Accepts { image_url, instruction, novel_id? } and returns task_id immediately.
// Uses DreamO (text-to-image with reference): instruction drives new composition, original image provides style/character consistency.
func (h *ImageHandler) EditImage(c *gin.Context) {
	var body struct {
		ImageURL    string `json:"image_url" binding:"required"`
		Instruction string `json:"instruction" binding:"required"`
		NovelID     uint   `json:"novel_id"`
	}
	if !bindJSON(c, &body) {
		return
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeImageEdit, "图片编辑", "novel", body.NovelID, map[string]interface{}{
		"image_url":   body.ImageURL,
		"instruction": body.Instruction,
		"novel_id":    body.NovelID,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	respondAccepted(c, task.TaskID, "图片编辑任务已提交")
}

// UpscaleImage POST /images/upscale（异步任务，AI 增强放大）
// Body: { image_url, scale?, novel_id? }
// scale: integer multiplier, default 2, max 8.
func (h *ImageHandler) UpscaleImage(c *gin.Context) {
	var body struct {
		ImageURL string `json:"image_url" binding:"required"`
		Scale    int    `json:"scale"`
		NovelID  uint   `json:"novel_id"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if body.Scale <= 0 {
		body.Scale = 2
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.CreateWithParams(tenantID, service.TaskTypeImageUpscale, "高清放大（AI）", "novel", body.NovelID, map[string]interface{}{
		"image_url": body.ImageURL,
		"scale":     body.Scale,
		"novel_id":  body.NovelID,
	})
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}

	respondAccepted(c, task.TaskID, "高清处理任务已提交")
}
