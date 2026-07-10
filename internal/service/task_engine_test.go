package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"gorm.io/gorm"
)

const testTaskType = "test_engine_task"

// newTestTaskService spins up an in-memory SQLite-backed TaskService for engine tests. SQLite
// is test-only (see go.mod) — the real server always runs against MySQL; the two are close
// enough in the plain SQL this repository issues (no MySQL-specific syntax) that this is a
// faithful stand-in for exercising ClaimForResume's atomic claim semantics, which is the one
// property these tests exist to prove.
func newTestTaskService(t *testing.T) (*TaskService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.AsyncTask{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	repo := repository.NewTaskRepository(db)
	svc := NewTaskService(repo)
	t.Cleanup(svc.Shutdown)
	return svc, db
}

// withPollInterval temporarily overrides the engine's poll interval for the duration of a test,
// restoring the previous value on cleanup. Tests must not run in parallel with each other since
// this mutates package-level state.
func withPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := taskEnginePollInterval
	taskEnginePollInterval = d
	t.Cleanup(func() { taskEnginePollInterval = prev })
}

func withConcurrencyLimit(t *testing.T, n int) {
	t.Helper()
	prev := defaultTaskEngineConcurrency
	defaultTaskEngineConcurrency = n
	t.Cleanup(func() { defaultTaskEngineConcurrency = prev })
}

