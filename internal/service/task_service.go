package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
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
	TaskTypeChapterReview            = "chapter_review"
	// TaskTypeChapterOutlineReview is distinct from TaskTypeChapterReview: OutlineReviewHandler's
	// "chapter_review" originally aliased the SAME string as ChapterHandler's chapter-content
	// review (both used "chapter_review" for entity_type="chapter"), even though they call
	// entirely different service methods (OutlineReviewService.ReviewChapterOutline vs
	// QualityControlService.ReviewChapter). Split out so the engine can route each to its own
	// executor unambiguously.
	TaskTypeChapterOutlineReview     = "chapter_outline_review"
	TaskTypeChapterReviewBatch       = "chapter_review_batch"
	TaskTypeStoryboardReview         = "storyboard_review"
	TaskTypeStoryboardOptimize       = "storyboard_optimize"
	TaskTypeStoryboardSceneRegen     = "storyboard_scene_regen" // 单场次分镜重新生成（与整视频 storyboard_gen 区分）
	TaskTypeImport                   = "import"
	TaskTypeNovelAnalysis            = "novel_analysis"
	TaskTypeRewriteAnalysis          = "rewrite_analysis" // Phase 0+1: literary analysis + bible generation
	TaskTypeRewriteChapters          = "rewrite_chapters" // Phase 2: chapter-by-chapter rewriting
	TaskTypeCrawlJob                 = "crawl_job"
	TaskTypeSkillGen                 = "skill_gen"
	TaskTypeBatchChapterGen          = "batch_chapter_gen"
	TaskTypeCharReanalyze            = "char_reanalyze"
	TaskTypeChapterCharExtract       = "chapter_char_extract"
	TaskTypeChapterSceneExtract      = "chapter_scene_extract"
	TaskTypeChapterItemExtract       = "chapter_item_extract"
	TaskTypeScreenplayGen            = "screenplay_gen"
	TaskTypeNovelOutlineGen          = "novel_outline_gen"
	TaskTypeCharImageGen             = "char_image_gen"
	TaskTypeVoicePreview             = "voice_preview"
	TaskTypeLookPromptGen            = "look_prompt_gen"
	TaskTypeLookImageGen             = "look_image_gen"
	TaskTypeCoverImageGen            = "cover_image_gen"
	TaskTypeImageEdit                = "image_edit"
	TaskTypeImageUpscale             = "image_upscale"
	TaskTypeLipSync                  = "lipsync" // shot-level lip-sync video generation (was an untyped string literal)
	TaskTypeChapterRewriteInstr      = "chapter_rewrite_instr"
	TaskTypeVideoGen                 = "video_gen"      // submit all shots + poll + stitch
	TaskTypeVideoSynthesis           = "video_synthesis" // final synthesis pipeline (stitch→subtitle→upload)
	TaskTypeChapterPostProcess       = "chapter_post_process" // quiet task: summary/title/refine/arc-summary tail after chapter_gen completes
)

// quietTaskTypes are tracked (persisted, resumable, failure-notified) like any other task,
// but are excluded from the default task list/panel query — they represent background
// continuation work the user already saw a different task type "complete" for, and
// resurfacing a second progress entry for the same user action would be confusing.
// An explicit `type=` filter still returns them (useful for future debugging UIs).
var quietTaskTypes = []string{TaskTypeChapterPostProcess}

