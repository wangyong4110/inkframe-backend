package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

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
	type templateRepoI interface {
		GetByGenreAndStage(genre, stage string) (*model.PromptTemplate, error)
		GetByID(id uint) (*model.PromptTemplate, error)
		List() ([]*model.PromptTemplate, error)
	}
	r, _ := repo.(templateRepoI)
	return &PromptService{templateRepo: r}
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
func (s *PromptService) BuildChapterPrompt(
	novel *model.Novel,
	chapter *model.Chapter,
	recentChapters []*model.Chapter,
	characters []*model.Character,
	characterSnapshots []*model.CharacterStateSnapshot,
	unfulfilledForeshadows []*model.KnowledgeBase,
) string {
	var sb strings.Builder

	// 系统提示
	sb.WriteString("你是一位专业的小说作家，创作内容需要：\n")
	sb.WriteString("1. 保持与前文的剧情连贯性\n")
	sb.WriteString("2. 角色性格和对话风格保持一致\n")
	sb.WriteString("3. 遵循世界观设定\n")
	sb.WriteString("4. 适当埋下伏笔并呼应已有伏笔\n")
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
		sb.WriteString("【主要角色设定】\n")
		for _, char := range characters {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", char.Name, char.Personality))
		}
		sb.WriteString("\n")
	}

	// 角色当前状态快照
	if len(characterSnapshots) > 0 {
		sb.WriteString("【角色当前状态（上章末）】\n")
		for _, snap := range characterSnapshots {
			sb.WriteString(fmt.Sprintf("- 角色ID[%d]：位置=%s，情绪=%s，目标=%s，能力等级=%d\n",
				snap.CharacterID, snap.Location, snap.Mood, snap.Motivation, snap.PowerLevel))
		}
		sb.WriteString("⚠️ 本章角色行为必须与上述状态保持连续性\n\n")
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

	// 未兑现伏笔
	if len(unfulfilledForeshadows) > 0 {
		sb.WriteString("【待兑现伏笔（可酌情呼应）】\n")
		for _, kb := range unfulfilledForeshadows {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", kb.Title, kb.Content))
		}
		sb.WriteString("\n")
	}

	// 章节要求
	sb.WriteString("【章节要求】\n")
	sb.WriteString(fmt.Sprintf("- 章节标题：%s\n", chapter.Title))
	sb.WriteString("- 字数要求：2000-3000字\n")

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
	type charRepoI interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
		GetLatestSnapshot(characterID uint) (*model.CharacterStateSnapshot, error)
	}
	type chapterRepoI interface {
		GetRecent(novelID uint, chapterNo, count int) ([]*model.Chapter, error)
	}
	cr, _ := charRepo.(charRepoI)
	cpr, _ := chapterRepo.(chapterRepoI)
	return &ContinuityService{
		characterRepo: cr,
		chapterRepo:   cpr,
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
		GetByID(id uint) (*model.KnowledgeBase, error)
	}
	vectorStore *vector.StoreManager
	aiClient    ai.AIProvider
}

func NewKnowledgeService(kbRepo interface{}, vectorStore *vector.StoreManager, aiClient ai.AIProvider) *KnowledgeService {
	type kbRepoI interface {
		Create(kb *model.KnowledgeBase) error
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
		GetByID(id uint) (*model.KnowledgeBase, error)
	}
	r, _ := kbRepo.(kbRepoI)
	return &KnowledgeService{
		kbRepo:      r,
		vectorStore: vectorStore,
		aiClient:    aiClient,
	}
}

