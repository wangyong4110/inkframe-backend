package service

// video_storyboard_service.go
//
// Storyboard generation, review, optimize, and shot-management methods
// extracted from video_service.go to keep the primary file manageable.
// All methods remain on *VideoService — no new types required.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"gorm.io/gorm"
)

// ─── Package-level constants for magic numbers ───────────────────────────────

const (
	defaultShotDurationSecs  = 5.0    // 默认分镜时长（秒）
	maxSegmentRunes          = 3500   // 每段最多字符数（约 25 个镜头，≈5000 tokens）
	charRuneOverlapThreshold = 0.7    // 角色名模糊匹配汉字重叠比例阈值（需≥70%重叠，避免"炎少"误匹配"萧炎"）
	shiftTempOffset          = 100000 // 两阶段 shot_no 位移时使用的临时偏移量，避免 MySQL 唯一键冲突
)

// beatDialogueLineRe 匹配纯文本节拍行里的"角色名：台词"对话格式，其余整行视为动作/描写。
// 与前端 ScreenplayTab.vue 的 serializeBeats/deserializeBeats 采用同一约定，保持前后端一致
// （ScreenplayScene.Beats 现在按行存纯文本，不再是结构化 []ScreenplayBeat 数组）。
var beatDialogueLineRe = regexp.MustCompile(`^([^：:]{1,20})[：:]\s*(.+)$`)

// parseBeatLine 解析一行节拍文本，返回节拍类型（action/dialogue）、说话人（非对话行为空）、内容。
func parseBeatLine(line string) (beatType, speaker, text string) {
	if m := beatDialogueLineRe.FindStringSubmatch(line); m != nil {
		return "dialogue", m[1], m[2]
	}
	return "action", "", line
}

// splitBeatLines 把 ScreenplayScene.Beats 纯文本拆成非空行。
func splitBeatLines(beats string) []string {
	raw := strings.Split(beats, "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// beatSheetItem 是从 storyboard_beat.j2 解析出的单个情节节拍
type beatSheetItem struct {
	No             int      `json:"no"`
	BeatType       string   `json:"beat_type"`
	Importance     string   `json:"importance"` // major/minor：供简洁模式（StoryboardMode=concise）筛选，只转化 major 节拍为分镜
	ContentSummary string   `json:"content_summary"`
	Location       string   `json:"location"`
	Characters     []string `json:"characters"`
	SuggestedShots int      `json:"suggested_shots"`
}

// ─── Storyboard Generation ────────────────────────────────────────────────────

// StoryboardGenResult carries the generated shots plus enough metadata for the caller to
// detect degraded/partial generation (some segments exhausted retries and produced nothing)
// instead of treating any non-empty result as a full success.
type StoryboardGenResult struct {
	Shots          []*model.StoryboardShot
	RequestedShots int // total shots calcTotalShots() aimed for
	FailedSegments int // number of content segments that produced zero shots after all retries
	TotalSegments  int
}

// GenerateStoryboardCtx 生成分镜
// ctx: 取消信号来源——调用方（handler）通过 TaskService.RegisterCancel 注册的 cancel 函数最终
// 会 Done() 这个 ctx，用来真正打断正在进行的 AI 调用，而不仅仅是跳过下一个分段。
// progressFn: 可选的进度回调（0-99），供调用方更新任务进度（传 nil 则跳过）
func (s *VideoService) GenerateStoryboardCtx(ctx context.Context, videoID uint, provider string, progressFn func(int), overrides StoryboardOverrides, chapterIDOverride ...*uint) (*StoryboardGenResult, error) {
	metrics.StoryboardGenerationInFlight.Inc()
	defer metrics.StoryboardGenerationInFlight.Dec()
	sbStart := time.Now()

	// Prevent concurrent storyboard generation for the same video — across instances via Redis SETNX.
	// This is a mutual-exclusion lock (a different concern from cancellation, which now flows through ctx).
	//
	// Lease + heartbeat instead of a single long TTL: a fixed 30min TTL meant that if the process
	// crashed or was restarted mid-generation, the deferred Del() never ran and the lock stayed
	// orphaned for up to 30 minutes, permanently blocking retries until it expired. Using a short
	// TTL that's continuously renewed while this goroutine is alive means a crash/restart releases
	// the lock within one lease period instead of up to 30 minutes, while still correctly blocking a
	// genuinely concurrent generation for the same video.
	if s.cache != nil {
		redisKey := lockKey("storyboard", "gen", videoID)
		const leaseTTL = 45 * time.Second
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", leaseTTL).Result()
		if err == nil {
			if !ok {
				metrics.StoryboardGenerationTotal.WithLabelValues("conflict").Inc()
				return nil, fmt.Errorf("storyboard generation already in progress for video %d", videoID)
			}
			renewDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(leaseTTL / 3)
				defer ticker.Stop()
				for {
					select {
					case <-renewDone:
						return
					case <-ticker.C:
						s.cache.Expire(context.Background(), redisKey, leaseTTL)
					}
				}
			}()
			defer close(renewDone)
			defer s.cache.Del(context.Background(), redisKey)
		}
		// err != nil: Redis unavailable, fall through without the distributed lock
	}

	totalStart := time.Now()

	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		metrics.StoryboardGenerationTotal.WithLabelValues("error").Inc()
		return nil, err
	}

	// 租户状态校验（与 StartGeneration 保持一致）
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		metrics.StoryboardGenerationTotal.WithLabelValues("error").Inc()
		return nil, err
	}

	// 允许调用方覆盖 chapterID（解决 StoryboardService 忽略 chapterID 的问题）
	chapterID := video.ChapterID
	if len(chapterIDOverride) > 0 && chapterIDOverride[0] != nil {
		chapterID = chapterIDOverride[0]
		// 同步更新 video 记录，保持一致性
		video.ChapterID = chapterID
		if err := s.videoRepo.Update(video); err != nil {
			logger.Errorf("[VideoService] failed to update video chapterID: %v", err)
		}
	}

	var content string
	chapterNo := 0
	if chapterID != nil {
		chapter, chErr := s.chapterRepo.GetByID(*chapterID)
		if chErr != nil {
			logger.Errorf("[Storyboard] GetByID chapterID=%d: %v", *chapterID, chErr)
		}
		if chapter != nil {
			content = chapter.Content
			chapterNo = chapter.ChapterNo
		}
	}
	if strings.TrimSpace(content) == "" {
		metrics.StoryboardGenerationTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("章节内容为空，请先在「写作」页面编写章节内容再生成分镜脚本")
	}

	const minChapterLength = 100 // characters
	if len([]rune(content)) < minChapterLength {
		metrics.StoryboardGenerationTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("chapter content too short (%d chars): minimum %d characters required for storyboard generation",
			len([]rune(content)), minChapterLength)
	}

	// 并行预取角色、场景锚点、情节点、道具（避免多次串行 DB 查询）
	tenantID := s.videoTenantID(video)
	var characters []*model.Character
	var anchors []*model.SceneAnchor
	var plotPoints []*model.PlotPoint
	var effectiveItems []*EffectiveItem
	{
		var wgPre sync.WaitGroup
		novelID := video.NovelID
		if novelID > 0 {
			wgPre.Add(1)
			go func() {
				defer wgPre.Done()
				// 优先使用章节级绑定的角色；无绑定或 repo 未配置时退回小说全量
				if chapterID != nil && s.chapterCharacterRepo != nil {
					bindings, e := s.chapterCharacterRepo.ListByChapter(*chapterID)
					if e != nil {
						logger.Errorf("[Storyboard] chapterCharacterRepo.ListByChapter chapterID=%d: %v", *chapterID, e)
					}
					if len(bindings) > 0 {
						ids := make([]uint, 0, len(bindings))
						for _, b := range bindings {
							ids = append(ids, b.CharacterID)
						}
						chars, e2 := s.characterRepo.ListByIDs(ids)
						if e2 != nil {
							logger.Errorf("[Storyboard] characterRepo.ListByIDs chapterID=%d: %v", *chapterID, e2)
						} else {
							characters = chars
							logger.Printf("[Storyboard] using chapter-bound characters chapterID=%d count=%d", *chapterID, len(characters))
							return
						}
					}
				}
				var e error
				characters, e = s.characterRepo.ListByNovel(novelID)
				if e != nil {
					logger.Errorf("[Storyboard] characterRepo.ListByNovel novelID=%d: %v", novelID, e)
				}
				logger.Printf("[Storyboard] using novel-level characters novelID=%d count=%d", novelID, len(characters))
			}()
			if s.sceneAnchorSvc != nil {
				wgPre.Add(1)
				go func() {
					defer wgPre.Done()
					// 优先使用章节级绑定的场景锚点；无绑定时退回小说全量
					if chapterID != nil {
						chAnchors, e := s.sceneAnchorSvc.ListChapterAnchors(novelID, *chapterID)
						if e != nil {
							logger.Errorf("[Storyboard] sceneAnchorSvc.ListChapterAnchors chapterID=%d: %v", *chapterID, e)
						}
						if len(chAnchors) > 0 {
							anchors = chAnchors
							logger.Printf("[Storyboard] using chapter-bound anchors chapterID=%d count=%d", *chapterID, len(anchors))
							return
						}
					}
					var e error
					anchors, e = s.sceneAnchorSvc.ListByNovel(novelID)
					if e != nil {
						logger.Errorf("[Storyboard] sceneAnchorSvc.ListByNovel novelID=%d: %v", novelID, e)
					}
					logger.Printf("[Storyboard] using novel-level anchors novelID=%d count=%d", novelID, len(anchors))
				}()
			}
		}
		if s.plotPointRepo != nil && chapterID != nil {
			wgPre.Add(1)
			go func() {
				defer wgPre.Done()
				var e error
				plotPoints, e = s.plotPointRepo.ListByChapter(*chapterID)
				if e != nil {
					logger.Errorf("[Storyboard] plotPointRepo.ListByChapter chapterID=%d: %v", *chapterID, e)
				}
			}()
		}
		// 并行预取有效道具（合并项目级+章节级覆盖）
		if s.itemRepo != nil && novelID > 0 {
			wgPre.Add(1)
			go func() {
				defer wgPre.Done()
				allItems, err := s.itemRepo.ListByNovel(novelID)
				if err != nil || len(allItems) == 0 {
					return
				}
				overrideMap := make(map[uint]*model.ChapterItem)
				if chapterID != nil && s.chapterItemRepo != nil {
					if overrides, e := s.chapterItemRepo.ListByChapter(*chapterID); e == nil {
						for _, ci := range overrides {
							overrideMap[ci.ItemID] = ci
						}
					}
				}
				eis := make([]*EffectiveItem, 0, len(allItems))
				for _, item := range allItems {
					ei := &EffectiveItem{Item: *item, EffectiveLocation: item.Location, EffectiveOwner: item.Owner}
					if ov, ok := overrideMap[item.ID]; ok {
						ei.ChapterOverride = ov
						if ov.Location != "" {
							ei.EffectiveLocation = ov.Location
						}
						if ov.Owner != "" {
							ei.EffectiveOwner = ov.Owner
						}
					}
					eis = append(eis, ei)
				}
				effectiveItems = eis
				logger.Printf("[Storyboard] pre-fetched effectiveItems count=%d", len(effectiveItems))
			}()
		}
		wgPre.Wait()
		// 如果章节内无情节点，降级到小说级别
		if s.plotPointRepo != nil && len(plotPoints) == 0 && novelID > 0 {
			var e error
			plotPoints, e = s.plotPointRepo.ListByNovel(novelID, "", true)
			if e != nil {
				logger.Errorf("[Storyboard] plotPointRepo.ListByNovel novelID=%d: %v", novelID, e)
			}
		}
	}

	// 批量预加载角色默认形象 VisualPrompt（单次 IN 查询，替代 buildStoryboardPrompt 内 N_char×N_seg 次串行 GetByID）
	charVisualPrompts := make(map[uint]string)
	if s.lookupService != nil && len(characters) > 0 {
		lookIDs := make([]uint, 0, len(characters))
		charToLook := make(map[uint]uint) // charID → lookID
		for _, c := range characters {
			if c.DefaultLookID != 0 {
				lookIDs = append(lookIDs, c.DefaultLookID)
				charToLook[c.ID] = c.DefaultLookID
			}
		}
		if len(lookIDs) > 0 {
			if looksMap, err := s.lookupService.BatchGetLooksByIDs(lookIDs); err == nil {
				for charID, lookID := range charToLook {
					if look, ok := looksMap[lookID]; ok && look != nil && look.VisualPrompt != "" {
						charVisualPrompts[charID] = look.VisualPrompt
					}
				}
			} else {
				logger.Errorf("[Storyboard] BatchGetLooksByIDs failed: %v", err)
			}
		}
	}

	// 获取小说的 Genre、ImageStyle、标题、世界观摘要
	genre := ""
	imageStyle := ""
	novelTitle := ""
	worldviewDesc := ""
	if s.novelRepo != nil && video.NovelID > 0 {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
			genre = novel.Meta.Genre
			imageStyle = novel.AIConfig.ImageStyle
			novelTitle = novel.Title
			// 加载世界观摘要（仅第一章需要，但预取开销很小）
			if s.worldviewRepo != nil && novel.WorldviewID != nil {
				if wv, wvErr := s.worldviewRepo.GetByID(*novel.WorldviewID); wvErr == nil && wv != nil {
					desc := wv.Description
					// 截断至 300 字，避免撑大 prompt token
					if runes := []rune(desc); len(runes) > 300 {
						desc = string(runes[:300]) + "…"
					}
					worldviewDesc = desc
				}
			}
		}
	}
	// video 级别的 ArtStyle 作为兜底（novel 未设置时使用）
	if imageStyle == "" {
		imageStyle = video.RenderConfig.ArtStyle
	}

	totalRunes := len([]rune(content))

	// 预计算各段的镜头分配、百分比区间、节拍子集——纯本地计算，不涉及 AI 调用。
	type segPlan struct {
		segShotCount int
		segStartPct  int
		segEndPct    int
		segBeatSheet []map[string]interface{}
	}

	var segments []string
	var plans []segPlan
	var sceneIDs []uint // 与 segments 一一对应的 ScreenplaySceneID；非分场模式下为 nil
	var totalShots int

	// 提前做一次纯 DB 读取（无 AI 调用），判断本次是走"已有分场剧本"路径、
	// "需要自动生成分场剧本"路径、还是"文本分段+节拍表"路径——从而决定下方情感弧线（arcPlan）
	// 生成能与哪一个同样独立的 AI 前置调用并发执行。
	var existingScenes []*model.ScreenplayScene
	if s.screenplaySvc != nil && chapterID != nil {
		var scErr error
		existingScenes, scErr = s.screenplaySvc.ListScenes(*chapterID)
		if scErr != nil {
			logger.Errorf("[Storyboard] screenplaySvc.ListScenes chapterID=%d: %v", *chapterID, scErr)
		}
	}
	needAutoGenScreenplay := s.screenplaySvc != nil && chapterID != nil && len(existingScenes) == 0
	needBeatSheet := !needAutoGenScreenplay && len(existingScenes) == 0

	// P2 优化：arcPlan（情感弧线骨架）与"自动生成分场剧本"/"生成节拍表"互不依赖（各自独立的 AI 调用，
	// 互相不读取对方的输出），此前严格串行执行会把 arcPlan 的 5-10s 完全浪费在等待队列里；
	// 并发执行后总耗时约等于二者中较慢的一个，而不是二者相加。
	var arcPlan string
	var autoScenes []*model.ScreenplayScene
	var beatSheetItems []beatSheetItem
	if needBeatSheet {
		totalShots = calcTotalShots(totalRunes, video.Mode, video.StoryboardMode)
	}
	{
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			arcPlan = s.generateStoryboardArc(content, characters, tenantID, video.NovelID, provider, video.Mode)
		}()
		switch {
		case needAutoGenScreenplay:
			wg.Add(1)
			go func() {
				defer wg.Done()
				logger.Printf("[Storyboard] no screenplay scenes for chapterID=%d, auto-generating", *chapterID)
				scenes, scErr := s.screenplaySvc.GenerateScreenplayScenesCtx(ctx, tenantID, *chapterID, provider, true)
				if scErr != nil {
					logger.Errorf("[Storyboard] auto-generate screenplay chapterID=%d: %v (falling back to text segmentation)", *chapterID, scErr)
				}
				autoScenes = scenes
			}()
		case needBeatSheet:
			wg.Add(1)
			go func() {
				defer wg.Done()
				// P1a: 情节节拍表（Beat Sheet）——提取章节中每一个可视化叙事单元，
				// 注入到各 segment prompt 中，确保分镜逐拍覆盖原文情节，防止"平均主义"跳跃。
				beatSheetItems = s.generateBeatSheet(content, characters, anchors, tenantID, video.NovelID, provider, totalShots, video.Mode)
			}()
		}
		wg.Wait()
	}
	logger.Printf("[Storyboard] arc ready (%d chars)", len(arcPlan))
	if needBeatSheet {
		logger.Printf("[Storyboard] beatSheet ready: %d beats", len(beatSheetItems))
	}

	scenes := existingScenes
	if len(autoScenes) > 0 {
		scenes = autoScenes
	}

	// 方案B：已注入 ScreenplayService 且该章节存在分场剧本时，逐场生成分镜（每场=一个"段"），
	// 剧本内容替代原文分段+内存节拍表作为 AI 输入；未注入或无剧本数据时，走原有的文本分段路径。
	usedScreenplay := false
	if s.screenplaySvc != nil && chapterID != nil {
		if len(scenes) > 0 {
			usedScreenplay = true
			segments = make([]string, len(scenes))
			plans = make([]segPlan, len(scenes))
			sceneIDs = make([]uint, len(scenes))
			for i, sc := range scenes {
				var b strings.Builder
				fmt.Fprintf(&b, "【场次 %d】%s\n%s\n", sc.SceneNo, sc.Heading, sc.Synopsis)
				var segBeatSheet []map[string]interface{}
				for j, line := range splitBeatLines(sc.Beats) {
					beatType, speaker, text := parseBeatLine(line)
					if beatType == "dialogue" {
						fmt.Fprintf(&b, "%d. （对话）%s：%s\n", j+1, speaker, text)
					} else {
						fmt.Fprintf(&b, "%d. （%s）%s\n", j+1, beatType, text)
					}
					segBeatSheet = append(segBeatSheet, map[string]interface{}{
						"No":             j + 1,
						"BeatType":       beatType,
						"Importance":     "major", // 分场剧本的节拍暂无 AI 重要度标注，简洁模式下按 major 处理（不裁剪）
						"ContentSummary": text,
						"Location":       sc.Heading,
						"Characters":     speaker,
						"SuggestedShots": 0,
					})
				}
				segments[i] = b.String()
				shotCount := sc.EstimatedShotCount
				if shotCount < 1 {
					shotCount = 3
				}
				plans[i] = segPlan{
					segShotCount: shotCount,
					segStartPct:  i * 100 / len(scenes),
					segEndPct:    (i + 1) * 100 / len(scenes),
					segBeatSheet: segBeatSheet,
				}
				sceneIDs[i] = sc.ID
				totalShots += shotCount
			}
			logger.Printf("[Storyboard] using screenplay scenes chapterID=%d scenes=%d totalShots=%d", *chapterID, len(scenes), totalShots)
		}
	}

	if !usedScreenplay {
		// 正常情况下 needBeatSheet 已在上方与 arcPlan 并发算好 totalShots/beatSheetItems；
		// 仅当"本应自动生成分场剧本但失败"（needAutoGenScreenplay=true 却 usedScreenplay=false）
		// 这一少见的兜底场景下才会走到这里补算，此时退化为串行调用，不影响常规路径的并发收益。
		if !needBeatSheet {
			totalShots = calcTotalShots(totalRunes, video.Mode, video.StoryboardMode)
			// P1a: 情节节拍表（Beat Sheet）——提取章节中每一个可视化叙事单元，
			// 注入到各 segment prompt 中，确保分镜逐拍覆盖原文情节，防止"平均主义"跳跃。
			beatSheetItems = s.generateBeatSheet(content, characters, anchors, tenantID, video.NovelID, provider, totalShots, video.Mode)
			logger.Printf("[Storyboard] beatSheet ready (fallback): %d beats", len(beatSheetItems))
		}

		// 动态分段：确保每段期望镜头数 ≤ maxShotsPerAICall，防止超出 AI 模型输出 token 上限，
		// 同时降低单次 AI 调用逼近 provider 默认超时（300s）的概率。
		// P0 优化：20 → 14，单段输出 token 从约 14000 降到约 9800（≈30%），单段更快完成、超时概率更低；
		// 段数会相应增加，但配合下方窗口化并发（P1）不会拖慢整体耗时。
		const maxShotsPerAICall = 14
		dynSegRunes := maxSegmentRunes
		if totalShots > maxShotsPerAICall && totalRunes > 0 {
			// 使每段镜头数 ≤ maxShotsPerAICall：segRunes = totalRunes * maxShotsPerAICall / totalShots
			dynSegRunes = totalRunes * maxShotsPerAICall / totalShots
			if dynSegRunes < 500 {
				dynSegRunes = 500 // 最小 500 字保证 AI 上下文充足
			}
		}
		segments = splitContentSegments(content, dynSegRunes)
		plans = make([]segPlan, len(segments))

		shotsAllocated := 0
		runesProcessed := 0
		for segIdx, seg := range segments {
			segRunes := len([]rune(seg))
			runesProcessed += segRunes
			// 累积分配：用"到目前为止应分配的总镜头数 - 已分配数"计算本段，
			// 最后一段直接取剩余全部，保证各段加总恰好等于 totalShots。
			var segShotCount int
			if segIdx == len(segments)-1 {
				segShotCount = totalShots - shotsAllocated
			} else {
				cumTarget := totalShots * runesProcessed / max(totalRunes, 1)
				segShotCount = cumTarget - shotsAllocated
			}
			if segShotCount < 3 {
				segShotCount = 3
			}
			shotsAllocated += segShotCount

			// 计算本段在全章的百分比区间，用于在弧线骨架中定位当前段落的情感阶段
			segStartPct := (runesProcessed - segRunes) * 100 / max(totalRunes, 1)
			segEndPct := runesProcessed * 100 / max(totalRunes, 1)

			// P1a: 按内容百分比比例切出本段对应的情节节拍子集
			var segBeatSheet []map[string]interface{}
			if len(beatSheetItems) > 0 {
				startIdx := segStartPct * len(beatSheetItems) / 100
				endIdx := (segEndPct*len(beatSheetItems) + 99) / 100 // ceil，确保末段不遗漏
				if endIdx > len(beatSheetItems) {
					endIdx = len(beatSheetItems)
				}
				if startIdx >= endIdx && endIdx > 0 {
					startIdx = endIdx - 1
				}
				for _, item := range beatSheetItems[startIdx:endIdx] {
					segBeatSheet = append(segBeatSheet, map[string]interface{}{
						"No":             item.No,
						"BeatType":       item.BeatType,
						"Importance":     item.Importance,
						"ContentSummary": item.ContentSummary,
						"Location":       item.Location,
						"Characters":     strings.Join(item.Characters, "、"),
						"SuggestedShots": item.SuggestedShots,
					})
				}
			}
			plans[segIdx] = segPlan{segShotCount: segShotCount, segStartPct: segStartPct, segEndPct: segEndPct, segBeatSheet: segBeatSheet}
		}
	}

	chIDStr := "nil"
	if chapterID != nil {
		chIDStr = fmt.Sprintf("%d", *chapterID)
	}
	logger.Printf("[Storyboard] start videoID=%d chapterID=%s provider=%q totalRunes=%d totalShots=%d segments=%d screenplayMode=%v chars=%d anchors=%d plotPoints=%d",
		videoID, chIDStr, provider, totalRunes, totalShots, len(segments), usedScreenplay, len(characters), len(anchors), len(plotPoints))

	// P1 优化：按窗口并发生成段落，而不是严格逐段串行等待。
	// 窗口内的段落共享同一份"上一窗口末尾镜头"快照作为上文（并发执行，无法互相等待对方产出），
	// 窗口之间仍严格传递真实的 prevTailShots，保留大部分跨段连贯性——
	// 这是"牺牲窗口内部分衔接精度换并发提速"的折中：比完全并发（零上下文）质量更好，
	// 比严格串行（零并发）速度更快。窗口越大提速越明显，但也更依赖 provider 并发配额。
	const storyboardConcurrentWindow = 3
	const prevTailN = 5 // 传递上一窗口末尾多少个镜头（更多上下文 → 跨段衔接更自然）
	type segResult struct {
		shots []*model.StoryboardShot
		err   error
	}
	results := make([]segResult, len(segments))

	genCtx := ctx
	// Each segment produces 8–15K chars of JSON. A 4096-token limit truncates it.
	// The AI API silently caps max_tokens at the model's own maximum when it exceeds it,
	// so requesting 16384 on a model that only supports 4096 is safe (no API error).
	segOverrides := overrides
	if segOverrides.MaxTokens < 16384 {
		segOverrides.MaxTokens = 16384
	}

	var prevTailShots []*model.StoryboardShot // 上一窗口末尾镜头，首窗口为 nil
	cancelled := false
	for winStart := 0; winStart < len(segments) && !cancelled; winStart += storyboardConcurrentWindow {
		select {
		case <-genCtx.Done():
			for i := winStart; i < len(segments); i++ {
				results[i] = segResult{err: genCtx.Err()}
			}
			cancelled = true
			continue
		default:
		}

		winEnd := winStart + storyboardConcurrentWindow
		if winEnd > len(segments) {
			winEnd = len(segments)
		}
		winPrevTail := prevTailShots // 窗口内所有段共享同一份上文快照
		logger.Printf("[Storyboard] window segs=[%d,%d) start prevTail=%d", winStart+1, winEnd, len(winPrevTail))

		var wg sync.WaitGroup
		for segIdx := winStart; segIdx < winEnd; segIdx++ {
			wg.Add(1)
			go func(segIdx int) {
				defer wg.Done()
				p := plans[segIdx]
				shots, err := s.generateStoryboardSegment(genCtx, video, segments[segIdx], segIdx, len(segments),
					p.segShotCount, characters, anchors, plotPoints, effectiveItems, winPrevTail,
					genre, arcPlan, imageStyle,
					chapterNo, novelTitle, worldviewDesc, p.segStartPct, p.segEndPct, charVisualPrompts,
					p.segBeatSheet, tenantID, provider, segOverrides, videoID, chapterID)
				results[segIdx] = segResult{shots: shots, err: err}
			}(segIdx)
		}
		wg.Wait()

		// 取窗口内最后一个成功段落的尾部镜头，作为下一窗口的 prevTail；
		// 若整窗口都失败，prevTail 置空（下一窗口退化为缺少上文，但不中止整体生成）。
		var winTail []*model.StoryboardShot
		for i := winEnd - 1; i >= winStart; i-- {
			if results[i].err == nil && len(results[i].shots) > 0 {
				shots := results[i].shots
				if len(shots) > prevTailN {
					winTail = shots[len(shots)-prevTailN:]
				} else {
					winTail = shots
				}
				break
			}
		}
		prevTailShots = winTail

		if progressFn != nil {
			progressFn(winEnd * 90 / len(segments))
		}
	}

	// 按原始顺序合并结果，统一重编号
	var allShots []*model.StoryboardShot
	shotCounter := 0
	failedSegs := 0
	var firstErr error
	for idx, r := range results {
		if r.err != nil {
			failedSegs++
			if idx == 0 {
				logger.Errorf("[Storyboard] seg 1/%d failed: %v", len(segments), r.err)
				firstErr = r.err
			} else {
				logger.Errorf("[Storyboard] seg %d/%d failed (non-fatal): %v", idx+1, len(segments), r.err)
			}
			continue
		}
		for _, shot := range r.shots {
			shotCounter++
			shot.ShotNo = shotCounter
			if sceneIDs != nil {
				sceneID := sceneIDs[idx]
				shot.ScreenplaySceneID = &sceneID
			}
		}
		allShots = append(allShots, r.shots...)
	}

	// 验证合并后的分镜序号连续性（1..N），以防合并逻辑出现 bug 导致序号跳空。
	// 重编号步骤已保证 1..N，此处仅为安全校验：若检测到序号跳空，尝试二次修复。
	if err := validateShotSequence(allShots); err != nil {
		logger.Errorf("[Storyboard] WARNING: shot sequence validation failed (%v); attempting re-sequence", err)
		for i, shot := range allShots {
			shot.ShotNo = i + 1
		}
		if err2 := validateShotSequence(allShots); err2 != nil {
			logger.Errorf("[Storyboard] ERROR: re-sequence also failed: %v", err2)
		}
	}

	if len(allShots) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("所有段落均未能生成分镜，请检查章节内容或重试")
	}
	shots := allShots
	if progressFn != nil {
		progressFn(92)
	}

	// 场景锚点自动匹配：按 shot.Location 名称匹配已注册的场景锚点
	if s.sceneAnchorSvc != nil {
		s.autoMatchShotAnchors(shots, anchors)
	}
	// 角色自动关联：按 shot.Characters JSON 中的名称匹配小说角色
	s.autoMatchShotCharacters(shots, characters)
	// 道具自动关联：按 shot.GenMeta.Items JSON 中的名称匹配小说道具
	s.autoMatchShotItems(shots, effectiveItems)
	if progressFn != nil {
		progressFn(95)
	}

	// 删除旧分镜前，把当前全部分镜整体序列化成一条历史快照。
	s.snapshotShotsBeforeOverwrite(videoID, "regenerate")

	// 删除旧分镜并批量插入新分镜（包裹在同一事务中，防止删除后插入失败导致数据丢失）
	if err := s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
		// 先收集旧 shot IDs，用于级联清理配音段
		var oldShotIDs []uint
		tx.Model(&model.StoryboardShot{}).Where("video_id = ?", videoID).Pluck("id", &oldShotIDs)
		if len(oldShotIDs) > 0 {
			// 物理删除孤立的配音段（soft delete 不够，外键引用已失效的行仍会留在表中）
			if err := tx.Unscoped().Where("shot_id IN ?", oldShotIDs).Delete(&model.ShotVoiceSegment{}).Error; err != nil {
				return err
			}
		}
		// 必须物理删除（Unscoped），软删除行仍触发 uk_video_shot 唯一键冲突
		if err := tx.Unscoped().Where("video_id = ?", videoID).Delete(&model.StoryboardShot{}).Error; err != nil {
			return err
		}
		if len(shots) == 0 {
			return nil
		}
		return tx.Create(shots).Error
	}); err != nil {
		return nil, fmt.Errorf("保存分镜失败: %w", err)
	}
	if progressFn != nil {
		progressFn(99)
	}

	// 更新视频状态；生成完成即视为已确认，无需手动确认步骤
	video.PublishMeta.TotalShots = len(shots)
	video.Status = "storyboard"
	video.TaskMeta.ScriptStatus = "confirmed"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, err)
	}

	logger.Infof("[Storyboard] finished videoID=%d totalShots=%d segments=%d failedSegs=%d elapsed=%s",
		videoID, len(shots), len(segments), failedSegs, time.Since(totalStart).Round(time.Millisecond))

	sbStatus := "success"
	if failedSegs > 0 {
		sbStatus = "partial"
	}
	metrics.StoryboardGenerationTotal.WithLabelValues(sbStatus).Inc()
	metrics.StoryboardGenerationDuration.Observe(time.Since(sbStart).Seconds())
	metrics.StoryboardShotsGenerated.Observe(float64(len(shots)))

	// 段落失败不再当作 error 返回——只要有任何分镜产出就是可用结果；FailedSegments 让调用方
	// （handler）决定报告 Complete 还是 CompletePartial，而不是在这里合成一个容易被忽略的 error。
	return &StoryboardGenResult{
		Shots:          shots,
		RequestedShots: totalShots,
		FailedSegments: failedSegs,
		TotalSegments:  len(segments),
	}, nil
}

