package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/vector"
)

// PromptService 提示词服务
type PromptService struct {
	templateRepo interface {
		GetByGenreAndStage(genre, stage string) (*model.PromptTemplate, error)
		GetByID(id uint) (*model.PromptTemplate, error)
		List() ([]*model.PromptTemplate, error)
	}
}

func NewPromptService(repo interface{}) *PromptService {
	return &PromptService{templateRepo: repo}
}

// RenderPrompt 渲染提示词
func (s *PromptService) RenderPrompt(templateID uint, variables map[string]interface{}) (string, error) {
	tmpl, err := s.templateRepo.GetByID(templateID)
	if err != nil {
		return "", err
	}

	return s.render(tmpl.Template, variables), nil
}

// BuildOutlinePrompt 构建大纲提示词
func (s *PromptService) BuildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	var sb strings.Builder

	// 系统提示
	sb.WriteString("你是一位专业的小说作家，擅长创作中长篇小说。\n\n")

	// 用户需求
	sb.WriteString(fmt.Sprintf("请为小说《%s》生成一个详细的大纲。\n\n", novel.Title))

	if novel.Description != "" {
		sb.WriteString(fmt.Sprintf("故事简介：%s\n\n", novel.Description))
	}

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

	sb.WriteString(fmt.Sprintf("请生成%d章的大纲。\n", req.ChapterNum))

	// 输出格式
	sb.WriteString("\n请以JSON格式返回，格式如下：\n")
	sb.WriteString(`{"title":"小说标题","chapters":[{"chapter_no":1,"title":"章节标题","summary":"章节概述","word_count":2500,"plot_points":["剧情点1","剧情点2"]}]}`)

	return sb.String()
}

// BuildChapterPrompt 构建章节提示词
func (s *PromptService) BuildChapterPrompt(novel *model.Novel, chapter *model.Chapter, recentChapters []*model.Chapter, characters []*model.Character) string {
	var sb strings.Builder

	// 系统提示
	sb.WriteString("你是一位专业的小说作家，创作内容需要：\n")
	sb.WriteString("1. 保持与前文的剧情连贯性\n")
	sb.WriteString("2. 角色性格和对话风格保持一致\n")
	sb.WriteString("3. 遵循世界观设定\n")
	sb.WriteString("4. 适当埋下伏笔\n")
	sb.WriteString("5. 语言生动，描写细腻\n\n")

	// 小说信息
	sb.WriteString(fmt.Sprintf("【小说标题】%s\n\n", novel.Title))

	// 世界观
	if novel.Worldview != nil {
		sb.WriteString("【世界观设定】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString(fmt.Sprintf("- 修炼体系：%s\n", novel.Worldview.MagicSystem))
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString(fmt.Sprintf("- 地理环境：%s\n", novel.Worldview.Geography))
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString(fmt.Sprintf("- 文化背景：%s\n", novel.Worldview.Culture))
		}
		sb.WriteString("\n")
	}

	// 角色信息
	if len(characters) > 0 {
		sb.WriteString("【主要角色】\n")
		for _, char := range characters {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", char.Name, char.Personality))
		}
		sb.WriteString("\n")
	}

	// 前情提要
	if len(recentChapters) > 0 {
		sb.WriteString("【前情提要】\n")
		for i := len(recentChapters) - 1; i >= 0; i-- {
			ch := recentChapters[i]
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, ch.Summary))
		}
		sb.WriteString("\n")
	}

	// 章节要求
	sb.WriteString(fmt.Sprintf("【章节要求】\n"))
	sb.WriteString(fmt.Sprintf("- 章节标题：%s\n", chapter.Title))
	sb.WriteString(fmt.Sprintf("- 字数要求：2000-3000字\n"))

	if req.Prompt, ok := interface{}(nil).(string); ok && req.Prompt != "" {
		sb.WriteString(fmt.Sprintf("- 创作要求：%s\n", req.Prompt))
	}

	return sb.String()
}

