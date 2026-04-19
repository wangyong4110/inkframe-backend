package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/vector"
	"strings"
)

// PromptService 提示词服务（使用模板系统）
type PromptService struct {
	templateSvc *TemplateService
}

func NewPromptService(templateSvc *TemplateService) *PromptService {
	return &PromptService{
		templateSvc: templateSvc,
	}
}

// BuildOutlinePrompt 构建大纲提示词
func (s *PromptService) BuildOutlinePrompt(novel *model.Novel, req *GenerateOutlineRequest) string {
	data := &NovelOutlineTemplateData{
		Title:          novel.Title,
		Genre:          novel.Genre,
		Theme:          "",
		Style:          "",
		TargetAudience: "",
		ChapterCount:   req.ChapterNum,
	}

	prompt, err := s.templateSvc.RenderNovelOutlinePrompt(data)
	if err != nil {
		// 如果模板渲染失败，返回简化版本
		return fmt.Sprintf("请为小说《%s》生成%d章的大纲。", novel.Title, req.ChapterNum)
	}

	return prompt
}

// BuildChapterPrompt 构建章节提示词
func (s *PromptService) BuildChapterPrompt(novel *model.Novel, chapter *model.Chapter, recentChapters []*model.Chapter, characters []*model.Character) string {
	// 转换为 ChapterInfo 格式
	recentChapterInfos := make([]ChapterInfo, len(recentChapters))
	for i, ch := range recentChapters {
		recentChapterInfos[i] = ChapterInfo{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Summary:   ch.Summary,
		}
	}

	characterInfos := make([]struct {
		Name   string
		Traits string
	}, len(characters))
	for i, ch := range characters {
		characterInfos[i].Name = ch.Name
		characterInfos[i].Traits = ch.Personality
	}

	// 简化数据以适配现有模板
	data := &ChapterTemplateData{
		Novel: NovelInfo{
			Title: novel.Title,
			Genre: novel.Genre,
		},
		Chapter: ChapterInfo{
			ChapterNo: chapter.ChapterNo,
			Title:     chapter.Title,
			Summary:   chapter.Summary,
		},
		ChapterNo:   chapter.ChapterNo,
			WordCount:  3000,
		Style:      novel.StylePrompt,
		UserPrompt: "",
		RecentChapters: recentChapterInfos,
	}

	prompt, err := s.templateSvc.RenderChapterPrompt(data)
	if err != nil {
		// 如果模板渲染失败，返回简化版本
		return fmt.Sprintf("请为小说《%s》创作第%d章《%s》，字数约3000字。", novel.Title, chapter.ChapterNo, chapter.Title)
	}

	return prompt
}

// ============================================
// ContinuityService 连续性检查服务
// ============================================

type ContinuityService struct {
	characterRepo *repository.CharacterRepository
	chapterRepo   *repository.ChapterRepository
}

func NewContinuityService(charRepo *repository.CharacterRepository, chapterRepo *repository.ChapterRepository) *ContinuityService {
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
	ChapterID   uint   `json:"chapter_id"`
	ChapterNo   int    `json:"chapter_no"`
	ChapterTitle string `json:"chapter_title"`
	Type        string `json:"type"` // inconsistency, plot_hole, timeline
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
}

// CheckContinuity 检查连续性
func (s *ContinuityService) CheckContinuity(novelID uint, chapterNos []int) (*ContinuityReport, error) {
	report := &ContinuityReport{
		HasIssues:       false,
		CharacterIssues: []CharacterIssue{},
		WorldviewIssues: []WorldviewIssue{},
		PlotIssues:      []PlotIssue{},
		Suggestions:     []string{},
	}

	// TODO: 实现连续性检查逻辑
	// 1. 检查角色外貌一致性
	// 2. 检查世界观设定一致性
	// 3. 检查时间线一致性
	// 4. 检查剧情漏洞

	return report, nil
}

// ============================================
// KnowledgeService 知识库服务
// ============================================

type KnowledgeService struct {
	kbRepo interface {
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
		Create(kb *model.KnowledgeBase) error
		Update(kb *model.KnowledgeBase) error
	}
	aiService *ai.AIService
}

func NewKnowledgeService(kbRepo interface {
	Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
	GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
	Create(kb *model.KnowledgeBase) error
	Update(kb *model.KnowledgeBase) error)
}, aiService *ai.AIService) *KnowledgeService {
	return &KnowledgeService{
		kbRepo:    kbRepo,
		aiService: aiService,
	}
}

