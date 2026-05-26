package repository

import (
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

func (r *LiteraryAnalysisRepository) DeleteByProjectID(projectID uint) error {
	return r.db.Where("project_id = ?", projectID).Delete(&model.LiteraryAnalysis{}).Error
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

func (r *RewriteBibleRepository) DeleteByProjectID(projectID uint) error {
	return r.db.Where("project_id = ?", projectID).Delete(&model.RewriteBible{}).Error
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

// ResetStaleRewriting resets chapters stuck in "rewriting" state back to "pending".
// Called at the start of StartRewriting to recover from a previous interrupted run.
func (r *ChapterRewriteTaskRepository) ResetStaleRewriting(projectID uint) error {
	return r.db.Model(&model.ChapterRewriteTask{}).
		Where("project_id = ? AND status = ?", projectID, "rewriting").
		Update("status", "pending").Error
}

// SaveAttempt stores the AI-generated content in AttemptContent without touching
// RewrittenContent or changing status. Safe to call on every attempt.
func (r *ChapterRewriteTaskRepository) SaveAttempt(id uint, content string) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).
		Update("attempt_content", content).Error
}

// AcceptAttempt promotes AttemptContent → RewrittenContent and marks the task completed.
// Also records similarity scores. Call only when the attempt passes quality gates.
func (r *ChapterRewriteTaskRepository) AcceptAttempt(id uint, lexSim, structSim float64, passed bool) error {
	combined := (lexSim + structSim) / 2

	// Copy attempt_content into rewritten_content in a single UPDATE
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"rewritten_content": gorm.Expr("attempt_content"),
		"lexical_sim":       lexSim,
		"structural_sim":    structSim,
		"similarity_score":  combined,
		"passed":            passed,
		"status":            "completed",
	}).Error
}

// UpdateRewritten is the legacy method kept for backward compatibility.
// New code should use SaveAttempt + AcceptAttempt instead.
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

func (r *ChapterRewriteTaskRepository) UpdatePostProcess(
	id uint, finalContent string, qualityScore float64, deaiApplied bool, issues string,
) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"rewritten_content":  finalContent,
		"quality_score":      qualityScore,
		"deai_applied":       deaiApplied,
		"consistency_issues": issues,
	}).Error
}

func (r *ChapterRewriteTaskRepository) MarkSummaryWritten(id uint) error {
	return r.db.Model(&model.ChapterRewriteTask{}).Where("id = ?", id).
		Update("summary_written", true).Error
}

// ── RewriteContinuityIndexRepository ─────────────────────────────────────────

type RewriteContinuityIndexRepository struct {
	db *gorm.DB
}

func NewRewriteContinuityIndexRepository(db *gorm.DB) *RewriteContinuityIndexRepository {
	return &RewriteContinuityIndexRepository{db: db}
}

// Upsert inserts or updates a single entity replacement entry.
func (r *RewriteContinuityIndexRepository) Upsert(projectID uint, entityKey, entityType, newName string, firstSeen int) error {
	entry := model.RewriteContinuityIndex{
		ProjectID:  projectID,
		EntityKey:  entityKey,
		EntityType: entityType,
		NewName:    newName,
		FirstSeen:  firstSeen,
	}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "project_id"}, {Name: "entity_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"new_name", "entity_type", "updated_at"}),
	}).Create(&entry).Error
}

// BatchUpsert inserts or updates multiple entries at once.
func (r *RewriteContinuityIndexRepository) BatchUpsert(entries []*model.RewriteContinuityIndex) error {
	if len(entries) == 0 {
		return nil
	}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "project_id"}, {Name: "entity_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"new_name", "entity_type", "updated_at"}),
	}).Create(&entries).Error
}

// GetByProject returns all continuity entries for a project.
func (r *RewriteContinuityIndexRepository) GetByProject(projectID uint) ([]*model.RewriteContinuityIndex, error) {
	var entries []*model.RewriteContinuityIndex
	err := r.db.Where("project_id = ?", projectID).Order("first_seen ASC").Find(&entries).Error
	return entries, err
}

// DeleteByProject removes all entries for a project (called when re-running analysis).
func (r *RewriteContinuityIndexRepository) DeleteByProject(projectID uint) error {
	return r.db.Where("project_id = ?", projectID).Delete(&model.RewriteContinuityIndex{}).Error
}

// ── RewriteChapterSummaryRepository ──────────────────────────────────────────

type RewriteChapterSummaryRepository struct {
	db *gorm.DB
}

func NewRewriteChapterSummaryRepository(db *gorm.DB) *RewriteChapterSummaryRepository {
	return &RewriteChapterSummaryRepository{db: db}
}

// Upsert inserts or updates the summary for a specific chapter.
func (r *RewriteChapterSummaryRepository) Upsert(projectID uint, chapterNo int, summary, charStateSnap string) error {
	entry := model.RewriteChapterSummary{
		ProjectID:     projectID,
		ChapterNo:     chapterNo,
		Summary:       summary,
		CharStateSnap: charStateSnap,
	}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "project_id"}, {Name: "chapter_no"}},
		DoUpdates: clause.AssignmentColumns([]string{"summary", "char_state_snap"}),
	}).Create(&entry).Error
}

// GetRecentByProject returns up to `limit` summaries immediately before `beforeChapterNo`.
func (r *RewriteChapterSummaryRepository) GetRecentByProject(projectID uint, beforeChapterNo, limit int) ([]*model.RewriteChapterSummary, error) {
	var summaries []*model.RewriteChapterSummary
	err := r.db.Where("project_id = ? AND chapter_no < ?", projectID, beforeChapterNo).
		Order("chapter_no DESC").Limit(limit).Find(&summaries).Error
	// Reverse so caller gets them in ascending order
	for i, j := 0, len(summaries)-1; i < j; i, j = i+1, j-1 {
		summaries[i], summaries[j] = summaries[j], summaries[i]
	}
	return summaries, err
}

// DeleteByProject removes all summaries for a project.
func (r *RewriteChapterSummaryRepository) DeleteByProject(projectID uint) error {
	return r.db.Where("project_id = ?", projectID).Delete(&model.RewriteChapterSummary{}).Error
}