func (s *PromptService) render(template string, variables map[string]interface{}) string {
	result := template

	for key, value := range variables {
		placeholder := fmt.Sprintf("{{%s}}", key)
		var replacement string
		switch v := value.(type) {
		case string:
			replacement = v
		case int, int64, float64:
			replacement = fmt.Sprintf("%v", v)
		default:
			replacement = fmt.Sprintf("%v", v)
		}
		result = strings.ReplaceAll(result, placeholder, replacement)
	}

	return result
}

// ContinuityService 连续性检查服务
type ContinuityService struct {
	characterRepo interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
		GetLatestSnapshot(characterID uint) (*model.CharacterStateSnapshot, error)
	}
	chapterRepo interface {
		GetRecent(novelID uint, chapterNo, count int) ([]*model.Chapter, error)
	}
}

func NewContinuityService(charRepo, chapterRepo interface{}) *ContinuityService {
	return &ContinuityService{
		characterRepo: charRepo,
		chapterRepo:   chapterRepo,
	}
}

// ContinuityReport 连续性报告
type ContinuityReport struct {
	HasIssues       bool             `json:"has_issues"`
	CharacterIssues []CharacterIssue `json:"character_issues"`
	WorldviewIssues []WorldviewIssue `json:"worldview_issues"`
	PlotIssues      []PlotIssue      `json:"plot_issues"`
	Suggestions     []string         `json:"suggestions"`
}

// CharacterIssue 角色问题
type CharacterIssue struct {
	CharacterID uint   `json:"character_id"`
	CharacterName string `json:"character_name"`
	Type        string `json:"type"` // appearance, personality, ability, dialogue
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Suggestion  string `json:"suggestion"`
}

// WorldviewIssue 世界观问题
type WorldviewIssue struct {
	Type        string `json:"type"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Suggestion  string `json:"suggestion"`
}

// PlotIssue 剧情问题
type PlotIssue struct {
	Type        string `json:"type"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Suggestion  string `json:"suggestion"`
}

// CheckContinuity 检查连续性
func (s *ContinuityService) CheckContinuity(novelID uint, chapterNo int, content string) (*ContinuityReport, error) {
	report := &ContinuityReport{
		HasIssues: false,
	}

	// 1. 获取角色列表
	characters, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	// 2. 检查角色一致性
	for _, char := range characters {
		issues := s.checkCharacterConsistency(char, content)
		report.CharacterIssues = append(report.CharacterIssues, issues...)
	}

	// 3. 检查剧情连续性
	report.PlotIssues = s.checkPlotContinuity(novelID, chapterNo, content)

	// 4. 生成建议
	if len(report.CharacterIssues) > 0 || len(report.PlotIssues) > 0 {
		report.HasIssues = true
		report.Suggestions = s.generateSuggestions(report)
	}

	return report, nil
}

func (s *ContinuityService) checkCharacterConsistency(character *model.Character, content string) []CharacterIssue {
	var issues []CharacterIssue

	// 检查角色名出现次数
	nameCount := strings.Count(content, character.Name)

	// 检查外貌描述一致性
	appearanceKeywords := []string{"身高", "眼睛", "头发", "服装", "外貌"}
	for _, keyword := range appearanceKeywords {
		if strings.Contains(content, keyword) {
			// 应该有连贯的外貌描写
			// 这里简化处理，实际应该使用 NLP 分析
		}
	}

	// 检查性格表现
	// 简化：检查是否有矛盾的性格表现
	if nameCount > 0 && nameCount < 3 {
		issues = append(issues, CharacterIssue{
			CharacterID:   character.ID,
			CharacterName: character.Name,
			Type:          "appearance",
			Severity:      "low",
			Description:   fmt.Sprintf("角色「%s」在本章中出现次数较少（%d次），可能存在感不足", character.Name, nameCount),
			Suggestion:    "确保主要角色有足够的出场和互动",
		})
	}

	return issues
}