// SearchKnowledge 搜索知识
func (s *KnowledgeService) SearchKnowledge(novelID uint, keyword string) ([]*model.KnowledgeBase, error) {
	results, err := s.kbRepo.Search(keyword, 20)
	if err != nil {
		return nil, err
	}

	// TODO: 实现知识图谱关联查询
	return results, nil
}

// ============================================
// QualityControlService 质量控制服务
// ============================================

type QualityControlService struct {
	novelRepo    interface{ GetByID(id uint) (*model.Novel, error) }
	chapterRepo  interface{ GetByID(id uint) (*model.Chapter, error) }
	charRepo     interface{ GetByID(id uint) (*model.Character, error) }
	aiService    *ai.AIService
}

func NewQualityControlService(
	novelRepo interface{ GetByID(id uint) (*model.Novel, error) },
	chapterRepo interface{ GetByID(id uint) (*model.Chapter, error) },
	charRepo interface{ GetByID(id uint) (*model.Character, error) },
	aiService *ai.AIService,
) *QualityControlService {
	return &QualityControlService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
		charRepo:    charRepo,
			aiService:  aiService,
	}
}

// QualityReport 质量报告
type QualityReport struct {
	ChapterID     uint      `json:"chapter_id"`
	ChapterNo     int       `json:"chapter_no"`
	ChapterTitle  string    `json:"chapter_title"`
	OverallScore  float64   `json:"overall_score"`
	LanguageScore  float64   `json:"language_score"`
	PlotScore      float64   `json:"plot_score"`
	CharacterScore float64   `json:"character_score"`
	WorldviewScore float64   `json:"worldview_score"`
	QualityIssues  []QualityIssue `json:"quality_issues"`
	Suggestions    []string      `json:"suggestions"`
}

// QualityIssue 质量问题
type QualityIssue struct {
	Type        string  `json:"type"`      // language, plot, character, worldview
	Severity    string  `json:"severity"`  // critical, major, minor
	Location    string  `json:"location"`
	Description string  `json:"description"`
	Suggestion  string  `json:"suggestion"`
}

// CheckQuality 检查质量
func (s *QualityControlService) CheckQuality(chapterID uint) (*QualityReport, error) {
	report := &QualityReport{
		OverallScore:  0,
		QualityIssues: []QualityIssue{},
		Suggestions:  []string{},
	}

	// 获取章节信息
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, err
	}

	report.ChapterID = chapterID
	report.ChapterNo = chapter.ChapterNo
	report.ChapterTitle = chapter.Title

	// TODO: 实现质量检查逻辑
	// 1. 语言质量检查
	// 2. 剧情连贯性检查
	// 3. 角色一致性检查
	// 4. 世界观一致性检查

	return report, nil
}

// PlotIssue 剧情问题
type PlotIssue struct {
	ChapterID   uint   `json:"chapter_id"`
	ChapterNo   int    `json:"chapter_no"`
	ChapterTitle string `json:"chapter_title"`
	Type        string `json:"type"` // inconsistency, plot_hole, timeline
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
}

// TimelineService 时间线服务
type TimelineService struct {
	novelRepo   interface{ GetByID(id uint) (*model.Novel, error) }
	chapterRepo interface{ ListByNovel(novelID uint) ([]*model.Chapter, error) }
}

func NewTimelineService(novelRepo interface{ GetByID(id uint) (*model.Novel, error) }, chapterRepo interface{ ListByNovel(novelID uint) ([]*model.Chapter, error)}) *TimelineService {
	return &TimelineService{
		novelRepo:   novelRepo,
		chapterRepo: chapterRepo,
	}
}

// Timeline 时间线
type Timeline struct {
	Events []TimelineEvent `json:"events"`
}

// TimelineEvent 时间线事件
type TimelineEvent struct {
	Position    string  `json:"position"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Characters  []string `json:"characters"`
	Emotion     string  `json:"emotion"`
}

// BuildTimeline 构建时间线
func (s *TimelineService) BuildTimeline(novelID uint) (*Timeline, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, err
	}

	// 获取所有章节
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}

	timeline := &Timeline{}

	for _, chapter := range chapters {
		event := TimelineEvent{
			Position:    fmt.Sprintf("第%d章", chapter.ChapterNo),
			Title:       chapter.Title,
			Description: chapter.Summary,
			Characters: []string{},
			Emotion:     "neutral",
		}
		timeline.Events = append(timeline.Events, event)
	}

	return timeline, nil
}
