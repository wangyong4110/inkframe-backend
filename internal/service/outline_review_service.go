package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// OutlineReviewService 章节大纲审查服务
type OutlineReviewService struct {
	reviewRepo  *repository.OutlineReviewRepository
	chapterRepo interface {
		GetByID(id uint) (*model.Chapter, error)
		ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
		GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error)
	}
	novelRepo interface {
		GetByID(id uint) (*model.Novel, error)
	}
	aiService *AIService
}

func NewOutlineReviewService(
	reviewRepo *repository.OutlineReviewRepository,
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
		reviewRepo:  reviewRepo,
		chapterRepo: chapterRepo,
		novelRepo:   novelRepo,
		aiService:   aiService,
	}
}

// ReviewChapterOutline 审查单章大纲（含规则检查 + AI审查）
func (s *OutlineReviewService) ReviewChapterOutline(ctx context.Context, tenantID, chapterID uint) (*model.OutlineReview, error) {
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	if chapter.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}

	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel not found: %w", err)
	}

	now := time.Now()
	review := &model.OutlineReview{
		TenantID:   tenantID,
		NovelID:    chapter.NovelID,
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
		logger.Printf("[OutlineReview] AI review failed for chapter %d: %v, falling back to rule-only", chapterID, aiErr)
		// 纯规则降级：按规则问题数量估算分数
		review = s.buildRuleOnlyReview(review, ruleIssues)
	} else {
		// 合并规则问题和 AI 问题
		allIssues := append(ruleIssues, aiResult.Issues...)
		review.StructureScore = aiResult.DimensionScores.Structure
		review.PacingScore = aiResult.DimensionScores.Pacing
		review.ContinuityScore = aiResult.DimensionScores.Continuity
		review.CharacterScore = aiResult.DimensionScores.Character
		review.ConflictScore = aiResult.DimensionScores.Conflict
		review.HookScore = aiResult.DimensionScores.Hook
		review.OverallScore = aiResult.OverallScore
		review.Suggestion = aiResult.Suggestion

		if b, _ := json.Marshal(allIssues); b != nil {
			review.IssuesJSON = string(b)
		}
		if b, _ := json.Marshal(aiResult.Highlights); b != nil {
			review.HighlightsJSON = string(b)
		}
	}

	review.Status = scoreToStatus(review.OverallScore)

	if err := s.reviewRepo.Upsert(review); err != nil {
		return nil, fmt.Errorf("save review: %w", err)
	}
	return review, nil
}

// BatchReviewNovel 批量审查小说所有章节大纲
func (s *OutlineReviewService) BatchReviewNovel(ctx context.Context, tenantID, novelID uint, progressFn func(done, total int)) ([]*model.OutlineReview, error) {
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, err
	}

	// 只审查有场景大纲的章节
	var reviewable []*model.Chapter
	for _, ch := range chapters {
		if strings.TrimSpace(ch.SceneOutline) != "" && strings.TrimSpace(ch.SceneOutline) != "{}" {
			reviewable = append(reviewable, ch)
		}
	}

	if len(reviewable) == 0 {
		return nil, fmt.Errorf("没有找到包含场景大纲的章节，请先生成章节内容")
	}

	var results []*model.OutlineReview
	for i, ch := range reviewable {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		review, err := s.ReviewChapterOutline(ctx, tenantID, ch.ID)
		if err != nil {
			logger.Printf("[OutlineReview] batch review chapter %d failed: %v", ch.ChapterNo, err)
			continue
		}
		results = append(results, review)
		if progressFn != nil {
			progressFn(i+1, len(reviewable))
		}
	}
	return results, nil
}

// GetReview 获取章节的最新审查结果
func (s *OutlineReviewService) GetReview(tenantID, chapterID uint) (*model.OutlineReview, error) {
	review, err := s.reviewRepo.GetByChapter(chapterID)
	if err != nil {
		return nil, err
	}
	if review.TenantID != tenantID {
		return nil, fmt.Errorf("not found")
	}
	return review, nil
}

// ListNovelReviews 获取小说所有章节的审查结果
func (s *OutlineReviewService) ListNovelReviews(tenantID, novelID uint) ([]*model.OutlineReview, error) {
	return s.reviewRepo.ListByNovel(novelID)
}

// ── 内部：规则检查 ──────────────────────────────────────────────────────────

