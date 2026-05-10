package service

import (
	"fmt"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

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
