package service

import (
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

const (
	arcSize          = 10            // 每弧章节数
	halfArcSize      = arcSize / 2   // 中段预摘要间隔（每5章）
	recentFullCount  = 3             // 最近N章注入详细摘要
	recentShortCount = 7             // 再往前N章注入简短摘要（30字）

	shortSummaryMaxRunes        = 80 // 简短摘要截断字符数
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
	snapshotRepo  snapshotLatestGetter
}

type novelGetter interface {
	GetByID(id uint) (*model.Novel, error)
}

type chapterMemoryRepo interface {
	GetByID(id uint) (*model.Chapter, error)
	GetRecent(novelID uint, chapterNo int, count int) ([]*model.Chapter, error)
	ListByNovel(novelID uint) ([]*model.Chapter, error)
	ListByNovelWithContent(novelID uint) ([]*model.Chapter, error)
	GetByNovelAndChapterNo(novelID uint, chapterNo int) (*model.Chapter, error)
	GetByNovelAndChapterRange(novelID uint, start, end int) ([]*model.Chapter, error)
}

type characterLister interface {
	ListByNovel(novelID uint) ([]*model.Character, error)
}

type snapshotLatestGetter interface {
	GetLatestForCharacter(characterID uint) (*model.CharacterStateSnapshot, error)
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

func (s *NarrativeMemoryService) WithSnapshotRepo(repo snapshotLatestGetter) *NarrativeMemoryService {
	s.snapshotRepo = repo
	return s
}

// ──────────────────────────────────────────────
// HierarchicalContext data structs
// ──────────────────────────────────────────────

type HierarchicalContext struct {
	RecentDetailed   []ChapterBrief // 最近 recentFullCount 章（详细摘要）
	RecentShort      []ChapterBrief // 再往前 recentShortCount 章（简短摘要）
	ArcSummaries     []ArcBrief     // 已完成弧
	GlobalSummary    string
	PlotTensionState string         // 当前剧情张力状态（供场景大纲决策参考）
	Characters       []CharacterBrief // 主要角色设定
}

type ChapterBrief struct {
	ChapterNo   int
	Title       string
	Summary     string
	ContentHead string // 正文前150字，用于摘要未生成时的临时上下文
}

type ArcBrief struct {
	ArcNo        int
	StartChapter int
	EndChapter   int
	Summary      string
	KeyEvents    string
}

type CharacterBrief struct {
	Name         string
	Role         string
	Description  string
	CurrentState string // 最新快照状态，为空表示无快照
}

// ──────────────────────────────────────────────
// BuildHierarchicalContext
// ──────────────────────────────────────────────

// BuildPlotTensionStateText 返回当前剧情张力状态文本（供场景大纲模板使用）
func (s *NarrativeMemoryService) BuildPlotTensionStateText(novelID uint, currentChapterNo int) string {
	return s.buildPlotTensionState(novelID, currentChapterNo)
}

// BuildHierarchicalContext 返回供 prompt 注入的层次化上下文文本
func (s *NarrativeMemoryService) BuildHierarchicalContext(novelID uint, currentChapterNo int) (string, error) {
	logger.Printf("[NarrativeMemory] BuildHierarchicalContext: novelID=%d chapterNo=%d", novelID, currentChapterNo)
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
		GlobalSummary:    s.buildGlobalSummary(novel),
		PlotTensionState: s.buildPlotTensionState(novel.ID, currentChapterNo),
	}

	// 加载角色信息（含最新快照状态，供上下文渲染使用）
	if chars, err := s.characterRepo.ListByNovel(novel.ID); err == nil {
		for _, c := range chars {
			brief := CharacterBrief{
				Name:        c.Name,
				Role:        c.Role,
				Description: c.Description,
			}
			if s.snapshotRepo != nil {
				if snap, snapErr := s.snapshotRepo.GetLatestForCharacter(c.ID); snapErr == nil && snap != nil {
					brief.CurrentState = formatCharacterState(snap)
				}
			}
			ctx.Characters = append(ctx.Characters, brief)
		}
	}

	// 弧摘要（所有已完成弧 + 当前弧中段预摘要）
	// 一次查询加载所有弧摘要，再做内存 map 查找，避免 N+1
	completedArcs := (currentChapterNo - 1) / arcSize
	allArcs, arcErr := s.arcRepo.ListByNovel(novel.ID)
	if arcErr != nil {
		logger.Printf("[NarrativeMemory] gatherContext: arc query failed (novel %d): %v — arc context skipped", novel.ID, arcErr)
	}
	arcMap := make(map[int]*model.ArcSummary, len(allArcs))
	for _, a := range allArcs {
		arcMap[a.ArcNo] = a
	}
	for arcNo := 1; arcNo <= completedArcs; arcNo++ {
		arc := arcMap[arcNo]
		if arc == nil {
			continue
		}
		ctx.ArcSummaries = append(ctx.ArcSummaries, ArcBrief{
			ArcNo:        arc.ArcNo,
			StartChapter: arc.StartChapter,
			EndChapter:   arc.EndChapter,
			Summary:      arc.Summary,
			KeyEvents:    arc.KeyEvents,
		})
	}

	// 当前弧中段预摘要（填补11-19章等弧内盲区）
	// 中段摘要以负 arcNo 存储（见 TriggerArcSummaryIfNeeded）
	lastMidArcCh := (currentChapterNo - 1) / halfArcSize * halfArcSize
	if lastMidArcCh > 0 && lastMidArcCh%arcSize != 0 {
		midArcNo := -(lastMidArcCh / halfArcSize)
		if arc := arcMap[midArcNo]; arc != nil {
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
	recentDetailed, recentErr := s.chapterRepo.GetRecent(novel.ID, currentChapterNo, recentFullCount)
	if recentErr != nil {
		logger.Printf("[NarrativeMemory] gatherContext: recent chapters query failed (novel %d): %v — recent context skipped", novel.ID, recentErr)
	}
	for i := len(recentDetailed) - 1; i >= 0; i-- {
		ch := recentDetailed[i]
		// 取章末 400 字作为 fallback（章末比章头更能体现"上次发生了什么"）
		tail := ""
		if runes := []rune(ch.Content); len(runes) > 0 {
			start := len(runes) - 400
			if start < 0 {
				start = 0
			}
			tail = string(runes[start:])
		}
		ctx.RecentDetailed = append(ctx.RecentDetailed, ChapterBrief{
			ChapterNo:   ch.ChapterNo,
			Title:       ch.Title,
			Summary:     ch.Summary,
			ContentHead: tail,
		})
	}

	// 稍远章简短摘要（往前第3~10章）
	// 一次 BETWEEN 查询代替逐章 GetByNovelAndChapterNo，避免 N+1
	shortStart := currentChapterNo - recentFullCount - recentShortCount
	if shortStart < 1 {
		shortStart = 1
	}
	shortEnd := currentChapterNo - recentFullCount - 1
	if shortEnd >= shortStart {
		shortChapters, shortErr := s.chapterRepo.GetByNovelAndChapterRange(novel.ID, shortStart, shortEnd)
		if shortErr != nil {
			logger.Printf("[NarrativeMemory] gatherContext: short chapters query failed (novel %d): %v — short context skipped", novel.ID, shortErr)
		}
		chMap := make(map[int]*model.Chapter, len(shortChapters))
		for _, ch := range shortChapters {
			chMap[ch.ChapterNo] = ch
		}
		for chNo := shortEnd; chNo >= shortStart; chNo-- {
			ch := chMap[chNo]
			if ch == nil {
				continue
			}
			brief := ch.Summary
			if brief == "" && ch.Content != "" {
				// 摘要缺失时，用章末80字兜底（章末比章头更能反映结局状态）
				r := []rune(ch.Content)
				s := len(r) - 80
				if s < 0 {
					s = 0
				}
				brief = "…" + string(r[s:])
			}
			if len([]rune(brief)) > shortSummaryMaxRunes {
				brief = string([]rune(brief)[:shortSummaryMaxRunes]) + "…"
			}
			if brief == "" {
				continue // 既无摘要也无内容，跳过此章
			}
			ctx.RecentShort = append(ctx.RecentShort, ChapterBrief{
				ChapterNo: ch.ChapterNo,
				Title:     ch.Title,
				Summary:   brief,
			})
		}
	}

	return ctx, nil
}

// buildPlotTensionState 分析近期章节张力走势，生成供场景大纲决策参考的状态描述
func (s *NarrativeMemoryService) buildPlotTensionState(novelID uint, currentChapterNo int) string {
	if currentChapterNo <= 1 {
		return ""
	}
	lookback := 5
	recent, err := s.chapterRepo.GetRecent(novelID, currentChapterNo, lookback)
	if err != nil || len(recent) == 0 {
		return ""
	}

	// 收集有效张力值（>0 表示已填充）
	type tensionPoint struct {
		chapterNo int
		level     int
		hook      string
	}
	var points []tensionPoint
	for i := len(recent) - 1; i >= 0; i-- { // 升序排列
		ch := recent[i]
		if ch.TensionLevel > 0 {
			points = append(points, tensionPoint{ch.ChapterNo, ch.TensionLevel, ch.ChapterHook})
		}
	}
	if len(points) == 0 {
		return ""
	}

	current := points[len(points)-1].level

	// 判断走势
	pattern := "plateau"
	if len(points) >= 2 {
		first, last := points[0].level, points[len(points)-1].level
		diff := last - first
		if diff >= 2 {
			pattern = "rising"
		} else if diff <= -2 {
			pattern = "falling"
		}
	}

	// 统计连续章节数
	consecutiveLow, consecutiveHigh := 0, 0
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].level <= 4 {
			consecutiveLow++
		} else {
			break
		}
	}
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].level >= 7 {
			consecutiveHigh++
		} else {
			break
		}
	}

	// 收集近章未收尾的钩子
	var hooks []string
	for _, p := range points {
		if p.hook != "" {
			hooks = append(hooks, fmt.Sprintf("第%d章钩子：「%s」", p.chapterNo, truncateForPrompt(p.hook, 40)))
		}
	}

	var sb strings.Builder
	patternZH := map[string]string{"rising": "持续上升", "falling": "持续下降", "plateau": "平稳维持"}[pattern]
	sb.WriteString(fmt.Sprintf("- 当前张力值：%d/10（近%d章走势：%s）\n", current, len(points), patternZH))

	if consecutiveHigh >= 3 {
		sb.WriteString(fmt.Sprintf("- ⚠️ 已连续%d章高张力（≥7），读者需要喘息空间，本章应安排低张力过渡或情感缓冲\n", consecutiveHigh))
	} else if consecutiveLow >= 3 {
		sb.WriteString(fmt.Sprintf("- ⚠️ 已连续%d章低张力（≤4），读者期待爆发，本章必须制造重大冲突或意外反转\n", consecutiveLow))
	} else if pattern == "rising" {
		sb.WriteString("- 张力持续上升中，可以在本章安排一个小高潮，或引入新的外部威胁推向更高点\n")
	} else if pattern == "falling" {
		sb.WriteString("- 张力连续下降，本章必须逆转趋势：引入新危机、揭示隐藏威胁、或打破当前平静\n")
	}

	if len(hooks) > 0 {
		sb.WriteString("- 待延续的上章悬念（本章应回应或深化）：\n")
		for _, h := range hooks {
			sb.WriteString("  · " + h + "\n")
		}
	}

	return sb.String()
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
		if novel.Worldview.CheatSystem != "" {
			sb.WriteString("金手指/系统：" + novel.Worldview.CheatSystem + "\n")
		}
	}
	return sb.String()
}

