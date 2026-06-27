package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// OutlineReviewService 章节大纲审查服务
type OutlineReviewService struct {
	reviewRepo    *repository.OutlineReviewRepository
	synthesisRepo *repository.NovelOutlineSynthesisRepository
	chapterRepo   interface {
		GetByID(id uint) (*model.Chapter, error)
		ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
		GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error)
	}
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
	aiService      *AIService
	foreshadowRepo interface {
		ListByNovel(novelID uint) ([]*model.Foreshadow, error)
	}
	arcSummaryRepo interface {
		ListByNovel(novelID uint) ([]*model.ArcSummary, error)
	}
}

func (s *OutlineReviewService) WithForeshadowRepo(r interface {
	ListByNovel(novelID uint) ([]*model.Foreshadow, error)
}) *OutlineReviewService {
	s.foreshadowRepo = r
	return s
}

func (s *OutlineReviewService) WithArcSummaryRepo(r interface {
	ListByNovel(novelID uint) ([]*model.ArcSummary, error)
}) *OutlineReviewService {
	s.arcSummaryRepo = r
	return s
}

func NewOutlineReviewService(
	reviewRepo *repository.OutlineReviewRepository,
	synthesisRepo *repository.NovelOutlineSynthesisRepository,
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
		GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error)
	},
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	},
	aiService *AIService,
) *OutlineReviewService {
	return &OutlineReviewService{
		reviewRepo:    reviewRepo,
		synthesisRepo: synthesisRepo,
		chapterRepo:   chapterRepo,
		novelRepo:     novelRepo,
		aiService:     aiService,
	}
}

// chapterBelongsToTenant verifies chapter ownership via novel.TenantID (novel-based ownership).
func (s *OutlineReviewService) chapterBelongsToTenant(chapter *model.Chapter, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
}

// novelBelongsToTenant verifies novel ownership via novel.TenantID.
func (s *OutlineReviewService) novelBelongsToTenant(novelID uint, tenantID uint) bool {
	if s.novelRepo == nil {
		return true
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return false
	}
	return novel.TenantID == 0 || novel.TenantID == tenantID
}

// BatchReviewResult 批量审查返回值（章级结果 + 小说级综合报告）
type BatchReviewResult struct {
	Reviews   []*model.OutlineReview
	Synthesis *model.NovelOutlineSynthesis
}

// ReviewChapterOutline 审查单章大纲（含规则检查 + AI审查）
func (s *OutlineReviewService) ReviewChapterOutline(ctx context.Context, tenantID, chapterID uint) (*model.OutlineReview, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	if !s.chapterBelongsToTenant(chapter, tenantID) {
		return nil, fmt.Errorf("not found")
	}

	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel not found: %w", err)
	}

	now := time.Now()
	review := &model.OutlineReview{
		NovelID: chapter.NovelID,
		ChapterID:  chapterID,
		ChapterNo:  chapter.ChapterNo,
		Status:     "reviewing",
		ReviewedAt: &now,
	}

	// ── 阶段1：规则检查（快速，不依赖 AI）────────────────
	ruleIssues := s.runRuleChecks(chapter)

	// ── 阶段2：AI 审查 ──────────────────────────────────
	aiResult, aiErr := s.runAIReview(ctx, tenantID, chapter, novel)
	if aiErr != nil {
		logger.Errorf("[OutlineReview] AI review failed for chapter %d: %v, falling back to rule-only", chapterID, aiErr)
		review = s.buildRuleOnlyReview(review, ruleIssues)
	} else {
		allIssues := append(ruleIssues, aiResult.Issues...)
		review.Scores.StructureScore = aiResult.DimensionScores.Structure
		review.Scores.PacingScore = aiResult.DimensionScores.Pacing
		review.Scores.ContinuityScore = aiResult.DimensionScores.Continuity
		review.Scores.CharacterScore = aiResult.DimensionScores.Character
		review.Scores.ConflictScore = aiResult.DimensionScores.Conflict
		review.Scores.HookScore = aiResult.DimensionScores.Hook
		// 用体裁权重重新计算综合分（覆盖 AI 的等权计算）
		review.OverallScore = applyWeightedScore(*aiResult, novel.Meta.Genre)
		review.Content.Suggestion = aiResult.Suggestion

		if b, _ := json.Marshal(allIssues); b != nil {
			review.Content.IssuesJSON = string(b)
		}
		if b, _ := json.Marshal(aiResult.Highlights); b != nil {
			review.Content.HighlightsJSON = string(b)
		}
	}

	review.Status = scoreToStatus(review.OverallScore)

	if err := s.reviewRepo.Upsert(review); err != nil {
		return nil, fmt.Errorf("save review: %w", err)
	}
	return review, nil
}