// fetchSceneRegenContext 为单场次分镜重新生成拉取所需的上下文（角色/场景锚点/情节点/道具/
// 角色形象/小说与世界观信息/章节内容）。逻辑与 GenerateStoryboardCtx 中的预取块
// （见上文并行预取部分）保持一致的"章节级绑定优先，回退小说全量"策略，但顺序执行而非
// 并发——单场次重新生成不是高频路径，牺牲一点延迟换取不改动、不复用那段已调优的并发代码，
// 避免为了去重而给整视频生成引入回归风险。
func (s *VideoService) fetchSceneRegenContext(video *model.Video, chapterID uint) (
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	effectiveItems []*EffectiveItem, charVisualPrompts map[uint]string,
	genre, imageStyle, novelTitle, worldviewDesc string, chapterNo int, content string,
) {
	novelID := video.NovelID

	if chapterID != 0 && s.chapterCharacterRepo != nil {
		if bindings, e := s.chapterCharacterRepo.ListByChapter(chapterID); e == nil && len(bindings) > 0 {
			ids := make([]uint, 0, len(bindings))
			for _, b := range bindings {
				ids = append(ids, b.CharacterID)
			}
			if chars, e2 := s.characterRepo.ListByIDs(ids); e2 == nil {
				characters = chars
			}
		}
	}
	if len(characters) == 0 && novelID > 0 {
		characters, _ = s.characterRepo.ListByNovel(novelID)
	}

	if s.sceneAnchorSvc != nil && novelID > 0 {
		if chapterID != 0 {
			anchors, _ = s.sceneAnchorSvc.ListChapterAnchors(novelID, chapterID)
		}
		if len(anchors) == 0 {
			anchors, _ = s.sceneAnchorSvc.ListByNovel(novelID)
		}
	}

	if s.plotPointRepo != nil {
		if chapterID != 0 {
			plotPoints, _ = s.plotPointRepo.ListByChapter(chapterID)
		}
		if len(plotPoints) == 0 && novelID > 0 {
			plotPoints, _ = s.plotPointRepo.ListByNovel(novelID, "", true)
		}
	}

	if s.itemRepo != nil && novelID > 0 {
		if allItems, err := s.itemRepo.ListByNovel(novelID); err == nil && len(allItems) > 0 {
			overrideMap := make(map[uint]*model.ChapterItem)
			if chapterID != 0 && s.chapterItemRepo != nil {
				if overrides, e := s.chapterItemRepo.ListByChapter(chapterID); e == nil {
					for _, ci := range overrides {
						overrideMap[ci.ItemID] = ci
					}
				}
			}
			for _, item := range allItems {
				ei := &EffectiveItem{Item: *item, EffectiveLocation: item.Location, EffectiveOwner: item.Owner}
				if ov, ok := overrideMap[item.ID]; ok {
					ei.ChapterOverride = ov
					if ov.Location != "" {
						ei.EffectiveLocation = ov.Location
					}
					if ov.Owner != "" {
						ei.EffectiveOwner = ov.Owner
					}
				}
				effectiveItems = append(effectiveItems, ei)
			}
		}
	}

	charVisualPrompts = make(map[uint]string)
	if s.lookupService != nil && len(characters) > 0 {
		lookIDs := make([]uint, 0, len(characters))
		charToLook := make(map[uint]uint)
		for _, c := range characters {
			if c.DefaultLookID != 0 {
				lookIDs = append(lookIDs, c.DefaultLookID)
				charToLook[c.ID] = c.DefaultLookID
			}
		}
		if len(lookIDs) > 0 {
			if looksMap, err := s.lookupService.BatchGetLooksByIDs(lookIDs); err == nil {
				for charID, lookID := range charToLook {
					if look, ok := looksMap[lookID]; ok && look != nil && look.VisualPrompt != "" {
						charVisualPrompts[charID] = look.VisualPrompt
					}
				}
			}
		}
	}

	if s.novelRepo != nil && novelID > 0 {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			genre = novel.Meta.Genre
			imageStyle = novel.AIConfig.ImageStyle
			novelTitle = novel.Title
			if s.worldviewRepo != nil && novel.WorldviewID != nil {
				if wv, wvErr := s.worldviewRepo.GetByID(*novel.WorldviewID); wvErr == nil && wv != nil {
					desc := wv.Description
					if runes := []rune(desc); len(runes) > 300 {
						desc = string(runes[:300]) + "…"
					}
					worldviewDesc = desc
				}
			}
		}
	}
	if imageStyle == "" {
		imageStyle = video.RenderConfig.ArtStyle
	}

	if s.chapterRepo != nil && chapterID != 0 {
		if chapter, chErr := s.chapterRepo.GetByID(chapterID); chErr == nil && chapter != nil {
			content = chapter.Content
			chapterNo = chapter.ChapterNo
		}
	}

	return
}

