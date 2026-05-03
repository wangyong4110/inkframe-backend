package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// AnalysisTask 分析任务状态（线程安全）
type AnalysisTask struct {
	NovelID        uint      `json:"novel_id"`
	CreateOutlines bool      `json:"-"` // 是否在分析完成后创建章节占位记录
	expiresAt      time.Time `json:"-"` // TTL 过期时间
	cancel         context.CancelFunc `json:"-"` // 取消 pipeline 上下文

	mu       sync.RWMutex `json:"-"`
	Status   string       `json:"status"`              // pending / running / completed / failed
	Progress int          `json:"progress"`            // 0-100
	Step     string       `json:"step"`
	Error    string       `json:"error,omitempty"`
	Warnings []string     `json:"warnings,omitempty"` // 各步骤非致命警告
}

func (t *AnalysisTask) setStatus(s string)   { t.mu.Lock(); t.Status = s; t.mu.Unlock() }
func (t *AnalysisTask) setProgress(p int)    { t.mu.Lock(); t.Progress = p; t.mu.Unlock() }
func (t *AnalysisTask) setStep(s string)     { t.mu.Lock(); t.Step = s; t.mu.Unlock() }
func (t *AnalysisTask) setError(e string)    { t.mu.Lock(); t.Error = e; t.mu.Unlock() }
func (t *AnalysisTask) addWarning(w string)  { t.mu.Lock(); t.Warnings = append(t.Warnings, w); t.mu.Unlock() }

// snapshot 返回字段快照，供 handler 安全读取
func (t *AnalysisTask) snapshot() *AnalysisTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ws := make([]string, len(t.Warnings))
	copy(ws, t.Warnings)
	return &AnalysisTask{
		NovelID:  t.NovelID,
		Status:   t.Status,
		Progress: t.Progress,
		Step:     t.Step,
		Error:    t.Error,
		Warnings: ws,
	}
}

// NovelAnalysisService 小说分析服务（异步 Pipeline）
type NovelAnalysisService struct {
	novelRepo          *repository.NovelRepository
	chapterRepo        *repository.ChapterRepository
	characterRepo      *repository.CharacterRepository
	worldviewRepo      *repository.WorldviewRepository
	itemRepo           *repository.ItemRepository
	itemService        *ItemService
	skillService       *SkillService
	novelService       *NovelService
	aiService          *AIService
	plotPointService   *PlotPointService
	sceneAnchorService *SceneAnchorService
	tasks       sync.Map      // taskID(string) → *AnalysisTask
	cleanupOnce sync.Once
	cleanupStop chan struct{}  // 关闭信号，通知 cleanupExpiredTasks 退出
}

func NewNovelAnalysisService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	worldviewRepo *repository.WorldviewRepository,
	novelService *NovelService,
	aiService *AIService,
) *NovelAnalysisService {
	return &NovelAnalysisService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		characterRepo: characterRepo,
		worldviewRepo: worldviewRepo,
		novelService:  novelService,
		aiService:     aiService,
		cleanupStop:   make(chan struct{}),
	}
}

// Shutdown 关闭后台清理协程，应在服务器退出时调用
func (s *NovelAnalysisService) Shutdown() {
	select {
	case <-s.cleanupStop:
	default:
		close(s.cleanupStop)
	}
}

// WithItemRepo 注入物品仓库（可选，支持物品提取步骤）
func (s *NovelAnalysisService) WithItemRepo(itemRepo *repository.ItemRepository) *NovelAnalysisService {
	s.itemRepo = itemRepo
	return s
}

// WithItemService 注入物品服务（可选，启用逐章并发提取）
func (s *NovelAnalysisService) WithItemService(svc *ItemService) *NovelAnalysisService {
	s.itemService = svc
	return s
}

// WithSkillService 注入技能服务（可选，支持技能自动生成步骤）
func (s *NovelAnalysisService) WithSkillService(skillService *SkillService) *NovelAnalysisService {
	s.skillService = skillService
	return s
}

// WithPlotPointService 注入剧情点服务（可选，支持剧情点提取步骤）
func (s *NovelAnalysisService) WithPlotPointService(svc *PlotPointService) *NovelAnalysisService {
	s.plotPointService = svc
	return s
}

// WithSceneAnchorService 注入场景锚点服务（可选，支持场景锚点提取步骤）
func (s *NovelAnalysisService) WithSceneAnchorService(svc *SceneAnchorService) *NovelAnalysisService {
	s.sceneAnchorService = svc
	return s
}

// StartAnalysis 启动分析任务，返回 taskID
func (s *NovelAnalysisService) StartAnalysis(tenantID, novelID uint, createOutlines bool) (string, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return "", fmt.Errorf("novel not found: %w", err)
	}

	taskID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())
	task := &AnalysisTask{
		NovelID:        novelID,
		Status:         "pending",
		Progress:       0,
		Step:           "准备中",
		CreateOutlines: createOutlines,
		expiresAt:      time.Now().Add(24 * time.Hour),
		cancel:         cancel,
	}
	s.tasks.Store(taskID, task)

	// 启动一次性 TTL 清理协程
	s.cleanupOnce.Do(func() {
		go s.cleanupExpiredTasks()
	})

	log.Printf("[NovelAnalysis] StartAnalysis: novelID=%d", novel.ID)
	go s.runPipeline(ctx, task, tenantID, novel)
	return taskID, nil
}