func (s *ContinuityService) checkPlotContinuity(novelID uint, chapterNo int, content string) []PlotIssue {
	var issues []PlotIssue

	// 获取前几章
	recentChapters, err := s.chapterRepo.GetRecent(novelID, chapterNo, 3)
	if err != nil || len(recentChapters) == 0 {
		return issues
	}

	// 简化：检查内容长度
	if len(content) < 1000 {
		issues = append(issues, PlotIssue{
			Type:        "length",
			Severity:    "medium",
			Description: fmt.Sprintf("章节内容过短（%d字），可能不够充实", len(content)),
			Suggestion:  "建议增加更多细节描写和对话",
		})
	}

	return issues
}

func (s *ContinuityService) generateSuggestions(report *ContinuityReport) []string {
	var suggestions []string

	if len(report.CharacterIssues) > 0 {
		suggestions = append(suggestions, "建议检查角色在章节中的表现是否与其设定一致")
	}

	if len(report.PlotIssues) > 0 {
		suggestions = append(suggestions, "建议检查剧情是否与前文连贯")
	}

	return suggestions
}

// KnowledgeService 知识库服务
type KnowledgeService struct {
	kbRepo    interface {
		Create(kb *model.KnowledgeBase) error
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
	}
	vectorStore *vector.StoreManager
}

func NewKnowledgeService(kbRepo interface{}, vectorStore *vector.StoreManager) *KnowledgeService {
	return &KnowledgeService{
		kbRepo:      kbRepo,
		vectorStore: vectorStore,
	}
}

// StoreKnowledge 存储知识
func (s *KnowledgeService) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase) error {
	// 存储到数据库
	if err := s.kbRepo.Create(kb); err != nil {
		return err
	}

	// 存储到向量数据库
	if s.vectorStore != nil {
		// 向量化并存储
		// 实际实现需要调用 embedder
	}

	return nil
}

// SearchKnowledge 搜索知识
func (s *KnowledgeService) SearchKnowledge(ctx context.Context, query string, limit int, novelID *uint) ([]*model.KnowledgeBase, error) {
	// 先从数据库搜索
	results, err := s.kbRepo.Search(query, limit)
	if err != nil {
		return nil, err
	}

	// 过滤
	if novelID != nil {
		var filtered []*model.KnowledgeBase
		for _, kb := range results {
			if kb.NovelID != nil && *kb.NovelID == *novelID {
				filtered = append(filtered, kb)
			}
		}
		results = filtered
	}

	return results, nil
}

