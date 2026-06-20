package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
)

const (
	arcSize          = 10          // 每弧章节数
	halfArcSize      = arcSize / 2 // 中段预摘要间隔（每5章）
	recentFullCount  = 3           // 最近N章注入详细摘要
	recentShortCount = 7           // 再往前N章注入简短摘要（30字）

	shortSummaryMaxRunes        = 300 // 简短摘要截断字符数
	repeatWordThreshold         = 5  // 重复词出现 N 次触发精修建议
	consecutivePronounThreshold = 4  // 连续以他/她开头的段落数阈值
	clichePhraseThreshold       = 2  // 长套话短语出现 N 次即触发精修（阈值低于单字）
)

// repeatWords 高频 AI 套词（出现≥5次触发精修）
var repeatWords = []string{
	"突然", "忽然", "然后", "接着", "不禁", "不由得", "内心",
	// 情绪直白陈述
	"感到", "觉得", "沉吟", "叹了口气", "深呼吸",
}

// clichePhrases 低频但典型的 AI 机器感短语（出现≥2次触发精修）
var clichePhrases = []string{
	"心中涌起", "内心深处", "复杂的情绪", "说不清的感觉",
	"与此同时", "就在此时", "不知过了多久",
	"这意味着", "换句话说", "确实如此",
	"空气中弥漫", "沉默笼罩",
}

// ============================================
// NarrativeMemoryService 层次化记忆服务
// ============================================
//
// 构建层次化上下文：
//   近章详摘（最近2章）→ 近章简摘（往前8章）→ 弧光摘要（每10章）→ 全局概述
//
// 该设计让第50章的生成能访问完整的故事记忆，不再受制于上下文窗口。

type NarrativeMemoryService struct {
	novelRepo          novelGetterUpdater
	chapterRepo        chapterMemoryRepo
	characterRepo      characterLister
	arcRepo            *repository.ArcSummaryRepository
	aiService          *AIService
	snapshotRepo       snapshotLatestGetter
	outlineVersionRepo outlineVersionCreator
	cache              *redis.Client // optional: for cross-instance arc gen deduplication

	// arcGenLocks 进程内去重锁（无 Redis 时使用；Redis 可用时同时维护以支持 WaitForArcSummary）。
	// key: "novelID-arcNo" (string), value: struct{}{}
	arcGenLocks sync.Map
}

type novelGetterUpdater interface {
	GetByID(id uint) (*model.Novel, error)
	UpdateFields(id uint, fields map[string]interface{}) error
}

// novelGetter 保留向后兼容（其他服务使用此接口名）
type novelGetter = novelGetterUpdater

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

type outlineVersionCreator interface {
	Create(v *model.NovelOutlineVersion) error
	MaxVersion(novelID uint) (int, error)
	CreateVersionAtomic(v *model.NovelOutlineVersion) error
}

