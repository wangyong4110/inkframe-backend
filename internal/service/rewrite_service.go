package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"text/template"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// RewriteService handles novel rewriting projects
type RewriteService struct {
	projectRepo     *repository.RewriteProjectRepository
	analysisRepo    *repository.LiteraryAnalysisRepository
	bibleRepo       *repository.RewriteBibleRepository
	chapterTaskRepo *repository.ChapterRewriteTaskRepository
	chapterRepo     *repository.ChapterRepository
	novelRepo       *repository.NovelRepository
	aiSvc           *AIService
	taskSvc         *TaskService
}

// NewRewriteService creates a new RewriteService
func NewRewriteService(
	projectRepo *repository.RewriteProjectRepository,
	analysisRepo *repository.LiteraryAnalysisRepository,
	bibleRepo *repository.RewriteBibleRepository,
	chapterTaskRepo *repository.ChapterRewriteTaskRepository,
	chapterRepo *repository.ChapterRepository,
	novelRepo *repository.NovelRepository,
	aiSvc *AIService,
) *RewriteService {
	return &RewriteService{
		projectRepo:     projectRepo,
		analysisRepo:    analysisRepo,
		bibleRepo:       bibleRepo,
		chapterTaskRepo: chapterTaskRepo,
		chapterRepo:     chapterRepo,
		novelRepo:       novelRepo,
		aiSvc:           aiSvc,
	}
}

// WithTaskService wires in the unified async task service.
func (s *RewriteService) WithTaskService(svc *TaskService) *RewriteService {
	s.taskSvc = svc
	return s
}

// renderRewriteTemplate loads and executes a named rewrite prompt template
func renderRewriteTemplate(name string, data interface{}) (string, error) {
	tmplStr := loadPromptTemplate(name + ".tmpl")
	if tmplStr == "" {
		return "", fmt.Errorf("template not found: %s", name)
	}
	tmpl, err := template.New(name).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %s: %w", name, err)
	}
	return buf.String(), nil
}

// CreateProject creates a new rewrite project
func (s *RewriteService) CreateProject(tenantID, novelID uint, name string, level int) (*model.RewriteProject, error) {
	project := &model.RewriteProject{
		TenantID: tenantID,
		NovelID:  novelID,
		Name:     name,
		Level:    level,
		Status:   "pending",
	}
	if err := s.projectRepo.Create(project); err != nil {
		return nil, err
	}
	return project, nil
}

// ListProjects returns all projects for a tenant
func (s *RewriteService) ListProjects(tenantID uint, page, pageSize int) ([]*model.RewriteProject, int64, error) {
	return s.projectRepo.ListByTenant(tenantID, page, pageSize)
}

// GetProject returns a project by ID
func (s *RewriteService) GetProject(id uint) (*model.RewriteProject, error) {
	return s.projectRepo.GetByID(id)
}

// DeleteProject deletes a project and all its data, cancelling any running tasks.
func (s *RewriteService) DeleteProject(id uint) error {
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteAnalysis)
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteChapters)
	}
	return s.projectRepo.Delete(id)
}

// StartAnalysis begins Phase 0+1: literary analysis + bible generation.
// Returns the async task ID that the caller can poll via GET /api/v1/tasks/:task_id.
func (s *RewriteService) StartAnalysis(tenantID, projectID uint) (string, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return "", err
	}

	// Cancel any existing analysis task for this project before creating a replacement.
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteAnalysis)
	}

	task, err := s.taskSvc.Create(tenantID, TaskTypeRewriteAnalysis,
		"文学分析 & 改写圣经生成", "rewrite_project", projectID)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runAnalysis(taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": projectID,
			"status":     "bible_ready",
		})
	}(task.TaskID)

	return task.TaskID, nil
}

