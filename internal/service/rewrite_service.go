package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
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

// ── Helpers ───────────────────────────────────────────────────────────────────

// renderRewriteTemplate renders a named rewrite prompt template using Jinja2.
func renderRewriteTemplate(name string, data interface{}) (string, error) {
	ctx, ok := data.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("renderRewriteTemplate: data must be map[string]interface{}")
	}
	return renderPrompt(name, ctx)
}

// stratifiedSample picks up to maxSamples chapters at 7 semantic positions:
// 0 / 0.15 / 0.35 / 0.55 / 0.75 / 0.90 / 1.0
// This captures early, early-mid, mid, late-mid, late, near-end, and final style layers.
func stratifiedSample(chapters []*model.Chapter, maxSamples int) []*model.Chapter {
	n := len(chapters)
	if n == 0 {
		return nil
	}
	if n <= maxSamples {
		return chapters
	}

	// 7-point semantic positions
	positions := []float64{0, 0.15, 0.35, 0.55, 0.75, 0.90, 1.0}
	if maxSamples < len(positions) {
		// fall back to evenly spaced
		positions = make([]float64, maxSamples)
		for i := range positions {
			if maxSamples == 1 {
				positions[i] = 0
			} else {
				positions[i] = float64(i) / float64(maxSamples-1)
			}
		}
	}

	seen := make(map[int]bool)
	result := make([]*model.Chapter, 0, len(positions))
	for _, p := range positions {
		idx := int(p*float64(n-1) + 0.5)
		if idx >= n {
			idx = n - 1
		}
		if !seen[idx] {
			seen[idx] = true
			result = append(result, chapters[idx])
		}
	}
	return result
}

// extractCoreElements returns the reference excerpt from a chapter for deep-rewrite prompts.
// Levels 1-3: leading 500 chars.
// Levels 4-5: beginning + middle + end to expose the full emotional arc.
func extractCoreElements(content string, level int) string {
	runes := []rune(content)
	n := len(runes)
	if level < 4 {
		return string(runes[:min(500, n)])
	}
	if n <= 1500 {
		return content
	}
	segLen := 300
	begin := string(runes[:segLen])
	mid := string(runes[n/2-segLen/2 : n/2+segLen/2])
	end := string(runes[n-segLen:])
	return begin + "\n[...中段...]\n" + mid + "\n[...末段...]\n" + end
}

// recentChapterInfo holds the opening and closing excerpts of a recently-rewritten chapter
// for richer narrative continuity injection.
type recentChapterInfo struct {
	ChapterNo int
	Opening   string // first 250 runes
	Closing   string // last 200 runes
}

// buildRichPrevContext formats recent rewritten chapter info (opening + closing) as a
// continuity block for the AI. This gives the model both the start AND end state of
// prior chapters, helping it maintain character arcs and avoid narrative loops.
func buildRichPrevContext(recent []recentChapterInfo) string {
	if len(recent) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【前文已改写内容摘要（保持叙事连贯，角色状态以此为准）】\n")
	for _, r := range recent {
		sb.WriteString(fmt.Sprintf("第%d章 开头：%s\n      末尾：%s\n", r.ChapterNo, r.Opening, r.Closing))
	}
	return sb.String()
}

// emotionalArcStage maps a chapter's position in the full novel to a human-readable
// narrative stage label. Used to help the AI maintain tonal coherence.
func emotionalArcStage(chapterNo, total int) string {
	if total <= 0 {
		return ""
	}
	ratio := float64(chapterNo) / float64(total)
	switch {
	case ratio < 0.12:
		return "开篇建立期（世界观铺垫，人物登场）"
	case ratio < 0.30:
		return "矛盾上升期（冲突萌芽，关系确立）"
	case ratio < 0.50:
		return "发展深化期（情节推进，张力积累）"
	case ratio < 0.70:
		return "高潮酝酿期（危机激化，情感爆发前夕）"
	case ratio < 0.85:
		return "高潮决战期（核心冲突爆发，命运转折）"
	case ratio < 0.95:
		return "收束期（余波平息，伏笔揭晓）"
	default:
		return "终章（主题升华，情感落幕）"
	}
}

