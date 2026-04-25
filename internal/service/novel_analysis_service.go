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

// AnalysisTask 分析任务状态
type AnalysisTask struct {
	NovelID        uint   `json:"novel_id"`
	Status         string `json:"status"`   // pending / running / completed / failed
	Progress       int    `json:"progress"` // 0-100
	Step           string `json:"step"`
	Error          string `json:"error,omitempty"`
	CreateOutlines bool   `json:"-"` // 是否在分析完成后创建章节占位记录
}

// NovelAnalysisService 小说分析服务（异步 Pipeline）
type NovelAnalysisService struct {
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
	characterRepo *repository.CharacterRepository
	worldviewRepo *repository.WorldviewRepository
	itemRepo      *repository.ItemRepository
	novelService  *NovelService
	aiService     *AIService
	tasks         sync.Map // taskID(string) → *AnalysisTask
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
	}
	s.tasks.Store(taskID, task)

	go s.runPipeline(context.Background(), task, tenantID, novel)
	return taskID, nil
}

// GetStatus 查询任务状态
func (s *NovelAnalysisService) GetStatus(taskID string) (*AnalysisTask, error) {
	v, ok := s.tasks.Load(taskID)
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	return v.(*AnalysisTask), nil
}

// ──────────────────────────────────────────────
// Pipeline 内部实现
// ──────────────────────────────────────────────

func (s *NovelAnalysisService) runPipeline(ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel) {
	task.Status = "running"

	chapters, err := s.chapterRepo.ListByNovel(novel.ID)
	if err != nil {
		s.fail(task, "获取章节列表失败: "+err.Error())
		return
	}

	// Step 1: 章节摘要 (0→30) — 仅在有章节内容时执行
	if len(chapters) > 0 {
		task.Step = "正在生成章节摘要..."
		if err := s.stepSummarizeChapters(ctx, task, tenantID, novel, chapters); err != nil {
			log.Printf("NovelAnalysis[%d]: stepSummarizeChapters warn: %v", novel.ID, err)
		}
	}

	// Step 2: 提取/生成角色 (30→55)
	task.Progress = 30
	task.Step = "正在生成角色信息..."
	if err := s.stepExtractCharacters(ctx, task, tenantID, novel, chapters); err != nil {
		log.Printf("NovelAnalysis[%d]: stepExtractCharacters warn: %v", novel.ID, err)
	}

	// Step 2.5: 提取/生成物品 (55→62)
	task.Progress = 55
	task.Step = "正在提取物品信息..."
	if s.itemRepo != nil {
		if err := s.stepExtractItems(ctx, task, tenantID, novel, chapters); err != nil {
			log.Printf("NovelAnalysis[%d]: stepExtractItems warn: %v", novel.ID, err)
		}
	}

	// Step 3: 提取/生成世界观 (62→78)
	task.Progress = 62
	task.Step = "正在生成世界观..."
	if err := s.stepExtractWorldview(ctx, task, tenantID, novel, chapters); err != nil {
		log.Printf("NovelAnalysis[%d]: stepExtractWorldview warn: %v", novel.ID, err)
		task.Step = "世界观生成失败（已跳过）"
		task.Error = "世界观生成失败: " + err.Error()
	}

	// Step 4: 生成大纲 (75→95)
	task.Progress = 75
	task.Step = "正在生成大纲..."
	outline, err := s.stepGenerateOutline(ctx, task, tenantID, novel)
	if err != nil {
		log.Printf("NovelAnalysis[%d]: stepGenerateOutline warn: %v", novel.ID, err)
	}

	// Step 5: 收尾 (95→100)
	task.Progress = 95
	task.Step = "收尾中..."
	if err := s.stepFinalize(task, novel); err != nil {
		log.Printf("NovelAnalysis[%d]: stepFinalize warn: %v", novel.ID, err)
	}

	// Step 6 (可选): 创建章节占位记录
	if task.CreateOutlines && outline != nil && len(outline.Chapters) > 0 {
		task.Step = "正在创建章节大纲..."
		if err := s.stepCreateChapterOutlines(ctx, task, novel, outline); err != nil {
			log.Printf("NovelAnalysis[%d]: stepCreateChapterOutlines warn: %v", novel.ID, err)
		}
	}

	task.Progress = 100
	task.Step = "分析完成"
	task.Status = "completed"
	log.Printf("NovelAnalysis[%d]: pipeline completed", novel.ID)
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
		task.Progress = (i + 1) * 30 / total
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
		const maxChapters = 5
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
		summariesText = truncateForPrompt(sb.String(), 3000)
	} else {
		// AI 创建模式：无章节，基于小说描述生成角色
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

	type abilityJSON struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	type dialogueStyleJSON struct {
		VocabularyLevel string   `json:"vocabulary_level"`
		Patterns        []string `json:"patterns"`
		SpeechHabits    string   `json:"speech_habits"`
	}
	type charJSON struct {
		Name            string            `json:"name"`
		Role            string            `json:"role"`
		Archetype       string            `json:"archetype"`
		Appearance      string            `json:"appearance"`
		Personality     string            `json:"personality"`
		PersonalityTags []string          `json:"personality_tags"`
		Background      string            `json:"background"`
		CharacterArc    string            `json:"character_arc"`
		Abilities       []abilityJSON     `json:"abilities"`
		DialogueStyle   dialogueStyleJSON `json:"dialogue_style"`
		VisualPrompt    string            `json:"visual_prompt"`
	}
	var chars []charJSON
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
		// 更新进度 30→55
		task.Progress = 30 + len(existingNames)*25/len(chars)
	}

	// 异步生成三视图（不阻塞 pipeline）
	if len(createdChars) > 0 {
		go s.generateThreeViewsAsync(context.Background(), createdChars)
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

	type wvJSON struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		MagicSystem string `json:"magic_system"`
		Geography   string `json:"geography"`
		History     string `json:"history"`
		Culture     string `json:"culture"`
		Technology  string `json:"technology"`
		Rules       string `json:"rules"`
	}
	var wv wvJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &wv); err != nil {
		return fmt.Errorf("parse worldview JSON: %w", err)
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
	task.Status = "failed"
	task.Error = msg
	task.Step = "失败"
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

// stepExtractItems 从章节摘要/小说描述提取物品信息
func (s *NovelAnalysisService) stepExtractItems(
	ctx context.Context, task *AnalysisTask, tenantID uint, novel *model.Novel, chapters []*model.Chapter,
) error {
	var summariesText string
	if len(chapters) > 0 {
		const maxChapters = 5
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
		summariesText = truncateForPrompt(sb.String(), 3000)
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

	type abilityJSON struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	type itemJSON struct {
		Name         string        `json:"name"`
		Category     string        `json:"category"`
		Appearance   string        `json:"appearance"`
		Location     string        `json:"location"`
		Owner        string        `json:"owner"`
		Significance string        `json:"significance"`
		Abilities    []abilityJSON `json:"abilities"`
		VisualPrompt string        `json:"visual_prompt"`
	}
	var items []itemJSON
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
		// 异步生成图片（非阻塞）
		go func(i *model.Item) {
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
	return nil
}