func NewNarrativeMemoryService(
	novelRepo novelGetterUpdater,
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

// WithRedis enables cross-instance arc summary deduplication via Redis SETNX.
func (s *NarrativeMemoryService) WithRedis(c *redis.Client) *NarrativeMemoryService {
	s.cache = c
	return s
}

// tryLockArc acquires the generation lock for the given arc.
// Redis SETNX is the primary gate (cross-instance); arcGenLocks is also set
// so WaitForArcSummary can poll process-locally.
// Returns true if this caller acquired the lock.
func (s *NarrativeMemoryService) tryLockArc(lockKey string) bool {
	if s.cache != nil {
		ok, err := s.cache.SetNX(context.Background(), "lock:arc:gen:"+lockKey, "1", 10*time.Minute).Result()
		if err != nil {
			logger.Errorf("[NarrativeMemory] Redis SETNX arc lock %s: %v, fallback to local lock", lockKey, err)
			_, loaded := s.arcGenLocks.LoadOrStore(lockKey, struct{}{})
			return !loaded
		}
		if !ok {
			return false
		}
		s.arcGenLocks.Store(lockKey, struct{}{})
		return true
	}
	_, loaded := s.arcGenLocks.LoadOrStore(lockKey, struct{}{})
	return !loaded
}

// unlockArc releases the generation lock for the given arc.
func (s *NarrativeMemoryService) unlockArc(lockKey string) {
	s.arcGenLocks.Delete(lockKey)
	if s.cache != nil {
		_ = s.cache.Del(context.Background(), "lock:arc:gen:"+lockKey).Err()
	}
}

func (s *NarrativeMemoryService) WithOutlineVersionRepo(repo outlineVersionCreator) *NarrativeMemoryService {
	s.outlineVersionRepo = repo
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
	ContentTail string // 正文前150字，用于摘要未生成时的临时上下文
}

type ArcBrief struct {
	ArcNo            int
	StartChapter     int
	EndChapter       int
	Summary          string
	KeyEvents        string
	CharacterChanges string // JSON map: {"角色名": "变化描述"}
	OpenForeshadows  string // JSON array: ["伏笔描述"]
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
const maxHierarchicalContextBytes = 50 * 1024 // 50KB 上下文上限，防止超出 LLM 输入窗口

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
	result := renderHierarchicalContextPriority(ctx)
	return result, nil
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
		logger.Errorf("[NarrativeMemory] gatherContext: arc query failed (novel %d): %v — arc context skipped", novel.ID, arcErr)
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
			ArcNo:            arc.ArcNo,
			StartChapter:     arc.StartChapter,
			EndChapter:       arc.EndChapter,
			Summary:          arc.Summary,
			KeyEvents:        arc.KeyEvents,
			CharacterChanges: arc.CharacterChanges,
			OpenForeshadows:  arc.OpenForeshadows,
		})
	}

	// 当前弧中段预摘要（填补11-19章等弧内盲区）
	// 中段摘要以负 arcNo 存储（见 TriggerArcSummaryIfNeeded）
	lastMidArcCh := (currentChapterNo - 1) / halfArcSize * halfArcSize
	if lastMidArcCh > 0 && lastMidArcCh%arcSize != 0 {
		midArcNo := -(lastMidArcCh / halfArcSize)
		if arc := arcMap[midArcNo]; arc != nil {
			ctx.ArcSummaries = append(ctx.ArcSummaries, ArcBrief{
				ArcNo:            arc.ArcNo,
				StartChapter:     arc.StartChapter,
				EndChapter:       arc.EndChapter,
				Summary:          arc.Summary,
				KeyEvents:        arc.KeyEvents,
				CharacterChanges: arc.CharacterChanges,
				OpenForeshadows:  arc.OpenForeshadows,
			})
		}
	}

	// 近章详细摘要（最近 recentFullCount 章）
	recentDetailed, recentErr := s.chapterRepo.GetRecent(novel.ID, currentChapterNo, recentFullCount)
	if recentErr != nil {
		logger.Errorf("[NarrativeMemory] gatherContext: recent chapters query failed (novel %d): %v — recent context skipped", novel.ID, recentErr)
	}
	for i := len(recentDetailed) - 1; i >= 0; i-- {
		ch := recentDetailed[i]
		// 跳过大纲阶段写入的占位章节（无内容也无摘要），避免上下文中出现空洞
		if ch.Content == "" && ch.Summary == "" {
			continue
		}
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
			ContentTail: tail,
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
			logger.Errorf("[NarrativeMemory] gatherContext: short chapters query failed (novel %d): %v — short context skipped", novel.ID, shortErr)
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
				// 摘要缺失时，取章首150字+章末150字兜底（同时保留开场状态和结局状态）
				const fbHead, fbTail = 150, 150
				r := []rune(ch.Content)
				if len(r) <= fbHead+fbTail {
					brief = "（未摘要）" + string(r)
				} else {
					brief = "（未摘要）" + string(r[:fbHead]) + "…（中略）…" + string(r[len(r)-fbTail:])
				}
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
		if novel.Worldview.Description != "" {
			sb.WriteString("概述：" + novel.Worldview.Description + "\n")
		}
		if novel.Worldview.MagicSystem != "" {
			sb.WriteString("修炼体系：" + novel.Worldview.MagicSystem + "\n")
		}
		if novel.Worldview.Geography != "" {
			sb.WriteString("关键地点：" + novel.Worldview.Geography + "\n")
		}
		if novel.Worldview.History != "" {
			sb.WriteString("背景矛盾：" + novel.Worldview.History + "\n")
		}
		if novel.Worldview.Rules != "" {
			sb.WriteString("【⚠️世界规则（必须严格遵守）】\n" + novel.Worldview.Rules + "\n")
		}
	}
	return sb.String()
}

