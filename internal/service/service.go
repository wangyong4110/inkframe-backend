package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// NovelService 小说服务
type NovelService struct {
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
	aiService     *AIService
	characterRepo *repository.CharacterRepository
	snapshotRepo  *repository.CharacterStateSnapshotRepository
}

func NewNovelService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	aiService *AIService,
) *NovelService {
	return &NovelService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
		aiService:   aiService,
	}
}

// WithCharacterRepos 设置角色相关仓库（用于快照写入）
func (s *NovelService) WithCharacterRepos(characterRepo *repository.CharacterRepository, snapshotRepo *repository.CharacterStateSnapshotRepository) *NovelService {
	s.characterRepo = characterRepo
	s.snapshotRepo = snapshotRepo
	return s
}

// GetAIService 返回 AIService（供 handler 查询默认 provider 名称）
func (s *NovelService) GetAIService() *AIService {
	return s.aiService
}

// CreateNovelRequest 创建小说请求
type CreateNovelRequest struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
	Genre       string `json:"genre" binding:"required"`
	WorldviewID *uint  `json:"worldview_id"`
}

// Create 创建小说
func (s *NovelService) Create(req *CreateNovelRequest) (*model.Novel, error) {
	novel := &model.Novel{
		UUID:        uuid.New().String(),
		Title:       req.Title,
		Description: req.Description,
		Genre:       req.Genre,
		Status:      "planning",
		WorldviewID: req.WorldviewID,
	}

	if err := s.novelRepo.Create(novel); err != nil {
		return nil, err
	}

	return novel, nil
}

// GetNovel 获取小说
func (s *NovelService) GetNovel(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
}

// ListNovelsFiltered 获取小说列表（带过滤器）
func (s *NovelService) ListNovelsFiltered(page, pageSize int, filters map[string]interface{}) ([]*model.Novel, int64, error) {
	return s.novelRepo.List(page, pageSize, filters)
}

// ListNovels 获取小说列表
func (s *NovelService) ListNovels(page, pageSize int) ([]*model.Novel, int, error) {
	novels, total, err := s.novelRepo.List(page, pageSize, nil)
	return novels, int(total), err
}

// UpdateNovelEntity 更新小说实体
func (s *NovelService) UpdateNovelEntity(novel *model.Novel) error {
	return s.novelRepo.Update(novel)
}

// DeleteNovel 删除小说
func (s *NovelService) DeleteNovel(id uint) error {
	return s.novelRepo.Delete(id)
}

// CreateNovel handler-compatible wrapper
func (s *NovelService) CreateNovel(req *model.CreateNovelRequest) (*model.Novel, error) {
	return s.Create(&CreateNovelRequest{
		Title:       req.Title,
		Description: req.Description,
		Genre:       req.Genre,
		WorldviewID: req.WorldviewID,
	})
}

// UpdateNovel handler-compatible wrapper
func (s *NovelService) UpdateNovel(id uint, req *model.UpdateNovelRequest) (*model.Novel, error) {
	novel, err := s.novelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		novel.Title = req.Title
	}
	if req.Description != "" {
		novel.Description = req.Description
	}
	if req.Genre != "" {
		novel.Genre = req.Genre
	}
	if req.Status != "" {
		novel.Status = req.Status
	}
	if req.WorldviewID != nil {
		novel.WorldviewID = req.WorldviewID
	}
	if req.CoverImage != "" {
		novel.CoverImage = req.CoverImage
	}
	if req.AIModel != "" {
		novel.AIModel = req.AIModel
	}
	if req.Temperature != nil {
		novel.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		novel.MaxTokens = *req.MaxTokens
	}
	if req.StylePrompt != "" {
		novel.StylePrompt = req.StylePrompt
	}
	if err := s.novelRepo.Update(novel); err != nil {
		return nil, err
	}
	return novel, nil
}


// GenerateOutlineRequest 生成大纲请求
type GenerateOutlineRequest struct {
	NovelID    uint     `json:"novel_id" binding:"required"`
	Prompt     string   `json:"prompt"`
	ChapterNum int      `json:"chapter_num" binding:"required"`
	Keywords   []string `json:"keywords"`
}

// GenerateOutline 生成大纲
func (s *NovelService) GenerateOutline(tenantID uint, req *GenerateOutlineRequest) (*OutlineResult, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}

	// 构建提示词
	prompt := s.buildOutlinePrompt(novel, req)

	// 调用AI生成（使用租户提供商）
	result, err := s.aiService.GenerateWithProvider(tenantID, req.NovelID, "outline", prompt, "")
	if err != nil {
		return nil, err
	}

	// 解析结果
	outline := &OutlineResult{}
	cleaned := extractJSON(result)
	if err := json.Unmarshal([]byte(cleaned), outline); err != nil {
		log.Printf("GenerateOutline: failed to parse AI response for novel %d: %v", req.NovelID, err)
		outline = &OutlineResult{
			Title:    novel.Title,
			Chapters: []ChapterOutline{},
		}
	}

	return outline, nil
}

// OutlineStructure 三幕结构信息
type OutlineStructure struct {
	Act1EndChapter   int `json:"act1_end_chapter"`
	Act2StartChapter int `json:"act2_start_chapter"`
	ClimaxChapter    int `json:"climax_chapter"`
	Act3StartChapter int `json:"act3_start_chapter"`
}

// ForeshadowMapItem 伏笔映射条目
type ForeshadowMapItem struct {
	ID           int    `json:"id"`
	Type         string `json:"type"`
	Description  string `json:"description"`
	PlantChapter int    `json:"plant_chapter"`
	PayoffChapter int   `json:"payoff_chapter"`
}

// OutlineResult 大纲结果
type OutlineResult struct {
	Title         string              `json:"title"`
	Genre         string              `json:"genre,omitempty"`
	Theme         string              `json:"theme,omitempty"`
	Summary       string              `json:"summary,omitempty"`
	Structure     *OutlineStructure   `json:"structure,omitempty"`
	ForeshadowMap []ForeshadowMapItem `json:"foreshadow_map,omitempty"`
	Chapters      []ChapterOutline    `json:"chapters"`
}

// ChapterOutline 章节大纲
type ChapterOutline struct {
	ChapterNo    int      `json:"chapter_no"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	WordCount    int      `json:"word_count"`
	PlotPoints   []string `json:"plot_points"`
	TensionLevel int      `json:"tension_level,omitempty"`
	Hook         string   `json:"hook,omitempty"`
	HookType     string   `json:"hook_type,omitempty"`
	ConflictType string   `json:"conflict_type,omitempty"`
	Act          int      `json:"act,omitempty"`
}

// buildOutlinePrompt 构建大纲提示词
func (s *NovelService) buildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))

	if novel.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Description))
	}

	if len(req.Keywords) > 0 {
		sb.WriteString(fmt.Sprintf("关键词：%s\n\n", strings.Join(req.Keywords, ", ")))
	}

	if req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("创作要求：%s\n\n", req.Prompt))
	}

	sb.WriteString(fmt.Sprintf("请生成%d章的大纲，每章包括：标题、简要概述、预计字数（2000-3000字）、主要剧情点。\n", req.ChapterNum))

	sb.WriteString("\n请以JSON格式返回，格式如下：\n")
	sb.WriteString(`{"title":"小说标题","chapters":[{"chapter_no":1,"title":"章节标题","summary":"章节概述","word_count":2500,"plot_points":["剧情点1","剧情点2"]}]}`)

	return sb.String()
}

// GenerateChapterRequest 生成章节请求
type GenerateChapterRequest struct {
	NovelID   uint   `json:"novel_id" binding:"required"`
	ChapterNo int    `json:"chapter_no" binding:"required"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
}

