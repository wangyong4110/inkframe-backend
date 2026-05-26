package service

// video_storyboard_service.go
//
// Storyboard generation, review, optimize, and shot-management methods
// extracted from video_service.go to keep the primary file manageable.
// All methods remain on *VideoService — no new types required.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ─── Package-level constants for magic numbers ───────────────────────────────

const (
	defaultShotDurationSecs  = 5.0  // 默认分镜时长（秒）
	maxSegmentRunes          = 3500 // 每段最多字符数（约 25 个镜头，≈5000 tokens）
	charRuneOverlapThreshold = 0.5  // 角色名模糊匹配汉字重叠比例阈值
)

// ─── Storyboard Generation ────────────────────────────────────────────────────

// GenerateStoryboard 生成分镜
// userPrompt: 用户自定义提示词（可选），将追加到系统 prompt 之后
// progressFn: 可选的进度回调（0-99），供调用方更新任务进度（传 nil 则跳过）
func (s *VideoService) GenerateStoryboard(videoID uint, provider, userPrompt string, progressFn func(int), overrides StoryboardOverrides, chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
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
			logger.Printf("[VideoService] failed to update video chapterID: %v", err)
		}
	}

	var content string
	if chapterID != nil {
		chapter, _ := s.chapterRepo.GetByID(*chapterID)
		if chapter != nil {
			content = chapter.Content
		}
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("章节内容为空，请先在「写作」页面编写章节内容再生成分镜脚本")
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
				characters, _ = s.characterRepo.ListByNovel(novelID)
			}()
			if s.sceneAnchorSvc != nil {
				wgPre.Add(1)
				go func() {
					defer wgPre.Done()
					anchors, _ = s.sceneAnchorSvc.ListByNovel(novelID)
				}()
			}
		}
		if s.plotPointRepo != nil && chapterID != nil {
			wgPre.Add(1)
			go func() {
				defer wgPre.Done()
				plotPoints, _ = s.plotPointRepo.ListByChapter(*chapterID)
			}()
		}
		wgPre.Wait()
		// 如果章节内无情节点，降级到小说级别
		if s.plotPointRepo != nil && len(plotPoints) == 0 && novelID > 0 {
			plotPoints, _ = s.plotPointRepo.ListByNovel(novelID, "", true)
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
	logger.Printf("[Storyboard] start videoID=%d chapterID=%s provider=%q totalRunes=%d segments=%d expectedShots=%d chars=%d anchors=%d plotPoints=%d",
		videoID, chIDStr, provider, totalRunes, len(segments), totalShots, len(characters), len(anchors), len(plotPoints))

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

			prompt := s.buildStoryboardPrompt(video, content, userPrompt, idx+1, len(segments), segShotCount, characters, anchors, plotPoints, nil, overrides.VoiceMode, promptLanguage)

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
					logger.Printf("[Storyboard] seg %d/%d attempt=%d AI error elapsed=%s err=%v", idx+1, len(segments), attempt, aiElapsed, aiErr)
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
					logger.Printf("[Storyboard] seg %d/%d attempt=%d parse failed: %v", idx+1, len(segments), attempt, parseErr)
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
	for idx, r := range results {
		if r.err != nil {
			failedSegs++
			if idx == 0 {
				logger.Printf("[Storyboard] seg 1/%d failed (fatal): %v", len(segments), r.err)
				return nil, r.err
			}
			logger.Printf("[Storyboard] seg %d/%d failed (non-fatal): %v", idx+1, len(segments), r.err)
			continue
		}
		for _, shot := range r.shots {
			shotCounter++
			shot.ShotNo = shotCounter
		}
		allShots = append(allShots, r.shots...)
	}

	if len(allShots) == 0 {
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

	// 删除旧分镜，再批量插入新分镜（单次 SQL，避免 N 次往返）
	if err := s.storyboardRepo.DeleteByVideoID(videoID); err != nil {
		return nil, err
	}
	if err := s.storyboardRepo.BatchCreate(shots); err != nil {
		return nil, fmt.Errorf("保存分镜失败: %w", err)
	}
	if progressFn != nil {
		progressFn(99)
	}

	// 更新视频状态
	video.TotalShots = len(shots)
	video.Status = "storyboard"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[VideoService] failed to update video %d status: %v", video.ID, err)
	}

	logger.Printf("[Storyboard] finished videoID=%d totalShots=%d segments=%d failedSegs=%d elapsed=%s",
		videoID, len(shots), len(segments), failedSegs, time.Since(totalStart).Round(time.Millisecond))

	// 分镜生成完成后，自动用 sfx_tags 触发音效搜索（后台执行，不阻塞接口返回）
	if s.sfxService != nil {
		sfxShots := make([]*model.StoryboardShot, len(shots))
		copy(sfxShots, shots)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			success, fail := s.sfxService.BatchAutoGenerateSFX(ctx, sfxShots, tenantID, "", nil)
			logger.Printf("[Storyboard] auto-SFX done videoID=%d success=%d fail=%d", videoID, success, fail)
		}()
	}

	return shots, nil
}