// renderHierarchicalContextPriority 按重要性优先级拼接上下文，超出50KB时：
//  1. 先截断 arc 摘要（每条限100字）
//  2. 保留完整 GlobalSummary + RecentDetailed
//  3. 最终还是超出则截尾部并加前缀 "…（较早内容已压缩）\n"
func renderHierarchicalContextPriority(ctx *HierarchicalContext) string {
	// 第一步：尝试完整渲染
	result := renderHierarchicalContext(ctx)
	if len(result) <= maxHierarchicalContextBytes {
		return result
	}

	// 第二步：压缩 arc 摘要（每条截至100字）
	compressed := make([]ArcBrief, len(ctx.ArcSummaries))
	for i, a := range ctx.ArcSummaries {
		compressed[i] = a
		runes := []rune(a.Summary)
		if len(runes) > 100 {
			compressed[i].Summary = string(runes[:100]) + "…"
		}
	}
	ctxCompressed := *ctx
	ctxCompressed.ArcSummaries = compressed
	result = renderHierarchicalContext(&ctxCompressed)
	if len(result) <= maxHierarchicalContextBytes {
		return result
	}

	// 第三步：按重要性拼接（GlobalSummary + RecentDetailed 优先，ArcSummaries 次之，RecentShort 最后）
	ctxMin := HierarchicalContext{
		GlobalSummary:    ctx.GlobalSummary,
		PlotTensionState: ctx.PlotTensionState,
		Characters:       ctx.Characters,
		RecentDetailed:   ctx.RecentDetailed,
	}

	// 逐条追加 arc 摘要，不超限
	arcLines := []ArcBrief{}
	for _, a := range compressed {
		arcLines = append(arcLines, a)
		ctxTry := ctxMin
		ctxTry.ArcSummaries = arcLines
		if len(renderHierarchicalContext(&ctxTry)) > maxHierarchicalContextBytes {
			arcLines = arcLines[:len(arcLines)-1]
			break
		}
	}
	ctxMin.ArcSummaries = arcLines

	// 逐条追加 recent short，不超限
	shortLines := []ChapterBrief{}
	for _, s := range ctx.RecentShort {
		shortLines = append(shortLines, s)
		ctxTry := ctxMin
		ctxTry.RecentShort = shortLines
		if len(renderHierarchicalContext(&ctxTry)) > maxHierarchicalContextBytes {
			shortLines = shortLines[:len(shortLines)-1]
			break
		}
	}
	ctxMin.RecentShort = shortLines

	result = renderHierarchicalContext(&ctxMin)
	if len(result) <= maxHierarchicalContextBytes {
		return result
	}

	// 最终兜底：截尾部
	runes := []rune(result)
	maxRunes := maxHierarchicalContextBytes / 3
	if len(runes) > maxRunes {
		return "…（较早内容已压缩）\n" + string(runes[len(runes)-maxRunes:])
	}
	return result
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
			if arc.CharacterChanges != "" {
				var ccMap map[string]string
				if json.Unmarshal([]byte(arc.CharacterChanges), &ccMap) == nil && len(ccMap) > 0 {
					sb.WriteString("  角色变化：")
					for name, change := range ccMap {
						sb.WriteString(fmt.Sprintf("[%s→%s] ", name, change))
					}
					sb.WriteString("\n")
				}
			}
			if arc.OpenForeshadows != "" {
				var fsList []string
				if json.Unmarshal([]byte(arc.OpenForeshadows), &fsList) == nil && len(fsList) > 0 {
					sb.WriteString("  未解伏笔：")
					for _, f := range fsList {
						sb.WriteString("「" + f + "」 ")
					}
					sb.WriteString("\n")
				}
			}
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
				if ch.ContentTail != "" {
					sum = "（章末节选）…" + ch.ContentTail
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
		lockKey := fmt.Sprintf("%d-%d", novelID, arcNo)
		if !s.tryLockArc(lockKey) {
			logger.Printf("[NarrativeMemory] arc %d (novel %d) already generating, skip", arcNo, novelID)
			return
		}
		go func(arcNo, startChapter, endChapter int, lockKey string) {
			defer s.unlockArc(lockKey)
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[NarrativeMemory] arc summary panic: %v", r)
				}
			}()
			if err := s.generateArcSummary(tenantID, novelID, arcNo, startChapter, endChapter); err != nil {
				logger.Errorf("NarrativeMemory: arc %d summary failed (novel %d): %v", arcNo, novelID, err)
			} else {
				logger.Printf("NarrativeMemory: arc %d summary done (novel %d, ch %d-%d)", arcNo, novelID, startChapter, endChapter)
			}
		}(arcNo, startChapter, completedChapterNo, lockKey)
	case completedChapterNo%halfArcSize == 0 && completedChapterNo%arcSize != 0:
		// 弧中段预摘要（第5、15、25...章）；arcNo 用负数标识，不覆盖完整弧
		midArcNo := -(completedChapterNo / halfArcSize)
		startChapter := completedChapterNo - halfArcSize + 1
		lockKey := fmt.Sprintf("%d-%d", novelID, midArcNo)
		if !s.tryLockArc(lockKey) {
			logger.Printf("[NarrativeMemory] mid-arc %d (novel %d) already generating, skip", midArcNo, novelID)
			return
		}
		go func(midArcNo, startChapter, endChapter int, lockKey string) {
			defer s.unlockArc(lockKey)
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[NarrativeMemory] arc summary panic: %v", r)
				}
			}()
			if err := s.generateArcSummary(tenantID, novelID, midArcNo, startChapter, endChapter); err != nil {
				logger.Errorf("NarrativeMemory: mid-arc %d summary failed (novel %d): %v", midArcNo, novelID, err)
			} else {
				logger.Printf("NarrativeMemory: mid-arc %d summary done (novel %d, ch %d-%d)", midArcNo, novelID, startChapter, endChapter)
			}
		}(midArcNo, startChapter, completedChapterNo, lockKey)
	}
}

