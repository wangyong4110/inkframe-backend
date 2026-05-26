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
	var sb strings.Builder

	segLabel := ""
	if totalSegs > 1 {
		segLabel = fmt.Sprintf("（第%d段，共%d段）", segNo, totalSegs)
	}
	sb.WriteString(fmt.Sprintf("你是专业影视分镜师，同时负责撰写旁白文案。请将以下章节内容%s转化为约%d个分镜，返回JSON数组。\n\n", segLabel, expectedShots))

	// ── 字段规范（核心约束，AI 必须严格遵守）──────────────────────────
	sb.WriteString(`【输出字段规范——严格遵守，违反规范将导致输出无法使用】
▸ camera_type（运镜方式）——必须体现多样性，禁止全部使用 static
  可选值（选其中一个填入）：static | push | pull | pan | track | crane_up | crane_down | follow | arc | tilt | whip_pan | zoom
  含义：static=固定机位，push=推镜头(向前推进)，pull=拉镜头(向后拉远)，pan=摇镜头(水平旋转)，track=移镜头(横向平移)，crane_up=升镜头，crane_down=降镜头，follow=跟镜头(跟随主体)，arc=环绕镜头(围绕主体环绕)，tilt=俯仰镜头(垂直俯仰)，whip_pan=甩镜头(快速甩动)，zoom=变焦镜头(焦距变化)
  分布建议：static 不超过全部分镜的 40%，动作场景用 push/follow/arc，对话场景用 pan/tilt，环境展示用 pull/track/crane_up

▸ camera_angle（摄像机角度）——选其中一个填入
  可选值：eye_level | high | low | dutch | overhead | POV

▸ shot_size（景别）——选其中一个填入
  可选值：extreme_wide | wide | full | medium | close_up | extreme_close_up

`)
	// description 字段（语言模式：en=英文，zh=中文）
	if isEn {
		sb.WriteString(`▸ description（英文画面描述）
  - 画面的客观英文描述，禁止出现中文、叙事句子、心理描写
  - 必须包含以下四层信息（缺少任一层将被视为不合格）：
    ① 角色站位：出现的角色、在画框中的位置（foreground/background, left/center/right）、朝向与动作姿态
    ② 道具/物品：镜头内关键道具的位置及与角色的空间关系（如 "a sword mounted on the wall to his left"）
    ③ 场景环境：背景、地点特征、氛围细节
    ④ 光线与构图：光源方向、色调、景深感
  - 示例（全要素）："A young man in white robes stands foreground-left, gripping a bronze sword at his side, facing right. An elder in dark armor stands background-right, arms crossed. Between them, an ancient scroll rests on a stone table. Collapsed city gate visible behind the elder. Dramatic amber backlight, dusk, wide shot."

`)
	} else {
		sb.WriteString(`▸ description（画面描述）
  - 客观描述画面内容，禁止出现叙事句子、心理描写
  - 必须包含以下四层信息（缺少任一层将被视为不合格）：
    ① 角色站位：出现的角色、在画框中的位置（前景/后景，左/中/右），朝向与动作姿态
    ② 道具/物品：镜头内关键道具的位置及与角色的空间关系（如"一把剑悬于其左侧墙上"）
    ③ 场景环境：背景、地点特征、氛围细节
    ④ 光线与构图：光源方向、色调、景深感
  - 示例（全要素）："白衣少年立于前景左侧，右手握青铜剑垂于腰际，面朝右方。黑甲老者站于后景右侧，双臂交叉。两人之间，一卷古卷置于石台之上。老者身后可见倒塌的城门。戏剧性琥珀色逆光，黄昏时分，广角构图。"

`)
	}
	// image_prompt 头部注释（English only 仅 en 模式显示）
	if isEn {
		sb.WriteString("▸ image_prompt（图片生成专用提示词，English only，必填）\n")
	} else {
		sb.WriteString("▸ image_prompt（图片生成专用提示词，必填）\n")
	}
	sb.WriteString(`  - 专为 Stable Diffusion / Flux / DALL-E 等图片生成模型优化，与 description 不同，需加入画质词和风格词
  - 结构（按顺序拼接）：
    [art_style] → [shot_size] shot, [camera_angle] angle → [主体描述：角色外貌关键词+动作+位置] → [道具与空间关系] → [场景/背景环境] → [光源方向+色温描述] → [色调] → [景深/焦段] → [画质词]
  - 角色外貌必须从【角色信息】中提取关键词（发色、服装、体型、标志性特征），保持各镜头一致
  - 光线描述须精确：方向（front/side/back/top/rim）+ 类型（sunlight/moonlight/candlelight/neon/magic glow）+ 色温（warm golden/cool blue/neutral white）
  - 画质词结尾固定格式（根据风格二选一）：
    写实风：masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting
    动漫/插画风：masterpiece, best quality, ultra-detailed, vibrant colors, clean linework, professional illustration
  - 示例（仙侠写实风）："ancient Chinese xianxia, cinematic photography, medium close-up shot, low angle, young male cultivator (early 20s, sharp features, long black hair tied with jade hairpin, flowing white hanfu with gold trim) stands foreground-center gripping a glowing azure longsword raised to chest height with both hands, enemy elder in black iron armor stands background-right arms outstretched casting dark energy, crumbling stone training arena floor with scattered rubble, ancient pagoda visible in background, dramatic rim lighting from above left with cool blue magic light, cool blue and silver palette, shallow depth of field 85mm f/2.8, masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting"

`)
	// video_prompt 头部注释
	if isEn {
		sb.WriteString("▸ video_prompt（视频生成专用提示词，English only，必填）\n")
	} else {
		sb.WriteString("▸ video_prompt（视频生成专用提示词，必填）\n")
	}
	sb.WriteString(`  - 专为 Kling / Seedance 等 AI 视频生成模型优化，核心是描述"运动"，而非静态构图
  - 结构（按顺序拼接）：
    [镜头运动轨迹和速度] → [主体动态行为：具体动作+运动方向+速度节奏] → [场景动态元素：风/粒子/布料/水/火等] → [光线变化（如有）] → [时间流逝感] → [情绪节奏词]
  - 必须以镜头运动开头，体现 camera_type 字段的运动类型（例如 static 镜头也要写 "locked-off camera with subtle breathing stabilization"）
  - 禁止纯描述静态构图，每个字段都要有动态感
  - 示例（推镜+战斗）："slow cinematic push in toward subject at 0.3x speed, young cultivator dramatically sweeps glowing azure sword upward in a wide arc with full body rotation, white robes billowing outward from centrifugal force, azure sword energy particles trail behind blade path, dark energy tendrils from enemy dissipating frame-right, stone debris from impact site still settling in slow-motion, magic light intensifying with each frame, building tension, epic fantasy atmosphere, fluid smooth motion"

`)
	// negative_prompt 头部注释
	if isEn {
		sb.WriteString("▸ negative_prompt（负向提示词，English only，必填）\n")
	} else {
		sb.WriteString("▸ negative_prompt（负向提示词，必填）\n")
	}
	sb.WriteString(`  - 针对本镜头内容的具体缺陷排除词，而非通用固定词表
  - 根据镜头实际内容决定：有角色时加入角色解剖类负向词；有特效/光效时加入过曝/噪点类词；风景/建筑镜头时加入透视变形类词
  - 基础结构：[解剖/画质类] + [与本镜头内容相悖的风格词] + [本镜头特有的规避点]
  - 解剖/画质类（视角色有无选填）：ugly, deformed, extra limbs, bad anatomy, malformed hands, missing fingers, fused fingers, poorly drawn face, asymmetrical eyes, blurry, low quality, jpeg artifacts, noise, grainy, overexposed, underexposed, watermark, text, logo, signature
  - 风格相悖词（根据 image_prompt 的风格决定）：
    写实风避免：cartoon, anime, illustration, painting, sketch, flat colors, cel shading
    动漫风避免：photorealistic, photo, realistic skin texture, 3d render, cgi
    古风/仙侠避免：modern clothes, western armor, gun, car, neon sign, sci-fi elements
  - 本镜头特有规避（举例）：
    战斗镜头：static pose, standing still, no action, peaceful expression
    近景/特写：wide shot, full body, crowd, background figures
    暗夜场景：overexposed, bright daylight, harsh sunlight
    浪漫/温情镜头：angry expression, violent gesture, weapons, blood
  - 示例（仙侠战斗近景）："ugly, deformed, extra limbs, bad anatomy, malformed hands, missing fingers, fused fingers, poorly drawn face, asymmetrical eyes, blurry, low quality, watermark, text, cartoon, anime, flat colors, modern clothes, gun, sci-fi elements, static pose, standing still, peaceful expression, overexposed, wide shot"

▸ narration 与 dialogue 互斥规则——核心约束，必须严格遵守
  ① 旁白镜头（narration 非空）：dialogue 必须为空字符串 ""，二者不能同时出现在同一个分镜中
  ② 对白镜头（dialogue 非空）：narration 必须为空字符串 ""，不为角色台词配旁白
  ③ 每个对白镜头只允许一个角色说话——如果原文中两个角色连续对话，必须拆分为两个独立分镜，每镜一句台词
  ④ 对白场景拆分示例：凌云说"你敢！"→ 一个镜头；敌将回答"我就敢！"→ 下一个独立镜头
  ⑤ 旁白镜头用于叙述动作、场景过渡、心理刻画；对白镜头专用于角色开口说话

▸ narration（中文旁白文案）——旁白镜头必填，对白镜头必须为空 ""
  - 观众"听到"的旁白内容，第三人称叙事视角，语言生动凝练
  - 严禁出现以下词汇：镜头、画面、特写、推进、切换、转场、画外音、摄影机、定格
  - 严禁以角色名称开头，如"妈妈说："、"凌云道："、"角色名：" 等格式——旁白是旁白，不是台词引用
  - ✅ 正确示例：narration="凌云握紧长剑，眼中燃起不灭的复仇之火。" dialogue=""
  - ❌ 错误示例：narration="凌云握紧长剑。" dialogue="凌云：你敢再说一遍！"（两个字段同时有内容）

▸ dialogue（角色台词）——对白镜头必填，旁白镜头必须为空 ""
  - 格式固定："角色名：台词内容"（如"凌云：你敢再说一遍！"）
  - 每个分镜只能包含一个角色的一句台词，多角色对话必须拆分为多个分镜
  - ✅ 正确示例：dialogue="凌云：你敢再说一遍！" narration=""
  - ❌ 错误示例：dialogue="凌云：你敢！\n敌将：我就敢！"（两个角色在同一镜头）

`)

	// ── 配音模式约束 ─────────────────────────────────────────────────
	switch voiceMode {
	case "narration":
		sb.WriteString("【配音模式：仅旁白】\n所有分镜一律使用旁白（narration 非空），dialogue 必须为空字符串 \"\"。\n禁止出现任何角色台词，角色对话内容应转化为第三人称叙事旁白描述。\n\n")
	case "dialogue":
		sb.WriteString("【配音模式：仅对白】\n所有分镜一律使用角色台词（dialogue 非空），narration 必须为空字符串 \"\"。\n场景过渡、动作描写等非对话内容，请尽量转化为角色的内心独白或台词形式；若实在无法转化为台词，可将 narration 和 dialogue 均留空。\n\n")
	case "narration_primary":
		sb.WriteString("【配音模式：旁白为主】\n旁白（narration）优先：无对话的场景必须使用旁白，有角色对话的场景可保留台词（dialogue），但全片旁白镜头数量须多于对白镜头数量。旁白与台词不可在同一镜头中同时出现（互斥）。\n\n")
	case "dialogue_primary":
		sb.WriteString("【配音模式：对白为主】\n台词（dialogue）优先：尽量将叙事转化为角色对话或内心独白。场景过渡、纯动作镜头等无法对话化的内容可使用简短旁白（narration），但全片对白镜头数量须多于旁白镜头数量。旁白与台词不可在同一镜头中同时出现（互斥）。\n\n")
	default:
		// "auto"/"both"/"" — 默认行为，旁白/台词互斥约束已在上方说明
	}

	// ── 连续性上下文（跨段落情节连贯）──────────────────────────────
	if len(prevShots) > 0 {
		sb.WriteString("【上一段末尾分镜——本段首镜须与其情节自然衔接】\n")
		for _, ps := range prevShots {
			narr := ps.Narration
			if narr == "" {
				narr = ps.Description
			}
			sb.WriteString(fmt.Sprintf("  第%d镜 旁白：%s", ps.ShotNo, narr))
			if ps.Dialogue != "" {
				sb.WriteString(fmt.Sprintf(" ／ 台词：%s", ps.Dialogue))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// ── 角色外貌信息（仅注入当前段落提及的角色）──────────────────
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
		if len(matched) > 0 {
			sb.WriteString("【角色信息（description 中须保持一致）】\n")
			for _, c := range matched {
				sb.WriteString(fmt.Sprintf("- %s（%s）", c.Name, c.Role))
				if c.Description != "" {
					sb.WriteString(fmt.Sprintf("：%s", c.Description))
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	// ── 场景锚点（仅注入当前段落提及的锚点）──────────────────────
	if len(anchors) > 0 {
		contentLower := strings.ToLower(content)
		var matchedAnchors []*model.SceneAnchor
		for _, a := range anchors {
			if strings.Contains(contentLower, strings.ToLower(a.Name)) {
				matchedAnchors = append(matchedAnchors, a)
			}
		}
		if len(matchedAnchors) == 0 && len(anchors) > 0 {
			limit := 5
			if len(anchors) < limit {
				limit = len(anchors)
			}
			matchedAnchors = anchors[:limit]
		}
		if len(matchedAnchors) > 8 {
			matchedAnchors = matchedAnchors[:8]
		}
		if len(matchedAnchors) > 0 {
			sb.WriteString("【已命名场景（location 字段请从以下名称中选择）】\n")
			for _, a := range matchedAnchors {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", a.Name, a.Description))
			}
			sb.WriteString("\n")
		}
	}

	// ── 剧情线（提示叙事张力）──────────────────────────────────
	if len(plotPoints) > 0 {
		sb.WriteString("【本章剧情线（旁白须体现叙事张力）】\n")
		maxPP := 5
		if len(plotPoints) < maxPP {
			maxPP = len(plotPoints)
		}
		for i := 0; i < maxPP; i++ {
			sb.WriteString(fmt.Sprintf("- [%s] %s\n", plotPoints[i].Type, plotPoints[i].Description))
		}
		sb.WriteString("\n")
	}

	// ── 章节内容 ──────────────────────────────────────────────
	if content != "" {
		sb.WriteString("【章节内容】\n")
		if len(content) > 10000 {
			content = content[:10000] + "…（已截断）"
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// ── 用户附加要求 ──────────────────────────────────────────
	if userPrompt != "" {
		sb.WriteString("【用户要求】\n")
		sb.WriteString(userPrompt)
		sb.WriteString("\n\n")
	}

	// ── 输出格式 ──────────────────────────────────────────────
	sb.WriteString(fmt.Sprintf(`请将章节内容分解为**%d个**分镜（每个重大动作、对话轮次、场景切换须单独成镜）。
⚠️ 注意：必须输出全部%d个分镜，不得省略。只返回JSON数组，不要任何额外说明或markdown代码块。

【sfx_tags 填写规则】
sfx_tags 是该镜头环境音效关键词列表（英文，用于音效库搜索）。须根据画面内容、情绪、动作严格选词，每镜头选 1-4 个最贴合的标签，不得照抄示例。

可用标签词库（按类别分组，从中选取）：

自然/天气：
  rain_light / rain_heavy / rainstorm / thunder / lightning_crack / wind_gentle / wind_strong / wind_howling
  forest_ambient / jungle_ambient / birdsong / crickets_night / frogs / insects_buzz
  river_flowing / waterfall / ocean_waves / beach_waves / dripping_water / splash / underwater_ambient
  fire_crackle / fire_roar / snow_crunch / ice_crack / earthquake_rumble / cave_echo / desert_wind

室内/日常：
  ambient_room / fireplace / clock_ticking / door_open / door_close / door_creak / door_knock
  footsteps_wood / footsteps_stone / footsteps_metal / footsteps_grass
  glass_break / window_shatter / paper_rustle / book_flip / chair_scrape / table_slam
  cooking_sizzle / kettle_boil / pouring_liquid / coins_clink
  temple_bell / gong_strike / drum_beat / flute_ambient / guqin_ambient / erhu_ambient

人群/城市：
  crowd_murmur / crowd_outdoor / crowd_cheer / crowd_panic / crowd_fight / crowd_pray
  market_outdoor / market_indoor / tavern_ambient / harbor_ambient / street_ambient
  horse_gallop / horse_neigh / cart_wheels / blacksmith / construction_noise
  announcement_horn / war_drums / parade_drums / gate_open / gate_close

战斗/动作：
  sword_clash / sword_draw / sword_slice / spear_thrust / bow_draw / arrow_whoosh / arrow_impact
  punch_impact / kick_impact / body_fall / bones_crack / armor_clank / shield_block
  whip_crack / chain_rattle / weapon_drop / weapon_throw
  explosion / cannon_fire / fire_spread / debris_fall / wall_crumble / ground_shake
  running_footsteps / jumping_land / tumble / rope_swing / grappling

修仙/玄幻特效（仙侠/修真/玄幻小说适用）：
  qi_surge / spirit_energy / cultivation_aura / energy_explosion / magic_blast / energy_impact
  spiritual_chime / dao_resonance / formation_activate / seal_break / rune_glow / treasure_glow
  flying_whoosh / teleport_flash / space_tear / dimension_crack / void_rumble
  breakthrough_rumble / spiritual_pressure / divine_thunder / heavenly_lightning / immortal_wind
  spirit_beast_roar / demon_roar / ghost_howl / evil_energy / purification_light

动物/生物：
  wolf_howl / tiger_roar / dragon_roar / eagle_cry / snake_hiss / bat_screech
  horse_neigh / cattle_moo / dog_bark / bird_flock / monster_growl / beast_stomp

情绪/心理：
  heartbeat / heartbeat_fast / breath_heavy / breath_slow / gasp / whisper
  tension_sting / horror_sting / mystery_hum / suspense_low / sadness_ambience
  bell_ring / wind_night / echo_distant / silence_deep

建筑/机械：
  mechanism_click / trap_trigger / stone_rolling / stone_door / bridge_creak
  rope_creak / pulley / lock_click / chest_open

纯对话/静态：留空数组 []
`, expectedShots, expectedShots))

	// ── JSON 格式示例（语言模式：en=英文提示词，zh=中文提示词）──────
	if isEn {
		sb.WriteString(fmt.Sprintf(`格式示例（以下仅展示2个分镜作为格式参考，实际须生成%d个）：
[
  {
    "shot_no": 1,
    "description": "English only. Must include: ① character positions (foreground/background, left/center/right) and poses ② key props and their spatial relation to characters ③ scene environment ④ lighting and composition",
    "image_prompt": "Detailed English prompt for image generation AI. Structure: [art_style], [shot_size] shot, [camera_angle] angle, [character appearance keywords + action + position], [props and spatial relation], [scene/background], [lighting direction + color temperature], [color palette], [depth of field], [quality boosters e.g. masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting]",
    "video_prompt": "Detailed English prompt for video generation AI (Kling/Seedance). Must start with camera movement. Structure: [camera movement trajectory + speed], [subject dynamic action + direction + rhythm], [scene dynamic elements: wind/particles/cloth/fire/etc], [lighting changes if any], [motion pace], [mood/atmosphere keywords]",
    "negative_prompt": "Scene-specific negative prompt. Include anatomy/quality terms if characters are present, style-conflict terms based on art style, and shot-specific avoidance terms (e.g. static pose for action shots, wide shot for close-ups, overexposed for night scenes).",
    "narration": "中文旁白（必填，严禁镜头语言）",
    "dialogue": "角色名：台词（无对话则为空字符串 \"\"）",
    "camera_type": "pan",
    "camera_angle": "eye_level",
    "shot_size": "medium",
    "duration": 5.0,
    "location": "场景地点",
    "time_of_day": "morning",
    "weather": "clear",
    "lighting": "natural",
    "characters": [{"name":"角色名","expression":"表情","pose":"姿势动作"}],
    "transition": "cut",
    "sfx_tags": ["rain_heavy", "thunder", "forest_ambient"]
  },
  {
    "shot_no": 2,
    "description": "English description for second shot...",
    "image_prompt": "ancient Chinese xianxia, cinematic photography, close-up shot, low angle, young male cultivator (sharp features, long black hair, white hanfu with gold trim) foreground-center raising glowing azure sword overhead with both hands, enemy elder in black armor background-right arms extended casting dark energy tendrils, crumbling stone arena floor, ancient stone pillars framing scene, dramatic cool blue rim lighting from upper-left, cool blue and silver palette, shallow depth of field 85mm f/2.8, masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting",
    "video_prompt": "slow cinematic push in toward subject at 0.3x speed, young cultivator dramatically raises glowing azure sword overhead in a powerful upswing motion, white robes and sleeve fabric billowing upward from the force, azure sword energy particles trail behind the blade arc, enemy dark energy tendrils retreating frame-right, magic light intensifying with each frame, cool blue magical glow pulsing rhythmically, stone debris still settling from previous impact, building dramatic tension, epic fantasy atmosphere, fluid smooth motion",
    "negative_prompt": "ugly, deformed, extra limbs, bad anatomy, malformed hands, missing fingers, fused fingers, poorly drawn face, asymmetrical eyes, blurry, low quality, watermark, text, cartoon, anime, flat colors, modern clothes, gun, sci-fi elements, static pose, standing still, peaceful expression, wide shot, crowd",
    "narration": "第二个镜头的中文旁白",
    "dialogue": "",
    "camera_type": "zoom",
    "camera_angle": "low",
    "shot_size": "close_up",
    "duration": 4.0,
    "location": "场景地点",
    "time_of_day": "morning",
    "weather": "clear",
    "lighting": "dramatic",
    "characters": [{"name":"角色名","expression":"表情","pose":"姿势动作"}],
    "transition": "cut",
    "sfx_tags": ["qi_surge", "energy_explosion", "spiritual_pressure"]
  }
  ... （继续输出剩余 %d 个分镜，格式与上述完全相同）
]`, expectedShots, expectedShots-2))
	} else {
		sb.WriteString(fmt.Sprintf(`格式示例（以下仅展示2个分镜作为格式参考，实际须生成%d个）：
[
  {
    "shot_no": 1,
    "description": "画面描述。必须包含：① 角色站位（前景/后景，左/中/右）和姿态 ② 关键道具及与角色空间关系 ③ 场景环境 ④ 光线与构图",
    "image_prompt": "图片生成提示词。结构：[画风]，[景别] shot，[角度] angle，[主体描述：角色外貌+动作+位置]，[道具与空间关系]，[场景/背景]，[光源方向+色温]，[色调]，[景深/焦段]，[画质词 masterpiece, best quality, ultra-detailed...]",
    "video_prompt": "视频生成提示词。必须以镜头运动开头。结构：[镜头运动轨迹+速度]，[主体动态行为：动作+方向+节奏]，[场景动态元素：风/粒子/布料/火等]，[光线变化]，[时间流逝感]，[情绪关键词]",
    "negative_prompt": "负向提示词。有角色时加解剖/画质类词；根据画风加风格相悖词；加本镜头特有规避词。",
    "narration": "中文旁白（必填，严禁镜头语言）",
    "dialogue": "角色名：台词（无对话则为空字符串 \"\"）",
    "camera_type": "pan",
    "camera_angle": "eye_level",
    "shot_size": "medium",
    "duration": 5.0,
    "location": "场景地点",
    "time_of_day": "morning",
    "weather": "clear",
    "lighting": "natural",
    "characters": [{"name":"角色名","expression":"表情","pose":"姿势动作"}],
    "transition": "cut",
    "sfx_tags": ["rain_heavy", "thunder", "forest_ambient"]
  },
  {
    "shot_no": 2,
    "description": "第二个镜头的画面描述...",
    "image_prompt": "古风仙侠，电影摄影，近景，低角度，年轻男修士（轮廓鲜明，长发玉簪，白色汉服金边）前景居中双手高举发光蓝剑，黑甲老者后景右方双臂展开施放黑色能量丝线，破碎石场地面，古石柱构框，左上方寒蓝逆光，清冷蓝银色调，浅景深85mm f/2.8，masterpiece, best quality, ultra-detailed, 8k uhd, sharp focus, photorealistic, cinematic lighting",
    "video_prompt": "慢速电影推镜以0.3x速度靠近主体，年轻修士戏剧性高举发光蓝剑大幅上扬，白袍袖摆随力道向上飘扬，蓝剑能量粒子沿剑弧飘散，敌方黑色能量丝线在右侧消散，魔法光芒随每帧增强，寒蓝魔光节律脉动，石块碎片从冲击处仍在缓慢沉落，积聚戏剧张力，史诗奇幻氛围，流畅柔滑运动",
    "negative_prompt": "丑陋，变形，多余肢体，骨骼异常，手型异常，缺指，并指，面部绘制粗糙，眼睛不对称，模糊，低质量，水印，文字，卡通，动漫，平涂，现代服装，枪，科幻元素，静止姿势，站立不动，平静表情，广角镜头，人群",
    "narration": "第二个镜头的中文旁白",
    "dialogue": "",
    "camera_type": "zoom",
    "camera_angle": "low",
    "shot_size": "close_up",
    "duration": 4.0,
    "location": "场景地点",
    "time_of_day": "morning",
    "weather": "clear",
    "lighting": "dramatic",
    "characters": [{"name":"角色名","expression":"表情","pose":"姿势动作"}],
    "transition": "cut",
    "sfx_tags": ["qi_surge", "energy_explosion", "spiritual_pressure"]
  }
  ... （继续输出剩余 %d 个分镜，格式与上述完全相同）
]`, expectedShots, expectedShots-2))
	}

	return sb.String()
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

	prompt := buildStoryboardReviewPrompt(shots, previousScore)

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
// previousScore > 0 时注入上次评分上下文，引导模型给出更稳定的相对评分
func buildStoryboardReviewPrompt(shots []*model.StoryboardShot, previousScore float64) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("你是资深影视分镜审查师，请从专业角度对以下%d个分镜脚本进行全面审查，返回JSON格式的审查报告。\n\n", len(shots)))

	sb.WriteString(`【审查标准】
1. 叙事连贯性（30分）：故事逻辑、场景衔接、情节推进是否自然流畅
2. 视觉多样性（20分）：景别、镜头类型、角度是否合理变化，避免长时间单调重复
3. 节奏控制（20分）：单镜时长分配是否匹配场景强度，快慢张弛有度
4. 旁白质量（20分）：旁白是否生动感人、避免镜头语言、符合叙事视角、无重复
5. 整体专业度（10分）：构图合理性、转场设计、角色调度

【分镜数据】
`)

	for _, shot := range shots {
		narr := shot.Narration
		if narr == "" {
			narr = shot.Description
		}
		// 截断过长的旁白
		if len([]rune(narr)) > 80 {
			runes := []rune(narr)
			narr = string(runes[:80]) + "…"
		}
		sb.WriteString(fmt.Sprintf("[镜%d] 景别:%s 时长:%.0fs 镜头:%s/%s",
			shot.ShotNo, shot.ShotSize, shot.Duration, shot.CameraType, shot.CameraAngle))
		if narr != "" {
			sb.WriteString(fmt.Sprintf(" 旁白:\"%s\"", narr))
		}
		if shot.Dialogue != "" {
			d := shot.Dialogue
			if len([]rune(d)) > 40 {
				d = string([]rune(d)[:40]) + "…"
			}
			sb.WriteString(fmt.Sprintf(" 台词:\"%s\"", d))
		}
		sb.WriteString("\n")
	}

	if previousScore > 0 {
		sb.WriteString(fmt.Sprintf(`
【历史评分参考】
上一版本分镜的整体评分为 %.1f/100。请以此为基准进行相对评估：若本版本有明显改善，评分可适当提高；若改善有限甚至倒退，评分应相应降低或维持。
`, previousScore))
	}

	sb.WriteString(`
【输出格式】
返回 JSON 对象（不含 markdown 代码块）：
{
  "overall_score": 85,
  "narrative_score": 28,
  "visual_diversity_score": 17,
  "pacing_score": 18,
  "narration_score": 16,
  "professionalism_score": 6,
  "global_suggestions": ["改进建议1", "改进建议2"],
  "shot_feedback": [
    {
      "shot_no": 3,
      "severity": "high",
      "issues": ["问题描述"],
      "suggestion": "改进建议"
    }
  ]
}`)

	return sb.String()
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
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("你是专业分镜优化师。以下是%d个分镜和AI审查报告，请根据改进建议优化分镜，只输出需要修改的镜头。\n\n", len(shots)))

	sb.WriteString("【当前分镜】\n")
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

	if len(review.GlobalSuggestions) > 0 {
		sb.WriteString("\n【整体改进建议】\n")
		for _, s := range review.GlobalSuggestions {
			sb.WriteString("- " + s + "\n")
		}
	}

	if len(review.ShotFeedback) > 0 {
		sb.WriteString("\n【逐镜问题反馈】\n")
		for _, fb := range review.ShotFeedback {
			sb.WriteString(fmt.Sprintf("[镜%d] 严重度:%s\n", fb.ShotNo, fb.Severity))
			for _, issue := range fb.Issues {
				sb.WriteString("  问题: " + issue + "\n")
			}
			if fb.Suggestion != "" {
				sb.WriteString("  建议: " + fb.Suggestion + "\n")
			}
		}
	}

	sb.WriteString(`
【输出要求】
- 只输出需要修改的镜头，无需修改的镜头不要输出
- 每个字段只有在有实质性改动时才输出，不改的字段不要输出
- description 用英文，其余字段用中文
- camera_type 取值: static/push/pull/pan/track/crane_up/crane_down/follow/arc/tilt/whip_pan/zoom
- camera_angle 取值: eye_level/low/high/dutch
- shot_size 取值: wide/medium/close_up/extreme_close_up
- transition 取值: cut/fade/dissolve/wipe
- duration 单位为秒（数字）

返回 JSON，不包含 markdown 代码块：
{
  "optimized_shots": [
    {
      "shot_no": 3,
      "narration": "改进后的旁白",
      "camera_type": "pan"
    }
  ]
}`)

	return sb.String()
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

	// 标记审查记录为已应用
	if s.reviewRecordRepo != nil && recordID > 0 {
		if rec, err := s.reviewRecordRepo.GetByID(recordID); err == nil {
			rec.Status = "applied"
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