func renderHierarchicalContext(ctx *HierarchicalContext) string {
	var sb strings.Builder
	sb.WriteString(ctx.GlobalSummary)

	if len(ctx.Characters) > 0 {
		sb.WriteString("\n\n【主要角色设定】\n")
		for _, c := range ctx.Characters {
			if isProtagonistRole(c.Role) {
				sb.WriteString(fmt.Sprintf("⚠️【主角】%s（%s）：%s", c.Name, c.Role, c.Description))
				if c.CurrentState != "" {
					sb.WriteString("\n  → 当前状态：" + c.CurrentState)
				}
				sb.WriteString("\n")
			} else {
				line := fmt.Sprintf("- %s（%s）：%s", c.Name, c.Role, c.Description)
				if c.CurrentState != "" {
					line += "【状态：" + c.CurrentState + "】"
				}
				sb.WriteString(line + "\n")
			}
		}
	}

	if ctx.PlotTensionState != "" {
		sb.WriteString("\n\n【剧情张力状态】\n")
		sb.WriteString(ctx.PlotTensionState)
	}

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
		sb.WriteString("\n【近三章详情（直接前情）】\n")
		for _, ch := range ctx.RecentDetailed {
			sum := ch.Summary
			if sum == "" {
				// 摘要尚未生成时，使用章末内容作为临时上下文
				if ch.ContentHead != "" {
					sum = "（章末节选）…" + ch.ContentHead
				} else {
					sum = "（摘要待生成）"
				}
			}
			sb.WriteString(fmt.Sprintf("第%d章「%s」：%s\n", ch.ChapterNo, ch.Title, sum))
		}
	}

	return sb.String()
}

