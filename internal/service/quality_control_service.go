package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// AIQualityScores AI质检评分结果
type AIQualityScores struct {
	Logic       float64  `json:"logic"`
	Character   float64  `json:"character"`
	Writing     float64  `json:"writing"`
	Pacing      float64  `json:"pacing"`
	Dramatic    float64  `json:"dramatic"` // 戏剧性：冲突密度、反转次数、悬念收尾质量
	Issues      []string `json:"issues"`
	Suggestions []string `json:"suggestions"`
}

// QualityControlService 质量控制服务
type QualityControlService struct {
	aiSvc       *AIService
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		Update(chapter *model.Chapter) error
		GetRecent(novelID uint, currentChapterNo, count int) ([]*model.Chapter, error)
		ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
	}
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
	reviewRecordRepo *repository.ReviewRecordRepository
	ignoredIssueRepo *repository.IgnoredReviewIssueRepository
	characterRepo    interface {
		ListByNovel(novelID uint) ([]*model.Character, error)
	}
	arcSummaryRepo interface {
		ListByNovel(novelID uint) ([]*model.ArcSummary, error)
	}
	foreshadowRepo interface {
		ListUnfulfilled(novelID uint) ([]*model.Foreshadow, error)
	}
}

func NewQualityControlService(
	aiSvc *AIService,
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		Update(chapter *model.Chapter) error
		GetRecent(novelID uint, currentChapterNo, count int) ([]*model.Chapter, error)
		ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
	},
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	},
) *QualityControlService {
	return &QualityControlService{aiSvc: aiSvc, chapterRepo: chapterRepo, novelRepo: novelRepo}
}

func (s *QualityControlService) WithReviewRepos(
	reviewRepo *repository.ReviewRecordRepository,
	ignoreRepo *repository.IgnoredReviewIssueRepository,
) *QualityControlService {
	s.reviewRecordRepo = reviewRepo
	s.ignoredIssueRepo = ignoreRepo
	return s
}

// chapterBelongsToTenant verifies chapter ownership via novel.TenantID (novel-based ownership).
func (s *QualityControlService) chapterBelongsToTenant(chapter *model.Chapter, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
}

func (s *QualityControlService) WithCharacterRepo(r interface {
	ListByNovel(novelID uint) ([]*model.Character, error)
}) *QualityControlService {
	s.characterRepo = r
	return s
}

func (s *QualityControlService) WithArcSummaryRepo(r interface {
	ListByNovel(novelID uint) ([]*model.ArcSummary, error)
}) *QualityControlService {
	s.arcSummaryRepo = r
	return s
}

func (s *QualityControlService) WithForeshadowRepo(r interface {
	ListUnfulfilled(novelID uint) ([]*model.Foreshadow, error)
}) *QualityControlService {
	s.foreshadowRepo = r
	return s
}

// ParagraphDiff describes a single paragraph replacement.
type ParagraphDiff struct {
	Index      int    `json:"index"`
	NewContent string `json:"new_content"`
	OrigText   string `json:"orig_text,omitempty"` // first ~80 chars of original; used for fallback content-based matching
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

	prompt, err := renderPrompt("quality_check", map[string]interface{}{
		"NovelInfo":      novelInfo,
		"ChapterTitle":   chapter.Title,
		"ChapterContent": contentPreview,
	})
	if err != nil {
		return nil, fmt.Errorf("render quality_check: %w", err)
	}

	result, err := s.aiSvc.GenerateWithProvider(novel.TenantID, novel.ID, "quality_check", prompt, s.aiSvc.taskRouting.QualityCheck)
	if err != nil {
		return nil, fmt.Errorf("AI quality check failed: %w", err)
	}

	// Strip markdown code fences that LLMs sometimes add
	result = strings.TrimSpace(result)
	if strings.HasPrefix(result, "```") {
		if idx := strings.Index(result, "\n"); idx != -1 {
			result = result[idx+1:]
		}
		result = strings.TrimSuffix(strings.TrimSpace(result), "```")
	}

	content := extractJSONObject(result)
	var scores AIQualityScores
	if err := json.Unmarshal([]byte(content), &scores); err != nil {
		return nil, fmt.Errorf("parse AI quality scores: %w (raw: %s)", err, content)
	}

	return &scores, nil
}

// MinAcceptableQualityScore is the minimum OverallScore (0–1 scale) for a chapter
// to be considered acceptable without triggering a refinement pass.
const MinAcceptableQualityScore = 0.6

// QualityReport 质量报告
type QualityReport struct {
	OverallScore     float64        `json:"overall_score"`
	ConsistencyScore float64        `json:"consistency_score"`
	QualityScore     float64        `json:"quality_score"`
	LogicScore       float64        `json:"logic_score"`
	StyleScore       float64        `json:"style_score"`
	DramaticScore    float64        `json:"dramatic_score"` // 戏剧性评分
	Issues           []QualityIssue `json:"issues"`
	Suggestions      []string       `json:"suggestions"`
}