// checkConsistency scans the rewritten text for original character names and forbidden
// elements that must NOT appear in the rewrite. Returns a list of violation descriptions.
// This is a rule-based check with no AI call required.
func checkConsistency(rewritten string, bible *model.RewriteBible) []string {
	var issues []string

	// Parse new char name map to find original names (keys)
	if bible.NewCharNames != "" && bible.NewCharNames != "null" {
		var nameMap map[string]string
		if err := json.Unmarshal([]byte(bible.NewCharNames), &nameMap); err == nil {
			for origName := range nameMap {
				if origName != "" && strings.Contains(rewritten, origName) {
					issues = append(issues, fmt.Sprintf("原著角色名残留：「%s」", origName))
				}
			}
		}
	}

	// Parse forbidden elements array/object
	if bible.ForbiddenElems != "" && bible.ForbiddenElems != "null" && bible.ForbiddenElems != "[]" {
		var forbidden []string
		if err := json.Unmarshal([]byte(bible.ForbiddenElems), &forbidden); err == nil {
			for _, elem := range forbidden {
				if elem != "" && len([]rune(elem)) >= 4 && strings.Contains(rewritten, elem) {
					issues = append(issues, fmt.Sprintf("禁止元素残留：「%s」", elem))
				}
			}
		}
	}

	return issues
}

// calculateHeuristicQuality returns a 0-100 quality score without any AI calls.
// It combines:
//   - Similarity penalty: high similarity → lower score
//   - Word-count ratio: too short or too long → penalty
//   - Consistency penalty: each violation deducts points
func calculateHeuristicQuality(lexSim, structSim float64, origLen, rewLen int, issues []string) float64 {
	// Base score from similarity (lower sim = better rewrite divergence)
	avgSim := (lexSim + structSim) / 2
	simScore := (1.0 - avgSim) * 60 // max 60 points

	// Word-count ratio score (max 30 points)
	var ratioScore float64
	if origLen > 0 {
		ratio := float64(rewLen) / float64(origLen)
		// Ideal range 0.8-1.2
		if ratio >= 0.8 && ratio <= 1.2 {
			ratioScore = 30
		} else if ratio >= 0.6 && ratio < 0.8 {
			ratioScore = 30 * (ratio - 0.6) / 0.2
		} else if ratio > 1.2 && ratio <= 1.5 {
			ratioScore = 30 * (1.5 - ratio) / 0.3
		} else {
			ratioScore = 0
		}
	}

	// Consistency penalty (max -10 per issue, capped at 10 points total deduction)
	consistencyPenalty := math.Min(float64(len(issues))*10, 10)

	score := simScore + ratioScore - consistencyPenalty
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return math.Round(score*10) / 10
}

// splitSentences splits Chinese text into sentence units by end-of-sentence punctuation.
func splitSentences(text string) []string {
	var result []string
	var buf strings.Builder
	for _, r := range text {
		buf.WriteRune(r)
		if r == '。' || r == '！' || r == '？' || r == '\n' {
			if s := strings.TrimSpace(buf.String()); s != "" {
				result = append(result, s)
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		result = append(result, s)
	}
	return result
}

// calculateStructuralSimilarity measures what fraction of rewritten sentences share
// 4-gram fingerprints with the original.  A high score means the sentence-level
// content is still largely the same even if individual characters differ.
func calculateStructuralSimilarity(original, rewritten string) float64 {
	origSents := splitSentences(original)
	rewSents := splitSentences(rewritten)
	if len(origSents) == 0 || len(rewSents) == 0 {
		return 0
	}
	origFP := make(map[string]bool, len(origSents)*10)
	for _, s := range origSents {
		r := []rune(s)
		for i := 0; i+4 <= len(r); i++ {
			origFP[string(r[i:i+4])] = true
		}
	}
	matches := 0
	for _, s := range rewSents {
		r := []rune(s)
		for i := 0; i+4 <= len(r); i++ {
			if origFP[string(r[i:i+4])] {
				matches++
				break
			}
		}
	}
	denom := len(origSents)
	if len(rewSents) > denom {
		denom = len(rewSents)
	}
	return float64(matches) / float64(denom)
}

// calculateLexicalSimilarity returns a 0-1 score based on character-bigram overlap (higher = more similar).
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
	return math.Min(float64(matches)/float64(totalBigrams), 1.0)
}

// ── CRUD ──────────────────────────────────────────────────────────────────────

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

func (s *RewriteService) ListProjects(tenantID uint, page, pageSize int) ([]*model.RewriteProject, int64, error) {
	return s.projectRepo.ListByTenant(tenantID, page, pageSize)
}

func (s *RewriteService) GetProject(id uint) (*model.RewriteProject, error) {
	return s.projectRepo.GetByID(id)
}

func (s *RewriteService) DeleteProject(id uint) error {
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteAnalysis)
		s.taskSvc.CancelActiveByEntity("rewrite_project", id, TaskTypeRewriteChapters)
	}
	return s.projectRepo.Delete(id)
}