// findSceneInsertionPoint 为一个当前尚无任何分镜的场次，找出其分镜应插入的位置——取该章节内
// 场次序号（SceneNo）小于本场的所有场次里，最大的分镜序号（shot_no）；若都没有（本场是章节
// 第一场，或此前从未生成过分镜），返回 0（插入到最前）。
func (s *VideoService) findSceneInsertionPoint(allShots []*model.StoryboardShot, scene *model.ScreenplayScene) int {
	siblings, err := s.screenplaySvc.ListScenes(scene.ChapterID)
	if err != nil {
		return 0
	}
	precedingSceneIDs := make(map[uint]bool)
	for _, sib := range siblings {
		if sib.SceneNo < scene.SceneNo {
			precedingSceneIDs[sib.ID] = true
		}
	}
	maxNo := 0
	for _, sh := range allShots {
		if sh.ScreenplaySceneID != nil && precedingSceneIDs[*sh.ScreenplaySceneID] && sh.ShotNo > maxNo {
			maxNo = sh.ShotNo
		}
	}
	return maxNo
}

// GetVideoByChapterID 按章节 id 查找其关联的（唯一的）视频项目；不存在时自动创建一个
// （渲染参数继承自项目级别的视频配置，见 CreateVideoFromChapter），避免用户在生成分镜前
// 必须先手动"创建视频项目"这一步。
func (s *VideoService) GetVideoByChapterID(tenantID, chapterID uint) (*model.Video, error) {
	videos, _, err := s.videoRepo.List(nil, &chapterID, "", tenantID, 1, 1)
	if err != nil {
		return nil, err
	}
	if len(videos) > 0 {
		return videos[0], nil
	}
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter %d not found: %w", chapterID, err)
	}
	video, err := s.CreateVideoFromChapter(chapter.NovelID, &chapterID)
	if err != nil {
		return nil, fmt.Errorf("auto-create video project for chapter %d: %w", chapterID, err)
	}
	logger.Printf("[VideoService] GetVideoByChapterID: auto-created video project id=%d for chapterID=%d (no video project existed yet)", video.ID, chapterID)
	return video, nil
}

// FindVideoByChapterID 按章节 id 查找其关联的视频项目；与 GetVideoByChapterID 不同，不存在时
// 返回 (nil, nil) 而不是自动创建——供导出等只读场景使用，避免产生"点一下导出就多建了一个空视频
// 项目"的副作用。
func (s *VideoService) FindVideoByChapterID(tenantID, chapterID uint) (*model.Video, error) {
	videos, _, err := s.videoRepo.List(nil, &chapterID, "", tenantID, 1, 1)
	if err != nil {
		return nil, err
	}
	if len(videos) > 0 {
		return videos[0], nil
	}
	return nil, nil
}

// GetVideoIDForScreenplayScene 供 handler 在创建异步任务前解析场次归属的视频 id
// （任务记录需要一个明确的 entity_id，才能像整视频分镜生成一样支持 /videos/:id/progress 轮询）。
func (s *VideoService) GetVideoIDForScreenplayScene(tenantID, sceneID uint) (uint, error) {
	if s.screenplaySvc == nil {
		return 0, fmt.Errorf("screenplay service not configured")
	}
	scene, err := s.screenplaySvc.GetScene(sceneID)
	if err != nil {
		return 0, fmt.Errorf("screenplay scene not found: %w", err)
	}
	video, err := s.GetVideoByChapterID(tenantID, scene.ChapterID)
	if err != nil {
		return 0, err
	}
	return video.ID, nil
}

// RegenerateShotsForScene 只重新生成单个分场剧本对应的分镜，不影响该视频其它场次的分镜内容，
// 但会在必要时整体位移后续场次的 shot_no（保持全视频 1..N 连续编号，其它功能——审查/优化/
// 插入删除分镜——均假设这一点成立）。
//
// 与整视频重新生成（GenerateStoryboardCtx）共用同一把 Redis 锁（video 级别，而非 scene 级别），
// 防止"整视频重新生成"与"单场次重新生成"并发写同一批分镜行导致数据损坏。
//
// 重新生成后会作废该视频当前所有待处理（pending）的分镜审查建议——它们按旧 shot_no 记录，
// 位移之后会指向错误的分镜（见 fetchSceneRegenContext 之上的说明）。
func (s *VideoService) RegenerateShotsForScene(ctx context.Context, tenantID, sceneID uint, provider string, progressFn func(int)) (*StoryboardGenResult, error) {
	if s.screenplaySvc == nil {
		return nil, fmt.Errorf("screenplay service not configured")
	}
	scene, err := s.screenplaySvc.GetScene(sceneID)
	if err != nil {
		return nil, fmt.Errorf("screenplay scene not found: %w", err)
	}
	chapterID := scene.ChapterID

	video, err := s.GetVideoByChapterID(tenantID, chapterID)
	if err != nil {
		return nil, err
	}
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		return nil, err
	}

	if s.cache != nil {
		redisKey := lockKey("storyboard", "gen", video.ID)
		ok, lockErr := s.cache.SetNX(context.Background(), redisKey, "1", 30*time.Minute).Result()
		if lockErr == nil {
			if !ok {
				return nil, fmt.Errorf("该视频当前正在生成分镜，请稍候再试")
			}
			defer s.cache.Del(context.Background(), redisKey)
		}
	}

	resolvedTenantID := s.videoTenantID(video)
	characters, anchors, plotPoints, effectiveItems, charVisualPrompts,
		genre, imageStyle, novelTitle, worldviewDesc, chapterNo, content :=
		s.fetchSceneRegenContext(video, chapterID)
	arcPlan := s.generateStoryboardArc(content, characters, resolvedTenantID, video.NovelID, provider, video.Mode)
	if progressFn != nil {
		progressFn(20)
	}

	allShots, err := s.storyboardRepo.ListByVideo(video.ID)
	if err != nil {
		return nil, err
	}
	var sceneShots []*model.StoryboardShot
	for _, sh := range allShots {
		if sh.ScreenplaySceneID != nil && *sh.ScreenplaySceneID == sceneID {
			sceneShots = append(sceneShots, sh)
		}
	}
	oldCount := len(sceneShots)
	firstShotNo := 0
	if oldCount > 0 {
		firstShotNo = sceneShots[0].ShotNo - 1
	} else {
		firstShotNo = s.findSceneInsertionPoint(allShots, scene)
	}

	const prevTailN = 5
	var prevTailShots []*model.StoryboardShot
	for _, sh := range allShots {
		if sh.ShotNo <= firstShotNo {
			prevTailShots = append(prevTailShots, sh)
		}
	}
	if len(prevTailShots) > prevTailN {
		prevTailShots = prevTailShots[len(prevTailShots)-prevTailN:]
	}

	// 单场次的 segment 文本 = 【场次 N】heading + synopsis + 逐条节拍，与
	// GenerateStoryboardCtx 里"方案B：逐场生成"分支（usedScreenplay）构造 segment 的方式一致。
	var segBuilder strings.Builder
	fmt.Fprintf(&segBuilder, "【场次 %d】%s\n%s\n", scene.SceneNo, scene.Heading, scene.Synopsis)
	var segBeatSheet []map[string]interface{}
	for j, line := range splitBeatLines(scene.Beats) {
		beatType, speaker, text := parseBeatLine(line)
		if beatType == "dialogue" {
			fmt.Fprintf(&segBuilder, "%d. （对话）%s：%s\n", j+1, speaker, text)
		} else {
			fmt.Fprintf(&segBuilder, "%d. （%s）%s\n", j+1, beatType, text)
		}
		segBeatSheet = append(segBeatSheet, map[string]interface{}{
			"No": j + 1, "BeatType": beatType, "Importance": "major", "ContentSummary": text,
			"Location": scene.Heading, "Characters": speaker, "SuggestedShots": 0,
		})
	}
	segShotCount := scene.EstimatedShotCount
	if segShotCount < 1 {
		segShotCount = 3
	}

	segOverrides := StoryboardOverrides{MaxTokens: 16384}

	newShots, err := s.generateStoryboardSegment(ctx, video, segBuilder.String(), 0, 1, segShotCount,
		characters, anchors, plotPoints, effectiveItems, prevTailShots,
		genre, arcPlan, imageStyle,
		chapterNo, novelTitle, worldviewDesc, 0, 100, charVisualPrompts,
		segBeatSheet, resolvedTenantID, provider, segOverrides, video.ID, &chapterID)
	if err != nil {
		return nil, err
	}
	if len(newShots) == 0 {
		return nil, fmt.Errorf("未能生成该场次的分镜，请重试")
	}
	if progressFn != nil {
		progressFn(70)
	}

	if s.sceneAnchorSvc != nil {
		s.autoMatchShotAnchors(newShots, anchors)
	}
	s.autoMatchShotCharacters(newShots, characters)
	s.autoMatchShotItems(newShots, effectiveItems)
	for _, sh := range newShots {
		sh.ScreenplaySceneID = &sceneID
		sh.ChapterID = &chapterID
	}
	if progressFn != nil {
		progressFn(85)
	}

	newCount := len(newShots)
	delta := newCount - oldCount

	err = s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
		if oldCount > 0 {
			oldIDs := make([]uint, 0, oldCount)
			for _, sh := range sceneShots {
				oldIDs = append(oldIDs, sh.ID)
			}
			if err := tx.Unscoped().Where("shot_id IN ?", oldIDs).Delete(&model.ShotVoiceSegment{}).Error; err != nil {
				return err
			}
			if err := tx.Unscoped().Where("id IN ?", oldIDs).Delete(&model.StoryboardShot{}).Error; err != nil {
				return err
			}
		}
		if delta != 0 {
			// 两阶段位移（与 InsertShot 相同的技巧）：先加大偏移量避开唯一键冲突区间，再减回目标值。
			// 此时本场旧分镜行已删除，"shot_no > firstShotNo" 与"shot_no > firstShotNo+oldCount"
			// 在这个事务内等价（(firstShotNo, firstShotNo+oldCount] 区间内已经没有任何行）。
			if err := tx.Exec(
				"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
				shiftTempOffset, video.ID, firstShotNo,
			).Error; err != nil {
				return err
			}
			if err := tx.Exec(
				"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? - ? WHERE video_id = ? AND shot_no > ? AND deleted_at IS NULL",
				delta, shiftTempOffset, video.ID, firstShotNo+shiftTempOffset,
			).Error; err != nil {
				return err
			}
		}
		for i, sh := range newShots {
			sh.ShotNo = firstShotNo + i + 1
		}
		return tx.Create(newShots).Error
	})
	if err != nil {
		return nil, fmt.Errorf("保存分镜失败: %w", err)
	}

	if s.reviewRecordRepo != nil {
		if err := s.reviewRecordRepo.DeletePendingByEntity(model.ReviewEntityStoryboard, video.ID); err != nil {
			logger.Errorf("[Storyboard] failed to invalidate pending reviews videoID=%d: %v", video.ID, err)
		}
	}

	video.PublishMeta.TotalShots += delta
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[VideoService] failed to update video %d after scene regen: %v", video.ID, err)
	}
	if progressFn != nil {
		progressFn(99)
	}

	logger.Infof("[Storyboard] scene regen finished videoID=%d sceneID=%d oldCount=%d newCount=%d",
		video.ID, sceneID, oldCount, newCount)

	return &StoryboardGenResult{Shots: newShots, RequestedShots: segShotCount, FailedSegments: 0, TotalSegments: 1}, nil
}

// generateStoryboardSegment 为单个内容分段生成分镜，含最多 3 次重试。
// P0 优化：重试因超时触发时（ai.IsTimeoutError）会同步下调期望镜头数，用更小的输出体量
// 换取在 provider 超时窗口内完成的概率；因空响应/解析失败/镜头数不足触发的重试维持原目标，
// 只追加提示词。可在多个 goroutine 中并发调用——只读取入参，不修改任何共享状态。
func (s *VideoService) generateStoryboardSegment(
	ctx context.Context, video *model.Video, seg string,
	segIdx, totalSegments, segShotCount int,
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	items []*EffectiveItem, prevTailShots []*model.StoryboardShot,
	genre, arcPlan, imageStyle string,
	chapterNo int, novelTitle, worldviewDesc string,
	segStartPct, segEndPct int, charVisualPrompts map[uint]string,
	segBeatSheet []map[string]interface{},
	tenantID uint, provider string, segOverrides StoryboardOverrides,
	videoID uint, chapterID *uint,
) ([]*model.StoryboardShot, error) {
	segStart := time.Now()
	logger.Printf("[Storyboard] seg %d/%d start runes=%d expectedShots=%d prevTail=%d",
		segIdx+1, totalSegments, len([]rune(seg)), segShotCount, len(prevTailShots))

	// P1b: 从上一窗口末尾镜头提取跨段世界状态快照（场景/时间/天气/在场角色）
	var segWorldState map[string]interface{}
	if len(prevTailShots) > 0 {
		segWorldState = extractWorldStateFromShots(prevTailShots)
	}

	var aiResult string
	var aiErr error
	var shots []*model.StoryboardShot
	var bestShots []*model.StoryboardShot // 历次尝试中镜头数最多的结果
	attemptShotCount := segShotCount
	for attempt := 0; attempt < 3; attempt++ {
		p := s.buildStoryboardPrompt(video, seg, segIdx+1, totalSegments, attemptShotCount,
			characters, anchors, plotPoints, items, prevTailShots, genre, arcPlan, imageStyle,
			chapterNo, novelTitle, worldviewDesc, segStartPct, segEndPct, charVisualPrompts,
			segBeatSheet, segWorldState)
		switch attempt {
		case 1:
			p += "\n\n⚠️ 重要提示：请只返回纯 JSON 数组，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (format hint) shotTarget=%d", segIdx+1, totalSegments, attempt, attemptShotCount)
		case 2:
			p += fmt.Sprintf("\n\n⚠️ 极重要：上一次你只返回了很少的分镜，请务必生成全部%d个分镜，只返回JSON数组不要截断。", attemptShotCount)
			logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (shot count hint) shotTarget=%d", segIdx+1, totalSegments, attempt, attemptShotCount)
		}
		aiStart := time.Now()
		aiResult, aiErr = s.aiService.GenerateWithProviderCtx(ctx, tenantID, "storyboard", p)
		aiElapsed := time.Since(aiStart)
		metrics.StoryboardSegmentDuration.Observe(aiElapsed.Seconds())
		if aiErr != nil {
			logger.Errorf("[Storyboard] seg %d/%d attempt=%d AI error elapsed=%s err=%v", segIdx+1, totalSegments, attempt, aiElapsed.Round(time.Millisecond), aiErr)
			if ai.IsTimeoutError(aiErr) {
				metrics.StoryboardSegmentTimeoutTotal.Inc()
				// P0：超时后不原样重试（大概率再次超时），而是把目标镜头数下调约 1/3 再重试，
				// 缩小单次输出体量以提高在超时窗口内完成的概率。
				if attempt < 2 && attemptShotCount > 6 {
					newTarget := attemptShotCount * 2 / 3
					if newTarget < 6 {
						newTarget = 6
					}
					logger.Printf("[Storyboard] seg %d/%d attempt=%d timed out, shrinking shot target %d -> %d and retrying",
						segIdx+1, totalSegments, attempt, attemptShotCount, newTarget)
					attemptShotCount = newTarget
					continue
				}
				break
			}
			// P3 优化：非超时错误说明 GenerateWithProviderCtx 内部（RetryProvider）已经对
			// 429/502/503/504/连接类瞬时错误做过最多3次指数退避重试仍未成功——要么是持续性
			// 故障，要么是重试也无法恢复的错误（内容策略拒绝/参数错误/额度耗尽等）。在外层原样
			// 再走2轮、每轮又各自触发一次完整的内部重试链，大概率只是重复相同的失败和退避等待，
			// 不再于外层继续重试，直接跳出（最坏情况延迟从 3(外层)×3(内层)=9 次调用降到最多3次）。
			break
		}
		logger.Printf("[Storyboard] seg %d/%d attempt=%d AI ok elapsed=%s responseLen=%d", segIdx+1, totalSegments, attempt, aiElapsed.Round(time.Millisecond), len(aiResult))
		if strings.TrimSpace(aiResult) == "" {
			continue
		}
		parsed, parseErr := s.parseStoryboardResult(videoID, chapterID, aiResult)
		if parseErr != nil {
			logger.Errorf("[Storyboard] seg %d/%d attempt=%d parse failed: %v", segIdx+1, totalSegments, attempt, parseErr)
			continue
		}
		// 始终保留历次中镜头数最多的结果
		if len(parsed) > len(bestShots) {
			bestShots = parsed
		}
		// P3 优化：验收阈值从75%放宽到60%——75%过于严格，模型基于内容自然压缩产出的镜头数
		// 略低于目标时也会触发整轮重试（一次完整的 LLM 往返），放宽后只在明显欠产出（<60%）时
		// 才重试，减少非必要的重试次数；bestShots 兜底逻辑不变，仍会保留历次最多镜头数的结果。
		if len(parsed) < (attemptShotCount*3)/5 && attempt < 2 {
			logger.Printf("[Storyboard] seg %d/%d attempt=%d too few shots got=%d expected=%d (threshold 60%%), retrying",
				segIdx+1, totalSegments, attempt, len(parsed), attemptShotCount)
			continue
		}
		shots = bestShots
		break
	}
	if shots == nil && len(bestShots) > 0 {
		shots = bestShots // 全部 attempt 均未达标时，仍用最佳部分结果
	}
	if aiErr != nil && shots == nil {
		return nil, aiErr
	}
	if shots == nil {
		logger.Printf("[Storyboard] seg %d/%d fatal: AI returned empty or unparseable response after all retries", segIdx+1, totalSegments)
		return nil, fmt.Errorf("AI返回空响应，请检查模型配置或更换提供商")
	}
	logger.Printf("[Storyboard] seg %d/%d done shots=%d elapsed=%s", segIdx+1, totalSegments, len(shots), time.Since(segStart).Round(time.Millisecond))
	return shots, nil
}