func (s *RewriteService) runAnalysis(taskID string, project *model.RewriteProject) error {
	s.projectRepo.UpdateStatus(project.ID, "analyzing", "")

	novel, err := s.novelRepo.GetByID(project.NovelID)
	if err != nil {
		return fmt.Errorf("get novel: %w", err)
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return fmt.Errorf("get chapters: %w", err)
	}

	var sampleContent strings.Builder
	for i, ch := range chapters {
		if i >= 3 {
			break
		}
		sampleContent.WriteString(ch.Content)
		sampleContent.WriteString("\n\n---\n\n")
	}

	prompt, err := renderRewriteTemplate("rewrite_analyze", map[string]interface{}{
		"Title":   novel.Title,
		"Content": sampleContent.String(),
	})
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	result, err := s.aiSvc.GenerateWithProvider(project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}

	s.taskSvc.UpdateProgress(taskID, 40)

	jsonStr := extractJSONObject(result)
	var analysisData map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &analysisData); err != nil {
		return fmt.Errorf("parse analysis: %w", err)
	}

	toJSON := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	analysis := &model.LiteraryAnalysis{
		ProjectID:         project.ID,
		VoiceFingerprint:  toJSON(analysisData["voice_fingerprint"]),
		SceneArchitecture: toJSON(analysisData["scene_architecture"]),
		CharacterPsych:    toJSON(analysisData["character_psychology"]),
		ThemeCore:         toJSON(analysisData["theme_core"]),
		WorldLogic:        toJSON(analysisData["world_logic"]),
		HighRiskMarkers:   toJSON(analysisData["high_risk_markers"]),
	}
	if err := s.analysisRepo.Create(analysis); err != nil {
		return fmt.Errorf("save analysis: %w", err)
	}

	total, _ := s.chapterRepo.CountByNovel(project.NovelID)
	s.projectRepo.UpdateTotalChapters(project.ID, int(total))

	s.taskSvc.UpdateProgress(taskID, 50)

	return s.generateBible(taskID, project, analysis, novel)
}

func (s *RewriteService) generateBible(taskID string, project *model.RewriteProject, analysis *model.LiteraryAnalysis, novel *model.Novel) error {
	_ = novel

	analysisJSON, _ := json.Marshal(map[string]interface{}{
		"voice_fingerprint":    analysis.VoiceFingerprint,
		"scene_architecture":   analysis.SceneArchitecture,
		"character_psychology": analysis.CharacterPsych,
		"theme_core":           analysis.ThemeCore,
		"world_logic":          analysis.WorldLogic,
		"high_risk_markers":    analysis.HighRiskMarkers,
	})

	levelDesc := map[int]string{
		1: "文学精炼：保留80-90%情节，用全新文学语言重写",
		2: "结构重构：保留40-60%情节，重构世界观和角色",
		3: "精神蒸馏：只保留5-20%精神内核，全面重创",
	}

	prompt, err := renderRewriteTemplate("rewrite_bible_generate", map[string]interface{}{
		"Analysis": string(analysisJSON),
		"Level":    project.Level,
		"Goal":     levelDesc[project.Level],
	})
	if err != nil {
		return err
	}

	result, err := s.aiSvc.GenerateWithProvider(project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		return err
	}

	s.taskSvc.UpdateProgress(taskID, 80)

	jsonStr := extractJSONObject(result)
	var bibleData map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &bibleData); err != nil {
		return fmt.Errorf("parse bible: %w", err)
	}

	toJSON := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	worldName, _ := bibleData["new_world_name"].(string)
	bible := &model.RewriteBible{
		ProjectID:      project.ID,
		NewWorldName:   worldName,
		NewCharNames:   toJSON(bibleData["new_char_names"]),
		PlotTransform:  toJSON(bibleData["plot_transform"]),
		VoiceStrategy:  toJSON(bibleData["voice_strategy"]),
		StyleGuide:     toJSON(bibleData["style_guide"]),
		ForbiddenElems: toJSON(bibleData["forbidden_elements"]),
	}
	if err := s.bibleRepo.Create(bible); err != nil {
		return err
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return err
	}
	for _, ch := range chapters {
		task := &model.ChapterRewriteTask{
			ProjectID:       project.ID,
			ChapterID:       ch.ID,
			ChapterNo:       ch.ChapterNo,
			Status:          "pending",
			OriginalContent: ch.Content,
		}
		s.chapterTaskRepo.Create(task)
	}

	return s.projectRepo.UpdateStatus(project.ID, "bible_ready", "")
}

// StartRewriting begins Phase 2: chapter-by-chapter rewriting.
// Returns the async task ID that the caller can poll via GET /api/v1/tasks/:task_id.
func (s *RewriteService) StartRewriting(tenantID, projectID uint) (string, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return "", err
	}
	if project.Status == "failed" && project.TotalChapters > 0 {
		// Allow retry: bible exists, rewriting phase failed
	} else if project.Status != "bible_ready" {
		return "", fmt.Errorf("project must be in bible_ready status, got: %s", project.Status)
	}

	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteChapters)
	}

	task, err := s.taskSvc.Create(tenantID, TaskTypeRewriteChapters,
		"章节改写", "rewrite_project", projectID)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	go func(taskID string) {
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runRewriting(taskID, project); err != nil {
			s.taskSvc.Fail(taskID, err.Error())
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
			return
		}
		s.taskSvc.Complete(taskID, map[string]interface{}{
			"project_id": projectID,
			"status":     "completed",
		})
	}(task.TaskID)

	return task.TaskID, nil
}