// TestTaskEngine_WakeDispatchesQuickly verifies that Create() nudges the engine to dispatch
// near-instantly, rather than waiting for the poll fallback. The poll interval is set very
// long, so if the executor runs quickly, it can only have been via the wake path.
func TestTaskEngine_WakeDispatchesQuickly(t *testing.T) {
	withPollInterval(t, time.Hour)
	svc, _ := newTestTaskService(t)

	var calls atomic.Int32
	done := make(chan struct{}, 1)
	svc.RegisterResumeHandler(testTaskType, func(task *model.AsyncTask) {
		calls.Add(1)
		_ = svc.Complete(task.TaskID, nil)
		done <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartEngine(ctx)

	task, err := svc.Create(1, testTaskType, "test", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executor was not called via wake within 2s (poll interval is 1h, so wake must be broken)")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls.Load())
	}

	got, err := svc.GetUnscoped(task.TaskID)
	if err != nil {
		t.Fatalf("GetUnscoped: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("expected status completed, got %q", got.Status)
	}
}

// TestTaskEngine_PollFallbackCatchesMissedWake verifies that a task inserted without going
// through Create() (simulating a task created by a different instance, whose wake signal this
// instance never saw) is still eventually picked up — by the poll fallback.
func TestTaskEngine_PollFallbackCatchesMissedWake(t *testing.T) {
	withPollInterval(t, 100*time.Millisecond)
	svc, db := newTestTaskService(t)

	var calls atomic.Int32
	done := make(chan struct{}, 1)
	svc.RegisterResumeHandler(testTaskType, func(task *model.AsyncTask) {
		calls.Add(1)
		_ = svc.Complete(task.TaskID, nil)
		done <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartEngine(ctx)

	// Insert directly via the DB, bypassing Create()/wake() entirely.
	task := &model.AsyncTask{TaskID: "po-simulated1", TenantID: 1, Type: testTaskType, Status: "pending"}
	if err := db.Create(task).Error; err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("executor was not called via poll fallback within 2s")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls.Load())
	}
}

// TestTaskEngine_ConcurrentWakeAndPollExecuteOnce is the core safety property of the whole
// design: if the wake signal and a poll tick race to dispatch the same task (or dispatchOnce is
// simply called concurrently, simulating multiple instances), ClaimForResume must guarantee it
// only actually runs once.
func TestTaskEngine_ConcurrentWakeAndPollExecuteOnce(t *testing.T) {
	withPollInterval(t, 5*time.Millisecond) // aggressive polling to maximize race pressure
	svc, _ := newTestTaskService(t)

	var calls atomic.Int32
	var completions atomic.Int32
	svc.RegisterResumeHandler(testTaskType, func(task *model.AsyncTask) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the window a real race would need to hit
		_ = svc.Complete(task.TaskID, nil)
		completions.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartEngine(ctx)

	task, err := svc.Create(1, testTaskType, "test", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fire a burst of extra wakes and manual dispatch calls concurrently with the poll ticker,
	// simulating the exact race the design is supposed to prevent.
	for i := 0; i < 20; i++ {
		go svc.wake()
		go svc.dispatchOnce(ctx)
	}

	time.Sleep(300 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 execution despite concurrent wake+poll+manual dispatch, got %d", got)
	}
	if got := completions.Load(); got != 1 {
		t.Fatalf("expected exactly 1 completion, got %d", got)
	}
	got, err := svc.GetUnscoped(task.TaskID)
	if err != nil {
		t.Fatalf("GetUnscoped: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("expected status completed, got %q", got.Status)
	}
}

// TestTaskEngine_CancelBeforeDispatchPreventsExecution verifies that cancelling a task before
// the engine gets to it results in a "cancelled" terminal state, and the registered executor
// never runs (ClaimForResume only claims "pending" rows — a cancelled row is never picked up).
func TestTaskEngine_CancelBeforeDispatchPreventsExecution(t *testing.T) {
	withPollInterval(t, time.Hour) // long enough that only an explicit dispatch can trigger this
	svc, _ := newTestTaskService(t)

	var calls atomic.Int32
	svc.RegisterResumeHandler(testTaskType, func(task *model.AsyncTask) {
		calls.Add(1)
		_ = svc.Complete(task.TaskID, nil)
	})

	task, err := svc.Create(1, testTaskType, "test", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Cancel(task.TaskID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Explicitly force a dispatch cycle (rather than relying on the poll interval) to prove the
	// cancelled row is skipped, not just "hasn't been looked at yet".
	svc.dispatchOnce(context.Background())
	time.Sleep(50 * time.Millisecond)

	if calls.Load() != 0 {
		t.Fatalf("expected executor to never run for a cancelled task, got %d calls", calls.Load())
	}
	got, err := svc.GetUnscoped(task.TaskID)
	if err != nil {
		t.Fatalf("GetUnscoped: %v", err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("expected status cancelled, got %q", got.Status)
	}
}

// TestTaskEngine_ConcurrencyLimitPerTenantType verifies that engine dispatch respects the
// (tenantID, taskType) concurrency cap: no more than the configured limit run at once, and
// tasks that don't fit are not dropped — they run once a slot frees up.
func TestTaskEngine_ConcurrencyLimitPerTenantType(t *testing.T) {
	withPollInterval(t, 30*time.Millisecond)
	withConcurrencyLimit(t, 2)
	svc, _ := newTestTaskService(t)

	var running atomic.Int32
	var maxObservedConcurrent atomic.Int32
	var totalCompleted atomic.Int32
	svc.RegisterResumeHandler(testTaskType, func(task *model.AsyncTask) {
		n := running.Add(1)
		for {
			max := maxObservedConcurrent.Load()
			if n <= max || maxObservedConcurrent.CompareAndSwap(max, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		running.Add(-1)
		_ = svc.Complete(task.TaskID, nil)
		totalCompleted.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartEngine(ctx)

	const numTasks = 6
	for i := 0; i < numTasks; i++ {
		if _, err := svc.Create(1, testTaskType, "test", "", 0); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	deadline := time.After(5 * time.Second)
	for totalCompleted.Load() < numTasks {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for all %d tasks to complete; completed=%d (some tasks were dropped, not just delayed)", numTasks, totalCompleted.Load())
		case <-time.After(20 * time.Millisecond):
		}
	}

	if max := maxObservedConcurrent.Load(); max > 2 {
		t.Fatalf("concurrency limit violated: observed %d tasks running at once, limit was 2", max)
	}
}