// StoreKnowledge 存储知识（含向量化）
func (s *KnowledgeService) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase) error {
	// 存储到数据库
	if err := s.kbRepo.Create(kb); err != nil {
		return err
	}

	// 向量化并存入向量库
	if s.vectorStore != nil && s.aiClient != nil {
		text := kb.Title + " " + kb.Content
		vec, err := s.aiClient.Embed(ctx, text)
		if err != nil {
			log.Printf("KnowledgeService.StoreKnowledge: embed error for kb %d: %v", kb.ID, err)
			// 不因为向量化失败就整体失败，降级处理
		} else {
			store := s.vectorStore.DefaultStore()
			if store != nil {
				payload := map[string]interface{}{
					"id":       kb.ID,
					"type":     kb.Type,
					"title":    kb.Title,
					"content":  kb.Content,
					"novel_id": kb.NovelID,
				}
				_, storeErr := store.Store(ctx, &vector.StoreRequest{
					Collection: "knowledge_base",
					ID:         fmt.Sprintf("%d", kb.ID),
					Vector:     vec,
					Payload:    payload,
				})
				if storeErr != nil {
					log.Printf("KnowledgeService.StoreKnowledge: vector store error for kb %d: %v", kb.ID, storeErr)
				}
			}
		}
	}

	return nil
}

// SearchKnowledge 搜索知识（优先向量语义搜索，降级到关键词）
func (s *KnowledgeService) SearchKnowledge(ctx context.Context, query string, limit int, novelID *uint) ([]*model.KnowledgeBase, error) {
	// 尝试向量语义搜索
	if s.vectorStore != nil && s.aiClient != nil {
		vec, err := s.aiClient.Embed(ctx, query)
		if err == nil {
			store := s.vectorStore.DefaultStore()
			if store != nil {
				filters := map[string]interface{}{}
				if novelID != nil {
					filters["novel_id"] = *novelID
				}
				vectorResults, searchErr := store.Search(ctx, &vector.SearchRequest{
					Collection: "knowledge_base",
					Vector:     vec,
					Limit:      limit,
					Filters:    filters,
					MinScore:   0.6,
				})
				if searchErr == nil && len(vectorResults) > 0 {
					// 从向量结果中获取 KB 对象
					kbs := make([]*model.KnowledgeBase, 0, len(vectorResults))
					for _, vr := range vectorResults {
						if idVal, ok := vr.Payload["id"]; ok {
							var id uint
							switch v := idVal.(type) {
							case float64:
								id = uint(v)
							case uint:
								id = v
							}
							if id > 0 {
								kb, kbErr := s.kbRepo.GetByID(id)
								if kbErr == nil {
									kbs = append(kbs, kb)
								}
							}
						}
					}
					if len(kbs) > 0 {
						return kbs, nil
					}
				}
			}
		}
		// 向量搜索失败，降级到关键词搜索
		log.Printf("KnowledgeService.SearchKnowledge: vector search failed, fallback to keyword: %v", err)
	}

	// 关键词搜索降级
	results, err := s.kbRepo.Search(query, limit)
	if err != nil {
		return nil, err
	}

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
		_, _ = json.Marshal(pp.Locations)

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

// AIQualityScores AI质检评分结果
type AIQualityScores struct {
	Logic       float64  `json:"logic"`
	Character   float64  `json:"character"`
	Writing     float64  `json:"writing"`
	Pacing      float64  `json:"pacing"`
	Issues      []string `json:"issues"`
	Suggestions []string `json:"suggestions"`
}

// QualityControlService 质量控制服务
type QualityControlService struct {
	aiClient    *ai.ModelManager
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
	}
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
}

func NewQualityControlService(
	aiClient *ai.ModelManager,
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
	},
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	},
) *QualityControlService {
	return &QualityControlService{aiClient: aiClient, chapterRepo: chapterRepo, novelRepo: novelRepo}
}

