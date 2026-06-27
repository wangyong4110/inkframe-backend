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

func (r *TaskRepository) ListByTenant(tenantID uint, taskType, status string, page, pageSize int) ([]*model.AsyncTask, int64, error) {
	q := r.db.Model(&model.AsyncTask{}).Where("tenant_id = ?", tenantID)
	if taskType != "" {
		q = q.Where("type = ?", taskType)
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

// ListDistinctActiveTenants returns distinct tenant IDs that have pending tasks.
// Used by per-tenant fairness scheduler to round-robin across tenants.
func (r *TaskRepository) ListDistinctActiveTenants() ([]uint, error) {
	var tenantIDs []uint
	err := r.db.Model(&model.AsyncTask{}).
		Select("DISTINCT tenant_id").
		Where("status = ?", "pending").
		Pluck("tenant_id", &tenantIDs).Error
	return tenantIDs, err
}

// ListOrphaned returns pending/running tasks of the given types not updated since `before`.
// Used to find resumable tasks after server restart.
func (r *TaskRepository) ListOrphaned(before time.Time, types []string) ([]*model.AsyncTask, error) {
	var tasks []*model.AsyncTask
	err := r.db.Where(
		"status IN ? AND updated_at < ? AND type IN ?",
		[]string{"pending", "running"}, before, types,
	).Find(&tasks).Error
	return tasks, err
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
// Used by recoverOrphaned to prevent two instances from executing the same task.
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
