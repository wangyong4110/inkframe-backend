package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

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
	aiSvc       *AIService
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
	}
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
}

func NewQualityControlService(
	aiSvc *AIService,
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
	},
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	},
) *QualityControlService {
	return &QualityControlService{aiSvc: aiSvc, chapterRepo: chapterRepo, novelRepo: novelRepo}
}

// runAIQualityCheck 调用 AI 对章节内容进行综合质检，返回各维度评分（0-10分制）
func (s *QualityControlService) runAIQualityCheck(chapter *model.Chapter, novel *model.Novel) (*AIQualityScores, error) {
	if s.aiSvc == nil {
		return nil, fmt.Errorf("AI service not initialized")
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

	result, err := s.aiSvc.GenerateWithProvider(0, 0, "quality_check", prompt, s.aiSvc.taskRouting.QualityCheck)
	if err != nil {
		return nil, fmt.Errorf("AI quality check failed: %w", err)
	}

	content := extractJSON(result)
	var scores AIQualityScores
	if err := json.Unmarshal([]byte(content), &scores); err != nil {
		return nil, fmt.Errorf("parse AI quality scores: %w (raw: %s)", err, content)
	}

	return &scores, nil
}

// QualityReport 质量报告
type QualityReport struct {
	OverallScore     float64        `json:"overall_score"`
	ConsistencyScore float64        `json:"consistency_score"`
	QualityScore     float64        `json:"quality_score"`
	LogicScore       float64        `json:"logic_score"`
	StyleScore       float64        `json:"style_score"`
	Issues           []QualityIssue `json:"issues"`
	Suggestions      []string       `json:"suggestions"`
}

// QualityIssue 质量问题
type QualityIssue struct {
	Type        string `json:"type"`     // consistency, quality, logic, style
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
	aiScores, err := s.runAIQualityCheck(chapter, novel)
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
	rWords := []string{"突然", "然后", "接着", "非常", "十分"}
	for _, word := range rWords {
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
	rWords := []string{"突然", "然后", "接着", "非常"}
	for _, word := range rWords {
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
	totalChars := len([]rune(chapter.Content))
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
	seen := map[string]bool{}
	suggestions := []string{}

	// Collect per-issue suggestions (skip generic fallback)
	for _, issue := range report.Issues {
		sg := strings.TrimSpace(issue.Suggestion)
		if sg != "" && sg != "请根据AI建议进行修改" && !seen[sg] {
			seen[sg] = true
			suggestions = append(suggestions, sg)
		}
	}

	// Append summary based on score
	if report.OverallScore >= 0.9 {
		suggestions = append(suggestions, "章节质量优秀，可适当润色句式增加表现力")
	} else if report.OverallScore >= 0.7 {
		suggestions = append(suggestions, "章节质量良好，建议根据上述问题进行小幅优化")
	} else {
		suggestions = append(suggestions, "章节存在较多问题，建议整体检查并重写关键部分")
	}
	return suggestions
}

// RefineWithSuggestions 按照指定改进建议对章节内容进行 AI 精修，返回改进后的文本（不保存）
func (s *QualityControlService) RefineWithSuggestions(chapterID uint, suggestions []string) (string, error) {
	if s.aiSvc == nil {
		return "", fmt.Errorf("AI client not initialized")
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return "", fmt.Errorf("chapter not found: %w", err)
	}
	if chapter.Content == "" {
		return "", fmt.Errorf("chapter has no content")
	}
	if len(suggestions) == 0 {
		return chapter.Content, nil
	}

	var sb strings.Builder
	for i, sg := range suggestions {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, sg))
	}

	prompt := fmt.Sprintf(`你是一位专业的小说编辑。请根据以下改进建议，对章节内容进行精修。
要求：保持原有情节、人物和对话不变，只优化写作质量；不添加任何解释说明，直接返回修改后的完整内容。

改进建议：
%s
原始内容：
%s`, sb.String(), chapter.Content)

	result, err := s.aiSvc.GenerateWithProvider(0, 0, "quality_check", prompt, "")
	if err != nil {
		return "", fmt.Errorf("AI refine failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}