// BatchReviewNovel 批量审查小说所有章节大纲（并行 + 综合报告）
func (s *OutlineReviewService) BatchReviewNovel(ctx context.Context, tenantID, novelID uint, progressFn func(done, total int)) (*BatchReviewResult, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, fmt.Errorf("novel not found: %w", err)
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, err
	}

	// 纳入有场景大纲、文字大纲或章节正文的章节
	var reviewable []*model.Chapter
	for _, ch := range chapters {
		hasScene := strings.TrimSpace(ch.NarrativeMeta.SceneOutline) != "" && strings.TrimSpace(ch.NarrativeMeta.SceneOutline) != "{}"
		hasOutline := strings.TrimSpace(ch.NarrativeMeta.Outline) != ""
		hasContent := strings.TrimSpace(ch.Content) != ""
		if hasScene || hasOutline || hasContent {
			reviewable = append(reviewable, ch)
		}
	}

	if len(reviewable) == 0 {
		return nil, fmt.Errorf("没有找到可审查的章节，请先生成章节内容或大纲")
	}

	// ── 并行审查（最多3个并发）────────────────────────────
	const concurrency = 3
	sem := make(chan struct{}, concurrency)

	var (
		mu      sync.Mutex
		results []*model.OutlineReview
		done    int32
	)
	total := len(reviewable)

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

			review, err := s.ReviewChapterOutline(ctx, tenantID, ch.ID)
			if err != nil {
				logger.Errorf("[OutlineReview] batch review chapter %d failed: %v", ch.ChapterNo, err)
				now := time.Now()
				review = &model.OutlineReview{
					NovelID:   novelID,
					ChapterID: ch.ID,
					ChapterNo: ch.ChapterNo,
					Status:    "failed",
					Content: model.OutlineReviewContent{
						Suggestion: err.Error(),
					},
					ReviewedAt: &now,
				}
				if saveErr := s.reviewRepo.Upsert(review); saveErr != nil {
					logger.Errorf("[OutlineReview] save failed review for chapter %d: %v", ch.ChapterNo, saveErr)
				}
			}

			mu.Lock()
			results = append(results, review)
			mu.Unlock()

			n := int(atomic.AddInt32(&done, 1))
			if progressFn != nil {
				// 个别章审查阶段占 0-80%
				progressFn(n*80/total, 100)
			}
		}(ch)
	}
	wg.Wait()

	// 按章节号排序
	sortReviewsByChapterNo(results)

	// ── 全局综合报告（占进度 80-100%）────────────────────
	if progressFn != nil {
		progressFn(82, 100)
	}
	synthesis := s.buildSynthesis(ctx, tenantID, novel, chapters, results)
	if s.synthesisRepo != nil {
		if err := s.synthesisRepo.Upsert(synthesis); err != nil {
			logger.Errorf("[OutlineReview] save synthesis failed: %v", err)
		}
	}
	if progressFn != nil {
		progressFn(100, 100)
	}

	return &BatchReviewResult{Reviews: results, Synthesis: synthesis}, nil
}

// GetReview 获取章节的最新审查结果
func (s *OutlineReviewService) GetReview(tenantID, chapterID uint) (*model.OutlineReview, error) {
	review, err := s.reviewRepo.GetByChapter(chapterID)
	if err != nil {
		return nil, err
	}
	if !s.novelBelongsToTenant(review.NovelID, tenantID) {
		return nil, fmt.Errorf("not found")
	}
	return review, nil
}

// ListNovelReviews 获取小说所有章节的审查结果
func (s *OutlineReviewService) ListNovelReviews(tenantID, novelID uint) ([]*model.OutlineReview, error) {
	return s.reviewRepo.ListByNovel(novelID)
}

// GetSynthesis 获取小说综合审查报告
func (s *OutlineReviewService) GetSynthesis(tenantID, novelID uint) (*model.NovelOutlineSynthesis, error) {
	if s.synthesisRepo == nil {
		return nil, fmt.Errorf("synthesis repo not available")
	}
	syn, err := s.synthesisRepo.GetByNovel(novelID)
	if err != nil {
		return nil, err
	}
	if !s.novelBelongsToTenant(novelID, tenantID) {
		return nil, fmt.Errorf("not found")
	}
	return syn, nil
}

// ── 综合报告生成 ────────────────────────────────────────────────────────────

