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
	"fmt"
	"math"
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
	defaultShotDurationSecs  = 5.0   // 默认分镜时长（秒）
	maxSegmentRunes          = 3500  // 每段最多字符数（约 25 个镜头，≈5000 tokens）
	charRuneOverlapThreshold = 0.7   // 角色名模糊匹配汉字重叠比例阈值（需≥70%重叠，避免"炎少"误匹配"萧炎"）
	shiftTempOffset          = 100000 // 两阶段 shot_no 位移时使用的临时偏移量，避免 MySQL 唯一键冲突
)

// ─── Storyboard Generation ────────────────────────────────────────────────────

// GenerateStoryboard 生成分镜
// userPrompt: 用户自定义提示词（可选），将追加到系统 prompt 之后
// progressFn: 可选的进度回调（0-99），供调用方更新任务进度（传 nil 则跳过）
// CancelStoryboardGeneration 取消正在进行的分镜生成（若有）。
// handler 在提交新任务前调用，让旧 goroutine 尽快退出，避免 "already in progress" 错误。
func (s *VideoService) CancelStoryboardGeneration(videoID uint) {
	if val, ok := s.generatingStoryboard.LoadAndDelete(videoID); ok {
		if cancelFn, ok := val.(context.CancelFunc); ok {
			cancelFn()
		}
	}
}

