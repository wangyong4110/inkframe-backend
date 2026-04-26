package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
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

	mu       sync.RWMutex `json:"-"`
	Status   string       `json:"status"`         // pending / running / completed / failed
	Progress int          `json:"progress"`       // 0-100
	Step     string       `json:"step"`
	Error    string       `json:"error,omitempty"`
}

func (t *AnalysisTask) setStatus(s string)   { t.mu.Lock(); t.Status = s; t.mu.Unlock() }
func (t *AnalysisTask) setProgress(p int)    { t.mu.Lock(); t.Progress = p; t.mu.Unlock() }
func (t *AnalysisTask) setStep(s string)     { t.mu.Lock(); t.Step = s; t.mu.Unlock() }
func (t *AnalysisTask) setError(e string)    { t.mu.Lock(); t.Error = e; t.mu.Unlock() }

// snapshot 返回字段快照，供 handler 安全读取
func (t *AnalysisTask) snapshot() *AnalysisTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return &AnalysisTask{
		NovelID:  t.NovelID,
		Status:   t.Status,
		Progress: t.Progress,
		Step:     t.Step,
		Error:    t.Error,
	}
}

// NovelAnalysisService 小说分析服务（异步 Pipeline）
type NovelAnalysisService struct {
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
	characterRepo *repository.CharacterRepository
	worldviewRepo *repository.WorldviewRepository
	itemRepo      *repository.ItemRepository
	skillService  *SkillService
	novelService  *NovelService
	aiService     *AIService
	tasks       sync.Map   // taskID(string) → *AnalysisTask
	cleanupOnce sync.Once
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
	}
}

// WithItemRepo 注入物品仓库（可选，支持物品提取步骤）
func (s *NovelAnalysisService) WithItemRepo(itemRepo *repository.ItemRepository) *NovelAnalysisService {
	s.itemRepo = itemRepo
	return s
}

// WithSkillService 注入技能服务（可选，支持技能自动生成步骤）
func (s *NovelAnalysisService) WithSkillService(skillService *SkillService) *NovelAnalysisService {
	s.skillService = skillService
	return s
}

// StartAnalysis 启动分析任务，返回 taskID
func (s *NovelAnalysisService) StartAnalysis(tenantID, novelID uint, createOutlines bool) (string, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return "", fmt.Errorf("novel not found: %w", err)
	}

	taskID := uuid.New().String()
	task := &AnalysisTask{
		NovelID:        novelID,
		Status:         "pending",
		Progress:       0,
		Step:           "准备中",
		CreateOutlines: createOutlines,
		expiresAt:      time.Now().Add(24 * time.Hour),
	}
	s.tasks.Store(taskID, task)

	// 启动一次性 TTL 清理协程
	s.cleanupOnce.Do(func() {
		go s.cleanupExpiredTasks()
	})

	go s.runPipeline(context.Background(), task, tenantID, novel)
	return taskID, nil
}

