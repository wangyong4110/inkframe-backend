package service

import (
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// Task type constants — used by handlers to tag tasks.
const (
	TaskTypeStoryboardGen = "storyboard_gen"
	TaskTypeChapterGen    = "chapter_gen"
	TaskTypeVoiceGen      = "voice_gen"
	TaskTypeImageGen      = "image_gen"
	TaskTypeThreeView     = "three_view"
	TaskTypeCharGen             = "char_gen"
	TaskTypeItemExtract         = "item_extract"
	TaskTypePlotExtract         = "plot_extract"
	TaskTypeAssetGen            = "asset_gen"
	TaskTypeSceneAnchorExtract       = "scene_anchor_extract"
	TaskTypeChapterSummaryBatch      = "chapter_summary_batch"
	TaskTypeSFXGen                   = "sfx_gen"
	TaskTypeStoryboardReview         = "storyboard_review"
	TaskTypeImport                   = "import"
	TaskTypeNovelAnalysis            = "novel_analysis"
)

// TaskService manages persistent async tasks.
type TaskService struct {
	repo *repository.TaskRepository
}

func NewTaskService(repo *repository.TaskRepository) *TaskService {
	svc := &TaskService{repo: repo}
	go svc.runCleanup()
	return svc
}

// Create inserts a new pending task and returns it.
func (s *TaskService) Create(tenantID uint, taskType, title, entityType string, entityID uint) (*model.AsyncTask, error) {
	prefix := taskType
	if len(taskType) >= 2 {
		prefix = taskType[:2]
	}
	task := &model.AsyncTask{
		TaskID:     prefix + "-" + uuid.New().String()[:8],
		TenantID:   tenantID,
		Type:       taskType,
		Status:     "pending",
		Title:      title,
		EntityType: entityType,
		EntityID:   entityID,
	}
	return task, s.repo.Create(task)
}

// SetRunning transitions the task to running.
func (s *TaskService) SetRunning(taskID string) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	task.Status = "running"
	return s.repo.Update(task)
}

// UpdateProgress updates the task's progress percentage (0-99). No-op if task is not running.
func (s *TaskService) UpdateProgress(taskID string, progress int) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if task.Status != "running" {
		return nil
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 99 {
		progress = 99
	}
	task.Progress = progress
	return s.repo.Update(task)
}

// Complete stores the result and marks the task completed.
// No-op if the task has already been cancelled.
func (s *TaskService) Complete(taskID string, result interface{}) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if task.Status == "cancelled" {
		return nil
	}
	if result != nil {
		b, err := json.Marshal(result)
		if err == nil {
			task.ResultJSON = string(b)
		}
	}
	task.Status = "completed"
	task.Progress = 100
	return s.repo.Update(task)
}

// Fail records the error message and marks the task failed.
// No-op if the task has already been cancelled.
func (s *TaskService) Fail(taskID string, errMsg string) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if task.Status == "cancelled" {
		return nil
	}
	task.Status = "failed"
	task.Error = errMsg
	return s.repo.Update(task)
}

// Cancel marks the task as cancelled. Running goroutines finish but their
// Complete/Fail calls become no-ops once cancelled.
func (s *TaskService) Cancel(taskID string) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" {
		return nil // already terminal
	}
	task.Status = "cancelled"
	return s.repo.Update(task)
}

// SetMeta updates the task's ResultJSON with arbitrary metadata without changing its status.
// Used to expose intermediate progress data (e.g. crawl_done/total, novel_id) during polling.
func (s *TaskService) SetMeta(taskID string, meta interface{}) error {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if meta != nil {
		b, err := json.Marshal(meta)
		if err == nil {
			task.ResultJSON = string(b)
		}
	}
	return s.repo.Update(task)
}

// Get returns a task by its task_id.
func (s *TaskService) Get(taskID string) (*model.AsyncTask, error) {
	return s.repo.GetByTaskID(taskID)
}

// List returns paginated tasks for a tenant.
func (s *TaskService) List(tenantID uint, taskType, status string, page, pageSize int) ([]*model.AsyncTask, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return s.repo.ListByTenant(tenantID, taskType, status, page, pageSize)
}

// runCleanup deletes completed/failed tasks older than 7 days, once per hour.
func (s *TaskService) runCleanup() {
	ticker := time.NewTicker(time.Hour)
	for range ticker.C {
		cutoff := time.Now().AddDate(0, 0, -7)
		if err := s.repo.DeleteOldCompleted(cutoff); err != nil {
			logger.Printf("TaskService: cleanup error: %v", err)
		}
	}
}