// TaskService manages persistent async tasks.
type TaskService struct {
	repo             *repository.TaskRepository
	db               *gorm.DB          // optional: used for cross-table cleanup (e.g. WebhookDelivery)
	cache            *redis.Client     // optional: for cross-instance task cancel broadcast
	notifSvc         *NotificationService              // optional: sends in-app notifications on task failure
	tenantUserRepo   *repository.TenantUserRepository  // optional: resolves tenant → user IDs for notifications
	stopCh           chan struct{}     // closed by Shutdown() to stop background goroutines
	cancelFns        sync.Map          // taskID string → context.CancelFunc
	resumeFns        sync.Map          // taskType string → func(*model.AsyncTask) — doubles as both the
	// crash-recovery registry and (since the task engine's introduction, see task_engine.go) the
	// canonical "how do I execute a task of this type" registry used for first dispatch too.
	semaphores       sync.Map          // semaphore key string → chan struct{} (RunTracked MaxConcurrency / task engine per-(tenant,type) backstop)
	rootCtx          context.Context   // server root context; cancelled on graceful shutdown

	// Task engine (task_engine.go): wakeCh is signalled by Create() so the engine's dispatch
	// loop reacts near-instantly instead of waiting for the next poll tick; engineOnce ensures
	// StartEngine only spawns one loop goroutine even if called more than once.
	wakeCh     chan struct{}
	engineOnce sync.Once
	// engineExcluded holds taskTypes the engine should NOT dispatch — a rollout safety valve
	// (see ExcludeAllRegisteredExcept) used while migrating handlers off direct execution
	// (raw goroutines / RunTracked) one at a time. Empty set = engine dispatches everything
	// registered in resumeFns, which is the end state once migration is complete.
	engineExcluded sync.Map // taskType string → struct{}
}

func NewTaskService(repo *repository.TaskRepository) *TaskService {
	svc := &TaskService{
		repo:    repo,
		stopCh:  make(chan struct{}),
		rootCtx: context.Background(), // default; overridden by Boot(ctx)
		wakeCh:  make(chan struct{}, 1),
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

// WithNotificationService enables in-app notifications when tasks fail.
func (s *TaskService) WithNotificationService(svc *NotificationService) *TaskService {
	s.notifSvc = svc
	return s
}

// WithTenantUserRepo allows resolving tenant → user IDs for failure notifications.
func (s *TaskService) WithTenantUserRepo(r *repository.TenantUserRepository) *TaskService {
	s.tenantUserRepo = r
	return s
}

const redisChanTaskCancel = "inkframe:task:cancel"

// WithRedis enables cross-instance task cancellation via Redis Pub/Sub.
// When Cancel() is called on any instance, the cancellation is broadcast to all
// other instances so their in-flight goroutines also receive context cancellation.
func (s *TaskService) WithRedis(c *redis.Client) *TaskService {
	s.cache = c
	if c != nil {
		go s.startCancelSubscriber()
	}
	return s
}

func (s *TaskService) startCancelSubscriber() {
	sub := s.cache.Subscribe(context.Background(), redisChanTaskCancel)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.cancelLocalTask(msg.Payload)
		case <-s.stopCh:
			return
		}
	}
}

// cancelLocalTask invokes the in-process cancel function for taskID if registered.
func (s *TaskService) cancelLocalTask(taskID string) {
	if fn, ok := s.cancelFns.Load(taskID); ok {
		if cancel, ok := fn.(context.CancelFunc); ok {
			cancel()
			logger.Printf("[TaskService] cancelled local goroutine for task %s (cross-instance signal)", taskID)
		}
	}
}

// Boot prepares the service for a fresh process start. Must be called after all
// RegisterResumeHandler calls so that resumable task types are already registered, and before
// StartEngine (see task_engine.go) so the engine's first dispatch cycle sees the reset rows.
// The provided ctx is stored as the service root context so that engine-dispatched task
// goroutines inherit it (and are cancelled when the server shuts down).
//
// Dispatch/execution of pending work is no longer done here — that is StartEngine's job.
// Boot's only remaining responsibility is state repair: any task still marked "running" is a
// leftover from a previous instance that died mid-task (this process just started, so nothing
// in it could have set that status), so it's reset to "pending" and picked up by the engine's
// normal ClaimForResume-gated dispatch path like any other pending task.
func (s *TaskService) Boot(ctx context.Context) {
	s.rootCtx = ctx
	if n, err := s.repo.ResetRunningToPending(); err != nil {
		logger.Errorf("[TaskService] Boot: ResetRunningToPending: %v", err)
	} else if n > 0 {
		logger.Printf("[TaskService] Boot: reset %d orphaned running task(s) to pending", n)
	}
}

