package service

import (
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// ============================================
// 质量控制服务 - Quality Control Service
// ============================================


// RepetitionReport 重复性报告
type RepetitionReport struct {
	TotalRepetitions int              `json:"total_repetitions"`
	Repetitions     []Repetition     `json:"repetitions"`
	DiversityScore  float64          `json:"diversity_score"` // 0-1，越高越好
}

// Repetition 重复内容
type Repetition struct {
	Type        string   `json:"type"` // sentence, word, paragraph
	Pattern     string   `json:"pattern"`
	Count       int      `json:"count"`
	Positions   []int   `json:"positions"`
	Suggestions []string `json:"suggestions"`
}

// QualityControlService 质量控制服务
type QualityControlService struct {
	aiService *AIService
}

// NewQualityControlService 创建质量控制服务
func NewQualityControlService(aiService *AIService) *QualityControlService {
	return &QualityControlService{
		aiService: aiService,
	}
}

// ============================================
// 1. 重复性检测器
// ============================================

// DetectRepetition 检测重复内容
func (s *QualityControlService) DetectRepetition(content string) *RepetitionReport {
	report := &RepetitionReport{
		Repetitions:    []Repetition{},
		DiversityScore: 1.0,
	}

	// 1. 检测句子结构重复
	sentenceReps := s.detectSentenceRepetition(content)
	report.Repetitions = append(report.Repetitions, sentenceReps...)

	// 2. 检测词汇重复
	wordReps := s.detectWordRepetition(content)
	report.Repetitions = append(report.Repetitions, wordReps...)

	// 3. 检测段落结构重复
	paragraphReps := s.detectParagraphRepetition(content)
	report.Repetitions = append(report.Repetitions, paragraphReps...)

	// 4. 计算多样性得分
	report.TotalRepetitions = len(report.Repetitions)
	report.DiversityScore = s.calculateDiversityScore(content, report.TotalRepetitions)

	return report
}

// 检测句子模式重复
func (s *QualityControlService) detectSentenceRepetition(content string) []Repetition {
	repetitions := []Repetition{}

	// 简单按句号分割
	sentences := strings.Split(content, "。")
	if len(sentences) < 2 {
		return repetitions
	}

	// 统计句子开头模式
	patterns := make(map[string][]int)
	for i, sentence := range sentences {
		if len(sentence) < 5 {
			continue
		}
		// 提取句子开头（前20个字符）
		start := strings.TrimSpace(sentence)
		if len(start) > 20 {
			start = start[:20]
		}
		// 归一化
		start = strings.ToLower(start)
		patterns[start] = append(patterns[start], i)
	}

	// 找出重复模式
	for pattern, positions := range patterns {
		if len(positions) >= 3 {
			repetitions = append(repetitions, Repetition{
				Type:      "sentence_pattern",
				Pattern:   pattern,
				Count:     len(positions),
				Positions: positions,
				Suggestions: []string{
					"使用不同的句式结构",
					"调整句子长度",
					"改变开头方式",
				},
			})
		}
	}

	return repetitions
}

// 检测词汇重复
func (s *QualityControlService) detectWordRepetition(content string) []Repetition {
	repetitions := []Repetition{}

	// 高频词汇表
	stopWords := map[string]bool{
		"的": true, "了": true, "是": true, "在": true, "有": true,
		"和": true, "与": true, "或": true, "但": true, "却": true,
	}

	// 统计词汇频率
	words := strings.Fields(content)
	wordCount := make(map[string]int)
	for _, word := range words {
		word = strings.TrimSpace(word)
		if len(word) >= 2 && !stopWords[word] {
			wordCount[word]++
		}
	}

	// 找出过度重复的词汇
	for word, count := range wordCount {
		// 如果一个词出现次数超过总词数的5%，认为过度重复
		if float64(count) > float64(len(words))*0.05 && count >= 5 {
			repetitions = append(repetitions, Repetition{
				Type:   "word_repetition",
				Pattern: word,
				Count:  count,
				Suggestions: []string{
					fmt.Sprintf("减少「%s」的使用频率", word),
					"使用同义词替换",
					"改变表达方式",
				},
			})
		}
	}

	return repetitions
}

// 检测段落结构重复
func (s *QualityControlService) detectParagraphRepetition(content string) []Repetition {
	repetitions := []Repetition{}

	paragraphs := strings.Split(content, "\n\n")
	if len(paragraphs) < 3 {
		return repetitions
	}

	// 统计段落开头模式
	patterns := make(map[string][]int)
	for i, para := range paragraphs {
		para = strings.TrimSpace(para)
		if len(para) < 10 {
			continue
		}
		// 取前30个字符作为模式
		start := para
		if len(start) > 30 {
			start = start[:30]
		}
		start = strings.ToLower(start)
		patterns[start] = append(patterns[start], i)
	}

	for pattern, positions := range patterns {
		if len(positions) >= 2 {
			repetitions = append(repetitions, Repetition{
				Type:      "paragraph_pattern",
				Pattern:   pattern,
				Count:     len(positions),
				Positions: positions,
				Suggestions: []string{
					"改变段落开头方式",
					"调整段落长度",
					"使用不同的叙述节奏",
				},
			})
		}
	}

	return repetitions
}

// 计算多样性得分
func (s *QualityControlService) calculateDiversityScore(content string, repetitionCount int) float64 {
	// 基于内容长度和重复次数计算得分
	totalChars := len(content)
	if totalChars == 0 {
		return 1.0
	}

	// 重复密度
	repetitionDensity := float64(repetitionCount) / (float64(totalChars) / 1000.0)

	// 得分范围 0-1
	score := 1.0 - (repetitionDensity * 0.1)
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	return score
}

// ============================================
// 2. 对话质量分析器
// ============================================

// DialogueAnalysis 对话分析结果
type DialogueAnalysis struct {
	NaturalnessScore  float64         `json:"naturalness_score"`  // 0-1
	CharacterScore    float64         `json:"character_score"`    // 角色独特性
	UniquenessScore  float64         `json:"uniqueness_score"`  // 独特性
	Issues           []DialogueIssue `json:"issues"`
}

// DialogueIssue 对话问题
type DialogueIssue struct {
	Type        string `json:"type"` // formal_language, info_dump, unnatural_length, etc.
	Severity   string `json:"severity"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Suggestion  string `json:"suggestion"`
}

// AnalyzeDialogue 分析对话质量
func (s *QualityControlService) AnalyzeDialogue(content string, character *model.Character) *DialogueAnalysis {
	analysis := &DialogueAnalysis{
		Issues: []DialogueIssue{},
	}

	// 1. 提取对话
	dialogues := s.extractDialogues(content)

	if len(dialogues) == 0 {
		analysis.NaturalnessScore = 1.0
		analysis.CharacterScore = 1.0
		analysis.UniquenessScore = 1.0
		return analysis
	}

	// 2. 检查自然度
	analysis.NaturalnessScore = s.checkDialogueNaturalness(dialogues)

	// 3. 检查角色独特性
	analysis.CharacterScore = s.checkCharacterDialogueUniqueness(dialogues, character)

	// 4. 检测常见问题
	issues := s.detectDialogueIssues(dialogues)
	analysis.Issues = append(analysis.Issues, issues...)

	// 5. 计算独特性得分
	analysis.UniquenessScore = s.calculateDialogueUniqueness(dialogues)

	return analysis
}

// 提取对话
func (s *QualityControlService) extractDialogues(content string) []string {
	dialogues := []string{}

	// 简单提取引号内的内容
	inQuote := false
	current := ""

	for _, char := range content {
		if char == '"' || char == '"' || char == '"' {
			if inQuote {
				if len(current) > 0 {
					dialogues = append(dialogues, current)
				}
				current = ""
			}
			inQuote = !inQuote
		} else if inQuote {
			current += string(char)
		}
	}

	return dialogues
}

// 检查对话自然度
func (s *QualityControlService) checkDialogueNaturalness(dialogues []string) float64 {
	score := 1.0

	// 书面语标记
	formalMarkers := []string{"因为", "所以", "由于", "因此", "然而", "但是", "并且", "而且"}
	informalMarkers := []string{"嗯", "啊", "哦", "嘛", "啦", "呀", "呃", "哎"}

	for _, dialogue := range dialogues {
		// 检查是否过于正式
		formalCount := 0
		for _, marker := range formalMarkers {
			if strings.Contains(dialogue, marker) {
				formalCount++
			}
		}

		// 检查是否过于口语
		informalCount := 0
		for _, marker := range informalMarkers {
			if strings.Contains(dialogue, marker) {
				informalCount++
			}
		}

		// 扣分
		if formalCount > 3 {
			score -= 0.1
		}
		if informalCount > 5 {
			score -= 0.05
		}
	}

	if score < 0 {
		score = 0
	}

	return score
}

// 检查角色对话独特性
func (s *QualityControlService) checkCharacterDialogueUniqueness(dialogues []string, character *model.Character) float64 {
	if character == nil || character.Personality == "" {
		return 0.8
	}

	score := 1.0

	// 检查是否符合角色性格设定
	personality := strings.ToLower(character.Personality)

	// 根据性格类型检测对话特点
	uniquePatterns := 0
	for _, dialogue := range dialogues {
		dialogue = strings.TrimSpace(dialogue)

		// 检查是否符合角色性格
		if strings.Contains(personality, "内向") || strings.Contains(personality, "害羞") {
			if len(dialogue) < 20 {
				uniquePatterns++
			}
		}

		if strings.Contains(personality, "外向") || strings.Contains(personality, "开朗") {
			if strings.Contains(dialogue, "!") || strings.Contains(dialogue, "？") {
				uniquePatterns++
			}
		}
	}

	// 计算独特性
	if len(dialogues) > 0 {
		patternRatio := float64(uniquePatterns) / float64(len(dialogues))
		score = 0.5 + patternRatio*0.5
	}

	return score
}

// 检测对话问题
func (s *QualityControlService) detectDialogueIssues(dialogues []string) []DialogueIssue {
	issues := []DialogueIssue{}

	for i, dialogue := range dialogues {
		location := fmt.Sprintf("对话 %d", i+1)

		// 检测信息倾倒
		if len(dialogue) > 200 {
			issues = append(issues, DialogueIssue{
				Type:        "info_dump",
				Severity:    "medium",
				Description: fmt.Sprintf("对话过长（%d字），可能存在信息倾倒", len(dialogue)),
				Location:    location,
				Suggestion:  "拆分对话，增加角色互动和反应",
			})
		}

		// 检测句子过长
		sentences := strings.Split(dialogue, "。")
		for _, sentence := range sentences {
			if len(sentence) > 50 {
				issues = append(issues, DialogueIssue{
					Type:        "unnatural_length",
					Severity:    "low",
					Description: fmt.Sprintf("对话句子过长（%d字），不够口语化", len(sentence)),
					Location:    location,
					Suggestion:  "拆分长句，使用更简短的口语表达",
				})
				break
			}
		}

		// 检测缺乏潜台词
		if len(dialogue) > 50 && !strings.ContainsAny(dialogue, "，、,.?!") {
			issues = append(issues, DialogueIssue{
				Type:        "lacks_subtext",
				Severity:    "low",
				Description: "对话过于直白，缺乏潜台词和言外之意",
				Location:    location,
				Suggestion:  "增加角色的言外之意或肢体语言暗示",
			})
		}
	}

	return issues
}

// 计算对话独特性
func (s *QualityControlService) calculateDialogueUniqueness(dialogues []string) float64 {
	if len(dialogues) < 2 {
		return 1.0
	}

	// 计算对话长度变化
	lengths := make([]int, len(dialogues))
	for i, d := range dialogues {
		lengths[i] = len(d)
	}

	avgLength := 0
	for _, l := range lengths {
		avgLength += l
	}
	avgLength /= len(lengths)

	// 计算方差
	variance := 0
	for _, l := range lengths {
		diff := l - avgLength
		variance += diff * diff
	}
	variance /= len(lengths)

	// 方差越大，独特性越高
	normalizedVariance := float64(variance) / float64(avgLength*avgLength+1)

	return normalizedVariance / (normalizedVariance + 1) // 归一化到 0-1
}

// ============================================
// 3. 风格一致性检查器
// ============================================

// StyleCheckResult 风格检查结果
type StyleCheckResult struct {
	ConsistencyScore float64    `json:"consistency_score"` // 与前文的风格一致性
	DriftAspects     []string   `json:"drift_aspects"`    // 漂移的方面
	Issues           []StyleIssue `json:"issues"`
}

// StyleIssue 风格问题
type StyleIssue struct {
	Type        string `json:"type"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
}

// CheckStyleConsistency 检查风格一致性
func (s *QualityControlService) CheckStyleConsistency(novelID uint, currentContent string, previousContents []string) *StyleCheckResult {
	result := &StyleCheckResult{
		Issues: []StyleIssue{},
	}

	if len(previousContents) == 0 {
		result.ConsistencyScore = 1.0
		return result
	}

	// 1. 分析当前内容风格
	currentStyle := s.analyzeStyle(currentContent)

	// 2. 分析历史风格
	var historicalStyles []*StyleFeatures
	for _, content := range previousContents {
		historicalStyles = append(historicalStyles, s.analyzeStyle(content))
	}

	// 3. 计算风格漂移
	result.ConsistencyScore = s.calculateStyleConsistency(currentStyle, historicalStyles)

	// 4. 检测漂移方面
	result.DriftAspects = s.detectStyleDrift(currentStyle, historicalStyles)

	// 5. 生成问题报告
	if result.ConsistencyScore < 0.7 {
		result.Issues = append(result.Issues, StyleIssue{
			Type:        "style_drift",
			Severity:    "medium",
			Description: fmt.Sprintf("文风一致性较低（%.2f），偏离历史风格", result.ConsistencyScore),
			Suggestion:  "调整叙述方式，使其与前面章节保持一致",
		})
	}

	return result
}

// StyleFeatures 风格特征
type StyleFeatures struct {
	AvgSentenceLength float64 `json:"avg_sentence_length"`
	DialogueRatio     float64 `json:"dialogue_ratio"`
	DescriptionDensity float64 `json:"description_density"`
	ParagraphLength   float64 `json:"paragraph_length"`
	POV               string  `json:"pov"` // first, third
}

// 分析风格特征
func (s *QualityControlService) analyzeStyle(content string) *StyleFeatures {
	features := &StyleFeatures{}

	// 统计句子
	sentences := strings.Split(content, "。")
	features.AvgSentenceLength = float64(len(content)) / float64(len(sentences)+1)

	// 统计对话比例
	dialogues := s.extractDialogues(content)
	totalChars := len(content)
	if totalChars > 0 {
		dialogueChars := 0
		for _, d := range dialogues {
			dialogueChars += len(d)
		}
		features.DialogueRatio = float64(dialogueChars) / float64(totalChars)
	}

	// 统计段落
	paragraphs := strings.Split(content, "\n\n")
	features.DescriptionDensity = float64(len(content)-len(dialogues)*2) / float64(len(paragraphs)+1)

	// 统计段落长度
	features.ParagraphLength = float64(len(paragraphs))

	// 检测视角
	if strings.Contains(content, "我") && strings.Count(content, "我") > strings.Count(content, "他") {
		features.POV = "first"
	} else {
		features.POV = "third"
	}

	return features
}

// 计算风格一致性
func (s *QualityControlService) calculateStyleConsistency(current *StyleFeatures, historical []*StyleFeatures) float64 {
	if len(historical) == 0 {
		return 1.0
	}

	// 计算历史平均值
	avg := &StyleFeatures{}
	for _, h := range historical {
		avg.AvgSentenceLength += h.AvgSentenceLength
		avg.DialogueRatio += h.DialogueRatio
		avg.DescriptionDensity += h.DescriptionDensity
	}
	avg.AvgSentenceLength /= float64(len(historical))
	avg.DialogueRatio /= float64(len(historical))
	avg.DescriptionDensity /= float64(len(historical))

	// 计算差异
	lengthDiff := 1.0 - abs(current.AvgSentenceLength-avg.AvgSentenceLength)/avg.AvgSentenceLength
	dialogueDiff := 1.0 - abs(current.DialogueRatio-avg.DialogueRatio)
	densityDiff := 1.0 - abs(current.DescriptionDensity-avg.DescriptionDensity)/avg.DescriptionDensity

	// 综合得分
	score := (lengthDiff + dialogueDiff + densityDiff) / 3.0

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	return score
}

// 检测风格漂移方面
func (s *QualityControlService) detectStyleDrift(current *StyleFeatures, historical []*StyleFeatures) []string {
	drifts := []string{}

	// 计算历史平均值
	avg := &StyleFeatures{}
	for _, h := range historical {
		avg.AvgSentenceLength += h.AvgSentenceLength
		avg.DialogueRatio += h.DialogueRatio
		avg.DescriptionDensity += h.DescriptionDensity
	}
	avg.AvgSentenceLength /= float64(len(historical))
	avg.DialogueRatio /= float64(len(historical))
	avg.DescriptionDensity /= float64(len(historical))

	// 检测漂移
	threshold := 0.3
	if abs(current.AvgSentenceLength-avg.AvgSentenceLength)/avg.AvgSentenceLength > threshold {
		if current.AvgSentenceLength > avg.AvgSentenceLength {
			drifts = append(drifts, "句子变长")
		} else {
			drifts = append(drifts, "句子变短")
		}
	}

	if abs(current.DialogueRatio-avg.DialogueRatio) > threshold {
		if current.DialogueRatio > avg.DialogueRatio {
			drifts = append(drifts, "对话增多")
		} else {
			drifts = append(drifts, "叙述增多")
		}
	}

	return drifts
}

// ============================================
// 4. 剧情逻辑检查器
// ============================================

// PlotLogicReport 剧情逻辑报告
type PlotLogicReport struct {
	Score        float64       `json:"score"`
	Issues       []PlotIssue  `json:"issues"`
	 Suggestions []string     `json:"suggestions"`
}


// CheckPlotLogic 检查剧情逻辑
func (s *QualityControlService) CheckPlotLogic(novelID uint, chapterNo int, content string) *PlotLogicReport {
	report := &PlotLogicReport{
		Issues:       []PlotIssue{},
		Suggestions: []string{},
	}

	// 1. 检测因果关系问题
	causalityIssues := s.checkCausality(content)
	report.Issues = append(report.Issues, causalityIssues...)

	// 2. 检测动机问题
	motivationIssues := s.checkMotivation(content)
	report.Issues = append(report.Issues, motivationIssues...)

	// 3. 检测知识合理性问题
	knowledgeIssues := s.checkKnowledge合理性(content)
	report.Issues = append(report.Issues, knowledgeIssues...)

	// 4. 计算总体得分
	totalIssues := len(report.Issues)
	if totalIssues == 0 {
		report.Score = 1.0
	} else {
		report.Score = 1.0 - float64(totalIssues)*0.1
		if report.Score < 0 {
			report.Score = 0
		}
	}

	// 5. 生成建议
	if len(report.Issues) > 0 {
		report.Suggestions = s.generatePlotSuggestions(report.Issues)
	}

	return report
}

// 检查因果关系
func (s *QualityControlService) checkCausality(content string) []PlotIssue {
	issues := []PlotIssue{}

	// 检测突然的行动（缺少铺垫）
	suddenActions := []string{
		"突然", "猛然", "忽然", "竟然", "没想到",
	}

	for _, phrase := range suddenActions {
		if strings.Contains(content, phrase) {
			// 简单检查：如果"突然"之后紧跟动作，可能是问题
			idx := strings.Index(content, phrase)
			after := content[idx:]
			if len(after) > 20 {
				after = after[:20]
			}

			actionMarkers := []string{"说", "做", "走", "打", "问", "看"}
			for _, marker := range actionMarkers {
				if strings.Contains(after, marker) {
					issues = append(issues, PlotIssue{
						Type:        "sudden_action",
						Severity:    "medium",
						Description: fmt.Sprintf("「%s」后角色行动突然，缺少心理铺垫", phrase),
						Location:    fmt.Sprintf("「%s」附近", phrase),
						Suggestion:  "增加行动前的心理描写或外部刺激",
					})
					break
				}
			}
		}
	}

	return issues
}

// 检查动机问题
func (s *QualityControlService) checkMotivation(content string) []PlotIssue {
	issues := []PlotIssue{}

	// 检测角色说的话和行动不匹配
	// 这是一个简化实现

	return issues
}

// 检查知识合理性问题
func (s *QualityControlService) checkKnowledge合理性(content string) []PlotIssue {
	issues := []PlotIssue{}

	// 检测角色可能不应该知道的知识被使用
	// 简化：如果一个角色第一次提到某事，后面直接使用，可能是问题

	return issues
}

// 生成剧情建议
func (s *QualityControlService) generatePlotSuggestions(issues []PlotIssue) []string {
	suggestions := []string{}

	for _, issue := range issues {
		if issue.Suggestion != "" {
			suggestions = append(suggestions, issue.Suggestion)
		}
	}

	// 去重
	seen := make(map[string]bool)
	unique := []string{}
	for _, s := range suggestions {
		if !seen[s] {
			seen[s] = true
			unique = append(unique, s)
		}
	}

	return unique
}

// ============================================
// 5. 综合质量报告
// ============================================

// ComprehensiveQualityReport 综合质量报告
type ComprehensiveQualityReport struct {
	OverallScore      float64         `json:"overall_score"`
	ConsistencyScore   float64         `json:"consistency_score"`
	QualityScore       float64         `json:"quality_score"`
	LogicScore         float64         `json:"logic_score"`
	StyleScore         float64         `json:"style_score"`
	RepetitionReport   *RepetitionReport `json:"repetition_report"`
	DialogueAnalysis   *DialogueAnalysis `json:"dialogue_analysis"`
	StyleCheckResult   *StyleCheckResult `json:"style_check_result"`
	PlotLogicReport   *PlotLogicReport  `json:"plot_logic_report"`
	TotalIssues        int              `json:"total_issues"`
	HighPriorityIssues int              `json:"high_priority_issues"`
}

// GenerateComprehensiveReport 生成综合质量报告
func (s *QualityControlService) GenerateComprehensiveReport(
	novelID uint,
	chapterNo int,
	currentContent string,
	previousContents []string,
	character *model.Character,
) *ComprehensiveQualityReport {
	report := &ComprehensiveQualityReport{}

	// 1. 重复性检测
	report.RepetitionReport = s.DetectRepetition(currentContent)

	// 2. 对话分析
	report.DialogueAnalysis = s.AnalyzeDialogue(currentContent, character)

	// 3. 风格一致性检查
	report.StyleCheckResult = s.CheckStyleConsistency(novelID, currentContent, previousContents)

	// 4. 剧情逻辑检查
	report.PlotLogicReport = s.CheckPlotLogic(novelID, chapterNo, currentContent)

	// 5. 计算各维度得分
	report.ConsistencyScore = report.StyleCheckResult.ConsistencyScore
	report.QualityScore = report.RepetitionReport.DiversityScore * 0.5 +
		report.DialogueAnalysis.NaturalnessScore*0.3 +
		report.DialogueAnalysis.CharacterScore*0.2
	report.LogicScore = report.PlotLogicReport.Score
	report.StyleScore = report.StyleCheckResult.ConsistencyScore

	// 6. 计算总体得分（加权平均）
	report.OverallScore = report.ConsistencyScore*0.3 +
		report.QualityScore*0.3 +
		report.LogicScore*0.25 +
		report.StyleScore*0.15

	// 7. 统计问题数量
	report.TotalIssues = len(report.RepetitionReport.Repetitions) +
		len(report.DialogueAnalysis.Issues) +
		len(report.StyleCheckResult.Issues) +
		len(report.PlotLogicReport.Issues)

	report.HighPriorityIssues = 0
	for _, issue := range report.DialogueAnalysis.Issues {
		if issue.Severity == "high" {
			report.HighPriorityIssues++
		}
	}
	for _, issue := range report.PlotLogicReport.Issues {
		if issue.Severity == "high" {
			report.HighPriorityIssues++
		}
	}

	return report
}

// ============================================
// 辅助函数
// ============================================

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