// ──────────────────────────────────────────────
// TriggerArcSummaryIfNeeded
// ──────────────────────────────────────────────

// TriggerArcSummaryIfNeeded 在章节写完后检查是否需要生成弧摘要，异步执行。
// 触发条件：
//   - 每 arcSize 章末尾 → 完整弧摘要（arcNo = 1,2,3…）
//   - 每 halfArcSize 章且不与完整弧重合 → 中段预摘要（arcNo = -1,-2,-3…）
//
// 两个触发条件互斥（case 2 的 %arcSize != 0 保证），不会重复执行。
// 中段摘要使用负 arcNo 以与完整弧区分，BuildHierarchicalContext 会一并检索。
func (s *NarrativeMemoryService) TriggerArcSummaryIfNeeded(tenantID, novelID uint, completedChapterNo int) {
	switch {
	case completedChapterNo%arcSize == 0:
		// 完整弧结束 — 显式传参避免闭包捕获外层变量（防止快速连续调用时的竞态）
		arcNo := completedChapterNo / arcSize
		startChapter := completedChapterNo - arcSize + 1
		go func(arcNo, startChapter, endChapter int) {
			if err := s.generateArcSummary(tenantID, novelID, arcNo, startChapter, endChapter); err != nil {
				logger.Printf("NarrativeMemory: arc %d summary failed (novel %d): %v", arcNo, novelID, err)
			} else {
				logger.Printf("NarrativeMemory: arc %d summary done (novel %d, ch %d-%d)", arcNo, novelID, startChapter, endChapter)
			}
		}(arcNo, startChapter, completedChapterNo)
	case completedChapterNo > halfArcSize && completedChapterNo%halfArcSize == 0:
		// 弧中段预摘要（第5、15、25...章）；arcNo 用负数标识，不覆盖完整弧
		midArcNo := -(completedChapterNo / halfArcSize)
		startChapter := completedChapterNo - halfArcSize + 1
		go func(midArcNo, startChapter, endChapter int) {
			if err := s.generateArcSummary(tenantID, novelID, midArcNo, startChapter, endChapter); err != nil {
				logger.Printf("NarrativeMemory: mid-arc %d summary failed (novel %d): %v", midArcNo, novelID, err)
			} else {
				logger.Printf("NarrativeMemory: mid-arc %d summary done (novel %d, ch %d-%d)", midArcNo, novelID, startChapter, endChapter)
			}
		}(midArcNo, startChapter, completedChapterNo)
	}
}