// RegisterResumeHandler registers the function used to execute a task of the given type.
// Despite the name, this is now the single executor registry used for both first dispatch
// (by the task engine, see task_engine.go) and crash recovery — a resumed task and a freshly
// created one are dispatched through the exact same path, so there is only one function to
// register and only one place that needs to stay correct. fn receives the full AsyncTask
// (including ParamsJSON) and must not assume any HTTP request context is available.
//
// fn also receives a cancellable context.Context, derived by the task engine's claimAndRun
// (bounded by taskEngineHardTimeout and cancelled when a user calls POST /tasks/:id/cancel).
// Implementations MUST thread this ctx down to any AI provider / outbound HTTP call (via the
// *Ctx variant of the relevant service method) for cancellation to actually stop in-flight
// work — passing context.Background() instead silently defeats cancellation, which used to be
// a codebase-wide bug (the ctx this func now receives was previously computed and discarded).
func (s *TaskService) RegisterResumeHandler(taskType string, fn func(context.Context, *model.AsyncTask)) {
	s.resumeFns.Store(taskType, fn)
}

// ExcludeAllRegisteredExcept is a rollout safety valve for the task engine migration: it marks
// every currently-registered task type as excluded from engine dispatch except the ones listed
// in allow. Call once at startup, after all RegisterResumeHandler calls, before StartEngine.
// As each task type's handlers are migrated off direct execution (raw goroutines / RunTracked)
// onto "Create-only", call IncludeInEngine for that type to let the engine take over dispatching
// it. Once every type has been migrated, this call (and the exclusion mechanism) can be deleted.
func (s *TaskService) ExcludeAllRegisteredExcept(allow ...string) {
	allowSet := make(map[string]bool, len(allow))
	for _, t := range allow {
		allowSet[t] = true
	}
	s.resumeFns.Range(func(k, _ interface{}) bool {
		t := k.(string)
		if !allowSet[t] {
			s.engineExcluded.Store(t, struct{}{})
		}
		return true
	})
}

// SetParams persists arbitrary resume parameters for a task as JSON.
func (s *TaskService) SetParams(taskID string, params interface{}) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.repo.UpdateFields(taskID, map[string]interface{}{"params": string(b)})
}

// Shutdown stops background goroutines. Call on server exit.
func (s *TaskService) Shutdown() {
	close(s.stopCh)
}

// maxQueuedTasksPerTenant is the maximum number of pending/running tasks allowed per tenant.
const maxQueuedTasksPerTenant = 10000

// Create inserts a new pending task and returns it.
func (s *TaskService) Create(tenantID uint, taskType, title, entityType string, entityID uint) (*model.AsyncTask, error) {
	return s.CreateWithParams(tenantID, taskType, title, entityType, entityID, nil)
}

// CreateWithParams inserts a new pending task with its resume params already populated in the
// same INSERT, then wakes the task engine. Callers whose executor (see task_resume.go) reads
// params (e.g. video_id) MUST use this instead of Create()+SetParams(): Create() wakes the
// engine immediately, and the engine can claim and execute the task before a subsequent
// SetParams() call has committed, reading a still-empty ParamsJSON and failing the task on a
// missing-param check. Passing params here means the engine never sees the row without them.
func (s *TaskService) CreateWithParams(tenantID uint, taskType, title, entityType string, entityID uint, params interface{}) (*model.AsyncTask, error) {
	// Enforce per-tenant queue size limit to prevent resource exhaustion.
	if count, err := s.repo.CountActive(tenantID); err != nil {
		logger.Errorf("[TaskService] queue size check failed for tenant %d: %v", tenantID, err)
	} else if count >= maxQueuedTasksPerTenant {
		return nil, fmt.Errorf("task queue full (%d tasks pending/running); try again later", count)
	}

	prefix := taskType
	if len(taskType) >= 2 {
		prefix = taskType[:2]
	}
	paramsJSON := ""
	if params != nil {
		if b, err := json.Marshal(params); err != nil {
			logger.Errorf("[TaskService] CreateWithParams: marshal params for %s: %v", taskType, err)
		} else {
			paramsJSON = string(b)
		}
	}
	task := &model.AsyncTask{
		TaskID:     prefix + "-" + uuid.New().String()[:8],
		TenantID:   tenantID,
		Type:       taskType,
		Status:     model.StatusPending,
		Title:      title,
		EntityType: entityType,
		EntityID:   entityID,
		ParamsJSON: paramsJSON,
	}
	if err := s.repo.Create(task); err != nil {
		return nil, err
	}
	metrics.TaskCreatedTotal.WithLabelValues(taskType).Inc()
	s.wake() // nudge the task engine so it dispatches this task without waiting for the next poll tick
	return task, nil
}

