package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
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
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImageEdit, "图片编辑", "novel", body.NovelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"image_url":   body.ImageURL,
		"instruction": body.Instruction,
		"novel_id":    body.NovelID,
	})

	imageURL := body.ImageURL
	instruction := body.Instruction
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ImageHandler] EditImage task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		newURL, err := h.aiSvc.EditImageWithInstruction(
			context.Background(), tenantID, imageURL, instruction,
		)
		if err != nil {
			logger.Errorf("[ImageHandler] EditImage task %s failed: %v", taskID, err)
			h.taskSvc.Fail(taskID, "failed to edit image: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"image_url": newURL}) //nolint:errcheck
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "图片编辑任务已提交")
}

// UpscaleImage POST /images/upscale（异步任务）
// Body: { image_url, scale?, method?, novel_id? }
// method: "bicubic"（默认，CatmullRom 插值，秒级完成）| "ai"（AI 增强，质量更好，需要 AI 配额）
// scale: integer multiplier, default 2, max 8.
func (h *ImageHandler) UpscaleImage(c *gin.Context) {
	var body struct {
		ImageURL string `json:"image_url" binding:"required"`
		Scale    int    `json:"scale"`
		Method   string `json:"method"` // "bicubic" or "ai"
		NovelID  uint   `json:"novel_id"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if body.Scale <= 0 {
		body.Scale = 2
	}
	if body.Method == "" {
		body.Method = "bicubic"
	}

	taskName := "高清放大（算法）"
	if body.Method == "ai" {
		taskName = "高清放大（AI）"
	}

	tenantID := getTenantID(c)
	task, err := h.taskSvc.Create(tenantID, service.TaskTypeImageUpscale, taskName, "novel", body.NovelID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to create task")
		return
	}
	_ = h.taskSvc.SetParams(task.TaskID, map[string]interface{}{
		"image_url": body.ImageURL,
		"scale":     body.Scale,
		"method":    body.Method,
		"novel_id":  body.NovelID,
	})

	imageURL := body.ImageURL
	scale := body.Scale
	method := body.Method
	novelID := body.NovelID
	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ImageHandler] UpscaleImage task %s panic: %v", taskID, r)
				h.taskSvc.Fail(taskID, "内部错误，请重试") //nolint:errcheck
			}
		}()
		h.taskSvc.SetRunning(taskID) //nolint:errcheck
		newURL, err := h.aiSvc.UpscaleImage(context.Background(), tenantID, novelID, imageURL, scale, method)
		if err != nil {
			logger.Errorf("[ImageHandler] UpscaleImage task %s method=%s failed: %v", taskID, method, err)
			h.taskSvc.Fail(taskID, "高清处理失败: "+err.Error()) //nolint:errcheck
			return
		}
		h.taskSvc.Complete(taskID, map[string]interface{}{"image_url": newURL}) //nolint:errcheck
	}(task.TaskID)
	respondAccepted(c, task.TaskID, "高清处理任务已提交")
}