// ── Phase 0+1: Analysis & Bible ───────────────────────────────────────────────

// StartAnalysis begins Phase 0+1: literary analysis + bible generation.
func (s *RewriteService) StartAnalysis(tenantID, projectID uint) (string, error) {
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return "", err
	}
	if s.taskSvc != nil {
		s.taskSvc.CancelActiveByEntity("rewrite_project", projectID, TaskTypeRewriteAnalysis)
	}
	task, err := s.taskSvc.Create(tenantID, TaskTypeRewriteAnalysis,
		"文学分析 & 改写圣经生成", "rewrite_project", projectID)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	go func(taskID string) {
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()

		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runAnalysis(ctx, taskID, project); err != nil {
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

func (s *RewriteService) runAnalysis(ctx context.Context, taskID string, project *model.RewriteProject) error {
	s.projectRepo.UpdateStatus(project.ID, "analyzing", "")

	novel, err := s.novelRepo.GetByID(project.NovelID)
	if err != nil {
		return fmt.Errorf("get novel: %w", err)
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(project.NovelID)
	if err != nil {
		return fmt.Errorf("get chapters: %w", err)
	}

	// 7-point stratified sampling across the full novel
	sampled := stratifiedSample(chapters, 7)
	var sampleContent strings.Builder
	for _, ch := range sampled {
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

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("task cancelled")
	}

	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}
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

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("task cancelled")
	}

	s.taskSvc.UpdateProgress(taskID, 50)
	return s.generateBible(ctx, taskID, project, analysis, novel)
}

func (s *RewriteService) generateBible(ctx context.Context, taskID string, project *model.RewriteProject, analysis *model.LiteraryAnalysis, novel *model.Novel) error {
	_ = novel

	// each analysis field is already a JSON string; unmarshal first so the
	// combined payload contains nested objects, not doubly-escaped strings.
	toObj := func(raw string) interface{} {
		var v interface{}
		if json.Unmarshal([]byte(raw), &v) == nil {
			return v
		}
		return raw
	}
	analysisJSON, _ := json.Marshal(map[string]interface{}{
		"voice_fingerprint":    toObj(analysis.VoiceFingerprint),
		"scene_architecture":   toObj(analysis.SceneArchitecture),
		"character_psychology": toObj(analysis.CharacterPsych),
		"theme_core":           toObj(analysis.ThemeCore),
		"world_logic":          toObj(analysis.WorldLogic),
		"high_risk_markers":    toObj(analysis.HighRiskMarkers),
	})

	levelDesc := map[int]string{
		1: "字词润色：保留90-95%情节，仅做词句级同义替换",
		2: "文学精炼：保留80-90%情节，用全新文学语言重写",
		3: "情节调整：保留60-75%情节，适度调整场景与对话",
		4: "结构重构：保留30-50%情节，重构世界观和角色",
		5: "精神蒸馏：只保留5-20%精神内核，全面重创",
	}

	prompt, err := renderRewriteTemplate("rewrite_bible_generate", map[string]interface{}{
		"Analysis": string(analysisJSON),
		"Level":    project.Level,
		"Goal":     levelDesc[project.Level],
	})
	if err != nil {
		return err
	}

	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}
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
	namingStyle, _ := bibleData["naming_style"].(string)
	bible := &model.RewriteBible{
		ProjectID:      project.ID,
		NewWorldName:   worldName,
		NewCharNames:   toJSON(bibleData["new_char_names"]),
		NamingStyle:    namingStyle,
		PlotTransform:  toJSON(bibleData["plot_transform"]),
		PropsTransform: toJSON(bibleData["props_transform"]),
		VoiceStrategy:  toJSON(bibleData["voice_strategy"]),
		StyleGuide:     toJSON(bibleData["style_guide"]),
		ForbiddenElems: toJSON(bibleData["forbidden_elements"]),
	}
	if err := s.bibleRepo.Create(bible); err != nil {
		return err
	}

	s.chapterTaskRepo.DeleteByProjectID(project.ID)

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

// ── Phase 2: Chapter Rewriting ────────────────────────────────────────────────

// StartRewriting begins Phase 2: chapter-by-chapter rewriting.
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
		ctx, cancel := context.WithCancel(context.Background())
		s.taskSvc.RegisterCancel(taskID, cancel)
		defer s.taskSvc.DeregisterCancel(taskID)
		defer cancel()

		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("内部错误: %v", r)
				s.taskSvc.Fail(taskID, msg)
				s.projectRepo.UpdateStatus(projectID, "failed", msg)
			}
		}()
		s.taskSvc.SetRunning(taskID)
		if err := s.runRewriting(ctx, taskID, project); err != nil {
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

const maxChapterRetries = 2

// retryHints provides progressively stronger retry instructions when similarity is too high.
var retryHints = []string{
	"", // attempt 0 — no hint
	"上次改写相似度过高，请采用更大幅度文学变形：更换叙事视角（如从第三人称改为第一人称内心独白）、打乱场景顺序、将对话转为心理描写，彻底改变句式结构。",
	"前两次均未达标，请完全重构叙事视角与表达方式：抛弃原文一切表层描述，只保留核心情感逻辑，用截然不同的故事形式（如倒叙、片段化意识流、书信体）重新承载相同的戏剧张力。",
}

// rewriteResult holds the outcome of a single chapter rewrite attempt.
type rewriteResult struct {
	Content   string
	LexSim    float64
	StructSim float64
	Passed    bool
}

func (s *RewriteService) runRewriting(ctx context.Context, taskID string, project *model.RewriteProject) error {
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
	// recentContext holds opening+closing excerpts of the last 3 rewritten chapters.
	var recentContext []recentChapterInfo

	for _, task := range tasks {
		if ctx.Err() != nil {
			return fmt.Errorf("task cancelled")
		}

		if task.Status == "completed" {
			done++
			s.updateRewriteProgress(taskID, project.ID, done, total)
			continue
		}

		prevContext := buildRichPrevContext(recentContext)
		arcStage := emotionalArcStage(task.ChapterNo, project.TotalChapters)

		result, err := s.rewriteChapterWithRetry(ctx, project, bible, task, prevContext, arcStage)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("task cancelled")
			}
			// rewriteChapterWithRetry already marked the chapter as failed.
			// Continue to next chapter — don't abort the whole batch.
		} else {
			done++

			// ── Post-processing pipeline ───────────────────────────────────
			finalContent := result.Content

			// Quality Gate 2: consistency check (rule-based, no AI)
			issues := checkConsistency(finalContent, bible)
			issuesJSON := "[]"
			if len(issues) > 0 {
				if b, err := json.Marshal(issues); err == nil {
					issuesJSON = string(b)
				}
				logger.Printf("[Rewrite] chapter %d consistency issues: %v", task.ChapterNo, issues)
			}

			// De-AI pass for Level 2+ (non-fatal)
			deaiApplied := false
			if project.Level >= 2 {
				if polished := s.deAIPass(ctx, project, finalContent, bible); polished != "" {
					finalContent = polished
					deaiApplied = true
					logger.Printf("[Rewrite] deAI pass applied for chapter %d", task.ChapterNo)
				}
			}

			// Quality Gate 3: heuristic quality score (no AI)
			origLen := len([]rune(task.OriginalContent))
			rewLen := len([]rune(finalContent))
			qualityScore := calculateHeuristicQuality(result.LexSim, result.StructSim, origLen, rewLen, issues)
			logger.Printf("[Rewrite] chapter %d quality_score=%.1f deai=%v", task.ChapterNo, qualityScore, deaiApplied)

			// Persist final content + metadata
			if err := s.chapterTaskRepo.UpdatePostProcess(task.ID, finalContent, qualityScore, deaiApplied, issuesJSON); err != nil {
				logger.Printf("[Rewrite] UpdatePostProcess chapter %d: %v", task.ChapterNo, err)
			}

			// Build rolling rich context from final content
			runes := []rune(finalContent)
			n := len(runes)
			opening := string(runes[:min(250, n)])
			closing := ""
			if n > 200 {
				closing = string(runes[n-200:])
			} else {
				closing = string(runes)
			}
			recentContext = append(recentContext, recentChapterInfo{
				ChapterNo: task.ChapterNo,
				Opening:   opening,
				Closing:   closing,
			})
			if len(recentContext) > 3 {
				recentContext = recentContext[1:]
			}
		}
		s.updateRewriteProgress(taskID, project.ID, done, total)
	}

	return s.projectRepo.UpdateStatus(project.ID, "completed", "")
}

