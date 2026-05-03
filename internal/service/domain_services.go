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
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ─── AI upsert helpers ───────────────────────────────────────────────────────

// fillIfEmpty returns (ai, true) when existing is blank and ai is non-blank;
// otherwise returns (existing, false). Used to avoid overwriting user-curated data.
func fillIfEmpty(existing, ai string) (string, bool) {
	if existing == "" && ai != "" {
		return ai, true
	}
	return existing, false
}

// collectContent joins chapter content up to maxChapters chapters and maxRunes runes total.
func collectContent(chapters []*model.Chapter, maxChapters, maxRunes int) string {
	var sb strings.Builder
	runeCount := 0
	for i, ch := range chapters {
		if i >= maxChapters || runeCount >= maxRunes {
			break
		}
		if ch.Content == "" {
			continue
		}
		runes := []rune(ch.Content)
		if runeCount > 0 {
			sb.WriteString("\n\n")
			runeCount += 2
		}
		remaining := maxRunes - runeCount
		if len(runes) > remaining {
			runes = runes[:remaining]
		}
		sb.WriteString(string(runes))
		runeCount += len(runes)
	}
	return sb.String()
}

// marshalExistingNames serialises a slice of items via transform and returns a compact JSON array string.
// Returns "" when items is empty.
func marshalExistingNames[T any](items []T, transform func(T) any) string {
	if len(items) == 0 {
		return ""
	}
	arr := make([]any, len(items))
	for i, it := range items {
		arr[i] = transform(it)
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return ""
	}
	return string(b)
}

// charNameEntry 阶段一提取的角色简要信息
type charNameEntry struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Brief string `json:"brief"`
}

// extractCharNamesFromContent 从单章内容中提取角色名单（纯 AI 提取，不操作 DB）
func (s *CharacterService) extractCharNamesFromContent(
	tenantID, novelID uint,
	novelTitle, genre, content string,
) ([]charNameEntry, error) {
	tmplStr := loadPromptTemplate("extract_character_names.tmpl")
	tmpl, err := template.New("extract_character_names").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"Summaries":     content,
		"ExistingNames": "",
	}); err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_character_names", buf.String(), "")
	if err != nil {
		return nil, err
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var entries []charNameEntry
	if err := json.Unmarshal([]byte(cleaned), &entries); err != nil {
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var e charNameEntry
				if dec.Decode(&e) == nil && e.Name != "" {
					entries = append(entries, e)
				}
			}
		}
	}
	valid := entries[:0]
	for _, e := range entries {
		if e.Name != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// extractCharacterNamesFromChapters Phase 1：逐章并发提取角色名单，合并去重
func (s *CharacterService) extractCharacterNamesFromChapters(
	tenantID, novelID uint,
	novelTitle, genre string,
	chapters []*model.Chapter,
) ([]charNameEntry, error) {
	const maxChapters = 10
	const concurrency = 3

	// 过滤有内容的章节（最多 maxChapters 章）
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" || ch.Summary != "" {
			candidates = append(candidates, ch)
			if len(candidates) >= maxChapters {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no chapter content available")
	}

	type chResult struct {
		entries []charNameEntry
		err     error
	}
	results := make([]chResult, len(candidates))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, ch := range candidates {
		wg.Add(1)
		go func(idx int, c *model.Chapter) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content := c.Content
			if content == "" {
				content = c.Summary
			}
			entries, err := s.extractCharNamesFromContent(tenantID, novelID, novelTitle, genre, content)
			results[idx] = chResult{entries, err}
		}(i, ch)
	}
	wg.Wait()

	// 合并去重（按小写名字，保留第一次出现）
	seen := make(map[string]bool)
	var merged []charNameEntry
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for _, e := range r.entries {
			key := strings.ToLower(e.Name)
			if !seen[key] {
				seen[key] = true
				merged = append(merged, e)
			}
		}
	}
	return merged, nil
}

// extractCharacterNameList 阶段一：从小说摘要中提取角色名单（输出极短，避免截断）
func (s *CharacterService) extractCharacterNameList(
	tenantID, novelID uint,
	novelTitle, genre, summariesText string,
	existing []*model.Character,
) ([]charNameEntry, error) {
	existingJSON := marshalExistingNames(existing, func(c *model.Character) any {
		return struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}{c.Name, c.Role}
	})

	tmplStr := loadPromptTemplate("extract_character_names.tmpl")
	tmpl, err := template.New("extract_character_names").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse extract_character_names.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"Summaries":     summariesText,
		"ExistingNames": existingJSON,
	}); err != nil {
		return nil, fmt.Errorf("render extract_character_names.tmpl: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_character_names", buf.String(), "")
	if err != nil {
		return nil, fmt.Errorf("AI call failed: %w", err)
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var entries []charNameEntry
	if err := json.Unmarshal([]byte(cleaned), &entries); err != nil {
		// 兜底：尝试用 Decoder 部分恢复
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var e charNameEntry
				if dec.Decode(&e) == nil && e.Name != "" {
					entries = append(entries, e)
				}
			}
		}
	}
	// 过滤掉名字为空的
	valid := entries[:0]
	for _, e := range entries {
		if e.Name != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// generateOneCharacterProfile 阶段二：为单个角色生成完整档案
func (s *CharacterService) generateOneCharacterProfile(
	tenantID, novelID uint,
	novelTitle, genre string,
	entry charNameEntry,
	shortSummaries string,
) (*analysisCharJSON, error) {
	tmplStr := loadPromptTemplate("generate_character_profile.tmpl")
	tmpl, err := template.New("generate_character_profile").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":     novelTitle,
		"Genre":          genre,
		"CharacterName":  entry.Name,
		"CharacterRole":  entry.Role,
		"CharacterBrief": entry.Brief,
		"Summaries":      shortSummaries,
	}); err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "generate_character_profile", buf.String(), "")
	if err != nil {
		return nil, fmt.Errorf("AI call: %w", err)
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var profile analysisCharJSON
	if err := json.Unmarshal([]byte(cleaned), &profile); err != nil {
		// 如果是包裹对象 {"character":{...}}，尝试解包
		var wrapper map[string]json.RawMessage
		if json.Unmarshal([]byte(cleaned), &wrapper) == nil {
			for _, v := range wrapper {
				if json.Unmarshal(v, &profile) == nil && profile.Name != "" {
					return &profile, nil
				}
			}
		}
		return nil, fmt.Errorf("parse profile JSON: %w", err)
	}
	if profile.Name == "" {
		profile.Name = entry.Name
	}
	if profile.Role == "" {
		profile.Role = entry.Role
	}
	return &profile, nil
}

// parseCharacterJSONResult 从 AI 响应中解析 []analysisCharJSON。
// 兼容以下几种常见输出形式：
//  1. 裸数组:        [{"name":"xxx",...}]
//  2. 被包裹的对象:  {"characters":[...]} / {"data":[...]} 等
//  3. 截断的 JSON:   输出被 token 上限截断，通过 json.Decoder 逐对象恢复
func parseCharacterJSONResult(raw string) ([]analysisCharJSON, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))
	var profiles []analysisCharJSON
	if err := json.Unmarshal([]byte(cleaned), &profiles); err == nil {
		return profiles, nil
	}
	// 如果直接解析失败，尝试从包裹对象中提取数组
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(cleaned), &wrapper) == nil {
		for _, v := range wrapper {
			if json.Unmarshal(v, &profiles) == nil {
				return profiles, nil
			}
		}
	}
	// 最后尝试部分恢复：用 json.Decoder 逐个解析，遇到截断就停止
	if partial := extractPartialCharacterObjects(raw); len(partial) > 0 {
		log.Printf("[parseCharacterJSONResult] partial recovery: got %d characters from truncated JSON", len(partial))
		return partial, nil
	}
	return nil, fmt.Errorf("cannot parse as character array: %.200s", raw)
}

// extractPartialCharacterObjects 从截断的 JSON 数组中尽量多地解析完整对象
func extractPartialCharacterObjects(raw string) []analysisCharJSON {
	start := strings.IndexByte(raw, '[')
	if start == -1 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw[start:]))
	if _, err := dec.Token(); err != nil { // consume '['
		return nil
	}
	var results []analysisCharJSON
	for dec.More() {
		var obj analysisCharJSON
		if err := dec.Decode(&obj); err != nil {
			break // truncated — stop here
		}
		if obj.Name != "" {
			results = append(results, obj)
		}
	}
	return results
}

// ============================================
// ChapterService 章节服务
// ============================================

type ChapterService struct {
	chapterRepo    *repository.ChapterRepository
	novelRepo      *repository.NovelRepository
	aiService      *AIService
	contextSvc     *GenerationContextService
	narrativeSvc   *NarrativeMemoryService // 层次化记忆 + 摘要 + 标题 + 精修
	hookSvc        *HookChainService
	spSvc          *SatisfactionPointService
	arcSvc         *ConflictArcService
	plotPointRepo  *repository.PlotPointRepository // 未解决剧情点注入
}

func NewChapterService(
	chapterRepo *repository.ChapterRepository,
	novelRepo *repository.NovelRepository,
	aiService *AIService,
	contextSvc *GenerationContextService,
) *ChapterService {
	return &ChapterService{
		chapterRepo: chapterRepo,
		novelRepo:   novelRepo,
		aiService:   aiService,
		contextSvc:  contextSvc,
	}
}

// WithNarrativeMemory 注入层次化记忆服务（可选）
func (s *ChapterService) WithNarrativeMemory(svc *NarrativeMemoryService) *ChapterService {
	s.narrativeSvc = svc
	return s
}

// WithPlotPointRepo 注入剧情点仓库（可选），用于将未解决的伏笔/冲突注入生成 prompt
func (s *ChapterService) WithPlotPointRepo(repo *repository.PlotPointRepository) *ChapterService {
	s.plotPointRepo = repo
	return s
}

// WithDramaticServices 注入戏剧张力服务（可选）
func (s *ChapterService) WithDramaticServices(hookSvc *HookChainService, spSvc *SatisfactionPointService, arcSvc *ConflictArcService) *ChapterService {
	s.hookSvc = hookSvc
	s.spSvc = spSvc
	s.arcSvc = arcSvc
	return s
}

// GetDefaultProviderName 返回默认 AI provider 名称
func (s *ChapterService) GetDefaultProviderName() string {
	return s.aiService.GetDefaultProviderName()
}

// syncNovelStats refreshes chapter_count and total_words on the novel (best-effort).
func (s *ChapterService) syncNovelStats(novelID uint) {
	_ = s.novelRepo.SyncStats(novelID)
}

func (s *ChapterService) CreateChapter(novelID uint, req *model.CreateChapterRequest) (*model.Chapter, error) {
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		NovelID:   novelID,
		ChapterNo: req.ChapterNo,
		Title:     req.Title,
		Content:   req.Content,
		WordCount: countChineseChars(req.Content),
		Status:    "completed",
	}
	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}
	s.syncNovelStats(novelID)
	return chapter, nil
}

