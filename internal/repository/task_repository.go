package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// TaskRepository handles DB persistence for AsyncTask.
type TaskRepository struct {
	db *gorm.DB
}

func NewTaskRepository(db *gorm.DB) *TaskRepository {
	return &TaskRepository{db: db}
}

func (r *TaskRepository) Create(task *model.AsyncTask) error {
	return r.db.Create(task).Error
}

func (r *TaskRepository) Update(task *model.AsyncTask) error {
	return r.db.Save(task).Error
}

func (r *TaskRepository) GetByTaskID(taskID string) (*model.AsyncTask, error) {
	var task model.AsyncTask
	err := r.db.Where("task_id = ?", taskID).First(&task).Error
	return &task, err
}

// excludeTypes is only applied when taskType is empty — an explicit type filter always wins.
func (r *TaskRepository) ListByTenant(tenantID uint, taskType, status string, excludeTypes []string, page, pageSize int) ([]*model.AsyncTask, int64, error) {
	q := r.db.Model(&model.AsyncTask{}).Where("tenant_id = ?", tenantID)
	if taskType != "" {
		q = q.Where("type = ?", taskType)
	} else if len(excludeTypes) > 0 {
		q = q.Where("type NOT IN ?", excludeTypes)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	var tasks []*model.AsyncTask
	err := q.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&tasks).Error
	return tasks, total, err
}

// DeleteOldCompleted removes completed/failed/dead tasks older than `before`.
func (r *TaskRepository) DeleteOldCompleted(before time.Time) error {
	return r.db.Where("status IN ? AND updated_at < ?", []string{"completed", "failed", "dead"}, before).
		Delete(&model.AsyncTask{}).Error
}

// MarkStaleRunning marks pending/running tasks not updated since `before` as failed.
// Used to recover orphaned tasks after server restart or goroutine timeout.
func (r *TaskRepository) MarkStaleRunning(before time.Time) (int64, error) {
	result := r.db.Model(&model.AsyncTask{}).
		Where("status IN ? AND updated_at < ?", []string{"pending", "running"}, before).
		Updates(map[string]interface{}{
			"status": "failed",
			"error":  "任务超时或服务重启，请重新提交",
		})
	return result.RowsAffected, result.Error
}

// ListPending returns up to `limit` pending tasks of the given types, oldest first.
// Used by the task engine's dispatch loop — deliberately NOT filtered by tenant or age
// (unlike ListOrphaned), since the engine dispatches all currently-pending work regardless
// of how recently it was created. Callers must still go through ClaimForResume before
// executing, since a row returned here may already be claimed by another dispatch cycle
// or another instance by the time the caller acts on it.
func (r *TaskRepository) ListPending(types []string, limit int) ([]*model.AsyncTask, error) {
	var tasks []*model.AsyncTask
	err := r.db.Where("status = ? AND type IN ?", "pending", types).
		Order("created_at ASC").
		Limit(limit).
		Find(&tasks).Error
	return tasks, err
}

// ResetRunningToPending resets every task still marked "running" back to "pending".
// Called once at Boot() — any row still "running" at process start is a leftover from a
// previous instance that died mid-task; resetting to pending lets the task engine's normal
// dispatch/ClaimForResume path pick it back up rather than needing separate recovery logic.
func (r *TaskRepository) ResetRunningToPending() (int64, error) {
	result := r.db.Model(&model.AsyncTask{}).
		Where("status = ?", "running").
		Updates(map[string]interface{}{
			"status": "pending",
			"error":  "",
		})
	return result.RowsAffected, result.Error
}

// CancelActiveByEntity cancels all pending/running tasks of the given type for a specific
// entity. Used before creating a replacement task to let zombie goroutines exit gracefully
// (Complete/Fail are no-ops once status is "cancelled").
func (r *TaskRepository) CancelActiveByEntity(entityType string, entityID uint, taskType string) error {
	return r.db.Model(&model.AsyncTask{}).
		Where("entity_type = ? AND entity_id = ? AND type = ? AND status IN ?",
			entityType, entityID, taskType, []string{"pending", "running"}).
		Updates(map[string]interface{}{
			"status": "cancelled",
			"error":  "已被新任务取代",
		}).Error
}

// ListActiveTaskIDsByEntity returns task_ids matching the same filter as CancelActiveByEntity,
// for callers that need to invoke in-process cancel functions after the bulk status update.
// Must be called BEFORE CancelActiveByEntity's UPDATE so the WHERE clause still matches
// (rows are pending/running at query time).
func (r *TaskRepository) ListActiveTaskIDsByEntity(entityType string, entityID uint, taskType string) ([]string, error) {
	var ids []string
	err := r.db.Model(&model.AsyncTask{}).
		Where("entity_type = ? AND entity_id = ? AND type = ? AND status IN ?",
			entityType, entityID, taskType, []string{"pending", "running"}).
		Pluck("task_id", &ids).Error
	return ids, err
}