// rewriteChapterWithRetry retries rewriteChapter up to maxChapterRetries times.
// On similarity-too-high failures it passes progressively stronger retry hints.
// On exhaustion it marks the chapter task as failed and returns the last error.
func (s *RewriteService) rewriteChapterWithRetry(
	ctx context.Context,
	project *model.RewriteProject,
	bible *model.RewriteBible,
	task *model.ChapterRewriteTask,
	prevContext string,
	arcStage string,
) (*rewriteResult, error) {
	var lastErr error
	for attempt := 0; attempt <= maxChapterRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hint := ""
		if attempt < len(retryHints) {
			hint = retryHints[attempt]
		}
		res, err := s.rewriteChapter(ctx, project, bible, task, prevContext, arcStage, hint, attempt)
		if err == nil {
			return res, nil
		}
		lastErr = err
		logger.Printf("[Rewrite] chapter %d attempt %d/%d failed: %v",
			task.ChapterNo, attempt+1, maxChapterRetries+1, err)
		if attempt < maxChapterRetries {
			// Reset to pending so the next attempt starts fresh.
			s.chapterTaskRepo.UpdateStatus(task.ID, "pending", "")
		}
	}
	s.chapterTaskRepo.UpdateStatus(task.ID, "failed", lastErr.Error())
	return nil, lastErr
}