func (s *ChapterService) GetChapter(id uint) (*model.Chapter, error) {
	return s.chapterRepo.GetByID(id)
}

func (s *ChapterService) ListChapters(novelID uint) ([]*model.Chapter, error) {
	return s.chapterRepo.ListByNovel(novelID)
}

// applyChapterUpdate patches non-zero request fields onto a chapter in place.
func applyChapterUpdate(chapter *model.Chapter, req *model.UpdateChapterRequest) {
	if req.Title != "" {
		chapter.Title = req.Title
	}
	if req.Content != "" {
		chapter.Content = req.Content
		chapter.WordCount = countChineseChars(req.Content)
	}
	if req.ChapterHook != "" {
		chapter.ChapterHook = req.ChapterHook
	}
	if req.Outline != "" {
		chapter.Outline = req.Outline
	}
}

func (s *ChapterService) UpdateChapter(id uint, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	applyChapterUpdate(chapter, req)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	if req.Content != "" {
		s.syncNovelStats(chapter.NovelID)
	}
	return chapter, nil
}

func (s *ChapterService) DeleteChapter(id uint) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return s.chapterRepo.Delete(id, 0)
	}
	if err := s.chapterRepo.Delete(id, chapter.NovelID); err != nil {
		return err
	}
	s.syncNovelStats(chapter.NovelID)
	return nil
}

func (s *ChapterService) GetChapterByNo(novelID uint, chapterNo int) (*model.Chapter, error) {
	return s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
}

func (s *ChapterService) UpdateChapterByNo(novelID uint, chapterNo int, req *model.UpdateChapterRequest) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}
	applyChapterUpdate(chapter, req)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	if req.Content != "" {
		s.syncNovelStats(novelID)
	}
	return chapter, nil
}

// GenerateChapterOutline 用 AI 为指定章节生成大纲（概述性文字，非场景 JSON）
func (s *ChapterService) GenerateChapterOutline(tenantID, novelID uint, chapterNo int, extraPrompt string) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return nil, err
	}

	// 构建上下文：近期章节摘要
	var recentCtx string
	if s.narrativeSvc != nil {
		if ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo); err == nil {
			recentCtx = ctx
		}
	}

	recentCtxSection := ""
	if recentCtx != "" {
		recentCtxSection = "叙事上下文：\n" + recentCtx
	}
	extraPromptSection := ""
	if extraPrompt != "" {
		extraPromptSection = "补充要求：" + extraPrompt
	}

	prompt := fmt.Sprintf(`请为小说《%s》第%d章生成一段简洁的章节大纲（200字以内）。

小说简介：%s
章节标题：%s
%s
%s

要求：
- 概述本章的核心情节和转折
- 点明主要人物行动与目标
- 语言简练，不超过200字
- 直接输出大纲文本，不要加前缀或说明`,
		novel.Title, chapterNo, novel.Description, chapter.Title,
		recentCtxSection, extraPromptSection,
	)

	outline, err := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter_outline", prompt, "")
	if err != nil {
		return nil, err
	}

	chapter.Outline = strings.TrimSpace(outline)
	if err := s.chapterRepo.Update(chapter); err != nil {
		return nil, err
	}
	return chapter, nil
}

func (s *ChapterService) DeleteChapterByNo(novelID uint, chapterNo int) error {
	chapter, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo)
	if err != nil {
		return err
	}
	if err := s.chapterRepo.Delete(chapter.ID, novelID); err != nil {
		return err
	}
	s.syncNovelStats(novelID)
	return nil
}

// GenerateChapter 专业级章节生成流水线：
//
//  Step 1  构建层次化上下文（近章详摘 + 弧摘要 + 全局概述）
//  Step 2  生成场景大纲（3-5 个场景，含节拍、情绪、钩子）
//  Step 3  按场景大纲生成完整章节内容
//  Step 4  存储章节（包含场景大纲、叙事元数据）
//  Step 5  异步后处理：摘要生成、标题生成、精修、角色状态提取、弧摘要触发
func (s *ChapterService) GenerateChapter(tenantID uint, novelID uint, req *model.GenerateChapterRequest) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// ── Step 1: 层次化上下文 ──────────────────────────────
	globalCtx := s.buildGlobalContext(novelID, req.ChapterNo, novel)

	// 自动检测最终章：当前章节号达到小说目标章节数时，确保故事完整收尾
	// 用户也可通过 is_standalone=true 显式触发（如临时想提前收尾）
	if !req.IsStandalone && novel.TargetChapters > 0 && req.ChapterNo >= novel.TargetChapters {
		req.IsStandalone = true
	}

	// 从小说大纲获取本章元数据（张力值、幕次、情感基调等）
	chapterMeta := s.extractChapterMeta(novelID, req.ChapterNo)

	// ── Step 2: 生成场景大纲 ──────────────────────────────
	sceneOutlineJSON, suggestedTitle := s.generateSceneOutline(
		tenantID, novelID, req, novel, globalCtx, chapterMeta,
	)

	// ── Step 3: 按场景大纲生成章节内容 ───────────────────
	content, chapterHook, err := s.generateFromSceneOutline(
		tenantID, novelID, req, novel, sceneOutlineJSON, globalCtx, chapterMeta,
	)
	if err != nil {
		return nil, err
	}

	// ── Step 4: 存储章节 (upsert: update if placeholder exists) ──────────────
	title := suggestedTitle
	if title == "" {
		title = fmt.Sprintf("第%d章", req.ChapterNo)
	}
	var chapter *model.Chapter
	if existing, _ := s.chapterRepo.GetByNovelAndChapterNo(novelID, req.ChapterNo); existing != nil {
		existing.Title = title
		existing.Content = content
		existing.WordCount = countChineseChars(content)
		existing.SceneOutline = sceneOutlineJSON
		existing.TensionLevel = chapterMeta.tensionLevel
		existing.ActNo = chapterMeta.actNo
		existing.EmotionalTone = chapterMeta.emotionalTone
		existing.HookType = chapterMeta.hookType
		existing.ChapterHook = chapterHook
		existing.Status = "completed"
		if err := s.chapterRepo.Update(existing); err != nil {
			return nil, err
		}
		chapter = existing
	} else {
		chapter = &model.Chapter{
			UUID:          uuid.New().String(),
			NovelID:       novelID,
			ChapterNo:     req.ChapterNo,
			Title:         title,
			Content:       content,
			WordCount:     countChineseChars(content),
			SceneOutline:  sceneOutlineJSON,
			TensionLevel:  chapterMeta.tensionLevel,
			ActNo:         chapterMeta.actNo,
			EmotionalTone: chapterMeta.emotionalTone,
			HookType:      chapterMeta.hookType,
			ChapterHook:   chapterHook,
			Status:        "completed",
		}
		if err := s.chapterRepo.Create(chapter); err != nil {
			return nil, err
		}
	}

	s.syncNovelStats(novelID)

	// ── Step 5: 异步后处理 ───────────────────────────────
	go s.postProcessChapter(tenantID, chapter, novel)

	return chapter, nil
}

// chapterOutlineMeta 从小说大纲中提取的章节叙事元数据
type chapterOutlineMeta struct {
	tensionLevel  int
	actNo         int
	emotionalTone string
	hookType      string
	summary       string // 大纲中的章节概述
}

