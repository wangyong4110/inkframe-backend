package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
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
	TaskTypeFaceCloseup   = "face_closeup"
	TaskTypeCharGen             = "char_gen"
	TaskTypeItemExtract         = "item_extract"
	TaskTypePlotExtract         = "plot_extract"
	TaskTypeAssetGen            = "asset_gen"
	TaskTypeSceneAnchorExtract       = "scene_anchor_extract"
	TaskTypeChapterSummaryBatch      = "chapter_summary_batch"
	TaskTypeSFXGen                   = "sfx_gen"
	TaskTypeChapterReview            = "chapter_review"
	TaskTypeStoryboardReview         = "storyboard_review"
	TaskTypeStoryboardOptimize       = "storyboard_optimize"
	TaskTypeImport                   = "import"
	TaskTypeNovelAnalysis            = "novel_analysis"
	TaskTypeRewriteAnalysis          = "rewrite_analysis" // Phase 0+1: literary analysis + bible generation
	TaskTypeRewriteChapters          = "rewrite_chapters" // Phase 2: chapter-by-chapter rewriting
	TaskTypeCrawlJob                 = "crawl_job"
	TaskTypeSkillGen                 = "skill_gen"
)

// TaskService manages persistent async tasks.
type TaskService struct {
	repo            *repository.TaskRepository
	stopCh          chan struct{}  // closed by Shutdown() to stop background goroutines
	cancelFns       sync.Map      // taskID string → context.CancelFunc
	resumeFns       sync.Map      // taskType string → func(*model.AsyncTask)
	cleanupCallbacks []func()     // optional hooks called during the hourly cleanup cycle
}

func NewTaskService(repo *repository.TaskRepository) *TaskService {
	svc := &TaskService{
		repo:   repo,
		stopCh: make(chan struct{}),
	}
	go svc.runCleanup()
	return svc
}

// Boot recovers orphaned tasks from a previous session. Must be called after all
// RegisterResumeHandler calls so that resumable task types are already registered.
func (s *TaskService) Boot() {
	s.recoverOrphaned(10 * time.Second)
}

// RegisterResumeHandler registers a function that can resume a task of the given type
// after server restart. The function receives the full AsyncTask (including ParamsJSON).
func (s *TaskService) RegisterResumeHandler(taskType string, fn func(*model.AsyncTask)) {
	s.resumeFns.Store(taskType, fn)
}

// SetParams persists arbitrary resume parameters for a task as JSON.
func (s *TaskService) SetParams(taskID string, params interface{}) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.repo.UpdateFields(taskID, map[string]interface{}{"params": string(b)})
}

// RegisterCleanupCallback adds a function that is called once per hour during the
// regular cleanup cycle. Use this to clean up domain-specific data associated with
// completed or failed tasks (e.g. ChapterRewriteTask records for old rewrite projects).
func (s *TaskService) RegisterCleanupCallback(fn func()) {
	s.cleanupCallbacks = append(s.cleanupCallbacks, fn)
}

// Shutdown stops background goroutines. Call on server exit.
func (s *TaskService) Shutdown() {
	close(s.stopCh)
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

// RegisterCancel stores a cancel function for an in-flight task.
// Call DeregisterCancel when the task finishes to avoid memory leaks.
func (s *TaskService) RegisterCancel(taskID string, cancel context.CancelFunc) {
	s.cancelFns.Store(taskID, cancel)
}

// DeregisterCancel removes the cancel function after the task finishes.
func (s *TaskService) DeregisterCancel(taskID string) {
	s.cancelFns.Delete(taskID)
}

// Cancel marks the task as cancelled only if it is still pending or running.
// If a cancel function is registered (task is in-flight), it is invoked immediately
// so the running goroutine's context is cancelled.
func (s *TaskService) Cancel(taskID string) error {
	if fn, ok := s.cancelFns.Load(taskID); ok {
		fn.(context.CancelFunc)()
	}
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

// recoverOrphaned first resumes tasks whose type has a registered resume handler,
// then marks remaining stale pending/running tasks as failed.
func (s *TaskService) recoverOrphaned(age time.Duration) {
	before := time.Now().Add(-age)

	// 1. Collect resumable task types.
	var resumableTypes []string
	s.resumeFns.Range(func(k, _ interface{}) bool {
		resumableTypes = append(resumableTypes, k.(string))
		return true
	})

	// 2. Resume matching orphaned tasks (reset to pending so MarkStaleRunning skips them).
	resumed := 0
	if len(resumableTypes) > 0 {
		if tasks, err := s.repo.ListOrphaned(before, resumableTypes); err == nil {
			for _, t := range tasks {
				if fn, ok := s.resumeFns.Load(t.Type); ok {
					_ = s.repo.UpdateFields(t.TaskID, map[string]interface{}{
						"status": "pending",
						"error":  "",
					})
					t.Status = "pending"
					go fn.(func(*model.AsyncTask))(t)
					resumed++
				}
			}
		}
	}

	// 3. Mark remaining stale tasks as failed.
	n, err := s.repo.MarkStaleRunning(before)
	if err != nil {
		logger.Printf("TaskService: recoverOrphaned error: %v", err)
	} else if n > 0 {
		logger.Printf("TaskService: recovered %d orphaned task(s) → failed", n)
	}
	if resumed > 0 {
		logger.Printf("TaskService: resumed %d task(s) from previous session", resumed)
	}
}

// runCleanup deletes completed/failed tasks older than 7 days, recovers stale
// running tasks (not updated in >2h), and runs any registered cleanup callbacks,
// once per hour. Exits when Shutdown() is called.
func (s *TaskService) runCleanup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Delete old terminal tasks.
			cutoff := time.Now().AddDate(0, 0, -7)
			if err := s.repo.DeleteOldCompleted(cutoff); err != nil {
				logger.Printf("TaskService: cleanup error: %v", err)
			}
			// Recover tasks stuck in running/pending for more than 2 hours.
			s.recoverOrphaned(2 * time.Hour)
			// Run domain-specific cleanup callbacks (e.g. rewrite chapter tasks).
			for _, fn := range s.cleanupCallbacks {
				fn()
			}
		case <-s.stopCh:
			return
		}
	}
}