// GenerateChapter 生成章节
func (s *NovelService) GenerateChapter(req *GenerateChapterRequest) (*model.Chapter, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}

	// 获取前几章作为上下文
	recentChapters, err := s.chapterRepo.GetRecent(req.NovelID, req.ChapterNo, 3)
	if err != nil {
		return nil, err
	}

	// 构建提示词
	prompt := s.buildChapterPrompt(novel, req, recentChapters)

	// 调用AI生成
	content, err := s.aiService.Generate(req.NovelID, "chapter", prompt)
	if err != nil {
		return nil, err
	}

	// 创建章节
	chapter := &model.Chapter{
		UUID:      uuid.New().String(),
		NovelID:   req.NovelID,
		ChapterNo: req.ChapterNo,
		Title:     fmt.Sprintf("第%d章", req.ChapterNo),
		Content:   content,
		WordCount: countChineseChars(content),
		Status:    "completed",
	}

	// 获取上一章
	if len(recentChapters) > 0 {
		prev := recentChapters[0]
		chapter.PreviousChapterID = &prev.ID
	}

	// 生成摘要
	summary, _ := s.aiService.Generate(req.NovelID, "summary", fmt.Sprintf("请简要概括以下内容，不超过100字：\n%s", content))
	chapter.Summary = summary

	if err := s.chapterRepo.Create(chapter); err != nil {
		return nil, err
	}

	// 更新小说统计
	s.updateNovelStats(req.NovelID)

	// 提取剧情点
	s.extractPlotPoints(chapter)

	// 写入角色状态快照（非阻塞）
	if s.characterRepo != nil && s.snapshotRepo != nil {
		go s.writeCharacterSnapshots(chapter)
	}

	return chapter, nil
}

// writeCharacterSnapshots 从章节内容中提取角色状态并写入快照
func (s *NovelService) writeCharacterSnapshots(chapter *model.Chapter) {
	characters, err := s.characterRepo.ListByNovel(chapter.NovelID)
	if err != nil || len(characters) == 0 {
		return
	}

	// 构建角色列表字符串
	charNames := make([]string, 0, len(characters))
	for _, c := range characters {
		charNames = append(charNames, c.Name)
	}

	contentPreview := chapter.Content
	if len(contentPreview) > 2000 {
		contentPreview = contentPreview[:2000] + "..."
	}

	prompt := fmt.Sprintf(`从以下章节内容中提取主要角色的当前状态，以JSON格式返回：
角色列表：%s
章节内容：
%s

请返回以下JSON格式（只包含章节中出现的角色）：
{"characters":[{"name":"角色名","mood":"情绪状态","location":"当前位置","motivation":"当前动机","power_level":5}]}`,
		strings.Join(charNames, "、"), contentPreview)

	result, err := s.aiService.Generate(chapter.NovelID, "character_state", prompt)
	if err != nil {
		log.Printf("writeCharacterSnapshots: AI extraction failed for chapter %d: %v", chapter.ID, err)
		return
	}

	cleaned := extractJSON(result)
	var extraction struct {
		Characters []struct {
			Name       string `json:"name"`
			Mood       string `json:"mood"`
			Location   string `json:"location"`
			Motivation string `json:"motivation"`
			PowerLevel int    `json:"power_level"`
		} `json:"characters"`
	}

	if err := json.Unmarshal([]byte(cleaned), &extraction); err != nil {
		log.Printf("writeCharacterSnapshots: parse failed: %v", err)
		return
	}

	// 建立名称到ID的映射
	nameToChar := make(map[string]*model.Character)
	for _, c := range characters {
		nameToChar[c.Name] = c
	}

	for _, state := range extraction.Characters {
		char, ok := nameToChar[state.Name]
		if !ok {
			continue
		}
		snapshot := &model.CharacterStateSnapshot{
			CharacterID:  char.ID,
			ChapterID:    chapter.ID,
			Mood:         state.Mood,
			Location:     state.Location,
			Motivation:   state.Motivation,
			PowerLevel:   state.PowerLevel,
			SnapshotTime: chapter.CreatedAt,
		}
		if err := s.snapshotRepo.Create(snapshot); err != nil {
			log.Printf("writeCharacterSnapshots: create snapshot failed for char %d: %v", char.ID, err)
		}
	}
}

// buildChapterPrompt 构建章节提示词
func (s *NovelService) buildChapterPrompt(novel *model.Novel, req *GenerateChapterRequest, recentChapters []*model.Chapter) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("请为小说《%s》撰写第%d章。\n\n", novel.Title, req.ChapterNo))

	// 添加世界观信息
	if novel.Worldview != nil {
		sb.WriteString("【世界观设定】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("修炼体系：%s\n", novel.Worldview.MagicSystem))
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("地理环境：%s\n", novel.Worldview.Geography))
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString(fmt.Sprintf("文化背景：%s\n", novel.Worldview.Culture))
		}
		sb.WriteString("\n")
	}

	// 添加前几章内容作为上下文
	if len(recentChapters) > 0 {
		sb.WriteString("【前情提要】\n")
		for i := len(recentChapters) - 1; i >= 0; i-- {
			ch := recentChapters[i]
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, ch.Summary))
		}
		sb.WriteString("\n")
	}

	if req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("【创作要求】%s\n\n", req.Prompt))
	}

	sb.WriteString(fmt.Sprintf("请撰写第%d章的完整内容，字数要求2000-3000字。\n", req.ChapterNo))
	sb.WriteString("请注意：\n")
	sb.WriteString("1. 保持与前文的剧情连贯性\n")
	sb.WriteString("2. 角色性格和对话风格保持一致\n")
	sb.WriteString("3. 遵循世界观设定\n")
	sb.WriteString("4. 适当埋下伏笔，为后续剧情做铺垫")

	return sb.String()
}

// countChineseChars 统计中文字符数
func countChineseChars(text string) int {
	count := 0
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fa5 {
			count++
		}
	}
	return count
}

// updateNovelStats 更新小说统计
func (s *NovelService) updateNovelStats(novelID uint) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		log.Printf("updateNovelStats: list chapters for novel %d: %v", novelID, err)
		return
	}

	var totalWords int
	for _, ch := range chapters {
		totalWords += ch.WordCount
	}

	fields := map[string]interface{}{
		"chapter_count": len(chapters),
		"total_words":   totalWords,
	}

	if len(chapters) > 0 {
		fields["status"] = "writing"
	}

	if err := s.novelRepo.UpdateFields(novelID, fields); err != nil {
		log.Printf("updateNovelStats: update novel %d: %v", novelID, err)
	}
}

// extractPlotPoints 提取剧情点
func (s *NovelService) extractPlotPoints(chapter *model.Chapter) {
	prompt := fmt.Sprintf(`请从以下章节内容中提取关键剧情点，返回JSON数组格式：
{
  "plot_points": [
    {
      "type": "conflict/climax/resolution/twist/foreshadow",
      "description": "剧情点描述",
      "characters": ["角色名1", "角色名2"],
      "locations": ["地点"]
    }
  ]
}
章节内容：%s`, chapter.Content)

	result, err := s.aiService.Generate(chapter.NovelID, "plot_extraction", prompt)
	if err != nil {
		return
	}

	var plotResult struct {
		PlotPoints []struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Characters  []string `json:"characters"`
			Locations   []string `json:"locations"`
		} `json:"plot_points"`
	}

	if err := json.Unmarshal([]byte(result), &plotResult); err != nil {
		return
	}

	// 这里可以存储剧情点到数据库
	_ = plotResult
}

// providerCacheEntry 提供商缓存条目
type providerCacheEntry struct {
	provider  ai.AIProvider
	expiresAt time.Time
}

