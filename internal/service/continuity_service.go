package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ContinuityService 连续性检查服务
type ContinuityService struct {
	characterRepo interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
	}
	chapterRepo interface {
		GetRecent(novelID uint, chapterNo, count int) ([]*model.Chapter, error)
	}
	reportRepo *repository.ContinuityReportRepository
}

func NewContinuityService(
	charRepo interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
	},
	chapterRepo interface {
		GetRecent(novelID uint, chapterNo, count int) ([]*model.Chapter, error)
	},
) *ContinuityService {
	return &ContinuityService{
		characterRepo: charRepo,
		chapterRepo:   chapterRepo,
	}
}

// WithReportRepo injects the persistence layer for continuity reports.
func (s *ContinuityService) WithReportRepo(r *repository.ContinuityReportRepository) *ContinuityService {
	s.reportRepo = r
	return s
}

// ListReports returns persisted continuity reports for a chapter, newest first.
func (s *ContinuityService) ListReports(chapterID uint) ([]*model.ContinuityReportRecord, error) {
	if s.reportRepo == nil {
		return nil, fmt.Errorf("report repository not configured")
	}
	return s.reportRepo.ListByChapter(chapterID)
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
	CharacterID   uint   `json:"character_id"`
	CharacterName string `json:"character_name"`
	Type          string `json:"type"` // appearance, personality, ability, dialogue, absence
	Severity      string `json:"severity"`
	Description   string `json:"description"`
	Location      string `json:"location"`
	Suggestion    string `json:"suggestion"`
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

// ValidateChapter 检查连续性并将结果持久化到数据库。
func (s *ContinuityService) ValidateChapter(novelID, chapterID, tenantID uint, chapterNo int, content string) (*ContinuityReport, error) {
	report, err := s.CheckContinuity(novelID, chapterNo, content)
	if err != nil {
		return nil, err
	}

	if s.reportRepo != nil {
		issueCount := len(report.CharacterIssues) + len(report.WorldviewIssues) + len(report.PlotIssues)
		data, _ := json.Marshal(report)
		rec := &model.ContinuityReportRecord{
			NovelID:    novelID,
			ChapterID:  chapterID,
			ReportJSON: string(data),
			IssueCount: issueCount,
			Passed:     !report.HasIssues,
		}
		if saveErr := s.reportRepo.Create(rec); saveErr != nil {
			logger.Errorf("ContinuityService: save report: %v", saveErr)
		}
	}

	return report, nil
}

// CheckContinuity 检查连续性（基于规则的多维度检查）
func (s *ContinuityService) CheckContinuity(novelID uint, chapterNo int, content string) (*ContinuityReport, error) {
	report := &ContinuityReport{}

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

	// 3. 检查剧情连续性（与前3章对比）
	report.PlotIssues = s.checkPlotContinuity(novelID, chapterNo, content)

	// 4. 检查文本质量问题
	report.WorldviewIssues = s.checkTextQuality(content)

	// 5. 汇总
	if len(report.CharacterIssues) > 0 || len(report.PlotIssues) > 0 || len(report.WorldviewIssues) > 0 {
		report.HasIssues = true
		report.Suggestions = s.generateSuggestions(report)
	}

	return report, nil
}

func (s *ContinuityService) checkCharacterConsistency(character *model.Character, content string) []CharacterIssue {
	var issues []CharacterIssue
	name := character.Name
	if name == "" {
		return issues
	}

	nameCount := strings.Count(content, name)

	// 角色完全未出现但描述中有较多戏份期望（名字较短说明是主角）
	if nameCount == 0 && len([]rune(name)) <= 3 {
		return issues // 跳过非本章角色，不报告
	}

	// 角色出场次数极少（主要角色期望多次出现）
	contentLen := len([]rune(content))
	if nameCount > 0 && nameCount < 2 && contentLen > 2000 {
		issues = append(issues, CharacterIssue{
			CharacterID:   character.ID,
			CharacterName: name,
			Type:          "absence",
			Severity:      "low",
			Description:   fmt.Sprintf("角色「%s」在本章仅出现%d次（全文%d字），存在感较弱", name, nameCount, contentLen),
			Suggestion:    "确保重要角色有足够的出场、对话或心理描写",
		})
	}

	// 检查外貌描述与角色设定的一致性
	if character.Description != "" && nameCount > 0 {
		issues = append(issues, s.checkAppearanceConsistency(character, content)...)
	}

	// 检查角色对话风格异常（连续多次出现完全不同的称谓）
	issues = append(issues, s.checkNameVariants(character, content)...)

	return issues
}

// checkAppearanceConsistency 检查外貌关键词是否与角色设定矛盾
func (s *ContinuityService) checkAppearanceConsistency(character *model.Character, content string) []CharacterIssue {
	var issues []CharacterIssue
	desc := character.Description

	// 提取角色设定中的外貌关键词
	type traitCheck struct {
		trait    string
		keywords []string
		opposite []string
	}

	checks := []traitCheck{
		{
			trait:    "发色",
			keywords: extractTraitKeywords(desc, []string{"黑发", "白发", "金发", "红发", "银发", "蓝发"}),
			opposite: []string{"黑发", "白发", "金发", "红发", "银发", "蓝发"},
		},
		{
			trait:    "身形",
			keywords: extractTraitKeywords(desc, []string{"高挑", "矮小", "魁梧", "纤细", "壮硕"}),
			opposite: []string{"高挑", "矮小", "魁梧", "纤细", "壮硕"},
		},
	}

	for _, check := range checks {
		if len(check.keywords) == 0 {
			continue
		}
		// 在以角色名为中心的段落中查找相反描述
		paras := extractParagraphsWithName(content, character.Name)
		for _, para := range paras {
			for _, opp := range check.opposite {
				if containsKeyword(check.keywords, opp) {
					continue // 正确描述，跳过
				}
				if strings.Contains(para, opp) {
					issues = append(issues, CharacterIssue{
						CharacterID:   character.ID,
						CharacterName: character.Name,
						Type:          "appearance",
						Severity:      "medium",
						Description:   fmt.Sprintf("角色「%s」的%s描述（\"%s\"）可能与其设定矛盾", character.Name, check.trait, opp),
						Suggestion:    fmt.Sprintf("请核实角色设定：%s", truncate(desc, 60)),
					})
					break
				}
			}
		}
	}

	return issues
}

// checkNameVariants 检查角色是否在同章使用了相互矛盾的称谓
func (s *ContinuityService) checkNameVariants(character *model.Character, content string) []CharacterIssue {
	// 提取各种对该角色的称谓（如"他"、角色名、别称），简化为只检测是否存在
	// 对话中将角色名与「她」/「他」性别标记进行一致性校验
	name := character.Name
	genderDesc := character.Description

	// 从描述推断性别标记
	isMale := strings.Contains(genderDesc, "男") || strings.Contains(genderDesc, "他")
	isFemale := strings.Contains(genderDesc, "女") || strings.Contains(genderDesc, "她")

	if !isMale && !isFemale {
		return nil
	}

	var issues []CharacterIssue
	paras := extractParagraphsWithName(content, name)
	for _, para := range paras {
		if isMale && strings.Contains(para, "她") && !strings.Contains(para, "她的") {
			issues = append(issues, CharacterIssue{
				CharacterID:   character.ID,
				CharacterName: name,
				Type:          "personality",
				Severity:      "medium",
				Description:   fmt.Sprintf("角色「%s」在同段落中使用了与性别设定不符的代词「她」", name),
				Suggestion:    "请检查该段落中的代词使用是否正确",
			})
			break
		}
		if isFemale && strings.Contains(para, "他") && !strings.Contains(para, "他的") {
			issues = append(issues, CharacterIssue{
				CharacterID:   character.ID,
				CharacterName: name,
				Type:          "personality",
				Severity:      "medium",
				Description:   fmt.Sprintf("角色「%s」在同段落中使用了与性别设定不符的代词「他」", name),
				Suggestion:    "请检查该段落中的代词使用是否正确",
			})
			break
		}
	}

	return issues
}

func (s *ContinuityService) checkPlotContinuity(novelID uint, chapterNo int, content string) []PlotIssue {
	var issues []PlotIssue

	contentRunes := []rune(content)
	contentLen := len(contentRunes)

	// 1. 内容长度检查
	if contentLen < 800 {
		issues = append(issues, PlotIssue{
			Type:        "length",
			Severity:    "high",
			Description: fmt.Sprintf("章节内容过短（%d字），远低于建议最低字数（800字）", contentLen),
			Suggestion:  "建议增加情节描写、对话和心理活动，丰富章节内容",
		})
	} else if contentLen < 1500 {
		issues = append(issues, PlotIssue{
			Type:        "length",
			Severity:    "medium",
			Description: fmt.Sprintf("章节内容偏短（%d字），建议在1500字以上", contentLen),
			Suggestion:  "可适当补充细节描写或对话",
		})
	}

	// 2. 获取前几章用于上下文对比
	recentChapters, err := s.chapterRepo.GetRecent(novelID, chapterNo, 3)
	if err != nil || len(recentChapters) == 0 {
		return issues
	}

	// 3. 检查重复词汇过多（滥用同一表达）
	issues = append(issues, s.checkRepetitiveWords(content)...)

	// 4. 检查对话比例是否合理
	issues = append(issues, s.checkDialogueRatio(content, contentLen)...)

	// 5. 检查时间线连贯性（简单检测时间词矛盾）
	issues = append(issues, s.checkTimelineConsistency(content, recentChapters)...)

	return issues
}

// checkRepetitiveWords 检测过度重复使用的词汇
func (s *ContinuityService) checkRepetitiveWords(content string) []PlotIssue {
	var issues []PlotIssue

	// 高频词汇检测（中文小说常见滥用词）
	overusedWords := []string{"突然", "忽然", "顿时", "瞬间", "竟然", "果然", "居然"}
	contentLen := len([]rune(content))
	if contentLen == 0 {
		return issues
	}

	for _, word := range overusedWords {
		count := strings.Count(content, word)
		threshold := contentLen / 500 // 每500字允许出现1次
		if threshold < 2 {
			threshold = 2
		}
		if count > threshold+3 {
			issues = append(issues, PlotIssue{
				Type:        "repetition",
				Severity:    "low",
				Description: fmt.Sprintf("词汇「%s」在本章出现%d次，频率偏高，可能影响阅读体验", word, count),
				Suggestion:  fmt.Sprintf("建议将部分「%s」替换为其他表达方式", word),
			})
		}
	}

	// 检测连续段首重复
	paragraphs := strings.Split(content, "\n")
	consecutiveSameStart := 0
	lastStart := ""
	maxConsec := 0
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if len([]rune(para)) < 5 {
			continue
		}
		runes := []rune(para)
		start := string(runes[:min(2, len(runes))])
		if start == lastStart {
			consecutiveSameStart++
			if consecutiveSameStart > maxConsec {
				maxConsec = consecutiveSameStart
			}
		} else {
			consecutiveSameStart = 0
		}
		lastStart = start
	}
	if maxConsec >= 4 {
		issues = append(issues, PlotIssue{
			Type:        "structure",
			Severity:    "medium",
			Description: fmt.Sprintf("检测到%d个连续段落以相同文字开头，句式结构单一", maxConsec+1),
			Suggestion:  "建议变换段落开头方式，避免句式过于单调",
		})
	}

	return issues
}