// cleanupExpiredTasks 每小时清理过期任务，防止 sync.Map 无限增长
func (s *NovelAnalysisService) cleanupExpiredTasks() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.tasks.Range(func(k, v interface{}) bool {
				if t, ok := v.(*AnalysisTask); ok && now.After(t.expiresAt) {
					if t.cancel != nil {
						t.cancel()
					}
					s.tasks.Delete(k)
				}
				return true
			})
		case <-s.cleanupStop:
			return
		}
	}
}

// GetStatus 查询任务状态，返回字段快照（线程安全）
func (s *NovelAnalysisService) GetStatus(taskID string) (*AnalysisTask, error) {
	v, ok := s.tasks.Load(taskID)
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	task := v.(*AnalysisTask)
	if time.Now().After(task.expiresAt) {
		s.tasks.Delete(taskID)
		return nil, fmt.Errorf("task not found")
	}
	return task.snapshot(), nil
}

// ──────────────────────────────────────────────
// Pipeline 内部实现
// ──────────────────────────────────────────────

func (s *NovelAnalysisService) runPipeline(ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel) {
	log.Printf("[NovelAnalysis] runPipeline start: novelID=%d", novel.ID)
	defer func() {
		if task.cancel != nil {
			task.cancel() // 释放 ctx 资源
		}
	}()
	task.setStatus("running")

	// 预检：确认 AI 提供商可用，否则立即报错，避免全流程静默空跑
	if err := s.aiService.CheckAvailability(tenantID); err != nil {
		s.fail(task, "AI 提供商未配置或不可用，请在「模型管理」页面为至少一个文本生成提供商添加 API Key（错误："+err.Error()+"）")
		return
	}

	chapters, err := s.chapterRepo.ListByNovel(novel.ID)
	if err != nil {
		s.fail(task, "获取章节列表失败: "+err.Error())
		return
	}

	// ── Phase 1: 章节摘要 (0→20) ─────────────────────────────────────────────
	if len(chapters) > 0 {
		task.setStep("正在生成章节摘要...")
		if err := s.stepSummarizeChapters(ctx, task, tenantID, novel, chapters); err != nil {
			log.Printf("NovelAnalysis[%d]: stepSummarizeChapters warn: %v", novel.ID, err)
			task.addWarning("章节摘要生成失败: " + err.Error())
		}
		// 刷新摘要
		if refreshed, err := s.chapterRepo.ListByNovel(novel.ID); err == nil {
			chapters = refreshed
		}
	}
	task.setProgress(20)

	// ── Phase 2: 并发提取 角色/物品/世界观/剧情点/场景锚点 (20→70) ─────────
	task.setStep("正在同步提取角色、物品、世界观、剧情点、场景锚点...")
	{
		type phaseTask struct {
			name string
			fn   func() error
		}
		phaseTasks := []phaseTask{
			{"角色", func() error {
				return s.stepExtractCharacters(ctx, task, tenantID, novel, chapters)
			}},
			{"物品", func() error {
				if s.itemRepo == nil {
					return nil
				}
				return s.stepExtractItems(ctx, task, tenantID, novel, chapters)
			}},
			{"世界观", func() error {
				return s.stepExtractWorldview(ctx, task, tenantID, novel, chapters)
			}},
			{"剧情点", func() error {
				return s.stepExtractPlotPoints(ctx, task, tenantID, novel, chapters)
			}},
			{"场景锚点", func() error {
				return s.stepExtractSceneAnchors(ctx, task, tenantID, novel, chapters)
			}},
		}

		var phWg sync.WaitGroup
		var doneCount atomic.Int32
		total := len(phaseTasks)
		for _, pt := range phaseTasks {
			pt := pt
			phWg.Add(1)
			go func() {
				defer phWg.Done()
				if err := pt.fn(); err != nil {
					msg := fmt.Sprintf("%s提取失败: %v", pt.name, err)
					log.Printf("NovelAnalysis[%d]: step%s warn: %v", novel.ID, pt.name, err)
					task.addWarning(msg)
				}
				n := int(doneCount.Add(1))
				task.setProgress(20 + n*50/total)
			}()
		}
		phWg.Wait()
	}
	task.setProgress(70)

	// ── Phase 3: 技能生成（依赖角色，顺序执行）(70→78) ───────────────────────
	task.setStep("正在生成技能数据...")
	if s.skillService != nil {
		if err := s.stepGenerateSkills(ctx, task, novel); err != nil {
			log.Printf("NovelAnalysis[%d]: stepGenerateSkills warn: %v", novel.ID, err)
		}
	}
	task.setProgress(78)

	// ── Phase 4: 大纲 + 设置 并行 (78→90) ────────────────────────────────────
	task.setStep("正在生成大纲与设置...")
	var outline *OutlineResult
	{
		var phWg sync.WaitGroup
		phWg.Add(2)
		go func() {
			defer phWg.Done()
			var oErr error
			outline, oErr = s.stepGenerateOutline(ctx, task, tenantID, novel)
			if oErr != nil {
				log.Printf("NovelAnalysis[%d]: stepGenerateOutline warn: %v", novel.ID, oErr)
			}
		}()
		go func() {
			defer phWg.Done()
			if err := s.stepUpdateNovelSettings(ctx, task, tenantID, novel, chapters); err != nil {
				log.Printf("NovelAnalysis[%d]: stepUpdateNovelSettings warn: %v", novel.ID, err)
			}
		}()
		phWg.Wait()
	}
	task.setProgress(90)

	// ── Phase 5: 章节大纲 (90→95) ─────────────────────────────────────────────
	if outline != nil && len(outline.Chapters) > 0 {
		task.setStep("正在创建章节大纲...")
		if err := s.stepCreateChapterOutlines(ctx, task, novel, outline); err != nil {
			log.Printf("NovelAnalysis[%d]: stepCreateChapterOutlines warn: %v", novel.ID, err)
		}
	}
	task.setProgress(95)

	// ── Phase 6: 收尾 (95→100) ────────────────────────────────────────────────
	task.setStep("收尾中...")
	if err := s.stepFinalize(task, novel); err != nil {
		log.Printf("NovelAnalysis[%d]: stepFinalize warn: %v", novel.ID, err)
	}

	task.setProgress(100)
	task.setStep("分析完成")
	task.setStatus("completed")
	log.Printf("NovelAnalysis[%d]: pipeline completed", novel.ID)
	log.Printf("[NovelAnalysis] runPipeline done: novelID=%d", novel.ID)
}