// AIService AI服务
type AIService struct {
	modelRepo    *repository.AIModelRepository
	taskRepo     *repository.TaskModelConfigRepository
	aiManager    *ai.ModelManager
	providerRepo *repository.ModelProviderRepository
	providerCache sync.Map // key: "tenantID:providerName" → providerCacheEntry
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	taskRepo *repository.TaskModelConfigRepository,
	aiManager *ai.ModelManager,
	providerRepo ...*repository.ModelProviderRepository,
) *AIService {
	svc := &AIService{
		modelRepo: modelRepo,
		taskRepo:  taskRepo,
		aiManager: aiManager,
	}
	if len(providerRepo) > 0 {
		svc.providerRepo = providerRepo[0]
	}
	return svc
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, novelID, taskType, prompt, "")
}

// GetDefaultProviderName 返回当前默认 provider 名称
func (s *AIService) GetDefaultProviderName() string {
	if s.aiManager == nil {
		return "unknown"
	}
	p, err := s.aiManager.GetProvider("")
	if err != nil {
		return "unknown"
	}
	return p.GetName()
}

// getTenantProvider 按租户加载提供商（带缓存，TTL 5 分钟）
func (s *AIService) getTenantProvider(tenantID uint, providerName string) (ai.AIProvider, error) {
	if s.providerRepo == nil {
		return s.aiManager.GetProvider(providerName)
	}

	cacheKey := fmt.Sprintf("%d:%s", tenantID, providerName)

	// 检查缓存
	if v, ok := s.providerCache.Load(cacheKey); ok {
		entry, assertOK := v.(providerCacheEntry)
		if !assertOK {
			s.providerCache.Delete(cacheKey)
		} else if time.Now().Before(entry.expiresAt) {
			return entry.provider, nil
		} else {
			s.providerCache.Delete(cacheKey)
		}
	}

	// 从 DB 加载（租户私有 + 系统级）
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return s.aiManager.GetProvider(providerName)
	}

	// 优先租户私有，其次系统级
	var tenantMatch, systemMatch *model.ModelProvider
	for _, p := range providers {
		if providerName == "" || p.Name == providerName {
			if p.TenantID == tenantID && tenantID != 0 {
				tenantMatch = p
				break
			}
			if p.TenantID == 0 && systemMatch == nil {
				systemMatch = p
			}
		}
	}
	matched := tenantMatch
	if matched == nil {
		matched = systemMatch
	}

	if matched == nil {
		// DB 中无配置，降级到内存 aiManager
		return s.aiManager.GetProvider(providerName)
	}

	// 按 Name 实例化对应的 AIProvider
	var provider ai.AIProvider
	switch matched.Name {
	case "openai":
		provider = ai.NewOpenAIProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	case "anthropic":
		provider = ai.NewAnthropicProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	case "google":
		provider = ai.NewGoogleProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	case "doubao":
		provider = ai.NewDoubaoProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	case "qianwen":
		provider = ai.NewQianwenProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion)
	default:
		return s.aiManager.GetProvider(providerName)
	}

	// 包装重试
	provider = ai.NewRetryProvider(provider, 3, 500*time.Millisecond)

	// 写入缓存
	s.providerCache.Store(cacheKey, providerCacheEntry{
		provider:  provider,
		expiresAt: time.Now().Add(5 * time.Minute),
	})

	return provider, nil
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
func (s *AIService) GenerateWithProvider(tenantID uint, novelID uint, taskType string, prompt string, providerName string) (string, error) {
	// 获取任务配置
	config, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		config = &model.TaskModelConfig{
			Temperature: 0.7,
			MaxTokens:   4096,
		}
	}

	// 调用真实AI API
	result, err := s.callAIWithProvider(tenantID, prompt, config, providerName)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}

	// 记录使用
	s.logUsage(config, prompt, result)

	return result, nil
}

// callAI 调用AI接口（使用系统级 provider）
func (s *AIService) callAI(prompt string, config *model.TaskModelConfig) (string, error) {
	return s.callAIWithProvider(0, prompt, config, "")
}

// GenerateWithVision 使用 Vision AI 分析图像内容
// 优先使用 anthropic（claude-3），其次 openai（gpt-4o），都失败则用默认 provider
func (s *AIService) GenerateWithVision(prompt string, imageURLs []string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider
	var err error
	for _, name := range []string{"anthropic", "openai"} {
		provider, err = s.aiManager.GetProvider(name)
		if err == nil {
			break
		}
	}
	if err != nil {
		provider, err = s.aiManager.GetProvider("")
		if err != nil {
			return "", fmt.Errorf("failed to get AI provider for vision: %w", err)
		}
	}

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{
			{
				Role:      "user",
				Content:   prompt,
				ImageURLs: imageURLs,
			},
		},
		MaxTokens:   512,
		Temperature: 0.1,
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	return resp.Content, nil
}

// callAIWithProvider 调用指定 Provider 的 AI 接口
func (s *AIService) callAIWithProvider(tenantID uint, prompt string, config *model.TaskModelConfig, providerName string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		log.Printf("callAIWithProvider: getTenantProvider failed (tenant=%d, provider=%q): %v", tenantID, providerName, err)
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}

	req := &ai.GenerateRequest{
		Messages:    []ai.ChatMessage{{Role: "user", Content: prompt}},
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
		TopP:        config.TopP,
	}
	// Claude 不支持 top_k，仅在非 Anthropic provider 时传入
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}
	// Stop sequences 仅 OpenAI 支持，其他 provider 忽略
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}

	return resp.Content, nil
}

// generateWithRetry 带容错重试的 JSON 生成（最多重试 2 次）
func (s *AIService) generateWithRetry(novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			log.Printf("generateWithRetry: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.Generate(novelID, taskType, p)
		if err != nil {
			lastErr = err
			continue
		}
		// 尝试提取 JSON
		cleaned := extractJSON(result)
		// 验证是否为有效 JSON
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		log.Printf("generateWithRetry: %v", lastErr)
	}
	return "", fmt.Errorf("generateWithRetry failed after %d attempts: %w", maxRetries+1, lastErr)
}