// UpdateFields 仅更新指定字段（避免 GetByTaskID + Update 两次 DB 操作）
func (r *TaskRepository) UpdateFields(taskID string, fields map[string]interface{}) error {
	return r.db.Model(&model.AsyncTask{}).Where("task_id = ?", taskID).Updates(fields).Error
}

// CompleteIfNotCancelled atomically completes a task only if it's not already cancelled.
// The resultJSON parameter must be the JSON-encoded result string (column name: result).
func (r *TaskRepository) CompleteIfNotCancelled(taskID string, resultJSON string) error {
	return r.db.Model(&model.AsyncTask{}).
		Where("task_id = ? AND status != ?", taskID, "cancelled").
		Updates(map[string]interface{}{
			"status":   "completed",
			"progress": 100,
			"result":   resultJSON,
		}).Error
}

// CompletePartialIfNotCancelled atomically completes a task with a non-fatal warning message,
// distinct from FailIfNotCancelled (status stays "completed", not "failed"). The warning is
// stored in the same `error` column as hard failures; callers/UI distinguish "partial success"
// from "hard failure" by checking status alongside error, not by a separate status value.
func (r *TaskRepository) CompletePartialIfNotCancelled(taskID string, resultJSON, warning string) error {
	return r.db.Model(&model.AsyncTask{}).
		Where("task_id = ? AND status != ?", taskID, "cancelled").
		Updates(map[string]interface{}{
			"status":   "completed",
			"progress": 100,
			"result":   resultJSON,
			"error":    warning,
		}).Error
}

// FailIfNotCancelled atomically fails a task only if it's not already cancelled.
func (r *TaskRepository) FailIfNotCancelled(taskID string, errMsg string) error {
	return r.db.Model(&model.AsyncTask{}).
		Where("task_id = ? AND status != ?", taskID, "cancelled").
		Updates(map[string]interface{}{
			"status": "failed",
			"error":  errMsg,
		}).Error
}

// CancelIfActive cancels a task only if it's still pending or running.
func (r *TaskRepository) CancelIfActive(taskID string) error {
	return r.db.Model(&model.AsyncTask{}).
		Where("task_id = ? AND status IN ?", taskID, []string{"pending", "running"}).
		Update("status", "cancelled").Error
}

// ClaimForResume atomically transitions a task from pending → running.
// Returns (true, nil) only when this instance wins the claim (rowsAffected==1).
// Returns (false, nil) when another instance already claimed it.
// Used by the task engine's dispatch loop (task_engine.go) to guarantee that a task is only
// ever executed once, even if the engine's wake signal and poll tick race to dispatch it, or
// two instances both try to.
func (r *TaskRepository) ClaimForResume(taskID string) (bool, error) {
	result := r.db.Model(&model.AsyncTask{}).
		Where("task_id = ? AND status = ?", taskID, "pending").
		Update("status", "running")
	return result.RowsAffected == 1, result.Error
}

// CountActive returns the number of pending/running tasks for a tenant.
func (r *TaskRepository) CountActive(tenantID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.AsyncTask{}).
		Where("tenant_id = ? AND status IN ?", tenantID, []string{"pending", "running"}).
		Count(&count).Error
	return count, err
}

// MarkStalePending marks pending tasks last updated before `before` as failed (expired in queue).
// Uses updated_at (not created_at) so that tasks freshly set to "pending" by recoverOrphaned
// are not immediately killed by the cleanup cycle that runs right after.
func (r *TaskRepository) MarkStalePending(before time.Time) (int64, error) {
	result := r.db.Model(&model.AsyncTask{}).
		Where("status = 'pending' AND updated_at < ?", before).
		Updates(map[string]interface{}{
			"status": "failed",
			"error":  "task expired in queue",
		})
	return result.RowsAffected, result.Error
}

// GetLatestByTypeAndEntity returns the most recently created task of the given type for an entity.
// entityType and entityID are matched against the entity_type and entity_id columns.
func (r *TaskRepository) GetLatestByTypeAndEntity(taskType, entityType string, entityID uint) (*model.AsyncTask, error) {
	var task model.AsyncTask
	err := r.db.
		Where("type = ? AND entity_type = ? AND entity_id = ?", taskType, entityType, entityID).
		Order("created_at DESC").
		First(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}