func (s *ChapterService) extractChapterMeta(novelID uint, chapterNo int) chapterOutlineMeta {
	// 尝试从小说 Outline 字段中解析章节元数据
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return chapterOutlineMeta{}
	}
	if novel.StylePrompt == "" {
		return chapterOutlineMeta{}
	}
	// 解析存储在 StylePrompt 中的大纲 JSON（大纲生成后存储在此）
	var outline struct {
		Chapters []struct {
			ChapterNo     int    `json:"chapter_no"`
			TensionLevel  int    `json:"tension_level"`
			Act           int    `json:"act"`
			EmotionalTone string `json:"emotional_tone"`
			HookType      string `json:"hook_type"`
			Summary       string `json:"summary"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(novel.StylePrompt), &outline); err != nil {
		return chapterOutlineMeta{}
	}
	for _, ch := range outline.Chapters {
		if ch.ChapterNo == chapterNo {
			return chapterOutlineMeta{
				tensionLevel:  ch.TensionLevel,
				actNo:         ch.Act,
				emotionalTone: ch.EmotionalTone,
				hookType:      ch.HookType,
				summary:       ch.Summary,
			}
		}
	}
	return chapterOutlineMeta{}
}

// buildGlobalContext 构建层次化全局上下文（优先使用 NarrativeMemoryService）
func (s *ChapterService) buildGlobalContext(novelID uint, chapterNo int, novel *model.Novel) string {
	// 优先使用层次化记忆
	if s.narrativeSvc != nil {
		ctx, err := s.narrativeSvc.BuildHierarchicalContext(novelID, chapterNo)
		if err == nil && ctx != "" {
			return ctx
		}
		log.Printf("GenerateChapter: hierarchical context failed: %v — fallback", err)
	}
	// 降级到原 GenerationContextService
	if s.contextSvc != nil {
		ctx, err := s.contextSvc.BuildGenerationPrompt(novelID, chapterNo, "", "", 8000)
		if err == nil {
			return ctx
		}
	}
	// 最终降级
	return fmt.Sprintf("【故事概要】\n%s", novel.Description)
}

// generateSceneOutline 调用 AI 生成场景级大纲，返回 JSON 字符串和建议标题
func (s *ChapterService) generateSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	globalCtx string,
	meta chapterOutlineMeta,
) (sceneOutlineJSON, suggestedTitle string) {

	// 获取角色状态
	charStateStr := s.buildCharacterStateString(novelID)

	// 构建伏笔提示
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	// 获取上一章结尾（供连贯性参考）
	prevEnding := s.getPreviousChapterEnding(novelID, req.ChapterNo)

	// 获取角色列表
	characters := s.getCharactersForPrompt(novelID)

	// 获取剧情张力状态（供场景大纲决策参考）
	plotTensionState := ""
	if s.narrativeSvc != nil {
		plotTensionState = s.narrativeSvc.BuildPlotTensionStateText(novelID, req.ChapterNo)
	}
	// 注入戏剧上下文（钩子链、爽点、冲突弧）
	if s.hookSvc != nil {
		if ctx := s.hookSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}
	if s.spSvc != nil {
		if ctx := s.spSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}
	if s.arcSvc != nil {
		if ctx := s.arcSvc.GetInjectionContext(novelID, req.ChapterNo); ctx != "" {
			if plotTensionState != "" {
				plotTensionState += "\n\n"
			}
			plotTensionState += ctx
		}
	}

	hookType := meta.hookType
	if hookType == "" {
		if req.IsStandalone {
			hookType = "大结局" // 独立故事：圆满/震撼收尾，不留悬念
		} else {
			hookType = "cliffhanger"
		}
	}
	emotionalTone := meta.emotionalTone
	if emotionalTone == "" {
		emotionalTone = "紧张"
	}
	tensionLevel := meta.tensionLevel
	if tensionLevel == 0 {
		tensionLevel = 6
	}
	actNo := meta.actNo
	if actNo == 0 {
		actNo = 1
	}
	chapterSummary := meta.summary
	if chapterSummary == "" && req.Prompt != "" {
		chapterSummary = req.Prompt
	}

	tmplStr := loadPromptTemplate("chapter_scene_outline.tmpl")
	tmpl, err := template.New("scene_outline").Parse(tmplStr)
	if err != nil {
		log.Printf("GenerateChapter: parse scene_outline.tmpl: %v", err)
		return "", ""
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":            novel.Title,
		"ChapterNo":             req.ChapterNo,
		"GlobalContext":         globalCtx,
		"ChapterSummary":        chapterSummary,
		"TensionLevel":          tensionLevel,
		"ActNo":                 actNo,
		"EmotionalTone":         emotionalTone,
		"HookType":              hookType,
		"IsStandalone":          req.IsStandalone,
		"PreviousChapterEnding": prevEnding,
		"Characters":            characters,
		"ForeshadowHints":       foreshadowHints,
		"CharacterStates":       charStateStr,
		"PlotTensionState":      plotTensionState,
	}); err != nil {
		log.Printf("GenerateChapter: execute scene_outline.tmpl: %v", err)
		return "", ""
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "scene_outline", buf.String(), req.ModelOverride)
	if err != nil {
		log.Printf("GenerateChapter: scene outline AI call failed: %v", err)
		return "", ""
	}

	resp = extractJSON(strings.TrimSpace(resp))

	// 提取建议标题
	var outlineResult struct {
		ChapterTitle string `json:"chapter_title"`
	}
	if err := json.Unmarshal([]byte(resp), &outlineResult); err == nil {
		suggestedTitle = outlineResult.ChapterTitle
	}

	return resp, suggestedTitle
}

// generateFromSceneOutline 根据场景大纲生成章节正文
// 返回 (正文内容, 章末钩子, error)
// AI 输出中「【章末钩子】」标记后的内容会被提取为独立钩子字段
func (s *ChapterService) generateFromSceneOutline(
	tenantID, novelID uint,
	req *model.GenerateChapterRequest,
	novel *model.Novel,
	sceneOutlineJSON string,
	globalCtx string,
	meta chapterOutlineMeta,
) (string, string, error) {

	// MaxTokens 约等于字数（中文约1token/字）；优先用请求参数，其次小说项目配置，最后默认3000字
	wordCount := req.MaxTokens
	if wordCount <= 0 {
		wordCount = novel.MaxTokens
	}
	if wordCount <= 0 {
		wordCount = 3000
	}

	// 解析场景大纲以注入模板
	var outlineData struct {
		ChapterTitle string `json:"chapter_title"`
		HookSetup    string `json:"hook_setup"`
		ChapterArc   string `json:"chapter_arc"`
		Scenes       []struct {
			SceneNo       int      `json:"scene_no"`
			Location      string   `json:"location"`
			TimeOfDay     string   `json:"time_of_day"`
			Characters    []string `json:"characters"`
			Goal          string   `json:"goal"`
			OpeningBeat   string   `json:"opening_beat"`
			KeyBeats      []string `json:"key_beats"`
			ClosingBeat   string   `json:"closing_beat"`
			EmotionalShift string  `json:"emotional_shift"`
			POVCharacter  string   `json:"pov_character"`
			Tension       int      `json:"tension"`
		} `json:"scenes"`
	}
	_ = json.Unmarshal([]byte(sceneOutlineJSON), &outlineData)

	// 获取角色对话风格
	characterVoices := s.getCharacterVoices(novelID)

	// 未解决剧情线（伏笔/冲突）
	foreshadowHints := s.buildForeshadowHints(novelID, req.ChapterNo)

	// 峰值张力
	peakTension := 0
	for _, sc := range outlineData.Scenes {
		if sc.Tension > peakTension {
			peakTension = sc.Tension
		}
	}

	chapterTitle := outlineData.ChapterTitle
	if chapterTitle == "" {
		chapterTitle = fmt.Sprintf("第%d章", req.ChapterNo)
	}

	// 如果没有场景大纲，降级到简单 prompt
	if sceneOutlineJSON == "" || len(outlineData.Scenes) == 0 {
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}

	tmplStr := loadPromptTemplate("chapter_from_outline.tmpl")
	tmpl, err := template.New("chapter_from_outline").Parse(tmplStr)
	if err != nil {
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novel.Title,
		"ChapterNo":     req.ChapterNo,
		"ChapterTitle":  chapterTitle,
		"WordCount":     wordCount,
		"GlobalContext": globalCtx,
		"Scenes":        outlineData.Scenes,
		"HookSetup":     outlineData.HookSetup,
		"PeakTension":   peakTension,
		"Characters":      characterVoices,
		"ForeshadowHints": foreshadowHints,
		"UserPrompt":      req.Prompt,
		"IsStandalone":    req.IsStandalone,
	}); err != nil {
		content, err := s.generateFallbackChapter(tenantID, novelID, req, novel, globalCtx)
		return content, "", err
	}

	raw, err := s.aiService.GenerateWithProvider(tenantID, novelID, "chapter", buf.String(), req.ModelOverride)
	if err != nil {
		return "", "", err
	}
	content, hook := extractChapterHook(raw)
	return content, hook, nil
}

// generateFallbackChapter 场景大纲失败时的降级生成
func (s *ChapterService) generateFallbackChapter(tenantID, novelID uint, req *model.GenerateChapterRequest, novel *model.Novel, globalCtx string) (string, error) {
	log.Printf("GenerateChapter: using fallback (no scene outline) for novel %d ch %d", novelID, req.ChapterNo)
	wc := req.MaxTokens
	if wc <= 0 {
		wc = novel.MaxTokens
	}
	if wc <= 0 {
		wc = 3000
	}
	prompt := globalCtx + fmt.Sprintf("\n\n请为小说《%s》生成第%d章内容，字数约%d字。", novel.Title, req.ChapterNo, wc)
	if req.Prompt != "" {
		prompt += "\n\n创作要求：" + req.Prompt
	}
	return s.aiService.GenerateWithProvider(tenantID, novelID, "chapter", prompt, req.ModelOverride)
}

// postProcessChapter 异步后处理：生成摘要→生成标题→精修→提取角色状态→触发弧摘要
func (s *ChapterService) postProcessChapter(tenantID uint, chapter *model.Chapter, novel *model.Novel) {
	// 1. 生成摘要（最重要：供后续章节的上下文使用）
	if s.narrativeSvc != nil && chapter.Summary == "" {
		if summary, err := s.narrativeSvc.GenerateChapterSummary(tenantID, chapter, novel.Title); err == nil {
			chapter.Summary = summary
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [摘要]: %v", chapter.ID, updateErr)
			}
		} else {
			log.Printf("postProcess: summary ch%d: %v", chapter.ChapterNo, err)
		}
	}

	// 2. 如果标题仍是"第N章"，生成创意标题
	defaultTitle := fmt.Sprintf("第%d章", chapter.ChapterNo)
	if s.narrativeSvc != nil && chapter.Title == defaultTitle && chapter.Summary != "" {
		if title, err := s.narrativeSvc.GenerateChapterTitle(tenantID, chapter, novel.Genre, chapter.EmotionalTone); err == nil && title != "" {
			chapter.Title = fmt.Sprintf("第%d章 %s", chapter.ChapterNo, title)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [标题]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 3. 精修（检测并修复重复词、AI惯用句等）
	if s.narrativeSvc != nil {
		if refined, err := s.narrativeSvc.RefineChapterContent(tenantID, chapter, novel.Title); err == nil && refined != chapter.Content {
			chapter.Content = refined
			chapter.WordCount = countChineseChars(refined)
			if updateErr := s.chapterRepo.Update(chapter); updateErr != nil {
				log.Printf("postProcessChapter: update chapter %d [精修]: %v", chapter.ID, updateErr)
			}
		}
	}

	// 4. 提取角色状态快照（原有逻辑）
	novelSvc := &NovelService{novelRepo: s.novelRepo, chapterRepo: s.chapterRepo, aiService: s.aiService}
	novelSvc.writeCharacterSnapshots(chapter)

	// 5. 触发弧摘要（每 arcSize 章触发一次）
	if s.narrativeSvc != nil {
		s.narrativeSvc.TriggerArcSummaryIfNeeded(tenantID, novel.ID, chapter.ChapterNo)
	}

	// 6. 自动检查并标记已解决的剧情点（伏笔/冲突）
	s.checkAndAutoResolvePlotPoints(tenantID, chapter)
}

// checkAndAutoResolvePlotPoints 用单次 AI 调用判断本章是否解决了悬而未决的剧情线，自动更新 is_resolved
func (s *ChapterService) checkAndAutoResolvePlotPoints(tenantID uint, chapter *model.Chapter) {
	if s.plotPointRepo == nil || chapter.Content == "" {
		return
	}
	pps, err := s.plotPointRepo.ListByNovel(chapter.NovelID, "", true) // unresolved only
	if err != nil || len(pps) == 0 {
		return
	}
	// 最多取前5条 foreshadow/conflict/twist 进行检查
	var relevant []*model.PlotPoint
	for _, pp := range pps {
		if pp.Type == "foreshadow" || pp.Type == "conflict" || pp.Type == "twist" {
			relevant = append(relevant, pp)
		}
		if len(relevant) >= 5 {
			break
		}
	}
	if len(relevant) == 0 {
		return
	}

	// 构建精简 prompt
	var sb strings.Builder
	sb.WriteString("请分析以下章节内容摘录，判断哪些剧情线在本章中已明确解决（不再是悬念或未完结冲突）：\n\n")
	sb.WriteString("【章节内容摘录】\n")
	excerpt := []rune(chapter.Content)
	if len(excerpt) > 2000 {
		excerpt = excerpt[:2000]
	}
	sb.WriteString(string(excerpt))
	sb.WriteString("\n\n【待检查的剧情线】\n")
	for i, pp := range relevant {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, pp.Type, pp.Description))
	}
	sb.WriteString("\n只返回在本章中明确解决的序号，以JSON格式：{\"resolved_indices\":[1,3]}\n")
	sb.WriteString("若全部未解决则返回 {\"resolved_indices\":[]}")

	result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "plot_resolution_check", sb.String(), "")
	if err != nil {
		log.Printf("checkAndAutoResolvePlotPoints[%d]: AI error: %v", chapter.NovelID, err)
		return
	}

	var resp struct {
		ResolvedIndices []int `json:"resolved_indices"`
	}
	if err := json.Unmarshal([]byte(extractJSON(strings.TrimSpace(result))), &resp); err != nil {
		return
	}
	for _, idx := range resp.ResolvedIndices {
		if idx < 1 || idx > len(relevant) {
			continue
		}
		pp := relevant[idx-1]
		pp.IsResolved = true
		pp.ResolvedIn = &chapter.ID
		if err := s.plotPointRepo.Update(pp); err != nil {
			log.Printf("checkAndAutoResolvePlotPoints: update pp#%d: %v", pp.ID, err)
		} else {
			desc := pp.Description
			if len([]rune(desc)) > 40 {
				desc = string([]rune(desc)[:40]) + "…"
			}
			log.Printf("postProcess[novel=%d ch=%d]: auto-resolved plot point #%d [%s]: %s",
				chapter.NovelID, chapter.ChapterNo, pp.ID, pp.Type, desc)
		}
	}
}

// ──────────────────────────────────────────────
// Context helpers for GenerateChapter
// ──────────────────────────────────────────────

type characterForPrompt struct {
	Name         string
	Role         string
	CurrentState string
	DialogueStyle string
}

func (s *ChapterService) getCharactersForPrompt(novelID uint) []characterForPrompt {
	// ChapterService 没有直接访问 charRepo，通过 novelSvc 的快照机制获取
	// 这里返回空列表，实际角色信息通过 globalCtx 已包含
	return nil
}

func (s *ChapterService) getCharacterVoices(novelID uint) []characterForPrompt {
	return nil
}

func (s *ChapterService) buildCharacterStateString(novelID uint) string {
	return ""
}

func (s *ChapterService) buildForeshadowHints(novelID uint, chapterNo int) string {
	var hints strings.Builder
	count := 0

	// 来源1：旧伏笔系统（ForeshadowService）
	if s.contextSvc != nil && s.contextSvc.foreshadowSvc != nil {
		foreshadows, err := s.contextSvc.foreshadowSvc.CheckForeshadowStatus(novelID, chapterNo)
		if err == nil {
			for _, fs := range foreshadows {
				if !fs.IsFulfilled && chapterNo-fs.ChapterNo >= 3 {
					hints.WriteString(fmt.Sprintf("- 请考虑回收伏笔：「%s」（第%d章埋设）\n", fs.Description, fs.ChapterNo))
					count++
					if count >= 3 {
						break
					}
				}
			}
		}
	}

	// 来源2：PlotPoint 表中未解决的伏笔与冲突（最多补充至5条）
	if s.plotPointRepo != nil && count < 5 {
		pps, err := s.plotPointRepo.ListByNovel(novelID, "", true) // unresolved only
		if err == nil {
			for _, pp := range pps {
				if count >= 5 {
					break
				}
				switch pp.Type {
				case "foreshadow":
					hints.WriteString(fmt.Sprintf("- 未回收伏笔：「%s」\n", pp.Description))
					count++
				case "conflict":
					hints.WriteString(fmt.Sprintf("- 进行中的冲突：「%s」\n", pp.Description))
					count++
				}
			}
		}
	}

	return hints.String()
}

func (s *ChapterService) getPreviousChapterEnding(novelID uint, chapterNo int) string {
	if chapterNo <= 1 {
		return "（本章为开篇）"
	}
	prev, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chapterNo-1)
	if err != nil || prev == nil {
		return ""
	}
	// 优先使用独立保存的章末钩子
	if prev.ChapterHook != "" {
		return prev.ChapterHook
	}
	if prev.Summary != "" {
		return prev.Summary
	}
	// 从内容末尾截取约200字
	content := []rune(prev.Content)
	if len(content) > 200 {
		content = content[len(content)-200:]
	}
	return string(content)
}

// extractChapterHook 从 AI 生成的原始内容中分离正文与章末钩子。
// AI 应在正文后输出「【章末钩子】」标记，之后的内容即为钩子正文。
func extractChapterHook(raw string) (content, hook string) {
	const marker = "【章末钩子】"
	idx := strings.Index(raw, marker)
	if idx < 0 {
		return strings.TrimSpace(raw), ""
	}
	content = strings.TrimSpace(raw[:idx])
	hook = strings.TrimSpace(raw[idx+len(marker):])
	return
}


func (s *ChapterService) RegenerateChapter(tenantID uint, id uint, prompt string) (*model.Chapter, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	content, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter", prompt, "")
	if err != nil {
		return nil, err
	}
	chapter.Content = content
	chapter.WordCount = countChineseChars(content)
	return chapter, s.chapterRepo.Update(chapter)
}

func (s *ChapterService) ApproveChapter(id uint, comment string) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return err
	}
	chapter.Status = "approved"
	return s.chapterRepo.Update(chapter)
}

func (s *ChapterService) RejectChapter(id uint, reason string) error {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return err
	}
	chapter.Status = "rejected"
	return s.chapterRepo.Update(chapter)
}

// ============================================
// ChapterVersionService 章节版本服务
// ============================================

type ChapterVersionService struct {
	versionRepo *repository.ChapterVersionRepository
	chapterRepo *repository.ChapterRepository
}

func NewChapterVersionService(
	versionRepo *repository.ChapterVersionRepository,
	chapterRepo *repository.ChapterRepository,
) *ChapterVersionService {
	return &ChapterVersionService{
		versionRepo: versionRepo,
		chapterRepo: chapterRepo,
	}
}

func (s *ChapterVersionService) GetVersions(chapterID uint) ([]*model.ChapterVersion, error) {
	return s.versionRepo.List(chapterID)
}

func (s *ChapterVersionService) RestoreVersion(chapterID uint, versionNo int) (*model.Chapter, error) {
	version, err := s.versionRepo.GetVersion(chapterID, versionNo)
	if err != nil {
		return nil, err
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, err
	}
	chapter.Content = version.Content
	return chapter, s.chapterRepo.Update(chapter)
}

// ============================================
// CharacterService 角色服务
// ============================================

// EffectiveCharacter 有效角色（合并项目级与章节级覆盖）
type EffectiveCharacter struct {
	model.Character
	ChapterOverride      *model.ChapterCharacter `json:"chapter_override,omitempty"`
	EffectiveAppearance  string                  `json:"effective_appearance"`
	EffectivePersonality string                  `json:"effective_personality"`
	EffectiveStatus      string                  `json:"effective_status"`
	EffectiveLocation    string                  `json:"effective_location"`
}

type CharacterService struct {
	characterRepo        *repository.CharacterRepository
	chapterCharacterRepo *repository.ChapterCharacterRepository
	aiService            *AIService
	novelRepo            *repository.NovelRepository  // optional, for AIBatchGenerate
	chapterRepo          *repository.ChapterRepository // optional, for AIBatchGenerate
}

func NewCharacterService(
	characterRepo *repository.CharacterRepository,
	aiService *AIService,
) *CharacterService {
	return &CharacterService{
		characterRepo: characterRepo,
		aiService:     aiService,
	}
}

// WithChapterCharacterRepo 注入章节角色覆盖仓库（可选）
func (s *CharacterService) WithChapterCharacterRepo(r *repository.ChapterCharacterRepository) *CharacterService {
	s.chapterCharacterRepo = r
	return s
}

func (s *CharacterService) WithNovelRepo(r *repository.NovelRepository) *CharacterService {
	s.novelRepo = r
	return s
}

func (s *CharacterService) WithChapterRepo(r *repository.ChapterRepository) *CharacterService {
	s.chapterRepo = r
	return s
}

func (s *CharacterService) CreateCharacter(novelID uint, req *model.CreateCharacterRequest) (*model.Character, error) {
	character := &model.Character{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		Name:        req.Name,
		Gender:      req.Gender,
		Role:        req.Role,
		Archetype:   req.Archetype,
		Background:  req.Background,
		Appearance:  req.Appearance,
		Personality: req.Personality,
		Status:      "active",
	}
	return character, s.characterRepo.Create(character)
}

func (s *CharacterService) GetCharacter(id uint) (*model.Character, error) {
	return s.characterRepo.GetByID(id)
}

func (s *CharacterService) ListCharacters(novelID uint) ([]*model.Character, error) {
	return s.characterRepo.ListByNovel(novelID)
}

func (s *CharacterService) UpdateCharacter(id uint, req *model.UpdateCharacterRequest) (*model.Character, error) {
	character, err := s.characterRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		character.Name = req.Name
	}
	if req.Gender != "" {
		character.Gender = req.Gender
	}
	if req.Role != "" {
		character.Role = req.Role
	}
	if req.Archetype != "" {
		character.Archetype = req.Archetype
	}
	if req.Background != "" {
		character.Background = req.Background
	}
	if req.Appearance != "" {
		character.Appearance = req.Appearance
	}
	if req.Personality != "" {
		character.Personality = req.Personality
	}
	if req.CharacterArc != "" {
		character.CharacterArc = req.CharacterArc
	}
	// PersonalityTags: nil = absent (skip), non-nil (including empty) = update
	if req.PersonalityTags != nil {
		if b, err := json.Marshal(req.PersonalityTags); err == nil {
			character.PersonalityTags = string(b)
		}
	}
	// Abilities: nil = absent (skip), non-nil = update
	if req.Abilities != nil {
		if b, err := json.Marshal(req.Abilities); err == nil {
			character.Abilities = string(b)
		}
	}
	if req.ThreeViewFront != "" {
		character.ThreeViewFront = req.ThreeViewFront
	}
	if req.ThreeViewSide != "" {
		character.ThreeViewSide = req.ThreeViewSide
	}
	if req.ThreeViewBack != "" {
		character.ThreeViewBack = req.ThreeViewBack
	}
	if req.Portrait != "" {
		character.Portrait = req.Portrait
	}
	if req.CoverImage != "" {
		character.CoverImage = req.CoverImage
	}
	if req.VoiceID != "" {
		character.VoiceID = req.VoiceID
	}
	if req.VoiceSpeed != nil {
		character.VoiceSpeed = *req.VoiceSpeed
	}
	if req.VoiceStyle != "" {
		character.VoiceStyle = req.VoiceStyle
	}
	if req.VoiceLanguage != "" {
		character.VoiceLanguage = req.VoiceLanguage
	}
	if req.VoiceSample != "" {
		character.VoiceSample = req.VoiceSample
	}
	return character, s.characterRepo.Update(character)
}

func (s *CharacterService) DeleteCharacter(id uint) error {
	return s.characterRepo.Delete(id)
}

// ListEffectiveCharacters 获取章节的有效角色列表（章节级覆盖优先）
func (s *CharacterService) ListEffectiveCharacters(novelID, chapterID uint) ([]*EffectiveCharacter, error) {
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	overrideMap := make(map[uint]*model.ChapterCharacter)
	if s.chapterCharacterRepo != nil {
		overrides, _ := s.chapterCharacterRepo.ListByChapter(chapterID)
		for _, o := range overrides {
			overrideMap[o.CharacterID] = o
		}
	}
	result := make([]*EffectiveCharacter, 0, len(chars))
	for _, ch := range chars {
		ec := &EffectiveCharacter{Character: *ch}
		if o, ok := overrideMap[ch.ID]; ok {
			ec.ChapterOverride = o
			if o.Appearance != "" {
				ec.EffectiveAppearance = o.Appearance
			} else {
				ec.EffectiveAppearance = ch.Appearance
			}
			if o.Personality != "" {
				ec.EffectivePersonality = o.Personality
			} else {
				ec.EffectivePersonality = ch.Personality
			}
			if o.Status != "" {
				ec.EffectiveStatus = o.Status
			} else {
				ec.EffectiveStatus = ch.Status
			}
			ec.EffectiveLocation = o.Location
		} else {
			ec.EffectiveAppearance = ch.Appearance
			ec.EffectivePersonality = ch.Personality
			ec.EffectiveStatus = ch.Status
		}
		result = append(result, ec)
	}
	return result, nil
}

// UpsertChapterCharacter 创建或更新章节级角色覆盖
func (s *CharacterService) UpsertChapterCharacter(novelID, chapterID, characterID uint, req *model.UpsertChapterCharacterRequest) (*model.ChapterCharacter, error) {
	if s.chapterCharacterRepo == nil {
		return nil, fmt.Errorf("chapter character repository not configured")
	}
	cc := &model.ChapterCharacter{
		CharacterID: characterID,
		ChapterID:   chapterID,
		NovelID:     novelID,
		Appearance:  req.Appearance,
		Personality: req.Personality,
		Status:      req.Status,
		Location:    req.Location,
		Notes:       req.Notes,
	}
	if err := s.chapterCharacterRepo.Upsert(cc); err != nil {
		return nil, err
	}
	saved, err := s.chapterCharacterRepo.GetByChapterAndCharacter(chapterID, characterID)
	if err != nil {
		return cc, nil
	}
	return saved, nil
}

// DeleteChapterCharacter 删除章节级角色覆盖（回退到项目级）
func (s *CharacterService) DeleteChapterCharacter(chapterID, characterID uint) error {
	if s.chapterCharacterRepo == nil {
		return fmt.Errorf("chapter character repository not configured")
	}
	return s.chapterCharacterRepo.Delete(chapterID, characterID)
}

func (s *CharacterService) GenerateProfile(tenantID uint, novelID uint, description string) (*model.Character, error) {
	prompt := fmt.Sprintf("根据以下描述生成小说角色档案：%s\n以JSON格式返回：{\"name\":\"角色名\",\"role\":\"protagonist/antagonist/supporting\",\"background\":\"背景故事\",\"appearance\":\"外貌描述\",\"personality\":\"性格特点\"}", description)
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "character_profile", prompt, "")
	if err != nil {
		return nil, err
	}

	var profile struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Background  string `json:"background"`
		Appearance  string `json:"appearance"`
		Personality string `json:"personality"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &profile); err != nil {
		return &model.Character{
			UUID:       uuid.New().String(),
			NovelID:    novelID,
			Name:       "AI生成角色",
			Role:       "supporting",
			Background: result,
			Status:     "active",
		}, nil
	}
	return &model.Character{
		UUID:       uuid.New().String(),
		NovelID:    novelID,
		Name:       profile.Name,
		Role:       profile.Role,
		Background: profile.Background,
		Appearance: profile.Appearance,
		Status:     "active",
	}, nil
}

