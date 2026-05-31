package service

import (
	"encoding/json"
	"fmt"
	"strings"

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
	Type          string `json:"type"` // appearance, personality, ability, dialogue
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
// chapterID 和 tenantID 用于写入持久化记录；novelID 和 chapterNo 用于上下文查询。
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
			TenantID:   tenantID,
			ReportJSON: string(data),
			IssueCount: issueCount,
			Passed:     !report.HasIssues,
		}
		if saveErr := s.reportRepo.Create(rec); saveErr != nil {
			logger.Printf("ContinuityService: save report: %v", saveErr)
		}
	}

	return report, nil
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
	if len([]rune(content)) < 1000 {
		issues = append(issues, PlotIssue{
			Type:        "length",
			Severity:    "medium",
			Description: fmt.Sprintf("章节内容过短（%d字），可能不够充实", len([]rune(content))),
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