// autoMatchShotAnchors 按场景名称自动将分镜绑定到场景锚点（模糊匹配 scene.location）
// 这样无需前端手动调用 SetShotAnchor，锚点注入即可在视频生成时自动生效。
// anchors 为调用方预取的数据（避免重复查 DB）。
func (s *VideoService) autoMatchShotAnchors(shots []*model.StoryboardShot, anchors []*model.SceneAnchor) {
	if len(anchors) == 0 {
		logger.Printf("[AutoMatch] scene: no anchors in DB, skipping")
		return
	}
	// 构建名称→ID映射（小写，方便模糊匹配）
	anchorMap := make(map[string]uint, len(anchors))
	anchorNames := make([]string, 0, len(anchors))
	for _, a := range anchors {
		anchorMap[strings.ToLower(a.Name)] = a.ID
		anchorNames = append(anchorNames, a.Name)
	}
	logger.Printf("[AutoMatch] scene: %d anchors available: %v", len(anchors), anchorNames)
	matchCount := 0
	for _, shot := range shots {
		if shot.SceneAnchorID != nil {
			continue // 已手动绑定，不覆盖
		}
		// shot.GenMeta.Scene 是 JSON: {"location":"...","time_of_day":"..."}
		loc := extractLocationFromScene(shot.GenMeta.Scene)
		if loc == "" {
			// 降级：从 Description 中做关键词匹配
			loc = shot.Description
		}
		loc = strings.ToLower(loc)

		// tryAnchor 从给定文本中查找场景锚点，找到则设置并返回 true。
		tryAnchor := func(text string) bool {
			text = strings.ToLower(text)
			if text == "" {
				return false
			}
			if id, ok := anchorMap[text]; ok {
				id := id
				shot.SceneAnchorID = &id
				return true
			}
			for name, id := range anchorMap {
				if strings.Contains(text, name) {
					id := id
					shot.SceneAnchorID = &id
					return true
				}
			}
			return false
		}

		// ① location 精确/包含匹配
		if loc != "" && tryAnchor(loc) {
			matchCount++
			logger.Printf("[AutoMatch] scene: shot#%d location=%q → anchorID=%d", shot.ShotNo, loc, *shot.SceneAnchorID)
			continue
		}
		// ② narration 关键词扫描
		if tryAnchor(shot.Narration()) {
			matchCount++
			logger.Printf("[AutoMatch] scene: shot#%d narration match → anchorID=%d", shot.ShotNo, *shot.SceneAnchorID)
			continue
		}
		// ③ description 关键词扫描（英文环境描述，最后兜底）
		if tryAnchor(shot.Description) {
			matchCount++
			logger.Printf("[AutoMatch] scene: shot#%d description match → anchorID=%d", shot.ShotNo, *shot.SceneAnchorID)
		} else {
			logger.Printf("[AutoMatch] scene: shot#%d no match (location=%q)", shot.ShotNo, loc)
		}
	}
	logger.Printf("[AutoMatch] scene: matched %d/%d shots", matchCount, len(shots))
}

// autoMatchShotCharacters 按 shot.GenMeta.Dialogue "角色名：台词" 匹配小说角色，写入 CharacterIDs。
// 已有 CharacterIDs 时不覆盖（保留手动绑定结果）。
func (s *VideoService) autoMatchShotCharacters(shots []*model.StoryboardShot, chars []*model.Character) {
	if len(chars) == 0 {
		logger.Printf("[AutoMatch] char: no characters in DB, skipping")
		return
	}
	// 构建 小写名→ID map
	nameMap := make(map[string]uint, len(chars))
	charNames := make([]string, 0, len(chars))
	for _, c := range chars {
		nameMap[strings.ToLower(c.Name)] = c.ID
		charNames = append(charNames, c.Name)
	}
	logger.Printf("[AutoMatch] char: %d characters available: %v", len(chars), charNames)

	// tryMatch 尝试将一个原始名称加入 matched；跳过已知占位符。
	tryMatch := func(rawName string, seen map[uint]bool, matched *model.JSONUintSlice) {
		nameLower := strings.ToLower(strings.TrimSpace(rawName))
		if nameLower == "" || nameLower == "角色名" || nameLower == "character" {
			return
		}
		if id, ok := nameMap[nameLower]; ok && !seen[id] {
			*matched = append(*matched, id)
			seen[id] = true
			return
		}
		for name, id := range nameMap {
			if strings.Contains(nameLower, name) || strings.Contains(name, nameLower) ||
				charRuneOverlap(nameLower, name) >= charRuneOverlapThreshold {
				if !seen[id] {
					*matched = append(*matched, id)
					seen[id] = true
				}
				return
			}
		}
	}

	charMatchCount := 0
	for _, shot := range shots {
		if len(shot.CharacterIDs) > 0 {
			continue // 已手动绑定，不覆盖
		}
		var matched model.JSONUintSlice
		seen := make(map[uint]bool)

		// 台词行的 character 字段即精确说话角色名，无需再从文本中解析
		for _, l := range shot.VoiceLines() {
			if l.Character != "" && len([]rune(l.Character)) <= 10 { // 合理的角色名长度
				tryMatch(l.Character, seen, &matched)
			}
		}

		// Narration 和 Description 扫描不用于匹配：
		// 旁白/描述中提到角色名（"三年前他来过这里"）≠ 该角色在画面中出现。
		// 用旁白扫描匹配会导致角色参考图被错误地注入到空景/环境镜头，产生"幽灵角色"。
		// CharacterIDs 只应来源于对白说话角色。

		if len(matched) > 0 {
			shot.CharacterIDs = matched
			charMatchCount++
			logger.Printf("[AutoMatch] char: shot#%d → charIDs=%v", shot.ShotNo, []uint(matched))
		} else {
			logger.Printf("[AutoMatch] char: shot#%d no match", shot.ShotNo)
		}
	}
	logger.Printf("[AutoMatch] char: matched %d/%d shots", charMatchCount, len(shots))
}

// autoMatchShotItems 按 shot.Description/Narration 关键词扫描匹配小说道具，写入 ItemIDs。
// 已有 ItemIDs 时不覆盖（保留手动绑定结果）。
func (s *VideoService) autoMatchShotItems(shots []*model.StoryboardShot, items []*EffectiveItem) {
	if len(items) == 0 {
		return
	}
	nameMap := make(map[string]uint, len(items))
	for _, ei := range items {
		nameMap[strings.ToLower(ei.Name)] = ei.ID
	}

	matchCount := 0
	for _, shot := range shots {
		if len(shot.ItemIDs) > 0 {
			continue // 已手动绑定，不覆盖
		}
		var matched model.JSONUintSlice
		seen := make(map[uint]bool)

		text := strings.ToLower(shot.Description + " " + shot.Narration())
		for name, id := range nameMap {
			if strings.Contains(text, name) && !seen[id] {
				matched = append(matched, id)
				seen[id] = true
			}
		}

		if len(matched) > 0 {
			shot.ItemIDs = matched
			matchCount++
		}
	}
	logger.Printf("[AutoMatch] items: matched %d/%d shots", matchCount, len(shots))
}

// charRuneOverlap 返回两个字符串的汉字级重叠比例（以较短串为分母）。
// 用于模糊角色名匹配，如"萧炎"vs"炎少"（"炎"重叠 → 0.5，超过阈值即视为同一角色）。
// 优化：对于 ≤8 个字符的短串（汉字人名典型情况），线性扫描比 map 分配更快。
func charRuneOverlap(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	shorter, longer := ra, rb
	if len(ra) > len(rb) {
		shorter, longer = rb, ra
	}
	overlap := 0
	if len(longer) > 8 {
		// 长串：map 查找 O(n)，避免 O(n²) 线性扫描
		longerSet := make(map[rune]struct{}, len(longer))
		for _, r := range longer {
			longerSet[r] = struct{}{}
		}
		for _, r := range shorter {
			if _, ok := longerSet[r]; ok {
				overlap++
			}
		}
	} else {
		// 短串（≤8 字符，汉字人名典型）：线性扫描避免 map 分配开销
		for _, r := range shorter {
			for _, s := range longer {
				if r == s {
					overlap++
					break
				}
			}
		}
	}
	return float64(overlap) / float64(len(shorter))
}

// calcTotalShots 按字数密度估算全章期望分镜总数（自动模式，唯一支持的模式）。
func calcTotalShots(totalRunes int, videoMode, storyboardMode string) int {
	s := 15 // 视频动画模式：AI 生成运镜视频，一个分镜约 15 秒
	if videoMode == "slideshow" {
		s = 5 // 图片解说模式：静态图 + Ken Burns，一个分镜约 5 秒
	}

	// 先估算视频时长，再折算分镜数。
	// 汉字阅读速度约 300 字/分钟，视频精炼比约 0.5（10 分钟文章 → 5 分钟视频）。
	// 视频时长（秒）= totalRunes / 300 * 60 * 0.5 = totalRunes / 10
	estimatedSecs := totalRunes / 10
	n := estimatedSecs / s
	if storyboardMode == "concise" {
		// 简洁模式：只保留 major 节拍生成分镜，目标镜头数下调至约一半，避免下游为凑数而拆细次要情节
		n = n * 5 / 10
	}
	// 整体分镜数下调至约一半，避免剧情过于拖沓
	n = n / 2
	if n < 5 {
		n = 5
	}
	if n > 200 {
		n = 200
	}
	return n
}

// splitContentSegments 按段落边界切割长文本，每段最多 maxRunes 个字符。
// 若内容不超过 maxRunes，直接返回单段切片。
// 切割优先在双换行（段落）处断开，其次在单换行处断开，保证分镜上下文完整。
func splitContentSegments(content string, maxRunes int) []string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return []string{content}
	}
	var segments []string
	start := 0
	for start < len(runes) {
		end := start + maxRunes
		if end >= len(runes) {
			segments = append(segments, string(runes[start:]))
			break
		}
		// 在最后 20% 区间内找段落边界：优先双换行，次选单换行
		boundary := -1
		searchFrom := end - maxRunes/5
		for i := end; i >= searchFrom; i-- {
			if runes[i] == '\n' {
				if i > 0 && runes[i-1] == '\n' {
					boundary = i + 1 // 双换行后断开
					break
				}
				if boundary < 0 {
					boundary = i + 1 // 先记录单换行，继续找双换行
				}
			}
		}
		if boundary < 0 {
			boundary = end
		}
		segments = append(segments, string(runes[start:boundary]))
		start = boundary
	}
	return segments
}

// extractLocationFromScene 从分镜 scene JSON 中提取 location 字段
func extractLocationFromScene(sceneJSON string) string {
	if sceneJSON == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(sceneJSON), &m); err != nil {
		return ""
	}
	return m["location"]
}

// extractSceneField 从分镜 scene JSON 中提取任意字段（time_of_day / weather / lighting 等）
func extractSceneField(sceneJSON, field string) string {
	if sceneJSON == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(sceneJSON), &m); err != nil {
		return ""
	}
	return m[field]
}

// extractWorldStateFromShots 从末尾分镜中提取跨段世界状态快照（P1b）。
// 返回的 map 包含 Location，供下一段 prompt 注入，保证地点的跨段一致性。
func extractWorldStateFromShots(shots []*model.StoryboardShot) map[string]interface{} {
	if len(shots) == 0 {
		return nil
	}
	last := shots[len(shots)-1]
	location := extractSceneField(last.GenMeta.Scene, "location")
	if location == "" {
		return nil
	}
	return map[string]interface{}{
		"Location": location,
	}
}