// runAIQualityCheck 调用 AI 对章节内容进行综合质检，返回各维度评分（0-10分制）
func (s *QualityControlService) runAIQualityCheck(ctx context.Context, chapter *model.Chapter, novel *model.Novel) (*AIQualityScores, error) {
	if s.aiClient == nil {
		return nil, fmt.Errorf("AI client not initialized")
	}
	provider, err := s.aiClient.GetProvider("")
	if err != nil {
		return nil, fmt.Errorf("get AI provider: %w", err)
	}

	novelInfo := fmt.Sprintf("小说：《%s》，类型：%s", novel.Title, novel.Genre)
	contentPreview := chapter.Content
	if len(contentPreview) > 3000 {
		contentPreview = contentPreview[:3000] + "...(已截断)"
	}

	prompt := fmt.Sprintf(`请从以下维度评估这段章节内容（0-10分制），并以JSON返回：
1. logic（逻辑连贯性）：情节是否自洽，因果关系是否合理
2. character（角色一致性）：角色行为是否符合其性格设定
3. writing（文笔质量）：语言是否生动，描写是否细腻，是否有重复词汇
4. pacing（节奏把控）：场景切换是否流畅，节奏是否合理，有无张力

%s
章节标题：%s
章节内容：
%s

请只返回以下JSON格式，不要包含任何markdown或说明文字：
{"logic":8,"character":7,"writing":9,"pacing":8,"issues":["问题1","问题2"],"suggestions":["建议1","建议2"]}`,
		novelInfo, chapter.Title, contentPreview)

	req := &ai.GenerateRequest{
		Messages:    []ai.ChatMessage{{Role: "user", Content: prompt}},
		MaxTokens:   1000,
		Temperature: 0.3,
	}

	resp, err := provider.Generate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("AI quality check failed: %w", err)
	}

	content := extractJSONContent(resp.Content)
	var scores AIQualityScores
	if err := json.Unmarshal([]byte(content), &scores); err != nil {
		return nil, fmt.Errorf("parse AI quality scores: %w (raw: %s)", err, content)
	}

	return &scores, nil
}

// QualityReport 质量报告
type QualityReport struct {
	OverallScore    float64           `json:"overall_score"`
	ConsistencyScore float64          `json:"consistency_score"`
	QualityScore    float64           `json:"quality_score"`
	LogicScore      float64           `json:"logic_score"`
	StyleScore      float64           `json:"style_score"`
	Issues          []QualityIssue    `json:"issues"`
	Suggestions     []string          `json:"suggestions"`
}

// QualityIssue 质量问题
type QualityIssue struct {
	Type        string `json:"type"` // consistency, quality, logic, style
	Severity    string `json:"severity"` // high, medium, low
	Description string `json:"description"`
	Location    string `json:"location"`
	Suggestion  string `json:"suggestion"`
}

// CheckChapterQuality 检查章节质量（AI评分 + 规则检查）
func (s *QualityControlService) CheckChapterQuality(ctx context.Context, chapter *model.Chapter, novel *model.Novel) (*QualityReport, error) {
	report := &QualityReport{
		Issues:      []QualityIssue{},
		Suggestions: []string{},
	}

	// 1. AI 综合质检（获取真实分数）
	aiScores, err := s.runAIQualityCheck(ctx, chapter, novel)
	if err != nil {
		log.Printf("QualityControlService: AI quality check failed: %v, falling back to rule-based", err)
		// AI 失败时降级到规则检查
		aiScores = nil
	}

	if aiScores != nil {
		// 将 AI 返回的 0-10 分转为 0-1
		report.LogicScore = aiScores.Logic / 10.0
		report.ConsistencyScore = aiScores.Character / 10.0
		report.QualityScore = aiScores.Writing / 10.0
		report.StyleScore = aiScores.Pacing / 10.0

		// 将 AI 发现的问题加入报告
		for _, issue := range aiScores.Issues {
			report.Issues = append(report.Issues, QualityIssue{
				Type:        "ai_detected",
				Severity:    "medium",
				Description: issue,
				Suggestion:  "请根据AI建议进行修改",
			})
		}
		report.Suggestions = append(report.Suggestions, aiScores.Suggestions...)
	} else {
		// 降级：规则检查
		report.ConsistencyScore = s.calcConsistencyScore(chapter)
		report.QualityScore = s.calcQualityScore(chapter)
		report.LogicScore = 0.7 // 规则无法检查逻辑，给中性分
		report.StyleScore = s.calcStyleScore(chapter)

		report.Issues = append(report.Issues, s.checkConsistency(chapter)...)
		report.Issues = append(report.Issues, s.checkQuality(chapter)...)
		report.Issues = append(report.Issues, s.checkStyle(chapter)...)
	}

	// 计算综合总分（加权平均）
	report.OverallScore = (report.LogicScore*0.3 + report.ConsistencyScore*0.25 + report.QualityScore*0.25 + report.StyleScore*0.2)
	if report.OverallScore > 1.0 {
		report.OverallScore = 1.0
	}
	if report.OverallScore < 0 {
		report.OverallScore = 0
	}

	// 追加通用建议
	report.Suggestions = append(report.Suggestions, s.generateQualitySuggestions(report)...)

	return report, nil
}