// GetLatestByTypeAndEntity 返回指定类型+实体最近一次创建的任务（不存在时返回 gorm.ErrRecordNotFound）。
// 供调用方做"冷却期"判断，避免同一实体在短时间内被重复触发同类任务。
func (s *TaskService) GetLatestByTypeAndEntity(taskType, entityType string, entityID uint) (*model.AsyncTask, error) {
	return s.repo.GetLatestByTypeAndEntity(taskType, entityType, entityID)
}

// SetRunning transitions the task to running.
func (s *TaskService) SetRunning(taskID string) error {
	if err := s.repo.UpdateFields(taskID, map[string]interface{}{"status": "running"}); err != nil {
		logger.Errorf("[TaskService] SetRunning(%s): %v", taskID, err)
		return err
	}
	return nil
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
	if err := s.repo.UpdateFields(taskID, map[string]interface{}{"progress": progress}); err != nil {
		logger.Errorf("[TaskService] UpdateProgress(%s, %d): %v", taskID, progress, err)
		return err
	}
	return nil
}

// UpdateProgressAndTitle updates progress percentage and the human-readable task title together.
// Used by long-running batch tasks to surface per-step status to the frontend.
func (s *TaskService) UpdateProgressAndTitle(taskID string, progress int, title string) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 99 {
		progress = 99
	}
	return s.repo.UpdateFields(taskID, map[string]interface{}{"progress": progress, "title": title})
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
	if err := s.repo.CompleteIfNotCancelled(taskID, resultJSON); err != nil {
		logger.Errorf("[TaskService] Complete(%s): %v", taskID, err)
		return err
	}
	taskType := taskTypeFromID(taskID)
	metrics.TaskCompletedTotal.WithLabelValues(taskType, "success").Inc()
	return nil
}

// Fail records the error message and marks the task failed.
// No-op if the task has already been cancelled.
func (s *TaskService) Fail(taskID string, errMsg string) error {
	if err := s.repo.FailIfNotCancelled(taskID, errMsg); err != nil {
		logger.Errorf("[TaskService] Fail(%s) reason=%q: %v", taskID, errMsg, err)
		return err
	}
	taskType := taskTypeFromID(taskID)
	metrics.TaskCompletedTotal.WithLabelValues(taskType, "error").Inc()
	// Send in-app failure notification asynchronously (best-effort).
	if s.notifSvc != nil && s.tenantUserRepo != nil {
		go func() {
			task, err2 := s.repo.GetByTaskID(taskID)
			if err2 != nil {
				return
			}
			s.sendFailureNotification(task, errMsg)
		}()
	}
	return nil
}

// CompletePartial marks a task completed but attaches a non-fatal warning distinct from Fail().
// Use when a task produced a usable-but-degraded result (e.g. storyboard generation that only
// produced some of the requested shots after exhausting retries on some segments, or a
// multi-step pipeline where some steps failed but the overall result is still usable).
func (s *TaskService) CompletePartial(taskID string, result interface{}, warning string) error {
	resultJSON := ""
	if result != nil {
		if b, err := json.Marshal(result); err == nil {
			resultJSON = string(b)
		}
	}
	if err := s.repo.CompletePartialIfNotCancelled(taskID, resultJSON, warning); err != nil {
		logger.Errorf("[TaskService] CompletePartial(%s): %v", taskID, err)
		return err
	}
	taskType := taskTypeFromID(taskID)
	metrics.TaskCompletedTotal.WithLabelValues(taskType, "partial").Inc()
	return nil
}

// taskTypeFromID extracts the task type prefix from a task ID (e.g. "st-abc12345" → "st").
func taskTypeFromID(taskID string) string {
	if i := strings.Index(taskID, "-"); i > 0 {
		return taskID[:i]
	}
	return "unknown"
}