// checkDialogueRatio 检查对话与叙述的比例
func (s *ContinuityService) checkDialogueRatio(content string, contentLen int) []PlotIssue {
	if contentLen < 1000 {
		return nil
	}
	// 统计对话字数（引号内）
	dialogueLen := countDialogueChars(content)
	ratio := float64(dialogueLen) / float64(contentLen)

	var issues []PlotIssue
	if ratio > 0.7 {
		issues = append(issues, PlotIssue{
			Type:        "structure",
			Severity:    "low",
			Description: fmt.Sprintf("对话占比%.0f%%，可能缺乏足够的叙述和描写", ratio*100),
			Suggestion:  "建议增加环境描写、心理活动和叙事过渡",
		})
	} else if ratio < 0.05 && contentLen > 2000 {
		issues = append(issues, PlotIssue{
			Type:        "structure",
			Severity:    "low",
			Description: "章节几乎没有对话，可能导致节奏沉闷",
			Suggestion:  "建议适当加入角色对话，增强互动感和节奏变化",
		})
	}
	return issues
}

// checkTimelineConsistency 检查时间线是否与前章存在明显矛盾
func (s *ContinuityService) checkTimelineConsistency(content string, prevChapters []*model.Chapter) []PlotIssue {
	var issues []PlotIssue

	// 简单检测：当前章节中的时间词
	timeWords := []string{"昨天", "今天", "明天", "昨日", "今日", "明日", "上午", "下午", "傍晚", "深夜", "清晨"}
	currentTimeCtx := extractTimeContext(content, timeWords)

	for _, prev := range prevChapters {
		if prev.Content == "" {
			continue
		}
		prevTimeCtx := extractTimeContext(prev.Content, timeWords)

		// 检测：前章说"明天"，本章又说"昨天"，可能存在时间跳跃
		if containsAny(prevTimeCtx, []string{"今天", "今日"}) && containsAny(currentTimeCtx, []string{"今天", "今日"}) {
			// 两章都是"今天"，可能是同一天的不同场景，正常
			continue
		}
		if containsAny(prevTimeCtx, []string{"明天", "明日"}) && containsAny(currentTimeCtx, []string{"昨天", "昨日"}) {
			issues = append(issues, PlotIssue{
				Type:        "timeline",
				Severity:    "medium",
				Description: "前章提及「明天/明日」，本章使用「昨天/昨日」，时间线可能存在逻辑矛盾",
				Suggestion:  "请核实章节间的时间衔接是否连贯",
			})
			break
		}
	}

	return issues
}