// ──────────────────────────────────────────────
// 共享 JSON 结构体（pipeline 各步骤复用）
// ──────────────────────────────────────────────

type analysisAbilityJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type analysisDialogueStyleJSON struct {
	VocabularyLevel string   `json:"vocabulary_level"`
	Patterns        []string `json:"patterns"`
	SpeechHabits    string   `json:"speech_habits"`
}

type analysisCharJSON struct {
	Name            string                    `json:"name"`
	Role            string                    `json:"role"`
	Archetype       string                    `json:"archetype"`
	Appearance      string                    `json:"appearance"`
	Personality     string                    `json:"personality"`
	PersonalityTags []string                  `json:"personality_tags"`
	Background      string                    `json:"background"`
	CharacterArc    string                    `json:"character_arc"`
	Abilities       []analysisAbilityJSON     `json:"abilities"`
	DialogueStyle   analysisDialogueStyleJSON `json:"dialogue_style"`
	VisualPrompt    string                    `json:"visual_prompt"`
}

type analysisItemJSON struct {
	Name         string               `json:"name"`
	Category     string               `json:"category"`
	Appearance   string               `json:"appearance"`
	Location     string               `json:"location"`
	Owner        string               `json:"owner"`
	Significance string               `json:"significance"`
	Abilities    []analysisAbilityJSON `json:"abilities"`
	VisualPrompt string               `json:"visual_prompt"`
}

// buildChapterSummariesText 从章节列表构建摘要文本，最多取 maxChapters 章，截断至 maxLen
func buildChapterSummariesText(chapters []*model.Chapter, maxChapters, maxLen int) string {
	n := len(chapters)
	if n > maxChapters {
		n = maxChapters
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		ch := chapters[i]
		summary := ch.Summary
		if summary == "" {
			summary = truncateForPrompt(ch.Content, 500)
		}
		if summary != "" {
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, summary))
		}
	}
	return truncateForPrompt(sb.String(), maxLen)
}