func (s *NarrativeMemoryService) generateArcSummary(tenantID, novelID uint, arcNo, startChapter, endChapter int) error {
	logger.Printf("[NarrativeMemory] generateArcSummary: novelID=%d arcNo=%d ch%d~ch%d", novelID, arcNo, startChapter, endChapter)
	type chSummary struct {
		ChapterNo int
		Title     string
		Summary   string
	}
	var chapters []chSummary
	rawChapters, err := s.chapterRepo.GetByNovelAndChapterRange(novelID, startChapter, endChapter)
	if err != nil {
		return fmt.Errorf("arc %d: fetch chapters [%d-%d]: %w", arcNo, startChapter, endChapter, err)
	}
	for _, ch := range rawChapters {
		chapters = append(chapters, chSummary{ChapterNo: ch.ChapterNo, Title: ch.Title, Summary: ch.Summary})
	}
	if len(chapters) == 0 {
		return fmt.Errorf("arc %d: no chapters [%d-%d]", arcNo, startChapter, endChapter)
	}

	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return err
	}

	prompt, err := renderPrompt("arc_summary", map[string]interface{}{
		"NovelTitle":       novel.Title,
		"ArcNo":            arcNo,
		"StartChapter":     startChapter,
		"EndChapter":       endChapter,
		"ChapterSummaries": chapters,
	})
	if err != nil {
		return fmt.Errorf("render arc_summary: %w", err)
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "arc_summary", prompt, "")
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

	logger.Printf("[NarrativeMemory] generateArcSummary done: novelID=%d arcNo=%d", novelID, arcNo)
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
	logger.Printf("[NarrativeMemory] GenerateChapterSummary: novelID=%d chapterNo=%d", chapter.NovelID, chapter.ChapterNo)
	prompt, err := renderPrompt("chapter_summary", map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      truncateForPrompt(chapter.Content, 6000),
	})
	if err != nil {
		return "", err
	}
	summary, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_summary", prompt, "")
	if err != nil {
		logger.Printf("[NarrativeMemory] GenerateChapterSummary AI error: chapterNo=%d err=%v", chapter.ChapterNo, err)
		return "", err
	}
	summary = strings.TrimSpace(summary)
	logger.Printf("[NarrativeMemory] GenerateChapterSummary done: novelID=%d chapterNo=%d len=%d", chapter.NovelID, chapter.ChapterNo, len(summary))
	return summary, nil
}