// AIBatchGenerate 使用 AI 批量生成/更新小说角色（按 novel_id+name upsert，仅补填空字段）
// AIBatchGenerate 使用 AI 批量生成/更新小说角色（两阶段：先提名单，再并发生成档案）
func (s *CharacterService) AIBatchGenerate(tenantID, novelID uint) ([]*model.Character, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}

	existing, _ := s.characterRepo.ListByNovel(novelID)
	byName := make(map[string]*model.Character, len(existing))
	for _, c := range existing {
		byName[c.Name] = c
	}

	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}

	// 获取小说标题/类型
	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	// ── 阶段一：逐章并发提取角色名单，合并去重 ──────────────────────────────
	nameList, err := s.extractCharacterNamesFromChapters(tenantID, novelID, novelTitle, novelGenre, chapters)
	if err != nil {
		return nil, fmt.Errorf("phase 1 (extract names per chapter): %w", err)
	}
	if len(nameList) == 0 {
		return nil, fmt.Errorf("AI 未识别到任何主要角色，请确认小说内容是否充足")
	}
	log.Printf("CharacterService.AIBatchGenerate: phase1 got %d characters: %v", len(nameList), func() []string {
		ns := make([]string, len(nameList))
		for i, e := range nameList {
			ns[i] = e.Name
		}
		return ns
	}())

	// ── 阶段二：并发生成每个角色的完整档案（短摘要，最多 3 并发）────────────
	// 阶段二每次只处理一个角色，用较短摘要节省 token
	shortSummaries := buildChapterSummariesText(chapters, 5, 2000)
	if shortSummaries == "" {
		shortSummaries = collectContent(chapters, 5, 2000)
	}

	type profileResult struct {
		profile *analysisCharJSON
		err     error
	}
	results := make([]profileResult, len(nameList))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, entry := range nameList {
		wg.Add(1)
		go func(idx int, e charNameEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := s.generateOneCharacterProfile(tenantID, novelID, novelTitle, novelGenre, e, shortSummaries)
			results[idx] = profileResult{p, err}
		}(i, entry)
	}
	wg.Wait()

	// ── Upsert ───────────────────────────────────────────────────────────────
	upserted := make([]*model.Character, 0, len(nameList))
	for i, res := range results {
		if res.err != nil {
			log.Printf("CharacterService.AIBatchGenerate: generate profile for %q: %v", nameList[i].Name, res.err)
			continue
		}
		p := res.profile
		if p == nil || p.Name == "" {
			continue
		}

		abilitiesJSON := ""
		if len(p.Abilities) > 0 {
			if b, err := json.Marshal(p.Abilities); err == nil {
				abilitiesJSON = string(b)
			}
		}
		personalityTagsJSON := ""
		if len(p.PersonalityTags) > 0 {
			if b, err := json.Marshal(p.PersonalityTags); err == nil {
				personalityTagsJSON = string(b)
			}
		}
		dialogueStyleJSON := ""
		if p.DialogueStyle.VocabularyLevel != "" || len(p.DialogueStyle.Patterns) > 0 {
			if b, err := json.Marshal(p.DialogueStyle); err == nil {
				dialogueStyleJSON = string(b)
			}
		}
		visualDesignJSON := ""
		if p.VisualPrompt != "" {
			if b, err := json.Marshal(map[string]string{"image_prompt": p.VisualPrompt}); err == nil {
				visualDesignJSON = string(b)
			}
		}
		role := p.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			role = "supporting"
		}

		if ch, ok := byName[p.Name]; ok {
			changed := false
			if v, ok := fillIfEmpty(ch.Role, role); ok { ch.Role = v; changed = true }
			if v, ok := fillIfEmpty(ch.Archetype, p.Archetype); ok { ch.Archetype = v; changed = true }
			if v, ok := fillIfEmpty(ch.Appearance, p.Appearance); ok { ch.Appearance = v; changed = true }
			if v, ok := fillIfEmpty(ch.Personality, p.Personality); ok { ch.Personality = v; changed = true }
			if v, ok := fillIfEmpty(ch.Background, p.Background); ok { ch.Background = v; changed = true }
			if v, ok := fillIfEmpty(ch.CharacterArc, p.CharacterArc); ok { ch.CharacterArc = v; changed = true }
			if v, ok := fillIfEmpty(ch.Abilities, abilitiesJSON); ok { ch.Abilities = v; changed = true }
			if v, ok := fillIfEmpty(ch.PersonalityTags, personalityTagsJSON); ok { ch.PersonalityTags = v; changed = true }
			if v, ok := fillIfEmpty(ch.DialogueStyle, dialogueStyleJSON); ok { ch.DialogueStyle = v; changed = true }
			if v, ok := fillIfEmpty(ch.VisualDesign, visualDesignJSON); ok { ch.VisualDesign = v; changed = true }
			if !changed {
				upserted = append(upserted, ch)
				continue
			}
			if err := s.characterRepo.Update(ch); err != nil {
				log.Printf("CharacterService.AIBatchGenerate: update %s: %v", ch.Name, err)
				continue
			}
			upserted = append(upserted, ch)
		} else {
			character := &model.Character{
				UUID:            uuid.New().String(),
				NovelID:         novelID,
				Name:            p.Name,
				Role:            role,
				Archetype:       p.Archetype,
				Appearance:      p.Appearance,
				Personality:     p.Personality,
				PersonalityTags: personalityTagsJSON,
				Background:      p.Background,
				CharacterArc:    p.CharacterArc,
				Abilities:       abilitiesJSON,
				DialogueStyle:   dialogueStyleJSON,
				VisualDesign:    visualDesignJSON,
				Status:          "active",
			}
			if err := s.characterRepo.Create(character); err != nil {
				log.Printf("CharacterService.AIBatchGenerate: create %s: %v", p.Name, err)
				continue
			}
			upserted = append(upserted, character)
		}
	}

	if len(upserted) == 0 && len(nameList) > 0 {
		return nil, fmt.Errorf("所有角色档案生成均失败，请检查 AI 提供商配置")
	}
	return upserted, nil
}