// ExtractAndStorePlotPoints 提取并存储剧情点
func (s *KnowledgeService) ExtractAndStorePlotPoints(ctx context.Context, chapter *model.Chapter, aiClient ai.AIProvider) error {
	// 使用 AI 提取剧情点
	prompt := fmt.Sprintf(`从以下章节内容中提取关键剧情点，返回JSON数组格式：
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

	req := ai.NewGenerateRequestBuilder().
		UserMessage(prompt).
		MaxTokens(2000).
		Temperature(0.3).
		Build()

	resp, err := aiClient.Generate(ctx, req)
	if err != nil {
		return err
	}

	// 解析结果
	var result struct {
		PlotPoints []struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Characters  []string `json:"characters"`
			Locations   []string `json:"locations"`
		} `json:"plot_points"`
	}

	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return err
	}

	// 存储剧情点
	for _, pp := range result.PlotPoints {
		charJSON, _ := json.Marshal(pp.Characters)
		locJSON, _ := json.Marshal(pp.Locations)

		kb := &model.KnowledgeBase{
			Type:     "plot_point",
			Title:    pp.Type + ": " + pp.Description[:min(50, len(pp.Description))],
			Content:  pp.Description,
			Tags:     string(charJSON),
			NovelID:  &chapter.NovelID,
		}

		s.StoreKnowledge(ctx, kb)
	}

	return nil
}


}



// CheckChapterQuality 检查章节质量
func (s *QualityControlService) CheckChapterQuality(ctx context.Context, chapter *model.Chapter, novel *model.Novel) (*QualityReport, error) {
	report := &QualityReport{
		OverallScore:     0.85,
		ConsistencyScore: 0.90,
		QualityScore:     0.85,
		LogicScore:       0.88,
		StyleScore:       0.82,
		Issues:           []QualityIssue{},
		Suggestions:      []string{},
	}

	// 1. 一致性检查
	consistencyIssues := s.checkConsistency(ctx, chapter, novel)
	report.Issues = append(report.Issues, consistencyIssues...)

	// 2. 质量检查
	qualityIssues := s.checkQuality(ctx, chapter)
	report.Issues = append(report.Issues, qualityIssues...)

	// 3. 逻辑检查
	logicIssues := s.checkLogic(ctx, chapter)
	report.Issues = append(report.Issues, logicIssues...)

	// 4. 风格检查
	styleIssues := s.checkStyle(ctx, chapter, novel)
	report.Issues = append(report.Issues, styleIssues...)

	// 计算评分
	if len(report.Issues) > 0 {
		highCount := 0
		mediumCount := 0
		for _, issue := range report.Issues {
			if issue.Severity == "high" {
				highCount++
			} else if issue.Severity == "medium" {
				mediumCount++
			}
		}
		report.OverallScore = 1.0 - float64(highCount)*0.15 - float64(mediumCount)*0.05
		if report.OverallScore < 0 {
			report.OverallScore = 0
		}
	}

	// 生成建议
	report.Suggestions = s.generateSuggestions(report)

	return report, nil
}


	// 简化：检查重复词汇
	repeatWords := []string{"突然", "然后", "接着", "非常"}
	for _, word := range repeatWords {
		count := strings.Count(chapter.Content, word)
		if count > 5 {
			issues = append(issues, QualityIssue{
				Type:        "consistency",
				Severity:    "low",
				Description: fmt.Sprintf("「%s」一词出现了%d次", word, count),
				Suggestion:  "建议使用同义词替换以增加表达多样性",
			})
		}
	}

	return issues
}


	// 检查字数
	if chapter.WordCount < 1500 {
		issues = append(issues, QualityIssue{
			Type:        "quality",
			Severity:    "medium",
			Description: fmt.Sprintf("章节字数较少（%d字），可能不够充实", chapter.WordCount),
			Suggestion:  "建议增加更多细节描写、对话或心理描写",
		})
	}

	return issues
}


	// 简化逻辑检查
	// 实际应该使用更复杂的 NLP 分析

	return issues
}


	// 检查对话比例
	dialogueCount := strings.Count(chapter.Content, "「") + strings.Count(chapter.Content, "」")
	totalChars := len(chapter.Content)
	dialogueRatio := float64(dialogueCount*10) / float64(totalChars)

	if dialogueRatio < 0.1 {
		issues = append(issues, QualityIssue{
			Type:        "style",
			Severity:    "low",
			Description: "对话比例较低，可能显得叙述过于单调",
			Suggestion:  "建议增加更多角色对话，使故事更生动",
		})
	}

	return issues
}


	highCount := 0
	for _, issue := range report.Issues {
		if issue.Severity == "high" {
			highCount++
		}
	}

	if highCount > 0 {
		suggestions = append(suggestions, fmt.Sprintf("有%d个高优先级问题需要修复", highCount))
	}

	if report.OverallScore >= 0.9 {
		suggestions = append(suggestions, "章节质量优秀，无需特别修改")
	} else if report.OverallScore >= 0.7 {
		suggestions = append(suggestions, "章节质量良好，建议根据上述问题进行小幅优化")
	} else {
		suggestions = append(suggestions, "章节存在较多问题，建议整体检查并重写关键部分")
	}

	return suggestions
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