// generateBeatSheet 从章节内容提取情节节拍表（P1a）。
// 提取一次，后续按百分比区间切片注入各 segment。失败时返回 nil（不阻断主流程）。
func (s *VideoService) generateBeatSheet(
	content string,
	characters []*model.Character,
	anchors []*model.SceneAnchor,
	tenantID, novelID uint,
	provider string,
	expectedShots int,
	videoMode string,
) []beatSheetItem {
	if s.aiService == nil {
		return nil
	}
	// 节拍数范围：专业/忠于原文模式下游按"一拍一镜、禁止合并"生成分镜，节拍数即约等于最终镜头数，
	// 故此处目标须直接贴近 expectedShots（±20%），而非放任节拍数膨胀到 2 倍再指望下游压缩。
	minBeats := expectedShots * 8 / 10
	maxBeats := expectedShots * 12 / 10
	if minBeats < 5 {
		minBeats = 5
	}
	if maxBeats < minBeats {
		maxBeats = minBeats
	}

	// 角色摘要（仅名称+身份，节省 token）
	type beatChar struct {
		Name string
		Role string
	}
	var beatChars []beatChar
	for _, c := range characters {
		beatChars = append(beatChars, beatChar{Name: c.Name, Role: c.Role})
		if len(beatChars) >= 10 {
			break
		}
	}
	// 场景锚点摘要
	var beatAnchors []map[string]interface{}
	for _, a := range anchors {
		beatAnchors = append(beatAnchors, map[string]interface{}{"Name": a.Name})
		if len(beatAnchors) >= 8 {
			break
		}
	}

	ctx := map[string]interface{}{
		"Content":    content,
		"Characters": beatChars,
		"Anchors":    beatAnchors,
		"MinBeats":   minBeats,
		"MaxBeats":   maxBeats,
		"VideoMode":  videoMode,
	}
	prompt, err := renderPrompt("storyboard_beat", ctx)
	if err != nil {
		logger.Errorf("[Storyboard] generateBeatSheet renderPrompt: %v", err)
		return nil
	}
	result, err := s.aiService.GenerateWithProvider(tenantID, "storyboard_beat", prompt)
	if err != nil {
		logger.Errorf("[Storyboard] generateBeatSheet AI call failed: %v", err)
		return nil
	}
	// 提取 JSON 对象（偶尔带 markdown 代码块）
	result = strings.TrimSpace(result)
	if idx := strings.Index(result, "{"); idx > 0 {
		result = result[idx:]
	}
	if idx := strings.LastIndex(result, "}"); idx >= 0 && idx < len(result)-1 {
		result = result[:idx+1]
	}
	var parsed struct {
		TotalBeats int             `json:"total_beats"`
		Beats      []beatSheetItem `json:"beats"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		logger.Errorf("[Storyboard] generateBeatSheet JSON parse failed: %v (raw=%q)", err, result)
		return nil
	}
	logger.Printf("[Storyboard] beatSheet parsed: %d beats (expected total_beats=%d)", len(parsed.Beats), parsed.TotalBeats)
	return parsed.Beats
}

// buildStoryboardPrompt 构建分镜提示词（含截断保护、角色信息、段落上下文）
// segNo/totalSegs 为分段编号（从 1 开始），单段调用时传 1, 1。
// expectedShots 为本段期望分镜数，由调用方通过 calcTotalShots 计算后传入。
// characters/anchors/plotPoints 为调用方预取的数据（避免每段重复查 DB）。
// prevShots: 上一段落末尾的 N 个分镜，用于跨段落情节连贯（顺序处理时传入，并发时为 nil）。
func (s *VideoService) buildStoryboardPrompt(
	video *model.Video, content string,
	segNo, totalSegs, expectedShots int,
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	items []*EffectiveItem,
	prevShots []*model.StoryboardShot,
	genre string,
	arcPlan string,
	imageStyle string,
	chapterNo int,
	novelTitle string,
	worldviewDesc string,
	segStartPct, segEndPct int,
	charVisualPrompts map[uint]string,
	beatSheet []map[string]interface{}, // P1a: 情节节拍表（本段子集）
	worldState map[string]interface{}, // P1b: 跨段世界状态快照
) string {
	segLabel := ""
	if totalSegs > 1 {
		segLabel = fmt.Sprintf("（第%d段，共%d段）", segNo, totalSegs)
	}

	// 过滤角色：优先匹配内容中出现的角色，否则回退到主角
	var matchedChars []map[string]interface{}
	if len(characters) > 0 {
		// 始终将所有角色传给 AI（最多 10 个），避免 AI 因拿不到角色名而写出无法匹配的随机字符串。
		// 优先级：① 名字字面出现在本段内容中 → ② 主角 → ③ 配角 → ④ 其余角色。
		const maxCharsInPrompt = 10
		contentLower := strings.ToLower(content)

		type scoredChar struct {
			c     *model.Character
			score int // 越大优先级越高
		}
		scored := make([]scoredChar, 0, len(characters))
		for _, c := range characters {
			s := 0
			if strings.Contains(contentLower, strings.ToLower(c.Name)) {
				s += 4
			}
			switch c.Role {
			case "protagonist":
				s += 3
			case "supporting":
				s += 2
			case "minor":
				s += 1
			}
			scored = append(scored, scoredChar{c, s})
		}
		// 稳定排序（score 相同保持原顺序）
		sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })

		limit := len(scored)
		if limit > maxCharsInPrompt {
			limit = maxCharsInPrompt
		}
		matchedChars = make([]map[string]interface{}, 0, limit)
		for _, sc := range scored[:limit] {
			c := sc.c
			matchedChars = append(matchedChars, map[string]interface{}{
				"Name":          c.Name,
				"Role":          c.Role,
				"Description":   c.Description,
				"VisualPrompt":  charVisualPrompts[c.ID], // 来自 CharacterLook.VisualPrompt（默认形象的英文视觉提示词）
				"DialogueLang":  voiceLangToDialogueLang(c.VoiceConfig.VoiceLanguage),
				"InnerConflict": c.Meta.InnerConflict,
				"CoreDesire":    c.Meta.CoreDesire,
			})
		}
	}

	// 过滤锚点：优先匹配内容中出现的锚点，否则取前 5 个，最多 8 个
	var matchedAnchors []map[string]interface{}
	if len(anchors) > 0 {
		contentLower := strings.ToLower(content)
		var ma []*model.SceneAnchor
		for _, a := range anchors {
			if strings.Contains(contentLower, strings.ToLower(a.Name)) {
				ma = append(ma, a)
			}
		}
		if len(ma) == 0 {
			limit := 5
			if len(anchors) < limit {
				limit = len(anchors)
			}
			ma = anchors[:limit]
		}
		if len(ma) > 8 {
			ma = ma[:8]
		}
		matchedAnchors = make([]map[string]interface{}, 0, len(ma))
		for _, a := range ma {
			matchedAnchors = append(matchedAnchors, map[string]interface{}{
				"Name":        a.Name,
				"Description": a.Description,
			})
		}
	}

	// 剧情线（最多 5 个）
	maxPP := len(plotPoints)
	if maxPP > 5 {
		maxPP = 5
	}
	ppData := make([]map[string]interface{}, 0, maxPP)
	for i := 0; i < maxPP; i++ {
		ppData = append(ppData, map[string]interface{}{
			"Type":        plotPoints[i].Type,
			"Description": plotPoints[i].Description,
		})
	}

	// 上一段末尾分镜（携带完整状态供 AI 做无缝衔接）
	prevShotsData := make([]map[string]interface{}, 0, len(prevShots))
	for _, ps := range prevShots {
		narrOrDesc := ps.Narration()
		if narrOrDesc == "" {
			narrOrDesc = ps.Description
		}
		prevShotsData = append(prevShotsData, map[string]interface{}{
			"ShotNo":     ps.ShotNo,
			"NarrOrDesc": narrOrDesc,
			"Dialogue":   ps.Dialogue(),
			"CameraType": ps.CamDir.CameraType,
			"Location":   extractLocationFromScene(ps.GenMeta.Scene),
		})
	}

	// 道具（最多 10 个，优先匹配内容中出现的道具）
	itemsData := make([]map[string]interface{}, 0, 10)
	if len(items) > 0 {
		contentLower := strings.ToLower(content)
		type scoredItem struct {
			ei    *EffectiveItem
			score int
		}
		scoredItems := make([]scoredItem, 0, len(items))
		for _, ei := range items {
			sc := 0
			if strings.Contains(contentLower, strings.ToLower(ei.Name)) {
				sc += 2
			}
			scoredItems = append(scoredItems, scoredItem{ei, sc})
		}
		sort.SliceStable(scoredItems, func(i, j int) bool { return scoredItems[i].score > scoredItems[j].score })
		limit := len(scoredItems)
		if limit > 10 {
			limit = 10
		}
		for _, si := range scoredItems[:limit] {
			m := map[string]interface{}{
				"Name":     si.ei.Name,
				"Location": si.ei.EffectiveLocation,
				"Owner":    si.ei.EffectiveOwner,
			}
			itemsData = append(itemsData, m)
		}
	}

	// 截断内容：上限对齐 maxSegmentRunes，消除分段长度与截断限制的不一致
	// splitContentSegments 已将每段限制在 dynSegRunes（≤maxSegmentRunes）内，
	// 此处截断仅作最后防护，不应再裁掉段内末尾内容。
	if cr := []rune(content); len(cr) > maxSegmentRunes {
		content = string(cr[:maxSegmentRunes]) + "…（已截断）"
	}

	expectedShotsMinus2 := expectedShots - 2
	if expectedShotsMinus2 < 0 {
		expectedShotsMinus2 = 0
	}

	ctx := map[string]interface{}{
		"SegLabel":            segLabel,
		"ExpectedShots":       expectedShots,
		"ExpectedShotsMinus2": expectedShotsMinus2,
		"VideoMode":           video.Mode,           // "slideshow"(图片解说) | "video"(视频动画，含未设置时的默认)
		"StoryboardMode":      video.StoryboardMode, // "professional"(专业分镜) | "faithful"(忠于原文) | "concise"(简洁模式)，含未设置时的默认
		"PrevShots":           prevShotsData,
		"Characters":          matchedChars,
		"Anchors":             matchedAnchors,
		"PlotPoints":          ppData,
		"Items":               itemsData,
		"Content":             content,
		"ArcPlan":             arcPlan,
		"GenreVisualHints":    genreVisualHints(genre),
		// ImageStyleHint: 画面风格的英文提示词，告知 LLM 生成 image_prompt 时必须使用的风格基调。
		// 不传原始 imageStyle ID（如 "anime"），而是传对 LLM 最有指导意义的英文描述词。
		"ImageStyleHint":     resolveStyleIllustrationDesc(imageStyle),
		"ImageStyleID":       imageStyle,
		"StyleQualityTokens": resolveStyleQualityTokens(imageStyle),
		"ChapterNo":          chapterNo,
		"IsFirstChapter":     chapterNo == 1,
		"IsFirstSegment":     segNo == 1,
		"NovelTitle":         novelTitle,
		"WorldviewDesc":      worldviewDesc,
		"SegStartPct":        segStartPct,
		"SegEndPct":          segEndPct,
		"BeatSheet":          beatSheet,  // P1a
		"WorldState":         worldState, // P1b
	}
	result, err := renderPrompt("storyboard_generate", ctx)
	if err != nil {
		logger.Errorf("[buildStoryboardPrompt] renderPrompt error: %v", err)
		return ""
	}
	return result
}

// parseStoryboardResult 解析AI分镜响应。解析失败时返回 error（不生成空占位）。
func (s *VideoService) parseStoryboardResult(videoID uint, chapterID *uint, result string) ([]*model.StoryboardShot, error) {
	// 提取 JSON 数组
	cleaned := extractJSON(result)

	type rawShotType struct {
		ShotNo       int     `json:"shot_no"`
		Description  string  `json:"description"`
		OriginalText string  `json:"original_text"`
		CameraType   string  `json:"camera_type"`
		Duration     float64 `json:"duration"`
		Location     string  `json:"location"`
		Transition   string  `json:"transition"`
		VoiceLines   []struct {
			Character string `json:"character"`
			Content   string `json:"content"`
			Emotion   string `json:"emotion"`
		} `json:"voice_lines"`
		SFXTags []struct {
			Tag     string `json:"tag"`
			SFXType string `json:"type"`
			Prompt  string `json:"prompt"`
		} `json:"sfx_tags"`
	}

	var rawShots []rawShotType

	parseErr := json.Unmarshal([]byte(cleaned), &rawShots)
	if parseErr != nil || len(rawShots) == 0 {
		// 尝试修复截断 JSON（模型输出被 max_tokens 截断时常见）
		repaired := repairTruncatedJSONArray(cleaned)
		if repaired != cleaned {
			var repairedShots []rawShotType
			if err2 := json.Unmarshal([]byte(repaired), &repairedShots); err2 == nil && len(repairedShots) > 0 {
				logger.Printf("[VideoService] parseStoryboardResult: JSON was truncated; repaired and recovered %d shots (original len=%d). Consider increasing Max Tokens.", len(repairedShots), len(result))
				rawShots = repairedShots
				parseErr = nil
			}
		}
	}
	if parseErr != nil || len(rawShots) == 0 {
		logger.Errorf("[VideoService] parseStoryboardResult: JSON parse failed (%v)\n===== AI RAW RESPONSE (len=%d) =====\n%s\n===== END =====", parseErr, len(result), result)
		if parseErr != nil {
			return nil, fmt.Errorf("分镜JSON解析失败（JSON 疑似被模型截断，建议在高级参数中增大 Max Tokens ≥16384 或减少目标分镜数量）: %w", parseErr)
		}
		return nil, fmt.Errorf("AI返回了空的分镜列表，请检查章节内容或重试")
	}

	// 按 shot_no 升序排列：保证 AI 以非顺序输出时仍能还原正确叙事顺序。
	// 先补全 shot_no=0 的条目（用数组下标），再统一排序。
	for i := range rawShots {
		if rawShots[i].ShotNo == 0 {
			rawShots[i].ShotNo = i + 1
		}
	}
	sort.SliceStable(rawShots, func(i, j int) bool {
		return rawShots[i].ShotNo < rawShots[j].ShotNo
	})

	shots := make([]*model.StoryboardShot, 0, len(rawShots))
	for i, r := range rawShots {
		shotNo := r.ShotNo
		if shotNo == 0 {
			shotNo = i + 1
		}
		duration := r.Duration
		if duration <= 0 {
			duration = defaultShotDurationSecs
		}

		// 将场景配置序列化
		scene := map[string]string{
			"location": r.Location,
		}
		var sceneJSON string
		if b, err := json.Marshal(scene); err == nil {
			sceneJSON = string(b)
		}

		// description 只保留纯画面内容（构图/光线/角色动作等），不含风格标签或画质提升词——
		// 这两者由 video_image_service.go 在实际生成图片/视频时按项目风格统一注入，
		// 确保 UI 展示的分镜描述干净可读，同时不影响出图/出视频质量。
		description := r.Description

		cameraType := validCameraType(r.CameraType)

		// 将音效标签序列化存储
		var sfxTagsJSON string
		if len(r.SFXTags) > 0 {
			if b, err := json.Marshal(r.SFXTags); err == nil {
				sfxTagsJSON = string(b)
			}
		}

		// 将旁白/台词行序列化存储（合并字段：character 为空=旁白，非空=该角色台词）
		var voiceLinesJSON string
		if len(r.VoiceLines) > 0 {
			if b, err := json.Marshal(r.VoiceLines); err == nil {
				voiceLinesJSON = string(b)
			}
		}

		// B1: 字段质量校验 ── description 长度
		if len([]rune(description)) < 100 {
			logger.Printf("[Storyboard][B1] shot_no=%d: description too short (%d runes), generation quality may degrade", shotNo, len([]rune(description)))
		}

		shot := &model.StoryboardShot{
			UUID:         uuid.New().String(),
			VideoID:      videoID,
			ChapterID:    chapterID,
			ShotNo:       shotNo,
			Description:  description,
			OriginalText: r.OriginalText,
			Duration:     duration,
			CamDir: model.ShotCamDir{
				CameraType: cameraType,
				Transition: validTransition(r.Transition),
			},
			GenMeta: model.ShotGenMeta{
				Scene:      sceneJSON,
				SFXTags:    sfxTagsJSON,
				VoiceLines: voiceLinesJSON,
			},
			Status: "pending",
		}
		shots = append(shots, shot)
	}
	return shots, nil
}

// validateShotSequence 验证分镜列表序号是否从 1 开始连续递增（无跳空）。
func validateShotSequence(shots []*model.StoryboardShot) error {
	if len(shots) == 0 {
		return fmt.Errorf("no shots generated")
	}
	for i, s := range shots {
		if s.ShotNo != i+1 {
			return fmt.Errorf("shot sequence gap: expected shot_no %d, got %d", i+1, s.ShotNo)
		}
	}
	return nil
}

// validTransition 验证过渡方式，无效时返回默认值 cut
func validTransition(t string) string {
	valid := map[string]bool{
		"cut": true, "fade": true, "dissolve": true,
		"wipe": true, "push": true, "j-cut": true, "l-cut": true,
	}
	if valid[t] {
		return t
	}
	return "cut"
}

// validCameraType 验证摄像机类型，无效时返回默认值 static
func validCameraType(t string) string {
	valid := map[string]bool{
		"static": true, "push": true, "pull": true, "pan": true,
		"track": true, "crane_up": true, "crane_down": true,
		"follow": true, "arc": true, "tilt": true, "whip_pan": true, "zoom": true,
	}
	if valid[t] {
		return t
	}
	return "static"
}

// ─── Storyboard CRUD ──────────────────────────────────────────────────────────

// GetStoryboard 获取分镜列表。sceneID 非零时按剧本场次过滤，避免按场次查看时
// 拉取整个视频的全部分镜（见 ListByVideoAndScene 注释）。
func (s *VideoService) GetStoryboard(videoID, sceneID uint) ([]*model.StoryboardShot, error) {
	if sceneID > 0 {
		return s.storyboardRepo.ListByVideoAndScene(videoID, sceneID)
	}
	return s.storyboardRepo.ListByVideo(videoID)
}

// GetStoryboardSummary 获取分镜轻量汇总（不含 description 等大字段），供场次侧边栏/
// 时间轴等聚合展示场景使用（见 ListSummaryByVideo 注释）。
func (s *VideoService) GetStoryboardSummary(videoID uint) ([]repository.ShotSummary, error) {
	return s.storyboardRepo.ListSummaryByVideo(videoID)
}

// snapshotShotsBeforeOverwrite 在删除某视频当前全部分镜前，把它们整体序列化成一条历史快照
// （best-effort：失败只记日志，不阻断调用方本身的操作）——整视频重新生成/恢复历史版本都是
// 删除重建，旧分镜 ID 不会保留，所以按视频存一份快照，而不是按单条 shot 存。changeType 标注
// 这次覆盖的原因（"regenerate"=重新生成分镜覆盖，"restore"=恢复到历史版本前保留当前内容）。
func (s *VideoService) snapshotShotsBeforeOverwrite(videoID uint, changeType string) {
	if s.shotVersionRepo == nil {
		return
	}
	oldShots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil || len(oldShots) == 0 {
		return
	}
	content, err := json.Marshal(oldShots)
	if err != nil {
		logger.Errorf("[VideoService] marshal storyboard snapshot videoID=%d: %v", videoID, err)
		return
	}
	if err := s.shotVersionRepo.CreateAtomic(&model.StoryboardShotVersion{
		VideoID:    videoID,
		Content:    string(content),
		ShotCount:  len(oldShots),
		ChangeType: changeType,
	}); err != nil {
		logger.Errorf("[VideoService] create storyboard version videoID=%d: %v", videoID, err)
	}
}

// GetStoryboardVersions 返回某视频的全部分镜历史版本（按版本号倒序）。
func (s *VideoService) GetStoryboardVersions(videoID uint) ([]*model.StoryboardShotVersion, error) {
	if s.shotVersionRepo == nil {
		return nil, fmt.Errorf("version history not available")
	}
	return s.shotVersionRepo.List(videoID)
}

// RestoreStoryboardVersion 把某视频的分镜恢复到指定历史版本：删除当前全部分镜，按快照重新插入
// （沿用整视频重新生成"删除重建"的语义——恢复出的分镜是全新的行，ID/UUID 不会与快照前一致）。
// 恢复前会把当前分镜也落一条历史快照，所以恢复本身可逆。
func (s *VideoService) RestoreStoryboardVersion(videoID uint, versionNo int) ([]*model.StoryboardShot, error) {
	if s.shotVersionRepo == nil {
		return nil, fmt.Errorf("version history not available")
	}
	version, err := s.shotVersionRepo.GetVersion(videoID, versionNo)
	if err != nil {
		return nil, fmt.Errorf("version not found: %w", err)
	}
	var snapshotShots []*model.StoryboardShot
	if err := json.Unmarshal([]byte(version.Content), &snapshotShots); err != nil {
		return nil, fmt.Errorf("invalid version snapshot: %w", err)
	}
	for _, shot := range snapshotShots {
		shot.ID = 0
		shot.UUID = uuid.New().String()
		shot.VideoID = videoID
	}
	s.snapshotShotsBeforeOverwrite(videoID, "restore")
	if err := s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
		var oldShotIDs []uint
		tx.Model(&model.StoryboardShot{}).Where("video_id = ?", videoID).Pluck("id", &oldShotIDs)
		if len(oldShotIDs) > 0 {
			if err := tx.Unscoped().Where("shot_id IN ?", oldShotIDs).Delete(&model.ShotVoiceSegment{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Unscoped().Where("video_id = ?", videoID).Delete(&model.StoryboardShot{}).Error; err != nil {
			return err
		}
		if len(snapshotShots) == 0 {
			return nil
		}
		return tx.Create(snapshotShots).Error
	}); err != nil {
		return nil, fmt.Errorf("恢复分镜失败: %w", err)
	}
	return s.storyboardRepo.ListByVideo(videoID)
}

// GetShot 根据 ID 获取单个分镜
func (s *VideoService) GetShot(id uint) (*model.StoryboardShot, error) {
	return s.storyboardRepo.GetByID(id)
}

// GetShotByID 获取分镜并验证归属
func (s *VideoService) GetShotByID(videoID, shotID uint) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}
	return shot, nil
}

// ListShotAssetHistory 返回某个分镜历史生成过的图片/视频素材（最新在前），供前端"视频生成历史"
// 面板展示——每次重新生成图片/视频前，旧素材会被 snapshotShotAsset 存一份，见 video_service.go。
func (s *VideoService) ListShotAssetHistory(videoID, shotID uint) ([]*model.Asset, error) {
	if _, err := s.GetShotByID(videoID, shotID); err != nil {
		return nil, err
	}
	if s.assetRepo == nil {
		return nil, nil
	}
	return s.assetRepo.ListByShotID(shotID)
}

// RestoreShotAsset 把分镜恢复到历史记录里的某个版本：直接把 asset 的 storage_url 写回 shot
// 对应字段（按 asset.Type 决定覆盖 ImageURL 还是 VideoURL），不产生新的历史记录——恢复动作本身
// 不算一次新生成，历史列表里的版本条数不应因为回滚而增加。
func (s *VideoService) RestoreShotAsset(tenantID, videoID, shotID, assetID uint) (*model.StoryboardShot, error) {
	shot, err := s.GetShotByID(videoID, shotID)
	if err != nil {
		return nil, err
	}
	if s.assetRepo == nil {
		return nil, fmt.Errorf("asset history not available")
	}
	asset, err := s.assetRepo.GetByID(assetID)
	if err != nil {
		return nil, fmt.Errorf("history asset not found: %w", err)
	}
	if asset.QualityMeta.ShotID == nil || *asset.QualityMeta.ShotID != shotID {
		return nil, fmt.Errorf("asset %d does not belong to shot %d", assetID, shotID)
	}
	switch asset.Type {
	case "image":
		shot.ImageURL = asset.MediaMeta.StorageURL
	case "video":
		shot.VideoURL = asset.MediaMeta.StorageURL
	default:
		return nil, fmt.Errorf("unsupported asset type %q", asset.Type)
	}
	if err := s.storyboardRepo.Update(shot); err != nil {
		return nil, fmt.Errorf("恢复历史版本失败: %w", err)
	}
	return shot, nil
}

// UpdateShot 更新分镜
func (s *VideoService) UpdateShot(id uint, req *model.StoryboardShot) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.CamDir.CameraType != "" {
		shot.CamDir.CameraType = req.CamDir.CameraType
	}
	if req.Duration > 0 {
		shot.Duration = req.Duration
	}
	if req.Status != "" {
		shot.Status = req.Status
	}
	if req.GenMeta.GenerationMode != "" {
		shot.GenMeta.GenerationMode = req.GenMeta.GenerationMode
	}
	return shot, s.storyboardRepo.Update(shot)
}

// UpdateShotPartial 按字段 map 部分更新分镜，仅更新请求中明确提供的字段。
// 允许的字段：description, narration, dialogue, subtitle, camera_type,
// duration, emotional_tone, transition, status, generation_mode.
//
// refreshShotUserEditableFields 在生成流水线（图片/视频/Ken Burns 等）覆盖式整行保存
// （storyboardRepo.Update，即 db.Save）之前调用，把该分镜当前 DB 值中"用户在 produce-v2.vue
// 里可编辑"的字段重新拉取覆盖到内存副本上。
//
// 背景：生成任务在开始时加载一次 shot 到内存，期间可能持续数秒到数分钟，只应改动自己拥有的
// 字段（status/image_url/video_url/task_meta/generation_mode 等），但沿用整行 Save 落库；
// 如果用户在这期间通过 UpdateShotPartial（旁白/台词/描述/角色绑定等，走安全的按字段 map 更新）
// 编辑了该分镜，任务收尾时的 Save 会用自己那份过期的内存副本把用户的编辑静默覆盖丢失。
// 这里只刷新用户可编辑字段、不动生成任务自己正在维护的字段，读取失败时静默忽略（尽力而为的
// 安全网，不应阻塞或中断生成流程本身）。
func (s *VideoService) refreshShotUserEditableFields(shot *model.StoryboardShot) {
	fresh, err := s.storyboardRepo.GetByID(shot.ID)
	if err != nil || fresh == nil {
		return
	}
	shot.Description = fresh.Description
	shot.CharacterIDs = fresh.CharacterIDs
	shot.ItemIDs = fresh.ItemIDs
	shot.SceneAnchorID = fresh.SceneAnchorID
	shot.GenMeta.VoiceLines = fresh.GenMeta.VoiceLines
	shot.CamDir.CameraType = fresh.CamDir.CameraType
	shot.CamDir.Transition = fresh.CamDir.Transition
}

// 注意：dialogue/generation_mode 存储在 gen_meta JSON 列；
// camera_type/transition 存储在 cam_dir JSON 列。
// GORM map Updates 不走 serializer，必须手动将这些字段合并到对应 JSON 列后再写入。
func (s *VideoService) UpdateShotPartial(id uint, fields map[string]interface{}) (*model.StoryboardShot, error) {
	// gen_meta JSON 列中的字段（narration/dialogue 现合并存储于 gen_meta.voice_lines）
	genMetaKeys := map[string]bool{
		"narration": true, "dialogue": true, "generation_mode": true,
	}
	// cam_dir JSON 列中的字段
	camDirKeys := map[string]bool{
		"camera_type": true, "transition": true,
	}
	// 直接列
	directKeys := map[string]bool{
		"description": true, "duration": true,
		"status": true, "image_url": true,
	}

	needsLoad := false
	for k := range fields {
		if genMetaKeys[k] || camDirKeys[k] {
			needsLoad = true
			break
		}
	}

	safe := make(map[string]interface{}, len(fields))

	if needsLoad {
		// 先读取当前值，再在结构体上应用变更，最后序列化整列写入
		shot, err := s.storyboardRepo.GetByID(id)
		if err != nil {
			return nil, err
		}
		updatedGenMeta := false
		updatedCamDir := false
		for k, v := range fields {
			sv, _ := v.(string)
			switch k {
			case "narration":
				shot.SetNarration(sv)
				updatedGenMeta = true
			case "dialogue":
				shot.SetDialogue(sv)
				updatedGenMeta = true
			case "generation_mode":
				shot.GenMeta.GenerationMode = sv
				updatedGenMeta = true
			case "camera_type":
				shot.CamDir.CameraType = sv
				updatedCamDir = true
			case "transition":
				shot.CamDir.Transition = sv
				updatedCamDir = true
			}
		}
		if updatedGenMeta {
			genMetaJSON, _ := json.Marshal(shot.GenMeta)
			safe["gen_meta"] = string(genMetaJSON)
		}
		if updatedCamDir {
			camDirJSON, _ := json.Marshal(shot.CamDir)
			safe["cam_dir"] = string(camDirJSON)
		}
	}

	// 直接列直接放入 safe map
	for k, v := range fields {
		if directKeys[k] {
			safe[k] = v
		}
	}

	if len(safe) == 0 {
		return s.storyboardRepo.GetByID(id)
	}
	if err := s.storyboardRepo.UpdateFields(id, safe); err != nil {
		return nil, err
	}
	return s.storyboardRepo.GetByID(id)
}

// SetShotCharacters 手动设置分镜的角色绑定
func (s *VideoService) SetShotCharacters(shotID uint, ids []uint) error {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return err
	}
	shot.CharacterIDs = model.JSONUintSlice(ids)
	return s.storyboardRepo.Update(shot)
}

func (s *VideoService) SetShotItems(shotID uint, ids []uint) error {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return err
	}
	shot.ItemIDs = model.JSONUintSlice(ids)
	return s.storyboardRepo.Update(shot)
}

// RegenerateShotPrompt 根据分镜当前绑定的角色/道具/场景，重新生成 description（AI 出图/出视频的唯一依据）。
// 背景：绑定/解绑角色、道具、场景锚点只更新结构化字段（CharacterIDs/ItemIDs/SceneAnchorID），
// Description 是分镜脚本生成时 LLM 一次性写死的叙事文本，不会随绑定变化自动重写——导致"改了
// 绑定但生成结果没变"。这个方法把旧描述连同当前绑定一起喂给 LLM 重新改写，而不是简单的关键词
// 替换（中文语法下增删人名很容易改出病句），由调用方（前端在用户手动点击"重新生成提示词"时）
// 显式触发，不在绑定变化时自动执行。
func (s *VideoService) RegenerateShotPrompt(ctx context.Context, tenantID, videoID, shotID uint) (*model.StoryboardShot, error) {
	shot, err := s.GetShotByID(videoID, shotID)
	if err != nil {
		return nil, fmt.Errorf("shot not found: %w", err)
	}
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, fmt.Errorf("video not found: %w", err)
	}

	var charNames []string
	if len(shot.CharacterIDs) > 0 && s.characterRepo != nil {
		if chars, e := s.characterRepo.ListByIDs([]uint(shot.CharacterIDs)); e == nil {
			for _, c := range chars {
				charNames = append(charNames, c.Name)
			}
		}
	}
	var itemNames []string
	if len(shot.ItemIDs) > 0 && s.itemRepo != nil {
		if items, e := s.itemRepo.ListByIDs([]uint(shot.ItemIDs)); e == nil {
			for _, it := range items {
				itemNames = append(itemNames, it.Name)
			}
		}
	}
	sceneDesc := ""
	if shot.SceneAnchorID != nil && s.sceneAnchorSvc != nil {
		if anchor, e := s.sceneAnchorSvc.GetByID(*shot.SceneAnchorID); e == nil {
			sceneDesc = anchor.Description
			if sceneDesc == "" {
				sceneDesc = anchor.Name
			}
		}
	}

	novelTitle, genre := novelPromptContext(s.novelRepo, video.NovelID)

	characterList := "无绑定角色"
	if len(charNames) > 0 {
		characterList = strings.Join(charNames, "、")
	}
	itemList := "无绑定道具"
	if len(itemNames) > 0 {
		itemList = strings.Join(itemNames, "、")
	}
	sceneText := "未绑定场景，沿用原提示词中的场景设定"
	if sceneDesc != "" {
		sceneText = sceneDesc
	}

	rendered, tplErr := renderPrompt("regenerate_shot_prompt", map[string]interface{}{
		"NovelTitle":     novelTitle,
		"Genre":          genre,
		"CharacterList":  characterList,
		"ItemList":       itemList,
		"SceneText":      sceneText,
		"OldDescription": shot.Description,
		"StoryboardMode": video.StoryboardMode,
	})
	if tplErr != nil {
		return nil, fmt.Errorf("render regenerate_shot_prompt: %w", tplErr)
	}

	result, genErr := s.aiService.GenerateWithProviderCtx(ctx, tenantID, "regenerate_shot_prompt", rendered)
	if genErr != nil {
		return nil, fmt.Errorf("AI regenerate shot prompt: %w", genErr)
	}

	type regenResult struct {
		Description string `json:"description"`
	}
	var parsed regenResult
	cleaned := extractJSON(strings.TrimSpace(result))
	if parseErr := json.Unmarshal([]byte(cleaned), &parsed); parseErr != nil {
		logger.Errorf("[VideoService] RegenerateShotPrompt: parse error: %v, raw: %.300s", parseErr, result)
		return nil, fmt.Errorf("parse regenerated prompt JSON: %w", parseErr)
	}
	if parsed.Description == "" {
		return nil, fmt.Errorf("AI 返回的画面描述为空")
	}

	// 先拉取最新用户可编辑字段（旁白/台词/角色绑定等），再把本函数要改的 Description 盖在上面——
	// 顺序不能反，否则 refresh 会把这里刚生成的新 Description 又冲回旧值。
	s.refreshShotUserEditableFields(shot)
	shot.Description = parsed.Description
	if err := s.storyboardRepo.Update(shot); err != nil {
		return nil, fmt.Errorf("save regenerated prompt: %w", err)
	}
	return shot, nil
}

// InsertShot 在 afterShotNo 之后插入一个空分镜（afterShotNo=0 表示插入到最前；afterShotNo<0 表示追加到末尾）
func (s *VideoService) InsertShot(videoID uint, afterShotNo int, narration, description string, duration float64) (*model.StoryboardShot, error) {
	appendToEnd := afterShotNo < 0
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	shot := &model.StoryboardShot{
		VideoID:     videoID,
		UUID:        uuid.New().String(),
		Description: description,
		Duration:    duration,
		CamDir: model.ShotCamDir{
			CameraType: "static",
			Transition: "cut",
		},
		Status: "pending",
	}
	shot.SetNarration(narration)
	// Shift + create must be atomic to avoid a corrupt shot_no sequence on partial failure.
	// Two-phase UPDATE: first shift all affected rows into a collision-free temp range
	// (+shiftTempOffset), then shift back to the intended position. A single-step UPDATE
	// causes MySQL to process rows one by one and trigger the unique key constraint
	// mid-scan (e.g. shot 7→8 conflicts while shot 8 still exists).
	// When appending to end, the MAX query runs inside the transaction under FOR UPDATE to
	// prevent two concurrent appends from computing the same shot_no.
	err := s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
		resolvedAfter := afterShotNo
		if appendToEnd {
			var maxNo int
			if e := tx.Raw(
				"SELECT COALESCE(MAX(shot_no), 0) FROM ink_storyboard_shot WHERE video_id = ? AND deleted_at IS NULL FOR UPDATE",
				videoID,
			).Scan(&maxNo).Error; e != nil {
				return e
			}
			resolvedAfter = maxNo
		}

		// 继承相邻分镜所属的分场：优先取插入位置前一个分镜的场次；若插入到最前
		// （resolvedAfter==0，没有"前一个"分镜），退而取原本排第一、插入后紧随其后的分镜的场次。
		neighborShotNo := resolvedAfter
		if neighborShotNo == 0 {
			neighborShotNo = 1
		}
		var neighbor model.StoryboardShot
		if e := tx.Where("video_id = ? AND shot_no = ? AND deleted_at IS NULL", videoID, neighborShotNo).First(&neighbor).Error; e == nil {
			shot.ScreenplaySceneID = neighbor.ScreenplaySceneID
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}

		newShotNo := resolvedAfter + 1
		shot.ShotNo = newShotNo
		if e := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			shiftTempOffset, videoID, newShotNo,
		).Error; e != nil {
			return e
		}
		if e := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? + 1 WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			shiftTempOffset, videoID, newShotNo+shiftTempOffset,
		).Error; e != nil {
			return e
		}
		return tx.Create(shot).Error
	})
	if err != nil {
		return nil, fmt.Errorf("insert shot: %w", err)
	}
	return shot, nil
}

// CopyShotAfter 复制分镜，插入到 afterShotNo 之后（afterShotNo=0 → 复制到最前；afterShotNo=-1 → 追加到列表末尾）
func (s *VideoService) CopyShotAfter(sourceShotID uint, afterShotNo int) (*model.StoryboardShot, error) {
	src, err := s.storyboardRepo.GetByID(sourceShotID)
	if err != nil {
		return nil, fmt.Errorf("source shot not found: %w", err)
	}
	appendToEnd := afterShotNo < 0
	shot := &model.StoryboardShot{
		VideoID:           src.VideoID,
		ChapterID:         src.ChapterID,
		UUID:              uuid.New().String(),
		Description:       src.Description,
		Duration:          src.Duration,
		CamDir:            src.CamDir,
		GenMeta:           src.GenMeta,
		SceneAnchorID:     src.SceneAnchorID,
		CharacterIDs:      src.CharacterIDs,
		ScreenplaySceneID: src.ScreenplaySceneID,
		// ImageURL / VideoURL intentionally NOT copied — copied shot starts fresh
		Status: "pending",
	}
	err = s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
		resolvedAfter := afterShotNo
		if appendToEnd {
			var maxNo int
			tx.Raw("SELECT COALESCE(MAX(shot_no), 0) FROM ink_storyboard_shot WHERE video_id = ? AND deleted_at IS NULL FOR UPDATE", src.VideoID).Scan(&maxNo)
			resolvedAfter = maxNo
		}
		newShotNo := resolvedAfter + 1
		shot.ShotNo = newShotNo
		if e := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no + ? WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			shiftTempOffset, src.VideoID, newShotNo,
		).Error; e != nil {
			return e
		}
		if e := tx.Exec(
			"UPDATE ink_storyboard_shot SET shot_no = shot_no - ? + 1 WHERE video_id = ? AND shot_no >= ? AND deleted_at IS NULL",
			shiftTempOffset, src.VideoID, newShotNo+shiftTempOffset,
		).Error; e != nil {
			return e
		}
		return tx.Create(shot).Error
	})
	if err != nil {
		return nil, fmt.Errorf("copy shot: %w", err)
	}
	return shot, nil
}

// DeleteShot 删除单个分镜并将后续分镜 shot_no 前移（保持连续）
func (s *VideoService) DeleteShot(shotID uint) error {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return fmt.Errorf("shot %d not found: %w", shotID, err)
	}
	if err := s.storyboardRepo.Delete(shotID); err != nil {
		return err
	}
	return s.storyboardRepo.CompactShotNosAfter(shot.VideoID, shot.ShotNo)
}

// ReorderShots 交换两个分镜的 shot_no，实现拖拽排序。
// fromShotID 和 toShotID 是前端传入的分镜 ID；操作在数据库事务内完成，避免唯一键冲突。
func (s *VideoService) ReorderShots(fromShotID, toShotID uint) (fromShotNo, toShotNo int, err error) {
	fromShot, err := s.storyboardRepo.GetByID(fromShotID)
	if err != nil {
		return 0, 0, fmt.Errorf("shot %d not found: %w", fromShotID, err)
	}
	toShot, err := s.storyboardRepo.GetByID(toShotID)
	if err != nil {
		return 0, 0, fmt.Errorf("shot %d not found: %w", toShotID, err)
	}
	if fromShot.VideoID != toShot.VideoID {
		return 0, 0, fmt.Errorf("shots belong to different videos")
	}

	// Use a large temporary offset to avoid unique-key collisions during swap.
	const tmpOffset = 100000
	db := s.storyboardRepo.DB()
	if err := db.Transaction(func(tx *gorm.DB) error {
		// Move fromShot to a temp position to avoid conflict
		if err := tx.Exec("UPDATE ink_storyboard_shot SET shot_no = ? WHERE id = ?",
			fromShot.ShotNo+tmpOffset, fromShotID).Error; err != nil {
			return err
		}
		// Move toShot to fromShot's original position
		if err := tx.Exec("UPDATE ink_storyboard_shot SET shot_no = ? WHERE id = ?",
			fromShot.ShotNo, toShotID).Error; err != nil {
			return err
		}
		// Move fromShot from temp to toShot's original position
		return tx.Exec("UPDATE ink_storyboard_shot SET shot_no = ? WHERE id = ?",
			toShot.ShotNo, fromShotID).Error
	}); err != nil {
		return 0, 0, err
	}
	return toShot.ShotNo, fromShot.ShotNo, nil
}

// ─── Storyboard Review & Optimize ────────────────────────────────────────────

// ReviewStoryboard 调用 AI 对分镜脚本进行专业审查，返回结构化报告及历史记录 ID。
// previousScore > 0 时，将上次评分注入提示词，引导 AI 做相对评估而非每次独立打分。
func (s *VideoService) ReviewStoryboard(ctx context.Context, tenantID, videoID uint, provider string, previousScore float64) (*model.StoryboardReview, uint, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return nil, 0, fmt.Errorf("获取分镜失败: %w", err)
	}
	if len(shots) == 0 {
		return nil, 0, fmt.Errorf("该视频暂无分镜，请先生成分镜脚本")
	}

	// 取 Video.NovelID / ChapterID / Mode 以便选择 provider、注入章节原文、区分审查标准
	var novelID uint
	var chapterContent string
	var videoMode string
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
		videoMode = video.Mode
		if video.ChapterID != nil && s.chapterRepo != nil {
			if ch, err := s.chapterRepo.GetByID(*video.ChapterID); err == nil && ch != nil {
				chapterContent = ch.Content
			}
		}
	}

	// 拉取最近一次已应用的审查反馈，注入提示词避免重复建议
	var previousFeedback []model.ShotReviewFeedback
	if s.reviewRecordRepo != nil {
		if latest, err := s.reviewRecordRepo.GetLatestApplied(model.ReviewEntityStoryboard, videoID); err == nil && latest.ReviewJSON != "" {
			var prev model.StoryboardReview
			if json.Unmarshal([]byte(latest.ReviewJSON), &prev) == nil {
				previousFeedback = prev.ShotFeedback
			}
		}
	}

	// 拉取用户永久忽略的建议
	var ignoredItems []*model.IgnoredReviewIssue
	if s.ignoredSuggestionRepo != nil {
		ignoredItems, _ = s.ignoredSuggestionRepo.ListByEntity(model.ReviewEntityStoryboard, videoID)
	}

	prompt := buildStoryboardReviewPrompt(shots, chapterContent, previousScore, previousFeedback, ignoredItems, videoMode)

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, "storyboard_review", prompt)
	if err != nil {
		return nil, 0, fmt.Errorf("AI审查失败: %w", err)
	}

	review, err := parseStoryboardReview(result)
	if err != nil {
		return nil, 0, err
	}

	// 持久化审查记录
	var recordID uint
	if s.reviewRecordRepo != nil {
		reviewJSON, _ := json.Marshal(review)
		rec := &model.ReviewRecord{
			NovelID:      novelID,
			EntityType:   model.ReviewEntityStoryboard,
			EntityID:     videoID,
			OverallScore: review.OverallScore,
			ReviewJSON:   string(reviewJSON),
			Status:       "pending",
		}
		if saveErr := s.reviewRecordRepo.Create(rec); saveErr != nil {
			logger.Errorf("ReviewStoryboard: save record failed: %v", saveErr)
		} else {
			recordID = rec.ID
		}
	}

	// Mark video as having a pending review
	if s.videoRepo != nil {
		if err := s.videoRepo.UpdateFields(videoID, map[string]interface{}{
			"review_status": "pending",
		}); err != nil {
			logger.Errorf("ReviewStoryboard: update video review_status: %v", err)
		}
	}

	return review, recordID, nil
}

// ApplyReviewInserts 将 AI 审查建议的插入分镜依次写入数据库。
// 从最大 after_shot_no 向小排序插入，避免逐步移位导致编号错乱。
func (s *VideoService) ApplyReviewInserts(videoID uint, inserts []model.ShotInsertSuggestion) (int, error) {
	// P2-6: 查询当前最大镜头编号，过滤越界插入请求（AI 可能幻构不存在的 shot_no）
	existingShots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("list shots: %w", err)
	}
	maxShotNo := 0
	for _, sh := range existingShots {
		if sh.ShotNo > maxShotNo {
			maxShotNo = sh.ShotNo
		}
	}

	type indexedIns struct {
		ins     model.ShotInsertSuggestion
		origIdx int
	}
	indexed := make([]indexedIns, 0, len(inserts))
	for i, ins := range inserts {
		if ins.AfterShotNo < 0 || ins.AfterShotNo > maxShotNo {
			logger.Errorf("[ApplyReviewInserts] videoID=%d: skipping invalid AfterShotNo=%d (max=%d)", videoID, ins.AfterShotNo, maxShotNo)
			continue
		}
		indexed = append(indexed, indexedIns{ins: ins, origIdx: i})
	}
	// 按 AfterShotNo 降序；同一位置内按原始序号降序（反向插入），
	// 使同组多条建议最终以原始顺序排列（后插的出现在前面，先插的被推后）。
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].ins.AfterShotNo != indexed[j].ins.AfterShotNo {
			return indexed[i].ins.AfterShotNo > indexed[j].ins.AfterShotNo
		}
		return indexed[i].origIdx > indexed[j].origIdx
	})
	sorted := make([]model.ShotInsertSuggestion, 0, len(indexed))
	for _, item := range indexed {
		sorted = append(sorted, item.ins)
	}
	count := 0
	for _, ins := range sorted {
		// 旁白不做自动删除：即使建议同时带了台词，也按 AI 给出的 narration 原样写入，
		// 不因为存在 dialogue 就强制清空。
		shot, err := s.InsertShot(videoID, ins.AfterShotNo, ins.Narration, ins.Description, ins.Duration)
		if err != nil {
			return count, fmt.Errorf("insert after shot %d: %w", ins.AfterShotNo, err)
		}
		// Apply optional fields from the suggestion
		fields := map[string]interface{}{}
		if ins.Dialogue != "" {
			fields["dialogue"] = ins.Dialogue
		}
		if ins.CameraType != "" {
			fields["camera_type"] = ins.CameraType
		}
		if len(fields) > 0 {
			_ = s.storyboardRepo.UpdateFields(shot.ID, fields)
		}
		count++
	}
	return count, nil
}

// ApplyReviewDeletes 将 AI 审查建议的删除分镜从数据库中移除。
// 从最大 shot_no 向小排序删除，避免逐步移位导致编号错乱。
func (s *VideoService) ApplyReviewDeletes(videoID uint, shotNos []int) (int, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("list shots: %w", err)
	}
	shotNoToID := make(map[int]uint, len(shots))
	for _, sh := range shots {
		shotNoToID[sh.ShotNo] = sh.ID
	}

	sorted := make([]int, len(shotNos))
	copy(sorted, shotNos)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))

	count := 0
	for _, shotNo := range sorted {
		shotID, ok := shotNoToID[shotNo]
		if !ok {
			continue
		}
		if err := s.DeleteShot(shotID); err != nil {
			return count, fmt.Errorf("delete shot %d: %w", shotNo, err)
		}
		// Keep remaining map entries consistent after deletion (lower shotNos are unaffected)
		delete(shotNoToID, shotNo)
		count++
	}
	return count, nil
}

// buildStoryboardReviewPrompt 构建分镜审查提示词
// chapterContent 非空时注入小说章节原文（用于对比覆盖率、建议插入/删除）。
// previousScore > 0 时注入上次评分上下文；previousFeedback 非空时注入已修正问题；ignoredItems 非空时注入永久忽略列表。
func buildStoryboardReviewPrompt(shots []*model.StoryboardShot, chapterContent string, previousScore float64, previousFeedback []model.ShotReviewFeedback, ignoredItems []*model.IgnoredReviewIssue, videoMode string) string {
	// 预格式化分镜数据（带截断保护）
	var sb strings.Builder
	truncate := func(s string, max int) string {
		r := []rune(s)
		if len(r) > max {
			return string(r[:max]) + "…"
		}
		return s
	}
	for _, shot := range shots {
		sb.WriteString(fmt.Sprintf("[镜%d] 时长:%4.1fs 运镜:%-8s",
			shot.ShotNo, shot.Duration, shot.CamDir.CameraType))
		// description 现在是 AI 出图/出视频的唯一依据，截断长度放宽以便审查员评估视觉质量
		if desc := truncate(shot.Description, 200); desc != "" {
			sb.WriteString(fmt.Sprintf("\n      描述: %s", desc))
		}
		if narr := truncate(shot.Narration(), 80); narr != "" {
			sb.WriteString(fmt.Sprintf("\n      旁白: %s", narr))
		}
		if dial := shot.Dialogue(); dial != "" {
			sb.WriteString(fmt.Sprintf("\n      台词: %s", truncate(dial, 50)))
		}
		sb.WriteString("\n")
	}

	// 构建已修正问题摘要（按镜头号聚合 issues 文本）
	var prevFixedLines []string
	for _, fb := range previousFeedback {
		if len(fb.Issues) == 0 {
			continue
		}
		line := fmt.Sprintf("镜%d: %s", fb.ShotNo, strings.Join(fb.Issues, "；"))
		prevFixedLines = append(prevFixedLines, line)
	}

	// 构建忽略列表摘要
	var ignoredLines []string
	for _, item := range ignoredItems {
		var ctx struct {
			ShotNo int `json:"shot_no"`
		}
		_ = json.Unmarshal([]byte(item.ContextJSON), &ctx)
		ignoredLines = append(ignoredLines, fmt.Sprintf("镜%d: %s", ctx.ShotNo, item.IssueText))
	}

	// 截断章节原文（防止过长撑爆上下文，保留前 5000 字）
	truncatedChapter := chapterContent
	if runes := []rune(truncatedChapter); len(runes) > 5000 {
		truncatedChapter = string(runes[:5000]) + "…（已截断）"
	}

	shotCount30Pct := int(math.Round(float64(len(shots)) * 0.3))
	ctx := map[string]interface{}{
		"ShotCount":         len(shots),
		"ShotCount30Pct":    shotCount30Pct,
		"ShotsText":         sb.String(),
		"HasChapterContent": truncatedChapter != "",
		"ChapterContent":    truncatedChapter,
		"HasPreviousScore":  false, // P2-3: 不注入历史评分，避免 AI 锚定偏差影响本次独立评估
		"PreviousScoreStr":  "",
		"HasPreviousFixed":  len(prevFixedLines) > 0,
		"PreviousFixedText": strings.Join(prevFixedLines, "\n"),
		"HasIgnored":        len(ignoredLines) > 0,
		"IgnoredText":       strings.Join(ignoredLines, "\n"),
		"VideoMode":         videoMode,
	}
	result, err := renderPrompt("storyboard_review", ctx)
	if err != nil {
		logger.Errorf("[buildStoryboardReviewPrompt] renderPrompt error: %v", err)
		return ""
	}
	return result
}

// parseStoryboardReview 解析 AI 返回的分镜审查报告
func parseStoryboardReview(result string) (*model.StoryboardReview, error) {
	cleaned := extractJSONObject(result)
	var review model.StoryboardReview
	if err := json.Unmarshal([]byte(cleaned), &review); err != nil {
		// DeepSeek 等模型有时在 JSON 字段间插入中文注释，导致 0xE8（è）等非 ASCII 字节
		// 出现在期望逗号/}的位置。尝试修复：移除字符串外非 ASCII 内容 + 补全缺失逗号。
		repaired := repairAIJSON(cleaned)
		if err2 := json.Unmarshal([]byte(repaired), &review); err2 != nil {
			return nil, fmt.Errorf("解析审查报告失败: %w; AI响应(前300字符): %.300s", err, result)
		}
		logger.Errorf("[parseStoryboardReview] JSON repaired successfully (original err: %v)", err)
	}
	return &review, nil
}

// shotOptimizeUpdate 表示 AI 返回的单个镜头优化更新
type shotOptimizeUpdate struct {
	ShotNo      int     `json:"shot_no"`
	Description string  `json:"description"`
	Narration   *string `json:"narration"` // 指针：区分"AI未提及此字段"(nil)与"AI显式清空"(指向"")，避免误删未提及字段
	Dialogue    *string `json:"dialogue"`  // 同上
	CameraType  string  `json:"camera_type"`
	Duration    float64 `json:"duration"`
	Transition  string  `json:"transition"`
	SFXTags     string  `json:"sfx_tags"` // gen_meta.sfx_tags
}

// buildStoryboardOptimizePrompt 构建分镜优化提示词
func buildStoryboardOptimizePrompt(shots []*model.StoryboardShot, review *model.StoryboardReview, videoMode string) string {
	trunc := func(s string, max int) string {
		r := []rune(s)
		if len(r) > max {
			return string(r[:max]) + "…"
		}
		return s
	}

	// 预格式化分镜数据，包含提示词内容供 AI 评估和改写
	var sb strings.Builder
	for _, sh := range shots {
		sb.WriteString(fmt.Sprintf("[镜%d] 时长:%.0fs 镜头:%s",
			sh.ShotNo, sh.Duration, sh.CamDir.CameraType))
		// description 现在是 AI 出图/出视频的唯一依据，截断长度放宽以便 AI 评估现有质量并决定是否改写
		if desc := trunc(sh.Description, 250); desc != "" {
			sb.WriteString(fmt.Sprintf("\n  描述: %s", desc))
		}
		if narr := trunc(sh.Narration(), 60); narr != "" {
			sb.WriteString(fmt.Sprintf("\n  旁白: %s", narr))
		}
		if dial := trunc(sh.Dialogue(), 60); dial != "" {
			sb.WriteString(fmt.Sprintf("\n  对白: %s", dial))
		}
		sb.WriteString("\n")
	}

	feedbackData := make([]map[string]interface{}, 0, len(review.ShotFeedback))
	for _, fb := range review.ShotFeedback {
		feedbackData = append(feedbackData, map[string]interface{}{
			"ShotNo":               fb.ShotNo,
			"Severity":             fb.Severity,
			"Issues":               fb.Issues,
			"Suggestion":           fb.Suggestion,
			"SuggestedNarration":   fb.SuggestedNarration,
			"SuggestedDialogue":    fb.SuggestedDialogue,
			"SuggestedDescription": fb.SuggestedDescription,
		})
	}

	ctx := map[string]interface{}{
		"ShotCount":         len(shots),
		"ShotsText":         sb.String(),
		"GlobalSuggestions": review.GlobalSuggestions,
		"ShotFeedback":      feedbackData,
		"VideoMode":         videoMode,
	}
	result, err := renderPrompt("storyboard_optimize", ctx)
	if err != nil {
		logger.Errorf("[buildStoryboardOptimizePrompt] renderPrompt error: %v", err)
		return ""
	}
	return result
}

// parseOptimizedShots 解析 AI 返回的优化镜头列表
func parseOptimizedShots(result string) ([]shotOptimizeUpdate, error) {
	cleaned := extractJSONObject(result) // optimize 模板返回 {"optimized_shots":[...]} 对象，不能用 extractJSON（会剥离外层对象只保留内部数组）
	var resp struct {
		OptimizedShots []shotOptimizeUpdate `json:"optimized_shots"`
	}
	if err := json.Unmarshal([]byte(cleaned), &resp); err != nil {
		return nil, fmt.Errorf("%w; AI原始响应(前300字符): %.300s", err, result)
	}
	return resp.OptimizedShots, nil
}

// OptimizeStoryboardFromReview 根据审查报告批量优化分镜
func (s *VideoService) OptimizeStoryboardFromReview(tenantID, videoID uint, review *model.StoryboardReview, provider string) (int, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("获取分镜失败: %w", err)
	}
	if len(shots) == 0 {
		return 0, fmt.Errorf("该视频暂无分镜")
	}

	// 取 Video.NovelID / Mode 以便 GenerateWithProvider 能通过小说级 AI 模型配置选择 provider，并区分优化规则
	var novelID uint
	var videoMode string
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
		videoMode = video.Mode
	}

	// 保存优化前快照（用于 rollback）
	if s.reviewRecordRepo != nil {
		snapshotData, _ := json.Marshal(shots)
		snap := &model.ReviewRecord{
			NovelID:      novelID,
			EntityType:   model.ReviewEntityStoryboard,
			EntityID:     videoID,
			OverallScore: review.OverallScore,
			ReviewJSON:   string(snapshotData),
			Status:       "snapshot",
		}
		if saveErr := s.reviewRecordRepo.Create(snap); saveErr != nil {
			logger.Errorf("OptimizeStoryboardFromReview: save snapshot failed: %v", saveErr)
		}
	}

	prompt := buildStoryboardOptimizePrompt(shots, review, videoMode)

	result, err := s.aiService.GenerateWithProvider(tenantID, "storyboard_optimize", prompt)
	if err != nil {
		return 0, fmt.Errorf("AI优化失败: %w", err)
	}

	updates, err := parseOptimizedShots(result)
	if err != nil {
		return 0, fmt.Errorf("解析优化结果失败: %w", err)
	}

	// 构建 ShotNo → Shot 映射，批量更新
	shotMap := make(map[int]*model.StoryboardShot, len(shots))
	for _, sh := range shots {
		shotMap[sh.ShotNo] = sh
	}

	updatedCount := 0
	for _, upd := range updates {
		sh, ok := shotMap[upd.ShotNo]
		if !ok {
			logger.Printf("OptimizeStoryboardFromReview: shot_no=%d not found, skipping", upd.ShotNo)
			continue
		}
		// Never overwrite approved or locked shots
		if sh.Status == "approved" || sh.Status == "locked" {
			logger.Printf("OptimizeStoryboard: skipping approved shot %d", sh.ShotNo)
			continue
		}

		// 按照 UpdateShotPartial 的正确模式：gen_meta 和 cam_dir 是 serializer:json 列，
		// GORM map Updates 不走 serializer，必须手动将子字段合并到对应 JSON 列后序列化整列写入。
		safeFields := make(map[string]interface{})
		updatedGenMeta := false
		updatedCamDir := false

		// 直接列
		if upd.Description != "" {
			safeFields["description"] = upd.Description
		}
		if upd.Duration > 0 {
			safeFields["duration"] = upd.Duration
		}
		// narration/dialogue：AI 遵循"只输出实质改动字段"的指令时，未提及的字段在 JSON 里
		// 是完全缺失的 key，而不是空字符串——用指针类型区分"未提及"(nil)与"显式清空"(指向"")，
		// 分别独立判断是否更新，避免只改其中一个字段时把另一个未提及的字段误清空。
		// narration/dialogue 现合并存储于 gen_meta.voice_lines，须走 JSON 列合并路径。
		if upd.Narration != nil {
			sh.SetNarration(*upd.Narration)
			updatedGenMeta = true
		}
		if upd.Dialogue != nil {
			sh.SetDialogue(*upd.Dialogue)
			updatedGenMeta = true
		}

		// cam_dir JSON 列子字段
		if upd.CameraType != "" {
			sh.CamDir.CameraType = upd.CameraType
			updatedCamDir = true
		}
		if upd.Transition != "" {
			sh.CamDir.Transition = upd.Transition
			updatedCamDir = true
		}

		// gen_meta JSON 列子字段（提示词，P0 Fix 2）
		if upd.SFXTags != "" {
			sh.GenMeta.SFXTags = upd.SFXTags
			updatedGenMeta = true
		}

		// 序列化修改过的 JSON 列
		if updatedCamDir {
			camDirJSON, _ := json.Marshal(sh.CamDir)
			safeFields["cam_dir"] = string(camDirJSON)
		}
		if updatedGenMeta {
			genMetaJSON, _ := json.Marshal(sh.GenMeta)
			safeFields["gen_meta"] = string(genMetaJSON)
		}

		if len(safeFields) == 0 {
			continue
		}
		if err := s.storyboardRepo.UpdateFields(sh.ID, safeFields); err != nil {
			logger.Errorf("OptimizeStoryboardFromReview: update shot %d failed: %v", sh.ShotNo, err)
			continue
		}
		updatedCount++
	}

	logger.Printf("OptimizeStoryboardFromReview: videoID=%d updated=%d/%d shots", videoID, updatedCount, len(updates))
	return updatedCount, nil
}

// ShotApplyDiff 表示单个分镜的差异数据（用于 ApplyStoryboardDiffs）
type ShotApplyDiff struct {
	ShotNo int                    `json:"shot_no"`
	Fields map[string]interface{} `json:"fields"`
}

// ApplyStoryboardDiffs 将一组差异批量应用到分镜（用于前端 diff 预览后的确认提交）
func (s *VideoService) ApplyStoryboardDiffs(videoID uint, diffs []ShotApplyDiff, recordID uint) (int, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("获取分镜失败: %w", err)
	}

	// 构建 ShotNo → Shot 映射
	shotMap := make(map[int]*model.StoryboardShot, len(shots))
	for _, sh := range shots {
		shotMap[sh.ShotNo] = sh
	}

	applied := 0
	for _, diff := range diffs {
		sh, ok := shotMap[diff.ShotNo]
		if !ok {
			continue
		}
		if len(diff.Fields) == 0 {
			continue
		}
		if err := s.storyboardRepo.UpdateFields(sh.ID, diff.Fields); err != nil {
			logger.Errorf("ApplyStoryboardDiffs: update shot %d failed: %v", diff.ShotNo, err)
			continue
		}
		applied++
	}

	// 标记审查记录为已应用，并记录应用时间
	if s.reviewRecordRepo != nil && recordID > 0 {
		if rec, err := s.reviewRecordRepo.GetByID(recordID); err == nil {
			now := time.Now()
			rec.Status = "applied"
			rec.AppliedAt = &now
			if err2 := s.reviewRecordRepo.Update(rec); err2 != nil {
				logger.Errorf("ApplyStoryboardDiffs: update record %d status failed: %v", recordID, err2)
			}
		}
	}

	// 审查已应用 → 更新 video.review_status 为 reviewed
	if s.videoRepo != nil {
		if err := s.videoRepo.UpdateFields(videoID, map[string]interface{}{
			"review_status": "reviewed",
		}); err != nil {
			logger.Errorf("ApplyStoryboardDiffs: update video review_status: %v", err)
		}
	}

	return applied, nil
}

// RollbackReview 回滚到指定审查记录的快照状态
func (s *VideoService) RollbackReview(tenantID, videoID, recordID uint) (int, error) {
	if s.reviewRecordRepo == nil {
		return 0, fmt.Errorf("review record repository not initialized")
	}

	rec, err := s.reviewRecordRepo.GetByID(recordID)
	if err != nil {
		return 0, fmt.Errorf("审查记录不存在: %w", err)
	}
	if rec.EntityID != videoID {
		return 0, fmt.Errorf("审查记录不属于该视频")
	}
	if rec.Status != "snapshot" {
		return 0, fmt.Errorf("该记录不是快照，无法回滚")
	}

	// 解析快照中的分镜数据
	var snapshotShots []*model.StoryboardShot
	if err := json.Unmarshal([]byte(rec.ReviewJSON), &snapshotShots); err != nil {
		return 0, fmt.Errorf("快照数据解析失败: %w", err)
	}

	// 获取当前分镜映射（通过 ShotNo 对齐）
	currentShots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("获取当前分镜失败: %w", err)
	}
	currentMap := make(map[int]*model.StoryboardShot, len(currentShots))
	for _, sh := range currentShots {
		currentMap[sh.ShotNo] = sh
	}

	restored := 0
	for _, snap := range snapshotShots {
		current, ok := currentMap[snap.ShotNo]
		if !ok {
			continue
		}
		camDirJSON, _ := json.Marshal(snap.CamDir)
		genMetaJSON, _ := json.Marshal(snap.GenMeta)
		fields := map[string]interface{}{
			"description": snap.Description,
			"narration":   snap.Narration,
			"gen_meta":    string(genMetaJSON), // GORM map updates 不走 serializer，必须手动序列化
			"duration":    snap.Duration,
			"cam_dir":     string(camDirJSON),
		}
		if err := s.storyboardRepo.UpdateFields(current.ID, fields); err != nil {
			logger.Errorf("RollbackReview: update shot %d failed: %v", snap.ShotNo, err)
			continue
		}
		restored++
	}

	// 标记审查记录为已回滚
	if rollbackRec, err := s.reviewRecordRepo.GetByID(recordID); err == nil {
		rollbackRec.Status = "rolled_back"
		if err2 := s.reviewRecordRepo.Update(rollbackRec); err2 != nil {
			logger.Errorf("RollbackReview: update record %d status failed: %v", recordID, err2)
		}
	}

	logger.Printf("RollbackReview: videoID=%d restored=%d shots from record %d", videoID, restored, recordID)
	return restored, nil
}

// ListReviewRecords 列出视频的所有审查记录
func (s *VideoService) ListReviewRecords(videoID uint) ([]*model.ReviewRecord, error) {
	if s.reviewRecordRepo == nil {
		return nil, fmt.Errorf("review record repository not initialized")
	}
	return s.reviewRecordRepo.ListByEntity(model.ReviewEntityStoryboard, videoID)
}

// ─── Ignored suggestions ─────────────────────────────────────────────────────

func issueHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

func (s *VideoService) IgnoreSuggestion(tenantID, videoID uint, shotNo int, issueText string) (*model.IgnoredReviewIssue, error) {
	if s.ignoredSuggestionRepo == nil {
		return nil, fmt.Errorf("ignored suggestion repository not initialized")
	}
	var novelID uint
	if v, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = v.NovelID
	}
	item := &model.IgnoredReviewIssue{
		NovelID:     novelID,
		EntityType:  model.ReviewEntityStoryboard,
		EntityID:    videoID,
		IssueText:   issueText,
		IssueHash:   issueHash(issueText),
		ContextJSON: fmt.Sprintf(`{"shot_no":%d}`, shotNo),
	}
	if err := s.ignoredSuggestionRepo.Create(item); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *VideoService) UnignoreSuggestion(videoID, id uint) error {
	if s.ignoredSuggestionRepo == nil {
		return fmt.Errorf("ignored suggestion repository not initialized")
	}
	items, err := s.ignoredSuggestionRepo.ListByEntity(model.ReviewEntityStoryboard, videoID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID == id {
			return s.ignoredSuggestionRepo.Delete(id)
		}
	}
	return fmt.Errorf("ignored suggestion %d not found for video %d", id, videoID)
}

func (s *VideoService) ListIgnoredSuggestions(videoID uint) ([]*model.IgnoredReviewIssue, error) {
	if s.ignoredSuggestionRepo == nil {
		return nil, fmt.Errorf("ignored suggestion repository not initialized")
	}
	return s.ignoredSuggestionRepo.ListByEntity(model.ReviewEntityStoryboard, videoID)
}

// generateStoryboardArc 在分段生成前调用 AI，从完整章节内容中提取情感弧线骨架。
// 返回 JSON 字符串（storyboard_arc.j2 格式），失败时返回空字符串（不阻塞主流程）。
func (s *VideoService) generateStoryboardArc(content string, characters []*model.Character, tenantID, novelID uint, provider, videoMode string) string {
	if s.aiService == nil {
		return ""
	}
	// 截断过长内容，弧线分析只需把握全局走向
	arcContent := content
	const maxArcRunes = 6000
	if len([]rune(arcContent)) > maxArcRunes {
		arcContent = string([]rune(arcContent)[:maxArcRunes]) + "…（已截断，请基于前段内容推断全章情感弧线）"
	}
	// 整理角色摘要（只传名称+身份+内在矛盾+核心渴望，供弧线分析生成角色化的 dialogue_guide）
	type arcChar struct {
		Name          string
		Role          string
		InnerConflict string
		CoreDesire    string
	}
	var arcChars []arcChar
	for _, c := range characters {
		if c.Meta.InnerConflict != "" || c.Meta.CoreDesire != "" || c.Role == "protagonist" || c.Role == "antagonist" {
			arcChars = append(arcChars, arcChar{
				Name:          c.Name,
				Role:          c.Role,
				InnerConflict: c.Meta.InnerConflict,
				CoreDesire:    c.Meta.CoreDesire,
			})
		}
		if len(arcChars) >= 5 {
			break
		}
	}
	ctx := map[string]interface{}{
		"Content":    arcContent,
		"Characters": arcChars,
		"VideoMode":  videoMode,
	}
	prompt, err := renderPrompt("storyboard_arc", ctx)
	if err != nil {
		logger.Errorf("[Storyboard] generateStoryboardArc renderPrompt: %v", err)
		return ""
	}
	result, err := s.aiService.GenerateWithProvider(tenantID, "storyboard_arc", prompt)
	if err != nil {
		logger.Errorf("[Storyboard] generateStoryboardArc AI call failed: %v", err)
		return ""
	}
	// 提取 JSON（AI 偶尔会包裹 markdown 代码块）
	result = strings.TrimSpace(result)
	if idx := strings.Index(result, "{"); idx > 0 {
		result = result[idx:]
	}
	if idx := strings.LastIndex(result, "}"); idx >= 0 && idx < len(result)-1 {
		result = result[:idx+1]
	}
	return result
}

// voiceLangToDialogueLang 将 Character.VoiceLanguage 转换为模板中展示给 LLM 的台词语言说明。
// TTS 的语言代码（zh/en/ja 等）直接决定对白应该使用什么文字，旁白始终固定为简体中文。
func voiceLangToDialogueLang(vl string) string {
	switch vl {
	case "en":
		return "English"
	case "ja":
		return "日文"
	case "zh-yue":
		return "粤语（中文）"
	default:
		return "简体中文"
	}
}

func resolveStyleQualityTokens(styleID string) string {
	switch resolveStyleCategory(styleID) {
	case "realistic":
		return "masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting"
	case "render_3d":
		return "masterpiece, best quality, ultra-detailed, 3D render, ray tracing, volumetric lighting, high-fidelity 3D"
	case "pixel":
		return "masterpiece, best quality, crisp pixel art, clean sharp pixels, retro game aesthetic"
	case "classic_illustration":
		return "masterpiece, best quality, ultra-detailed, exquisite brushwork, vibrant colors, professional illustration"
	case "dark_stylized":
		return "masterpiece, best quality, ultra-detailed, dramatic atmosphere, vibrant colors, professional digital art"
	default: // anime, unknown
		return "masterpiece, best quality, ultra-detailed, vibrant colors, clean linework, professional illustration"
	}
}

// ─── Ensure unused imports are satisfied ─────────────────────────────────────

var _ *repository.PlotPointRepository // force import of repository package