// checkTextQuality 检测文本质量问题（归类为 WorldviewIssue 以复用结构）
func (s *ContinuityService) checkTextQuality(content string) []WorldviewIssue {
	var issues []WorldviewIssue

	// 1. 检测大量重复字符（排版问题）— RE2 不支持反向引用，改用 rune 逐字符计数
	if hasRepeatedChars(content, 6) {
		issues = append(issues, WorldviewIssue{
			Type:        "format",
			Severity:    "medium",
			Description: "检测到大量重复字符（连续6个以上相同字符），可能存在格式错误",
			Suggestion:  "请检查并修复重复字符问题",
		})
	}

	// 2. 检测未替换的占位符
	placeholderRe := regexp.MustCompile(`\[.*?\]|\{.*?\}|【.*?】|<.*?>`)
	matches := placeholderRe.FindAllString(content, -1)
	for _, m := range matches {
		if len([]rune(m)) > 2 && len([]rune(m)) < 20 {
			issues = append(issues, WorldviewIssue{
				Type:        "placeholder",
				Severity:    "high",
				Description: fmt.Sprintf("发现疑似未替换的占位符：「%s」", m),
				Suggestion:  "请替换所有占位符为实际内容",
			})
			break
		}
	}

	// 3. 检测段落数量是否合理
	paragraphs := splitParagraphs(content)
	if len(paragraphs) < 3 && len([]rune(content)) > 800 {
		issues = append(issues, WorldviewIssue{
			Type:        "structure",
			Severity:    "low",
			Description: fmt.Sprintf("全章仅%d个段落，段落划分过于密集，可读性较低", len(paragraphs)),
			Suggestion:  "建议适当增加换行，提高文本可读性",
		})
	}

	return issues
}

