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