// cleanupExpiredTasks 每小时清理过期任务，防止 sync.Map 无限增长
func (s *NovelAnalysisService) cleanupExpiredTasks() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.tasks.Range(func(k, v interface{}) bool {
			if t, ok := v.(*AnalysisTask); ok && now.After(t.expiresAt) {
				s.tasks.Delete(k)
			}
			return true
		})
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
	task.setStatus("running")

	chapters, err := s.chapterRepo.ListByNovel(novel.ID)
	if err != nil {
		s.fail(task, "获取章节列表失败: "+err.Error())
		return
	}

	// Step 1: 章节摘要 (0→30) — 仅在有章节内容时执行
	if len(chapters) > 0 {
		task.setStep("正在生成章节摘要...")
		if err := s.stepSummarizeChapters(ctx, task, tenantID, novel, chapters); err != nil {
			log.Printf("NovelAnalysis[%d]: stepSummarizeChapters warn: %v", novel.ID, err)
		}
	}

	// Step 2: 提取/生成角色 (30→55)
	task.setProgress(30)
	task.setStep("正在生成角色信息...")
	if err := s.stepExtractCharacters(ctx, task, tenantID, novel, chapters); err != nil {
		log.Printf("NovelAnalysis[%d]: stepExtractCharacters warn: %v", novel.ID, err)
	}

	// Step 2.5: 提取/生成物品 (55→62)
	task.setProgress(55)
	task.setStep("正在提取物品信息...")
	if s.itemRepo != nil {
		if err := s.stepExtractItems(ctx, task, tenantID, novel, chapters); err != nil {
			log.Printf("NovelAnalysis[%d]: stepExtractItems warn: %v", novel.ID, err)
		}
	}

	// Step 2.75: 生成技能数据 (62→68)
	task.setProgress(62)
	task.setStep("正在生成技能数据...")
	if s.skillService != nil {
		if err := s.stepGenerateSkills(ctx, task, novel); err != nil {
			log.Printf("NovelAnalysis[%d]: stepGenerateSkills warn: %v", novel.ID, err)
		}
	}

	// Step 3: 提取/生成世界观 (68→75)
	task.setProgress(68)
	task.setStep("正在生成世界观...")
	if err := s.stepExtractWorldview(ctx, task, tenantID, novel, chapters); err != nil {
		log.Printf("NovelAnalysis[%d]: stepExtractWorldview warn: %v", novel.ID, err)
		task.setStep("世界观生成失败（已跳过）")
		task.setError("世界观生成失败: " + err.Error())
	}

	// Step 4: 生成大纲 (75→95)
	task.setProgress(75)
	task.setStep("正在生成大纲...")
	outline, err := s.stepGenerateOutline(ctx, task, tenantID, novel)
	if err != nil {
		log.Printf("NovelAnalysis[%d]: stepGenerateOutline warn: %v", novel.ID, err)
	}

	// Step 5: 收尾 (95→100)
	task.setProgress(95)
	task.setStep("收尾中...")
	if err := s.stepFinalize(task, novel); err != nil {
		log.Printf("NovelAnalysis[%d]: stepFinalize warn: %v", novel.ID, err)
	}

	// Step 6 (可选): 创建章节占位记录
	if task.CreateOutlines && outline != nil && len(outline.Chapters) > 0 {
		task.setStep("正在创建章节大纲...")
		if err := s.stepCreateChapterOutlines(ctx, task, novel, outline); err != nil {
			log.Printf("NovelAnalysis[%d]: stepCreateChapterOutlines warn: %v", novel.ID, err)
		}
	}

	task.setProgress(100)
	task.setStep("分析完成")
	task.setStatus("completed")
	log.Printf("NovelAnalysis[%d]: pipeline completed", novel.ID)
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