// AIExtractMinorChars 从单章内容中提取次要角色（role=minor），并写入 ChapterCharacter 关联
func (s *CharacterService) AIExtractMinorChars(tenantID, novelID, chapterID uint) ([]*model.Character, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	content := chapter.Content
	if content == "" {
		content = chapter.Summary
	}
	if content == "" {
		return nil, fmt.Errorf("chapter has no content")
	}

	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	// 已有角色名列表，用于去重
	existing, _ := s.characterRepo.ListByNovel(novelID)
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, c := range existing {
		existingNames = append(existingNames, c.Name)
		existingNameSet[strings.ToLower(c.Name)] = true
	}

	tmplStr := loadPromptTemplate("extract_minor_characters.tmpl")
	tmpl, err := template.New("extract_minor_characters").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         novelGenre,
		"ExistingNames": existingNames,
		"Content":       content,
	}); err != nil {
		return nil, err
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_minor_characters", buf.String(), "")
	if err != nil {
		return nil, fmt.Errorf("AI extract minor chars: %w", err)
	}

	var chars []analysisCharJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &chars); err != nil {
		return nil, fmt.Errorf("parse minor chars JSON: %w", err)
	}

	var created []*model.Character
	for _, c := range chars {
		if c.Name == "" || existingNameSet[strings.ToLower(c.Name)] {
			continue
		}
		visualDesign := ""
		if c.VisualPrompt != "" {
			if vd, e := json.Marshal(map[string]string{"image_prompt": c.VisualPrompt}); e == nil {
				visualDesign = string(vd)
			}
		}
		char := &model.Character{
			NovelID:      novelID,
			UUID:         uuid.New().String(),
			Name:         c.Name,
			Role:         "minor",
			Archetype:    c.Archetype,
			Appearance:   c.Appearance,
			Personality:  c.Personality,
			Background:   c.Background,
			CharacterArc: c.CharacterArc,
			VisualDesign: visualDesign,
			Status:       "active",
		}
		if e := s.characterRepo.Create(char); e != nil {
			log.Printf("CharacterService.AIExtractMinorChars: create %q: %v", c.Name, e)
			continue
		}
		existingNameSet[strings.ToLower(c.Name)] = true
		// 关联到章节
		if s.chapterCharacterRepo != nil {
			cc := &model.ChapterCharacter{
				CharacterID: char.ID,
				ChapterID:   chapterID,
				NovelID:     novelID,
			}
			if e := s.chapterCharacterRepo.Upsert(cc); e != nil {
				log.Printf("CharacterService.AIExtractMinorChars: link chapter %v: %v", chapterID, e)
			}
		}
		created = append(created, char)
	}
	return created, nil
}