func (s *RewriteService) runRewriting(taskID string, project *model.RewriteProject) error {
	s.projectRepo.UpdateStatus(project.ID, "rewriting", "")

	bible, err := s.bibleRepo.GetByProjectID(project.ID)
	if err != nil {
		return err
	}

	tasks, err := s.chapterTaskRepo.ListByProject(project.ID)
	if err != nil {
		return err
	}

	done := 0
	total := len(tasks)
	for _, task := range tasks {
		// Check if the async task has been cancelled between chapters.
		if at, err := s.taskSvc.Get(taskID); err == nil && at.Status == "cancelled" {
			return fmt.Errorf("task cancelled")
		}

		if task.Status == "completed" {
			done++
			s.updateRewriteProgress(taskID, project.ID, done, total)
			continue
		}

		if err := s.rewriteChapter(project, bible, task); err != nil {
			s.chapterTaskRepo.UpdateStatus(task.ID, "failed", err.Error())
		} else {
			done++
		}

		s.updateRewriteProgress(taskID, project.ID, done, total)
	}

	return s.projectRepo.UpdateStatus(project.ID, "completed", "")
}

func (s *RewriteService) updateRewriteProgress(taskID string, projectID uint, done, total int) {
	if total > 0 {
		s.taskSvc.UpdateProgress(taskID, done*100/total)
	}
	s.projectRepo.UpdateProgress(projectID, done, total)
}

func (s *RewriteService) rewriteChapter(project *model.RewriteProject, bible *model.RewriteBible, task *model.ChapterRewriteTask) error {
	s.chapterTaskRepo.UpdateStatus(task.ID, "rewriting", "")

	var tmplName string
	if project.Level <= 2 {
		tmplName = "rewrite_chapter_l1"
	} else {
		tmplName = "rewrite_chapter_l3"
	}

	origRunes := []rune(task.OriginalContent)
	coreEnd := min(500, len(origRunes))
	coreElements := string(origRunes[:coreEnd])

	prompt, err := renderRewriteTemplate(tmplName, map[string]interface{}{
		"WorldName":       bible.NewWorldName,
		"CharNames":       bible.NewCharNames,
		"PlotTransform":   bible.PlotTransform,
		"VoiceStrategy":   bible.VoiceStrategy,
		"StyleGuide":      bible.StyleGuide,
		"ForbiddenElems":  bible.ForbiddenElems,
		"OriginalContent": task.OriginalContent,
		"CoreElements":    coreElements,
	})
	if err != nil {
		return err
	}

	rewritten, err := s.aiSvc.GenerateWithProvider(project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		return err
	}

	lexSim := calculateLexicalSimilarity(task.OriginalContent, rewritten)
	passed := lexSim < 0.35

	return s.chapterTaskRepo.UpdateRewritten(task.ID, rewritten, lexSim, passed)
}

// calculateLexicalSimilarity returns a 0-1 score (higher = more similar)
func calculateLexicalSimilarity(original, rewritten string) float64 {
	origRunes := []rune(original)
	rewRunes := []rune(rewritten)
	if len(origRunes) == 0 || len(rewRunes) == 0 {
		return 0
	}

	origBigrams := make(map[string]int)
	for i := 0; i < len(origRunes)-1; i++ {
		bg := string(origRunes[i : i+2])
		origBigrams[bg]++
	}

	matches := 0
	for i := 0; i < len(rewRunes)-1; i++ {
		bg := string(rewRunes[i : i+2])
		if origBigrams[bg] > 0 {
			matches++
			origBigrams[bg]--
		}
	}

	totalBigrams := len(origRunes) - 1
	if totalBigrams <= 0 {
		return 0
	}
	sim := float64(matches) / float64(totalBigrams)
	return math.Min(sim, 1.0)
}

// GetBible returns the rewrite bible for a project
func (s *RewriteService) GetBible(projectID uint) (*model.RewriteBible, error) {
	return s.bibleRepo.GetByProjectID(projectID)
}

// GetAnalysis returns the literary analysis for a project
func (s *RewriteService) GetAnalysis(projectID uint) (*model.LiteraryAnalysis, error) {
	return s.analysisRepo.GetByProjectID(projectID)
}

// ListChapterTasks returns all chapter tasks for a project
func (s *RewriteService) ListChapterTasks(projectID uint) ([]*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.ListByProject(projectID)
}

// GetChapterTask returns a single chapter task
func (s *RewriteService) GetChapterTask(taskID uint) (*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.GetByID(taskID)
}

// ApproveChapter marks a chapter task as approved
func (s *RewriteService) ApproveChapter(taskID uint) error {
	return s.chapterTaskRepo.UpdateStatus(taskID, "completed", "")
}