// sendFailureNotification sends a failure in-app notification to all users in the task's tenant.
func (s *TaskService) sendFailureNotification(task *model.AsyncTask, errMsg string) {
	users, err := s.tenantUserRepo.ListByTenant(task.TenantID)
	if err != nil || len(users) == 0 {
		return
	}
	typeLabel := taskTypeLabelForNotif(task.Type)
	title := fmt.Sprintf("任务失败：%s", task.Title)
	body := fmt.Sprintf("类型：%s\n错误：%s", typeLabel, errMsg)
	for _, tu := range users {
		_ = s.notifSvc.Send(task.TenantID, tu.UserID, "task_failed", title, body, task.EntityType, task.EntityID, "")
	}
}

// taskTypeLabelForNotif returns a human-readable Chinese label for a task type.
func taskTypeLabelForNotif(t string) string {
	labels := map[string]string{
		TaskTypeStoryboardGen:        "分镜生成",
		TaskTypeChapterGen:           "章节生成",
		TaskTypeVoiceGen:             "配音",
		TaskTypeImageGen:             "图像",
		TaskTypeThreeView:            "三视图",
		TaskTypeCharGen:              "角色生成",
		TaskTypeItemExtract:          "道具提取",
		TaskTypeChapterItemExtract:   "道具提取",
		TaskTypeScreenplayGen:        "剧本生成",
		TaskTypePlotExtract:          "情节提取",
		TaskTypeAssetGen:             "素材",
		TaskTypeSceneAnchorExtract:   "场景提取",
		TaskTypeChapterSummaryBatch:  "批量摘要",
		TaskTypeSFXGen:               "音效",
		TaskTypeChapterReview:        "章节审查",
		TaskTypeChapterReviewBatch:   "批量审查",
		TaskTypeStoryboardReview:     "分镜审查",
		TaskTypeStoryboardOptimize:   "分镜优化",
		TaskTypeImport:               "导入",
		TaskTypeNovelAnalysis:        "小说分析",
		TaskTypeRewriteAnalysis:      "改写分析",
		TaskTypeRewriteChapters:      "改写章节",
		TaskTypeCrawlJob:             "爬取任务",
		TaskTypeSkillGen:             "技能生成",
		TaskTypeBatchChapterGen:      "批量生成",
		TaskTypeCharReanalyze:        "角色重分析",
		TaskTypeNovelOutlineGen:      "生成大纲",
		TaskTypeCharImageGen:         "角色图像",
		TaskTypeCoverImageGen:        "封面生成",
		TaskTypeImageEdit:            "图像编辑",
		TaskTypeImageUpscale:         "图像放大",
		TaskTypeVideoGen:             "视频生成",
		TaskTypeVideoSynthesis:       "视频合成",
		TaskTypeChapterPostProcess:   "章节后处理",
	}
	if l, ok := labels[t]; ok {
		return l
	}
	return t
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
// When Redis is wired, the cancellation is also broadcast to all other instances.
func (s *TaskService) Cancel(taskID string) error {
	s.cancelLocalTask(taskID)
	if s.cache != nil {
		_ = s.cache.Publish(context.Background(), redisChanTaskCancel, taskID).Err()
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

// List returns paginated tasks for a tenant. When taskType is empty, quietTaskTypes are
// excluded so the default task list/panel doesn't surface background continuation work the
// user never explicitly asked to track (see quietTaskTypes doc comment). Pass an explicit
// taskType to query a quiet type directly (e.g. a future debugging UI).
func (s *TaskService) List(tenantID uint, taskType, status string, page, pageSize int) ([]*model.AsyncTask, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return s.repo.ListByTenant(tenantID, taskType, status, quietTaskTypes, page, pageSize)
}

// CancelActiveByEntity cancels all pending/running tasks of the given type for an entity.
// Used before creating a replacement task; cancelled status makes goroutine Complete/Fail no-ops.
func (s *TaskService) CancelActiveByEntity(entityType string, entityID uint, taskType string) {
	if err := s.repo.CancelActiveByEntity(entityType, entityID, taskType); err != nil {
		logger.Errorf("TaskService: CancelActiveByEntity %s/%d/%s: %v", entityType, entityID, taskType, err)
	}
}

// CancelActiveByEntityAndInvoke does everything CancelActiveByEntity does (bulk-marks matching
// pending/running tasks as cancelled), and additionally invokes each matching task's registered
// cancel function (if any), so in-flight goroutines actually stop instead of just having their
// DB row marked "cancelled" while the underlying work (e.g. an AI call) keeps running to
// completion. Prefer this over CancelActiveByEntity whenever the caller wants a genuine
// "stop the old one" semantics (e.g. superseding an in-progress generation with a new request).
func (s *TaskService) CancelActiveByEntityAndInvoke(entityType string, entityID uint, taskType string) {
	ids, err := s.repo.ListActiveTaskIDsByEntity(entityType, entityID, taskType)
	if err != nil {
		logger.Errorf("TaskService: CancelActiveByEntityAndInvoke list %s/%d/%s: %v", entityType, entityID, taskType, err)
	}
	s.CancelActiveByEntity(entityType, entityID, taskType)
	for _, id := range ids {
		s.cancelLocalTask(id)
		if s.cache != nil {
			_ = s.cache.Publish(context.Background(), redisChanTaskCancel, id).Err()
		}
	}
}

// failStaleTasks marks pending/running tasks not updated since `before` as failed.
// Dispatch of pending work is handled exclusively by the task engine's continuous
// wake+poll loop now (task_engine.go) — this only does cleanup: a task that is still
// "pending"/"running" and hasn't been touched in `age` is either stuck behind a bug, was
// dropped by a crashed instance with no other instance around to claim it, or exceeded its
// own hard timeout without reporting back. Formerly named recoverOrphaned; it also used to
// re-dispatch tasks itself (duplicating what the engine now does continuously) — that part
// was removed to avoid two dispatch paths racing/disagreeing about who owns a task.
func (s *TaskService) failStaleTasks(age time.Duration) {
	before := time.Now().Add(-age)
	// Heartbeat all tasks currently running on this instance first so they are not
	// falsely considered stale by MarkStaleRunning (cross-instance safety).
	s.heartbeatRunning()
	n, err := s.repo.MarkStaleRunning(before)
	if err != nil {
		logger.Errorf("TaskService: failStaleTasks error: %v", err)
	} else if n > 0 {
		logger.Errorf("TaskService: marked %d stale task(s) as failed", n)
	}
}

// heartbeatRunning refreshes updated_at for all tasks currently running on this instance.
// Called before MarkStaleRunning so that long-running tasks on this instance are not
// incorrectly marked as failed by another instance running the periodic cleanup.
func (s *TaskService) heartbeatRunning() {
	var ids []string
	s.cancelFns.Range(func(k, _ interface{}) bool {
		if id, ok := k.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	if len(ids) == 0 || s.db == nil {
		return
	}
	if err := s.db.Model(&model.AsyncTask{}).
		Where("task_id IN ?", ids).
		Update("updated_at", time.Now()).Error; err != nil {
		logger.Errorf("[TaskService] heartbeatRunning: %v", err)
	}
}

// runCleanup deletes completed/failed tasks older than 7 days, fails stale
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
				logger.Errorf("TaskService: cleanup error: %v", err)
			}
			// Fail tasks stuck in "running" for more than 2h (no heartbeat).
			s.failStaleTasks(2 * time.Hour)
			// Expire tasks stuck in "pending" for more than 1h (never picked up).
			pendingCutoff := time.Now().Add(-1 * time.Hour)
			if n, err := s.repo.MarkStalePending(pendingCutoff); err != nil {
				logger.Errorf("TaskService: expire stale pending tasks error: %v", err)
			} else if n > 0 {
				logger.Printf("TaskService: expired %d stale pending task(s)", n)
			}
			// Clean up old webhook delivery records (keep last 30 days)
			if s.db != nil {
				cutoff := time.Now().AddDate(0, 0, -30)
				s.db.Where("created_at < ?", cutoff).Delete(&model.WebhookDelivery{})
			}
		case <-s.stopCh:
			return
		}
	}
}
