package handler

import "github.com/google/uuid"

// Task status constants shared across all async task handlers.
const (
	taskStatusPending   = "pending"
	taskStatusRunning   = "running"
	taskStatusCompleted = "completed"
	taskStatusFailed    = "failed"
)

// newTaskID returns a unique task identifier with the given prefix (e.g. "gen", "sb").
func newTaskID(prefix string) string {
	return prefix + "-" + uuid.New().String()[:8]
}
