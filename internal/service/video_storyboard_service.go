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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
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
func (s *VideoService) GenerateStoryboard(videoID uint, provider, userPrompt string, progressFn func(int), overrides StoryboardOverrides, chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
	// Prevent concurrent storyboard generation for the same video.
	if _, loaded := s.generatingStoryboard.LoadOrStore(videoID, struct{}{}); loaded {
		return nil, fmt.Errorf("storyboard generation already in progress for video %d", videoID)
	}
	defer s.generatingStoryboard.Delete(videoID)

	totalStart := time.Now()

	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}

	// 租户状态校验（与 StartGeneration 保持一致）
	if err := s.checkTenantAccess(video.NovelID); err != nil {
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
		return nil, fmt.Errorf("章节内容为空，请先在「写作」页面编写章节内容再生成分镜脚本")
	}

	const minChapterLength = 100 // characters
	if len([]rune(content)) < minChapterLength {
		return nil, fmt.Errorf("chapter content too short (%d chars): minimum %d characters required for storyboard generation",
			len([]rune(content)), minChapterLength)
	}

	// 并行预取角色、场景锚点、情节点（避免多次串行 DB 查询）
	// tenantID 直接从已加载的 video 取，无需额外查小说。
	tenantID := video.TenantID
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
				var e error
				characters, e = s.characterRepo.ListByNovel(novelID)
				if e != nil {
					logger.Errorf("[Storyboard] characterRepo.ListByNovel novelID=%d: %v", novelID, e)
				}
			}()
			if s.sceneAnchorSvc != nil {
				wgPre.Add(1)
				go func() {
					defer wgPre.Done()
					var e error
					anchors, e = s.sceneAnchorSvc.ListByNovel(novelID)
					if e != nil {
						logger.Errorf("[Storyboard] sceneAnchorSvc.ListByNovel novelID=%d: %v", novelID, e)
					}
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

	// 获取小说的 PromptLanguage 设置（决定 description/image_prompt/video_prompt 使用中文还是英文）
	promptLanguage := "zh"
	if s.novelRepo != nil && video.NovelID > 0 {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil && novel.PromptLanguage != "" {
			promptLanguage = novel.PromptLanguage
		}
	}

	// 分段生成：长章节按 maxSegmentRunes 字切割，每段独立调用 AI，合并后顺序重编号。
	// 短章节（≤maxSegmentRunes 字）等同于原单段调用路径，行为完全一致。
	segments := splitContentSegments(content, maxSegmentRunes)

	totalRunes := len([]rune(content))
	totalShots := calcTotalShots(totalRunes, video.TargetDuration, video.Pacing)

	chIDStr := "nil"
	if chapterID != nil {
		chIDStr = fmt.Sprintf("%d", *chapterID)
	}
	logger.Printf("[Storyboard] start videoID=%d chapterID=%s provider=%q voiceMode=%q totalRunes=%d segments=%d expectedShots=%d chars=%d anchors=%d plotPoints=%d",
		videoID, chIDStr, provider, overrides.VoiceMode, totalRunes, len(segments), totalShots, len(characters), len(anchors), len(plotPoints))

	// 并行处理各段落：段间内容本身保证情节连贯（AI 读段落文本即可自然衔接），
	// 不再传递 prevTailShots，换取所有段同时发起 AI 调用。
	// 最多允许 3 段并发，避免超出大多数 API 的并发限制。
	const maxParallelSegs = 3
	type segResult struct {
		shots []*model.StoryboardShot
		err   error
	}
	results := make([]segResult, len(segments))
	sem := make(chan struct{}, maxParallelSegs)
	var wg sync.WaitGroup
	var doneCount int32

	genCtx := context.Background()
	for segIdx, seg := range segments {
		wg.Add(1)
		go func(idx int, content string) {
			defer wg.Done()
			select {
			case <-genCtx.Done():
				results[idx] = segResult{err: genCtx.Err()}
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			segStart := time.Now()
			segRunes := len([]rune(content))
			segShotCount := totalShots * segRunes / max(totalRunes, 1)
			if segShotCount < 3 {
				segShotCount = 3
			}
			logger.Printf("[Storyboard] seg %d/%d start runes=%d expectedShots=%d", idx+1, len(segments), segRunes, segShotCount)

			prompt := s.buildStoryboardPrompt(video, content, userPrompt, idx+1, len(segments), segShotCount, characters, anchors, plotPoints, nil, overrides.VoiceMode, promptLanguage, video.Pacing)

			var result string
			var aiErr error
			var shots []*model.StoryboardShot
			for attempt := 0; attempt < 3; attempt++ {
				p := prompt
				switch attempt {
				case 1:
					p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON 数组，不要包含任何 markdown 代码块（```）或说明文字。"
					logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (format hint)", idx+1, len(segments), attempt)
				case 2:
					p = prompt + fmt.Sprintf("\n\n⚠️ 极重要：上一次你只返回了很少的分镜，请务必生成全部%d个分镜，只返回JSON数组不要截断。", segShotCount)
					logger.Printf("[Storyboard] seg %d/%d retry attempt=%d (shot count hint)", idx+1, len(segments), attempt)
				}
				aiStart := time.Now()
				result, aiErr = s.aiService.GenerateWithProvider(tenantID, video.NovelID, "storyboard", p, provider, overrides)
				aiElapsed := time.Since(aiStart).Round(time.Millisecond)
				if aiErr != nil {
					logger.Errorf("[Storyboard] seg %d/%d attempt=%d AI error elapsed=%s err=%v", idx+1, len(segments), attempt, aiElapsed, aiErr)
					if ai.IsTimeoutError(aiErr) {
						break
					}
					continue
				}
				logger.Printf("[Storyboard] seg %d/%d attempt=%d AI ok elapsed=%s responseLen=%d", idx+1, len(segments), attempt, aiElapsed, len(result))
				if strings.TrimSpace(result) == "" {
					continue
				}
				parsed, parseErr := s.parseStoryboardResult(videoID, chapterID, result)
				if parseErr != nil {
					logger.Errorf("[Storyboard] seg %d/%d attempt=%d parse failed: %v", idx+1, len(segments), attempt, parseErr)
					continue
				}
				// Retry if AI returned far fewer shots than requested (< 50% of target).
				if len(parsed) < segShotCount/2 && attempt < 2 {
					logger.Printf("[Storyboard] seg %d/%d attempt=%d too few shots got=%d expected=%d, retrying", idx+1, len(segments), attempt, len(parsed), segShotCount)
					shots = parsed // keep as fallback
					continue
				}
				shots = parsed
				break
			}
			if aiErr != nil && shots == nil {
				results[idx] = segResult{err: aiErr}
				return
			}
			if shots == nil {
				logger.Printf("[Storyboard] seg %d/%d fatal: AI returned empty or unparseable response after all retries", idx+1, len(segments))
				results[idx] = segResult{err: fmt.Errorf("AI返回空响应，请检查模型配置或更换提供商")}
				return
			}
			logger.Printf("[Storyboard] seg %d/%d done shots=%d elapsed=%s", idx+1, len(segments), len(shots), time.Since(segStart).Round(time.Millisecond))
			results[idx] = segResult{shots: shots}

			// 进度回调：每段完成即上报（并发完成顺序不定，取当前已完成数比例）
			if progressFn != nil {
				done := int(atomic.AddInt32(&doneCount, 1))
				progressFn(done * 90 / len(segments))
			}
		}(segIdx, seg)
	}
	wg.Wait()

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

	// 更新视频状态
	video.TotalShots = len(shots)
	video.Status = "storyboard"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[VideoService] failed to update video %d status: %v", video.ID, err)
	}

	logger.Errorf("[Storyboard] finished videoID=%d totalShots=%d segments=%d failedSegs=%d elapsed=%s",
		videoID, len(shots), len(segments), failedSegs, time.Since(totalStart).Round(time.Millisecond))

	// 若存在失败段落，返回包含失败信息的 error（不阻止已成功段落的结果）
	var returnErr error
	if failedSegs > 0 && firstErr != nil {
		returnErr = fmt.Errorf("部分段落生成失败（%d/%d），已返回成功段落的分镜: %w", failedSegs, len(segments), firstErr)
	}

	// 分镜生成完成后，自动用 sfx_tags 触发音效搜索（后台执行，不阻塞接口返回）
	if s.sfxService != nil {
		sfxShots := make([]*model.StoryboardShot, len(shots))
		copy(sfxShots, shots)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			success, fail, _ := s.sfxService.BatchAutoGenerateSFX(ctx, sfxShots, tenantID, "", "", nil)
			logger.Printf("[Storyboard] auto-SFX done videoID=%d success=%d fail=%d", videoID, success, fail)
		}()
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
		// shot.Scene 是 JSON: {"location":"...","time_of_day":"..."}
		loc := extractLocationFromScene(shot.Scene)
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
// 匹配优先级：① shot.Characters JSON → ② shot.Dialogue "角色名：台词" → ③ shot.Narration 关键词扫描。
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
		if shot.Characters != "" {
			var shotChars []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err == nil {
				for _, sc := range shotChars {
					tryMatch(sc.Name, seen, &matched)
				}
			} else {
				logger.Printf("[AutoMatch] char: shot#%d Characters JSON parse err: %v (raw=%q)", shot.ShotNo, err, shot.Characters)
			}
		}

		// ② Dialogue: "角色名：台词" — 提取冒号前的角色名
		if len(matched) == 0 && shot.Dialogue != "" {
			for _, sep := range []string{"：", ":"} {
				if idx := strings.Index(shot.Dialogue, sep); idx > 0 {
					name := shot.Dialogue[:idx]
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

		if len(matched) > 0 {
			shot.CharacterIDs = matched
			charMatchCount++
			logger.Printf("[AutoMatch] char: shot#%d → charIDs=%v (chars_json=%q)", shot.ShotNo, []uint(matched), shot.Characters)
		} else {
			logger.Printf("[AutoMatch] char: shot#%d no match (chars_json=%q)", shot.ShotNo, shot.Characters)
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
// targetDuration=0 时降级为字数密度估算（向后兼容）。
func calcTotalShots(totalRunes, targetDuration int, pacing string) int {
	if targetDuration > 0 {
		avg := map[string]int{"slow": 8, "normal": 5, "fast": 3}
		s, ok := avg[pacing]
		if !ok {
			s = 5
		}
		n := targetDuration / s
		if n < 3 {
			n = 3
		}
		return n
	}
	// 降级：字数估算
	n := totalRunes / 280
	if n < 5 {
		n = 5
	}
	if n > 60 {
		n = 60
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
	pacing string,
) string {
	isEn := promptLanguage == "en"

	segLabel := ""
	if totalSegs > 1 {
		segLabel = fmt.Sprintf("（第%d段，共%d段）", segNo, totalSegs)
	}

	// 过滤角色：优先匹配内容中出现的角色，否则回退到主角
	var matchedChars []map[string]interface{}
	if len(characters) > 0 {
		contentLower := strings.ToLower(content)
		var matched []*model.Character
		for _, c := range characters {
			if strings.Contains(contentLower, strings.ToLower(c.Name)) {
				matched = append(matched, c)
			}
		}
		if len(matched) == 0 {
			for _, c := range characters {
				if c.Role == "protagonist" {
					matched = append(matched, c)
				}
			}
		}
		matchedChars = make([]map[string]interface{}, 0, len(matched))
		for _, c := range matched {
			matchedChars = append(matchedChars, map[string]interface{}{
				"Name":         c.Name,
				"Role":         c.Role,
				"Description":  c.Description,
				"VisualPrompt": c.VisualPrompt,
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

	// 上一段末尾分镜
	prevShotsData := make([]map[string]interface{}, 0, len(prevShots))
	for _, ps := range prevShots {
		narrOrDesc := ps.Narration
		if narrOrDesc == "" {
			narrOrDesc = ps.Description
		}
		prevShotsData = append(prevShotsData, map[string]interface{}{
			"ShotNo":     ps.ShotNo,
			"NarrOrDesc": narrOrDesc,
			"Dialogue":   ps.Dialogue,
		})
	}

	// 截断内容（byte-level，保持与原行为一致）
	if len(content) > 10000 {
		content = content[:10000] + "…（已截断）"
	}

	expectedShotsMinus2 := expectedShots - 2
	if expectedShotsMinus2 < 0 {
		expectedShotsMinus2 = 0
	}

	ctx := map[string]interface{}{
		"IsEn":                isEn,
		"SegLabel":            segLabel,
		"ExpectedShots":       expectedShots,
		"ExpectedShotsMinus2": expectedShotsMinus2,
		"VoiceMode":           voiceMode,
		"Pacing":              pacing,
		"PrevShots":           prevShotsData,
		"Characters":          matchedChars,
		"Anchors":             matchedAnchors,
		"PlotPoints":          ppData,
		"Content":             content,
		"UserPrompt":          userPrompt,
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
		Transition string   `json:"transition"`
		SFXTags    []string `json:"sfx_tags"`
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

		// video_prompt: 优先使用 LLM 生成的专业视频提示词，兜底用 buildMotionPrompt 生成
		cameraType := validCameraType(r.CameraType)
		cameraAngle := validCameraAngle(r.CameraAngle)
		shotSize := validShotSize(r.ShotSize)
		videoPrompt := r.VideoPrompt
		if videoPrompt == "" {
			videoPrompt = buildMotionPrompt(&model.StoryboardShot{
				CameraType:  cameraType,
				CameraAngle: cameraAngle,
				ShotSize:    shotSize,
				Description: r.Description,
			})
		}

		// 将 sfx_tags 序列化为 JSON 字符串存储
		var sfxTagsJSON string
		if len(r.SFXTags) > 0 {
			if b, err := json.Marshal(r.SFXTags); err == nil {
				sfxTagsJSON = string(b)
			}
		}

		shot := &model.StoryboardShot{
			UUID:           uuid.New().String(),
			VideoID:        videoID,
			ChapterID:      chapterID,
			ShotNo:         shotNo,
			Description:    r.Description,
			Narration:      r.Narration,
			Prompt:         imagePrompt,
			MotionPrompt:   videoPrompt,
			NegativePrompt: r.NegativePrompt,
			Dialogue:       r.Dialogue,
			CameraType:     cameraType,
			CameraAngle:    cameraAngle,
			ShotSize:       shotSize,
			Duration:       duration,
			Transition:     validTransition(r.Transition),
			Characters:     charsJSON,
			Scene:          sceneJSON,
			SFXTags:        sfxTagsJSON,
			Status:         "pending",
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

// buildMotionPrompt 根据分镜信息构建运动提示词
func buildMotionPrompt(shot *model.StoryboardShot) string {
	motionMap := map[string]string{
		"static":     "locked-off camera, stable frame, no camera movement",
		"push":       "slow cinematic push in toward subject",
		"pull":       "slow cinematic pull back from subject",
		"pan":        "smooth horizontal camera pan",
		"track":      "smooth tracking shot following subject",
		"crane_up":   "crane shot slowly rising upward",
		"crane_down": "crane shot slowly descending",
		"follow":     "handheld follow shot tracking subject",
		"arc":        "smooth arc shot orbiting around subject",
		"tilt":       "smooth camera tilt",
		"whip_pan":   "fast whip pan transition",
		"zoom":       "smooth zoom toward subject",
	}
	motion := motionMap[shot.CameraType]
	if motion == "" {
		motion = "smooth camera movement"
	}
	desc := shot.Description
	if len(desc) > 200 {
		desc = desc[:200]
	}
	return fmt.Sprintf("%s, %s", motion, desc)
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
// 同时保证总像素数不低于 seedreamMinPixels（921600），满足 Seedream API 最低要求。
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
	if req.CameraType != "" {
		shot.CameraType = req.CameraType
	}
	if req.CameraAngle != "" {
		shot.CameraAngle = req.CameraAngle
	}
	if req.ShotSize != "" {
		shot.ShotSize = req.ShotSize
	}
	if req.Duration > 0 {
		shot.Duration = req.Duration
	}
	if req.Status != "" {
		shot.Status = req.Status
	}
	if req.GenerationMode != "" {
		shot.GenerationMode = req.GenerationMode
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
		"image_url": true, "sfx_volume": true,
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
	if afterShotNo < 0 {
		// Treat as append-to-end: find current max shot_no for this video
		var maxNo int
		s.storyboardRepo.DB().Model(&model.StoryboardShot{}).
			Where("video_id = ? AND deleted_at IS NULL", videoID).
			Select("COALESCE(MAX(shot_no), 0)").Scan(&maxNo)
		afterShotNo = maxNo
	}
	newShotNo := afterShotNo + 1
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	shot := &model.StoryboardShot{
		VideoID:     videoID,
		ShotNo:      newShotNo,
		UUID:        uuid.New().String(),
		Narration:   narration,
		Description: description,
		Duration:    duration,
		CameraType:  "static",
		CameraAngle: "eye_level",
		ShotSize:    "medium",
		Transition:  "cut",
		Status:      "pending",
	}
	// Shift + create must be atomic to avoid a corrupt shot_no sequence on partial failure.
	// Two-phase UPDATE: first shift all affected rows into a collision-free temp range
	// (+shiftTempOffset), then shift back to the intended position. A single-step UPDATE
	// causes MySQL to process rows one by one and trigger the unique key constraint
	// mid-scan (e.g. shot 7→8 conflicts while shot 8 still exists).
	err := s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
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
	if afterShotNo < 0 {
		// append at end: find max shot_no for this video
		var maxNo int
		s.storyboardRepo.DB().Model(&model.StoryboardShot{}).
			Where("video_id = ? AND deleted_at IS NULL", src.VideoID).
			Select("COALESCE(MAX(shot_no), 0)").Scan(&maxNo)
		afterShotNo = maxNo
	}
	newShotNo := afterShotNo + 1
	shot := &model.StoryboardShot{
		VideoID:        src.VideoID,
		ChapterID:      src.ChapterID,
		ShotNo:         newShotNo,
		UUID:           uuid.New().String(),
		Description:    src.Description,
		Narration:      src.Narration,
		Dialogue:       src.Dialogue,
		Subtitle:       src.Subtitle,
		CameraType:     src.CameraType,
		CameraAngle:    src.CameraAngle,
		ShotSize:       src.ShotSize,
		Duration:       src.Duration,
		EmotionalTone:  src.EmotionalTone,
		Transition:     src.Transition,
		Characters:     src.Characters,
		Scene:          src.Scene,
		Prompt:         src.Prompt,
		NegativePrompt: src.NegativePrompt,
		SceneAnchorID:  src.SceneAnchorID,
		CharacterIDs:   src.CharacterIDs,
		GenerationMode: src.GenerationMode,
		// ImageURL and VideoURL intentionally NOT copied — copied shot starts fresh
		Status: "pending",
	}
	err = s.storyboardRepo.DB().Transaction(func(tx *gorm.DB) error {
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
			TenantID:     tenantID,
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
			shot.ShotNo, shot.ShotSize, shot.Duration, shot.CameraType, shot.CameraAngle))
		if shot.EmotionalTone != "" {
			sb.WriteString(fmt.Sprintf(" 情绪:%s", truncate(shot.EmotionalTone, 12)))
		}
		if desc := truncate(shot.Description, 50); desc != "" {
			sb.WriteString(fmt.Sprintf("\n      描述: %s", desc))
		}
		if narr := truncate(shot.Narration, 80); narr != "" {
			sb.WriteString(fmt.Sprintf("\n      旁白: %s", narr))
		}
		if shot.Dialogue != "" {
			sb.WriteString(fmt.Sprintf("\n      台词: %s", truncate(shot.Dialogue, 50)))
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
			sh.ShotNo, sh.ShotSize, sh.Duration, sh.CameraType, sh.CameraAngle))
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
			TenantID:   tenantID,
			EntityType: model.ReviewEntityStoryboard,
			EntityID:   videoID,
			OverallScore: review.OverallScore,
			ReviewJSON:   string(snapshotData),
			Status:     "snapshot",
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
			"description":    snap.Description,
			"narration":      snap.Narration,
			"dialogue":       snap.Dialogue,
			"camera_type":    snap.CameraType,
			"camera_angle":   snap.CameraAngle,
			"shot_size":      snap.ShotSize,
			"duration":       snap.Duration,
			"emotional_tone": snap.EmotionalTone,
			"transition":     snap.Transition,
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
	item := &model.IgnoredReviewIssue{
		TenantID:    tenantID,
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

// ─── Ensure unused imports are satisfied ─────────────────────────────────────

var _ *repository.PlotPointRepository // force import of repository package