// autoMatchShotAnchors 按场景名称自动将分镜绑定到场景锚点（模糊匹配 scene.location）
// 这样无需前端手动调用 SetShotAnchor，锚点注入即可在视频生成时自动生效。
// anchors 为调用方预取的数据（避免重复查 DB）。
func (s *VideoService) autoMatchShotAnchors(shots []*model.StoryboardShot, anchors []*model.SceneAnchor) {
	if len(anchors) == 0 {
		return
	}
	// 构建名称→ID映射（小写，方便模糊匹配）
	anchorMap := make(map[string]uint, len(anchors))
	for _, a := range anchors {
		anchorMap[strings.ToLower(a.Name)] = a.ID
	}
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
		if loc == "" {
			continue
		}
		// 精确匹配优先，其次包含匹配
		if id, ok := anchorMap[loc]; ok {
			id := id
			shot.SceneAnchorID = &id
			continue
		}
		for name, id := range anchorMap {
			if strings.Contains(loc, name) || strings.Contains(name, loc) {
				id := id
				shot.SceneAnchorID = &id
				break
			}
		}
	}
}

// autoMatchShotCharacters 按 shot.Characters JSON 中的名称匹配小说角色，写入 CharacterIDs
// 已有 CharacterIDs 时不覆盖（保留手动绑定结果）。
// chars 为调用方预取的数据（避免重复查 DB）。
func (s *VideoService) autoMatchShotCharacters(shots []*model.StoryboardShot, chars []*model.Character) {
	if len(chars) == 0 {
		return
	}
	// 构建 小写名→ID map
	nameMap := make(map[string]uint, len(chars))
	for _, c := range chars {
		nameMap[strings.ToLower(c.Name)] = c.ID
	}
	for _, shot := range shots {
		if len(shot.CharacterIDs) > 0 {
			continue // 已手动绑定，不覆盖
		}
		// shot.Characters = JSON array: [{"name":"...","expression":"...","pose":"..."}]
		var shotChars []struct {
			Name string `json:"name"`
		}
		if shot.Characters == "" {
			continue
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err != nil {
			continue
		}
		var matched model.JSONUintSlice
		seen := make(map[uint]bool)
		for _, sc := range shotChars {
			nameLower := strings.ToLower(sc.Name)
			if id, ok := nameMap[nameLower]; ok {
				if !seen[id] {
					matched = append(matched, id)
					seen[id] = true
				}
				continue
			}
			// 模糊匹配：子串包含 + 汉字字符级重叠（阈值 50%，适配"萧炎"vs"炎少"等写法）
			for name, id := range nameMap {
				if strings.Contains(nameLower, name) || strings.Contains(name, nameLower) ||
					charRuneOverlap(nameLower, name) >= charRuneOverlapThreshold {
					if !seen[id] {
						matched = append(matched, id)
						seen[id] = true
					}
					break
				}
			}
		}
		if len(matched) > 0 {
			shot.CharacterIDs = matched
		}
	}
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
				"Name":        c.Name,
				"Role":        c.Role,
				"Description": c.Description,
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
		"IsEn":              isEn,
		"SegLabel":          segLabel,
		"ExpectedShots":     expectedShots,
		"ExpectedShotsMinus2": expectedShotsMinus2,
		"VoiceMode":         voiceMode,
		"PrevShots":         prevShotsData,
		"Characters":        matchedChars,
		"Anchors":           matchedAnchors,
		"PlotPoints":        ppData,
		"Content":           content,
		"UserPrompt":        userPrompt,
	}
	result, err := renderPrompt("storyboard_generate", ctx)
	if err != nil {
		logger.Printf("[buildStoryboardPrompt] renderPrompt error: %v", err)
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
		logger.Printf("[VideoService] parseStoryboardResult: JSON parse failed (%v)\n===== AI RAW RESPONSE (len=%d) =====\n%s\n===== END =====", parseErr, len(result), result)
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
		return 1280, 50, 8.0
	default:
		return 768, 25, 7.0
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

// InsertShot 在 afterShotNo 之后插入一个空分镜（afterShotNo=0 表示插入到最前）
func (s *VideoService) InsertShot(videoID uint, afterShotNo int, narration, description string, duration float64) (*model.StoryboardShot, error) {
	newShotNo := afterShotNo + 1
	// 后移所有 shot_no >= newShotNo 的分镜
	if err := s.storyboardRepo.ShiftShotNos(videoID, newShotNo, 1); err != nil {
		return nil, fmt.Errorf("shift shot numbers: %w", err)
	}
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
	return shot, s.storyboardRepo.Create(shot)
}

// CopyShotAfter 复制分镜，插入到 afterShotNo 之后（afterShotNo=0 → 复制到最前；afterShotNo=-1 → 紧接源分镜之后）
func (s *VideoService) CopyShotAfter(sourceShotID uint, afterShotNo int) (*model.StoryboardShot, error) {
	src, err := s.storyboardRepo.GetByID(sourceShotID)
	if err != nil {
		return nil, fmt.Errorf("source shot not found: %w", err)
	}
	if afterShotNo < 0 {
		afterShotNo = src.ShotNo // 紧接在原分镜之后
	}
	newShotNo := afterShotNo + 1
	if err := s.storyboardRepo.ShiftShotNos(src.VideoID, newShotNo, 1); err != nil {
		return nil, fmt.Errorf("shift shot numbers: %w", err)
	}
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
	return shot, s.storyboardRepo.Create(shot)
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

	// 取 Video.NovelID 以便 GenerateWithProvider 能通过小说级 AI 模型配置选择 provider
	var novelID uint
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
	}

	// 拉取最近一次已应用的审查反馈，注入提示词避免重复建议
	var previousFeedback []model.ShotReviewFeedback
	if s.reviewRecordRepo != nil {
		if latest, err := s.reviewRecordRepo.GetLatestApplied(videoID); err == nil && latest.ReviewDataJSON != "" {
			var prev model.StoryboardReview
			if json.Unmarshal([]byte(latest.ReviewDataJSON), &prev) == nil {
				previousFeedback = prev.ShotFeedback
			}
		}
	}

	prompt := buildStoryboardReviewPrompt(shots, previousScore, previousFeedback)

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
		rec := &model.StoryboardReviewRecord{
			TenantID:       tenantID,
			VideoID:        videoID,
			OverallScore:   review.OverallScore,
			ReviewDataJSON: string(reviewJSON),
			Status:         "pending",
		}
		if saveErr := s.reviewRecordRepo.Create(rec); saveErr != nil {
			logger.Printf("ReviewStoryboard: save record failed: %v", saveErr)
		} else {
			recordID = rec.ID
		}
	}

	return review, recordID, nil
}