// extractJSON 从 AI 输出中提取纯 JSON 字符串
func extractJSON(content string) string {
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "```json"); idx != -1 {
		content = content[idx+7:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	} else if idx := strings.Index(content, "```"); idx != -1 {
		content = content[idx+3:]
		if end := strings.Index(content, "```"); end != -1 {
			content = content[:end]
		}
	}
	content = strings.TrimSpace(content)
	start := strings.IndexAny(content, "{[")
	if start == -1 {
		return content
	}
	openChar := content[start]
	closeChar := byte('}')
	if openChar == '[' {
		closeChar = ']'
	}
	depth := 0
	for i := start; i < len(content); i++ {
		if content[i] == openChar {
			depth++
		} else if content[i] == closeChar {
			depth--
			if depth == 0 {
				return content[start : i+1]
			}
		}
	}
	return content[start:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// logUsage 记录使用
func (s *AIService) logUsage(config *model.TaskModelConfig, prompt, result string) {
	inputTokens := countChineseChars(prompt)
	outputTokens := countChineseChars(result)

	log := &model.ModelUsageLog{
		ModelID:      config.PrimaryModelID,
		TaskType:     "generation",
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         float64(inputTokens+outputTokens) / 1000 * 0.01,
		Latency:      1.5,
		Success:      true,
	}

	s.modelRepo.LogUsage(log)
}

// GenerateImage 调用AI生成图像
func (s *AIService) GenerateImage(prompt string, options *ImageGenerationOptions) (*GeneratedImage, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.aiManager.GetProvider("")
	if err != nil {
		return nil, fmt.Errorf("get AI provider failed: %w", err)
	}
	req := &ai.ImageGenerateRequest{
		Prompt:         prompt,
		NegativePrompt: options.NegativePrompt,
		Size:           options.Size,
		Steps:          options.Steps,
		CFGScale:       options.CFGScale,
	}
	resp, err := provider.ImageGenerate(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return &GeneratedImage{
		URL:    resp.URL,
		Width:  resp.Width,
		Height: resp.Height,
	}, nil
}

// AudioGenerate 调用默认 AI provider 生成 TTS 音频，返回本地文件路径（file:// URL）
func (s *AIService) AudioGenerate(ctx context.Context, text, voice string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.aiManager.GetProvider("")
	if err != nil {
		return "", fmt.Errorf("get AI provider failed: %w", err)
	}
	if voice == "" {
		voice = "alloy"
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:  text,
		Voice: voice,
		Speed: 1.0,
	})
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

// QualityService 质量服务
type QualityService struct {
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
	characterRepo *repository.CharacterRepository
	aiService     *AIService
}

func NewQualityService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	aiService *AIService,
) *QualityService {
	return &QualityService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		characterRepo: characterRepo,
		aiService:     aiService,
	}
}

// CheckChapterQuality 检查章节质量
func (s *QualityService) CheckChapterQuality(chapterID uint) (*QualityReport, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, err
	}

	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, err
	}

	report := &QualityReport{
		Issues:      []QualityIssue{},
		Suggestions: []string{},
	}

	// 1. 检查角色一致性
	charIssues := s.checkCharacterConsistency(chapter, novel)
	report.Issues = append(report.Issues, charIssues...)

	// 2. 检查世界观一致性
	worldIssues := s.checkWorldviewConsistency(chapter, novel)
	report.Issues = append(report.Issues, worldIssues...)

	// 3. 检查重复性
	repetitionIssues := s.checkRepetition(chapter)
	report.Issues = append(report.Issues, repetitionIssues...)

	// 4. 计算整体评分（基于问题权重）
	highCount, mediumCount := 0, 0
	for _, issue := range report.Issues {
		switch issue.Severity {
		case "high":
			highCount++
		case "medium":
			mediumCount++
		}
	}
	report.OverallScore = 1.0 - float64(highCount)*0.15 - float64(mediumCount)*0.05
	if report.OverallScore < 0 {
		report.OverallScore = 0
	}
	if report.OverallScore > 1.0 {
		report.OverallScore = 1.0
	}

	// 5. 生成建议
	report.Suggestions = s.generateSuggestions(report.Issues)

	return report, nil
}

// checkCharacterConsistency 检查角色一致性（基于文本规则）
func (s *QualityService) checkCharacterConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	characters, _ := s.characterRepo.ListByNovel(novel.ID)
	if len(characters) == 0 {
		return issues
	}

	// 检查主要角色在章节中的出现次数
	for _, char := range characters {
		nameCount := strings.Count(chapter.Content, char.Name)
		if nameCount == 0 && len(chapter.Content) > 1000 {
			issues = append(issues, QualityIssue{
				Type:        "character_consistency",
				Severity:    "low",
				Description: fmt.Sprintf("主要角色「%s」在本章未出现，可能影响剧情连贯", char.Name),
				Location:    "全文",
				Suggestion:  fmt.Sprintf("确认角色「%s」是否应在本章出场", char.Name),
			})
		}
	}

	return issues
}

// checkWorldviewConsistency 检查世界观一致性（基于文本规则）
func (s *QualityService) checkWorldviewConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	if novel.Worldview == nil {
		return issues
	}

	// 检查世界观关键词是否与内容一致
	if novel.Worldview.MagicSystem != "" {
		keywords := strings.Fields(novel.Worldview.MagicSystem)
		foundAny := false
		for _, kw := range keywords {
			if len(kw) >= 2 && strings.Contains(chapter.Content, kw) {
				foundAny = true
				break
			}
		}
		if !foundAny && len(chapter.Content) > 2000 {
			issues = append(issues, QualityIssue{
				Type:        "worldview_consistency",
				Severity:    "low",
				Description: "本章未提及修炼/魔法体系相关内容，可能导致世界观缺失感",
				Location:    "全文",
				Suggestion:  "建议在适当位置融入世界观设定元素",
			})
		}
	}

	return issues
}

// checkRepetition 检查重复性
func (s *QualityService) checkRepetition(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}

	// 检查重复词汇
	words := []string{"突然", "然后", "接着"}
	for _, word := range words {
		count := strings.Count(chapter.Content, word)
		if count > 5 {
			issues = append(issues, QualityIssue{
				Type:        "repetition",
				Severity:    "low",
				Description: fmt.Sprintf("「%s」一词出现了%d次", word, count),
				Location:    "全文",
				Suggestion:  "建议使用同义词替换以增加表达多样性",
			})
		}
	}

	return issues
}

// generateSuggestions 生成建议
func (s *QualityService) generateSuggestions(issues []QualityIssue) []string {
	suggestions := []string{}

	highCount := 0
	for _, issue := range issues {
		if issue.Severity == "high" {
			highCount++
		}
	}

	if highCount > 0 {
		suggestions = append(suggestions, fmt.Sprintf("有%d个高优先级问题需要修复", highCount))
	}

	if len(issues) > 10 {
		suggestions = append(suggestions, "章节存在较多问题，建议整体重写或大幅修改")
	}

	if len(suggestions) == 0 {
		suggestions = append(suggestions, "章节质量良好，无需特别修改")
	}

	return suggestions
}

// VideoService 视频服务
type VideoService struct {
	videoRepo           *repository.VideoRepository
	storyboardRepo      *repository.StoryboardRepository
	chapterRepo         *repository.ChapterRepository
	characterRepo       *repository.CharacterRepository
	novelRepo           *repository.NovelRepository
	tenantRepo          *repository.TenantRepository
	aiService           *AIService
	videoProviders      map[string]ai.VideoProvider
	consistencyService  *CharacterConsistencyService
	bgmService          *BGMService
}

func NewVideoService(
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	novelRepo *repository.NovelRepository,
	tenantRepo *repository.TenantRepository,
	aiService *AIService,
	videoProviders map[string]ai.VideoProvider,
) *VideoService {
	return &VideoService{
		videoRepo:      videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		characterRepo:  characterRepo,
		novelRepo:      novelRepo,
		tenantRepo:     tenantRepo,
		aiService:      aiService,
		videoProviders: videoProviders,
	}
}

// WithConsistencyService 设置一致性服务（选填）
func (s *VideoService) WithConsistencyService(cs *CharacterConsistencyService) *VideoService {
	s.consistencyService = cs
	return s
}

// WithBGMService 设置BGM服务（选填）
func (s *VideoService) WithBGMService(bgm *BGMService) *VideoService {
	s.bgmService = bgm
	return s
}

// CreateVideoFromChapter 从章节创建视频
func (s *VideoService) CreateVideoFromChapter(novelID uint, chapterID *uint) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   chapterID,
		Title:       "新视频",
		Status:      "planning",
		FrameRate:   24,
		Resolution:  "1080p",
		AspectRatio: "16:9",
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// CreateVideo 创建视频（接受请求对象）
func (s *VideoService) CreateVideo(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	return s.CreateVideoFromReq(novelID, req)
}

