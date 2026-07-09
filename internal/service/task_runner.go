package service

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// TrackedResult is returned by a RunTracked work function to distinguish full success,
// partial/degraded success, and hard failure without overloading `error`.
type TrackedResult struct {
	Data    interface{} // marshalled into AsyncTask.ResultJSON on success or partial success
	Warning string      // non-empty => CompletePartial instead of Complete
}

// RunTrackedOptions configures optional RunTracked behavior. Zero value is safe (no timeout,
// no concurrency cap, default panic message).
type RunTrackedOptions struct {
	// Timeout, if >0, bounds the derived context in addition to cancellation.
	Timeout time.Duration
	// OnPanicMessage overrides the default Chinese panic message stored via Fail().
	OnPanicMessage string
	// MaxConcurrency, if >0, caps the number of concurrently-running RunTracked goroutines
	// sharing SemaphoreKey. This is a lightweight backstop against runaway concurrency for a
	// given task type — not a fair per-tenant scheduler. A task waiting for a free slot stays
	// in "pending" (SetRunning is only called once the slot is acquired) and exits cleanly
	// without ever running if its context is cancelled while queued.
	MaxConcurrency int
	// SemaphoreKey groups tasks sharing the same concurrency cap. Defaults to task.Type when
	// MaxConcurrency > 0 and this is left empty. Ignored when MaxConcurrency <= 0.
	SemaphoreKey string
}

// RunTracked collapses the repeated boilerplate of:
//
//	go func(taskID){ defer recover(); SetRunning(); work(); Complete/Fail }()
//
// into one call. It:
//  1. Derives a cancellable context from parentCtx (or context.Background() if nil),
//     optionally bounded by opts.Timeout.
//  2. Registers/deregisters the cancel func with RegisterCancel/DeregisterCancel so the
//     generic POST /tasks/:id/cancel endpoint and cross-instance Redis broadcast work.
//  3. Optionally waits for a concurrency slot (opts.MaxConcurrency) before running.
//  4. Spawns exactly one goroutine that recovers panics (mapped to Fail, never crashes the process).
//  5. Calls SetRunning(task.TaskID) once it actually starts running fn.
//  6. Maps the (*TrackedResult, error) returned by fn into Complete / CompletePartial / Fail.
//
// fn receives the cancellable ctx and the *model.AsyncTask (so it can read EntityID/ParamsJSON).
// RunTracked returns immediately (non-blocking) — the goroutine runs in the background.
func (s *TaskService) RunTracked(
	parentCtx context.Context,
	task *model.AsyncTask,
	fn func(ctx context.Context, task *model.AsyncTask) (*TrackedResult, error),
	opts ...RunTrackedOptions,
) {
	var o RunTrackedOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if o.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parentCtx, o.Timeout)
	} else {
		ctx, cancel = context.WithCancel(parentCtx)
	}
	s.RegisterCancel(task.TaskID, cancel)

	go func() {
		defer cancel()
		defer s.DeregisterCancel(task.TaskID)
		defer func() {
			if r := recover(); r != nil {
				msg := o.OnPanicMessage
				if msg == "" {
					msg = "内部错误，请重试"
				}
				logger.Errorf("[TaskService] RunTracked task %s panic: %v\n%s", task.TaskID, r, debug.Stack())
				_ = s.Fail(task.TaskID, msg)
			}
		}()

		if o.MaxConcurrency > 0 {
			key := o.SemaphoreKey
			if key == "" {
				key = task.Type
			}
			if !s.acquireSlot(ctx, key, o.MaxConcurrency) {
				// ctx was cancelled while queued — task row is already cancelled (or will be),
				// nothing to run, nothing to report.
				return
			}
			defer s.releaseSlot(key)
		}

		_ = s.SetRunning(task.TaskID)
		result, err := fn(ctx, task)
		if err != nil {
			_ = s.Fail(task.TaskID, err.Error())
			return
		}
		if result != nil && result.Warning != "" {
			_ = s.CompletePartial(task.TaskID, result.Data, result.Warning)
			return
		}
		var data interface{}
		if result != nil {
			data = result.Data
		}
		_ = s.Complete(task.TaskID, data)
	}()
}

// acquireSlot blocks until a concurrency slot for key is available or ctx is done.
// Returns false if ctx was cancelled before a slot opened up.
func (s *TaskService) acquireSlot(ctx context.Context, key string, max int) bool {
	v, _ := s.semaphores.LoadOrStore(key, make(chan struct{}, max))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseSlot releases a previously-acquired concurrency slot for key.
func (s *TaskService) releaseSlot(key string) {
	if v, ok := s.semaphores.Load(key); ok {
		sem := v.(chan struct{})
		select {
		case <-sem:
		default:
		}
	}
}
