package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
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
	mu              sync.Mutex
	runningTasks    map[uint]context.CancelFunc
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
		runningTasks:    make(map[uint]context.CancelFunc),
	}
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

// DeleteProject deletes a project and all its data
func (s *RewriteService) DeleteProject(id uint) error {
	s.mu.Lock()
	if cancel, ok := s.runningTasks[id]; ok {
		cancel()
		delete(s.runningTasks, id)
	}
	s.mu.Unlock()
	return s.projectRepo.Delete(id)
}

// StartAnalysis begins Phase 0: literary analysis of the original novel
func (s *RewriteService) StartAnalysis(ctx context.Context, projectID uint) error {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.runningTasks[projectID] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.runningTasks, projectID)
			s.mu.Unlock()
		}()
		defer cancel()

		if err := s.runAnalysis(ctx, project); err != nil {
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
		}
	}()
	return nil
}

func (s *RewriteService) runAnalysis(ctx context.Context, project *model.RewriteProject) error {
	_ = ctx
	s.projectRepo.UpdateStatus(project.ID, "analyzing", "")

	// Get novel and sample chapters
	novel, err := s.novelRepo.GetByID(project.NovelID)
	if err != nil {
		return fmt.Errorf("get novel: %w", err)
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return fmt.Errorf("get chapters: %w", err)
	}

	// Build sample content (first 3 chapters)
	var sampleContent strings.Builder
	for i, ch := range chapters {
		if i >= 3 {
			break
		}
		sampleContent.WriteString(ch.Content)
		sampleContent.WriteString("\n\n---\n\n")
	}

	// Render analysis prompt
	prompt, err := renderRewriteTemplate("rewrite_analyze", map[string]interface{}{
		"Title":   novel.Title,
		"Content": sampleContent.String(),
	})
	if err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	// Call AI
	result, err := s.aiSvc.Generate(project.NovelID, "chapter_gen", prompt)
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}

	// Parse JSON result
	jsonStr := extractJSON(result)
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

	// Count chapters
	total, _ := s.chapterRepo.CountByNovel(project.NovelID)
	s.projectRepo.UpdateTotalChapters(project.ID, int(total))

	// Proceed to generate bible
	return s.generateBible(ctx, project, analysis, novel)
}

func (s *RewriteService) generateBible(ctx context.Context, project *model.RewriteProject, analysis *model.LiteraryAnalysis, novel *model.Novel) error {
	_ = ctx
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

	result, err := s.aiSvc.Generate(project.NovelID, "chapter_gen", prompt)
	if err != nil {
		return err
	}

	jsonStr := extractJSON(result)
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

	// Create chapter tasks
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

// StartRewriting begins Phase 3: chapter-by-chapter rewriting
func (s *RewriteService) StartRewriting(ctx context.Context, projectID uint) error {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return err
	}
	if project.Status != "bible_ready" {
		return fmt.Errorf("project must be in bible_ready status, got: %s", project.Status)
	}

	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.runningTasks[projectID] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.runningTasks, projectID)
			s.mu.Unlock()
		}()
		defer cancel()

		if err := s.runRewriting(ctx, project); err != nil {
			s.projectRepo.UpdateStatus(projectID, "failed", err.Error())
		}
	}()
	return nil
}

func (s *RewriteService) runRewriting(ctx context.Context, project *model.RewriteProject) error {
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
	for _, task := range tasks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if task.Status == "completed" {
			done++
			continue
		}

		if err := s.rewriteChapter(ctx, project, bible, task); err != nil {
			s.chapterTaskRepo.UpdateStatus(task.ID, "failed", err.Error())
		} else {
			done++
		}

		s.projectRepo.UpdateProgress(project.ID, done, len(tasks))
	}

	return s.projectRepo.UpdateStatus(project.ID, "completed", "")
}

func (s *RewriteService) rewriteChapter(ctx context.Context, project *model.RewriteProject, bible *model.RewriteBible, task *model.ChapterRewriteTask) error {
	_ = ctx
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

	rewritten, err := s.aiSvc.Generate(project.NovelID, "chapter_gen", prompt)
	if err != nil {
		return err
	}

	// Simple similarity check (lexical overlap)
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
