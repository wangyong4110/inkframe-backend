package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// TaskHandler exposes the unified async task endpoints.
type TaskHandler struct {
	taskSvc *service.TaskService
}

func NewTaskHandler(taskSvc *service.TaskService) *TaskHandler {
	return &TaskHandler{taskSvc: taskSvc}
}

// ListTasks GET /api/v1/tasks
// Query params: type, status, page, page_size
func (h *TaskHandler) ListTasks(c *gin.Context) {
	tenantID := getTenantID(c)
	taskType := c.Query("type")
	status := c.Query("status")
	p := parsePagination(c)

	tasks, total, err := h.taskSvc.List(tenantID, taskType, status, p.Page, p.PageSize)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{
		"items":     tasks,
		"total":     total,
		"page":      p.Page,
		"page_size": p.PageSize,
	})
}

// GetTask GET /api/v1/tasks/:task_id
func (h *TaskHandler) GetTask(c *gin.Context) {
	taskID := c.Param("task_id")
	task, err := h.taskSvc.Get(taskID)
	if err != nil {
		respondErr(c, http.StatusNotFound, "task not found")
		return
	}
	respondOK(c, task)
}