func (s *CharacterService) AnalyzeConsistency(id uint, images []string) (interface{}, error) {
	return map[string]interface{}{
		"character_id":      id,
		"consistency_score": 0.9,
		"images_analyzed":   len(images),
	}, nil
}

// ============================================
// ImageGenerationService 图像生成服务
// ============================================

type ImageGenerationService struct {
	aiService *AIService
}

func NewImageGenerationService(aiService *AIService) *ImageGenerationService {
	return &ImageGenerationService{aiService: aiService}
}

type GeneratedCharacterImage struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

func (s *ImageGenerationService) GenerateCharacterImage(req *model.GenerateImageRequest) (*GeneratedCharacterImage, error) {
	options := &ImageGenerationOptions{
		Prompt:   fmt.Sprintf("%s, %s, %s style", req.Subject, req.Description, req.Style),
		Size:     "512x512",
		Steps:    50,
		CFGScale: 7.5,
	}
	image, err := s.aiService.GenerateImage(options.Prompt, options)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: image.URL, Description: req.Description}, nil
}

// GenerateThreeViewImage 生成单个视角的角色三视图
// viewType: "front" | "side" | "back"
// gender: "male" | "female" | "neutral" | ""（空时不注入性别词）
// referenceImage: 肖像参考图 URL（用于 IP-Adapter 保持面部一致性，可为空）
// provider: 指定图像生成提供者（可为空，空时自动选择）
func (s *ImageGenerationService) GenerateThreeViewImage(tenantID uint, name, appearance, viewType, style, gender, referenceImage, provider string) (*GeneratedCharacterImage, error) {
	// "角色设定参考图" 会被模型理解成"多视角设计总表"，导致单图出现多个人物，改用单视角描述词。
	viewDesc := map[string]string{
		"front": "正面站立，面朝镜头，全身",
		"side":  "侧身站立，侧面朝向，全身",
		"back":  "背对镜头站立，全身",
	}
	angleDesc, ok := viewDesc[viewType]
	if !ok {
		return nil, fmt.Errorf("invalid view type: %s", viewType)
	}
	genderDesc := map[string]string{
		"male":    "男性",
		"female":  "女性",
		"neutral": "中性",
	}
	genderStr := genderDesc[gender] // empty string if gender not set
	// 将前端 image_style ID 映射为 AI 可理解的中文风格描述
	styleDesc := map[string]string{
		"anime":        "日系动漫插画",
		"realistic":    "写实摄影",
		"ink_painting": "水墨中国风插画",
		"cyberpunk":    "赛博朋克风格插画",
		"xianxia_style": "古典仙侠国风插画",
		"oil_painting": "油画风格插画",
		"watercolor":   "水彩插画",
	}
	styleStr := styleDesc[style]
	if styleStr == "" {
		if style != "" {
			styleStr = style // 未知风格直接透传
		} else {
			styleStr = "日系动漫插画"
		}
	}

	// 性别 token 放在提示词最前面以获得最高权重。
	// 英文 booru 标签（1boy/1girl）对插画模型约束力最强；中文作为辅助。
	genderTag := map[string]string{"male": "1boy", "female": "1girl"}[gender]
	genderLeader := genderTag // prefix: "1boy" / "1girl" / ""
	if genderStr != "" && genderLeader == "" {
		genderLeader = genderStr // neutral: 用中文作前缀
	}

	var prompt string
	if style == "realistic" {
		// 写实：English terms 更有效
		realisticGender := map[string]string{"male": "1man, male, ", "female": "1woman, female, ", "neutral": ""}[gender]
		prompt = fmt.Sprintf("%ssolo, 单人, 只有一个人物, %s, %s, %s, realistic photography, pure white background, detailed lighting, high quality portrait",
			realisticGender, name, appearance, angleDesc)
	} else {
		if genderLeader != "" {
			prompt = fmt.Sprintf("%s, solo, 单人, 只有一个人物，%s，%s，%s，%s风格，白色背景，线条清晰，高品质",
				genderLeader, name, appearance, angleDesc, styleStr)
		} else {
			prompt = fmt.Sprintf("solo, 单人, 只有一个人物，%s，%s，%s，%s风格，白色背景，线条清晰，高品质",
				name, appearance, angleDesc, styleStr)
		}
	}
	// Only pass an absolute HTTP(S) URL to the AI — local/relative paths cannot be fetched by remote APIs.
	aiRef := referenceImage
	if !strings.HasPrefix(aiRef, "http://") && !strings.HasPrefix(aiRef, "https://") {
		aiRef = ""
	}
	if aiRef != "" {
		log.Printf("GenerateThreeViewImage: %s/%s using reference image %s", name, viewType, aiRef)
	} else {
		log.Printf("GenerateThreeViewImage: %s/%s no valid reference image", name, viewType)
	}
	// 负向提示词：始终禁止多人，再叠加性别排斥词
	baseNeg := "multiple people, two people, duo, couple, group, 多人, 两人, 三人, 合照, nsfw, lowres, bad anatomy"
	genderNeg := map[string]string{
		"male":   "female, girl, woman, 女性, 女生, 裙子, 长裙, 女装, feminine, she, her",
		"female": "male, man, boy, 男性, 男生, 胡须, beard, mustache, masculine, he, him",
	}[gender]
	negativePrompt := baseNeg
	if genderNeg != "" {
		negativePrompt = baseNeg + ", " + genderNeg
	}
	url, err := s.aiService.GenerateCharacterThreeView(context.Background(), tenantID, provider, prompt, aiRef, style, negativePrompt)
	if err != nil {
		return nil, err
	}
	return &GeneratedCharacterImage{URL: url, Description: fmt.Sprintf("%s %s", name, viewType)}, nil
}

// ============================================
// StoryboardService 分镜服务（handler-facing）
// ============================================

type StoryboardService struct {
	videoService *VideoService
	aiService    *AIService
}

func NewStoryboardService(videoService *VideoService, aiService *AIService) *StoryboardService {
	return &StoryboardService{videoService: videoService, aiService: aiService}
}

func (s *StoryboardService) GenerateStoryboard(videoID, chapterID uint, characters []string, style, provider, userPrompt string) (interface{}, error) {
	var chapterIDPtr *uint
	if chapterID != 0 {
		chapterIDPtr = &chapterID
	}
	shots, err := s.videoService.GenerateStoryboard(videoID, provider, userPrompt, chapterIDPtr)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"video_id":   videoID,
		"chapter_id": chapterID,
		"shots":      shots,
		"total":      len(shots),
	}, nil
}

func (s *StoryboardService) AnalyzeEmotions(content string) (interface{}, error) {
	prompt := fmt.Sprintf("请分析以下内容的情感曲线：\n%s", content)
	result, err := s.aiService.Generate(0, "analysis", prompt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"analysis": result,
		"content":  content[:min(100, len(content))],
	}, nil
}

// ============================================
// QualityControlService adapter methods
// ============================================

// CheckChapter handler-compatible wrapper — delegates to the real AI+rule-based check.
func (s *QualityControlService) CheckChapter(id uint) (*QualityReport, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("chapter %d not found: %w", id, err)
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel %d not found: %w", chapter.NovelID, err)
	}
	return s.CheckChapterQuality(context.Background(), chapter, novel)
}

// ============================================
// VideoEnhancementService adapter methods
// ============================================

// EnhanceVideo handler-compatible wrapper (accepts model.EnhancementConfig)
func (s *VideoEnhancementService) EnhanceVideo(videoURL string, enhancements []model.EnhancementConfig) (interface{}, error) {
	configs := make([]EnhancementConfig, 0, len(enhancements))
	for _, e := range enhancements {
		configs = append(configs, EnhancementConfig{
			Type:      EnhancementType(e.Type),
			Enabled:   e.Enabled,
			Intensity: e.Intensity,
		})
	}
	return s.EnhanceVideoWithConfigs(videoURL, configs)
}

// GetRecommendations handler-compatible wrapper
func (s *VideoEnhancementService) GetRecommendations(fps int, resolution string, duration int, style string) (interface{}, error) {
	return map[string]interface{}{
		"fps":        fps,
		"resolution": resolution,
		"duration":   duration,
		"style":      style,
		"recommendations": []map[string]interface{}{
			{"type": "frame_interpolation", "priority": "high", "reason": "提升流畅度"},
			{"type": "super_resolution", "priority": "medium", "reason": "提升画质"},
		},
	}, nil
}

// ============================================
// CharacterArcService adapter methods
// ============================================

func (s *CharacterArcService) GetAllArcs(novelID uint) (interface{}, error) {
	characters, err := s.charRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	arcs := make([]interface{}, 0, len(characters))
	for _, c := range characters {
		arc, _ := s.GetCharacterArc(novelID, c.ID)
		arcs = append(arcs, arc)
	}
	return arcs, nil
}

func (s *CharacterArcService) UpdateArc(novelID, characterID uint, currentStage int, note string) (interface{}, error) {
	arc, err := s.GetCharacterArc(novelID, characterID)
	if err != nil {
		return nil, err
	}
	arc.CurrentStage = currentStage
	return arc, nil
}

// ============================================
// GenerationContextService adapter methods
// ============================================

func (s *GenerationContextService) BuildGenerationPrompt(novelID uint, chapterNo int, style, extraPrompt string, maxContextLen int) (string, error) {
	ctx, err := s.GetContext(novelID, chapterNo)
	if err != nil {
		return "", err
	}
	var sc *StyleConfig
	if style != "" {
		sc = &StyleConfig{NarrativeVoice: style}
	}
	prompt := s.buildGenerationPrompt(ctx, chapterNo, sc, extraPrompt)
	return prompt, nil
}

func (s *GenerationContextService) GetContextPreview(novelID uint) (interface{}, error) {
	ctx, err := s.GetContext(novelID, 0)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"novel_id":      novelID,
		"total_context": fmt.Sprintf("%d chars", len(ctx.GlobalSummary)),
		"summary":       ctx.GlobalSummary,
	}, nil
}

// ============================================
// StyleService adapter methods
// ============================================

func (s *StyleService) GetDefaultStyle() (*StyleConfig, error) {
	return s.getDefaultStyleConfig(), nil
}