// buildStoryboardReviewPrompt 构建分镜审查提示词
// previousScore > 0 时注入上次评分上下文；previousFeedback 非空时注入已修正问题摘要。
func buildStoryboardReviewPrompt(shots []*model.StoryboardShot, previousScore float64, previousFeedback []model.ShotReviewFeedback) string {
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

	ctx := map[string]interface{}{
		"ShotCount":          len(shots),
		"ShotsText":          sb.String(),
		"HasPreviousScore":   previousScore > 0,
		"PreviousScoreStr":   fmt.Sprintf("%.1f", previousScore),
		"HasPreviousFixed":   len(prevFixedLines) > 0,
		"PreviousFixedText":  strings.Join(prevFixedLines, "\n"),
	}
	result, err := renderPrompt("storyboard_review", ctx)
	if err != nil {
		logger.Printf("[buildStoryboardReviewPrompt] renderPrompt error: %v", err)
		return ""
	}
	return result
}

// parseStoryboardReview 解析 AI 返回的分镜审查报告
func parseStoryboardReview(result string) (*model.StoryboardReview, error) {
	cleaned := extractJSONObject(result)
	var review model.StoryboardReview
	if err := json.Unmarshal([]byte(cleaned), &review); err != nil {
		return nil, fmt.Errorf("解析审查报告失败: %w; AI响应(前300字符): %.300s", err, result)
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
		logger.Printf("[buildStoryboardOptimizePrompt] renderPrompt error: %v", err)
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
		snap := &model.StoryboardReviewRecord{
			TenantID:       tenantID,
			VideoID:        videoID,
			OverallScore:   review.OverallScore,
			ReviewDataJSON: string(snapshotData),
			Status:         "snapshot",
		}
		if saveErr := s.reviewRecordRepo.Create(snap); saveErr != nil {
			logger.Printf("OptimizeStoryboardFromReview: save snapshot failed: %v", saveErr)
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
		fields := upd.toFieldMap()
		if len(fields) == 0 {
			continue
		}
		if err := s.storyboardRepo.UpdateFields(sh.ID, fields); err != nil {
			logger.Printf("OptimizeStoryboardFromReview: update shot %d failed: %v", sh.ShotNo, err)
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
			logger.Printf("ApplyStoryboardDiffs: update shot %d failed: %v", diff.ShotNo, err)
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
				logger.Printf("ApplyStoryboardDiffs: update record %d status failed: %v", recordID, err2)
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
	if rec.VideoID != videoID {
		return 0, fmt.Errorf("审查记录不属于该视频")
	}
	if rec.Status != "snapshot" {
		return 0, fmt.Errorf("该记录不是快照，无法回滚")
	}

	// 解析快照中的分镜数据
	var snapshotShots []*model.StoryboardShot
	if err := json.Unmarshal([]byte(rec.ReviewDataJSON), &snapshotShots); err != nil {
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
			logger.Printf("RollbackReview: update shot %d failed: %v", snap.ShotNo, err)
			continue
		}
		restored++
	}

	// 标记审查记录为已回滚
	if rollbackRec, err := s.reviewRecordRepo.GetByID(recordID); err == nil {
		rollbackRec.Status = "rolled_back"
		if err2 := s.reviewRecordRepo.Update(rollbackRec); err2 != nil {
			logger.Printf("RollbackReview: update record %d status failed: %v", recordID, err2)
		}
	}

	logger.Printf("RollbackReview: videoID=%d restored=%d shots from record %d", videoID, restored, recordID)
	return restored, nil
}

// ListReviewRecords 列出视频的所有审查记录
func (s *VideoService) ListReviewRecords(videoID uint) ([]*model.StoryboardReviewRecord, error) {
	if s.reviewRecordRepo == nil {
		return nil, fmt.Errorf("review record repository not initialized")
	}
	return s.reviewRecordRepo.ListByVideo(videoID)
}

// ─── Ensure unused imports are satisfied ─────────────────────────────────────

var _ *repository.PlotPointRepository // force import of repository package
