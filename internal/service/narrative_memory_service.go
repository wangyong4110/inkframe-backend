package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"text/template"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

const (
	arcSize          = 10 // 每弧章节数
	recentFullCount  = 2  // 最近N章注入详细摘要
	recentShortCount = 8  // 再往前N章注入简短摘要（30字）

	shortSummaryMaxRunes        = 40 // 简短摘要截断字符数
	repeatWordThreshold         = 5  // 重复词出现 N 次触发精修建议
	consecutivePronounThreshold = 4  // 连续以他/她开头的段落数阈值
)

var repeatWords = []string{"突然", "忽然", "然后", "接着", "不禁", "不由得", "内心"}

// ============================================
// NarrativeMemoryService 层次化记忆服务
// ============================================
//
// 构建层次化上下文：
//   近章详摘（最近2章）→ 近章简摘（往前8章）→ 弧光摘要（每10章）→ 全局概述
//
// 该设计让第50章的生成能访问完整的故事记忆，不再受制于上下文窗口。

type NarrativeMemoryService struct {
	novelRepo     novelGetter
	chapterRepo   chapterMemoryRepo
	characterRepo characterLister
	arcRepo       *repository.ArcSummaryRepository
	aiService     *AIService
}

type novelGetter interface {
	GetByID(id uint) (*model.Novel, error)
}

type chapterMemoryRepo interface {
	GetByID(id uint) (*model.Chapter, error)
	GetRecent(novelID uint, chapterNo int, count int) ([]*model.Chapter, error)
	ListByNovel(novelID uint) ([]*model.Chapter, error)
	GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error)
}

type characterLister interface {
	ListByNovel(novelID uint) ([]*model.Character, error)
}

func NewNarrativeMemoryService(
	novelRepo novelGetter,
	chapterRepo chapterMemoryRepo,
	characterRepo characterLister,
	arcRepo *repository.ArcSummaryRepository,
	aiService *AIService,
) *NarrativeMemoryService {
	return &NarrativeMemoryService{
		novelRepo:     novelRepo,
		chapterRepo:   chapterRepo,
		characterRepo: characterRepo,
		arcRepo:       arcRepo,
		aiService:     aiService,
	}
}

// ──────────────────────────────────────────────
// HierarchicalContext data structs
// ──────────────────────────────────────────────

type HierarchicalContext struct {
	RecentDetailed []ChapterBrief // 最近 recentFullCount 章（详细摘要）
	RecentShort    []ChapterBrief // 再往前 recentShortCount 章（简短摘要）
	ArcSummaries   []ArcBrief     // 已完成弧
	GlobalSummary  string
}

type ChapterBrief struct {
	ChapterNo int
	Title     string
	Summary   string
}

type ArcBrief struct {
	ArcNo        int
	StartChapter int
	EndChapter   int
	Summary      string
	KeyEvents    string
}

// ──────────────────────────────────────────────
// BuildHierarchicalContext
// ──────────────────────────────────────────────

// BuildHierarchicalContext 返回供 prompt 注入的层次化上下文文本
func (s *NarrativeMemoryService) BuildHierarchicalContext(novelID uint, currentChapterNo int) (string, error) {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return "", fmt.Errorf("NarrativeMemory.BuildHierarchicalContext: %w", err)
	}
	ctx, err := s.gatherContext(novel, currentChapterNo)
	if err != nil {
		return "", err
	}
	return renderHierarchicalContext(ctx), nil
}