// ──────────────────────────────────────────────
// GenerateChapterTitle
// ──────────────────────────────────────────────

// GenerateChapterTitle 根据摘要和情感基调生成创意章节标题
func (s *NarrativeMemoryService) GenerateChapterTitle(tenantID uint, chapter *model.Chapter, genre, emotionalTone string) (string, error) {
	logger.Printf("[NarrativeMemory] GenerateChapterTitle: novelID=%d chapterNo=%d", chapter.NovelID, chapter.ChapterNo)
	prompt, err := renderPrompt("chapter_title", map[string]interface{}{
		"Genre":         genre,
		"Summary":       chapter.Summary,
		"EmotionalTone": emotionalTone,
	})
	if err != nil {
		return "", err
	}
	title, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_title", prompt, "")
	if err != nil {
		logger.Printf("[NarrativeMemory] GenerateChapterTitle AI error: chapterNo=%d err=%v", chapter.ChapterNo, err)
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
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
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

	prompt, err := renderPrompt("character_voice", map[string]interface{}{
		"Name":            character.Name,
		"Role":            character.Role,
		"Personality":     character.Description,
		"Background":      character.Description,
		"DialogueSamples": samples.String(),
	})
	if err != nil {
		return "", err
	}
	return s.aiService.GenerateWithProvider(tenantID, novelID, "character_voice", prompt, "")
}

// ──────────────────────────────────────────────
// RefineChapterContent
// ──────────────────────────────────────────────

// RefineChapterContent 对章节内容做一轮精修（仅在检测到质量问题时执行）
func (s *NarrativeMemoryService) RefineChapterContent(tenantID uint, chapter *model.Chapter, novelTitle string) (string, error) {
	logger.Printf("[NarrativeMemory] RefineChapterContent: novelID=%d chapterNo=%d", chapter.NovelID, chapter.ChapterNo)
	focusAreas := detectRefinementNeeds(chapter.Content)
	if focusAreas == "" {
		return chapter.Content, nil
	}

	prompt, err := renderPrompt("refinement_pass", map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      chapter.Content,
		"FocusAreas":   focusAreas,
	})
	if err != nil {
		return chapter.Content, err
	}

	refined, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "refinement", prompt, "")
	if err != nil {
		logger.Printf("NarrativeMemory: refinement ch%d failed: %v — using original", chapter.ChapterNo, err)
		return chapter.Content, nil
	}
	refined = strings.TrimSpace(refined)

	// 护栏：精修后字数不能比原文少超过 20%，防止 AI 删减情节
	origRunes := len([]rune(chapter.Content))
	refinedRunes := len([]rune(refined))
	if origRunes > 0 && refinedRunes < origRunes*80/100 {
		logger.Printf("NarrativeMemory: refinement ch%d rejected — word count dropped %d→%d (>20%%)", chapter.ChapterNo, origRunes, refinedRunes)
		return chapter.Content, nil
	}

	return refined, nil
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