func (s *ContinuityService) generateSuggestions(report *ContinuityReport) []string {
	var suggestions []string
	seen := map[string]bool{}

	for _, issue := range report.CharacterIssues {
		if issue.Severity == "high" || issue.Severity == "medium" {
			if !seen["char"] {
				suggestions = append(suggestions, "建议重点检查主要角色在本章的外貌、性格、能力描写是否与人物设定一致")
				seen["char"] = true
			}
		}
	}
	for _, issue := range report.PlotIssues {
		if issue.Type == "timeline" && !seen["timeline"] {
			suggestions = append(suggestions, "建议核查章节间的时间线衔接，确保前后逻辑连贯")
			seen["timeline"] = true
		}
		if issue.Type == "length" && !seen["length"] {
			suggestions = append(suggestions, "建议增加章节篇幅，丰富情节细节和人物互动")
			seen["length"] = true
		}
		if issue.Type == "repetition" && !seen["rep"] {
			suggestions = append(suggestions, "建议使用词汇多样化工具检查并优化重复表达")
			seen["rep"] = true
		}
	}
	for _, issue := range report.WorldviewIssues {
		if issue.Type == "placeholder" && !seen["placeholder"] {
			suggestions = append(suggestions, "章节中存在未替换的占位符，请仔细审查后发布")
			seen["placeholder"] = true
		}
	}

	if len(suggestions) == 0 {
		suggestions = append(suggestions, "章节存在轻微问题，建议结合 AI 深度审查功能进行进一步检查")
	}

	return suggestions
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func extractTraitKeywords(desc string, candidates []string) []string {
	var found []string
	for _, kw := range candidates {
		if strings.Contains(desc, kw) {
			found = append(found, kw)
		}
	}
	return found
}

func extractParagraphsWithName(content, name string) []string {
	var result []string
	for _, para := range strings.Split(content, "\n") {
		if strings.Contains(para, name) {
			result = append(result, para)
		}
	}
	return result
}

func containsKeyword(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func containsAny(list []string, targets []string) bool {
	for _, t := range targets {
		for _, item := range list {
			if item == t {
				return true
			}
		}
	}
	return false
}

func extractTimeContext(content string, timeWords []string) []string {
	var found []string
	for _, tw := range timeWords {
		if strings.Contains(content, tw) {
			found = append(found, tw)
		}
	}
	return found
}

func countDialogueChars(content string) int {
	total := 0
	inDialogue := false
	for _, r := range content {
		// “ = " (left double quotation), 「 = 「
		// ” = " (right double quotation), 」 = 」
		if r == '“' || r == '「' {
			inDialogue = true
		} else if r == '”' || r == '」' {
			inDialogue = false
		} else if inDialogue && !unicode.IsSpace(r) {
			total++
		}
	}
	return total
}

func splitParagraphs(content string) []string {
	var result []string
	for _, p := range strings.Split(content, "\n") {
		if strings.TrimSpace(p) != "" {
			result = append(result, p)
		}
	}
	return result
}

func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// hasRepeatedChars reports whether content contains n or more consecutive identical runes.
// RE2 (Go regexp) does not support backreferences, so we use a simple rune scan.
func hasRepeatedChars(content string, n int) bool {
	if n <= 1 {
		return true
	}
	count := 1
	prev := rune(-1)
	for _, r := range content {
		if r == prev {
			count++
			if count >= n {
				return true
			}
		} else {
			count = 1
			prev = r
		}
	}
	return false
}