// stepSummarizeChapters 为每章生成摘要（复用 chapter_summary.tmpl）
func (s *NovelAnalysisService) stepSummarizeChapters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	tmplStr := loadPromptTemplate("chapter_summary.tmpl")
	tmpl, err := template.New("chapter_summary").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse chapter_summary.tmpl: %w", err)
	}

	total := len(chapters)
	for i, ch := range chapters {
		if ch.Summary != "" {
			continue // 已有摘要则跳过
		}
		if ch.Content == "" {
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, map[string]interface{}{
			"NovelTitle":   novel.Title,
			"ChapterNo":    ch.ChapterNo,
			"ChapterTitle": ch.Title,
			"Content":      truncateForPrompt(ch.Content, 6000),
		}); err != nil {
			log.Printf("NovelAnalysis: chapter %d tmpl exec: %v", ch.ChapterNo, err)
			continue
		}
		summary, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_summary", buf.String(), "")
		if err != nil {
			log.Printf("NovelAnalysis: chapter %d summary AI error: %v", ch.ChapterNo, err)
			continue
		}
		ch.Summary = strings.TrimSpace(summary)
		if err := s.chapterRepo.Update(ch); err != nil {
			log.Printf("NovelAnalysis: chapter %d save summary error: %v", ch.ChapterNo, err)
		}
		// 更新进度 0→30
		task.setProgress((i + 1) * 30 / total)
		// 限速避免大量章节并发
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// stepExtractCharacters 从章节摘要提取角色；无章节时基于小说描述生成角色
func (s *NovelAnalysisService) stepExtractCharacters(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	var summariesText string
	if len(chapters) > 0 {
		summariesText = buildChapterSummariesText(chapters, 5, 3000)
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

	var chars []analysisCharJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &chars); err != nil {
		return fmt.Errorf("parse characters JSON: %w", err)
	}

	// 获取已有角色，用于去重
	existingChars, _ := s.characterRepo.ListByNovel(novel.ID)
	existingNames := make(map[string]bool, len(existingChars))
	for _, ec := range existingChars {
		existingNames[strings.ToLower(ec.Name)] = true
	}

	var createdChars []*model.Character
	for _, c := range chars {
		if c.Name == "" {
			continue
		}
		if existingNames[strings.ToLower(c.Name)] {
			continue // 去重
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
		// 更新进度 30→55（用已创建数量/总数，避免除零）
		if len(chars) > 0 {
			task.setProgress(30 + len(createdChars)*25/len(chars))
		}
	}

	// 异步生成三视图（不阻塞 pipeline，传递父 ctx 避免孤儿 goroutine）
	if len(createdChars) > 0 {
		go s.generateThreeViewsAsync(ctx, createdChars)
	}
	return nil
}

// stepExtractWorldview 从章节摘要提取世界观；无章节时基于小说描述生成世界观
func (s *NovelAnalysisService) stepExtractWorldview(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
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
	log.Printf("NovelAnalysis[%d]: stepExtractWorldview raw response (first 300): %.300s", novel.ID, result)

	type wvRaw struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		MagicSystem json.RawMessage `json:"magic_system"`
		Geography   json.RawMessage `json:"geography"`
		History     json.RawMessage `json:"history"`
		Culture     json.RawMessage `json:"culture"`
		Technology  json.RawMessage `json:"technology"`
		Rules       json.RawMessage `json:"rules"`
	}
	var raw wvRaw
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &raw); err != nil {
		log.Printf("NovelAnalysis[%d]: worldview JSON parse failed, cleaned: %.500s", novel.ID, cleaned)
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
		Name, Description, MagicSystem, Geography, History, Culture, Technology, Rules string
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
			url, err := s.aiService.GenerateCharacterThreeView(ctx, prompt)
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

// stepGenerateSkills 自动生成技能数据；结果可为空，失败不影响 pipeline
func (s *NovelAnalysisService) stepGenerateSkills(
	ctx context.Context, task *AnalysisTask, novel *model.Novel,
) error {
	// 若已有技能则跳过
	existing, _ := s.skillService.ListSkills(novel.ID, repository.ListSkillsOpts{})
	if len(existing) > 0 {
		log.Printf("NovelAnalysis[%d]: skills already exist (%d), skip generation", novel.ID, len(existing))
		task.setProgress(68)
		return nil
	}

	// 生成世界级技能（不绑定角色）
	worldReq := &model.GenerateSkillsRequest{
		Count: 5,
		Hints: fmt.Sprintf("请根据小说类型(%s)设计核心技能体系，涵盖不同类别，数量5个左右。", novel.Genre),
	}
	worldSkills, err := s.skillService.GenerateSkills(novel.ID, worldReq)
	if err != nil {
		log.Printf("NovelAnalysis[%d]: world skills gen warn: %v", novel.ID, err)
	} else {
		log.Printf("NovelAnalysis[%d]: generated %d world skills", novel.ID, len(worldSkills))
	}
	task.setProgress(65)

	// 为主角/反派并发生成专属技能（各角色独立，可并行）
	chars, _ := s.characterRepo.ListByNovel(novel.ID)
	var wg sync.WaitGroup
	for _, char := range chars {
		if char.Role != "protagonist" && char.Role != "antagonist" {
			continue
		}
		char := char // capture loop variable
		wg.Add(1)
		go func() {
			defer wg.Done()
			charReq := &model.GenerateSkillsRequest{
				CharacterID: &char.ID,
				Count:       3,
				Hints:       fmt.Sprintf("根据角色【%s】的背景和能力，设计专属技能，要有鲜明个性。", char.Name),
			}
			charSkills, err := s.skillService.GenerateSkills(novel.ID, charReq)
			if err != nil {
				log.Printf("NovelAnalysis[%d]: char %q skills gen warn: %v", novel.ID, char.Name, err)
				return
			}
			log.Printf("NovelAnalysis[%d]: generated %d skills for char %q", novel.ID, len(charSkills), char.Name)
		}()
	}
	wg.Wait()
	task.setProgress(68)
	return nil
}

// stepExtractItems 从章节摘要/小说描述提取物品信息
func (s *NovelAnalysisService) stepExtractItems(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	var summariesText string
	if len(chapters) > 0 {
		summariesText = buildChapterSummariesText(chapters, 5, 3000)
	} else {
		summariesText = novel.Description
	}
	if summariesText == "" {
		summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计主要物品道具。", novel.Genre, novel.Title)
	}

	tmplStr := loadPromptTemplate("extract_items.tmpl")
	tmpl, err := template.New("extract_items").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse extract_items.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle": novel.Title,
		"Genre":      novel.Genre,
		"Summaries":  summariesText,
	}); err != nil {
		return err
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "extract_items", buf.String(), "")
	if err != nil {
		return fmt.Errorf("AI extract_items: %w", err)
	}

	var items []analysisItemJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &items); err != nil {
		return fmt.Errorf("parse items JSON: %w", err)
	}

	// 去重（按名称）
	existing, _ := s.itemRepo.ListByNovel(novel.ID)
	existingNames := make(map[string]bool, len(existing))
	for _, e := range existing {
		existingNames[strings.ToLower(e.Name)] = true
	}

	const maxConcurrentImageGen = 3
	sem := make(chan struct{}, maxConcurrentImageGen)
	var wg sync.WaitGroup

	for _, it := range items {
		if it.Name == "" || existingNames[strings.ToLower(it.Name)] {
			continue
		}
		abilities := ""
		if len(it.Abilities) > 0 {
			if ab, err := json.Marshal(it.Abilities); err == nil {
				abilities = string(ab)
			}
		}
		cat := it.Category
		valid := map[string]bool{"weapon": true, "treasure": true, "tool": true, "document": true, "artifact": true, "other": true}
		if !valid[cat] {
			cat = "other"
		}
		item := &model.Item{
			NovelID:      novel.ID,
			UUID:         uuid.New().String(),
			Name:         it.Name,
			Category:     cat,
			Appearance:   it.Appearance,
			Location:     it.Location,
			Owner:        it.Owner,
			Significance: it.Significance,
			Abilities:    abilities,
			VisualPrompt: it.VisualPrompt,
			Status:       "active",
		}
		if err := s.itemRepo.Create(item); err != nil {
			log.Printf("NovelAnalysis: create item %q: %v", it.Name, err)
			continue
		}
		existingNames[strings.ToLower(it.Name)] = true
		// 有界并发生成图片（最多 maxConcurrentImageGen 个并发，等待全部完成）
		sem <- struct{}{}
		wg.Add(1)
		go func(i *model.Item) {
			defer func() {
				wg.Done()
				<-sem
			}()
			prompt := i.VisualPrompt
			if prompt == "" {
				prompt = fmt.Sprintf("%s, %s, fantasy item illustration, high detail", i.Name, i.Appearance)
			}
			url, err := s.aiService.GenerateCharacterThreeView(ctx, prompt+", item concept art, no background")
			if err != nil {
				log.Printf("NovelAnalysis: item image gen %q: %v", i.Name, err)
				return
			}
			if url != "" {
				i.ImageURL = url
				if err := s.itemRepo.Update(i); err != nil {
					log.Printf("NovelAnalysis: save item image %q: %v", i.Name, err)
				}
			}
		}(item)
	}
	wg.Wait()
	return nil
}