func (s *OutlineReviewService) buildSynthesis(ctx context.Context, tenantID uint, novel *model.Novel, allChapters []*model.Chapter, reviews []*model.OutlineReview) *model.NovelOutlineSynthesis {
	// 统计
	reviewMap := make(map[int]*model.OutlineReview, len(reviews))
	for _, r := range reviews {
		reviewMap[r.ChapterNo] = r
	}

	var passed, warning, failed int
	var scoreSum float64
	var reviewedCount int
	for _, r := range reviews {
		if r.Status == "failed" && r.OverallScore == 0 {
			failed++
			continue
		}
		reviewedCount++
		scoreSum += r.OverallScore
		switch r.Status {
		case "passed":
			passed++
		case "warning":
			warning++
		default:
			failed++
		}
	}
	avgScore := 0.0
	if reviewedCount > 0 {
		avgScore = scoreSum / float64(reviewedCount)
	}

	// 解析 novel.Outline 获取全书章节规划
	plannedChapters := parseNovelOutlineChapters(novel.Outline)
	totalPlanned := len(plannedChapters)
	if totalPlanned == 0 {
		totalPlanned = len(allChapters)
	}

	// 幕次统计
	act1, act2, act3 := countActChapters(plannedChapters, allChapters, reviewMap)

	// 构建张力曲线数据
	tensionCurve := buildTensionCurve(plannedChapters, allChapters, reviewMap)
	tensionCurveJSON, _ := json.Marshal(tensionCurve)

	// 幕次均衡 JSON
	arcBalance := model.ArcBalance{
		Act1Count: act1.count,
		Act2Count: act2.count,
		Act3Count: act3.count,
		Act1AvgScore: act1.avgScore,
		Act2AvgScore: act2.avgScore,
		Act3AvgScore: act3.avgScore,
	}

	syn := &model.NovelOutlineSynthesis{
		NovelID: novel.ID,
		TotalChapters:    totalPlanned,
		ReviewedCount:    reviewedCount,
		PassedCount:      passed,
		WarningCount:     warning,
		FailedCount:      failed,
		AvgScore:         math.Round(avgScore*10) / 10,
		TensionCurveJSON: string(tensionCurveJSON),
		Status:           "partial",
	}

	// ── AI 综合分析 ──────────────────────────────────────
	aiSynResult, aiErr := s.runBatchSynthesisAI(ctx, tenantID, novel, plannedChapters, allChapters, reviews, arcBalance)
	if aiErr != nil {
		logger.Errorf("[OutlineReview] batch synthesis AI failed: %v", aiErr)
		// 降级：仅用规则统计
		syn.GlobalSuggestion = buildRuleFallbackSuggestion(passed, warning, failed, totalPlanned, reviewedCount)
		syn.Status = "partial"
	} else {
		arcBalance.Assessment = aiSynResult.ArcBalance.Assessment
		arcBalance.Suggestion = aiSynResult.ArcBalance.Suggestion
		arcBalance.Act1AvgScore = act1.avgScore
		arcBalance.Act2AvgScore = act2.avgScore
		arcBalance.Act3AvgScore = act3.avgScore

		if b, _ := json.Marshal(arcBalance); b != nil {
			syn.ArcBalanceJSON = string(b)
		}
		if b, _ := json.Marshal(aiSynResult.RecurringIssues); b != nil {
			syn.RecurringIssuesJSON = string(b)
		}
		if b, _ := json.Marshal(aiSynResult.ChapterAdvices); b != nil {
			syn.ChapterAdvicesJSON = string(b)
		}
		syn.GlobalSuggestion = aiSynResult.GlobalSuggestion
		syn.Status = "completed"
	}

	if syn.ArcBalanceJSON == "" {
		if b, _ := json.Marshal(arcBalance); b != nil {
			syn.ArcBalanceJSON = string(b)
		}
	}

	return syn
}

type actStats struct {
	count    int
	scoreSum float64
	avgScore float64
}

func countActChapters(planned []plannedChapter, chapters []*model.Chapter, reviewMap map[int]*model.OutlineReview) (act1, act2, act3 actStats) {
	actOf := func(chNo, total int) int {
		if total == 0 {
			return 1
		}
		pct := float64(chNo) / float64(total)
		if pct <= 0.25 {
			return 1
		} else if pct <= 0.75 {
			return 2
		}
		return 3
	}

	totalPlan := len(planned)
	totalActual := len(chapters)

	countScore := func(actNum int, chNo int) {
		a := actNum
		var stats *actStats
		switch a {
		case 1:
			stats = &act1
		case 2:
			stats = &act2
		case 3:
			stats = &act3
		}
		stats.count++
		if r, ok := reviewMap[chNo]; ok && r.OverallScore > 0 {
			stats.scoreSum += r.OverallScore
		}
	}

	// 优先用大纲规划的幕次
	if len(planned) > 0 {
		for _, p := range planned {
			actNum := p.Act
			if actNum == 0 {
				actNum = actOf(p.ChapterNo, totalPlan)
			}
			countScore(actNum, p.ChapterNo)
		}
	} else {
		for _, ch := range chapters {
			actNum := ch.NarrativeMeta.ActNo
			if actNum == 0 {
				actNum = actOf(ch.ChapterNo, totalActual)
			}
			countScore(actNum, ch.ChapterNo)
		}
	}

	calc := func(s *actStats) {
		if s.count > 0 && s.scoreSum > 0 {
			s.avgScore = math.Round(s.scoreSum/float64(s.count)*10) / 10
		}
	}
	calc(&act1)
	calc(&act2)
	calc(&act3)
	return
}

