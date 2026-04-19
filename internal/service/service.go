package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// NovelService 小说服务
type NovelService struct {
	novelRepo   *repository.NovelRepository
	chapterRepo *repository.ChapterRepository
	aiService  *AIService
}

func NewNovelService(
	novelRepo *repository.NovelRepository,
	chapterRepo *repository.ChapterRepository,
	aiService *AIService,
) *NovelService {
	return &NovelService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
		aiService:  aiService,
	}
}

// CreateNovelRequest 创建小说请求
type CreateNovelRequest struct {
	Title       string  `json:"title" binding:"required"`
	Description string  `json:"description"`
	Genre       string  `json:"genre" binding:"required"`
	WorldviewID *uint   `json:"worldview_id"`
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

// ListNovels 获取小说列表
func (s *NovelService) ListNovels(page, pageSize int, filters map[string]interface{}) ([]*model.Novel, int64, error) {
	return s.novelRepo.List(page, pageSize, filters)
}

// UpdateNovel 更新小说
func (s *NovelService) UpdateNovel(novel *model.Novel) error {
	return s.novelRepo.Update(novel)
}

// DeleteNovel 删除小说
func (s *NovelService) DeleteNovel(id uint) error {
	return s.novelRepo.Delete(id)
}

// GenerateOutlineRequest 生成大纲请求
type GenerateOutlineRequest struct {
	NovelID     uint     `json:"novel_id" binding:"required"`
	Prompt     string   `json:"prompt"`
	ChapterNum int      `json:"chapter_num" binding:"required"`
	Keywords   []string `json:"keywords"`
}

// GenerateOutline 生成大纲
func (s *NovelService) GenerateOutline(req *GenerateOutlineRequest) (*OutlineResult, error) {
	novel, err := s.novelRepo.GetByID(req.NovelID)
	if err != nil {
		return nil, err
	}

	// 构建提示词
	prompt := s.buildOutlinePrompt(novel, req)

	// 调用AI生成
	result, err := s.aiService.Generate(req.NovelID, "outline", prompt)
	if err != nil {
		return nil, err
	}

	// 解析结果
	outline := &OutlineResult{}
	if err := json.Unmarshal([]byte(result), outline); err != nil {
		// 如果解析失败，返回原始文本
		outline = &OutlineResult{
			Title:    novel.Title,
			Chapters: []ChapterOutline{},
		}
	}

	return outline, nil
}

// OutlineResult 大纲结果
type OutlineResult struct {
	Title    string           `json:"title"`
	Chapters []ChapterOutline `json:"chapters"`
}

// ChapterOutline 章节大纲
type ChapterOutline struct {
	ChapterNo int    `json:"chapter_no"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	WordCount int    `json:"word_count"`
	PlotPoints []string `json:"plot_points"`
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
	NovelID   uint    `json:"novel_id" binding:"required"`
	ChapterNo int     `json:"chapter_no" binding:"required"`
	Prompt    string  `json:"prompt"`
	MaxTokens int     `json:"max_tokens"`
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

	return chapter, nil
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
	chapters, _ := s.chapterRepo.ListByNovel(novelID)

	var totalWords int
	for _, ch := range chapters {
		totalWords += ch.WordCount
	}

	stats := map[string]interface{}{
		"chapter_count": len(chapters),
		"total_words":   totalWords,
	}

	if len(chapters) > 0 {
		stats["status"] = "writing"
	}

	s.novelRepo.Update(&model.Novel{ID: novelID})
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

// AIService AI服务
type AIService struct {
	modelRepo *repository.AIModelRepository
	taskRepo  *repository.TaskModelConfigRepository
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	taskRepo *repository.TaskModelConfigRepository,
) *AIService {
	return &AIService{
		modelRepo: modelRepo,
		taskRepo:  taskRepo,
	}
}

// Generate 生成内容
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	// 获取任务配置
	config, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		// 使用默认配置
		config = &model.TaskModelConfig{
			Temperature: 0.7,
			MaxTokens:   4096,
		}
	}

	// 模拟AI生成（实际应该调用真实API）
	result := s.mockAIGenerate(prompt)

	// 记录使用
	s.logUsage(config, prompt, result)

	return result, nil
}

// mockAIGenerate 模拟AI生成
func (s *AIService) mockAIGenerate(prompt string) string {
	// 这里应该是真实的API调用
	// 目前返回模拟数据
	time.Sleep(100 * time.Millisecond)

	// 根据提示词生成不同的模拟结果
	if strings.Contains(prompt, "生成一个详细的大纲") {
		return `{"title":"模拟小说标题","chapters":[{"chapter_no":1,"title":"意外的开始","summary":"主角意外穿越到异世界","word_count":2500,"plot_points":["主角穿越","发现异能","初遇伙伴"]}]}`
	}

	if strings.Contains(prompt, "概括以下内容") {
		return "这是一个精彩的故事章节，描述了主角在异世界的冒险经历。"
	}

	// 返回模拟章节内容
	return fmt.Sprintf("第X章\n\n这是一个由AI生成的章节内容。\n\n%s\n\n（正文内容约2000字）\n\n本章完。", prompt[:min(100, len(prompt))])
}

// logUsage 记录使用
func (s *AIService) logUsage(config *model.TaskModelConfig, prompt, result string) {
	inputTokens := countChineseChars(prompt)
	outputTokens := countChineseChars(result)

	log := &model.ModelUsageLog{
		ModelID:     config.PrimaryModelID,
		TaskType:    "generation",
		InputTokens: inputTokens,
		OutputTokens: outputTokens,
		TotalTokens: inputTokens + outputTokens,
		Cost:        float64(inputTokens+outputTokens) / 1000 * 0.01,
		Latency:     1.5,
		Success:     true,
	}

	s.modelRepo.LogUsage(log)
}

// min 返回较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// QualityService 质量服务
type QualityService struct {
	novelRepo    *repository.NovelRepository
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
		novelRepo:    novelRepo,
		chapterRepo: chapterRepo,
		characterRepo: characterRepo,
		aiService:   aiService,
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
		OverallScore: 0.85,
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

	// 4. 计算整体评分
	if len(report.Issues) > 0 {
		highCount := 0
		for _, issue := range report.Issues {
			if issue.Severity == "high" {
				highCount++
			}
		}
		report.OverallScore = 1.0 - float64(highCount)*0.1
		if report.OverallScore < 0 {
			report.OverallScore = 0
		}
	}

	// 5. 生成建议
	report.Suggestions = s.generateSuggestions(report.Issues)

	return report, nil
}

// checkCharacterConsistency 检查角色一致性
func (s *QualityService) checkCharacterConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	characters, _ := s.characterRepo.ListByNovel(novel.ID)
	if len(characters) == 0 {
		return issues
	}

	// 模拟检查（实际应该使用AI分析）
	// 这里简化处理
	for _, char := range characters {
		if len(chapter.Content) > 1000 && rand.Float32() < 0.1 {
			issues = append(issues, QualityIssue{
				Type:        "character_consistency",
				Severity:    "medium",
				Description: fmt.Sprintf("角色「%s」在章节中的表现可能与设定不符", char.Name),
				Location:    "第1段",
				Suggestion:  fmt.Sprintf("请检查角色「%s」的行为是否符合其性格设定", char.Name),
			})
		}
	}

	return issues
}

// checkWorldviewConsistency 检查世界观一致性
func (s *QualityService) checkWorldviewConsistency(chapter *model.Chapter, novel *model.Novel) []QualityIssue {
	issues := []QualityIssue{}

	if novel.Worldview == nil {
		return issues
	}

	// 模拟检查
	if rand.Float32() < 0.05 {
		issues = append(issues, QualityIssue{
			Type:        "worldview_consistency",
			Severity:    "low",
			Description: "可能存在轻微的世界观不一致",
			Location:    "第3段",
			Suggestion:  "请检查描述是否符合世界观设定",
		})
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
	videoRepo       *repository.VideoRepository
	storyboardRepo *repository.StoryboardRepository
	chapterRepo    *repository.ChapterRepository
	aiService      *AIService
}

func NewVideoService(
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
	chapterRepo *repository.ChapterRepository,
	aiService *AIService,
) *VideoService {
	return &VideoService{
		videoRepo:       videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		aiService:      aiService,
	}
}

// CreateVideo 创建视频
func (s *VideoService) CreateVideo(novelID uint, chapterID *uint) (*model.Video, error) {
	video := &model.Video{
		UUID:       uuid.New().String(),
		NovelID:    novelID,
		ChapterID:  chapterID,
		Title:      "新视频",
		Status:     "planning",
		FrameRate:  24,
		Resolution: "1080p",
		AspectRatio: "16:9",
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// GenerateStoryboard 生成分镜
func (s *VideoService) GenerateStoryboard(videoID uint) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}

	var content string
	if video.ChapterID != nil {
		chapter, _ := s.chapterRepo.GetByID(*video.ChapterID)
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

	// 解析分镜
	shots := s.parseStoryboardResult(videoID, result)

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

// buildStoryboardPrompt 构建分镜提示词
func (s *VideoService) buildStoryboardPrompt(video *model.Video, content string) string {
	var sb strings.Builder

	sb.WriteString("请根据以下内容生成分镜脚本：\n\n")

	if content != "" {
		sb.WriteString("【原始内容】\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	sb.WriteString("请将内容分解为多个分镜，每个分镜包括：\n")
	sb.WriteString("- 镜头编号和时长\n")
	sb.WriteString("- 场景描述\n")
	sb.WriteString("- 对话内容（如有）\n")
	sb.WriteString("- 摄像机类型（静态/平移/缩放/跟拍）\n")
	sb.WriteString("- 镜头尺寸（远景/中景/近景/特写）\n")
	sb.WriteString("- 涉及的角色的表情和动作\n\n")

	sb.WriteString("请以JSON格式返回分镜数组")

	return sb.String()
}

// parseStoryboardResult 解析分镜结果
func (s *VideoService) parseStoryboardResult(videoID uint, result string) []*model.StoryboardShot {
	shots := []*model.StoryboardShot{}

	// 简化解析（实际应该更复杂）
	for i := 1; i <= 5; i++ {
		shot := &model.StoryboardShot{
			UUID:       uuid.New().String(),
			VideoID:    videoID,
			ShotNo:     i,
			CameraType: "static",
			CameraAngle: "eye_level",
			ShotSize:   "medium",
			Duration:   5.0,
			Status:     "pending",
		}
		shots = append(shots, shot)
	}

	return shots
}

// ModelService 模型服务
type ModelService struct {
	modelRepo *repository.AIModelRepository
	providerRepo *repository.ModelProviderRepository
	taskRepo  *repository.TaskModelConfigRepository
	experimentRepo *repository.ModelComparisonRepository
}

func NewModelService(
	modelRepo *repository.AIModelRepository,
	providerRepo *repository.ModelProviderRepository,
	taskRepo *repository.TaskModelConfigRepository,
	experimentRepo *repository.ModelComparisonRepository,
) *ModelService {
	return &ModelService{
		modelRepo: modelRepo,
		providerRepo: providerRepo,
		taskRepo:  taskRepo,
		experimentRepo: experimentRepo,
	}
}

// ModelProviderRepository 模型提供商仓库
type ModelProviderRepository struct {
	db *gorm.DB
}

func NewModelProviderRepository(db *gorm.DB) *ModelProviderRepository {
	return &ModelProviderRepository{db: db}
}

// List 获取提供商列表
func (r *ModelProviderRepository) List() ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// Create 创建提供商
func (r *ModelProviderRepository) Create(provider *model.ModelProvider) error {
	return r.db.Create(provider).Error
}

// UpdateHealthStatus 更新健康状态
func (r *ModelProviderRepository) UpdateHealthStatus(id uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", id).
		Update("health_check", status).Error
}

// ModelComparisonRepository 模型对比仓库
type ModelComparisonRepository struct {
	db *gorm.DB
}

func NewModelComparisonRepository(db *gorm.DB) *ModelComparisonRepository {
	return &ModelComparisonRepository{db: db}
}

// Create 创建对比实验
func (r *ModelComparisonRepository) Create(exp *model.ModelComparisonExperiment) error {
	return r.db.Create(exp).Error
}

// GetByID 获取实验
func (r *ModelComparisonRepository) GetByID(id uint) (*model.ModelComparisonExperiment, error) {
	var exp model.ModelComparisonExperiment
	if err := r.db.First(&exp, id).Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

// Update 更新实验
func (r *ModelComparisonRepository) Update(exp *model.ModelComparisonExperiment) error {
	return r.db.Save(exp).Error
}

// List 获取实验列表
func (r *ModelComparisonRepository) List(limit int) ([]*model.ModelComparisonExperiment, error) {
	var experiments []*model.ModelComparisonExperiment
	if err := r.db.Order("created_at DESC").Limit(limit).Find(&experiments).Error; err != nil {
		return nil, err
	}
	return experiments, nil
}

// AddResult 添加实验结果
func (r *ModelComparisonRepository) AddResult(result *model.ExperimentResult) error {
	return r.db.Create(result).Error
}

// GetResults 获取实验结果
func (r *ModelComparisonRepository) GetResults(experimentID uint) ([]*model.ExperimentResult, error) {
	var results []*model.ExperimentResult
	if err := r.db.Preload("Model").Where("experiment_id = ?", experimentID).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

// SelectModel 选择模型
func (s *ModelService) SelectModel(taskType string, strategy string) (*model.AIModel, error) {
	// 获取可用模型
	models, err := s.modelRepo.GetAvailableByTaskType(taskType)
	if err != nil || len(models) == 0 {
		return nil, fmt.Errorf("no available models for task: %s", taskType)
	}

	// 根据策略选择
	var selected *model.AIModel
	switch strategy {
	case "quality_first":
		selected = selectByQuality(models)
	case "cost_first":
		selected = selectByCost(models)
	default: // balanced
		selected = selectBalanced(models)
	}

	return selected, nil
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