// stepSummarizeChapters 为每章生成摘要（复用 chapter_summary.tmpl）。
// Pipeline 关键路径只处理前 maxForPipeline 章（并发），其余章节后台异步补全。
func (s *NovelAnalysisService) stepSummarizeChapters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepSummarizeChapters: novelID=%d chapters=%d", novel.ID, len(chapters))
	tmplStr := loadPromptTemplate("chapter_summary.tmpl")
	tmpl, err := template.New("chapter_summary").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse chapter_summary.tmpl: %w", err)
	}

	// 关键路径：只并发处理前 N 章（Phase 2 提取最多用前 5 章摘要）
	const maxForPipeline = 15
	const maxConcurrent = 3

	pipeline := chapters
	if len(pipeline) > maxForPipeline {
		pipeline = chapters[:maxForPipeline]
	}

	total := len(pipeline)
	if total == 0 {
		return nil
	}

	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for _, ch := range pipeline {
		ch := ch
		if ch.Summary != "" || ch.Content == "" {
			mu.Lock()
			done++
			task.setProgress(done * 20 / total)
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, map[string]interface{}{
				"NovelTitle":   novel.Title,
				"ChapterNo":    ch.ChapterNo,
				"ChapterTitle": ch.Title,
				"Content":      truncateForPrompt(ch.Content, 6000),
			}); err != nil {
				log.Printf("NovelAnalysis: chapter %d tmpl exec: %v", ch.ChapterNo, err)
			} else if summary, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_summary", buf.String(), ""); err != nil {
				log.Printf("NovelAnalysis: chapter %d summary AI error: %v", ch.ChapterNo, err)
			} else {
				ch.Summary = strings.TrimSpace(summary)
				if err := s.chapterRepo.Update(ch); err != nil {
					log.Printf("NovelAnalysis: chapter %d save summary error: %v", ch.ChapterNo, err)
				}
			}
			mu.Lock()
			done++
			task.setProgress(done * 20 / total)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// 剩余章节后台异步补全摘要，不阻塞 pipeline
	if len(chapters) > maxForPipeline {
		go s.summarizeChaptersBackground(ctx, tenantID, novel, chapters[maxForPipeline:], tmpl)
	}
	return nil
}

// summarizeChaptersBackground 后台低优先级补全剩余章节摘要（不影响 pipeline 进度）
func (s *NovelAnalysisService) summarizeChaptersBackground(
	ctx context.Context, tenantID uint, novel *model.Novel,
	chapters []*model.Chapter, tmpl *template.Template,
) {
	log.Printf("[NovelAnalysis] summarizeChaptersBackground: novelID=%d chapters=%d", novel.ID, len(chapters))
	const maxConcurrent = 2
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, ch := range chapters {
		if ch.Summary != "" || ch.Content == "" {
			continue
		}
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
				if r := recover(); r != nil {
					log.Printf("NovelAnalysis[bg][%d]: chapter %d panic: %v", novel.ID, ch.ChapterNo, r)
				}
			}()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, map[string]interface{}{
				"NovelTitle":   novel.Title,
				"ChapterNo":    ch.ChapterNo,
				"ChapterTitle": ch.Title,
				"Content":      truncateForPrompt(ch.Content, 6000),
			}); err != nil {
				return
			}
			summary, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_summary", buf.String(), "")
			if err != nil {
				log.Printf("NovelAnalysis[bg][%d]: chapter %d summary error: %v", novel.ID, ch.ChapterNo, err)
				return
			}
			ch.Summary = strings.TrimSpace(summary)
			if err := s.chapterRepo.Update(ch); err != nil {
				log.Printf("NovelAnalysis[bg][%d]: chapter %d save error: %v", novel.ID, ch.ChapterNo, err)
			}
		}()
	}
	wg.Wait()
	log.Printf("NovelAnalysis[bg][%d]: background summarization complete (%d chapters)", novel.ID, len(chapters))
}

// stepExtractCharacters 从章节摘要提取角色；无章节时基于小说描述生成角色
func (s *NovelAnalysisService) stepExtractCharacters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepExtractCharacters: novelID=%d", novel.ID)
	var summariesText string
	if len(chapters) > 0 {
		summariesText = buildChapterSummariesText(chapters, 15, 8000)
	} else {
		summariesText = novel.Description
	}
	if summariesText == "" {
		summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计主要角色。", novel.Genre, novel.Title)
	}

	tmplStr := loadPromptTemplate("extract_characters.tmpl")
	tmpl, err := template.New("extract_characters").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse extract_characters.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle": novel.Title,
		"Genre":      novel.Genre,
		"Summaries":  summariesText,
	}); err != nil {
		return err
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_characters", buf.String(), "")
	if err != nil {
		return fmt.Errorf("AI extract_characters: %w", err)
	}

	chars, err := parseCharacterJSONResult(result)
	if err != nil {
		return fmt.Errorf("parse characters JSON: %w", err)
	}

	// 获取已有角色，用于去重
	existingChars, _ := s.characterRepo.ListByNovel(novel.ID)
	existingNames := make(map[string]bool, len(existingChars))
	for _, ec := range existingChars {
		existingNames[strings.ToLower(ec.Name)] = true
	}

	const maxMainCharacters = 20
	var createdChars []*model.Character
	for _, c := range chars {
		if len(createdChars)+len(existingChars) >= maxMainCharacters {
			break
		}
		if c.Name == "" {
			continue
		}
		if existingNames[strings.ToLower(c.Name)] {
			continue // 去重
		}
		// 项目级只保留主要角色
		if c.Role == "minor" {
			continue
		}
		abilities := ""
		if len(c.Abilities) > 0 {
			if ab, err := json.Marshal(c.Abilities); err == nil {
				abilities = string(ab)
			}
		}
		personalityTags := ""
		if len(c.PersonalityTags) > 0 {
			if pt, err := json.Marshal(c.PersonalityTags); err == nil {
				personalityTags = string(pt)
			}
		}
		dialogueStyle := ""
		if c.DialogueStyle.VocabularyLevel != "" || len(c.DialogueStyle.Patterns) > 0 {
			if ds, err := json.Marshal(c.DialogueStyle); err == nil {
				dialogueStyle = string(ds)
			}
		}
		visualDesign := ""
		if c.VisualPrompt != "" {
			if vd, err := json.Marshal(map[string]string{"image_prompt": c.VisualPrompt}); err == nil {
				visualDesign = string(vd)
			}
		}
		role := c.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			role = "supporting"
		}
		char := &model.Character{
			NovelID:         novel.ID,
			UUID:            uuid.New().String(),
			Name:            c.Name,
			Role:            role,
			Archetype:       c.Archetype,
			Appearance:      c.Appearance,
			Personality:     c.Personality,
			PersonalityTags: personalityTags,
			Background:      c.Background,
			CharacterArc:    c.CharacterArc,
			Abilities:       abilities,
			DialogueStyle:   dialogueStyle,
			VisualDesign:    visualDesign,
			Status:          "active",
		}
		if err := s.characterRepo.Create(char); err != nil {
			log.Printf("NovelAnalysis: create character %q: %v", c.Name, err)
			continue
		}
		existingNames[strings.ToLower(c.Name)] = true
		createdChars = append(createdChars, char)
	}

	// 异步生成三视图（不阻塞 pipeline，传递父 ctx 避免孤儿 goroutine）
	if len(createdChars) > 0 {
		go s.generateThreeViewsAsync(ctx, createdChars)
	}
	log.Printf("[NovelAnalysis] stepExtractCharacters done: novelID=%d characters created", novel.ID)
	return nil
}