func (s *StyleService) BuildStylePrompt(req interface{}) (string, error) {
	cfg := s.getDefaultStyleConfig()
	// Use encoding/json to copy compatible fields
	if data, err := json.Marshal(req); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return s.buildStylePromptInternal(cfg), nil
}

func (s *StyleService) GetStylePresets() interface{} {
	return []map[string]interface{}{
		{"name": "literary", "description": "文学风格"},
		{"name": "commercial", "description": "商业小说风格"},
		{"name": "young_adult", "description": "青春小说风格"},
	}
}

func (s *StyleService) ApplyPreset(name string) (interface{}, error) {
	presets := map[string]*StyleConfig{
		"literary": {
			NarrativeVoice:     "third_limited",
			EmotionalTone:      "cold",
			SentenceComplexity: "complex",
			DescriptionDensity: "rich",
		},
		"commercial": {
			NarrativeVoice:     "third_omniscient",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "moderate",
		},
		"young_adult": {
			NarrativeVoice:     "first_person",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "minimal",
		},
	}
	style, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("preset %s not found", name)
	}
	return style, nil
}

// ============================================
// ModelService adapter methods
// ============================================

func (s *ModelService) ListProviders(tenantID uint) (interface{}, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	return providers, err
}

// ListSystemProviders 返回系统预置提供商列表（tenant_id=0），用于前端模板下拉框。
func (s *ModelService) ListSystemProviders() ([]*model.ModelProvider, error) {
	return s.providerRepo.ListSystem()
}

// CapableProvider is a minimal provider descriptor returned by capability-filtered listing endpoints.
type CapableProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// providerDisplayNames maps well-known provider names to human-readable labels.
var providerDisplayNames = map[string]string{
	"openai":            "OpenAI",
	"claude":            "Claude (Anthropic)",
	"anthropic":         "Claude (Anthropic)",
	"deepseek":          "DeepSeek",
	"doubao":            "豆包 (Doubao)",
	"qianwen":           "通义千问 (Qianwen)",
	"gemini":            "Gemini (Google)",
	"google":            "Gemini (Google)",
	"kling":             "可灵 (Kling)",
	"seedance":          "Seedance",
	ai.ProviderNameVolcengineVisual: "火山引擎图像",
}

// providerHasCredentials reports whether p has all required credentials.
// volcengine-visual uses AK/SK (two fields); all other providers use a single APIKey.
func providerHasCredentials(p *model.ModelProvider) bool {
	if p.Name == ai.ProviderNameVolcengineVisual {
		return strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.APISecretKey) != ""
	}
	return strings.TrimSpace(p.APIKey) != ""
}

func capableProviderDisplayName(providerName, dbDisplayName string) string {
	if dbDisplayName != "" {
		return dbDisplayName
	}
	if dn := providerDisplayNames[providerName]; dn != "" {
		return dn
	}
	return providerName
}

// listCapableProviders returns active, key-bearing providers whose Type field matches typeFilter.
func (s *ModelService) listCapableProviders(tenantID uint, typeFilter string) ([]CapableProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	var result []CapableProvider
	for _, p := range providers {
		if !p.IsActive || !strings.EqualFold(p.Type, typeFilter) {
			continue
		}
		if providerHasCredentials(p) {
			result = append(result, CapableProvider{
				Name:        p.Name,
				DisplayName: capableProviderDisplayName(p.Name, p.DisplayName),
			})
		}
	}
	return result, nil
}

// ListCapableProviders returns active, credentialed providers matching the given type (e.g. "LLM", "IMAGE").
func (s *ModelService) ListCapableProviders(tenantID uint, providerType string) ([]CapableProvider, error) {
	return s.listCapableProviders(tenantID, providerType)
}

func (s *ModelService) GetProvider(id uint, tenantID uint) (*model.ModelProvider, error) {
	return s.providerRepo.GetByIDAndTenant(id, tenantID)
}

// suitableTasksForProviderType returns the JSON suitable_tasks array for a provider type.
func suitableTasksForProviderType(providerType string) string {
	switch strings.ToLower(providerType) {
	case "image":
		return `["image_gen"]`
	case "video":
		return `["video_gen"]`
	case "embedding":
		return `["embedding"]`
	case "voice", "tts":
		return `["voice_gen"]`
	default: // "llm" and anything unrecognised
		return `["chapter"]`
	}
}

// seedProviderModel upserts a default AIModel row for the given provider if api_version is set.
func (s *ModelService) seedProviderModel(provider *model.ModelProvider) {
	if provider.APIVersion == "" {
		return
	}
	tasks := suitableTasksForProviderType(provider.Type)
	existing, _ := s.modelRepo.List(&provider.ID)
	for _, m := range existing {
		if m.Name == provider.APIVersion {
			return // already seeded
		}
	}
	m := &model.AIModel{
		ProviderID:    provider.ID,
		Name:          provider.APIVersion,
		DisplayName:   provider.APIVersion,
		SuitableTasks: tasks,
		IsActive:      true,
		IsAvailable:   true,
	}
	_ = s.modelRepo.Create(m)
}

// SeedAllProviders seeds AIModel rows for every existing provider that has an
// api_version set but no matching model row yet. Also fixes existing rows that
// were created with is_available=false due to a prior bug in CreateModel.
// Call once at startup.
func (s *ModelService) SeedAllProviders() {
	providers, err := s.providerRepo.List()
	if err != nil {
		return
	}
	for _, p := range providers {
		s.seedProviderModel(p)
	}
	// One-time fix: activate any manually-created models that have is_active=true
	// but is_available=false (created before the bug was fixed).
	all, err := s.modelRepo.List(nil)
	if err != nil {
		return
	}
	for _, m := range all {
		if m.IsActive && !m.IsAvailable {
			m.IsAvailable = true
			_ = s.modelRepo.Update(m)
		}
	}
}

func (s *ModelService) CreateProvider(req *model.CreateModelProviderRequest, tenantID uint) (*model.ModelProvider, error) {
	provider := &model.ModelProvider{
		TenantID:     tenantID,
		Name:         req.Name,
		DisplayName:  req.DisplayName,
		Type:         req.Type,
		APIEndpoint:  req.APIEndpoint,
		APIKey:       req.APIKey,
		APISecretKey: req.APISecretKey,
		APIVersion:   req.APIVersion,
		IsActive:     req.IsActive,
	}
	if err := s.providerRepo.Create(provider); err != nil {
		return nil, err
	}
	s.seedProviderModel(provider)
	return provider, nil
}

func (s *ModelService) UpdateProvider(id uint, tenantID uint, req *model.UpdateModelProviderRequest) (*model.ModelProvider, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		provider.Name = req.Name
	}
	if req.DisplayName != "" {
		provider.DisplayName = req.DisplayName
	}
	if req.Type != "" {
		provider.Type = req.Type
	}
	if req.APIEndpoint != "" {
		provider.APIEndpoint = req.APIEndpoint
	}
	if req.APIKey != "" {
		provider.APIKey = req.APIKey
	}
	if req.APISecretKey != "" {
		provider.APISecretKey = req.APISecretKey
	}
	if req.APIVersion != "" {
		provider.APIVersion = req.APIVersion
	}
	if req.IsActive != nil {
		provider.IsActive = *req.IsActive
	}
	if err := s.providerRepo.Update(provider); err != nil {
		return nil, err
	}
	s.seedProviderModel(provider)
	// 清除缓存，使下次调用重新从 DB 加载最新凭据
	if s.aiService != nil {
		s.aiService.InvalidateProviderCache(provider.Name)
	}
	return provider, nil
}

func (s *ModelService) DeleteProvider(id uint, tenantID uint) error {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return err
	}
	// 系统级 provider（tenant_id=0）不允许租户删除
	if provider.TenantID != tenantID {
		return fmt.Errorf("cannot delete system-level provider")
	}
	if err := s.providerRepo.Delete(id); err != nil {
		return err
	}
	// 清除缓存，防止已删除的提供商被继续使用
	if s.aiService != nil {
		s.aiService.InvalidateProviderCache(provider.Name)
	}
	return nil
}

func (s *ModelService) TestProvider(id uint, tenantID uint) (interface{}, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}

	// 即梦AI Visual API（AK/SK 鉴权）：直接构造 provider 进行健康检查
	if provider.Name == "volcengine-visual" {
		if provider.APIKey == "" || provider.APISecretKey == "" {
			return map[string]interface{}{"status": "error", "error": "AccessKey 和 SecretKey 均不能为空", "provider_id": id}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vp := ai.NewVolcengineVisualProvider(provider.APIKey, provider.APISecretKey)
		if checkErr := vp.HealthCheck(ctx); checkErr != nil {
			return map[string]interface{}{"status": "error", "error": checkErr.Error(), "provider_id": id}, nil
		}
		return map[string]interface{}{"status": "ok", "provider_id": id}, nil
	}

	if s.aiService != nil {
		if _, loadErr := s.aiService.getTenantProvider(tenantID, provider.Name); loadErr != nil {
			return map[string]interface{}{"status": "error", "error": loadErr.Error(), "provider_id": id}, nil
		}
	}
	return map[string]interface{}{"status": "ok", "provider_id": id}, nil
}

func (s *ModelService) ListModels(providerID *uint) (interface{}, error) {
	models, err := s.modelRepo.List(providerID)
	return models, err
}

func (s *ModelService) CreateModel(req *model.CreateAIModelRequest) (*model.AIModel, error) {
	m := &model.AIModel{
		ProviderID:    req.ProviderID,
		Name:          req.Name,
		SuitableTasks: req.TaskTypes,
		MaxTokens:     req.MaxTokens,
		CostPer1K:     req.CostPer1K,
		IsActive:      true,
		IsAvailable:   true,
	}
	return m, s.modelRepo.Create(m)
}

func (s *ModelService) UpdateModel(id uint, req *model.UpdateAIModelRequest) (*model.AIModel, error) {
	m, err := s.modelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		m.Name = req.Name
	}
	if req.MaxTokens != 0 {
		m.MaxTokens = req.MaxTokens
	}
	if req.CostPer1K != 0 {
		m.CostPer1K = req.CostPer1K
	}
	return m, s.modelRepo.Update(m)
}

func (s *ModelService) DeleteModel(id uint) error {
	return s.modelRepo.Delete(id)
}


func (s *ModelService) TestModel(id uint) (interface{}, error) {
	return map[string]interface{}{"status": "ok", "model_id": id}, nil
}

func (s *ModelService) GetTaskConfig(taskType string) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	return cfg, err
}

func (s *ModelService) UpdateTaskConfig(taskType string, req *model.UpdateTaskConfigRequest) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		return nil, err
	}
	if req.PrimaryModelID != 0 {
		cfg.PrimaryModelID = req.PrimaryModelID
	}
	if req.MaxTokens != 0 {
		cfg.MaxTokens = req.MaxTokens
	}
	if req.Temperature != 0 {
		cfg.Temperature = req.Temperature
	}
	if req.TopP != 0 {
		cfg.TopP = req.TopP
	}
	return cfg, s.taskRepo.Update(cfg)
}

