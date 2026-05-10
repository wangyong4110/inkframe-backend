package service

import (
	"encoding/json"
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
	TaskTypeStoryboardOptimize       = "storyboard_optimize"
	TaskTypeImport                   = "import"
	TaskTypeNovelAnalysis            = "novel_analysis"
)

// TaskService manages persistent async tasks.
type TaskService struct {
	repo *repository.TaskRepository
}

func NewTaskService(repo *repository.TaskRepository) *TaskService {
	svc := &TaskService{repo: repo}
	// Immediately recover tasks left in running/pending state from a previous server session.
	// Tasks not updated within the last 10 seconds are considered orphaned.
	svc.recoverOrphaned(10 * time.Second)
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
	return s.repo.UpdateFields(taskID, map[string]interface{}{"status": "running"})
}

// UpdateProgress updates the task's progress percentage (0-99).
// The status check against "running" is omitted to avoid a SELECT; callers should
// only call UpdateProgress when the task is known to be running.
func (s *TaskService) UpdateProgress(taskID string, progress int) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 99 {
		progress = 99
	}
	return s.repo.UpdateFields(taskID, map[string]interface{}{"progress": progress})
}

// Complete stores the result and marks the task completed.
// No-op if the task has already been cancelled.
func (s *TaskService) Complete(taskID string, result interface{}) error {
	resultJSON := ""
	if result != nil {
		if b, err := json.Marshal(result); err == nil {
			resultJSON = string(b)
		}
	}
	return s.repo.CompleteIfNotCancelled(taskID, resultJSON)
}

// Fail records the error message and marks the task failed.
// No-op if the task has already been cancelled.
func (s *TaskService) Fail(taskID string, errMsg string) error {
	return s.repo.FailIfNotCancelled(taskID, errMsg)
}

// Cancel marks the task as cancelled only if it is still pending or running.
// Already-terminal tasks (completed/failed/cancelled) are unaffected.
func (s *TaskService) Cancel(taskID string) error {
	return s.repo.CancelIfActive(taskID)
}

// SetMeta updates the task's ResultJSON with arbitrary metadata without changing its status.
// Used to expose intermediate progress data (e.g. crawl_done/total, novel_id) during polling.
func (s *TaskService) SetMeta(taskID string, meta interface{}) error {
	if meta == nil {
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return s.repo.UpdateFields(taskID, map[string]interface{}{"result": string(b)})
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

// CancelActiveByEntity cancels all pending/running tasks of the given type for an entity.
// Used before creating a replacement task; cancelled status makes goroutine Complete/Fail no-ops.
func (s *TaskService) CancelActiveByEntity(entityType string, entityID uint, taskType string) {
	if err := s.repo.CancelActiveByEntity(entityType, entityID, taskType); err != nil {
		logger.Printf("TaskService: CancelActiveByEntity %s/%d/%s: %v", entityType, entityID, taskType, err)
	}
}

// recoverOrphaned marks tasks stuck in pending/running as failed if not updated within `age`.
func (s *TaskService) recoverOrphaned(age time.Duration) {
	before := time.Now().Add(-age)
	n, err := s.repo.MarkStaleRunning(before)
	if err != nil {
		logger.Printf("TaskService: recoverOrphaned error: %v", err)
	} else if n > 0 {
		logger.Printf("TaskService: recovered %d orphaned task(s) from previous session", n)
	}
}

// runCleanup deletes completed/failed tasks older than 7 days, and recovers stale
// running tasks (not updated in >2h), once per hour.
func (s *TaskService) runCleanup() {
	ticker := time.NewTicker(time.Hour)
	for range ticker.C {
		// Delete old terminal tasks.
		cutoff := time.Now().AddDate(0, 0, -7)
		if err := s.repo.DeleteOldCompleted(cutoff); err != nil {
			logger.Printf("TaskService: cleanup error: %v", err)
		}
		// Recover tasks stuck in running/pending for more than 2 hours.
		s.recoverOrphaned(2 * time.Hour)
	}
}