func (s *VideoService) GenerateStoryboard(videoID uint, provider, userPrompt string, progressFn func(int), overrides StoryboardOverrides, chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
	metrics.StoryboardGenerationInFlight.Inc()
	defer metrics.StoryboardGenerationInFlight.Dec()
	sbStart := time.Now()

	// Prevent concurrent storyboard generation for the same video — across instances via Redis SETNX.
	if s.cache != nil {
		redisKey := fmt.Sprintf("lock:storyboard:gen:%d", videoID)
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", 30*time.Minute).Result()
		if err == nil {
			if !ok {
				metrics.StoryboardGenerationTotal.WithLabelValues("conflict").Inc()
				return nil, fmt.Errorf("storyboard generation already in progress for video %d", videoID)
			}
			defer s.cache.Del(context.Background(), redisKey)
		}
		// err != nil: Redis unavailable, fall through to local sync.Map check
	}

	// Store the cancel function so that CancelStoryboardGeneration can interrupt this goroutine.
	genCtxInner, cancelInner := context.WithCancel(context.Background())
	if _, loaded := s.generatingStoryboard.LoadOrStore(videoID, cancelInner); loaded {
		cancelInner() // discard the context we just created
		metrics.StoryboardGenerationTotal.WithLabelValues("conflict").Inc()
		return nil, fmt.Errorf("storyboard generation already in progress for video %d", videoID)
	}
	defer func() {
		s.generatingStoryboard.Delete(videoID)
		cancelInner()
	}()

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
	if chapterID != nil {
		chapter, chErr := s.chapterRepo.GetByID(*chapterID)
		if chErr != nil {
			logger.Errorf("[Storyboard] GetByID chapterID=%d: %v", *chapterID, chErr)
		}
		if chapter != nil {
			content = chapter.Content
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

	// 并行预取角色、场景锚点、情节点（避免多次串行 DB 查询）
	tenantID := s.videoTenantID(video)
	var characters []*model.Character
	var anchors []*model.SceneAnchor
	var plotPoints []*model.PlotPoint
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

	// 获取小说的 PromptLanguage、Genre 和 ImageStyle（影响分镜 image_prompt 的风格基调）
	promptLanguage := "zh"
	genre := ""
	imageStyle := ""
	if s.novelRepo != nil && video.NovelID > 0 {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
			if novel.AIConfig.PromptLanguage != "" {
				promptLanguage = novel.AIConfig.PromptLanguage
			}
			genre = novel.Meta.Genre
			imageStyle = novel.AIConfig.ImageStyle
		}
	}
	// video 级别的 ArtStyle 作为兜底（novel 未设置时使用）
	if imageStyle == "" {
		imageStyle = video.RenderConfig.ArtStyle
	}

	// 前置：生成情感弧线骨架（轻量调用，用于指导每个分段的叙事节奏）
	arcPlan := s.generateStoryboardArc(content, tenantID, video.NovelID, provider)
	if arcPlan != "" {
		logger.Printf("[Storyboard] arc plan generated (%d chars)", len(arcPlan))
	}

	totalRunes := len([]rune(content))
	totalShots := calcTotalShots(totalRunes, video.RenderConfig.TargetDuration, video.RenderConfig.Pacing)

	// 动态分段：确保每段期望镜头数 ≤ maxShotsPerAICall，防止超出 AI 模型输出 token 上限。
	// 大多数模型输出上限 8192-16384 tokens；每个镜头约 700 tokens；12 镜 × 700 = 8400 tokens。
	const maxShotsPerAICall = 12
	dynSegRunes := maxSegmentRunes
	if totalShots > maxShotsPerAICall && totalRunes > 0 {
		// 使每段镜头数 ≤ maxShotsPerAICall：segRunes = totalRunes * maxShotsPerAICall / totalShots
		dynSegRunes = totalRunes * maxShotsPerAICall / totalShots
		if dynSegRunes < 500 {
			dynSegRunes = 500 // 最小 500 字保证 AI 上下文充足
		}
	}
	segments := splitContentSegments(content, dynSegRunes)

	chIDStr := "nil"
	if chapterID != nil {
		chIDStr = fmt.Sprintf("%d", *chapterID)
	}
	logger.Printf("[Storyboard] start videoID=%d chapterID=%s provider=%q voiceMode=%q totalRunes=%d totalShots=%d dynSegRunes=%d segments=%d chars=%d anchors=%d plotPoints=%d",
		videoID, chIDStr, provider, overrides.VoiceMode, totalRunes, totalShots, dynSegRunes, len(segments), len(characters), len(anchors), len(plotPoints))

	// 顺序处理各段落：每段将上一段末尾 3 个镜头作为 prevShots 传入，
	// 确保跨段落的情节、场景、情绪连贯性（storyboard_generate.j2 中的【上一段末尾分镜】规则生效）。
	// 牺牲并发换取叙事连贯——对于用户可感知的质量提升，这是必要的权衡。
	const prevTailN = 3 // 传递上一段末尾多少个镜头
	type segResult struct {
		shots []*model.StoryboardShot
		err   error
	}
	results := make([]segResult, len(segments))

	genCtx := genCtxInner
	var prevTailShots []*model.StoryboardShot // 上一段末尾镜头，首段为 nil
	// 累积已分配镜头数和已处理字数，避免逐段整除截断导致总数偏少
	shotsAllocated := 0
	runesProcessed := 0

	// Each segment produces 8–15K chars of JSON. A 4096-token limit truncates it.
	// The AI API silently caps max_tokens at the model's own maximum when it exceeds it,
	// so requesting 16384 on a model that only supports 4096 is safe (no API error).
	segOverrides := overrides
	if segOverrides.MaxTokens < 16384 {
		segOverrides.MaxTokens = 16384
	}

	for segIdx, seg := range segments {
		// 检查是否已取消
		select {
		case <-genCtx.Done():
			results[segIdx] = segResult{err: genCtx.Err()}
			// 后续段落也标记取消
			for i := segIdx + 1; i < len(segments); i++ {
				results[i] = segResult{err: genCtx.Err()}
			}
			break
		default:
		}
		if results[segIdx].err != nil {
			break
		}

		segStart := time.Now()
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
		logger.Printf("[Storyboard] seg %d/%d start runes=%d expectedShots=%d prevTail=%d",
			segIdx+1, len(segments), segRunes, segShotCount, len(prevTailShots))

		prompt := s.buildStoryboardPrompt(video, seg, userPrompt, segIdx+1, len(segments), segShotCount,
			characters, anchors, plotPoints, prevTailShots, overrides.VoiceMode, promptLanguage, genre, video.RenderConfig.Pacing, arcPlan, imageStyle)

		var aiResult string
		var aiErr error
		var shots []*model.StoryboardShot
		var bestShots []*model.StoryboardShot // 历次尝试中镜头数最多的结果
		for attempt := 0; attempt < 3; attempt++ {
			p := prompt
			switch attempt {
			case 1:
				p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON 数组，不要包含任何 markdown 代码块（```）或说明文字。"
				logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (format hint)", segIdx+1, len(segments), attempt)
			case 2:
				p = prompt + fmt.Sprintf("\n\n⚠️ 极重要：上一次你只返回了很少的分镜，请务必生成全部%d个分镜，只返回JSON数组不要截断。", segShotCount)
				logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (shot count hint)", segIdx+1, len(segments), attempt)
			}
			aiStart := time.Now()
			aiResult, aiErr = s.aiService.GenerateWithProvider(tenantID, video.NovelID, "storyboard", p, provider, segOverrides)
			aiElapsed := time.Since(aiStart).Round(time.Millisecond)
			if aiErr != nil {
				logger.Errorf("[Storyboard] seg %d/%d attempt=%d AI error elapsed=%s err=%v", segIdx+1, len(segments), attempt, aiElapsed, aiErr)
				if ai.IsTimeoutError(aiErr) {
					break
				}
				continue
			}
			logger.Printf("[Storyboard] seg %d/%d attempt=%d AI ok elapsed=%s responseLen=%d", segIdx+1, len(segments), attempt, aiElapsed, len(aiResult))
			if strings.TrimSpace(aiResult) == "" {
				continue
			}
			parsed, parseErr := s.parseStoryboardResult(videoID, chapterID, aiResult)
			if parseErr != nil {
				logger.Errorf("[Storyboard] seg %d/%d attempt=%d parse failed: %v", segIdx+1, len(segments), attempt, parseErr)
				continue
			}
			// 始终保留历次中镜头数最多的结果
			if len(parsed) > len(bestShots) {
				bestShots = parsed
			}
			if len(parsed) < segShotCount*4/5 && attempt < 2 {
				logger.Printf("[Storyboard] seg %d/%d attempt=%d too few shots got=%d expected=%d (threshold 80%%), retrying", segIdx+1, len(segments), attempt, len(parsed), segShotCount)
				continue
			}
			shots = bestShots
			break
		}
		if shots == nil && len(bestShots) > 0 {
			shots = bestShots // 全部 attempt 均未达标时，仍用最佳部分结果
		}
		if aiErr != nil && shots == nil {
			results[segIdx] = segResult{err: aiErr}
			// 非首段失败不终止，后续段落继续（会缺少 prevTail 但比完全失败好）
			prevTailShots = nil
			continue
		}
		if shots == nil {
			logger.Printf("[Storyboard] seg %d/%d fatal: AI returned empty or unparseable response after all retries", segIdx+1, len(segments))
			results[segIdx] = segResult{err: fmt.Errorf("AI返回空响应，请检查模型配置或更换提供商")}
			prevTailShots = nil
			continue
		}
		logger.Printf("[Storyboard] seg %d/%d done shots=%d elapsed=%s", segIdx+1, len(segments), len(shots), time.Since(segStart).Round(time.Millisecond))
		results[segIdx] = segResult{shots: shots}

		// 取本段末尾 prevTailN 个镜头，传给下一段
		if len(shots) > prevTailN {
			prevTailShots = shots[len(shots)-prevTailN:]
		} else {
			prevTailShots = shots
		}

		if progressFn != nil {
			progressFn((segIdx + 1) * 90 / len(segments))
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
	if progressFn != nil {
		progressFn(95)
	}

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

	logger.Errorf("[Storyboard] finished videoID=%d totalShots=%d segments=%d failedSegs=%d elapsed=%s",
		videoID, len(shots), len(segments), failedSegs, time.Since(totalStart).Round(time.Millisecond))

	sbStatus := "success"
	if failedSegs > 0 {
		sbStatus = "partial"
	}
	metrics.StoryboardGenerationTotal.WithLabelValues(sbStatus).Inc()
	metrics.StoryboardGenerationDuration.Observe(time.Since(sbStart).Seconds())
	metrics.StoryboardShotsGenerated.Observe(float64(len(shots)))

	// 若存在失败段落，返回包含失败信息的 error（不阻止已成功段落的结果）
	var returnErr error
	if failedSegs > 0 && firstErr != nil {
		returnErr = fmt.Errorf("部分段落生成失败（%d/%d），已返回成功段落的分镜: %w", failedSegs, len(segments), firstErr)
	}

	return shots, returnErr
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
				if strings.Contains(text, name) || strings.Contains(name, text) {
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
		if tryAnchor(shot.Narration) {
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

// autoMatchShotCharacters 按多来源匹配小说角色，写入 CharacterIDs。
// 匹配优先级：① shot.Characters JSON → ② shot.GenMeta.Dialogue "角色名：台词" → ③ shot.Narration 关键词扫描。
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

		// ① Characters JSON: [{"name":"..."}]
		if shot.GenMeta.Characters != "" {
			var shotChars []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(shot.GenMeta.Characters), &shotChars); err == nil {
				for _, sc := range shotChars {
					tryMatch(sc.Name, seen, &matched)
				}
			} else {
				logger.Printf("[AutoMatch] char: shot#%d Characters JSON parse err: %v (raw=%q)", shot.ShotNo, err, shot.GenMeta.Characters)
			}
		}

		// ② Dialogue: "角色名：台词" — 提取冒号前的角色名
		if len(matched) == 0 && shot.GenMeta.Dialogue != "" {
			for _, sep := range []string{"：", ":"} {
				if idx := strings.Index(shot.GenMeta.Dialogue, sep); idx > 0 {
					name := shot.GenMeta.Dialogue[:idx]
					if len([]rune(name)) <= 10 { // 合理的角色名长度
						tryMatch(name, seen, &matched)
					}
					break
				}
			}
		}

		// ③ Narration: 全文扫描角色名关键词
		if len(matched) == 0 && shot.Narration != "" {
			narrLower := strings.ToLower(shot.Narration)
			for name, id := range nameMap {
				if strings.Contains(narrLower, name) && !seen[id] {
					matched = append(matched, id)
					seen[id] = true
				}
			}
		}

		// ④ Description: 全文扫描角色名关键词（兜底）
		if len(matched) == 0 && shot.Description != "" {
			descLower := strings.ToLower(shot.Description)
			for name, id := range nameMap {
				if strings.Contains(descLower, name) && !seen[id] {
					matched = append(matched, id)
					seen[id] = true
				}
			}
		}

		if len(matched) > 0 {
			shot.CharacterIDs = matched
			charMatchCount++
			logger.Printf("[AutoMatch] char: shot#%d → charIDs=%v (chars_json=%q)", shot.ShotNo, []uint(matched), shot.GenMeta.Characters)
		} else {
			logger.Printf("[AutoMatch] char: shot#%d no match (chars_json=%q)", shot.ShotNo, shot.GenMeta.Characters)
		}
	}
	logger.Printf("[AutoMatch] char: matched %d/%d shots", charMatchCount, len(shots))
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

// calcTotalShots 按目标时长+节奏计算全章期望分镜总数。
// targetDuration=0 时按字数密度+节奏估算（自动模式）。
func calcTotalShots(totalRunes, targetDuration int, pacing string) int {
	// 各节奏对应平均单镜时长（秒）
	avgSec := map[string]int{"slow": 8, "normal": 5, "fast": 3}
	s, ok := avgSec[pacing]
	if !ok {
		s = 4 // 自动节奏：比 normal(5s) 略快，镜头更丰富
	}

	if targetDuration > 0 {
		n := targetDuration / s
		if n < 3 {
			n = 3
		}
		return n
	}

	// 自动模式：先估算视频时长，再折算分镜数。
	// 汉字阅读速度约 300 字/分钟，视频精炼比约 0.5（10 分钟文章 → 5 分钟视频）。
	// 视频时长（秒）= totalRunes / 300 * 60 * 0.5 = totalRunes / 10
	estimatedSecs := totalRunes / 10
	n := estimatedSecs / s
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

// buildStoryboardPrompt 构建分镜提示词（含截断保护、角色信息、段落上下文）
// segNo/totalSegs 为分段编号（从 1 开始），单段调用时传 1, 1。
// expectedShots 为本段期望分镜数，由调用方通过 calcTotalShots 计算后传入。
// characters/anchors/plotPoints 为调用方预取的数据（避免每段重复查 DB）。
// prevShots: 上一段落末尾的 N 个分镜，用于跨段落情节连贯（顺序处理时传入，并发时为 nil）。
func (s *VideoService) buildStoryboardPrompt(
	video *model.Video, content, userPrompt string,
	segNo, totalSegs, expectedShots int,
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	prevShots []*model.StoryboardShot,
	voiceMode string,
	promptLanguage string,
	genre string,
	pacing string,
	arcPlan string,
	imageStyle string,
) string {
	// isEn / isImageEn 均由 novel.AIConfig.PromptLanguage 决定，与项目「AI 提示词的语言」设置保持一致。
	// image_prompt 在生图前会经过自动翻译（translatePromptToEnglish），保持中文可供用户编辑。
	isEn := promptLanguage == "en"
	isImageEn := promptLanguage == "en"

	segLabel := ""
	if totalSegs > 1 {
		segLabel = fmt.Sprintf("（第%d段，共%d段）", segNo, totalSegs)
	}

	// 预加载角色的默认形象 VisualPrompt（供 storyboard_generate.j2 中 [image_prompt 外貌关键词参考] 使用）
	charVisualPrompts := make(map[uint]string) // charID → VisualPrompt
	if s.lookRepo != nil && len(characters) > 0 {
		for _, c := range characters {
			if c.DefaultLookID != 0 {
				if look, err := s.lookRepo.GetByID(c.DefaultLookID); err == nil && look != nil && look.VisualPrompt != "" {
					charVisualPrompts[c.ID] = look.VisualPrompt
				}
			}
		}
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
				"Name":         c.Name,
				"Role":         c.Role,
				"Description":  c.Description,
				"VisualPrompt": charVisualPrompts[c.ID], // 来自 CharacterLook.VisualPrompt（默认形象的英文视觉提示词）
				"DialogueLang": voiceLangToDialogueLang(c.VoiceConfig.VoiceLanguage),
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
		narrOrDesc := ps.Narration
		if narrOrDesc == "" {
			narrOrDesc = ps.Description
		}
		prevShotsData = append(prevShotsData, map[string]interface{}{
			"ShotNo":        ps.ShotNo,
			"NarrOrDesc":    narrOrDesc,
			"Dialogue":      ps.GenMeta.Dialogue,
			"EmotionalTone": ps.CamDir.EmotionalTone,
			"ShotSize":      ps.CamDir.ShotSize,
			"CameraType":    ps.CamDir.CameraType,
			"Location":      extractLocationFromScene(ps.GenMeta.Scene),
		})
	}

	// 截断内容（rune 级别，避免 byte-level slice 截断 UTF-8 汉字）
	// 3500 rune × 3 bytes = 10500 bytes；原 [:10000] 会漏掉末尾约 167 字
	if cr := []rune(content); len(cr) > 3200 {
		content = string(cr[:3200]) + "…（已截断）"
	}

	expectedShotsMinus2 := expectedShots - 2
	if expectedShotsMinus2 < 0 {
		expectedShotsMinus2 = 0
	}

	ctx := map[string]interface{}{
		"IsEn":                isEn,
		"IsImageEn":           isImageEn,
		"SegLabel":            segLabel,
		"ExpectedShots":       expectedShots,
		"ExpectedShotsMinus2": expectedShotsMinus2,
		"VoiceMode":           voiceMode,
		"Pacing":              pacing,
		"AutoDuration":        video.RenderConfig.TargetDuration == 0,
		"PrevShots":           prevShotsData,
		"Characters":          matchedChars,
		"Anchors":             matchedAnchors,
		"PlotPoints":          ppData,
		"Content":             content,
		"UserPrompt":          userPrompt,
		"ArcPlan":             arcPlan,
		"GenreVisualHints":    genreVisualHints(genre),
		// ImageStyleHint: 画面风格的英文提示词，告知 LLM 生成 image_prompt 时必须使用的风格基调。
		// 不传原始 imageStyle ID（如 "anime"），而是传对 LLM 最有指导意义的英文描述词。
		"ImageStyleHint": resolveStyleIllustrationDesc(imageStyle),
		"ImageStyleID":   imageStyle,
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
		ShotNo         int     `json:"shot_no"`
		Description    string  `json:"description"`
		ImagePrompt    string  `json:"image_prompt"`
		VideoPrompt    string  `json:"video_prompt"`
		NegativePrompt string  `json:"negative_prompt"`
		Narration      string  `json:"narration"`
		Dialogue       string  `json:"dialogue"`
		CameraType     string  `json:"camera_type"`
		CameraAngle    string  `json:"camera_angle"`
		ShotSize       string  `json:"shot_size"`
		Duration       float64 `json:"duration"`
		Location       string  `json:"location"`
		TimeOfDay      string  `json:"time_of_day"`
		Weather        string  `json:"weather"`
		Lighting       string  `json:"lighting"`
		Characters     []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
			Pose       string `json:"pose"`
		} `json:"characters"`
		Subtitle      string `json:"subtitle"`       // 画面叠字：时间词或关键字幕（非空时叠加在视频画面上）
		Transition    string `json:"transition"`
		TransitionOut string `json:"transition_out"` // 本镜头如何结束（画面状态/角色动作/镜头动势）
		TransitionIn  string `json:"transition_in"`  // 本镜头如何开始（衔接上一镜头 TransitionOut）
		SFXTags       []struct {
			Tag     string `json:"tag"`
			SFXType string `json:"type"`
			Prompt  string `json:"prompt"`
		} `json:"sfx_tags"`
		EmotionalTone string `json:"emotional_tone"`
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

		// 将角色信息序列化为 JSON 存储
		var charsJSON string
		if len(r.Characters) > 0 {
			if b, err := json.Marshal(r.Characters); err == nil {
				charsJSON = string(b)
			}
		}

		// 将场景配置序列化
		scene := map[string]string{
			"location":    r.Location,
			"time_of_day": r.TimeOfDay,
			"weather":     r.Weather,
			"lighting":    r.Lighting,
		}
		var sceneJSON string
		if b, err := json.Marshal(scene); err == nil {
			sceneJSON = string(b)
		}

		// image_prompt: 优先使用 LLM 生成的专业图片提示词，兜底用 description 拼接
		imagePrompt := r.ImagePrompt
		if imagePrompt == "" {
			imagePrompt = r.Description
			if r.CameraAngle != "" || r.ShotSize != "" {
				imagePrompt = fmt.Sprintf("%s, %s shot, %s angle", r.Description, r.ShotSize, r.CameraAngle)
			}
			if r.Lighting != "" {
				imagePrompt += ", " + r.Lighting + " lighting"
			}
		}
		// LLM 有时会漏掉质量词，在存储前统一补齐，确保 UI 展示和生图时均包含画质词
		if !strings.Contains(strings.ToLower(imagePrompt), "masterpiece") {
			imagePrompt += ", masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting"
		}

		// video_prompt: 优先使用 LLM 生成的专业视频提示词，兜底用 buildMotionPrompt 生成
		cameraType := validCameraType(r.CameraType)
		cameraAngle := validCameraAngle(r.CameraAngle)
		shotSize := validShotSize(r.ShotSize)
		videoPrompt := r.VideoPrompt
		if videoPrompt == "" {
			videoPrompt = buildMotionPrompt(&model.StoryboardShot{
				CamDir: model.ShotCamDir{
					CameraType:  cameraType,
					CameraAngle: cameraAngle,
					ShotSize:    shotSize,
				},
				Description: r.Description,
			})
		}

		// 将音效标签序列化存储
		var sfxTagsJSON string
		if len(r.SFXTags) > 0 {
			if b, err := json.Marshal(r.SFXTags); err == nil {
				sfxTagsJSON = string(b)
			}
		}

		shot := &model.StoryboardShot{
			UUID:        uuid.New().String(),
			VideoID:     videoID,
			ChapterID:   chapterID,
			ShotNo:      shotNo,
			Description: r.Description,
			Narration:   r.Narration,
			Duration:    duration,
			CamDir: model.ShotCamDir{
				CameraType:    cameraType,
				CameraAngle:   cameraAngle,
				ShotSize:      shotSize,
				Transition:    validTransition(r.Transition),
				TransitionOut: r.TransitionOut,
				TransitionIn:  r.TransitionIn,
				EmotionalTone: r.EmotionalTone,
			},
			GenMeta: model.ShotGenMeta{
				Prompt:         imagePrompt,
				MotionPrompt:   videoPrompt,
				NegativePrompt: r.NegativePrompt,
				Characters:     charsJSON,
				Scene:          sceneJSON,
				SFXTags:        sfxTagsJSON,
				Subtitle:       r.Subtitle,
				Dialogue:       r.Dialogue,
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
		"wipe": true, "push": true, "slide": true,
	}
	if valid[t] {
		return t
	}
	return "cut"
}

// buildMotionPrompt 根据分镜信息构建运动提示词（降级方案，LLM 生成的 video_prompt 优先）
func buildMotionPrompt(shot *model.StoryboardShot) string {
	motionMap := map[string]string{
		"static":     "locked-off camera with organic micro-stabilization, imperceptible ±0.3px breathing drift, no intentional camera movement",
		"push":       "slow cinematic dolly-push at 0.2x speed closing distance to subject by approximately 30% over shot duration, horizon held stable",
		"pull":       "steady dolly-pull at 0.15x speed, environment expanding outward from center frame as distance increases",
		"pan":        "smooth horizontal camera pan at 12°/sec, horizon line held level throughout, subject tracking smoothly from left-to-right",
		"track":      "smooth tracking shot following primary subject at constant distance, camera maintaining framing as subject moves",
		"crane_up":   "slow vertical crane rise at 0.1m/sec, horizon line gradually descending in frame, environment expanding into view below",
		"crane_down": "slow vertical crane descent at 0.1m/sec, camera closing toward ground, horizon rising in frame",
		"follow":     "handheld follow shot with subtle organic stabilization tracking subject, slight natural camera float maintaining close framing",
		"arc":        "smooth arc shot orbiting around subject at constant radius, background environment rotating behind while subject stays centered",
		"tilt":       "smooth vertical camera tilt revealing scene from top to bottom at steady pace, maintaining horizontal level throughout",
		"whip_pan":   "fast whip pan transition sweeping frame left-to-right in 0.3s, motion blur trail visible during sweep, settling to stable frame",
		"zoom":       "smooth optical zoom closing in on subject, focal length increasing gradually over shot duration, background compression effect visible",
	}
	motion := motionMap[shot.CamDir.CameraType]
	if motion == "" {
		motion = "smooth camera movement with organic micro-stabilization, imperceptible breathing drift"
	}

	// 从 shot.GenMeta.Scene 中提取时间/天气用于大气描述
	timeOfDay := "daytime"
	if shot.GenMeta.Scene != "" {
		var sceneData map[string]string
		if err := json.Unmarshal([]byte(shot.GenMeta.Scene), &sceneData); err == nil {
			if tod := sceneData["time_of_day"]; tod != "" {
				timeOfDay = tod
			}
		}
	}

	atmos := "ambient dust motes drifting slowly in available light shafts at 0.3cm/sec"
	if strings.Contains(timeOfDay, "night") || strings.Contains(timeOfDay, "dusk") || strings.Contains(timeOfDay, "evening") {
		atmos = "subtle ground-level mist at 5-10cm height drifting slowly, torch or moonlight casting soft moving shadows"
	}

	shotSizeDesc := map[string]string{
		"extreme_close_up": "extreme close-up framing, subject fills frame edge-to-edge",
		"close_up":         "close-up framing, subject fills majority of frame",
		"medium":           "medium shot framing, subject and immediate environment both visible",
		"wide":             "wide shot framing, broad environment context dominates",
		"extreme_wide":     "extreme wide shot, vast environment with subject small in frame",
		"full":             "full-body shot framing, subject visible head to toe",
	}
	framing := shotSizeDesc[shot.CamDir.ShotSize]
	if framing == "" {
		framing = "standard shot framing"
	}

	desc := shot.Description
	if len([]rune(desc)) > 150 {
		runes := []rune(desc)
		desc = string(runes[:150])
	}

	return fmt.Sprintf("cinematic sequence, professional cinematography, %s, %s — scene: %s — atmosphere: %s — "+
		"lighting holds stable throughout with natural subtle variation, shadows maintain direction, "+
		"no abrupt scene changes, smooth temporal consistency from first to last frame",
		motion, framing, desc, atmos)
}

// qualityTierImageParams 返回图片生成质量档位对应的参数（宽度、步数、CFG scale）
func qualityTierImageParams(tier string) (width, steps int, cfgScale float64) {
	switch tier {
	case "draft":
		return 512, 20, 6.0
	case "preview":
		return 768, 25, 7.0
	case "production":
		return 1024, 35, 7.5
	case "master":
		return 2048, 50, 8.0
	case "ultra": // alias for master; reserved for future upscaling pipeline
		return 2048, 50, 8.5
	default:
		return 1024, 30, 7.5
	}
}

// imageAspectRatioToSize 根据宽高比和质量档位计算 "WxH" 图片尺寸。
// base 为较长边像素值，按 8 取整以兼容大多数图生图 API 的对齐要求。
// 同时保证总像素数不低于 seedreamMinPixels（921600 = 960×960），满足 Seedream 4.0 基础要求。
// Seedream 5.0 更高的像素下限（3686400）由 doubao.go 的 seedreamEnforceMinSize 按模型版本动态处理。
const seedreamMinPixels = 921600

func imageAspectRatioToSize(aspectRatio, qualityTier string) string {
	base, _, _ := qualityTierImageParams(qualityTier)
	if base == 0 {
		base = 1024
	}
	r8 := func(n int) int { return (n + 4) / 8 * 8 }
	var w, h int
	switch aspectRatio {
	case "16:9":
		w, h = base, r8(base*9/16)
	case "9:16":
		w, h = r8(base*9/16), base
	case "4:3":
		w, h = base, r8(base*3/4)
	case "3:4":
		w, h = r8(base*3/4), base
	case "21:9":
		w, h = base, r8(base*9/21)
	default: // 1:1 or unknown
		w, h = base, base
	}
	// Enforce Seedream minimum pixel count by scaling up proportionally.
	if w*h < seedreamMinPixels {
		scale := math.Sqrt(float64(seedreamMinPixels) / float64(w*h))
		w = r8(int(math.Ceil(float64(w) * scale)))
		h = r8(int(math.Ceil(float64(h) * scale)))
	}
	return fmt.Sprintf("%dx%d", w, h)
}

// colorGradeToPromptKeyword 将色彩调色配置映射为 prompt 关键词，注入图片/视频生成 prompt。
func colorGradeToPromptKeyword(grade string) string {
	switch grade {
	case "cinematic":
		return "cinematic color grading, teal and orange, high contrast film look"
	case "warm":
		return "warm color tones, golden warm lighting"
	case "cool":
		return "cool color tones, cool blue atmosphere"
	case "teal_orange":
		return "teal and orange color grading"
	case "vintage":
		return "vintage film look, faded warm colors, retro tone"
	case "noir":
		return "film noir, dramatic high-contrast shadows, moody lighting"
	default:
		return ""
	}
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

// validCameraAngle 验证摄像机角度，无效时返回默认值 eye_level
func validCameraAngle(a string) string {
	valid := map[string]bool{
		"eye_level": true, "high": true, "low": true,
		"dutch": true, "overhead": true, "POV": true,
	}
	if valid[a] {
		return a
	}
	return "eye_level"
}

// validShotSize 验证景别，无效时返回默认值 medium
func validShotSize(s string) string {
	valid := map[string]bool{
		"extreme_wide": true, "wide": true, "full": true,
		"medium": true, "close_up": true, "extreme_close_up": true,
	}
	if valid[s] {
		return s
	}
	return "medium"
}

// ─── Storyboard CRUD ──────────────────────────────────────────────────────────

// GetStoryboard 获取分镜列表
func (s *VideoService) GetStoryboard(videoID uint) ([]*model.StoryboardShot, error) {
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

// UpdateShot 更新分镜
func (s *VideoService) UpdateShot(id uint, req *model.StoryboardShot) (*model.StoryboardShot, error) {
	shot, err := s.storyboardRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.CamDir.CameraType != "" {
		shot.CamDir.CameraType = req.CamDir.CameraType
	}
	if req.CamDir.CameraAngle != "" {
		shot.CamDir.CameraAngle = req.CamDir.CameraAngle
	}
	if req.CamDir.ShotSize != "" {
		shot.CamDir.ShotSize = req.CamDir.ShotSize
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
// 允许的字段：description, narration, dialogue, subtitle, camera_type, camera_angle,
// shot_size, duration, emotional_tone, transition, status, generation_mode.
func (s *VideoService) UpdateShotPartial(id uint, fields map[string]interface{}) (*model.StoryboardShot, error) {
	allowed := map[string]bool{
		"description": true, "narration": true, "dialogue": true, "subtitle": true,
		"camera_type": true, "camera_angle": true, "shot_size": true, "duration": true,
		"emotional_tone": true, "transition": true, "status": true, "generation_mode": true,
		"image_url": true,
		"prompt": true, "motion_prompt": true,
		"transition_out": true, "transition_in": true,
	}
	safe := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		if allowed[k] {
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

// InsertShot 在 afterShotNo 之后插入一个空分镜（afterShotNo=0 表示插入到最前；afterShotNo<0 表示追加到末尾）
func (s *VideoService) InsertShot(videoID uint, afterShotNo int, narration, description string, duration float64) (*model.StoryboardShot, error) {
	appendToEnd := afterShotNo < 0
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	shot := &model.StoryboardShot{
		VideoID:     videoID,
		UUID:        uuid.New().String(),
		Narration:   narration,
		Description: description,
		Duration:    duration,
		CamDir: model.ShotCamDir{
			CameraType:  "static",
			CameraAngle: "eye_level",
			ShotSize:    "medium",
			Transition:  "cut",
		},
		Status: "pending",
	}
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
		VideoID:       src.VideoID,
		ChapterID:     src.ChapterID,
		UUID:          uuid.New().String(),
		Description:   src.Description,
		Narration:     src.Narration,
		Duration:      src.Duration,
		CamDir:        src.CamDir,
		GenMeta:       src.GenMeta,
		SceneAnchorID: src.SceneAnchorID,
		CharacterIDs:  src.CharacterIDs,
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
func (s *VideoService) ReviewStoryboard(tenantID, videoID uint, provider string, previousScore float64) (*model.StoryboardReview, uint, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return nil, 0, fmt.Errorf("获取分镜失败: %w", err)
	}
	if len(shots) == 0 {
		return nil, 0, fmt.Errorf("该视频暂无分镜，请先生成分镜脚本")
	}

	// 取 Video.NovelID / ChapterID 以便选择 provider 并注入章节原文
	var novelID uint
	var chapterContent string
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
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

	prompt := buildStoryboardReviewPrompt(shots, chapterContent, previousScore, previousFeedback, ignoredItems)

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "storyboard_review", prompt, provider)
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
		ins   model.ShotInsertSuggestion
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
		// 对白镜头：narration 留空，由 dialogue 填充
		insertNarration := ins.Narration
		if ins.Dialogue != "" {
			insertNarration = ""
		}
		shot, err := s.InsertShot(videoID, ins.AfterShotNo, insertNarration, ins.Description, ins.Duration)
		if err != nil {
			return count, fmt.Errorf("insert after shot %d: %w", ins.AfterShotNo, err)
		}
		// Apply optional fields from the suggestion
		fields := map[string]interface{}{}
		if ins.Dialogue != "" {
			fields["dialogue"] = ins.Dialogue
		}
		if ins.ShotSize != "" {
			fields["shot_size"] = ins.ShotSize
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
func buildStoryboardReviewPrompt(shots []*model.StoryboardShot, chapterContent string, previousScore float64, previousFeedback []model.ShotReviewFeedback, ignoredItems []*model.IgnoredReviewIssue) string {
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
		sb.WriteString(fmt.Sprintf("[镜%d] 景别:%-12s 时长:%4.1fs 运镜:%-8s 角度:%-10s",
			shot.ShotNo, shot.CamDir.ShotSize, shot.Duration, shot.CamDir.CameraType, shot.CamDir.CameraAngle))
		if shot.CamDir.EmotionalTone != "" {
			sb.WriteString(fmt.Sprintf(" 情绪:%s", truncate(shot.CamDir.EmotionalTone, 12)))
		}
		if desc := truncate(shot.Description, 50); desc != "" {
			sb.WriteString(fmt.Sprintf("\n      描述: %s", desc))
		}
		if narr := truncate(shot.Narration, 80); narr != "" {
			sb.WriteString(fmt.Sprintf("\n      旁白: %s", narr))
		}
		if shot.GenMeta.Dialogue != "" {
			sb.WriteString(fmt.Sprintf("\n      台词: %s", truncate(shot.GenMeta.Dialogue, 50)))
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

	// 截断章节原文（防止过长撑爆上下文，保留前 3000 字）
	truncatedChapter := chapterContent
	if runes := []rune(truncatedChapter); len(runes) > 3000 {
		truncatedChapter = string(runes[:3000]) + "…（已截断）"
	}

	ctx := map[string]interface{}{
		"ShotCount":          len(shots),
		"ShotsText":          sb.String(),
		"HasChapterContent":  truncatedChapter != "",
		"ChapterContent":     truncatedChapter,
		"HasPreviousScore":   false, // P2-3: 不注入历史评分，避免 AI 锚定偏差影响本次独立评估
		"PreviousScoreStr":   "",
		"HasPreviousFixed":   len(prevFixedLines) > 0,
		"PreviousFixedText":  strings.Join(prevFixedLines, "\n"),
		"HasIgnored":         len(ignoredLines) > 0,
		"IgnoredText":        strings.Join(ignoredLines, "\n"),
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
	ShotNo        int     `json:"shot_no"`
	Description   string  `json:"description"`
	Narration     string  `json:"narration"`
	Dialogue      string  `json:"dialogue"`
	CameraType    string  `json:"camera_type"`
	CameraAngle   string  `json:"camera_angle"`
	ShotSize      string  `json:"shot_size"`
	Duration      float64 `json:"duration"`
	EmotionalTone string  `json:"emotional_tone"`
	Transition    string  `json:"transition"`
	TransitionOut string  `json:"transition_out"`
	TransitionIn  string  `json:"transition_in"`
}

func (u shotOptimizeUpdate) toFieldMap() map[string]interface{} {
	m := make(map[string]interface{})
	if u.Description != "" {
		m["description"] = u.Description
	}
	if u.Narration != "" {
		m["narration"] = u.Narration
	}
	if u.Dialogue != "" {
		m["dialogue"] = u.Dialogue
	}
	if u.CameraType != "" {
		m["camera_type"] = u.CameraType
	}
	if u.CameraAngle != "" {
		m["camera_angle"] = u.CameraAngle
	}
	if u.ShotSize != "" {
		m["shot_size"] = u.ShotSize
	}
	if u.Duration > 0 {
		m["duration"] = u.Duration
	}
	if u.EmotionalTone != "" {
		m["emotional_tone"] = u.EmotionalTone
	}
	if u.Transition != "" {
		m["transition"] = u.Transition
	}
	if u.TransitionOut != "" {
		m["transition_out"] = u.TransitionOut
	}
	if u.TransitionIn != "" {
		m["transition_in"] = u.TransitionIn
	}
	return m
}

// buildStoryboardOptimizePrompt 构建分镜优化提示词
func buildStoryboardOptimizePrompt(shots []*model.StoryboardShot, review *model.StoryboardReview) string {
	// 预格式化分镜数据
	var sb strings.Builder
	for _, sh := range shots {
		narr := sh.Narration
		if len([]rune(narr)) > 60 {
			narr = string([]rune(narr)[:60]) + "…"
		}
		desc := sh.Description
		if len([]rune(desc)) > 60 {
			desc = string([]rune(desc)[:60]) + "…"
		}
		sb.WriteString(fmt.Sprintf("[镜%d] 景别:%s 时长:%.0fs 镜头:%s/%s",
			sh.ShotNo, sh.CamDir.ShotSize, sh.Duration, sh.CamDir.CameraType, sh.CamDir.CameraAngle))
		if desc != "" {
			sb.WriteString(fmt.Sprintf(" 描述:\"%s\"", desc))
		}
		if narr != "" {
			sb.WriteString(fmt.Sprintf(" 旁白:\"%s\"", narr))
		}
		sb.WriteString("\n")
	}

	feedbackData := make([]map[string]interface{}, 0, len(review.ShotFeedback))
	for _, fb := range review.ShotFeedback {
		feedbackData = append(feedbackData, map[string]interface{}{
			"ShotNo":     fb.ShotNo,
			"Severity":   fb.Severity,
			"Issues":     fb.Issues,
			"Suggestion": fb.Suggestion,
		})
	}

	ctx := map[string]interface{}{
		"ShotCount":         len(shots),
		"ShotsText":         sb.String(),
		"GlobalSuggestions": review.GlobalSuggestions,
		"ShotFeedback":      feedbackData,
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
	cleaned := extractJSON(result)
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

	// 取 Video.NovelID 以便 GenerateWithProvider 能通过小说级 AI 模型配置选择 provider
	var novelID uint
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
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

	prompt := buildStoryboardOptimizePrompt(shots, review)

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "storyboard_optimize", prompt, provider)
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
		fields := upd.toFieldMap()
		if len(fields) == 0 {
			continue
		}
		if err := s.storyboardRepo.UpdateFields(sh.ID, fields); err != nil {
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
		fields := map[string]interface{}{
			"description": snap.Description,
			"narration":   snap.Narration,
			"dialogue":    snap.GenMeta.Dialogue,
			"duration":    snap.Duration,
			"cam_dir":     snap.CamDir,
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
func (s *VideoService) generateStoryboardArc(content string, tenantID, novelID uint, provider string) string {
	if s.aiService == nil {
		return ""
	}
	// 截断过长内容，弧线分析只需把握全局走向
	arcContent := content
	const maxArcRunes = 6000
	if len([]rune(arcContent)) > maxArcRunes {
		arcContent = string([]rune(arcContent)[:maxArcRunes]) + "…（已截断，请基于前段内容推断全章情感弧线）"
	}
	ctx := map[string]interface{}{"Content": arcContent}
	prompt, err := renderPrompt("storyboard_arc", ctx)
	if err != nil {
		logger.Errorf("[Storyboard] generateStoryboardArc renderPrompt: %v", err)
		return ""
	}
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "storyboard_arc", prompt, provider)
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

// ─── Ensure unused imports are satisfied ─────────────────────────────────────

var _ *repository.PlotPointRepository // force import of repository package