// GenerateStoryboard 生成分镜
func (s *VideoService) GenerateStoryboard(videoID uint, chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}

	// 允许调用方覆盖 chapterID（解决 StoryboardService 忽略 chapterID 的问题）
	chapterID := video.ChapterID
	if len(chapterIDOverride) > 0 && chapterIDOverride[0] != nil {
		chapterID = chapterIDOverride[0]
		// 同步更新 video 记录，保持一致性
		video.ChapterID = chapterID
		s.videoRepo.Update(video) //nolint:errcheck
	}

	var content string
	if chapterID != nil {
		chapter, _ := s.chapterRepo.GetByID(*chapterID)
		if chapter != nil {
			content = chapter.Content
		}
	}

	// 构建分镜提示词
	prompt := s.buildStoryboardPrompt(video, content)

	// 调用AI生成分镜
	result, err := s.aiService.Generate(video.NovelID, "storyboard", prompt)
	if err != nil {
		return nil, err
	}

	// 解析分镜（传入 chapterID，供每个 shot 继承）
	shots := s.parseStoryboardResult(videoID, chapterID, result)

	// 保存分镜
	for _, shot := range shots {
		if err := s.storyboardRepo.Create(shot); err != nil {
			return nil, err
		}
	}

	// 更新视频状态
	video.TotalShots = len(shots)
	video.Status = "storyboard"
	s.videoRepo.Update(video)

	return shots, nil
}

// buildStoryboardPrompt 构建分镜提示词（含截断保护和角色信息）
func (s *VideoService) buildStoryboardPrompt(video *model.Video, content string) string {
	var sb strings.Builder

	sb.WriteString("你是一名专业分镜师。请根据以下内容生成分镜脚本，以JSON数组格式返回。\n\n")

	// 注入角色视觉信息（portrait/外貌）
	if video.NovelID > 0 {
		characters, _ := s.characterRepo.ListByNovel(video.NovelID)
		if len(characters) > 0 {
			sb.WriteString("【角色信息】\n")
			for _, c := range characters {
				sb.WriteString(fmt.Sprintf("- %s（%s）", c.Name, c.Role))
				if c.Appearance != "" {
					sb.WriteString(fmt.Sprintf("：%s", c.Appearance))
				}
				if c.Portrait != "" {
					sb.WriteString(fmt.Sprintf("，参考图：%s", c.Portrait))
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	if content != "" {
		sb.WriteString("【章节内容】\n")
		// 截断保护：最多 6000 字符
		if len(content) > 6000 {
			content = content[:6000] + "…（已截断）"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	sb.WriteString(`请将内容分解为若干分镜（5-15个），以JSON数组返回，格式如下：
[
  {
    "shot_no": 1,
    "description": "场景描述（英文，用于图像生成）",
    "dialogue": "对话内容（如无则留空）",
    "camera_type": "static|pan|zoom|tracking|dolly|crane",
    "camera_angle": "eye_level|high|low|dutch|overhead|POV",
    "shot_size": "extreme_wide|wide|full|medium|close_up|extreme_close_up",
    "duration": 5.0,
    "location": "场景地点",
    "time_of_day": "dawn|morning|afternoon|evening|night",
    "weather": "clear|cloudy|rainy|snowy|foggy",
    "lighting": "natural|dramatic|soft|backlit",
    "characters": [{"name":"角色名","expression":"表情","pose":"动作姿势"}],
    "transition": "cut|fade|dissolve|wipe"
  }
]
只返回JSON数组，不要任何额外说明。`)

	return sb.String()
}

// parseStoryboardResult 解析AI分镜响应，失败时生成基础默认分镜
func (s *VideoService) parseStoryboardResult(videoID uint, chapterID *uint, result string) []*model.StoryboardShot {
	// 提取 JSON 数组
	cleaned := extractJSON(result)

	var rawShots []struct {
		ShotNo      int     `json:"shot_no"`
		Description string  `json:"description"`
		Dialogue    string  `json:"dialogue"`
		CameraType  string  `json:"camera_type"`
		CameraAngle string  `json:"camera_angle"`
		ShotSize    string  `json:"shot_size"`
		Duration    float64 `json:"duration"`
		Location    string  `json:"location"`
		TimeOfDay   string  `json:"time_of_day"`
		Weather     string  `json:"weather"`
		Lighting    string  `json:"lighting"`
		Characters  []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
			Pose       string `json:"pose"`
		} `json:"characters"`
		Transition string `json:"transition"`
	}

	if err := json.Unmarshal([]byte(cleaned), &rawShots); err != nil || len(rawShots) == 0 {
		// 解析失败时生成基础占位分镜（5个）
		log.Printf("parseStoryboardResult: JSON parse failed (%v), using fallback", err)
		shots := make([]*model.StoryboardShot, 5)
		for i := range shots {
			shots[i] = &model.StoryboardShot{
				UUID:        uuid.New().String(),
				VideoID:     videoID,
				ChapterID:   chapterID,
				ShotNo:      i + 1,
				CameraType:  "static",
				CameraAngle: "eye_level",
				ShotSize:    "medium",
				Duration:    5.0,
				Status:      "pending",
			}
		}
		return shots
	}

	shots := make([]*model.StoryboardShot, 0, len(rawShots))
	for i, r := range rawShots {
		shotNo := r.ShotNo
		if shotNo == 0 {
			shotNo = i + 1
		}
		duration := r.Duration
		if duration <= 0 {
			duration = 5.0
		}

		// 将角色信息序列化为 JSON 存储
		var charsJSON string
		if len(r.Characters) > 0 {
			if b, err := json.Marshal(r.Characters); err == nil {
				charsJSON = string(b)
			}
		}

		// 将场景配置序列化
		scene := map[string]string{
			"location":    r.Location,
			"time_of_day": r.TimeOfDay,
			"weather":     r.Weather,
			"lighting":    r.Lighting,
		}
		var sceneJSON string
		if b, err := json.Marshal(scene); err == nil {
			sceneJSON = string(b)
		}

		// Prompt 用 Description 填充（供视频生成接口使用）
		// 附加摄像机和场景信息以丰富 prompt
		prompt := r.Description
		if r.CameraAngle != "" || r.ShotSize != "" {
			prompt = fmt.Sprintf("%s, %s shot, %s angle", r.Description, r.ShotSize, r.CameraAngle)
		}
		if r.Lighting != "" {
			prompt += ", " + r.Lighting + " lighting"
		}

		shot := &model.StoryboardShot{
			UUID:        uuid.New().String(),
			VideoID:     videoID,
			ChapterID:   chapterID,
			ShotNo:      shotNo,
			Description: r.Description,
			Prompt:      prompt,
			Dialogue:    r.Dialogue,
			CameraType:  validCameraType(r.CameraType),
			CameraAngle: validCameraAngle(r.CameraAngle),
			ShotSize:    validShotSize(r.ShotSize),
			Duration:    duration,
			Characters:  charsJSON,
			Scene:       sceneJSON,
			Status:      "pending",
		}
		shots = append(shots, shot)
	}
	return shots
}

// validCameraType 验证摄像机类型，无效时返回默认值
func validCameraType(t string) string {
	valid := map[string]bool{"static": true, "pan": true, "zoom": true, "tracking": true, "dolly": true, "crane": true}
	if valid[t] {
		return t
	}
	return "static"
}

// validCameraAngle 验证摄像机角度
func validCameraAngle(a string) string {
	valid := map[string]bool{"eye_level": true, "high": true, "low": true, "dutch": true, "overhead": true, "POV": true}
	if valid[a] {
		return a
	}
	return "eye_level"
}

// validShotSize 验证镜头尺寸
func validShotSize(s string) string {
	valid := map[string]bool{"extreme_wide": true, "wide": true, "full": true, "medium": true, "close_up": true, "extreme_close_up": true}
	if valid[s] {
		return s
	}
	return "medium"
}

// CreateVideo handler-compatible wrapper
func (s *VideoService) CreateVideoFromReq(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   req.ChapterID,
		Title:       req.Title,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:    req.ArtStyle,
		QualityTier: req.QualityTier,
		Status:      "planning",
	}
	if video.FrameRate == 0 {
		video.FrameRate = 24
	}
	if video.Resolution == "" {
		video.Resolution = "1080p"
	}
	if video.AspectRatio == "" {
		video.AspectRatio = "16:9"
	}
	if video.QualityTier == "" {
		video.QualityTier = "preview"
	}
	return video, s.videoRepo.Create(video)
}

// GetVideo 获取视频
func (s *VideoService) GetVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetByID(id)
}

// ListVideos 获取视频列表
func (s *VideoService) ListVideos(novelId *uint, status string, page, pageSize int) ([]*model.Video, int, error) {
	videos, total, err := s.videoRepo.List(novelId, page, pageSize)
	return videos, int(total), err
}

// UpdateVideo 更新视频
func (s *VideoService) UpdateVideo(id uint, req *model.UpdateVideoRequest) (*model.Video, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		video.Title = req.Title
	}
	if req.Resolution != "" {
		video.Resolution = req.Resolution
	}
	if req.FrameRate != 0 {
		video.FrameRate = req.FrameRate
	}
	if req.AspectRatio != "" {
		video.AspectRatio = req.AspectRatio
	}
	if req.ArtStyle != "" {
		video.ArtStyle = req.ArtStyle
	}
	return video, s.videoRepo.Update(video)
}

// DeleteVideo 删除视频
func (s *VideoService) DeleteVideo(id uint) error {
	return s.videoRepo.DeleteByID(id)
}

// StartGeneration 开始生成视频（调用真实视频 Provider）
func (s *VideoService) StartGeneration(id uint) (string, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return "", err
	}

	// 租户状态校验
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		s.videoRepo.Update(video)
		return "", err
	}

	// 选择 provider：优先 kling，其次 seedance，均无则返回错误
	providerName := "kling"
	provider, ok := s.videoProviders[providerName]
	if !ok {
		providerName = "seedance"
		provider, ok = s.videoProviders[providerName]
	}
	if !ok {
		// 无可用 provider：标记失败并返回错误
		video.Status = "failed"
		video.ErrorMessage = "no video provider configured (set KLING_API_KEY or SEEDANCE_API_KEY)"
		s.videoRepo.Update(video)
		return "", fmt.Errorf("no video provider configured")
	}

	// 构建生成请求
	req := &ai.VideoGenerateRequest{
		Prompt:      fmt.Sprintf("%s — cinematic, high quality", video.Title),
		AspectRatio: video.AspectRatio,
		Duration:    5.0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		video.RetryCount++
		s.videoRepo.Update(video)
		return "", fmt.Errorf("video generation start failed: %w", err)
	}

	// 持久化任务 ID 和 provider 信息
	video.Status = "generating"
	video.ProviderName = providerName
	video.TaskID = task.TaskID
	video.ErrorMessage = ""
	s.videoRepo.Update(video)

	return task.TaskID, nil
}

