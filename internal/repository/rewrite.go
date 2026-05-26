package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// RewriteProjectRepository handles rewrite project data
type RewriteProjectRepository struct {
	db    *gorm.DB
	redis *redis.Client
}

func NewRewriteProjectRepository(db *gorm.DB, redis *redis.Client) *RewriteProjectRepository {
	return &RewriteProjectRepository{db: db, redis: redis}
}

func (r *RewriteProjectRepository) Create(p *model.RewriteProject) error {
	return r.db.Create(p).Error
}

func (r *RewriteProjectRepository) GetByID(id uint) (*model.RewriteProject, error) {
	var p model.RewriteProject
	if err := r.db.First(&p, id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *RewriteProjectRepository) ListByTenant(tenantID uint, page, pageSize int) ([]*model.RewriteProject, int64, error) {
	var projects []*model.RewriteProject
	var total int64
	offset := (page - 1) * pageSize
	r.db.Model(&model.RewriteProject{}).Where("tenant_id = ?", tenantID).Count(&total)
	err := r.db.Where("tenant_id = ?", tenantID).Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&projects).Error
	return projects, total, err
}

func (r *RewriteProjectRepository) UpdateStatus(id uint, status, errMsg string) error {
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "error_msg": errMsg,
	}).Error
}

func (r *RewriteProjectRepository) UpdateProgress(id uint, done, total int) error {
	progress := 0
	if total > 0 {
		progress = done * 100 / total
	}
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Updates(map[string]interface{}{
		"done_chapters": done, "progress": progress,
	}).Error
}

func (r *RewriteProjectRepository) UpdateTotalChapters(id uint, total int) error {
	return r.db.Model(&model.RewriteProject{}).Where("id = ?", id).Update("total_chapters", total).Error
}

func (r *RewriteProjectRepository) Delete(id uint) error {
	return r.db.Delete(&model.RewriteProject{}, id).Error
}

// LiteraryAnalysisRepository handles literary analysis data
type LiteraryAnalysisRepository struct {
	db *gorm.DB
}

func NewLiteraryAnalysisRepository(db *gorm.DB) *LiteraryAnalysisRepository {
	return &LiteraryAnalysisRepository{db: db}
}

func (r *LiteraryAnalysisRepository) Create(a *model.LiteraryAnalysis) error {
	// Delete any existing record first to handle re-analysis on the same project.
	r.db.Where("project_id = ?", a.ProjectID).Delete(&model.LiteraryAnalysis{})
	return r.db.Create(a).Error
}

func (r *LiteraryAnalysisRepository) GetByProjectID(projectID uint) (*model.LiteraryAnalysis, error) {
	var a model.LiteraryAnalysis
	if err := r.db.Where("project_id = ?", projectID).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// RewriteBibleRepository handles rewrite bible data
type RewriteBibleRepository struct {
	db *gorm.DB
}

func NewRewriteBibleRepository(db *gorm.DB) *RewriteBibleRepository {
	return &RewriteBibleRepository{db: db}
}

func (r *RewriteBibleRepository) Create(b *model.RewriteBible) error {
	// Delete any existing record first to handle re-analysis on the same project.
	r.db.Where("project_id = ?", b.ProjectID).Delete(&model.RewriteBible{})
	return r.db.Create(b).Error
}

func (r *RewriteBibleRepository) GetByProjectID(projectID uint) (*model.RewriteBible, error) {
	var b model.RewriteBible
	if err := r.db.Where("project_id = ?", projectID).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *RewriteBibleRepository) Update(b *model.RewriteBible) error {
	return r.db.Save(b).Error
}

func (r *RewriteBibleRepository) UpdateFields(projectID uint, fields map[string]interface{}) error {
	return r.db.Model(&model.RewriteBible{}).Where("project_id = ?", projectID).Updates(fields).Error
}

// ChapterRewriteTaskRepository handles chapter rewrite task data
type ChapterRewriteTaskRepository struct {
	db *gorm.DB
}

func NewChapterRewriteTaskRepository(db *gorm.DB) *ChapterRewriteTaskRepository {
	return &ChapterRewriteTaskRepository{db: db}
}

func (r *ChapterRewriteTaskRepository) Create(t *model.ChapterRewriteTask) error {
	return r.db.Create(t).Error
}

func (r *ChapterRewriteTaskRepository) DeleteByProjectID(projectID uint) error {
	return r.db.Where("project_id = ?", projectID).Delete(&model.ChapterRewriteTask{}).Error
}

func (r *ChapterRewriteTaskRepository) GetByID(id uint) (*model.ChapterRewriteTask, error) {
	var t model.ChapterRewriteTask
	if err := r.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *ChapterRewriteTaskRepository) ListByProject(projectID uint) ([]*model.ChapterRewriteTask, error) {
	var tasks []*model.ChapterRewriteTask
	err := r.db.Where("project_id = ?", projectID).Order("chapter_no ASC").Find(&tasks).Error
	return tasks, err
}

func (r *ChapterRewriteTaskRepository) UpdateStatus(id uint, status, errMsg string) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "error_msg": errMsg,
	}).Error
}

func (r *ChapterRewriteTaskRepository) UpdateRewritten(id uint, content string, lexSim, structSim float64, passed bool) error {
	combined := (lexSim + structSim) / 2
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"rewritten_content": content,
		"lexical_sim":       lexSim,
		"structural_sim":    structSim,
		"similarity_score":  combined,
		"passed":            passed,
		"status":            "completed",
	}).Error
}