// stepExtractWorldview 从章节摘要提取世界观；无章节时基于小说描述生成世界观
func (s *NovelAnalysisService) stepExtractWorldview(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepExtractWorldview: novelID=%d", novel.ID)
	var summariesText string
	if len(chapters) > 0 {
		var sb strings.Builder
		for _, ch := range chapters {
			if ch.Summary != "" {
				sb.WriteString(fmt.Sprintf("第%d章：%s\n", ch.ChapterNo, ch.Summary))
			}
		}
		summariesText = truncateForPrompt(sb.String(), 2000)
	}
	if summariesText == "" {
		if novel.Description != "" {
			summariesText = novel.Description
		} else {
			summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计世界观体系。", novel.Genre, novel.Title)
		}
	}

	tmplStr := loadPromptTemplate("extract_worldview.tmpl")
	tmpl, err := template.New("extract_worldview").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse extract_worldview.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle": novel.Title,
		"Genre":      novel.Genre,
		"Summaries":  summariesText,
	}); err != nil {
		return err
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_worldview", buf.String(), "")
	if err != nil {
		return fmt.Errorf("AI extract_worldview: %w", err)
	}
	type wvRaw struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		MagicSystem json.RawMessage `json:"magic_system"`
		Geography   json.RawMessage `json:"geography"`
		History     json.RawMessage `json:"history"`
		Culture     json.RawMessage `json:"culture"`
		Technology  json.RawMessage `json:"technology"`
		Rules       json.RawMessage `json:"rules"`
		CheatSystem json.RawMessage `json:"cheat_system"`
	}
	var raw wvRaw
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &raw); err != nil {
		return fmt.Errorf("parse worldview JSON: %w", err)
	}
	parseField := func(data json.RawMessage) string {
		if len(data) == 0 {
			return ""
		}
		var s string
		if err := json.Unmarshal(data, &s); err == nil {
			return s
		}
		var arr []string
		if err := json.Unmarshal(data, &arr); err == nil {
			return strings.Join(arr, "；")
		}
		return strings.Trim(string(data), `"`)
	}
	type wvParsed struct {
		Name, Description, MagicSystem, Geography, History, Culture, Technology, Rules, CheatSystem string
	}
	wv := wvParsed{
		Name:        parseField(raw.Name),
		Description: parseField(raw.Description),
		MagicSystem: parseField(raw.MagicSystem),
		Geography:   parseField(raw.Geography),
		History:     parseField(raw.History),
		Culture:     parseField(raw.Culture),
		Technology:  parseField(raw.Technology),
		Rules:       parseField(raw.Rules),
		CheatSystem: parseField(raw.CheatSystem),
	}

	if wv.Name == "" {
		wv.Name = novel.Title + " 世界观"
	}

	worldview := &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        wv.Name,
		Description: wv.Description,
		Genre:       novel.Genre,
		MagicSystem: wv.MagicSystem,
		Geography:   wv.Geography,
		History:     wv.History,
		Culture:     wv.Culture,
		Technology:  wv.Technology,
		Rules:       wv.Rules,
		CheatSystem: wv.CheatSystem,
	}
	log.Printf("NovelAnalysis[%d]: creating worldview %q", novel.ID, worldview.Name)
	if err := s.worldviewRepo.Create(worldview); err != nil {
		return fmt.Errorf("save worldview: %w", err)
	}

	// 关联到小说
	if novel.WorldviewID == nil {
		if err := s.novelRepo.UpdateFields(novel.ID, map[string]interface{}{
			"worldview_id": worldview.ID,
		}); err != nil {
			log.Printf("NovelAnalysis: link worldview to novel %d: %v", novel.ID, err)
		} else {
			novel.WorldviewID = &worldview.ID
		}
	}
	return nil
}