func (s *OutlineReviewService) runRuleChecks(chapter *model.Chapter) []model.OutlineIssue {
	var issues []model.OutlineIssue

	if strings.TrimSpace(chapter.SceneOutline) == "" {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "structure",
			Severity:    "error",
			Description: "章节缺少场景大纲，无法进行专业审查",
			Suggestion:  "请先生成章节场景大纲（可通过 AI 生成或手动编写）",
		})
		return issues // 无大纲时其他规则无意义
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
	if err := json.Unmarshal([]byte(chapter.SceneOutline), &outline); err == nil {
		n := len(outline.Scenes)
		if n < 2 {
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

		// 张力曲线检查
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
				// 检查是否全程高张力（缺乏喘息）
				allHigh := true
				for _, l := range levels {
					if l < 7 {
						allHigh = false
						break
					}
				}
				if allHigh && len(levels) >= 3 {
					issues = append(issues, model.OutlineIssue{
						Dimension:   "pacing",
						Severity:    "info",
						Description: "章节全程高张力，读者可能产生疲劳感",
						Suggestion:  "建议在高潮前插入短暂的情感缓冲场景，形成张弛对比",
					})
				}
			}

			// 检查场景目标是否为空
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

	// 章末钩子检查
	if chapter.HookType == "" && chapter.ChapterHook == "" {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "hook",
			Severity:    "info",
			Description: "章节未设置章末钩子类型",
			Suggestion:  "建议明确钩子类型（悬念/冲突/反转/情感），增强读者的继续阅读动力",
		})
	}

	// 幕次合理性（第1幕不应出现高张力高潮，第3幕不应是低张力）
	if chapter.ActNo == 1 && chapter.TensionLevel >= 9 {
		issues = append(issues, model.OutlineIssue{
			Dimension:   "pacing",
			Severity:    "warning",
			Description: "第一幕出现极高张力（9-10），可能透支叙事空间",
			Suggestion:  "第一幕建议以铺垫、人物塑造为主，为后续高潮留出空间",
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

	// 获取上一章摘要
	prevSummary := ""
	if chapter.ChapterNo > 1 {
		if prev, err := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, chapter.ChapterNo-1); err == nil {
			prevSummary = prev.Summary
		}
	}

	// 获取下一章标题
	nextTitle := ""
	if next, err := s.chapterRepo.GetByNovelAndChapterNo(chapter.NovelID, chapter.ChapterNo+1); err == nil {
		nextTitle = next.Title
	}

	// 大纲摘录（取前500字）
	outlineExcerpt := ""
	if novel.Outline != "" {
		runes := []rune(novel.Outline)
		if len(runes) > 500 {
			outlineExcerpt = string(runes[:500]) + "..."
		} else {
			outlineExcerpt = novel.Outline
		}
	}

	prompt, err := renderPrompt("outline_review", map[string]interface{}{
		"NovelTitle":          novel.Title,
		"Genre":               novel.Genre,
		"Style":               novel.StylePrompt,
		"TargetChapters":      novel.TargetChapters,
		"ChapterNo":           chapter.ChapterNo,
		"ActNo":               chapter.ActNo,
		"EmotionalTone":       chapter.EmotionalTone,
		"TensionLevel":        chapter.TensionLevel,
		"HookType":            chapter.HookType,
		"SceneOutlineJSON":    chapter.SceneOutline,
		"PrevChapterSummary":  prevSummary,
		"NextChapterTitle":    nextTitle,
		"NovelOutlineExcerpt": outlineExcerpt,
	})
	if err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_review", prompt, "", StoryboardOverrides{MaxTokens: 2048})
	if err != nil {
		return nil, err
	}

	cleaned := extractJSONObject(strings.TrimSpace(resp))
	var result aiReviewResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("parse AI response: %w", err)
	}

	// 分数边界保护
	clamp := func(v float64) float64 { return math.Max(0, math.Min(100, v)) }
	result.OverallScore = clamp(result.OverallScore)
	result.DimensionScores.Structure = clamp(result.DimensionScores.Structure)
	result.DimensionScores.Pacing = clamp(result.DimensionScores.Pacing)
	result.DimensionScores.Continuity = clamp(result.DimensionScores.Continuity)
	result.DimensionScores.Character = clamp(result.DimensionScores.Character)
	result.DimensionScores.Conflict = clamp(result.DimensionScores.Conflict)
	result.DimensionScores.Hook = clamp(result.DimensionScores.Hook)

	// 若 AI 未给 overall，取六维平均
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
	// 简单估算：每个error扣20分，每个warning扣8分，满分80（无AI加成）
	score := math.Max(0, 80-float64(errCount)*20-float64(warnCount)*8)
	review.OverallScore = score
	review.StructureScore = score
	review.PacingScore = score
	review.ContinuityScore = score
	review.CharacterScore = score
	review.ConflictScore = score
	review.HookScore = score

	if b, _ := json.Marshal(issues); b != nil {
		review.IssuesJSON = string(b)
	}
	review.Suggestion = "AI 审查暂不可用，已完成基础规则检查。建议配置 AI 服务后重新审查以获得专业建议。"
	return review
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