// IsAcceptable reports whether the report's overall score meets the minimum threshold.
func (r *QualityReport) IsAcceptable() bool {
	return r.OverallScore >= MinAcceptableQualityScore
}

// SummarizeIssues returns a compact JSON string of all issues (for storage in Chapter.QualityIssues).
func (r *QualityReport) SummarizeIssues() string {
	if len(r.Issues) == 0 {
		return ""
	}
	b, err := json.Marshal(r.Issues)
	if err != nil {
		return ""
	}
	return string(b)
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
		logger.Errorf("QualityControlService: AI quality check failed: %v, falling back to rule-based", err)
		// AI 失败时降级到规则检查
		aiScores = nil
	}

	if aiScores != nil {
		// 将 AI 返回的 0-10 分转为 0-1
		report.LogicScore = aiScores.Logic / 10.0
		// AIQualityScores.Character 字段含义是"角色一致性"；业务上将角色一致性与情节一致性
		// 合并处理，统一映射到 ConsistencyScore（不改字段名，仅此注释澄清语义）。
		report.ConsistencyScore = aiScores.Character / 10.0
		report.QualityScore = aiScores.Writing / 10.0
		report.StyleScore = aiScores.Pacing / 10.0
		report.DramaticScore = aiScores.Dramatic / 10.0

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
		report.DramaticScore = s.calcDramaticScore(chapter)

		report.Issues = append(report.Issues, s.checkConsistency(chapter)...)
		report.Issues = append(report.Issues, s.checkQuality(chapter)...)
		report.Issues = append(report.Issues, s.checkStyle(chapter)...)
		report.Issues = append(report.Issues, s.checkDramatic(chapter)...)
	}

	// safeScore 保护每个维度分值，防止 NaN/Inf/-Inf 污染加权平均结果
	safeScore := func(s float64) float64 {
		if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
			return 0.5
		}
		if s > 1.0 {
			return 1.0
		}
		return s
	}
	// 计算综合总分（加权平均）: Logic 25% + Consistency 20% + Quality 20% + Style 15% + Dramatic 20%
	report.OverallScore = safeScore(report.LogicScore)*0.25 + safeScore(report.ConsistencyScore)*0.20 + safeScore(report.QualityScore)*0.20 + safeScore(report.StyleScore)*0.15 + safeScore(report.DramaticScore)*0.20
	if report.OverallScore > 1.0 {
		report.OverallScore = 1.0
	}
	if report.OverallScore < 0 {
		report.OverallScore = 0
	}

	// 追加通用建议
	report.Suggestions = append(report.Suggestions, s.generateQualitySuggestions(report)...)

	// 记录检查方式和分数分布
	method := "rule"
	if aiScores != nil {
		method = "ai"
	}
	metrics.QualityCheckTotal.WithLabelValues(method).Inc()
	metrics.QualityScoreOverall.Observe(report.OverallScore)
	metrics.QualityScoreByDimension.WithLabelValues("logic").Observe(report.LogicScore)
	metrics.QualityScoreByDimension.WithLabelValues("consistency").Observe(report.ConsistencyScore)
	metrics.QualityScoreByDimension.WithLabelValues("quality").Observe(report.QualityScore)
	metrics.QualityScoreByDimension.WithLabelValues("style").Observe(report.StyleScore)
	metrics.QualityScoreByDimension.WithLabelValues("dramatic").Observe(report.DramaticScore)

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
	totalChars := len([]rune(chapter.Content))
	if totalChars > 0 {
		dialogueRatio := float64(countDialogueChars(chapter.Content)) / float64(totalChars)
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
	totalChars := len([]rune(chapter.Content))
	if totalChars > 0 {
		dialogueRatio := float64(countDialogueChars(chapter.Content)) / float64(totalChars)
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

// conflictKeywords 表示冲突/阻碍/意外的关键词
var conflictKeywords = []string{
	"但是", "然而", "却", "不料", "没想到", "出乎意料", "意外", "突然改变",
	"失败", "阻碍", "阻止", "拦截", "发现问题", "出了差错", "计划落空",
	"不对", "不妙", "危险", "威胁", "陷阱", "背叛",
}

// calcDramaticScore 基于规则计算戏剧性分数
func (s *QualityControlService) calcDramaticScore(chapter *model.Chapter) float64 {
	if chapter.Content == "" {
		return 0.5
	}
	score := 0.5
	content := chapter.Content
	totalRunes := len([]rune(content))

	// 1. 冲突密度：冲突关键词出现频率
	conflictCount := 0
	for _, kw := range conflictKeywords {
		conflictCount += strings.Count(content, kw)
	}
	if totalRunes > 0 {
		density := float64(conflictCount) / float64(totalRunes) * 1000 // 每千字冲突词数
		if density >= 3 {
			score += 0.2
		} else if density >= 1.5 {
			score += 0.1
		} else if density < 0.5 {
			score -= 0.15 // 冲突密度过低
		}
	}

	// 2. 悬念收尾：检查结尾段落
	paragraphs := strings.Split(strings.TrimSpace(content), "\n")
	lastPara := ""
	for i := len(paragraphs) - 1; i >= 0; i-- {
		p := strings.TrimSpace(paragraphs[i])
		if len([]rune(p)) >= 10 {
			lastPara = p
			break
		}
	}
	hookIndicators := []string{"?", "？", "……", "...", "不知", "会不会", "什么", "为什么", "难道", "竟然", "不可能"}
	hasHook := false
	for _, ind := range hookIndicators {
		if strings.Contains(lastPara, ind) {
			hasHook = true
			break
		}
	}
	if hasHook {
		score += 0.15
	} else {
		score -= 0.1 // 平淡收场扣分
	}

	// 3. 顺序陈述检测：连续"然后/接着/于是"的线性推进
	sequentialCount := strings.Count(content, "然后") + strings.Count(content, "于是") + strings.Count(content, "接着")
	if totalRunes > 0 {
		seqDensity := float64(sequentialCount) / float64(totalRunes) * 1000
		if seqDensity > 3 {
			score -= 0.15 // 顺序陈述过多
		}
	}

	if score > 1.0 {
		score = 1.0
	}
	if score < 0 {
		score = 0
	}
	return score
}

func (s *QualityControlService) checkDramatic(chapter *model.Chapter) []QualityIssue {
	issues := []QualityIssue{}
	if chapter.Content == "" {
		return issues
	}

	conflictCount := 0
	for _, kw := range conflictKeywords {
		conflictCount += strings.Count(chapter.Content, kw)
	}
	totalRunes := len([]rune(chapter.Content))
	if totalRunes > 1000 {
		density := float64(conflictCount) / float64(totalRunes) * 1000
		if density < 1.0 {
			issues = append(issues, QualityIssue{
				Type:        "dramatic",
				Severity:    "medium",
				Description: "冲突密度不足：章节中阻碍/意外/对立情节偏少，情节推进过于顺畅",
				Suggestion:  "建议在主角行动推进中插入意外阻碍、信息反转或对立角色的干扰",
			})
		}
	}

	sequentialCount := strings.Count(chapter.Content, "然后") + strings.Count(chapter.Content, "于是") + strings.Count(chapter.Content, "接着")
	if sequentialCount > 8 {
		issues = append(issues, QualityIssue{
			Type:        "dramatic",
			Severity:    "low",
			Description: fmt.Sprintf("顺序叙述过多：「然后/于是/接着」出现%d次，情节推进缺乏波折", sequentialCount),
			Suggestion:  "建议将部分线性段落改写为因果冲突或内心挣扎",
		})
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

	// Append dramatic-specific suggestions
	if report.DramaticScore < 0.6 && !seen["建议增加情节冲突和反转"] {
		seen["建议增加情节冲突和反转"] = true
		suggestions = append(suggestions, "建议增加情节冲突和反转：在主角顺利推进时插入意外阻碍，结尾留下悬念钩子")
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

	// Resolve tenant ID from the chapter's novel.
	var tenantID uint
	if novel, err := s.novelRepo.GetByID(chapter.NovelID); err == nil {
		tenantID = novel.TenantID
	}

	var sb strings.Builder
	for i, sg := range suggestions {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, sg))
	}

	prompt, err := renderPrompt("quality_refine", map[string]interface{}{
		"Suggestions": sb.String(),
		"Content":     chapter.Content,
	})
	if err != nil {
		return "", fmt.Errorf("render quality_refine: %w", err)
	}

	result, err := s.aiSvc.GenerateWithProvider(tenantID, chapter.NovelID, "quality_refine", prompt, "")
	if err != nil {
		return "", fmt.Errorf("AI refine failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}

// RewriteByInstruction rewrites a chapter according to a user-supplied natural-language instruction.
// Returns the modified full chapter content (not saved — caller persists it).
func (s *QualityControlService) RewriteByInstruction(ctx context.Context, chapterID uint, instruction string) (string, error) {
	if s.aiSvc == nil {
		return "", fmt.Errorf("AI client not initialized")
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return "", fmt.Errorf("chapter not found: %w", err)
	}
	if chapter.Content == "" {
		return "", fmt.Errorf("chapter has no content to rewrite")
	}
	if strings.TrimSpace(instruction) == "" {
		return "", fmt.Errorf("instruction must not be empty")
	}

	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return "", fmt.Errorf("novel %d not found: %w", chapter.NovelID, err)
	}

	prompt, err := renderPrompt("chapter_rewrite", map[string]interface{}{
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      chapter.Content,
		"Instruction":  instruction,
	})
	if err != nil {
		return "", fmt.Errorf("render chapter_rewrite: %w", err)
	}

	result, err := s.aiSvc.GenerateWithProvider(novel.TenantID, novel.ID, "chapter_rewrite", prompt, "")
	if err != nil {
		return "", fmt.Errorf("AI rewrite failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}

// RefineSelection rewrites a short selected text fragment according to a user instruction.
// Returns only the refined fragment (not saved — caller replaces it in the content).
func (s *QualityControlService) RefineSelection(ctx context.Context, chapterID uint, selectedText, instruction string) (string, error) {
	if s.aiSvc == nil {
		return "", fmt.Errorf("AI client not initialized")
	}
	if strings.TrimSpace(selectedText) == "" {
		return "", fmt.Errorf("selected text must not be empty")
	}
	if strings.TrimSpace(instruction) == "" {
		return "", fmt.Errorf("instruction must not be empty")
	}

	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return "", fmt.Errorf("chapter not found: %w", err)
	}
	if chapter.Content == "" {
		return "", fmt.Errorf("chapter has no content")
	}

	prompt, err := renderPrompt("selection_refine", map[string]interface{}{
		"ChapterNo":      chapter.ChapterNo,
		"ChapterTitle":   chapter.Title,
		"ChapterContent": chapter.Content,
		"SelectedText":   selectedText,
		"Instruction":    instruction,
	})
	if err != nil {
		return "", fmt.Errorf("render selection_refine: %w", err)
	}

	var tenantID uint
	if novel, nErr := s.novelRepo.GetByID(chapter.NovelID); nErr == nil {
		tenantID = novel.TenantID
	}
	result, err := s.aiSvc.GenerateWithProvider(tenantID, chapter.NovelID, "selection_refine", prompt, "")
	if err != nil {
		return "", fmt.Errorf("AI refine selection failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}

// ─── Chapter AI Review ────────────────────────────────────────────────────────

// ReviewChapter performs a deep AI review of a chapter and stores the record.
func (s *QualityControlService) ReviewChapter(ctx context.Context, chapterID uint, provider string) (retReview *model.ChapterReview, retErr error) {
	reviewStart := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ChapterDeepReviewTotal.WithLabelValues(status).Inc()
		metrics.ChapterDeepReviewDuration.Observe(time.Since(reviewStart).Seconds())
	}()
	if s.reviewRecordRepo == nil {
		return nil, fmt.Errorf("review repos not wired")
	}

	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter %d not found: %w", chapterID, err)
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel %d not found: %w", chapter.NovelID, err)
	}

	// Fetch previous review score and weaknesses for verification mode
	var previousScore float64
	var previousWeaknessesText string
	if s.reviewRecordRepo != nil {
		if recs, err2 := s.reviewRecordRepo.ListByEntity(model.ReviewEntityChapter, chapterID); err2 == nil && len(recs) > 0 {
			previousScore = recs[0].OverallScore
			// Extract weaknesses from previous review JSON for verification mode
			if recs[0].ReviewJSON != "" {
				var prevReview model.ChapterReview
				if json.Unmarshal([]byte(recs[0].ReviewJSON), &prevReview) == nil && len(prevReview.Weaknesses) > 0 {
					var wLines []string
					for i, w := range prevReview.Weaknesses {
						line := fmt.Sprintf("%d. 【问题】%s", i+1, w.Issue)
						if w.Suggestion != "" {
							line += "  【上轮建议】" + w.Suggestion
						}
						wLines = append(wLines, line)
					}
					previousWeaknessesText = strings.Join(wLines, "\n")
				}
			}
		}
	}

	// Fetch user-ignored issues to exclude from this review
	var ignoredLines []string
	if s.ignoredIssueRepo != nil {
		if items, err2 := s.ignoredIssueRepo.ListByEntity(model.ReviewEntityChapter, chapterID); err2 == nil {
			for _, item := range items {
				ignoredLines = append(ignoredLines, item.IssueText)
			}
		}
	}

	// Fetch up to 2 previous chapters as cross-chapter context
	type prevChapterInfo struct {
		ChapterNo int
		Title     string
		Content   string
	}
	var prevChapters []prevChapterInfo
	if recent, err2 := s.chapterRepo.GetRecent(novel.ID, chapter.ChapterNo, 2); err2 == nil {
		// GetRecent returns DESC order; reverse to chronological
		for i := len(recent) - 1; i >= 0; i-- {
			c := recent[i]
			content := c.Content
			if len([]rune(content)) > 1500 {
				runes := []rune(content)
				content = string(runes[:1500]) + "…（已截断）"
			}
			prevChapters = append(prevChapters, prevChapterInfo{
				ChapterNo: c.ChapterNo,
				Title:     c.Title,
				Content:   content,
			})
		}
	}

	// Build numbered paragraph list for target chapter.
	// Uses splitContentParagraphs so the indices sent to the AI match exactly
	// what ApplyDiffs will use when the user applies suggestions.
	paragraphs, _ := splitContentParagraphs(chapter.Content)
	var sb strings.Builder
	for i, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		fmt.Fprintf(&sb, "[%d] %s\n\n", i, p)
	}

	charSummary := s.buildCharacterVoiceSummary(chapter.NovelID)
	arcContext := s.buildArcContext(chapter.NovelID, chapter.ChapterNo)
	foreshadowContext := s.buildForeshadowContext(chapter.NovelID)

	prompt, err := renderPrompt("chapter_review", map[string]interface{}{
		"Genre":              novel.Genre,
		"CharacterSummary":   charSummary,
		"ChapterNo":          chapter.ChapterNo,
		"ChapterTitle":       chapter.Title,
		"HasPrevChapters":    len(prevChapters) > 0,
		"PrevChapters":       prevChapters,
		"Content":            sb.String(),
		"HasIgnored":         len(ignoredLines) > 0,
		"IgnoredText":        strings.Join(ignoredLines, "\n"),
		"HasPreviousScore":    previousScore > 0,
		"PreviousScoreStr":    fmt.Sprintf("%.0f", previousScore),
		"PreviousWeaknesses":  previousWeaknessesText,
		"CoreTheme":           novel.CoreTheme,
		"HasArcContext":      arcContext != "",
		"ArcContext":         arcContext,
		"HasForeshadows":     foreshadowContext != "",
		"OpenForeshadows":    foreshadowContext,
	})
	if err != nil {
		return nil, fmt.Errorf("render chapter_review: %w", err)
	}

	raw, err := s.aiSvc.GenerateWithProvider(novel.TenantID, novel.ID, "chapter_review", prompt, provider)
	if err != nil {
		return nil, fmt.Errorf("AI chapter review failed: %w", err)
	}

	// Strip markdown code fences that LLMs sometimes add
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if idx := strings.Index(raw, "\n"); idx != -1 {
			raw = raw[idx+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}

	content := extractJSONObject(raw)
	var review model.ChapterReview
	if err := json.Unmarshal([]byte(content), &review); err != nil {
		// 兜底修复：移除字符串外的中文注释，补全缺失逗号
		repaired := repairAIJSON(content)
		if err2 := json.Unmarshal([]byte(repaired), &review); err2 != nil {
			return nil, fmt.Errorf("parse chapter review JSON: %w (raw: %.200s)", err, content)
		}
		logger.Errorf("[ReviewChapter] JSON repaired successfully (original err: %v)", err)
	}

	// Persist record
	reviewBytes, err := json.Marshal(review)
	if err != nil {
		return nil, fmt.Errorf("marshal review result: %w", err)
	}
	rec := &model.ReviewRecord{
		EntityType:   model.ReviewEntityChapter,
		EntityID:     chapterID,
		OverallScore: review.OverallScore,
		Status:       "pending",
		ReviewJSON:   string(reviewBytes),
		SnapshotJSON: chapter.Content,
	}
	if err := s.reviewRecordRepo.Create(rec); err != nil {
		return nil, fmt.Errorf("save review record: %w", err)
	}
	review.RecordID = rec.ID

	return &review, nil
}

func (s *QualityControlService) ListReviewRecords(chapterID uint) ([]*model.ReviewRecord, error) {
	if s.reviewRecordRepo == nil {
		return nil, nil
	}
	return s.reviewRecordRepo.ListByEntity(model.ReviewEntityChapter, chapterID)
}

func (s *QualityControlService) GetReviewRecord(recordID uint, tenantID uint) (*model.ReviewRecord, error) {
	if s.reviewRecordRepo == nil {
		return nil, fmt.Errorf("review repos not wired")
	}
	rec, err := s.reviewRecordRepo.GetByID(recordID)
	if err != nil {
		return nil, err
	}
	// 验证 entity 属于当前租户（chapter 验证）
	if rec.EntityType == model.ReviewEntityChapter {
		chapter, err := s.chapterRepo.GetByID(rec.EntityID)
		if err != nil || !s.chapterBelongsToTenant(chapter, tenantID) {
			return nil, fmt.Errorf("review record not found")
		}
	}
	return rec, nil
}

// RollbackReview restores the chapter content to the snapshot taken at review time.
// chapterID and tenantID are used to verify the caller owns the chapter.
func (s *QualityControlService) RollbackReview(recordID, chapterID, tenantID uint) error {
	if s.reviewRecordRepo == nil {
		return fmt.Errorf("review repos not wired")
	}
	rec, err := s.reviewRecordRepo.GetByID(recordID)
	if err != nil {
		return fmt.Errorf("record %d not found: %w", recordID, err)
	}
	// Verify the record actually belongs to the requested chapter.
	if rec.EntityID != chapterID {
		return fmt.Errorf("record does not belong to this chapter")
	}
	if rec.SnapshotJSON == "" {
		return fmt.Errorf("no snapshot available for record %d", recordID)
	}
	chapter, err := s.chapterRepo.GetByID(rec.EntityID)
	if err != nil {
		return fmt.Errorf("chapter %d not found: %w", rec.EntityID, err)
	}
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return fmt.Errorf("permission denied")
	}
	chapter.Content = rec.SnapshotJSON
	if err := s.chapterRepo.Update(chapter); err != nil {
		return fmt.Errorf("restore chapter content: %w", err)
	}
	rec.Status = "rolled_back"
	return s.reviewRecordRepo.Update(rec)
}

// splitContentParagraphs splits novel content into non-empty paragraphs.
// Tries \n\n first (standard blank-line separation); if that yields only one
// block, falls back to \n so single-newline formatted content also works.
// Returns paragraphs with their original indices preserved (empty slots kept as "").
func splitContentParagraphs(content string) (paragraphs []string, sep string) {
	content = strings.TrimSpace(content)
	parts := strings.Split(content, "\n\n")
	nonEmpty := 0
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty++
		}
	}
	if nonEmpty > 1 {
		return parts, "\n\n"
	}
	// Fallback: treat each non-empty line as a paragraph.
	lines := strings.Split(content, "\n")
	return lines, "\n"
}

// ApplyDiffs replaces selected paragraphs in the chapter content.
// Returns the number of paragraphs actually replaced.
func (s *QualityControlService) ApplyDiffs(chapterID uint, diffs []ParagraphDiff, recordID uint, tenantID uint) (int, error) {
	if len(diffs) == 0 {
		return 0, nil
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return 0, fmt.Errorf("chapter %d not found: %w", chapterID, err)
	}
	if tenantID != 0 && !s.chapterBelongsToTenant(chapter, tenantID) {
		return 0, fmt.Errorf("not found")
	}

	rawParagraphs, sep := splitContentParagraphs(chapter.Content)

	// Fix 3: 先过滤空段落，再做索引校验，避免过滤后索引失效
	paragraphs := rawParagraphs[:0]
	for _, p := range rawParagraphs {
		if strings.TrimSpace(p) != "" {
			paragraphs = append(paragraphs, p)
		}
	}

	logger.Printf("[ApplyDiffs] chapterID=%d paragraphs=%d sep=%q diffs=%d",
		chapterID, len(paragraphs), sep, len(diffs))

	// Validate for duplicate indices — silent overwrite would cause data loss.
	indexSeen := make(map[int]bool, len(diffs))
	for _, d := range diffs {
		if indexSeen[d.Index] {
			return 0, fmt.Errorf("duplicate diff index %d: each paragraph can only be replaced once", d.Index)
		}
		indexSeen[d.Index] = true
	}

	// Validate that all indices are within range (against filtered paragraphs).
	for _, d := range diffs {
		if d.Index < 0 || d.Index >= len(paragraphs) {
			return 0, fmt.Errorf("diff index %d out of range (chapter has %d paragraphs)", d.Index, len(paragraphs))
		}
	}

	diffMap := make(map[int]string, len(diffs))
	for _, d := range diffs {
		diffMap[d.Index] = d.NewContent
		logger.Printf("[ApplyDiffs] diff index=%d origText=%.40q", d.Index, d.OrigText)
	}

	// Index-based replacement (matches the numbered paragraphs sent during review).
	indexApplied := make(map[int]bool, len(diffs))
	applied := 0
	for i := range paragraphs {
		if newP, ok := diffMap[i]; ok {
			paragraphs[i] = newP // empty string = marked for deletion
			applied++
			indexApplied[i] = true
		}
	}
	logger.Printf("[ApplyDiffs] index-based applied=%d", applied)

	// Fix 4: 对每个未被索引匹配的 diff 单独执行 OrigText 前缀 fallback，
	// 而不是 applied==0 才整体 fallback（确保即使部分 diff 已匹配，剩余仍能走 fallback）。
	matchedParagraphs := make(map[int]bool, len(diffs))
	// 先标记已被 index 替换的段落
	for i := range indexApplied {
		matchedParagraphs[i] = true
	}
	for di, d := range diffs {
		if d.OrigText == "" {
			continue
		}
		// 已经被 index 成功匹配就跳过
		if indexApplied[d.Index] {
			_ = di
			continue
		}
		// 在所有段落中寻找前缀匹配
		prefix := []rune(d.OrigText)
		if len(prefix) > 80 {
			prefix = prefix[:80]
		}
		for i, p := range paragraphs {
			if matchedParagraphs[i] {
				continue
			}
			trimmed := strings.TrimSpace(p)
			if trimmed == "" {
				continue
			}
			pRunes := []rune(trimmed)
			matchLen := len(prefix)
			if matchLen > len(pRunes) {
				matchLen = len(pRunes)
			}
			if string(pRunes[:matchLen]) == string(prefix[:matchLen]) {
				paragraphs[i] = d.NewContent
				applied++
				matchedParagraphs[i] = true
				break
			}
		}
	}

	// Filter out deleted paragraphs and trim each.
	kept := make([]string, 0, len(paragraphs))
	for _, p := range paragraphs {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}

	chapter.Content = strings.Join(kept, "\n\n")
	if err := s.chapterRepo.Update(chapter); err != nil {
		metrics.ChapterApplyDiffsTotal.WithLabelValues("error").Inc()
		return 0, fmt.Errorf("update chapter content: %w", err)
	}

	// Mark record as applied if provided
	if recordID > 0 && s.reviewRecordRepo != nil {
		if rec, err := s.reviewRecordRepo.GetByID(recordID); err == nil {
			now := time.Now()
			rec.Status = "applied"
			rec.AppliedAt = &now
			_ = s.reviewRecordRepo.Update(rec)
		}
	}

	metrics.ChapterApplyDiffsTotal.WithLabelValues("success").Inc()
	metrics.ChapterDiffsApplied.Add(float64(applied))
	return applied, nil
}

func (s *QualityControlService) ListIgnoredIssues(chapterID uint) ([]*model.IgnoredReviewIssue, error) {
	if s.ignoredIssueRepo == nil {
		return nil, nil
	}
	return s.ignoredIssueRepo.ListByEntity(model.ReviewEntityChapter, chapterID)
}

// IgnoreIssue adds an issue to the ignored list (idempotent by hash).
func (s *QualityControlService) IgnoreIssue(chapterID uint, issueText, note string) error {
	if s.ignoredIssueRepo == nil {
		return fmt.Errorf("ignore repo not wired")
	}
	h := sha256.Sum256([]byte(issueText))
	hash := hex.EncodeToString(h[:])
	item := &model.IgnoredReviewIssue{
		EntityType: model.ReviewEntityChapter,
		EntityID:   chapterID,
		IssueText:  issueText,
		IssueHash:  hash,
		Note:       note,
	}
	return s.ignoredIssueRepo.Create(item)
}

func (s *QualityControlService) UnignoreIssue(issueID uint) error {
	if s.ignoredIssueRepo == nil {
		return fmt.Errorf("ignore repo not wired")
	}
	return s.ignoredIssueRepo.Delete(issueID)
}

// RunAutoReview 在章节生成后自动执行最多 rounds 轮 AI 深度审查 + 自动应用修改。
// 结束条件（任一满足即停止）：
//   1. 已完成 rounds 轮；
//   2. 当轮评分 >= minScore（minScore=0 表示不设分数阈值，只按轮数控制）。
//
// 返回：最终分数、总共应用的段落数、遇到的错误（非致命，调用方可忽略）。
func (s *QualityControlService) RunAutoReview(
	ctx context.Context,
	chapterID uint,
	tenantID uint,
	rounds int,
	minScore float64,
) (finalScore float64, totalApplied int, err error) {
	if rounds <= 0 {
		return 0, 0, nil
	}
	if rounds > 5 {
		rounds = 5
	}
	// minScore=0 表示不启用分数阈值条件，只按轮数控制
	useScoreThreshold := minScore > 0

	for round := 1; round <= rounds; round++ {
		logger.Printf("[AutoReview] chapterID=%d round=%d/%d scoreThreshold=%.1f", chapterID, round, rounds, minScore)

		review, reviewErr := s.ReviewChapter(ctx, chapterID, "")
		if reviewErr != nil {
			logger.Errorf("[AutoReview] chapterID=%d round=%d review failed: %v", chapterID, round, reviewErr)
			return finalScore, totalApplied, reviewErr
		}
		finalScore = review.OverallScore
		logger.Printf("[AutoReview] chapterID=%d round=%d score=%.1f", chapterID, round, finalScore)

		// 条件2：分数达标，停止
		if useScoreThreshold && finalScore >= minScore {
			logger.Printf("[AutoReview] chapterID=%d round=%d: score %.1f >= threshold %.1f, stopping (score condition)",
				chapterID, round, finalScore, minScore)
			break
		}

		// 收集 error/warning 级别的改写建议
		var diffs []ParagraphDiff
		for _, pf := range review.ParagraphFeedback {
			if pf.Action != "rewrite" || pf.SuggestedRewrite == "" {
				continue
			}
			if pf.Severity != "error" && pf.Severity != "warning" {
				continue
			}
			diffs = append(diffs, ParagraphDiff{
				Index:      pf.Index,
				NewContent: pf.SuggestedRewrite,
				OrigText:   pf.OrigText,
			})
		}

		if len(diffs) == 0 {
			logger.Printf("[AutoReview] chapterID=%d round=%d: no applicable diffs, stopping", chapterID, round)
			break
		}

		applied, applyErr := s.ApplyDiffs(chapterID, diffs, review.RecordID, tenantID)
		if applyErr != nil {
			logger.Errorf("[AutoReview] chapterID=%d round=%d ApplyDiffs failed: %v", chapterID, round, applyErr)
			return finalScore, totalApplied, applyErr
		}
		totalApplied += applied
		logger.Printf("[AutoReview] chapterID=%d round=%d: applied %d/%d diffs", chapterID, round, applied, len(diffs))

		if applied == 0 {
			break
		}
	}
	return finalScore, totalApplied, nil
}

// ── Context builders ──────────────────────────────────────────────────────────

// buildCharacterVoiceSummary builds a compact voice profile summary for all
// major characters in the novel. This is injected into the chapter review
// prompt so the AI can check dialogue voice consistency.
func (s *QualityControlService) buildCharacterVoiceSummary(novelID uint) string {
	if s.characterRepo == nil {
		return ""
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil || len(chars) == 0 {
		return ""
	}
	var lines []string
	for _, c := range chars {
		if c.VoiceProfile == "" && c.Description == "" {
			continue
		}
		// Only include protagonist/antagonist/supporting roles
		role := c.Role
		if role != "protagonist" && role != "antagonist" && role != "supporting" {
			continue
		}
		var voiceLine string
		if c.VoiceProfile != "" {
			// Extract overall_voice field from JSON without full parse
			var vp struct {
				OverallVoice   string   `json:"overall_voice"`
				SpeechHabits   []string `json:"speech_habits"`
				VocabularyLevel string  `json:"vocabulary_level"`
			}
			if err2 := json.Unmarshal([]byte(c.VoiceProfile), &vp); err2 == nil && vp.OverallVoice != "" {
				voiceLine = vp.OverallVoice
				if len(vp.SpeechHabits) > 0 {
					habits := vp.SpeechHabits
					if len(habits) > 3 {
						habits = habits[:3]
					}
					voiceLine += "；口头禅/习惯：" + strings.Join(habits, "、")
				}
			}
		}
		if voiceLine == "" && c.Description != "" {
			desc := c.Description
			if len([]rune(desc)) > 60 {
				desc = string([]rune(desc)[:60]) + "…"
			}
			voiceLine = desc
		}
		lines = append(lines, fmt.Sprintf("- %s（%s）：%s", c.Name, roleLabel(role), voiceLine))
	}
	return strings.Join(lines, "\n")
}

func roleLabel(role string) string {
	switch role {
	case "protagonist":
		return "主角"
	case "antagonist":
		return "反派"
	case "supporting":
		return "配角"
	default:
		return role
	}
}

// buildArcContext returns arc summaries for arcs that precede the current
// chapter, giving the reviewer long-range narrative context.
func (s *QualityControlService) buildArcContext(novelID uint, currentChapterNo int) string {
	if s.arcSummaryRepo == nil {
		return ""
	}
	arcs, err := s.arcSummaryRepo.ListByNovel(novelID)
	if err != nil || len(arcs) == 0 {
		return ""
	}
	var lines []string
	for _, arc := range arcs {
		if arc.EndChapter >= currentChapterNo {
			continue // only past arcs provide useful context
		}
		summary := arc.Summary
		if len([]rune(summary)) > 120 {
			summary = string([]rune(summary)[:120]) + "…"
		}
		lines = append(lines, fmt.Sprintf("- 第%d-%d章（第%d弧）：%s", arc.StartChapter, arc.EndChapter, arc.ArcNo, summary))
	}
	return strings.Join(lines, "\n")
}

// buildForeshadowContext returns open (unresolved) foreshadows for injection
// into the chapter review prompt.
func (s *QualityControlService) buildForeshadowContext(novelID uint) string {
	if s.foreshadowRepo == nil {
		return ""
	}
	foreshadows, err := s.foreshadowRepo.ListUnfulfilled(novelID)
	if err != nil || len(foreshadows) == 0 {
		return ""
	}
	var lines []string
	for _, f := range foreshadows {
		desc := f.Description
		if len([]rune(desc)) > 60 {
			desc = string([]rune(desc)[:60]) + "…"
		}
		lines = append(lines, fmt.Sprintf("- 【%s】%s", f.Title, desc))
	}
	return strings.Join(lines, "\n")
}

// ── Batch chapter content review ─────────────────────────────────────────────

// BatchReviewNovelChapters reviews all chapters with content in parallel
// (max 3 concurrent). It skips chapters with no content.
// progressFn is called with (done, total) after each chapter finishes.
func (s *QualityControlService) BatchReviewNovelChapters(ctx context.Context, tenantID, novelID uint, progressFn func(done, total int)) error {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return fmt.Errorf("novel not found: %w", err)
	}
	if novel.TenantID != tenantID {
		return fmt.Errorf("not found")
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return err
	}

	var reviewable []*model.Chapter
	for _, ch := range chapters {
		if strings.TrimSpace(ch.Content) != "" {
			reviewable = append(reviewable, ch)
		}
	}
	if len(reviewable) == 0 {
		return fmt.Errorf("没有找到有正文内容的章节，请先生成章节内容")
	}

	const concurrency = 3
	sem := make(chan struct{}, concurrency)
	total := len(reviewable)
	done := int32(0)
	var wg sync.WaitGroup

outerLoop:
	for _, ch := range reviewable {
		select {
		case <-ctx.Done():
			break outerLoop
		default:
		}
		wg.Add(1)
		go func(ch *model.Chapter) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if _, err := s.ReviewChapter(ctx, ch.ID, ""); err != nil {
				logger.Errorf("[BatchReviewChapters] chapter %d failed: %v", ch.ChapterNo, err)
			}
			n := int(atomic.AddInt32(&done, 1))
			if progressFn != nil {
				progressFn(n, total)
			}
		}(ch)
	}
	wg.Wait()
	return nil
}