// stepGenerateOutline 生成大纲并写入小说描述，返回大纲结果供后续步骤使用
func (s *NovelAnalysisService) stepGenerateOutline(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel,
) (*OutlineResult, error) {
	chapterCount, _ := s.chapterRepo.CountByNovel(novel.ID)

	// AI 创建模式：无章节时使用 TargetChapters，最低 30 章
	chapterNum := int(chapterCount)
	if chapterNum == 0 {
		chapterNum = novel.TargetChapters
	}
	if chapterNum <= 0 {
		chapterNum = 30
	}

	req := &GenerateOutlineRequest{
		NovelID:    novel.ID,
		ChapterNum: chapterNum,
	}
	outline, err := s.novelService.GenerateOutline(tenantID, req)
	if err != nil {
		return nil, fmt.Errorf("GenerateOutline: %w", err)
	}

	// 若小说描述为空，写入 outline summary
	if novel.Description == "" && outline.Summary != "" {
		if err := s.novelRepo.UpdateFields(novel.ID, map[string]interface{}{
			"description": outline.Summary,
		}); err != nil {
			log.Printf("NovelAnalysis: update novel description: %v", err)
		}
	}
	return outline, nil
}

// stepCreateChapterOutlines 根据大纲为每个章节创建占位记录
func (s *NovelAnalysisService) stepCreateChapterOutlines(
	ctx context.Context, task *AnalysisTask, novel *model.Novel, outline *OutlineResult,
) error {
	for _, co := range outline.Chapters {
		if co.ChapterNo <= 0 {
			continue
		}
		// 检查是否已存在同 NovelID+ChapterNo 的记录
		existing, err := s.chapterRepo.GetByNovelAndChapterNo(novel.ID, co.ChapterNo)
		if err == nil && existing != nil {
			continue // 已存在，跳过
		}
		ch := &model.Chapter{
			UUID:      uuid.New().String(),
			NovelID:   novel.ID,
			ChapterNo: co.ChapterNo,
			Title:     co.Title,
			Summary:   co.Summary,
			Status:    "draft",
			Content:   "",
		}
		if err := s.chapterRepo.Create(ch); err != nil {
			log.Printf("NovelAnalysis: create chapter placeholder %d: %v", co.ChapterNo, err)
		}
	}
	return nil
}

// stepFinalize 收尾：更新小说状态
func (s *NovelAnalysisService) stepFinalize(task *AnalysisTask, novel *model.Novel) error {
	fields := map[string]interface{}{
		"status": "writing",
	}
	if err := s.novelRepo.UpdateFields(novel.ID, fields); err != nil {
		return fmt.Errorf("update novel status: %w", err)
	}
	return nil
}

func (s *NovelAnalysisService) fail(task *AnalysisTask, msg string) {
	task.setStatus("failed")
	task.setError(msg)
	task.setStep("失败")
}

// generateThreeViewsAsync 为角色异步生成三视图（正/侧/背面），失败仅记录日志不影响流程
func (s *NovelAnalysisService) generateThreeViewsAsync(ctx context.Context, chars []*model.Character) {
	log.Printf("[NovelAnalysis] generateThreeViewsAsync: characters=%d", len(chars))
	// 优先为主角和反派生成，配角次之
	sorted := make([]*model.Character, 0, len(chars))
	for _, c := range chars {
		if c.Role == "protagonist" || c.Role == "antagonist" {
			sorted = append(sorted, c)
		}
	}
	for _, c := range chars {
		if c.Role == "supporting" {
			sorted = append(sorted, c)
		}
	}

	for _, char := range sorted {
		// 从 VisualDesign JSON 中提取 image_prompt
		basePrompt := ""
		if char.VisualDesign != "" {
			var vd map[string]string
			if err := json.Unmarshal([]byte(char.VisualDesign), &vd); err == nil {
				basePrompt = vd["image_prompt"]
			}
		}
		// 若无 visual_prompt，用 Appearance 构造简单英文 prompt
		if basePrompt == "" {
			if char.Appearance != "" {
				basePrompt = fmt.Sprintf("character named %s, %s, high quality illustration", char.Name, char.Appearance)
			} else {
				basePrompt = fmt.Sprintf("character named %s, full body, high quality illustration", char.Name)
			}
		}

		views := []struct {
			suffix    string
			fieldSet  func(c *model.Character, url string)
		}{
			{"front view, facing camera, full body, character design sheet", func(c *model.Character, url string) { c.ThreeViewFront = url }},
			{"side profile view, full body, character design sheet", func(c *model.Character, url string) { c.ThreeViewSide = url }},
			{"back view, full body, character design sheet", func(c *model.Character, url string) { c.ThreeViewBack = url }},
		}

		changed := false
		for _, v := range views {
			prompt := basePrompt + ", " + v.suffix
			url, err := s.aiService.GenerateCharacterThreeView(ctx, 0, "", prompt, "", "", "")
			if err != nil {
				log.Printf("NovelAnalysis: three-view %q for char %d: %v", v.suffix, char.ID, err)
				continue
			}
			if url != "" {
				v.fieldSet(char, url)
				changed = true
			}
			// 小间隔避免限速
			time.Sleep(500 * time.Millisecond)
		}

		if changed {
			if err := s.characterRepo.Update(char); err != nil {
				log.Printf("NovelAnalysis: save three-view for char %d: %v", char.ID, err)
			}
		}
	}
}