func (s *NarrativeMemoryService) gatherContext(novel *model.Novel, currentChapterNo int) (*HierarchicalContext, error) {
	ctx := &HierarchicalContext{
		GlobalSummary: s.buildGlobalSummary(novel),
	}

	// 弧摘要（所有已完成弧）
	completedArcs := (currentChapterNo - 1) / arcSize
	for arcNo := 1; arcNo <= completedArcs; arcNo++ {
		arc, err := s.arcRepo.GetByNovelAndArcNo(novel.ID, arcNo)
		if err == nil && arc != nil {
			ctx.ArcSummaries = append(ctx.ArcSummaries, ArcBrief{
				ArcNo:        arc.ArcNo,
				StartChapter: arc.StartChapter,
				EndChapter:   arc.EndChapter,
				Summary:      arc.Summary,
				KeyEvents:    arc.KeyEvents,
			})
		}
	}

	// 近章详细摘要（最近 recentFullCount 章）
	recentDetailed, _ := s.chapterRepo.GetRecent(novel.ID, currentChapterNo, recentFullCount)
	for i := len(recentDetailed) - 1; i >= 0; i-- {
		ch := recentDetailed[i]
		ctx.RecentDetailed = append(ctx.RecentDetailed, ChapterBrief{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Summary:   ch.Summary,
		})
	}

	// 稍远章简短摘要（往前第3~10章）
	shortStart := currentChapterNo - recentFullCount - recentShortCount
	if shortStart < 1 {
		shortStart = 1
	}
	shortEnd := currentChapterNo - recentFullCount - 1
	for chNo := shortEnd; chNo >= shortStart; chNo-- {
		ch, err := s.chapterRepo.GetByNovelAndChapterNo(novel.ID, chNo)
		if err != nil || ch == nil {
			continue
		}
		brief := ch.Summary
		if len([]rune(brief)) > shortSummaryMaxRunes {
			brief = string([]rune(brief)[:shortSummaryMaxRunes]) + "…"
		}
		ctx.RecentShort = append(ctx.RecentShort, ChapterBrief{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Summary:   brief,
		})
	}

	return ctx, nil
}

func (s *NarrativeMemoryService) buildGlobalSummary(novel *model.Novel) string {
	var sb strings.Builder
	sb.WriteString("【故事概要】\n" + novel.Description)
	if novel.Worldview != nil {
		sb.WriteString("\n\n【世界观】\n")
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString("修炼体系：" + novel.Worldview.MagicSystem + "\n")
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString("地理：" + novel.Worldview.Geography + "\n")
		}
		if novel.Worldview.Culture != "" {
			sb.WriteString("文化：" + novel.Worldview.Culture + "\n")
		}
	}
	return sb.String()
}

func renderHierarchicalContext(ctx *HierarchicalContext) string {
	var sb strings.Builder
	sb.WriteString(ctx.GlobalSummary)

	if len(ctx.ArcSummaries) > 0 {
		sb.WriteString("\n\n【历史弧光摘要（长期记忆）】\n")
		for _, arc := range ctx.ArcSummaries {
			sb.WriteString(fmt.Sprintf("▸ 弧%d（第%d-%d章）：%s\n",
				arc.ArcNo, arc.StartChapter, arc.EndChapter, arc.Summary))
		}
	}

	if len(ctx.RecentShort) > 0 {
		sb.WriteString("\n【近期章节回顾】\n")
		for _, ch := range ctx.RecentShort {
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, ch.Summary))
		}
	}

	if len(ctx.RecentDetailed) > 0 {
		sb.WriteString("\n【上章详情（直接前情）】\n")
		for _, ch := range ctx.RecentDetailed {
			sum := ch.Summary
			if sum == "" {
				sum = "（摘要待生成）"
			}
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, sum))
		}
	}

	return sb.String()
}

// ──────────────────────────────────────────────
// TriggerArcSummaryIfNeeded
// ──────────────────────────────────────────────

// TriggerArcSummaryIfNeeded 在章节写完后检查是否需要生成弧摘要，异步执行
func (s *NarrativeMemoryService) TriggerArcSummaryIfNeeded(tenantID, novelID uint, completedChapterNo int) {
	if completedChapterNo%arcSize != 0 {
		return
	}
	arcNo := completedChapterNo / arcSize
	startChapter := completedChapterNo - arcSize + 1
	go func() {
		if err := s.generateArcSummary(tenantID, novelID, arcNo, startChapter, completedChapterNo); err != nil {
			log.Printf("NarrativeMemory: arc %d summary failed (novel %d): %v", arcNo, novelID, err)
		} else {
			log.Printf("NarrativeMemory: arc %d summary done (novel %d, ch %d-%d)", arcNo, novelID, startChapter, completedChapterNo)
		}
	}()
}

