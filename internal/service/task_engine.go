package service

// task_engine.go
//
// The task engine is the single place that decides *when* a pending task actually runs.
// HTTP handlers (and any other code) are expected to only call TaskService.Create/SetParams
// and then return — never execute the task's work themselves. Create() nudges the engine via
// an in-memory wake channel so dispatch happens near-instantly on the instance that created
// the task; a short poll interval is the fallback for tasks created by another instance (which
// won't see this instance's wake signal) or for a dropped wake signal.
//
// Dispatch reuses the exact same primitives the old crash-recovery path (recoverOrphaned) used:
// TaskRepository.ClaimForResume does an atomic `UPDATE ... WHERE status='pending'` so that if
// the wake signal and a poll tick (or two instances) race to dispatch the same task, only one
// of them actually claims and runs it — the rest see RowsAffected==0 and back off silently.

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// taskEnginePollInterval is the fallback poll cadence. Wake-on-Create covers the common case
// (same-instance dispatch, near-zero latency); this bounds the worst case (task created by a
// different instance, or a dropped wake signal) to a few seconds, which is fine for
// fire-and-forget background work. A var (not const) so tests can override it to isolate the
// wake path from the poll path.
var taskEnginePollInterval = 3 * time.Second

// taskEngineDispatchBatchSize caps how many pending tasks a single dispatch cycle pulls from
// the DB. Tasks left over (because a batch was full, or their concurrency slot was busy) are
// simply picked up again on the next wake/poll — nothing is lost, just delayed.
const taskEngineDispatchBatchSize = 200

// taskEngineHardTimeout bounds how long a single engine-dispatched task may run before its
// context is cancelled, mirroring the timeout the old recoverOrphaned path used for resumed
// tasks. Executor functions (func(*model.AsyncTask), see RegisterResumeHandler) don't take a
// context parameter, so this mostly affects Cancel()/shutdown bookkeeping rather than
// interrupting in-flight work that ignores it — same limitation the old resume path had.
const taskEngineHardTimeout = 30 * time.Minute

// defaultTaskEngineConcurrency is the default cap on concurrently-running tasks per
// (tenantID, taskType) key. Deliberately small: this is a backstop against one tenant's burst
// of task creation (e.g. a bulk batch action) monopolizing goroutines/DB connections, not a
// throughput tuning knob — the real per-model throughput limit lives in ai_service.go's
// (tenantID, modelName) limiter, which this is independent of and does not replace.
// A var (not const) so tests can override it to make the concurrency cap deterministic to assert on.
var defaultTaskEngineConcurrency = 3

// StartEngine launches the task engine's dispatch loop. Safe to call more than once — only the
// first call actually starts the loop. Must be called after all RegisterResumeHandler calls
// (so dispatch has executors to route to) and after Boot (so orphaned "running" rows have
// already been reset to "pending" before the engine's first dispatch cycle runs).
func (s *TaskService) StartEngine(ctx context.Context) {
	s.engineOnce.Do(func() {
		go s.engineLoop(ctx)
	})
}

// wake nudges the engine to dispatch immediately instead of waiting for the next poll tick.
// Non-blocking: multiple wakes between dispatch cycles coalesce into one, and a caller (e.g.
// Create()) never blocks on this.
func (s *TaskService) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default: // a wake is already pending; this dispatch cycle will cover both
	}
}

func (s *TaskService) engineLoop(ctx context.Context) {
	ticker := time.NewTicker(taskEnginePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.wakeCh:
			s.dispatchOnce(ctx)
		case <-ticker.C:
			s.dispatchOnce(ctx)
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// dispatchOnce pulls a batch of pending tasks whose type has a registered executor (and isn't
// currently excluded — see ExcludeAllRegisteredExcept) and, for each one that has a free
// (tenantID, taskType) concurrency slot, spawns a goroutine to claim and run it. Tasks that
// don't fit in this cycle's batch size or find their slot busy are simply left "pending" for
// the next wake/poll to pick up.
func (s *TaskService) dispatchOnce(ctx context.Context) {
	var types []string
	s.resumeFns.Range(func(k, _ interface{}) bool {
		t := k.(string)
		if _, excluded := s.engineExcluded.Load(t); !excluded {
			types = append(types, t)
		}
		return true
	})
	if len(types) == 0 {
		return
	}

	tasks, err := s.repo.ListPending(types, taskEngineDispatchBatchSize)
	if err != nil {
		logger.Errorf("[TaskEngine] ListPending: %v", err)
		return
	}

	for _, t := range tasks {
		fnVal, ok := s.resumeFns.Load(t.Type)
		if !ok {
			continue // registered after ListPending ran, or raced with an unregister — skip, retried next cycle
		}
		fn := fnVal.(func(*model.AsyncTask))
		task := t
		key := fmt.Sprintf("%d:%s", task.TenantID, task.Type)
		if !s.tryAcquireSlot(key, s.concurrencyLimitFor(task.Type)) {
			continue // no free slot for this (tenant, type) right now; retried next cycle
		}
		go func() {
			defer s.releaseSlot(key)
			s.claimAndRun(ctx, task, fn)
		}()
	}
}

// concurrencyLimitFor returns the per-(tenantID, taskType) concurrency cap for taskType.
// Currently a single global default — the hook exists so a future per-type override table
// (e.g. driven by config) can be added without touching call sites.
func (s *TaskService) concurrencyLimitFor(taskType string) int {
	return defaultTaskEngineConcurrency
}

// claimAndRun atomically claims a single pending task (so a racing wake+poll, or another
// instance, cannot also run it) and executes it. Mirrors the claim/cancel/panic-recovery
// pattern the old recoverOrphaned path used for resumed tasks.
func (s *TaskService) claimAndRun(parentCtx context.Context, task *model.AsyncTask, fn func(*model.AsyncTask)) {
	ok, err := s.repo.ClaimForResume(task.TaskID)
	if err != nil {
		logger.Errorf("[TaskEngine] ClaimForResume(%s): %v", task.TaskID, err)
		return
	}
	if !ok {
		// Lost the race to another dispatch cycle or another instance — not an error.
		return
	}

	_, cancel := context.WithTimeout(parentCtx, taskEngineHardTimeout)
	s.RegisterCancel(task.TaskID, cancel)
	defer s.DeregisterCancel(task.TaskID)
	defer cancel()
	defer s.recoverTaskPanic(task.TaskID, "")

	fn(task)
}

// recoverTaskPanic is the shared panic-recovery helper for anything that executes a task's
// registered work function outside of the HTTP request that created it (RunTracked and the
// task engine's claimAndRun): on panic, log the stack and mark the task failed instead of
// crashing the process. msg overrides the default failure message; pass "" to use it.
func (s *TaskService) recoverTaskPanic(taskID string, msg string) {
	if r := recover(); r != nil {
		if msg == "" {
			msg = "内部错误，请重试"
		}
		logger.Errorf("[TaskService] task %s panic: %v\n%s", taskID, r, debug.Stack())
		_ = s.Fail(taskID, msg)
	}
}