// stepGenerateSkills 逐章提取技能；结果可为空，失败不影响 pipeline
func (s *NovelAnalysisService) stepGenerateSkills(
	ctx context.Context, task *AnalysisTask, novel *model.Novel,
) error {
	log.Printf("[NovelAnalysis] stepGenerateSkills: novelID=%d", novel.ID)
	// 若已有技能则跳过
	existing, _ := s.skillService.ListSkills(novel.ID, repository.ListSkillsOpts{})
	if len(existing) > 0 {
		log.Printf("NovelAnalysis[%d]: skills already exist (%d), skip", novel.ID, len(existing))
		task.setProgress(68)
		return nil
	}

	skills, err := s.skillService.AIExtractAllFromNovel(novel.TenantID, novel.ID)
	if err != nil {
		log.Printf("NovelAnalysis[%d]: skill extraction warn: %v", novel.ID, err)
		task.setProgress(68)
		return nil // 非致命错误，不影响 pipeline
	}
	log.Printf("NovelAnalysis[%d]: extracted %d skills from chapters", novel.ID, len(skills))
	task.setProgress(68)
	return nil
}

// stepExtractItems 逐章并发提取物品信息
func (s *NovelAnalysisService) stepExtractItems(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepExtractItems: novelID=%d", novel.ID)
	// 若已有物品则跳过
	existing, _ := s.itemRepo.ListByNovel(novel.ID)
	if len(existing) > 0 {
		log.Printf("NovelAnalysis[%d]: items already exist (%d), skip", novel.ID, len(existing))
		return nil
	}

	items, err := s.itemService.AIExtractAllFromNovel(tenantID, novel.ID)
	if err != nil {
		return fmt.Errorf("AIExtractAllFromNovel items: %w", err)
	}
	log.Printf("NovelAnalysis[%d]: extracted %d items", novel.ID, len(items))
	return nil
}

// stepExtractPlotPoints 从章节中提取剧情点（伏笔/冲突/转折等）
func (s *NovelAnalysisService) stepExtractPlotPoints(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepExtractPlotPoints: novelID=%d", novel.ID)
	if s.plotPointService == nil {
		return nil
	}
	// 若已有剧情点则跳过
	existing, _ := s.plotPointService.ListByNovel(novel.ID, "", false)
	if len(existing) > 0 {
		log.Printf("NovelAnalysis[%d]: plot points already exist (%d), skip", novel.ID, len(existing))
		return nil
	}
	pps, err := s.plotPointService.AIExtractFromNovel(tenantID, novel.ID)
	if err != nil {
		return fmt.Errorf("AIExtractPlotPoints: %w", err)
	}
	log.Printf("NovelAnalysis[%d]: extracted %d plot points", novel.ID, len(pps))
	return nil
}

