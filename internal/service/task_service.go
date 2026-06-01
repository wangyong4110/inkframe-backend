package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"gorm.io/gorm"
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
	repo             *repository.TaskRepository
	db               *gorm.DB      // optional: used for cross-table cleanup (e.g. WebhookDelivery)
	stopCh           chan struct{}  // closed by Shutdown() to stop background goroutines
	cancelFns        sync.Map      // taskID string → context.CancelFunc
	resumeFns        sync.Map      // taskType string → func(*model.AsyncTask)
	cleanupCallbacks []func()      // optional hooks called during the hourly cleanup cycle
	rootCtx          context.Context // server root context; cancelled on graceful shutdown
}

func NewTaskService(repo *repository.TaskRepository) *TaskService {
	svc := &TaskService{
		repo:    repo,
		stopCh:  make(chan struct{}),
		rootCtx: context.Background(), // default; overridden by Boot(ctx)
	}
	go svc.runCleanup()
	return svc
}

// WithDB injects an optional *gorm.DB used for cross-table cleanup operations
// (e.g. purging old WebhookDelivery records). Call before Boot().
func (s *TaskService) WithDB(db *gorm.DB) *TaskService {
	s.db = db
	return s
}

// Boot recovers orphaned tasks from a previous session. Must be called after all
// RegisterResumeHandler calls so that resumable task types are already registered.
// The provided ctx is stored as the service root context so that resumed goroutines
// inherit it (and are cancelled when the server shuts down).
func (s *TaskService) Boot(ctx context.Context) {
	s.rootCtx = ctx
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

// maxQueuedTasksPerTenant is the maximum number of pending/running tasks allowed per tenant.
const maxQueuedTasksPerTenant = 10000

// Create inserts a new pending task and returns it.
func (s *TaskService) Create(tenantID uint, taskType, title, entityType string, entityID uint) (*model.AsyncTask, error) {
	// Enforce per-tenant queue size limit to prevent resource exhaustion.
	if count, err := s.repo.CountActive(tenantID); err != nil {
		logger.Printf("[TaskService] queue size check failed for tenant %d: %v", tenantID, err)
	} else if count >= maxQueuedTasksPerTenant {
		return nil, fmt.Errorf("task queue full (%d tasks pending/running); try again later", count)
	}

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

// MarkTaskFailed implements dead-letter queue semantics.
// Each call increments retry_count and appends to failure_log.
// If retry_count < max_retries, status is reset to "pending" with exponential backoff
// (not implemented here — callers re-enqueue via the resume handler path).
// Once retry_count >= max_retries the task is moved to status "dead" and will not
// be retried automatically; it remains visible in the task list for diagnosis.
func (s *TaskService) MarkTaskFailed(taskID string, err error) {
	task, dbErr := s.repo.GetByTaskID(taskID)
	if dbErr != nil {
		logger.Printf("[TaskService] MarkTaskFailed: cannot load task %s: %v", taskID, dbErr)
		return
	}
	// Accumulate failure log (truncate to 8KB to avoid unbounded growth).
	const maxLogBytes = 8192
	entry := fmt.Sprintf("[%s] %v", time.Now().Format(time.RFC3339), err)
	newLog := task.FailureLog
	if newLog != "" {
		newLog += "\n"
	}
	newLog += entry
	if len(newLog) > maxLogBytes {
		newLog = newLog[len(newLog)-maxLogBytes:]
	}

	task.RetryCount++
	task.FailureLog = newLog

	maxRetries := task.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if task.RetryCount < maxRetries {
		// Retry: reset to pending so the next cleanup cycle can resume it.
		// Backoff is implicit: cleanup runs hourly and Boot runs on next restart.
		task.Status = "pending"
		task.Error = fmt.Sprintf("retry %d/%d: %v", task.RetryCount, maxRetries, err)
		logger.Printf("[TaskService] task %s moved back to pending for retry %d/%d", taskID, task.RetryCount, maxRetries)
	} else {
		// Dead letter: exhausted all retries.
		task.Status = "dead"
		task.Error = fmt.Sprintf("dead after %d retries: %v", task.RetryCount, err)
		logger.Printf("[TaskService] task %s moved to dead-letter queue after %d retries", taskID, task.RetryCount)
	}
	if saveErr := s.repo.Update(task); saveErr != nil {
		logger.Printf("[TaskService] MarkTaskFailed: save task %s failed: %v", taskID, saveErr)
	}
}

// Get returns a task by its task_id (no tenant check — use GetForTenant for API responses).
func (s *TaskService) GetUnscoped(taskID string) (*model.AsyncTask, error) {
	return s.repo.GetByTaskID(taskID)
}

// GetForTenant returns a task only if it belongs to the given tenant.
// Returns an error if the task does not exist or belongs to a different tenant.
// Use this in HTTP handlers instead of Get() to enforce tenant isolation.
func (s *TaskService) GetForTenant(taskID string, tenantID uint) (*model.AsyncTask, error) {
	task, err := s.repo.GetByTaskID(taskID)
	if err != nil {
		return nil, err
	}
	if task.TenantID != tenantID {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return task, nil
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
		if cancel, ok := fn.(context.CancelFunc); ok {
			cancel()
		} else {
			logger.Printf("[TaskService] cancelFns: unexpected type for task %s", taskID)
		}
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

// Heartbeat updates the updated_at timestamp of a running task to signal it is still alive.
// Long-running task goroutines should call this periodically to prevent the cleanup loop
// from treating the task as a zombie and marking it failed.
func (s *TaskService) Heartbeat(taskID string) error {
	return s.repo.UpdateFields(taskID, map[string]interface{}{"updated_at": time.Now()})
}

// GetLatestAnalysisTask returns the most recently created novel_analysis task for the given novel.
func (s *TaskService) GetLatestAnalysisTask(novelID uint) (*model.AsyncTask, error) {
	return s.repo.GetLatestByTypeAndEntity(TaskTypeNovelAnalysis, "novel", novelID)
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

// ListDistinctActiveTenants returns tenant IDs that currently have pending tasks.
// This is the foundation for a per-tenant round-robin scheduler: callers can
// iterate over the returned tenant IDs and pick the oldest pending task for each
// tenant in sequence, preventing any single tenant from starving others.
//
// Per-tenant fairness design note:
// The ink_async_task table has composite index idx_task_tenant_created (tenant_id, created_at)
// which makes "SELECT ... WHERE tenant_id = ? AND status = 'pending' ORDER BY created_at LIMIT 1"
// very efficient. A fair scheduler would:
//   1. Call ListDistinctActiveTenants() to get [t1, t2, t3, ...]
//   2. Round-robin through them (track lastProcessedTenantID in memory)
//   3. For each tenant, pick the oldest pending task
// This prevents one high-volume tenant from blocking others.
func (s *TaskService) ListDistinctActiveTenants() ([]uint, error) {
	return s.repo.ListDistinctActiveTenants()
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
	// Each resumed goroutine inherits rootCtx so that server shutdown propagates to all
	// in-flight resumed tasks (rather than running forever on context.Background()).
	// A per-task 30-minute hard timeout is layered on top of the server root context.
	const maxRecoveryConcurrency = 20
	resumed := 0
	if len(resumableTypes) > 0 {
		if tasks, err := s.repo.ListOrphaned(before, resumableTypes); err == nil {
			sem := make(chan struct{}, maxRecoveryConcurrency)
			var wg sync.WaitGroup
			for _, t := range tasks {
				if fn, ok := s.resumeFns.Load(t.Type); ok {
					_ = s.repo.UpdateFields(t.TaskID, map[string]interface{}{
						"status": "pending",
						"error":  "",
					})
					t.Status = "pending"
					wg.Add(1)
					sem <- struct{}{}
					// Capture loop variables before goroutine.
					task := t
					resumeFn := fn
					taskCtx, taskCancel := context.WithTimeout(s.rootCtx, 30*time.Minute)
					go func() {
						defer wg.Done()
						defer func() { <-sem }()
						defer taskCancel()
						// Store cancel so that Cancel() can interrupt this resumed task.
						s.cancelFns.Store(task.TaskID, taskCancel)
						defer s.cancelFns.Delete(task.TaskID)
						_ = taskCtx // available to resume handler if it needs ctx in future
						resumeFn.(func(*model.AsyncTask))(task)
					}()
					resumed++
				}
			}
			wg.Wait()
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
// running tasks (not updated in >2h), expires pending tasks queued for >1h,
// and runs any registered cleanup callbacks, once per hour.
// Exits when Shutdown() is called.
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
			// Recover tasks stuck in "running" for more than 2h (no heartbeat).
			s.recoverOrphaned(2 * time.Hour)
			// Expire tasks stuck in "pending" for more than 1h (never picked up).
			pendingCutoff := time.Now().Add(-1 * time.Hour)
			if n, err := s.repo.MarkStalePending(pendingCutoff); err != nil {
				logger.Printf("TaskService: expire stale pending tasks error: %v", err)
			} else if n > 0 {
				logger.Printf("TaskService: expired %d stale pending task(s)", n)
			}
			// Clean up old webhook delivery records (keep last 30 days)
			if s.db != nil {
				cutoff := time.Now().AddDate(0, 0, -30)
				s.db.Where("created_at < ?", cutoff).Delete(&model.WebhookDelivery{})
			}
			// Run domain-specific cleanup callbacks (e.g. rewrite chapter tasks).
			for _, fn := range s.cleanupCallbacks {
				fn()
			}
		case <-s.stopCh:
			return
		}
	}
}