func (s *NarrativeMemoryService) generateArcSummary(tenantID, novelID uint, arcNo, startChapter, endChapter int) error {
	type chSummary struct {
		ChapterNo int
		Title     string
		Summary   string
	}
	var chapters []chSummary
	for chNo := startChapter; chNo <= endChapter; chNo++ {
		ch, err := s.chapterRepo.GetByNovelAndChapterNo(novelID, chNo)
		if err != nil || ch == nil {
			continue
		}
		chapters = append(chapters, chSummary{ChapterNo: ch.ChapterNo, Title: ch.Title, Summary: ch.Summary})
	}
	if len(chapters) == 0 {
		return fmt.Errorf("arc %d: no chapters [%d-%d]", arcNo, startChapter, endChapter)
	}

	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return err
	}

	tmplStr := loadPromptTemplate("arc_summary.tmpl")
	tmpl, err := template.New("arc_summary").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parse arc_summary.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":       novel.Title,
		"ArcNo":            arcNo,
		"StartChapter":     startChapter,
		"EndChapter":       endChapter,
		"ChapterSummaries": chapters,
	}); err != nil {
		return err
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "arc_summary", buf.String(), "")
	if err != nil {
		return fmt.Errorf("AI arc summary: %w", err)
	}

	resp = extractJSON(strings.TrimSpace(resp))
	var result struct {
		ArcSummary       string                   `json:"arc_summary"`
		KeyEvents        []map[string]interface{} `json:"key_events"`
		CharacterChanges map[string]string        `json:"character_changes"`
		OpenForeshadows  []string                 `json:"open_foreshadows"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return fmt.Errorf("unmarshal arc summary: %w", err)
	}

	keyEventsJSON, _ := json.Marshal(result.KeyEvents)
	charChangesJSON, _ := json.Marshal(result.CharacterChanges)
	openForeshadowsJSON, _ := json.Marshal(result.OpenForeshadows)

	now := time.Now()
	existing, _ := s.arcRepo.GetByNovelAndArcNo(novelID, arcNo)
	if existing != nil {
		existing.Summary = result.ArcSummary
		existing.KeyEvents = string(keyEventsJSON)
		existing.CharacterChanges = string(charChangesJSON)
		existing.OpenForeshadows = string(openForeshadowsJSON)
		existing.UpdatedAt = now
		return s.arcRepo.Update(existing)
	}
	return s.arcRepo.Create(&model.ArcSummary{
		NovelID:          novelID,
		ArcNo:            arcNo,
		StartChapter:     startChapter,
		EndChapter:       endChapter,
		Summary:          result.ArcSummary,
		KeyEvents:        string(keyEventsJSON),
		CharacterChanges: string(charChangesJSON),
		OpenForeshadows:  string(openForeshadowsJSON),
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

// ──────────────────────────────────────────────
// GenerateChapterSummary
// ──────────────────────────────────────────────

// GenerateChapterSummary 为已生成章节内容生成80-120字摘要
func (s *NarrativeMemoryService) GenerateChapterSummary(tenantID uint, chapter *model.Chapter, novelTitle string) (string, error) {
	tmplStr := loadPromptTemplate("chapter_summary.tmpl")
	tmpl, err := template.New("chapter_summary").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      truncateForPrompt(chapter.Content, 6000),
	}); err != nil {
		return "", err
	}
	summary, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_summary", buf.String(), "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(summary), nil
}

// ──────────────────────────────────────────────
// GenerateChapterTitle
// ──────────────────────────────────────────────

// GenerateChapterTitle 根据摘要和情感基调生成创意章节标题
func (s *NarrativeMemoryService) GenerateChapterTitle(tenantID uint, chapter *model.Chapter, genre, emotionalTone string) (string, error) {
	tmplStr := loadPromptTemplate("chapter_title.tmpl")
	tmpl, err := template.New("chapter_title").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"Genre":         genre,
		"Summary":       chapter.Summary,
		"EmotionalTone": emotionalTone,
	}); err != nil {
		return "", err
	}
	title, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_title", buf.String(), "")
	if err != nil {
		return "", err
	}
	title = strings.TrimSpace(strings.Trim(title, "「」『』\"'【】"))
	runes := []rune(title)
	if len(runes) > 20 {
		title = string(runes[:20])
	}
	return title, nil
}

// ──────────────────────────────────────────────
// ExtractCharacterVoice
// ──────────────────────────────────────────────

// ExtractCharacterVoice 从已有章节中提取角色对话风格（JSON格式）
func (s *NarrativeMemoryService) ExtractCharacterVoice(tenantID uint, character *model.Character, novelID uint) (string, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return "", err
	}

	var samples strings.Builder
	count := 0
	for _, ch := range chapters {
		if count >= 25 || ch.Content == "" {
			break
		}
		content := truncateForPrompt(ch.Content, 3000)
		if !strings.Contains(content, character.Name) {
			continue
		}
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, character.Name) && len([]rune(line)) > 5 {
				samples.WriteString(line + "\n")
				count++
				if count >= 25 {
					break
				}
			}
		}
	}

	if samples.Len() == 0 {
		return "", fmt.Errorf("no dialogue samples for %s", character.Name)
	}

	tmplStr := loadPromptTemplate("character_voice.tmpl")
	tmpl, err := template.New("character_voice").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"Name":            character.Name,
		"Role":            character.Role,
		"Personality":     character.Personality,
		"Background":      character.Background,
		"DialogueSamples": samples.String(),
	}); err != nil {
		return "", err
	}
	return s.aiService.GenerateWithProvider(tenantID, novelID, "character_voice", buf.String(), "")
}

// ──────────────────────────────────────────────
// RefineChapterContent
// ──────────────────────────────────────────────

// RefineChapterContent 对章节内容做一轮精修（仅在检测到质量问题时执行）
func (s *NarrativeMemoryService) RefineChapterContent(tenantID uint, chapter *model.Chapter, novelTitle string) (string, error) {
	focusAreas := detectRefinementNeeds(chapter.Content)
	if focusAreas == "" {
		return chapter.Content, nil
	}

	tmplStr := loadPromptTemplate("refinement_pass.tmpl")
	tmpl, err := template.New("refinement").Parse(tmplStr)
	if err != nil {
		return chapter.Content, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      chapter.Content,
		"FocusAreas":   focusAreas,
	}); err != nil {
		return chapter.Content, err
	}

	refined, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "refinement", buf.String(), "")
	if err != nil {
		log.Printf("NarrativeMemory: refinement ch%d failed: %v — using original", chapter.ChapterNo, err)
		return chapter.Content, nil
	}
	return strings.TrimSpace(refined), nil
}

// ──────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────

// detectRefinementNeeds 检测内容质量问题，返回需要修复的问题描述（空则跳过精修）
func detectRefinementNeeds(content string) string {
	if content == "" {
		return ""
	}
	var issues []string

	for _, w := range repeatWords {
		if cnt := strings.Count(content, w); cnt >= repeatWordThreshold {
			issues = append(issues, fmt.Sprintf("「%s」×%d", w, cnt))
		}
	}

	// 检测连续段落以「他」/「她」开头
	consecutive := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if len([]rune(line)) < 3 {
			continue
		}
		first := string([]rune(line)[:1])
		if first == "他" || first == "她" {
			consecutive++
		} else {
			consecutive = 0
		}
		if consecutive >= consecutivePronounThreshold {
			issues = append(issues, "连续段落以「他/她」开头")
			break
		}
	}

	return strings.Join(issues, "、")
}

// truncateForPrompt 截断文本至 maxChars 字
func truncateForPrompt(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "…"
}