// stepExtractSceneAnchors 从前几章提取场景锚点（并发提取，最多 10 章）
func (s *NovelAnalysisService) stepExtractSceneAnchors(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	log.Printf("[NovelAnalysis] stepExtractSceneAnchors: novelID=%d chapters=%d", novel.ID, len(chapters))
	if s.sceneAnchorService == nil {
		return nil
	}
	// 若已有锚点则跳过
	existing, _ := s.sceneAnchorService.ListByNovel(novel.ID)
	if len(existing) > 0 {
		log.Printf("NovelAnalysis[%d]: scene anchors already exist (%d), skip", novel.ID, len(existing))
		return nil
	}
	// 从前 10 章提取，覆盖更多场景
	maxCh := 10
	if len(chapters) < maxCh {
		maxCh = len(chapters)
	}

	// 过滤出有内容的章节
	candidates := make([]*model.Chapter, 0, maxCh)
	for i := 0; i < maxCh; i++ {
		if chapters[i].Content != "" {
			candidates = append(candidates, chapters[i])
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// 并发提取，最多 3 个并发
	const maxConcurrent = 3
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, ch := range candidates {
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			anchors, err := s.sceneAnchorService.ExtractFromChapter(ctx, tenantID, novel.ID, novel.Title, ch.Content)
			if err != nil {
				log.Printf("NovelAnalysis[%d]: ExtractSceneAnchors ch%d warn: %v", novel.ID, ch.ChapterNo, err)
				return
			}
			log.Printf("NovelAnalysis[%d]: extracted %d scene anchors from ch%d", novel.ID, len(anchors), ch.ChapterNo)
		}()
	}
	wg.Wait()
	return nil
}

// stepUpdateNovelSettings 使用 AI 自动填写小说设置（类型、写作风格、图片风格、简介、目标字数/章节数）
// 每个子字段独立判断是否需要填充，互不阻断。
func (s *NovelAnalysisService) stepUpdateNovelSettings(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	needStylePrompt := novel.StylePrompt == ""
	needDescription := novel.Description == ""
	needGenre := novel.Genre == "" || novel.Genre == "unknown"
	needImageStyle := novel.ImageStyle == ""
	needTargets := novel.TargetChapters == 0 || novel.TargetWordCount == 0

	if !needStylePrompt && !needDescription && !needGenre && !needImageStyle && !needTargets {
		return nil // 所有字段已填，无需更新
	}

	sampleContent := ""
	for _, ch := range chapters {
		if ch.Content != "" {
			sampleContent = truncateForPrompt(ch.Content, 2000)
			break
		}
	}
	if sampleContent == "" {
		sampleContent = novel.Description
	}
	if sampleContent == "" {
		return nil
	}

	updates := map[string]interface{}{}

	// ── 1. 自动识别小说类型 ─────────────────────────────────────────────────
	if needGenre {
		validGenres := []string{
			"现代言情", "古代言情", "幻想言情", "历史", "军事", "科幻",
			"游戏", "游戏竞技", "玄幻奇幻", "都市", "奇闻异事", "武侠仙侠",
			"体育", "N次元", "文学艺术",
		}
		genrePrompt := fmt.Sprintf(`小说《%s》部分内容：
%s

请从以下类型中选择最符合的一个：%s
只输出类型名称，不要任何其他内容。`, novel.Title, sampleContent, strings.Join(validGenres, "、"))
		if g, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "genre_detection", genrePrompt, ""); err == nil {
			g = strings.TrimSpace(g)
			for _, vg := range validGenres {
				if strings.Contains(g, vg) {
					novel.Genre = vg
					updates["genre"] = vg
					break
				}
			}
		}
	}

	// ── 2. 根据类型自动推断图片风格（仅当未设置时）─────────────────────────
	if needImageStyle {
		genreStyleMap := map[string]string{
			"武侠仙侠": "ink_painting", "古代言情": "ink_painting", "历史": "ink_painting",
			"玄幻奇幻": "anime", "幻想言情": "anime", "游戏": "anime", "游戏竞技": "anime", "N次元": "anime",
			"科幻": "cyberpunk", "军事": "realistic",
			"都市": "realistic", "现代言情": "realistic", "体育": "realistic",
		}
		if style, ok := genreStyleMap[novel.Genre]; ok {
			updates["image_style"] = style
		}
	}

	// ── 3. 写作风格描述 ─────────────────────────────────────────────────────
	if needStylePrompt {
		stylePrompt := fmt.Sprintf(`小说《%s》，类型：%s。
以下是部分章节内容示例：
%s

请用简洁中文（100字以内）描述本书写作风格特点（叙事视角、语言风格、情感基调、节奏特点），作为AI续写的风格参考指引。只输出风格描述，不要标题或解释。`,
			novel.Title, novel.Genre, sampleContent)
		if styleText, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "style_analysis", stylePrompt, ""); err == nil {
			if styleText = strings.TrimSpace(styleText); styleText != "" {
				updates["style_prompt"] = styleText
			}
		}
	}

	// ── 4. 简介 ─────────────────────────────────────────────────────────────
	if needDescription {
		descPrompt := fmt.Sprintf(`小说《%s》（类型：%s）部分内容：
%s

请为这部小说写一段150字以内的作品简介：
- 介绍故事背景、主人公与核心冲突
- 语言生动，吸引读者，不剧透结局
- 只输出简介正文，无需标题或解释`, novel.Title, novel.Genre, sampleContent)
		if desc, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "novel_description", descPrompt, ""); err == nil {
			if desc = strings.TrimSpace(desc); desc != "" {
				updates["description"] = desc
			}
		}
	}

	// ── 5. 目标章节数 / 目标字数（从已有章节推断）──────────────────────────
	if needTargets && len(chapters) > 0 {
		if novel.TargetChapters == 0 {
			updates["target_chapters"] = len(chapters)
		}
		if novel.TargetWordCount == 0 {
			totalWords := 0
			for _, ch := range chapters {
				totalWords += ch.WordCount
			}
			if totalWords == 0 {
				// WordCount 未计算时按 2000字/章 估算
				totalWords = len(chapters) * 2000
			}
			updates["target_word_count"] = totalWords
		}
	}

	if len(updates) == 0 {
		return nil
	}
	return s.novelRepo.UpdateFields(novel.ID, updates)
}
