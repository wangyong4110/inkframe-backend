package handler

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Task status constants shared across all async task handlers.
const (
	taskStatusPending   = "pending"
	taskStatusRunning   = "running"
	taskStatusCompleted = "completed"
	taskStatusFailed    = "failed"
)

// AsyncTask is the generic async task object returned to callers.
// Handler-specific result payload is stored in Data.
type AsyncTask struct {
	TaskID    string      `json:"task_id"`
	Status    string      `json:"status"` // pending / running / completed / failed
	Data      interface{} `json:"data,omitempty"`
	Error     string      `json:"error,omitempty"`
	CreatedAt int64       `json:"created_at"`
	UpdatedAt int64       `json:"updated_at"`
}

// newTaskID returns a unique task identifier with the given prefix (e.g. "gen", "img").
func newTaskID(prefix string) string {
	return prefix + "-" + uuid.New().String()[:8]
}

// TaskStore is a TTL-bounded in-memory store for AsyncTask objects.
// Tasks older than ttl are automatically evicted every 5 minutes.
type TaskStore struct {
	m   sync.Map
	ttl time.Duration
}

// newTaskStore creates a TaskStore and starts its background cleanup goroutine.
func newTaskStore(ttl time.Duration) *TaskStore {
	ts := &TaskStore{ttl: ttl}
	go ts.runCleanup()
	return ts
}

func (ts *TaskStore) store(task *AsyncTask) {
	task.UpdatedAt = time.Now().Unix()
	ts.m.Store(task.TaskID, task)
}

func (ts *TaskStore) load(id string) (*AsyncTask, bool) {
	v, ok := ts.m.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*AsyncTask), true
}

func (ts *TaskStore) runCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		cutoff := time.Now().Add(-ts.ttl).Unix()
		ts.m.Range(func(k, v interface{}) bool {
			if t := v.(*AsyncTask); t.CreatedAt < cutoff {
				ts.m.Delete(k)
			}
			return true
		})
	}
}

// Package-level task stores — one per async operation type, 24-hour TTL.
var (
	chapterGenTasks = newTaskStore(24 * time.Hour)
	itemImageTasks  = newTaskStore(24 * time.Hour)
	threeViewTasks  = newTaskStore(24 * time.Hour)
	storyboardTasks = newTaskStore(24 * time.Hour)
)