type rewriteLevelConfig struct {
	Template        string
	Goal            string
	RetentionTarget string
	SimilarityLimit string
	LexSimThreshold float64
}

var rewriteLevelConfigs = map[int]rewriteLevelConfig{
	1: {"rewrite_chapter_l1", "仅做词句级同义替换，不改变情节与对话内容", "90-95%", "60%", 0.75},
	2: {"rewrite_chapter_l1", "用全新文学语言重新表达，保留情节骨架", "80-90%", "35%", 0.60},
	3: {"rewrite_chapter_l2", "适度调整场景顺序与细节，改写对话语气", "60-75%", "50%", 0.50},
	4: {"rewrite_chapter_l3", "重构世界观与角色设定，大幅改变故事形式", "30-50%", "65%", 0.35},
	5: {"rewrite_chapter_l3", "只保留精神内核与情感逻辑，全面重创", "5-20%", "90%", 0.20},
}

// rewriteChapter runs a single chapter rewrite attempt.
// Returns rewriteResult on success, error on AI failure or similarity threshold exceeded.
// On success it also persists the raw similarity scores via UpdateRewritten.
// The caller (rewriteChapterWithRetry) is responsible for the final UpdatePostProcess.
func (s *RewriteService) rewriteChapter(
	ctx context.Context,
	project *model.RewriteProject,
	bible *model.RewriteBible,
	task *model.ChapterRewriteTask,
	prevContext string,
	arcStage string,
	retryHint string,
	attempt int,
) (*rewriteResult, error) {
	s.chapterTaskRepo.UpdateStatus(task.ID, "rewriting", "")

	cfg, ok := rewriteLevelConfigs[project.Level]
	if !ok {
		cfg = rewriteLevelConfigs[2]
	}

	// Level 4-5 use beginning+middle+end; Level 1-3 use leading excerpt.
	coreElements := extractCoreElements(task.OriginalContent, project.Level)

	// Pass target word count so AI maintains similar chapter length.
	targetWords := len([]rune(task.OriginalContent))

	prompt, err := renderRewriteTemplate(cfg.Template, map[string]interface{}{
		"WorldName":       bible.NewWorldName,
		"CharNames":       bible.NewCharNames,
		"NamingStyle":     bible.NamingStyle,
		"PlotTransform":   bible.PlotTransform,
		"PropsTransform":  bible.PropsTransform,
		"VoiceStrategy":   bible.VoiceStrategy,
		"StyleGuide":      bible.StyleGuide,
		"ForbiddenElems":  bible.ForbiddenElems,
		"OriginalContent": task.OriginalContent,
		"CoreElements":    coreElements,
		"LevelGoal":       cfg.Goal,
		"RetentionTarget": cfg.RetentionTarget,
		"SimilarityLimit": cfg.SimilarityLimit,
		"TargetWords":     targetWords,
		"PrevContext":     prevContext,
		"ArcStage":        arcStage,
		"RetryHint":       retryHint,
	})
	if err != nil {
		return nil, err
	}

	rewritten, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		return nil, err
	}

	lexSim := calculateLexicalSimilarity(task.OriginalContent, rewritten)
	structSim := calculateStructuralSimilarity(task.OriginalContent, rewritten)
	passed := lexSim < cfg.LexSimThreshold

	// Persist raw similarity scores immediately
	if err := s.chapterTaskRepo.UpdateRewritten(task.ID, rewritten, lexSim, structSim, passed); err != nil {
		return nil, err
	}

	// Quality Gate 1: if similarity is still too high and we have retries left, signal retry
	if !passed && attempt < maxChapterRetries {
		return nil, fmt.Errorf("相似度过高 lexSim=%.3f threshold=%.2f", lexSim, cfg.LexSimThreshold)
	}

	return &rewriteResult{
		Content:   rewritten,
		LexSim:    lexSim,
		StructSim: structSim,
		Passed:    passed,
	}, nil
}