// GetStoryboard 获取分镜列表
func (s *VideoService) GetStoryboard(videoID uint) ([]*model.StoryboardShot, error) {
	return s.storyboardRepo.ListByVideo(videoID)
}

// UpdateShot 更新分镜
func (s *VideoService) UpdateShot(id uint, req *model.StoryboardShot) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.CameraType != "" {
		shot.CameraType = req.CameraType
	}
	if req.CameraAngle != "" {
		shot.CameraAngle = req.CameraAngle
	}
	if req.ShotSize != "" {
		shot.ShotSize = req.ShotSize
	}
	if req.Duration > 0 {
		shot.Duration = req.Duration
	}
	if req.Status != "" {
		shot.Status = req.Status
	}
	if req.GenerationMode != "" {
		shot.GenerationMode = req.GenerationMode
	}
	return shot, s.storyboardRepo.Update(shot)
}

// GenerateSingleShot 触发单个分镜生成（异步）
func (s *VideoService) GenerateSingleShot(videoID, shotID uint) (*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}
	shot.Status = "generating"
	s.storyboardRepo.Update(shot) //nolint:errcheck
	go func() {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			log.Printf("GenerateSingleShot: shot %d failed: %v", shot.ShotNo, err)
		}
	}()
	return shot, nil
}

// BatchGenerateShots 批量触发指定分镜生成（异步）
func (s *VideoService) BatchGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	if qualityTierOverride != "" {
		video.QualityTier = qualityTierOverride
	}
	var queued []*model.StoryboardShot
	for _, sid := range shotIDs {
		shot, err := s.storyboardRepo.GetByID(sid)
		if err != nil || shot.VideoID != videoID {
			continue
		}
		shot.Status = "generating"
		s.storyboardRepo.Update(shot) //nolint:errcheck
		queued = append(queued, shot)
		go func(sh *model.StoryboardShot) {
			if err := s.GenerateShotVideo(sh, video.AspectRatio); err != nil {
				log.Printf("BatchGenerateShots: shot %d failed: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	return queued, nil
}

// GetStatus 获取视频生成状态（从 provider 同步最新进度）
func (s *VideoService) GetStatus(id uint) (interface{}, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	// 超时检查：生成中超过 30 分钟
	if video.Status == "generating" && time.Since(video.UpdatedAt) > 30*time.Minute {
		video.Status = "failed"
		video.ErrorMessage = "generation timed out (>30min)"
		s.videoRepo.Update(video)
	}

	// 自动重试：失败且重试次数 < 3
	if video.Status == "failed" && video.RetryCount < 3 && video.TaskID != "" {
		video.RetryCount++
		video.Status = "generating"
		video.ErrorMessage = ""
		s.videoRepo.Update(video)
		go func() { s.StartGeneration(id) }() //nolint:errcheck
	}

	// 如果有外部任务 ID 且状态为 generating，则同步 provider 状态
	if video.TaskID != "" && video.Status == "generating" && video.ProviderName != "" {
		if provider, ok := s.videoProviders[video.ProviderName]; ok {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			taskStatus, err := provider.GetVideoStatus(ctx, video.TaskID)
			if err == nil {
				video.Progress = taskStatus.Progress
				switch taskStatus.Status {
				case "completed":
					// 获取视频 URL
					if videoURL, urlErr := provider.GetVideoURL(ctx, video.TaskID); urlErr == nil {
						video.VideoPath = videoURL
						video.Status = "completed"
					}
				case "failed":
					video.Status = "failed"
					video.ErrorMessage = taskStatus.Error
				}
				s.videoRepo.Update(video)
			}
		}
	}

	return map[string]interface{}{
		"status":        video.Status,
		"progress":      video.Progress,
		"task_id":       video.TaskID,
		"provider":      video.ProviderName,
		"error_message": video.ErrorMessage,
		"video_url":     video.VideoPath,
	}, nil
}

// checkTenantAccess 校验 novel 关联租户状态
func (s *VideoService) checkTenantAccess(novelID uint) error {
	if s.tenantRepo == nil || s.novelRepo == nil {
		return nil
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil // 不阻塞，让其他逻辑处理
	}
	tenant, err := s.tenantRepo.GetByID(novel.TenantID)
	if err != nil {
		return nil
	}
	if tenant.Status != "active" {
		return fmt.Errorf("tenant account is %s", tenant.Status)
	}
	if !tenant.ExpiresAt.IsZero() && time.Now().After(tenant.ExpiresAt) {
		return fmt.Errorf("tenant account has expired")
	}
	return nil
}

// generateShotReferenceImage 为分镜生成参考帧图像（非致命：失败时返回空字符串）
func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) string {
	if s.aiService == nil {
		return ""
	}

	// 通过 ChapterID 找到 NovelID，然后取第一个有肖像的角色作为 IP-Adapter 参考
	var characterPortrait string
	if shot.ChapterID != nil {
		chapter, err := s.chapterRepo.GetByID(*shot.ChapterID)
		if err == nil && chapter != nil {
			chars, err := s.characterRepo.ListByNovel(chapter.NovelID)
			if err == nil {
				for _, c := range chars {
					if c.Portrait != "" {
						characterPortrait = c.Portrait
						break
					}
				}
			}
		}
	}

	promptText := shot.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	imgReq := &ai.ImageGenerateRequest{
		Prompt:         promptText,
		NegativePrompt: shot.NegativePrompt,
		Size:           "1280x720",
		CFGScale:       7.0,
		ReferenceImage: characterPortrait, // IP-Adapter 参考
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	provider, err := s.aiService.aiManager.GetProvider("")
	if err != nil {
		log.Printf("generateShotReferenceImage: get provider failed: %v", err)
		return ""
	}

	resp, err := provider.ImageGenerate(ctx, imgReq)
	if err != nil || resp == nil || resp.URL == "" {
		log.Printf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}

	return resp.URL
}

// extractLastFrame 使用 FFmpeg 提取视频最后一帧，返回本地 jpeg 路径
func (s *VideoService) extractLastFrame(clipPath string) (string, error) {
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(clipPath, "file://")

	tmpJpeg := fmt.Sprintf("/tmp/inkframe-lastframe-%d.jpg", time.Now().UnixNano())
	cmd := exec.Command("ffmpeg", "-y",
		"-sseof", "-0.1",
		"-i", localPath,
		"-vframes", "1",
		"-f", "image2",
		tmpJpeg,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extractLastFrame failed: %w\noutput: %s", err, string(out))
	}
	return tmpJpeg, nil
}

// GenerateShotVideo 为单个分镜提交视频生成任务
func (s *VideoService) GenerateShotVideo(shot *model.StoryboardShot, videoAspectRatio string) error {
	providerName := "kling"
	provider, ok := s.videoProviders[providerName]
	if !ok {
		providerName = "seedance"
		provider, ok = s.videoProviders[providerName]
	}
	if !ok {
		return fmt.Errorf("no video provider configured")
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	// 确定参考图：优先使用时序连贯的前镜最后一帧，其次生成新参考帧
	referenceImage := shot.ReferenceImageURL
	if referenceImage == "" {
		// 生成本镜参考帧（非致命）
		frameURL := s.generateShotReferenceImage(shot)
		if frameURL != "" {
			shot.FrameImageURL = frameURL
			referenceImage = frameURL
		}
	}

	req := &ai.VideoGenerateRequest{
		Prompt:         shot.Prompt,
		NegativePrompt: shot.NegativePrompt,
		Duration:       5,
		AspectRatio:    videoAspectRatio,
		ImageURL:       referenceImage, // image-to-video（空时退化为 text-to-video）
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	shot.ShotTaskID = task.TaskID
	shot.ShotProviderName = providerName
	shot.Status = "processing"
	return s.storyboardRepo.Update(shot)
}

// PollShotStatus 轮询单个分镜视频生成状态
func (s *VideoService) PollShotStatus(shot *model.StoryboardShot) error {
	// 超时检查
	if shot.Status == "processing" && time.Since(shot.UpdatedAt) > 30*time.Minute {
		shot.Status = "failed"
		shot.RetryCount++
		s.storyboardRepo.Update(shot) //nolint:errcheck
		return nil
	}

	provider, ok := s.videoProviders[shot.ShotProviderName]
	if !ok {
		return fmt.Errorf("provider %s not found", shot.ShotProviderName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	taskStatus, err := provider.GetVideoStatus(ctx, shot.ShotTaskID)
	if err != nil {
		return err
	}

	switch taskStatus.Status {
	case "completed", "succeed":
		videoURL, urlErr := provider.GetVideoURL(ctx, shot.ShotTaskID)
		if urlErr != nil {
			return urlErr
		}

		// 立即下载到本地，防止临时签名 URL 在拼接时过期
		localClip := fmt.Sprintf("/tmp/inkframe-shot-%d.mp4", shot.ID)
		if dlErr := downloadFile(videoURL, localClip); dlErr != nil {
			log.Printf("PollShotStatus: download shot %d clip failed (%v), storing URL as fallback", shot.ID, dlErr)
			shot.ClipPath = videoURL
		} else {
			shot.ClipPath = "file://" + localClip
		}
		shot.Status = "completed"

		// 一致性评分（可选）
		if s.consistencyService != nil && shot.ChapterID != nil {
			chapter, _ := s.chapterRepo.GetByID(*shot.ChapterID)
			if chapter != nil {
				chars, _ := s.characterRepo.ListByNovel(chapter.NovelID)
				for _, c := range chars {
					if c.Portrait != "" {
						score, scoreErr := s.consistencyService.CalculateConsistencyScore(c.Portrait, []string{videoURL})
						if scoreErr == nil {
							shot.ConsistencyScore = score.OverallScore
							// 一致性过低时自动重试
							if score.OverallScore < 0.5 && shot.RetryCount < 2 {
								shot.Status = "pending"
								shot.RetryCount++
								shot.ClipPath = ""
								shot.ConsistencyScore = 0
								shot.ShotTaskID = "" // 必须清除，否则重试不会重新提交
							}
						}
						break
					}
				}
			}
		}
		s.storyboardRepo.Update(shot) //nolint:errcheck

		// 时序连贯：提取本镜最后一帧，存入下一镜 ReferenceImageURL
		if shot.Status == "completed" && strings.HasPrefix(shot.ClipPath, "file://") {
			if lastFramePath, frameErr := s.extractLastFrame(shot.ClipPath); frameErr == nil {
				// 查找下一镜（同 VideoID，ShotNo+1）
				nextShots, listErr := s.storyboardRepo.ListByVideo(shot.VideoID)
				if listErr == nil {
					for _, ns := range nextShots {
						if ns.ShotNo == shot.ShotNo+1 && ns.ShotTaskID == "" {
							ns.ReferenceImageURL = "file://" + lastFramePath
							s.storyboardRepo.Update(ns) //nolint:errcheck
							break
						}
					}
				}
			} else {
				log.Printf("PollShotStatus: extractLastFrame for shot %d failed: %v", shot.ShotNo, frameErr)
			}
		}

	case "failed":
		shot.Status = "failed"
		shot.RetryCount++
		if shot.RetryCount < 2 {
			shot.Status = "pending"
			shot.ShotTaskID = ""
		}
		s.storyboardRepo.Update(shot) //nolint:errcheck
	}

	return nil
}

// GenerateShotAudio 为单个分镜生成 TTS 音频
func (s *VideoService) GenerateShotAudio(shot *model.StoryboardShot) error {
	if shot.Dialogue == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	audioURL, err := s.aiService.AudioGenerate(ctx, shot.Dialogue, "alloy")
	if err != nil {
		log.Printf("GenerateShotAudio: shot %d failed: %v", shot.ID, err)
		return nil // non-fatal
	}
	if audioURL != "" {
		shot.AudioPath = audioURL
		s.storyboardRepo.Update(shot) //nolint:errcheck
	}
	return nil
}

// downloadFile 下载 HTTP URL 到本地路径
func downloadFile(url, dest string) error {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// StitchVideo 将所有 completed 分镜拼接为最终视频
func (s *VideoService) StitchVideo(videoID uint) (string, error) {
	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "completed")
	if err != nil {
		return "", err
	}
	if len(shots) == 0 {
		return "", fmt.Errorf("no completed shots to stitch")
	}

	tmpDir := fmt.Sprintf("/tmp/inkframe-%d", videoID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var localShotFiles []string // 记录 PollShotStatus 下载的本地文件，拼接后清理
	defer func() {
		for _, f := range localShotFiles {
			os.Remove(f) //nolint:errcheck
		}
	}()
	var concatLines []string
	for i, shot := range shots {
		clipFile := fmt.Sprintf("%s/clip_%d.mp4", tmpDir, i)
		finalClip := clipFile

		// 如果已是本地文件（PollShotStatus 立即下载过），直接使用，无需再下载
		if strings.HasPrefix(shot.ClipPath, "file://") {
			clipFile = strings.TrimPrefix(shot.ClipPath, "file://")
			finalClip = clipFile
			localShotFiles = append(localShotFiles, clipFile)
		} else {
			// 仍是远程 URL（fallback），下载到 tmpDir
			if err := downloadFile(shot.ClipPath, clipFile); err != nil {
				// URL 可能已过期，尝试从 provider 重新获取
				if shot.ShotTaskID != "" && shot.ShotProviderName != "" {
					if p, ok := s.videoProviders[shot.ShotProviderName]; ok {
						rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
						freshURL, fErr := p.GetVideoURL(rCtx, shot.ShotTaskID)
						rCancel()
						if fErr == nil {
							if err2 := downloadFile(freshURL, clipFile); err2 != nil {
								return "", fmt.Errorf("download shot %d clip failed (fresh URL also failed): %w", shot.ShotNo, err2)
							}
						} else {
							return "", fmt.Errorf("download shot %d clip failed and refresh URL failed: %w", shot.ShotNo, err)
						}
					} else {
						return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
					}
				} else {
					return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
				}
			}
		}

		// Merge audio if present
		if shot.AudioPath != "" {
			audioPath := strings.TrimPrefix(shot.AudioPath, "file://")
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			cmd := exec.Command("ffmpeg", "-y",
				"-i", clipFile,
				"-i", audioPath,
				"-c:v", "copy",
				"-c:a", "aac",
				"-shortest",
				mergedFile,
			)
			if err := cmd.Run(); err != nil {
				log.Printf("StitchVideo: merge audio for shot %d failed: %v, using clip without audio", shot.ShotNo, err)
			} else {
				finalClip = mergedFile
			}
		}

		concatLines = append(concatLines, fmt.Sprintf("file '%s'", finalClip))
	}

	listFile := fmt.Sprintf("%s/list.txt", tmpDir)
	if err := os.WriteFile(listFile, []byte(strings.Join(concatLines, "\n")), 0644); err != nil {
		return "", err
	}

	stitchedPath := fmt.Sprintf("/tmp/inkframe-%d-stitched.mp4", videoID)
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		stitchedPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg stitch failed: %w\noutput: %s", err, string(out))
	}

	// BGM 混音（非致命：失败时使用无BGM版本）
	outputPath := fmt.Sprintf("/tmp/inkframe-%d-output.mp4", videoID)
	if s.bgmService != nil {
		bgmURL := s.bgmService.SelectBGM("")
		if bgmURL != "" {
			if mixErr := s.bgmService.MixBGM(stitchedPath, bgmURL, outputPath); mixErr != nil {
				log.Printf("StitchVideo: BGM mixing failed (video %d): %v, using stitched without BGM", videoID, mixErr)
				outputPath = stitchedPath
			}
		} else {
			outputPath = stitchedPath
		}
	} else {
		outputPath = stitchedPath
	}

	// Update video record
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return outputPath, nil
	}
	video.VideoPath = outputPath
	video.Status = "completed"
	s.videoRepo.Update(video) //nolint:errcheck

	return outputPath, nil
}

// GenerateAllShotVideos 提交所有待生成分镜的视频任务
func (s *VideoService) GenerateAllShotVideos(videoID uint) error {
	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil {
		return err
	}
	if len(shots) == 0 {
		return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
	}

	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	// 更新状态，让用户可以通过 GetStatus 感知进度
	video.Status = "generating"
	video.ErrorMessage = ""
	s.videoRepo.Update(video) //nolint:errcheck

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			log.Printf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(shot) //nolint:errcheck
	}
	return nil
}

// PollAndStitchVideo 后台轮询所有分镜状态，完成后拼接
func (s *VideoService) PollAndStitchVideo(videoID uint) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Hour)
	noProgressCount := 0

	for {
		if time.Now().After(deadline) {
			log.Printf("PollAndStitchVideo: videoID %d timed out after 2h", videoID)
			video, _ := s.videoRepo.GetByID(videoID)
			if video != nil && video.Status != "completed" {
				video.Status = "failed"
				video.ErrorMessage = "stitch pipeline timed out (>2h)"
				s.videoRepo.Update(video) //nolint:errcheck
			}
			return
		}

		<-ticker.C

		// Retry pending shots (from consistency/failed retry)
		pending, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		for _, shot := range pending {
			if shot.ShotTaskID == "" {
				video, _ := s.videoRepo.GetByID(videoID)
				aspectRatio := "16:9"
				if video != nil {
					aspectRatio = video.AspectRatio
				}
				s.GenerateShotVideo(shot, aspectRatio) //nolint:errcheck
			}
		}

		// Poll processing shots
		processing, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		for _, shot := range processing {
			s.PollShotStatus(shot) //nolint:errcheck
		}

		// Check if all completed
		allShots, _ := s.storyboardRepo.ListByVideo(videoID)
		if len(allShots) == 0 {
			continue
		}
		completedCount := 0
		failedCount := 0
		for _, shot := range allShots {
			switch shot.Status {
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			}
		}

		if completedCount+failedCount == len(allShots) {
			if completedCount > 0 {
				if _, err := s.StitchVideo(videoID); err != nil {
					log.Printf("PollAndStitchVideo: stitch failed: %v", err)
					video, _ := s.videoRepo.GetByID(videoID)
					if video != nil {
						video.Status = "failed"
						video.ErrorMessage = err.Error()
						s.videoRepo.Update(video) //nolint:errcheck
					}
				}
			} else {
				video, _ := s.videoRepo.GetByID(videoID)
				if video != nil {
					video.Status = "failed"
					video.ErrorMessage = "all shots failed"
					s.videoRepo.Update(video) //nolint:errcheck
				}
			}
			return
		}

		// Stall detection (no progress after 5 ticks): re-query to get fresh counts
		processingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		pendingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if len(processingNow) == 0 && len(pendingNow) == 0 {
			noProgressCount++
			if noProgressCount >= 5 {
				log.Printf("PollAndStitchVideo: videoID %d stalled, stopping", videoID)
				return
			}
		} else {
			noProgressCount = 0
		}
	}
}

// ModelService 模型服务
type ModelService struct {
	modelRepo      *repository.AIModelRepository
	providerRepo   *repository.ModelProviderRepository
	taskRepo       *repository.TaskModelConfigRepository
	experimentRepo *repository.ModelComparisonRepository
	aiService      *AIService
}

func NewModelService(
	modelRepo *repository.AIModelRepository,
	providerRepo *repository.ModelProviderRepository,
	taskRepo *repository.TaskModelConfigRepository,
	experimentRepo *repository.ModelComparisonRepository,
	aiService ...*AIService,
) *ModelService {
	svc := &ModelService{
		modelRepo:      modelRepo,
		providerRepo:   providerRepo,
		taskRepo:       taskRepo,
		experimentRepo: experimentRepo,
	}
	if len(aiService) > 0 {
		svc.aiService = aiService[0]
	}
	return svc
}

func selectByQuality(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		score := m.Quality
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}

func selectByCost(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestCost := 999999.0

	for _, m := range models {
		if m.CostPer1K < bestCost {
			bestCost = m.CostPer1K
			best = m
		}
	}

	return best
}

func selectBalanced(models []*model.AIModel) *model.AIModel {
	var best *model.AIModel
	bestScore := 0.0

	for _, m := range models {
		// 质量/成本比
		score := m.Quality / m.CostPer1K
		if score > bestScore {
			bestScore = score
			best = m
		}
	}

	return best
}
