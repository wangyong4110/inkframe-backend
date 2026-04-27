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

// DeleteOldCompleted removes completed/failed tasks older than `before`.
func (r *TaskRepository) DeleteOldCompleted(before time.Time) error {
	return r.db.Where("status IN ? AND updated_at < ?", []string{"completed", "failed"}, before).
		Delete(&model.AsyncTask{}).Error
}