// WaitForArcSummary blocks until the arc summary for the given arcNo of the given novel is done
// (i.e., the async goroutine has released its lock), or until timeout elapses.
// Checks both the process-local arcGenLocks and the Redis key so cross-instance arc generation
// on other instances is also detected.
func (s *NarrativeMemoryService) WaitForArcSummary(novelID uint, arcNo int, timeout time.Duration) {
	lockKey := fmt.Sprintf("%d-%d", novelID, arcNo)
	redisKey := "lock:arc:gen:" + lockKey
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, localRunning := s.arcGenLocks.Load(lockKey); localRunning {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if s.cache != nil {
			if n, err := s.cache.Exists(context.Background(), redisKey).Result(); err == nil && n > 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}
		return
	}
	logger.Printf("[NarrativeMemory] WaitForArcSummary: timeout waiting for arc %d of novel %d", arcNo, novelID)
}

func (s *NarrativeMemoryService) generateArcSummary(tenantID, novelID uint, arcNo, startChapter, endChapter int) (retErr error) {
	arcStart := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ArcSummaryTotal.WithLabelValues(status).Inc()
		metrics.ArcSummaryDuration.Observe(time.Since(arcStart).Seconds())
	}()
	logger.Printf("[NarrativeMemory] generateArcSummary: novelID=%d arcNo=%d ch%d~ch%d", novelID, arcNo, startChapter, endChapter)
	// chSummary 除摘要外，额外携带章首/章末原文节选和章末钩子。
	// 弧摘要 AI 只靠摘要（约200字/章）会丢失大量细节（伏笔措辞、对话权力博弈、世界观细节）；
	// 加入原文节选后，AI 能更准确地识别 open_foreshadows 和角色状态变化。
	type chSummary struct {
		ChapterNo      int
		Title          string
		Summary        string
		OpeningExcerpt string // 章首150字原文（记录开场状态）
		EndingExcerpt  string // 章末150字原文（记录结局状态）
		Hook           string // 章末钩子（设置的伏笔悬念）
	}
	var chapters []chSummary
	rawChapters, err := s.chapterRepo.GetByNovelAndChapterRange(novelID, startChapter, endChapter)
	if err != nil {
		return fmt.Errorf("arc %d: fetch chapters [%d-%d]: %w", arcNo, startChapter, endChapter, err)
	}
	for _, ch := range rawChapters {
		entry := chSummary{
			ChapterNo: ch.ChapterNo,
			Title:     ch.Title,
			Summary:   ch.Summary,
			Hook:      ch.ChapterHook,
		}
		if ch.Content != "" {
			r := []rune(ch.Content)
			const excerptLen = 150
			if len(r) <= excerptLen*2 {
				entry.OpeningExcerpt = string(r)
			} else {
				entry.OpeningExcerpt = string(r[:excerptLen])
				entry.EndingExcerpt = string(r[len(r)-excerptLen:])
			}
		}
		chapters = append(chapters, entry)
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
		ArcSummary        string                   `json:"arc_summary"`
		KeyEvents         []map[string]interface{} `json:"key_events"`
		CharacterChanges  map[string]string        `json:"character_changes"`  // 旧字段，向后兼容
		CharacterStates   map[string]string        `json:"character_states"`   // 新字段（优先使用）
		RelationshipMap   []map[string]interface{} `json:"relationship_map"`
		ResolvedConflicts []string                 `json:"resolved_conflicts"`
		OpenForeshadows   []string                 `json:"open_foreshadows"`
		WorldUpdates      string                   `json:"world_updates"`
		ProtagonistPower  string                   `json:"protagonist_power_level"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return fmt.Errorf("unmarshal arc summary: %w", err)
	}

	// 合并 character_changes（旧）和 character_states（新）
	charMap := result.CharacterStates
	if len(charMap) == 0 {
		charMap = result.CharacterChanges
	}

	// 将主角实力、角色章末状态、已解决冲突嵌入 Summary 文本，
	// 无需修改数据模型，renderHierarchicalContext 直接可用。
	// P2-8: 主角实力前置，确保弧摘要被压缩至100字时关键实力信息仍在首部（不被截断）。
	fullSummary := result.ArcSummary
	if result.ProtagonistPower != "" {
		powerShort := result.ProtagonistPower
		if pr := []rune(powerShort); len(pr) > 25 {
			powerShort = string(pr[:25])
		}
		fullSummary = "【主角实力】" + powerShort + " " + fullSummary
	}
	if len(charMap) > 0 {
		fullSummary += "\n【关键角色章末状态】"
		for name, state := range charMap {
			fullSummary += "\n- " + name + "：" + state
		}
	}
	if len(result.ResolvedConflicts) > 0 {
		fullSummary += "\n【本弧已解决的冲突（后续章节不得重提为未解决）】"
		for _, rc := range result.ResolvedConflicts {
			fullSummary += "\n✅ " + rc
		}
	}

	keyEventsJSON, _ := json.Marshal(result.KeyEvents)
	charChangesJSON, _ := json.Marshal(charMap)
	openForeshadowsJSON, _ := json.Marshal(result.OpenForeshadows)

	logger.Printf("[NarrativeMemory] generateArcSummary done: novelID=%d arcNo=%d summaryLen=%d", novelID, arcNo, len(fullSummary))
	now := time.Now()
	// Upsert: ON DUPLICATE KEY UPDATE — 多实例并发时第二个写入直接覆盖而不会产生重复行。
	if err := s.arcRepo.Upsert(&model.ArcSummary{
		NovelID:          novelID,
		ArcNo:            arcNo,
		StartChapter:     startChapter,
		EndChapter:       endChapter,
		Summary:          fullSummary,
		KeyEvents:        string(keyEventsJSON),
		CharacterChanges: string(charChangesJSON),
		OpenForeshadows:  string(openForeshadowsJSON),
		CreatedAt:        now,
		UpdatedAt:        now,
	}); err != nil {
		return err
	}

	// 正向弧（非中段预摘要）结束后，自适应修订后续章节大纲
	if arcNo > 0 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[NarrativeMemory] adaptOutlineAfterArc panic: %v", r)
				}
			}()
			if err := s.adaptOutlineAfterArc(tenantID, novelID, arcNo, endChapter, result.ArcSummary,
				result.KeyEvents, charMap, result.WorldUpdates,
				result.OpenForeshadows, result.ResolvedConflicts, result.ProtagonistPower); err != nil {
				logger.Errorf("[NarrativeMemory] adaptOutlineAfterArc failed: novelID=%d arcNo=%d: %v", novelID, arcNo, err)
			}
		}()
	}

	return nil
}

// ──────────────────────────────────────────────
// adaptOutlineAfterArc
// ──────────────────────────────────────────────

// adaptOutlineAfterArc 在正向弧摘要生成后，用弧光数据修订后续章节大纲。
// 只修改 chapter_no > endChapter 的章节；已完成章节不受影响。
// 若后续章节为空（小说已写完）或无大纲，则跳过。
func (s *NarrativeMemoryService) adaptOutlineAfterArc(
	tenantID, novelID uint,
	arcNo, endChapter int,
	arcSummary string,
	keyEvents []map[string]interface{},
	charStates map[string]string,
	worldUpdates string,
	openForeshadows []string,
	resolvedConflicts []string,
	protagonistPower string,
) error {
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: get novel: %w", err)
	}
	if novel.Outline == "" {
		logger.Printf("[NarrativeMemory] adaptOutlineAfterArc: novel %d has no outline, skip", novelID)
		return nil
	}

	// 解析全量大纲 JSON
	var fullOutline map[string]json.RawMessage
	if err := json.Unmarshal([]byte(novel.Outline), &fullOutline); err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: parse outline: %w", err)
	}
	var allChapters []map[string]interface{}
	if chaptersRaw, ok := fullOutline["chapters"]; ok {
		if err := json.Unmarshal(chaptersRaw, &allChapters); err != nil {
			return fmt.Errorf("adaptOutlineAfterArc: parse chapters: %w", err)
		}
	}

	// 分离已完成和剩余章节
	var completedChapters []map[string]interface{}
	var remainingChapters []map[string]interface{}
	for _, ch := range allChapters {
		no, _ := ch["chapter_no"].(float64)
		if int(no) <= endChapter {
			completedChapters = append(completedChapters, ch)
		} else {
			remainingChapters = append(remainingChapters, ch)
		}
	}
	if len(remainingChapters) == 0 {
		logger.Printf("[NarrativeMemory] adaptOutlineAfterArc: no remaining chapters after arc %d (novel %d), skip", arcNo, novelID)
		return nil
	}

	nextChapter := endChapter + 1
	lastChapter := endChapter + len(remainingChapters)

	remainingJSON, _ := json.MarshalIndent(remainingChapters, "", "  ")

	prompt, err := renderPrompt("arc_outline_update", map[string]interface{}{
		"NovelTitle":        novel.Title,
		"ArcNo":             arcNo,
		"StartChapter":      endChapter - arcSize + 1,
		"EndChapter":        endChapter,
		"NextChapter":       nextChapter,
		"LastChapter":       lastChapter,
		"RemainingCount":    len(remainingChapters),
		"ArcSummary":        arcSummary,
		"KeyEvents":         keyEvents,
		"CharacterStates":   charStates,
		"WorldUpdates":      worldUpdates,
		"OpenForeshadows":   openForeshadows,
		"ResolvedConflicts": resolvedConflicts,
		"ProtagonistPower":  protagonistPower,
		"RemainingOutlineJSON": string(remainingJSON),
	})
	if err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: render prompt: %w", err)
	}

	resp, err := s.aiService.GenerateWithProvider(tenantID, novelID, "arc_outline_update", prompt, "")
	if err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: AI call: %w", err)
	}

	resp = extractJSON(strings.TrimSpace(resp))
	var updatedChapters []map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &updatedChapters); err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: parse AI response: %w", err)
	}
	if len(updatedChapters) == 0 {
		return fmt.Errorf("adaptOutlineAfterArc: AI returned empty chapter list")
	}

	// 验证章节号匹配（防止AI返回错误章节）
	expectedNos := make(map[int]bool, len(remainingChapters))
	for _, ch := range remainingChapters {
		no, _ := ch["chapter_no"].(float64)
		expectedNos[int(no)] = true
	}
	for _, ch := range updatedChapters {
		no, _ := ch["chapter_no"].(float64)
		if !expectedNos[int(no)] {
			return fmt.Errorf("adaptOutlineAfterArc: AI returned unexpected chapter_no=%d", int(no))
		}
	}

	// 快照旧大纲（快照失败不阻断更新）
	if s.outlineVersionRepo != nil {
		_ = s.outlineVersionRepo.CreateVersionAtomic(&model.NovelOutlineVersion{
			NovelID: novelID,
			Prompt:  fmt.Sprintf("弧%d自适应更新（第%d-%d章完成后）", arcNo, endChapter-arcSize+1, endChapter),
			Outline: novel.Outline,
		})
	}

	// P2-7: 记录每章大纲变更详情，便于排查自适应修订的效果
	originalMap := make(map[int]map[string]interface{}, len(remainingChapters))
	for _, ch := range remainingChapters {
		no, _ := ch["chapter_no"].(float64)
		originalMap[int(no)] = ch
	}
	changedCount := 0
	for _, ch := range updatedChapters {
		no, _ := ch["chapter_no"].(float64)
		chNo := int(no)
		orig, ok := originalMap[chNo]
		if !ok {
			continue
		}
		var diffs []string
		for _, field := range []string{"summary", "plot_points", "title", "theme", "arc_goal"} {
			origVal := fmt.Sprintf("%v", orig[field])
			newVal := fmt.Sprintf("%v", ch[field])
			if origVal != newVal {
				diffs = append(diffs, field)
			}
		}
		if len(diffs) > 0 {
			changedCount++
			logger.Printf("[NarrativeMemory] adaptOutlineAfterArc: ch%d modified fields=%v", chNo, diffs)
		}
	}

	// 合并：已完成章节不变 + AI修订的剩余章节
	mergedChapters := append(completedChapters, updatedChapters...)
	fullOutline["chapters"], _ = json.Marshal(mergedChapters)
	updatedOutlineJSON, err := json.Marshal(fullOutline)
	if err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: marshal updated outline: %w", err)
	}

	if err := s.novelRepo.UpdateFields(novelID, map[string]interface{}{"outline": string(updatedOutlineJSON)}); err != nil {
		return fmt.Errorf("adaptOutlineAfterArc: save outline: %w", err)
	}

	logger.Printf("[NarrativeMemory] adaptOutlineAfterArc done: novelID=%d arcNo=%d total=%d changed=%d (ch%d~ch%d)",
		novelID, arcNo, len(updatedChapters), changedCount, nextChapter, lastChapter)
	return nil
}

// ──────────────────────────────────────────────
// GenerateChapterSummary
// ──────────────────────────────────────────────

// GenerateChapterSummary 为已生成章节内容生成80-120字摘要
func (s *NarrativeMemoryService) GenerateChapterSummary(tenantID uint, chapter *model.Chapter, novelTitle string) (retSummary string, retErr error) {
	summaryStart := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ChapterSummaryTotal.WithLabelValues(status).Inc()
		metrics.ChapterSummaryDuration.Observe(time.Since(summaryStart).Seconds())
	}()
	logger.Printf("[NarrativeMemory] GenerateChapterSummary: novelID=%d chapterNo=%d", chapter.NovelID, chapter.ChapterNo)

	// P0-1: 头尾截断，确保章末结局状态也被摘要覆盖（前6000字只看开头，无法感知章末事件）
	const sumHead, sumTail = 3000, 3000
	var contentForSummary string
	r := []rune(chapter.Content)
	if len(r) <= sumHead+sumTail {
		contentForSummary = chapter.Content
	} else {
		contentForSummary = string(r[:sumHead]) + "\n…（中间段已省略）…\n" + string(r[len(r)-sumTail:])
	}

	prompt, err := renderPrompt("chapter_summary", map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      contentForSummary,
	})
	if err != nil {
		return "", err
	}

	// P1-3: 摘要过短时重试（最多3次），防止 AI 返回草率的一句话摘要
	const minSummaryRunes = 150
	var summary string
	for attempt := 0; attempt < 3; attempt++ {
		summary, err = s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "chapter_summary", prompt, "")
		if err != nil {
			logger.Errorf("[NarrativeMemory] GenerateChapterSummary AI error: chapterNo=%d attempt=%d err=%v", chapter.ChapterNo, attempt+1, err)
			return "", err
		}
		summary = strings.TrimSpace(summary)
		if len([]rune(summary)) >= minSummaryRunes {
			break
		}
		logger.Printf("[NarrativeMemory] GenerateChapterSummary retry %d: summary too short (%d chars, min=%d) for ch%d",
			attempt+1, len([]rune(summary)), minSummaryRunes, chapter.ChapterNo)
	}

	logger.Printf("[NarrativeMemory] GenerateChapterSummary done: novelID=%d chapterNo=%d len=%d", chapter.NovelID, chapter.ChapterNo, len(summary))
	return summary, nil
}

// ──────────────────────────────────────────────
// GenerateChapterTitle
// ──────────────────────────────────────────────

// GenerateChapterTitle 根据摘要和情感基调生成创意章节标题
func (s *NarrativeMemoryService) GenerateChapterTitle(tenantID uint, chapter *model.Chapter, genre, emotionalTone string) (retTitle string, retErr error) {
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ChapterTitleTotal.WithLabelValues(status).Inc()
	}()
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
		logger.Errorf("[NarrativeMemory] GenerateChapterTitle AI error: chapterNo=%d err=%v", chapter.ChapterNo, err)
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

// maxRefinementContentRunes 精修时传入 LLM 的最大正文字符数。
// 9000字覆盖绝大多数章节（含8000字长章）；超出时用头尾截取保留开头与结局两端。
const maxRefinementContentRunes = 9000

// RefineChapterContent 对章节内容做一轮精修（仅在检测到质量问题时执行）
func (s *NarrativeMemoryService) RefineChapterContent(tenantID uint, chapter *model.Chapter, novelTitle string) (string, error) {
	logger.Printf("[NarrativeMemory] RefineChapterContent: novelID=%d chapterNo=%d", chapter.NovelID, chapter.ChapterNo)
	refineStatus := "success"
	defer func() { metrics.ChapterRefinementTotal.WithLabelValues(refineStatus).Inc() }()
	focusAreas := detectRefinementNeeds(chapter.Content)
	if focusAreas == "" {
		refineStatus = "skipped"
		return chapter.Content, nil
	}

	// 对超长章节用头尾截取传入 prompt（覆盖开头与结局两端），避免超出 LLM context window。
	// 护栏对照截断长度（而非原文长度）判断，防止截断导致误判。
	contentForPrompt, wasTruncated := truncateForRefinement(chapter.Content, maxRefinementContentRunes)

	prompt, err := renderPrompt("refinement_pass", map[string]interface{}{
		"NovelTitle":   novelTitle,
		"ChapterNo":    chapter.ChapterNo,
		"ChapterTitle": chapter.Title,
		"Content":      contentForPrompt,
		"FocusAreas":   focusAreas,
	})
	if err != nil {
		return chapter.Content, err
	}

	refined, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "refinement", prompt, "")
	if err != nil {
		logger.Errorf("NarrativeMemory: refinement ch%d failed: %v — using original", chapter.ChapterNo, err)
		refineStatus = "error"
		return chapter.Content, nil
	}
	refined = strings.TrimSpace(refined)

	// 护栏：精修后字数不能比送入 AI 的内容少超过 20%，防止 AI 大量删减。
	// 超长章节截断时，对照截断长度而非原文长度，避免误判。
	baseRunes := len([]rune(contentForPrompt))
	refinedRunes := len([]rune(refined))
	if baseRunes > 0 && refinedRunes < baseRunes*80/100 {
		logger.Printf("NarrativeMemory: refinement ch%d rejected — word count dropped %d→%d (>20%%)", chapter.ChapterNo, baseRunes, refinedRunes)
		refineStatus = "rejected"
		return chapter.Content, nil
	}

	// 超长章节：精修结果只覆盖头尾部分，中间段保留原文
	if wasTruncated {
		half := maxRefinementContentRunes / 2
		origRunes := []rune(chapter.Content)
		refinedRunes2 := []rune(refined)
		// 用精修后的头尾替换原文对应部分，中间段保持不变
		if len(refinedRunes2) >= half && len(origRunes) > maxRefinementContentRunes {
			middle := origRunes[half : len(origRunes)-half]
			merged := append(refinedRunes2[:half], middle...)
			merged = append(merged, refinedRunes2[len(refinedRunes2)-half:]...)
			return string(merged), nil
		}
		// P1-6: 合并条件不满足（AI 输出过短无法对齐头尾），保守返回原文，避免中间内容丢失
		logger.Errorf("NarrativeMemory: refinement ch%d wasTruncated but merge failed (refinedLen=%d < half=%d) — using original",
			chapter.ChapterNo, len(refinedRunes2), half)
		refineStatus = "rejected"
		return chapter.Content, nil
	}

	return refined, nil
}

// ──────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────

// flatStartConnectors 顺序连接词：段首出现3+次表示平铺直叙
var flatStartConnectors = []string{"随后", "接着", "然后", "于是", "之后", "就这样", "就在这时"}

// weakEndingResolutionWords 章末平静解决词（缺少悬念时出现意味着收尾无力）
var weakEndingResolutionWords = []string{
	"离开了", "回去了", "离去", "皆大欢喜", "顺利", "满意地", "安心",
	"放松了", "平静下来", "舒了口气", "微微点头", "点了点头", "心满意足",
}

// weakEndingTensionWords 章末张力词（出现则不视为无力收尾）
var weakEndingTensionWords = []string{
	"然而", "不对", "危险", "警觉", "皱眉", "？！", "！？",
	"猛然", "骤然", "忽地", "突然", "难道", "怎么", "怎会",
}

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

	// 低频但典型的 AI 机器感短语（阈值更低）
	for _, p := range clichePhrases {
		if cnt := strings.Count(content, p); cnt >= clichePhraseThreshold {
			issues = append(issues, fmt.Sprintf("套语「%s」×%d", p, cnt))
		}
	}

	// 检测连续段落以「他」/「她」开头
	// P1-4: 空行视为段落边界，必须重置 consecutive（否则跨段落误计）
	consecutive := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if len([]rune(line)) < 3 {
			consecutive = 0 // 空行 = 段落边界，重置连续计数
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

	// P0-2a: 检测平铺直叙（连续3+段落以顺序连接词开头，叙述缺乏起伏）
	flatCount := 0
	maxFlatCount := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if len([]rune(line)) < 10 {
			if len([]rune(line)) == 0 {
				flatCount = 0 // 空行重置
			}
			continue
		}
		isFlat := false
		for _, conn := range flatStartConnectors {
			if strings.HasPrefix(line, conn) {
				isFlat = true
				break
			}
		}
		if isFlat {
			flatCount++
			if flatCount > maxFlatCount {
				maxFlatCount = flatCount
			}
		} else {
			flatCount = 0
		}
	}
	if maxFlatCount >= 3 {
		issues = append(issues, fmt.Sprintf("平铺直叙（连续%d段落以顺序连接词开头）", maxFlatCount))
	}

	// P0-2b: 检测章末收尾无力（最后200字含平静解决词但不含张力词，意味着无钩子/悬念）
	const weakEndingWindow = 200
	contentRunes := []rune(content)
	if len(contentRunes) > 0 {
		startIdx := len(contentRunes) - weakEndingWindow
		if startIdx < 0 {
			startIdx = 0
		}
		ending := strings.ReplaceAll(string(contentRunes[startIdx:]), "【章末钩子】", "")
		hasResolution := false
		hasTension := false
		for _, w := range weakEndingResolutionWords {
			if strings.Contains(ending, w) {
				hasResolution = true
				break
			}
		}
		for _, w := range weakEndingTensionWords {
			if strings.Contains(ending, w) {
				hasTension = true
				break
			}
		}
		if hasResolution && !hasTension {
			issues = append(issues, "章末收尾平淡（缺少悬念或钩子）")
		}
	}

	return strings.Join(issues, "、")
}

// truncateForPrompt 截断文本至 maxChars 字（前向截断，适用于普通场景）
func truncateForPrompt(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "…"
}

// truncateForRefinement 头尾截取文本，保留开头与结局两端，适合精修场景。
// 返回截取后内容及是否发生了截取。
func truncateForRefinement(s string, maxChars int) (string, bool) {
	r := []rune(s)
	if len(r) <= maxChars {
		return s, false
	}
	half := maxChars / 2
	head := string(r[:half])
	tail := string(r[len(r)-half:])
	return head + "\n\n…（中间段已省略，精修时保持前后文风格一致）…\n\n" + tail, true
}