// calcConsistencyScore 基于规则计算一致性分数
func (s *QualityControlService) calcConsistencyScore(chapter *model.Chapter) float64 {
	score := 1.0
	repeatWords := []string{"突然", "然后", "接着", "非常", "十分"}
	for _, word := range repeatWords {
		count := strings.Count(chapter.Content, word)
		if count > 8 {
			score -= 0.1
		} else if count > 5 {
			score -= 0.05
		}
	}
	if score < 0 {
		score = 0
	}
	return score
}

// calcQualityScore 基于规则计算质量分数
func (s *QualityControlService) calcQualityScore(chapter *model.Chapter) float64 {
	if chapter.WordCount < 1000 {
		return 0.5
	} else if chapter.WordCount < 1500 {
		return 0.7
	}
	return 0.85
}

// calcStyleScore 基于规则计算风格分数
func (s *QualityControlService) calcStyleScore(chapter *model.Chapter) float64 {
	score := 0.8
	dialogueCount := strings.Count(chapter.Content, "「") + strings.Count(chapter.Content, "\u201c")
	totalChars := len([]rune(chapter.Content))
	if totalChars > 0 {
		dialogueRatio := float64(dialogueCount*20) / float64(totalChars)
		if dialogueRatio < 0.05 {
			score -= 0.15
		}
	}
	// 检查句式多样性（粗略：以句号结尾的句子平均长度）
	sentences := strings.Split(chapter.Content, "。")
	if len(sentences) > 5 {
		totalLen := 0
		for _, s := range sentences {
			totalLen += len([]rune(s))
		}
		avgLen := totalLen / len(sentences)
		if avgLen < 10 {
			score -= 0.1 // 句子过短，缺乏描写
		} else if avgLen > 80 {
			score -= 0.05 // 句子过长，节奏沉闷
		}
	}
	if score < 0 {
		score = 0
	}
	return score
}

func (s *QualityControlService) checkConsistency(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}
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

func (s *QualityControlService) checkQuality(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}
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

func (s *QualityControlService) checkStyle(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}
	dialogueCount := strings.Count(chapter.Content, "「") + strings.Count(chapter.Content, "\u201c")
	totalChars := len(chapter.Content)
	if totalChars > 0 {
		dialogueRatio := float64(dialogueCount*10) / float64(totalChars)
		if dialogueRatio < 0.05 {
			issues = append(issues, QualityIssue{
				Type:        "style",
				Severity:    "low",
				Description: "对话比例较低，可能显得叙述过于单调",
				Suggestion:  "建议增加更多角色对话，使故事更生动",
			})
		}
	}
	return issues
}

func (s *QualityControlService) generateQualitySuggestions(report *QualityReport) []string {
	suggestions := []string{}
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

// extractJSONContent 从 AI 返回内容中提取纯 JSON（去除 markdown 代码块包裹）
func extractJSONContent(content string) string {
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