func (s *ModelService) ListExperiments() (interface{}, error) {
	experiments, err := s.experimentRepo.List(100)
	return experiments, err
}

func (s *ModelService) CreateExperiment(req *model.CreateModelComparisonRequest) (interface{}, error) {
	experiment := &model.ModelComparisonExperiment{
		Name:     req.Name,
		TaskType: req.TaskType,
		Status:   "pending",
	}
	return experiment, s.experimentRepo.Create(experiment)
}

func (s *ModelService) GetExperiment(id uint) (interface{}, error) {
	return s.experimentRepo.GetByID(id)
}

func (s *ModelService) StartExperiment(id uint) error {
	return nil
}

func (s *ModelService) GetAvailableModels(taskType string) ([]*model.AIModel, error) {
	return s.modelRepo.GetAvailableByTaskType(taskType)
}

func (s *ModelService) SelectModel(taskType, strategy string) (*model.AIModel, error) {
	models, err := s.modelRepo.GetAvailableByTaskType(taskType)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no available models for task type: %s", taskType)
	}
	switch strategy {
	case "quality":
		return selectByQuality(models), nil
	case "cost":
		return selectByCost(models), nil
	default:
		return selectBalanced(models), nil
	}
}

// ============================================
// ForeshadowService adapter methods
// ============================================

func (s *ForeshadowService) GetForeshadows(novelID uint, chapterNo int) ([]*ForeshadowItem, error) {
	return s.CheckForeshadowStatus(novelID, chapterNo)
}

func (s *ForeshadowService) MarkFulfilledByID(novelID, foreshadowID, chapterID uint) error {
	chapter := &model.Chapter{ID: chapterID}
	return s.MarkFulfilled(novelID, foreshadowID, chapter)
}

// ============================================
// TimelineService adapter methods
// ============================================

func (s *TimelineService) GetTimeline(novelID uint) (*Timeline, error) {
	return s.BuildTimeline(novelID)
}

// ============================================
// WorldviewService
// ============================================

type WorldviewService struct {
	worldviewRepo *repository.WorldviewRepository
	aiService     *AIService
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
}

func NewWorldviewService(worldviewRepo *repository.WorldviewRepository, aiService *AIService) *WorldviewService {
	return &WorldviewService{worldviewRepo: worldviewRepo, aiService: aiService}
}

func (s *WorldviewService) WithNovelRepos(novelRepo *repository.NovelRepository, chapterRepo *repository.ChapterRepository) *WorldviewService {
	s.novelRepo = novelRepo
	s.chapterRepo = chapterRepo
	return s
}

func (s *WorldviewService) CreateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Create(worldview)
}

func (s *WorldviewService) GetWorldview(id uint) (*model.Worldview, error) {
	return s.worldviewRepo.GetByID(id)
}

func (s *WorldviewService) ListWorldviews(page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	return s.worldviewRepo.List(page, pageSize, genre)
}

func (s *WorldviewService) UpdateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Update(worldview)
}

func (s *WorldviewService) DeleteWorldview(id uint) error {
	return s.worldviewRepo.Delete(id)
}

// Entity CRUD

func (s *WorldviewService) GetEntities(worldviewID uint) ([]*model.WorldviewEntity, error) {
	return s.worldviewRepo.GetEntities(worldviewID)
}

func (s *WorldviewService) GetEntity(id uint) (*model.WorldviewEntity, error) {
	var entity model.WorldviewEntity
	if err := s.worldviewRepo.DB().First(&entity, id).Error; err != nil {
		return nil, err
	}
	return &entity, nil
}

func (s *WorldviewService) CreateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.CreateEntity(entity)
}

func (s *WorldviewService) UpdateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.UpdateEntity(entity)
}

func (s *WorldviewService) DeleteEntity(id uint) error {
	return s.worldviewRepo.DeleteEntity(id)
}

// GenerateWorldview AI生成世界观
func (s *WorldviewService) GenerateWorldview(tenantID uint, novelID uint, genre string, hints []string) (*model.Worldview, error) {
	prompt := fmt.Sprintf(`请为【%s】类型的小说生成一个完整、详细的世界观设定。`, genre)

	// 若传入 novelID，优先从小说数据构建上下文
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			prompt = fmt.Sprintf("请根据以下小说信息，为该小说生成一个完整、详细且与之高度契合的世界观设定。\n")
			prompt += fmt.Sprintf("【小说名称】%s\n", novel.Title)
			prompt += fmt.Sprintf("【题材类型】%s\n", novel.Genre)
			if novel.Description != "" {
				prompt += fmt.Sprintf("【小说简介】%s\n", novel.Description)
			}
			if novel.StylePrompt != "" {
				prompt += fmt.Sprintf("【写作风格】%s\n", novel.StylePrompt)
			}
			genre = novel.Genre
			// 附加前几章内容摘要作为上下文
			if s.chapterRepo != nil {
				if chapters, err := s.chapterRepo.ListByNovel(novelID); err == nil && len(chapters) > 0 {
					limit := 3
					if len(chapters) < limit {
						limit = len(chapters)
					}
					prompt += "【已有章节摘要】\n"
					for i := 0; i < limit; i++ {
						ch := chapters[i]
						if ch.Summary != "" {
							prompt += fmt.Sprintf("第%d章《%s》摘要：%s\n", ch.ChapterNo, ch.Title, ch.Summary)
						} else if ch.Content != "" {
							content := ch.Content
							if len(content) > 300 {
								content = content[:300] + "..."
							}
							prompt += fmt.Sprintf("第%d章《%s》内容节选：%s\n", ch.ChapterNo, ch.Title, content)
						}
					}
				}
			}
		}
	} else if len(hints) > 0 {
		prompt += fmt.Sprintf("\n背景参考：%s", strings.Join(hints, "\n"))
	}
	prompt += `

请严格按以下 JSON 格式返回，所有字段均需填写，内容尽量详实（每字段不少于100字）：
{
  "name": "世界观名称（富有特色的专有名词，非类型名）",
  "description": "世界观总体概述，包括核心世界观念、整体氛围和主要冲突主题",
  "magic_system": "修炼/魔法/异能体系的详细描述，包括力量来源、境界划分、修炼方式、天花板设定",
  "geography": "世界地理格局描述，包括主要大陆/区域、重要城市/圣地、地理特征与禁区",
  "history": "世界历史背景，包括重大历史事件、时代更迭、上古传说、现存历史遗留问题",
  "culture": "世界的文化风俗，包括种族/文明构成、宗教信仰、礼仪习俗、价值观念",
  "technology": "世界的科技/炼器/阵法水平，与修炼体系的关系，普通人与修炼者的生活差异",
  "rules": "世界运行的核心规则与禁忌，包括天道法则、禁术禁地、不可违背的世界规律",
  "cheat_system": "主角金手指/系统描述（可选，无则留空）：系统名称、核心功能、等级/点数机制、触发条件"
}`

	result, err := s.aiService.GenerateWithProvider(tenantID, 0, "worldview", prompt, "")
	if err != nil {
		return nil, err
	}

	var data struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		MagicSystem string `json:"magic_system"`
		Geography   string `json:"geography"`
		History     string `json:"history"`
		Culture     string `json:"culture"`
		Technology  string `json:"technology"`
		Rules       string `json:"rules"`
		CheatSystem string `json:"cheat_system"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &data); err != nil {
		log.Printf("GenerateWorldview: failed to parse AI response: %v, raw: %.300s", err, result)
	}

	name := data.Name
	if name == "" {
		name = genre + "世界"
	}

	return &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        name,
		Genre:       genre,
		Description: data.Description,
		MagicSystem: data.MagicSystem,
		Geography:   data.Geography,
		History:     data.History,
		Culture:     data.Culture,
		Technology:  data.Technology,
		Rules:       data.Rules,
		CheatSystem: data.CheatSystem,
	}, nil
}

// ============================================
// ReviewTaskService
// ============================================

type ReviewTaskService struct {
	reviewTaskRepo *repository.ReviewTaskRepository
}

func NewReviewTaskService(reviewTaskRepo *repository.ReviewTaskRepository) *ReviewTaskService {
	return &ReviewTaskService{reviewTaskRepo: reviewTaskRepo}
}

func (s *ReviewTaskService) CreateTask(task *model.ReviewTask) error {
	return s.reviewTaskRepo.Create(task)
}

func (s *ReviewTaskService) GetTask(id uint) (*model.ReviewTask, error) {
	return s.reviewTaskRepo.GetByID(id)
}

func (s *ReviewTaskService) ListPendingTasks(priority string, limit int) ([]*model.ReviewTask, error) {
	return s.reviewTaskRepo.ListPending(priority, limit)
}

func (s *ReviewTaskService) UpdateTaskStatus(id uint, status, note string) error {
	return s.reviewTaskRepo.UpdateStatus(id, status, note)
}

// ============================================
// TenantService
// ============================================

type TenantService struct {
	tenantRepo     *repository.TenantRepository
	tenantUserRepo *repository.TenantUserRepository
}

func NewTenantService(tenantRepo *repository.TenantRepository, tenantUserRepo *repository.TenantUserRepository) *TenantService {
	return &TenantService{tenantRepo: tenantRepo, tenantUserRepo: tenantUserRepo}
}

func (s *TenantService) ListTenants(page, pageSize int) ([]*model.Tenant, int64, error) {
	return s.tenantRepo.List(page, pageSize)
}

func (s *TenantService) GetTenant(id uint) (*model.Tenant, error) {
	return s.tenantRepo.GetByID(id)
}

func (s *TenantService) CreateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Create(tenant)
}

func (s *TenantService) UpdateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Update(tenant)
}

func (s *TenantService) DeleteTenant(id uint) error {
	return s.tenantRepo.Delete(id)
}

func (s *TenantService) GetQuota(id uint) (interface{}, error) {
	tenant, err := s.tenantRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	quota := tenant.GetQuota()
	return map[string]interface{}{
		"tenant_id":  tenant.ID,
		"max_users":  quota.MaxUsers,
		"used_users": tenant.UsedUsers,
	}, nil
}

func (s *TenantService) ListMembers(tenantID uint) ([]*model.TenantUser, error) {
	return s.tenantUserRepo.ListByTenant(tenantID)
}

func (s *TenantService) AddMember(tenantID, userID uint, role string) error {
	tu := &model.TenantUser{
		TenantID: tenantID,
		UserID:   userID,
		Role:     role,
		Status:   "active",
	}
	return s.tenantUserRepo.Create(tu)
}

func (s *TenantService) RemoveMember(tenantID, userID uint) error {
	return s.tenantUserRepo.DeleteByTenantAndUser(tenantID, userID)
}

func (s *TenantService) UpdateMemberRole(tenantID, userID uint, role string) error {
	return s.tenantUserRepo.UpdateRole(tenantID, userID, role)
}