// deAIPass calls the rewrite_deai template to remove AI writing patterns.
// Non-fatal: on any error it logs and returns "" so the caller can use the original content.
func (s *RewriteService) deAIPass(ctx context.Context, project *model.RewriteProject, content string, bible *model.RewriteBible) string {
	if ctx.Err() != nil {
		return ""
	}
	prompt, err := renderRewriteTemplate("rewrite_deai", map[string]interface{}{
		"Content":       content,
		"StyleGuide":    bible.StyleGuide,
		"VoiceStrategy": bible.VoiceStrategy,
	})
	if err != nil {
		logger.Printf("[Rewrite] deAIPass render: %v", err)
		return ""
	}
	result, err := s.aiSvc.GenerateWithProviderCtx(ctx, project.TenantID, project.NovelID, "chapter_gen", prompt, "")
	if err != nil {
		logger.Printf("[Rewrite] deAIPass generate: %v", err)
		return ""
	}
	if strings.TrimSpace(result) == "" {
		return ""
	}
	return result
}

func (s *RewriteService) updateRewriteProgress(taskID string, projectID uint, done, total int) {
	if total > 0 {
		s.taskSvc.UpdateProgress(taskID, done*100/total)
	}
	s.projectRepo.UpdateProgress(projectID, done, total)
}

// ── Bible editing ─────────────────────────────────────────────────────────────

// UpdateBibleRequest holds optional fields for patching the rewrite bible.
// Only non-nil fields are applied.
type UpdateBibleRequest struct {
	NewWorldName   *string `json:"new_world_name,omitempty"`
	NamingStyle    *string `json:"naming_style,omitempty"`
	NewCharNames   *string `json:"new_char_names,omitempty"`
	PlotTransform  *string `json:"plot_transform,omitempty"`
	PropsTransform *string `json:"props_transform,omitempty"`
	VoiceStrategy  *string `json:"voice_strategy,omitempty"`
	StyleGuide     *string `json:"style_guide,omitempty"`
	ForbiddenElems *string `json:"forbidden_elems,omitempty"`
}

// UpdateBible applies a partial update to the rewrite bible for a project.
func (s *RewriteService) UpdateBible(projectID uint, req UpdateBibleRequest) error {
	fields := map[string]interface{}{}
	if req.NewWorldName != nil {
		fields["new_world_name"] = *req.NewWorldName
	}
	if req.NamingStyle != nil {
		fields["naming_style"] = *req.NamingStyle
	}
	if req.NewCharNames != nil {
		fields["new_char_names"] = *req.NewCharNames
	}
	if req.PlotTransform != nil {
		fields["plot_transform"] = *req.PlotTransform
	}
	if req.PropsTransform != nil {
		fields["props_transform"] = *req.PropsTransform
	}
	if req.VoiceStrategy != nil {
		fields["voice_strategy"] = *req.VoiceStrategy
	}
	if req.StyleGuide != nil {
		fields["style_guide"] = *req.StyleGuide
	}
	if req.ForbiddenElems != nil {
		fields["forbidden_elems"] = *req.ForbiddenElems
	}
	if len(fields) == 0 {
		return nil
	}
	return s.bibleRepo.UpdateFields(projectID, fields)
}

// ── Accessors ─────────────────────────────────────────────────────────────────

func (s *RewriteService) GetBible(projectID uint) (*model.RewriteBible, error) {
	return s.bibleRepo.GetByProjectID(projectID)
}

func (s *RewriteService) GetAnalysis(projectID uint) (*model.LiteraryAnalysis, error) {
	return s.analysisRepo.GetByProjectID(projectID)
}

func (s *RewriteService) ListChapterTasks(projectID uint) ([]*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.ListByProject(projectID)
}

func (s *RewriteService) GetChapterTask(taskID uint) (*model.ChapterRewriteTask, error) {
	return s.chapterTaskRepo.GetByID(taskID)
}

func (s *RewriteService) ApproveChapter(taskID uint) error {
	return s.chapterTaskRepo.UpdateStatus(taskID, "completed", "")
}