type plannedChapter struct {
	ChapterNo    int
	Title        string
	TensionLevel int
	Act          int
	EmotionalTone string
	Summary      string
}

func parseNovelOutlineChapters(outline string) []plannedChapter {
	if outline == "" {
		return nil
	}
	var raw struct {
		Chapters []struct {
			ChapterNo     int    `json:"chapter_no"`
			Title         string `json:"title"`
			TensionLevel  int    `json:"tension_level"`
			Act           int    `json:"act"`
			EmotionalTone string `json:"emotional_tone"`
			Summary       string `json:"summary"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(outline), &raw); err != nil {
		return nil
	}
	out := make([]plannedChapter, 0, len(raw.Chapters))
	for _, c := range raw.Chapters {
		out = append(out, plannedChapter{
			ChapterNo:     c.ChapterNo,
			Title:         c.Title,
			TensionLevel:  c.TensionLevel,
			Act:           c.Act,
			EmotionalTone: c.EmotionalTone,
			Summary:       c.Summary,
		})
	}
	return out
}

func buildTensionCurve(planned []plannedChapter, chapters []*model.Chapter, reviewMap map[int]*model.OutlineReview) []model.TensionPoint {
	// 优先用大纲规划；若无规划，退回实际章节
	if len(planned) > 0 {
		pts := make([]model.TensionPoint, 0, len(planned))
		for _, p := range planned {
			score := -1.0
			status := "skipped"
			if r, ok := reviewMap[p.ChapterNo]; ok {
				score = r.OverallScore
				status = r.Status
			}
			pts = append(pts, model.TensionPoint{
				ChapterNo:    p.ChapterNo,
				PlannedLevel: p.TensionLevel,
				Score:        score,
				Status:       status,
			})
		}
		return pts
	}
	pts := make([]model.TensionPoint, 0, len(chapters))
	for _, ch := range chapters {
		score := -1.0
		status := "skipped"
		if r, ok := reviewMap[ch.ChapterNo]; ok {
			score = r.OverallScore
			status = r.Status
		}
		pts = append(pts, model.TensionPoint{
			ChapterNo:    ch.ChapterNo,
			PlannedLevel: ch.NarrativeMeta.TensionLevel,
			Score:        score,
			Status:       status,
		})
	}
	return pts
}

// ── AI 综合分析 ──────────────────────────────────────────────────────────────

type batchSynthesisAIResult struct {
	ArcBalance      model.ArcBalance      `json:"arc_balance"`
	RecurringIssues []model.OutlineIssue  `json:"recurring_issues"`
	ChapterAdvices  []model.ChapterAdvice `json:"chapter_advices"`
	GlobalSuggestion string               `json:"global_suggestion"`
}

func (s *OutlineReviewService) runBatchSynthesisAI(ctx context.Context, tenantID uint, novel *model.Novel, planned []plannedChapter, chapters []*model.Chapter, reviews []*model.OutlineReview, arcBalance model.ArcBalance) (*batchSynthesisAIResult, error) {
	if s.aiService == nil {
		return nil, fmt.Errorf("AI service not available")
	}

	// 构建章节规划摘要表（截取 summary 前80字）
	chapterPlanTable := buildChapterPlanTable(planned, chapters)
	// 构建审查得分表
	chapterScoreTable := buildChapterScoreTable(reviews)

	// 需要改进建议的章节数（得分<75的，最多20章）
	maxAdvice := 0
	for _, r := range reviews {
		if r.OverallScore < 75 && r.OverallScore > 0 {
			maxAdvice++
		}
	}
	if maxAdvice > 20 {
		maxAdvice = 20
	}
	if maxAdvice == 0 {
		maxAdvice = 5
	}

	arcForeshadows := s.buildArcOpenForeshadowsText(novel.ID)

	prompt, err := renderPrompt("outline_review_batch", map[string]interface{}{
		"NovelTitle":        novel.Title,
		"Genre":             novel.Meta.Genre,
		"Style":             novel.AIConfig.StylePrompt,
		"TotalChapters":     len(planned),
		"Act1Count":         arcBalance.Act1Count,
		"Act2Count":         arcBalance.Act2Count,
		"Act3Count":         arcBalance.Act3Count,
		"ChapterPlanTable":  chapterPlanTable,
		"ChapterScoreTable": chapterScoreTable,
		"MaxAdviceCount":    maxAdvice,
		"ArcForeshadows":    arcForeshadows,
	})
	if err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novel.ID, "chapter_review", prompt, "", StoryboardOverrides{})
	if err != nil {
		return nil, err
	}

	cleaned := extractJSONObject(strings.TrimSpace(resp))
	var result batchSynthesisAIResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("parse AI response: %w", err)
	}
	return &result, nil
}

func buildChapterPlanTable(planned []plannedChapter, chapters []*model.Chapter) string {
	var sb strings.Builder
	sb.WriteString("章节 | 标题 | 幕次 | 规划张力 | 情感基调 | 情节摘要\n")
	sb.WriteString("---|---|---|---|---|---\n")

	write := func(no int, title string, act, tension int, tone, summary string) {
		if len([]rune(summary)) > 80 {
			summary = string([]rune(summary)[:80]) + "…"
		}
		actStr := fmt.Sprintf("第%d幕", act)
		if act == 0 {
			actStr = "-"
		}
		fmt.Fprintf(&sb, "第%d章 | %s | %s | %d/10 | %s | %s\n",
			no, title, actStr, tension, tone, summary)
	}

	if len(planned) > 0 {
		for _, p := range planned {
			write(p.ChapterNo, p.Title, p.Act, p.TensionLevel, p.EmotionalTone, p.Summary)
		}
	} else {
		for _, ch := range chapters {
			write(ch.ChapterNo, ch.Title, ch.NarrativeMeta.ActNo, ch.NarrativeMeta.TensionLevel, ch.NarrativeMeta.EmotionalTone, ch.Summary)
		}
	}
	return sb.String()
}

func buildChapterScoreTable(reviews []*model.OutlineReview) string {
	var sb strings.Builder
	sb.WriteString("章节 | 综合评分 | 结构 | 节奏 | 连贯 | 人物 | 冲突 | 钩子 | 状态\n")
	sb.WriteString("---|---|---|---|---|---|---|---|---\n")
	for _, r := range reviews {
		if r.OverallScore == 0 && r.Status == "failed" {
			fmt.Fprintf(&sb, "第%d章 | 审查失败 | - | - | - | - | - | - | failed\n", r.ChapterNo)
			continue
		}
		fmt.Fprintf(&sb, "第%d章 | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %s\n",
			r.ChapterNo,
			r.OverallScore,
			r.Scores.StructureScore, r.Scores.PacingScore, r.Scores.ContinuityScore,
			r.Scores.CharacterScore, r.Scores.ConflictScore, r.Scores.HookScore,
			r.Status,
		)
	}
	return sb.String()
}

func buildRuleFallbackSuggestion(passed, warning, failed, total, reviewed int) string {
	return fmt.Sprintf(
		"已完成 %d/%d 章大纲审查（通过: %d，需改进: %d，问题较多: %d）。AI 综合分析暂不可用，建议配置 AI 服务后重新执行批量审查以获得全局战略建议。",
		reviewed, total, passed, warning, failed,
	)
}

// ── 内部：规则检查 ──────────────────────────────────────────────────────────

func (s *OutlineReviewService) runRuleChecks(chapter *model.Chapter) []model.OutlineIssue {
	var issues []model.OutlineIssue

	hasScene := strings.TrimSpace(chapter.NarrativeMeta.SceneOutline) != "" && strings.TrimSpace(chapter.NarrativeMeta.SceneOutline) != "{}"
	hasOutline := strings.TrimSpace(chapter.NarrativeMeta.Outline) != ""

	if !hasScene && !hasOutline {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "structure",
			Severity:    "error",
			Description: "章节缺少大纲，无法进行专业审查",
			Suggestion:  "请先生成章节场景大纲（可通过 AI 生成或手动编写）",
		})
		return issues
	}

	if !hasScene {
		// 仅有文字大纲，跳过场景 JSON 规则检查
		return issues
	}

	// 解析场景大纲
	var outline struct {
		Scenes []struct {
			TensionLevel int    `json:"tension_level"`
			Goal         string `json:"goal"`
			Beats        string `json:"beats"`
			POV          string `json:"pov"`
		} `json:"scenes"`
	}
	if err := json.Unmarshal([]byte(chapter.NarrativeMeta.SceneOutline), &outline); err == nil {
		n := len(outline.Scenes)
		// 单场景仅对中低张力章节报错；高张力章节（如决战/高潮）可以是一个完整长场景
		if n < 2 && chapter.NarrativeMeta.TensionLevel < 8 {
			issues = append(issues, model.OutlineIssue{
				Dimension:   "structure",
				Severity:    "error",
				Description: fmt.Sprintf("场景数量过少（当前%d个），叙事空间不足", n),
				Suggestion:  "建议设计3-5个场景，确保章节有完整的起承转合",
			})
		} else if n > 7 {
			issues = append(issues, model.OutlineIssue{
				Dimension:   "structure",
				Severity:    "warning",
				Description: fmt.Sprintf("场景数量偏多（当前%d个），可能导致节奏拖沓", n),
				Suggestion:  "建议合并相近场景，保持3-5个核心场景，提升节奏紧凑度",
			})
		}

		if n >= 3 {
			levels := make([]int, 0, n)
			for _, sc := range outline.Scenes {
				if sc.TensionLevel > 0 {
					levels = append(levels, sc.TensionLevel)
				}
			}
			if len(levels) >= 3 {
				allSame := true
				for _, l := range levels[1:] {
					if l != levels[0] {
						allSame = false
						break
					}
				}
				if allSame {
					issues = append(issues, model.OutlineIssue{
						Dimension:   "pacing",
						Severity:    "warning",
						Description: fmt.Sprintf("所有场景张力值相同（均为%d），节奏过于平稳", levels[0]),
						Suggestion:  "建议调整场景间的张力梯度，制造张弛有度的节奏曲线",
					})
				}
				// 全程高张力警告：只对章节目标张力本身不高的章节触发
				// 如果章节规划张力就是 8+，全程高张力是预期行为（决战/高潮章）
				if chapter.NarrativeMeta.TensionLevel < 7 {
					allHigh := true
					for _, l := range levels {
						if l < 7 {
							allHigh = false
							break
						}
					}
					if allHigh {
						issues = append(issues, model.OutlineIssue{
							Dimension:   "pacing",
							Severity:    "info",
							Description: "章节全程高张力，读者可能产生疲劳感",
							Suggestion:  "建议在高潮前插入短暂的情感缓冲场景，形成张弛对比",
						})
					}
				}
			}

			emptyGoals := 0
			for _, sc := range outline.Scenes {
				if strings.TrimSpace(sc.Goal) == "" {
					emptyGoals++
				}
			}
			if emptyGoals > 0 {
				issues = append(issues, model.OutlineIssue{
					Dimension:   "structure",
					Severity:    "warning",
					Description: fmt.Sprintf("%d个场景缺少明确目标（Goal），叙事动力不足", emptyGoals),
					Suggestion:  "每个场景都应有清晰的角色目标，驱动场景推进",
				})
			}
		}
	}

	if chapter.NarrativeMeta.HookType == "" && chapter.NarrativeMeta.ChapterHook == "" {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "hook",
			Severity:    "info",
			Description: "章节未设置章末钩子类型",
			Suggestion:  "建议明确钩子类型（悬念/冲突/反转/情感），增强读者的继续阅读动力",
		})
	}

	// 第一幕极高张力警告：排除章节号 <= 2 的情况（开场倒叙/in medias res 是经典手法）
	if chapter.NarrativeMeta.ActNo == 1 && chapter.NarrativeMeta.TensionLevel >= 9 && chapter.ChapterNo > 2 {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "pacing",
			Severity:    "warning",
			Description: fmt.Sprintf("第一幕第%d章出现极高张力（%d/10），可能透支后续叙事空间", chapter.ChapterNo, chapter.NarrativeMeta.TensionLevel),
			Suggestion:  "第一幕中期建议以铺垫和人物塑造为主，将极高张力节点集中于幕末转折点，为第二幕留出对比空间",
		})
	}

	return issues
}

// ── 内部：AI 审查 ──────────────────────────────────────────────────────────

type aiReviewResult struct {
	OverallScore    float64 `json:"overall_score"`
	DimensionScores struct {
		Structure  float64 `json:"structure"`
		Pacing     float64 `json:"pacing"`
		Continuity float64 `json:"continuity"`
		Character  float64 `json:"character"`
		Conflict   float64 `json:"conflict"`
		Hook       float64 `json:"hook"`
	} `json:"dimension_scores"`
	Issues     []model.OutlineIssue `json:"issues"`
	Highlights []string             `json:"highlights"`
	Suggestion string               `json:"suggestion"`
}

func (s *OutlineReviewService) runAIReview(ctx context.Context, tenantID uint, chapter *model.Chapter, novel *model.Novel) (*aiReviewResult, error) {
	if s.aiService == nil {
		return nil, fmt.Errorf("AI service not available")
	}

	prevSummary := ""
	if chapter.ChapterNo > 1 {
		if prev, err := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, chapter.ChapterNo-1); err == nil {
			prevSummary = prev.Summary
		}
	}

	nextTitle := ""
	if next, err := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, chapter.ChapterNo+1); err == nil {
		nextTitle = next.Title
	}

	outlineExcerpt := ""
	if novel.Outline != "" {
		runes := []rune(novel.Outline)
		if len(runes) > 500 {
			outlineExcerpt = string(runes[:500]) + "..."
		} else {
			outlineExcerpt = novel.Outline
		}
	}

	// 优先使用场景大纲，降级到文字大纲
	outlineContent := chapter.NarrativeMeta.SceneOutline
	if strings.TrimSpace(outlineContent) == "" || strings.TrimSpace(outlineContent) == "{}" {
		outlineContent = chapter.NarrativeMeta.Outline
	}

	openForeshadows := s.buildOpenForeshadowsText(chapter.NovelID)

	// 体裁权重摘要（告知 AI 本体裁的审查侧重，辅助其打分）
	w := getGenreWeights(novel.Meta.Genre)
	weightHint := fmt.Sprintf("结构%.0f%%·节奏%.0f%%·连贯%.0f%%·人物%.0f%%·冲突%.0f%%·钩子%.0f%%",
		w.Structure*100, w.Pacing*100, w.Continuity*100,
		w.Character*100, w.Conflict*100, w.Hook*100)

	prompt, err := renderPrompt("outline_review", map[string]interface{}{
		"NovelTitle":          novel.Title,
		"Genre":               novel.Meta.Genre,
		"Style":               novel.AIConfig.StylePrompt,
		"TargetChapters":      novel.Meta.TargetChapters,
		"ChapterNo":           chapter.ChapterNo,
		"ActNo":               chapter.NarrativeMeta.ActNo,
		"EmotionalTone":       chapter.NarrativeMeta.EmotionalTone,
		"TensionLevel":        chapter.NarrativeMeta.TensionLevel,
		"HookType":            chapter.NarrativeMeta.HookType,
		"SceneOutlineJSON":    outlineContent,
		"PrevChapterSummary":  prevSummary,
		"NextChapterTitle":    nextTitle,
		"NovelOutlineExcerpt": outlineExcerpt,
		"OpenForeshadows":     openForeshadows,
		"WeightHint":          weightHint,
	})
	if err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_review", prompt, "", StoryboardOverrides{})
	if err != nil {
		return nil, err
	}

	cleaned := extractJSONObject(strings.TrimSpace(resp))
	var result aiReviewResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("parse AI response: %w", err)
	}

	clamp := func(v float64) float64 { return math.Max(0, math.Min(100, v)) }
	result.OverallScore = clamp(result.OverallScore)
	result.DimensionScores.Structure = clamp(result.DimensionScores.Structure)
	result.DimensionScores.Pacing = clamp(result.DimensionScores.Pacing)
	result.DimensionScores.Continuity = clamp(result.DimensionScores.Continuity)
	result.DimensionScores.Character = clamp(result.DimensionScores.Character)
	result.DimensionScores.Conflict = clamp(result.DimensionScores.Conflict)
	result.DimensionScores.Hook = clamp(result.DimensionScores.Hook)

	if result.OverallScore == 0 {
		d := result.DimensionScores
		result.OverallScore = clamp((d.Structure + d.Pacing + d.Continuity + d.Character + d.Conflict + d.Hook) / 6)
	}

	return &result, nil
}

func (s *OutlineReviewService) buildRuleOnlyReview(review *model.OutlineReview, issues []model.OutlineIssue) *model.OutlineReview {
	errCount := 0
	warnCount := 0
	for _, iss := range issues {
		switch iss.Severity {
		case "error":
			errCount++
		case "warning":
			warnCount++
		}
	}
	score := math.Max(0, 80-float64(errCount)*20-float64(warnCount)*8)
	review.OverallScore = score
	review.Scores.StructureScore = score
	review.Scores.PacingScore = score
	review.Scores.ContinuityScore = score
	review.Scores.CharacterScore = score
	review.Scores.ConflictScore = score
	review.Scores.HookScore = score

	if b, _ := json.Marshal(issues); b != nil {
		review.Content.IssuesJSON = string(b)
	}
	review.Content.Suggestion = "AI 审查暂不可用，已完成基础规则检查。建议配置 AI 服务后重新审查以获得专业建议。"
	return review
}

// ── 体裁权重系统 ─────────────────────────────────────────────────────────────

type dimensionWeights struct {
	Structure  float64
	Pacing     float64
	Continuity float64
	Character  float64
	Conflict   float64
	Hook       float64
}

// 各体裁对六维度的侧重不同：权重之和均为1.0
var genreWeightMap = map[string]dimensionWeights{
	"言情": {0.12, 0.15, 0.20, 0.25, 0.15, 0.13},
	"爱情": {0.12, 0.15, 0.20, 0.25, 0.15, 0.13},
	"悬疑": {0.15, 0.15, 0.18, 0.10, 0.17, 0.25},
	"惊悚": {0.15, 0.15, 0.18, 0.10, 0.17, 0.25},
	"推理": {0.15, 0.15, 0.18, 0.10, 0.17, 0.25},
	"武侠": {0.15, 0.20, 0.15, 0.15, 0.25, 0.10},
	"玄幻": {0.15, 0.20, 0.15, 0.15, 0.25, 0.10},
	"修仙": {0.15, 0.20, 0.15, 0.15, 0.25, 0.10},
	"奇幻": {0.18, 0.18, 0.16, 0.15, 0.23, 0.10},
	"科幻": {0.20, 0.15, 0.18, 0.15, 0.22, 0.10},
	"历史": {0.20, 0.15, 0.22, 0.18, 0.15, 0.10},
	"都市": {0.15, 0.15, 0.20, 0.20, 0.20, 0.10},
	"职场": {0.15, 0.15, 0.20, 0.20, 0.20, 0.10},
}

var equalWeights = dimensionWeights{1.0 / 6, 1.0 / 6, 1.0 / 6, 1.0 / 6, 1.0 / 6, 1.0 / 6}

func getGenreWeights(genre string) dimensionWeights {
	if w, ok := genreWeightMap[genre]; ok {
		return w
	}
	for k, w := range genreWeightMap {
		if strings.Contains(genre, k) {
			return w
		}
	}
	return equalWeights
}

func applyWeightedScore(d aiReviewResult, genre string) float64 {
	w := getGenreWeights(genre)
	score := d.DimensionScores.Structure*w.Structure +
		d.DimensionScores.Pacing*w.Pacing +
		d.DimensionScores.Continuity*w.Continuity +
		d.DimensionScores.Character*w.Character +
		d.DimensionScores.Conflict*w.Conflict +
		d.DimensionScores.Hook*w.Hook
	return math.Max(0, math.Min(100, math.Round(score*10)/10))
}

// ── 伏笔上下文构建 ───────────────────────────────────────────────────────────

// buildOpenForeshadowsText 返回小说当前未兑现伏笔的摘要文本，注入审查 prompt
func (s *OutlineReviewService) buildOpenForeshadowsText(novelID uint) string {
	if s.foreshadowRepo == nil {
		return ""
	}
	foreshadows, err := s.foreshadowRepo.ListByNovel(novelID)
	if err != nil || len(foreshadows) == 0 {
		return ""
	}
	var lines []string
	for _, f := range foreshadows {
		if f.Status == "planted" {
			desc := f.Meta.Description
			if len([]rune(desc)) > 60 {
				desc = string([]rune(desc)[:60]) + "…"
			}
			lines = append(lines, fmt.Sprintf("- 【%s】%s", f.Title, desc))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// buildArcOpenForeshadowsText 从弧光摘要中聚合跨弧未兑现伏笔，用于综合报告
func (s *OutlineReviewService) buildArcOpenForeshadowsText(novelID uint) string {
	if s.arcSummaryRepo == nil {
		return ""
	}
	arcs, err := s.arcSummaryRepo.ListByNovel(novelID)
	if err != nil || len(arcs) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	for _, arc := range arcs {
		if arc.OpenForeshadows == "" {
			continue
		}
		var items []string
		if err := json.Unmarshal([]byte(arc.OpenForeshadows), &items); err != nil {
			continue
		}
		for _, item := range items {
			if !seen[item] {
				seen[item] = true
				lines = append(lines, fmt.Sprintf("- %s（源自第%d-%d章）", item, arc.StartChapter, arc.EndChapter))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func scoreToStatus(score float64) string {
	switch {
	case score >= 80:
		return "passed"
	case score >= 60:
		return "warning"
	default:
		return "failed"
	}
}

func sortReviewsByChapterNo(reviews []*model.OutlineReview) {
	for i := 1; i < len(reviews); i++ {
		for j := i; j > 0 && reviews[j].ChapterNo < reviews[j-1].ChapterNo; j-- {
			reviews[j], reviews[j-1] = reviews[j-1], reviews[j]
		}
	}
}
