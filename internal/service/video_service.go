package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

type VideoService struct {
	videoRepo          *repository.VideoRepository
	storyboardRepo     *repository.StoryboardRepository
	chapterRepo        *repository.ChapterRepository
	characterRepo      *repository.CharacterRepository
	novelRepo          *repository.NovelRepository
	tenantRepo         *repository.TenantRepository
	aiService          *AIService
	videoProviders     map[string]ai.VideoProvider
	consistencyService *CharacterConsistencyService
	bgmService         *BGMService
	sfxService         *SFXService
	storageSvc         storage.Service
	sceneAnchorSvc     *SceneAnchorService
	plotPointRepo      *repository.PlotPointRepository
	systemSettingRepo  *repository.SystemSettingRepository
	segmentRepo        *repository.ShotVoiceSegmentRepository
	reviewRecordRepo   *repository.StoryboardReviewRecordRepository
	taskSvc            *TaskService
	videoSem           chan struct{} // nil = unlimited; set via WithVideoConcurrency
	videoSemMu         sync.RWMutex  // protects videoSem replacement
	audioSem           chan struct{} // limits concurrent TTS calls; nil = unlimited
	charListCache      sync.Map      // novelID → *charListEntry (short-lived cache for batch voice gen)
	// 广场社交
	videoLikeRepo    *repository.VideoLikeRepository
	videoCommentRepo *repository.VideoCommentRepository
	viewDedupCache   sync.Map // key "ip:id" → expiry time.Time（防刷播放量）
}

// charListEntry is a TTL-bounded cache entry for ListByNovel results.
type charListEntry struct {
	chars     []*model.Character
	expiresAt time.Time
}

// listCharsByNovelCached returns the character list for a novel, using a 60-second
// in-process cache to avoid repeated DB calls during batch voice generation.
func (s *VideoService) listCharsByNovelCached(novelID uint) ([]*model.Character, error) {
	if v, ok := s.charListCache.Load(novelID); ok {
		if entry := v.(*charListEntry); time.Now().Before(entry.expiresAt) {
			return entry.chars, nil
		}
	}
	chars, err := s.characterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	s.charListCache.Store(novelID, &charListEntry{
		chars:     chars,
		expiresAt: time.Now().Add(60 * time.Second),
	})
	return chars, nil
}

// GetNovelByID 通过 novelRepo 加载小说（供 handler 传递给 CapCutService 等下游服务）
func (s *VideoService) GetNovelByID(id uint) (*model.Novel, error) {
	return s.novelRepo.GetByID(id)
}

// GetNovelVideoConfig 获取小说的视频配置（供 handler 层使用）
func (s *VideoService) GetNovelVideoConfig(novelID uint) *model.NovelVideoConfig {
	if s.novelRepo == nil {
		return nil
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil || novel == nil {
		return nil
	}
	return novel.VideoConfig
}

func (s *VideoService) WithSystemSettingRepo(r *repository.SystemSettingRepository) *VideoService {
	s.systemSettingRepo = r
	return s
}

// WithVideoConcurrency 设置视频生成的最大并发数（启动时调用）。
func (s *VideoService) WithVideoConcurrency(n int) *VideoService {
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	}
	return s
}

// WithAudioConcurrency 设置 TTS 音频生成的最大并发数。
// 默认不限制；推荐设置为 3，防止批量生成时触发 API 限速（429）。
func (s *VideoService) WithAudioConcurrency(n int) *VideoService {
	if n > 0 {
		s.audioSem = make(chan struct{}, n)
	}
	return s
}

// SetVideoConcurrency 运行时动态调整视频并发度（线程安全）。
func (s *VideoService) SetVideoConcurrency(n int) {
	s.videoSemMu.Lock()
	defer s.videoSemMu.Unlock()
	if n > 0 {
		s.videoSem = make(chan struct{}, n)
	} else {
		s.videoSem = nil
	}
}

func (s *VideoService) WithReviewRecordRepo(r *repository.StoryboardReviewRecordRepository) *VideoService {
	s.reviewRecordRepo = r
	return s
}

func (s *VideoService) WithSegmentRepo(r *repository.ShotVoiceSegmentRepository) *VideoService {
	s.segmentRepo = r
	return s
}

func (s *VideoService) WithTaskService(svc *TaskService) *VideoService {
	s.taskSvc = svc
	return s
}

func (s *VideoService) WithSceneAnchorService(svc *SceneAnchorService) *VideoService {
	s.sceneAnchorSvc = svc
	return s
}

func (s *VideoService) WithPlotPointRepo(repo *repository.PlotPointRepository) *VideoService {
	s.plotPointRepo = repo
	return s
}

func NewVideoService(
	videoRepo *repository.VideoRepository,
	storyboardRepo *repository.StoryboardRepository,
	chapterRepo *repository.ChapterRepository,
	characterRepo *repository.CharacterRepository,
	novelRepo *repository.NovelRepository,
	tenantRepo *repository.TenantRepository,
	aiService *AIService,
	videoProviders map[string]ai.VideoProvider,
) *VideoService {
	return &VideoService{
		videoRepo:      videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		characterRepo:  characterRepo,
		novelRepo:      novelRepo,
		tenantRepo:     tenantRepo,
		aiService:      aiService,
		videoProviders: videoProviders,
	}
}

// WithConsistencyService 设置一致性服务（选填）
func (s *VideoService) WithConsistencyService(cs *CharacterConsistencyService) *VideoService {
	s.consistencyService = cs
	return s
}

// WithBGMService 设置BGM服务（选填）
func (s *VideoService) WithBGMService(bgm *BGMService) *VideoService {
	s.bgmService = bgm
	return s
}

// WithSFXService 设置音效服务（选填）
func (s *VideoService) WithSFXService(sfx *SFXService) *VideoService {
	s.sfxService = sfx
	return s
}

// WithStorage 注入媒体存储服务（选填；配置 OSS 时上传至 OSS，否则存 DB）
func (s *VideoService) WithStorage(svc storage.Service) *VideoService {
	s.storageSvc = svc
	return s
}

// CreateVideoFromChapter 从章节创建视频
func (s *VideoService) CreateVideoFromChapter(novelID uint, chapterID *uint) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   chapterID,
		Title:       "新视频",
		Status:      "planning",
		FrameRate:   24,
		Resolution:  "1080p",
		AspectRatio: "16:9",
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		video.TenantID = novel.TenantID
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// CreateVideo 创建视频（接受请求对象）
func (s *VideoService) CreateVideo(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	return s.CreateVideoFromReq(novelID, req)
}

// GenerateStoryboard 生成分镜
// userPrompt: 用户自定义提示词（可选），将追加到系统 prompt 之后
// progressFn: 可选的进度回调（0-99），供调用方更新任务进度（传 nil 则跳过）
func (s *VideoService) GenerateStoryboard(videoID uint, provider, userPrompt string, progressFn func(int), overrides StoryboardOverrides, chapterIDOverride ...*uint) ([]*model.StoryboardShot, error) {
	totalStart := time.Now()

	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
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

	// 分段生成：长章节按 3500 字切割，每段独立调用 AI，合并后顺序重编号。
	// 短章节（≤3500 字）等同于原单段调用路径，行为完全一致。
	// 3500 字对应约 25 个镜头（≈5000 tokens），在 8192 MaxTokens 内有充足余量。
	const segmentMaxRunes = 3500
	segments := splitContentSegments(content, segmentMaxRunes)

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

			prompt := s.buildStoryboardPrompt(video, content, userPrompt, idx+1, len(segments), segShotCount, characters, anchors, plotPoints, nil, overrides.VoiceMode)

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
	s.videoRepo.Update(video)

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
			const runeOverlapThreshold = 0.5
			for name, id := range nameMap {
				if strings.Contains(nameLower, name) || strings.Contains(name, nameLower) ||
					charRuneOverlap(nameLower, name) >= runeOverlapThreshold {
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
		if boundary > start {
			end = boundary
		}
		segments = append(segments, string(runes[start:end]))
		start = end
	}
	return segments
}

// extractLocationFromScene 从 shot.Scene JSON 中提取 location 字符串
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
// buildStoryboardPrompt 构建分镜生成 Prompt。
// prevShots: 上一段落末尾的 N 个分镜，用于跨段落情节连贯（顺序处理时传入，并发时为 nil）。
func (s *VideoService) buildStoryboardPrompt(
	video *model.Video, content, userPrompt string,
	segNo, totalSegs, expectedShots int,
	characters []*model.Character, anchors []*model.SceneAnchor, plotPoints []*model.PlotPoint,
	prevShots []*model.StoryboardShot,
	voiceMode string,
) string {
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

▸ description（英文画面描述）
  - 仅用于 AI 图片生成的英文视觉提示词，禁止出现中文、叙事句子、心理描写
  - 必须包含以下四层信息（缺少任一层将被视为不合格）：
    ① 角色站位：出现的角色、在画框中的位置（foreground/background, left/center/right）、朝向与动作姿态
    ② 道具/物品：镜头内关键道具的位置及与角色的空间关系（如 "a sword mounted on the wall to his left"）
    ③ 场景环境：背景、地点特征、氛围细节
    ④ 光线与构图：光源方向、色调、景深感
  - 示例（全要素）："A young man in white robes stands foreground-left, gripping a bronze sword at his side, facing right. An elder in dark armor stands background-right, arms crossed. Between them, an ancient scroll rests on a stone table. Collapsed city gate visible behind the elder. Dramatic amber backlight, dusk, wide shot."

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
	default:
		// "both" or "" — default behavior, narration/dialogue mutual exclusion already specified above
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

格式示例（以下仅展示2个分镜作为格式参考，实际须生成%d个）：
[
  {
    "shot_no": 1,
    "description": "English only. Must include: ① character positions (foreground/background, left/center/right) and poses ② key props and their spatial relation to characters ③ scene environment ④ lighting and composition",
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
]`, expectedShots, expectedShots, expectedShots, expectedShots-2))

	return sb.String()
}

// parseStoryboardResult 解析AI分镜响应。解析失败时返回 error（不生成空占位）。
func (s *VideoService) parseStoryboardResult(videoID uint, chapterID *uint, result string) ([]*model.StoryboardShot, error) {
	// 提取 JSON 数组
	cleaned := extractJSON(result)

	var rawShots []struct {
		ShotNo      int     `json:"shot_no"`
		Description string  `json:"description"` // 英文画面描述（供图片/视频生成）
		Narration   string  `json:"narration"`   // 中文旁白文案（供TTS和字幕）
		Dialogue    string  `json:"dialogue"`    // 角色台词（格式："角色名：台词"）
		CameraType  string  `json:"camera_type"`
		CameraAngle string  `json:"camera_angle"`
		ShotSize    string  `json:"shot_size"`
		Duration    float64 `json:"duration"`
		Location    string  `json:"location"`
		TimeOfDay   string  `json:"time_of_day"`
		Weather     string  `json:"weather"`
		Lighting    string  `json:"lighting"`
		Characters  []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
			Pose       string `json:"pose"`
		} `json:"characters"`
		Transition string   `json:"transition"`
		SFXTags    []string `json:"sfx_tags"`
	}

	parseErr := json.Unmarshal([]byte(cleaned), &rawShots)
	if parseErr != nil || len(rawShots) == 0 {
		// 尝试修复截断 JSON（模型输出被 max_tokens 截断时常见）
		repaired := repairTruncatedJSONArray(cleaned)
		if repaired != cleaned {
			var repairedShots []struct {
				ShotNo      int     `json:"shot_no"`
				Description string  `json:"description"`
				Narration   string  `json:"narration"`
				Dialogue    string  `json:"dialogue"`
				CameraType  string  `json:"camera_type"`
				CameraAngle string  `json:"camera_angle"`
				ShotSize    string  `json:"shot_size"`
				Duration    float64 `json:"duration"`
				Location    string  `json:"location"`
				TimeOfDay   string  `json:"time_of_day"`
				Weather     string  `json:"weather"`
				Lighting    string  `json:"lighting"`
				Characters  []struct {
					Name       string `json:"name"`
					Expression string `json:"expression"`
					Pose       string `json:"pose"`
				} `json:"characters"`
				Transition string   `json:"transition"`
				SFXTags    []string `json:"sfx_tags"`
			}
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
			duration = 5.0
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

		// Prompt 用 Description 填充（供视频生成接口使用）
		// 附加摄像机和场景信息以丰富 prompt
		prompt := r.Description
		if r.CameraAngle != "" || r.ShotSize != "" {
			prompt = fmt.Sprintf("%s, %s shot, %s angle", r.Description, r.ShotSize, r.CameraAngle)
		}
		if r.Lighting != "" {
			prompt += ", " + r.Lighting + " lighting"
		}

		// 将 sfx_tags 序列化为 JSON 字符串存储
		var sfxTagsJSON string
		if len(r.SFXTags) > 0 {
			if b, err := json.Marshal(r.SFXTags); err == nil {
				sfxTagsJSON = string(b)
			}
		}

		cameraType := validCameraType(r.CameraType)
		cameraAngle := validCameraAngle(r.CameraAngle)
		shotSize := validShotSize(r.ShotSize)
		shot := &model.StoryboardShot{
			UUID:        uuid.New().String(),
			VideoID:     videoID,
			ChapterID:   chapterID,
			ShotNo:      shotNo,
			Description: r.Description,
			Narration:   r.Narration,
			Prompt:      prompt,
			MotionPrompt: buildMotionPrompt(&model.StoryboardShot{
				CameraType:  cameraType,
				CameraAngle: cameraAngle,
				ShotSize:    shotSize,
				Description: r.Description,
			}),
			Dialogue:    r.Dialogue, // 保留"角色名：台词"格式供TTS音色解析
			CameraType:  cameraType,
			CameraAngle: cameraAngle,
			ShotSize:    shotSize,
			Duration:    duration,
			Transition:  validTransition(r.Transition),
			Characters:  charsJSON,
			Scene:       sceneJSON,
			SFXTags:     sfxTagsJSON,
			Status:      "pending",
		}
		shots = append(shots, shot)
	}
	return shots, nil
}

// validTransition 验证过渡方式，无效时返回默认值 cut
func validTransition(t string) string {
	valid := map[string]bool{
		"cut": true, "j-cut": true, "l-cut": true,
		"fade": true, "dissolve": true, "dip-black": true, "dip-white": true,
		"wipe": true, "push": true, "slide": true, "zoom": true,
		"whip-pan": true, "spin": true, "flash": true, "glitch": true,
		"blur": true, "morph": true,
	}
	if valid[t] {
		return t
	}
	return "cut"
}

// buildMotionPrompt 根据分镜的摄像机设置和情绪基调，生成适合 Kling/Seedance 的运镜提示词。
// 视频AI(image-to-video)需要描述运动/镜头的英文短语，而非静态画面描述。
func buildMotionPrompt(shot *model.StoryboardShot) string {
	parts := make([]string, 0, 6)

	// 摄像机运动翻译
	switch shot.CameraType {
	case "push":
		parts = append(parts, "slow push in, camera moves forward toward subject")
	case "pull":
		parts = append(parts, "slow pull back, camera moves away from subject")
	case "pan":
		parts = append(parts, "smooth camera pan")
	case "track":
		parts = append(parts, "lateral tracking shot, camera moves sideways")
	case "crane_up":
		parts = append(parts, "crane shot moving upward, rising camera")
	case "crane_down":
		parts = append(parts, "crane shot moving downward, descending camera")
	case "follow":
		parts = append(parts, "follow shot tracking subject movement")
	case "arc":
		parts = append(parts, "arc shot, camera orbits around subject")
	case "tilt":
		parts = append(parts, "camera tilt, vertical pivot movement")
	case "whip_pan":
		parts = append(parts, "whip pan, fast swinging camera movement")
	case "zoom":
		// 根据情绪决定推拉方向
		if strings.Contains(shot.EmotionalTone, "紧张") || strings.Contains(shot.EmotionalTone, "压迫") || strings.Contains(shot.EmotionalTone, "危险") {
			parts = append(parts, "dramatic zoom in")
		} else {
			parts = append(parts, "slow zoom out")
		}
	// 向后兼容旧值
	case "tracking":
		parts = append(parts, "tracking shot following subject")
	case "dolly":
		parts = append(parts, "smooth dolly movement")
	case "crane":
		parts = append(parts, "crane shot, vertical camera movement")
	default: // static
		parts = append(parts, "static locked-off shot, subtle breathing movement")
	}

	// 镜头角度
	switch shot.CameraAngle {
	case "low":
		parts = append(parts, "low angle looking up")
	case "high":
		parts = append(parts, "high angle bird's eye view")
	case "dutch":
		parts = append(parts, "dutch angle tilted frame")
	}

	// 景别影响景深和焦距
	switch shot.ShotSize {
	case "extreme_close_up":
		parts = append(parts, "extreme close-up, shallow depth of field, bokeh background")
	case "close_up":
		parts = append(parts, "close-up shot, shallow depth of field")
	case "wide":
		parts = append(parts, "wide establishing shot")
	}

	// 注入镜头焦距特征，提升视频生成中的光学真实感
	switch shot.ShotSize {
	case "extreme_close_up":
		parts = append(parts, "macro photography, f/1.8 aperture, extreme bokeh, subject fills frame")
	case "close_up":
		parts = append(parts, "portrait photography, 85mm equivalent, f/2.8, soft background separation")
	case "medium":
		parts = append(parts, "50mm equivalent, f/5.6, natural perspective")
	case "wide", "extreme_wide":
		parts = append(parts, "wide angle, 24mm equivalent, deep focus, environmental storytelling")
	}

	// 情绪基调 → 运动节奏
	tone := shot.EmotionalTone
	switch {
	case strings.Contains(tone, "紧张") || strings.Contains(tone, "战斗") || strings.Contains(tone, "危险"):
		parts = append(parts, "fast dynamic motion, high energy")
	case strings.Contains(tone, "悲") || strings.Contains(tone, "离别") || strings.Contains(tone, "伤"):
		parts = append(parts, "slow melancholic movement, gentle sway")
	case strings.Contains(tone, "浪漫") || strings.Contains(tone, "温情"):
		parts = append(parts, "soft gentle movement, warm atmosphere")
	case strings.Contains(tone, "史诗") || strings.Contains(tone, "宏大") || strings.Contains(tone, "壮观"):
		parts = append(parts, "epic cinematic sweep")
	default:
		parts = append(parts, "natural fluid movement")
	}

	// 大气元素（根据情绪增加环境细节）
	switch {
	case strings.Contains(tone, "紧张") || strings.Contains(tone, "战斗") || strings.Contains(tone, "危险"):
		parts = append(parts, "atmospheric smoke, dynamic lighting")
	case strings.Contains(tone, "悲") || strings.Contains(tone, "离别"):
		parts = append(parts, "soft diffused light, gentle lens flare")
	case strings.Contains(tone, "史诗") || strings.Contains(tone, "宏大"):
		parts = append(parts, "volumetric god rays, epic atmosphere, dust particles in light")
	case strings.Contains(tone, "浪漫") || strings.Contains(tone, "温情"):
		parts = append(parts, "warm golden hour light, soft bokeh, romantic haze")
	}

	// 添加简短的场景描述（前40字符）
	if shot.Description != "" {
		desc := shot.Description
		runes := []rune(desc)
		if len(runes) > 40 {
			desc = string(runes[:40])
		}
		parts = append(parts, desc)
	}

	return strings.Join(parts, ", ")
}

// qualityTierImageParams 根据视频质量档位返回图片生成参数。
// draft:   512px / 20步 / CFG5   — 快速预览，适合脚本确认阶段
// preview: 1024px / 35步 / CFG7  — 默认，平衡质量与速度
// final:   2048px / 50步 / CFG8.5 — 最高质量，正式输出
func qualityTierImageParams(tier string) (width, steps int, cfgScale float64) {
	switch tier {
	case "draft":
		return 512, 20, 5.0
	case "final":
		return 2048, 50, 8.5
	default: // preview (default)
		return 1024, 35, 7.0
	}
}

// validCameraType 验证摄像机类型，无效时返回默认值 "static"。
// 注意：如日志出现 "invalid camera_type"，说明 AI 输出了管道分隔枚举字符串或其他非法值，需检查 Prompt 格式。
func validCameraType(t string) string {
	valid := map[string]bool{
		// 当前规范值（10种常见运镜）
		"static": true, "push": true, "pull": true, "pan": true,
		"track": true, "crane_up": true, "crane_down": true,
		"follow": true, "arc": true, "tilt": true, "whip_pan": true, "zoom": true,
		// 向后兼容旧值
		"tracking": true, "dolly": true, "crane": true,
	}
	if valid[t] {
		return t
	}
	if t != "" {
		logger.Printf("validCameraType: invalid camera_type=%q, falling back to static", t)
	}
	return "static"
}

// validCameraAngle 验证摄像机角度
func validCameraAngle(a string) string {
	valid := map[string]bool{"eye_level": true, "high": true, "low": true, "dutch": true, "overhead": true, "POV": true}
	if valid[a] {
		return a
	}
	return "eye_level"
}

// validShotSize 验证镜头尺寸
func validShotSize(s string) string {
	valid := map[string]bool{"extreme_wide": true, "wide": true, "full": true, "medium": true, "close_up": true, "extreme_close_up": true}
	if valid[s] {
		return s
	}
	return "medium"
}

// CreateVideo handler-compatible wrapper
func (s *VideoService) CreateVideoFromReq(novelID uint, req *model.CreateVideoRequest) (*model.Video, error) {
	video := &model.Video{
		UUID:        uuid.New().String(),
		NovelID:     novelID,
		ChapterID:   req.ChapterID,
		Title:       req.Title,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		AspectRatio: req.AspectRatio,
		ArtStyle:    req.ArtStyle,
		QualityTier: req.QualityTier,
		Mode:        req.Mode,
		VisualMode:  req.VisualMode,
		ThreeDStyle: req.ThreeDStyle,
		Status:      "planning",
	}
	if novel, err := s.novelRepo.GetByID(novelID); err == nil {
		video.TenantID = novel.TenantID
		// 默认画面风格：继承项目设置中的画面风格
		if video.ArtStyle == "" && novel.ImageStyle != "" {
			video.ArtStyle = novel.ImageStyle
		}
	}
	if video.FrameRate == 0 {
		video.FrameRate = 24
	}
	if video.Resolution == "" {
		video.Resolution = "1080p"
	}
	if video.AspectRatio == "" {
		video.AspectRatio = "16:9"
	}
	if video.QualityTier == "" {
		video.QualityTier = "preview"
	}
	if video.Mode == "" {
		video.Mode = "slideshow"
	}
	return video, s.videoRepo.Create(video)
}

// GetVideo 获取视频（内部调用，无租户隔离）
func (s *VideoService) GetVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetByID(id)
}

// GetVideoByTenant 获取视频（带租户隔离，防止越权访问）
func (s *VideoService) GetVideoByTenant(id, tenantID uint) (*model.Video, error) {
	return s.videoRepo.GetByIDAndTenant(id, tenantID)
}

// ListVideos 获取视频列表
func (s *VideoService) ListVideos(novelId *uint, chapterID *uint, status string, tenantID uint, page, pageSize int) ([]*model.Video, int, error) {
	videos, total, err := s.videoRepo.List(novelId, chapterID, tenantID, page, pageSize)
	return videos, int(total), err
}

// UpdateVideo 更新视频
func (s *VideoService) UpdateVideo(id uint, req *model.UpdateVideoRequest) (*model.Video, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Title != "" {
		video.Title = req.Title
	}
	if req.Resolution != "" {
		video.Resolution = req.Resolution
	}
	if req.FrameRate != 0 {
		video.FrameRate = req.FrameRate
	}
	if req.AspectRatio != "" {
		video.AspectRatio = req.AspectRatio
	}
	if req.ArtStyle != "" {
		video.ArtStyle = req.ArtStyle
	}
	if req.ScriptStatus != "" {
		video.ScriptStatus = req.ScriptStatus
	}
	if req.Mode != "" {
		video.Mode = req.Mode
	}
	if req.VisualMode != "" {
		video.VisualMode = req.VisualMode
	}
	if req.ThreeDStyle != "" {
		video.ThreeDStyle = req.ThreeDStyle
	}
	return video, s.videoRepo.Update(video)
}

// UpdatePacingConfig 更新视频的节奏和目标时长配置（供分镜生成前调用）
func (s *VideoService) UpdatePacingConfig(id uint, pacing string, targetDuration int) error {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return err
	}
	if pacing != "" {
		video.Pacing = pacing
	}
	video.TargetDuration = targetDuration
	return s.videoRepo.Update(video)
}

// UpdateVideoFields 更新视频任意字段（用于发布状态更新）
func (s *VideoService) UpdateVideoFields(id uint, fields map[string]interface{}) error {
	return s.videoRepo.UpdateFields(id, fields)
}

// WithVideoSocial 注入广场社交仓库
func (s *VideoService) WithVideoSocial(likeRepo *repository.VideoLikeRepository, commentRepo *repository.VideoCommentRepository) *VideoService {
	s.videoLikeRepo = likeRepo
	s.videoCommentRepo = commentRepo
	go s.cleanupViewDedup()
	return s
}

// cleanupViewDedup 每小时扫描并清除已过期的去重条目，防止 sync.Map 无限增长。
func (s *VideoService) cleanupViewDedup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.viewDedupCache.Range(func(k, v any) bool {
			if expiry, ok := v.(time.Time); ok && now.After(expiry) {
				s.viewDedupCache.Delete(k)
			}
			return true
		})
	}
}

// IncrVideoViewCount 增加视频播放量
func (s *VideoService) IncrVideoViewCount(id uint) error {
	return s.videoRepo.IncrViewCount(id)
}

// RecordViewDeduped 防刷播放量（同一 IP 对同一视频 1 小时内只计一次）
func (s *VideoService) RecordViewDeduped(id uint, clientIP string) error {
	key := fmt.Sprintf("%s:%d", clientIP, id)
	if v, ok := s.viewDedupCache.Load(key); ok {
		if expiry, ok2 := v.(time.Time); ok2 && time.Now().Before(expiry) {
			return nil // 已记录，跳过
		}
	}
	s.viewDedupCache.Store(key, time.Now().Add(time.Hour))
	return s.videoRepo.IncrViewCount(id)
}

// GetPublicVideo 获取单条公开视频（无需 tenantID）
func (s *VideoService) GetPublicVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetPublicByID(id)
}

// ListPublicVideos 列出公开视频（支持排序 latest|hot 和关键词搜索）
func (s *VideoService) ListPublicVideos(sort, q string, page, pageSize int) ([]*model.Video, int64, error) {
	if sort == "" {
		sort = "hot"
	}
	return s.videoRepo.ListPublicSorted(sort, q, page, pageSize)
}

// ToggleVideoLike 点赞/取消点赞，返回最终状态
func (s *VideoService) ToggleVideoLike(videoID, userID uint) (liked bool, err error) {
	if s.videoLikeRepo == nil {
		return false, fmt.Errorf("like feature not available")
	}
	liked, err = s.videoLikeRepo.Toggle(videoID, userID)
	if err != nil {
		return false, err
	}
	delta := 1
	if !liked {
		delta = -1
	}
	_ = s.videoRepo.IncrLikeCount(videoID, delta)
	return liked, nil
}

// IsVideoLiked 检查用户是否已点赞
func (s *VideoService) IsVideoLiked(videoID, userID uint) bool {
	if s.videoLikeRepo == nil {
		return false
	}
	exists, _ := s.videoLikeRepo.Exists(videoID, userID)
	return exists
}

// ListVideoComments 获取评论列表
func (s *VideoService) ListVideoComments(videoID uint, page, size int) ([]*model.VideoComment, int64, error) {
	if s.videoCommentRepo == nil {
		return []*model.VideoComment{}, 0, nil
	}
	return s.videoCommentRepo.ListByVideo(videoID, page, size)
}

// AddVideoComment 发表评论
func (s *VideoService) AddVideoComment(videoID, userID uint, nickname, content string, parentID *uint) (*model.VideoComment, error) {
	if s.videoCommentRepo == nil {
		return nil, fmt.Errorf("comment feature not available")
	}
	c := &model.VideoComment{
		VideoID:  videoID,
		UserID:   userID,
		Nickname: nickname,
		Content:  content,
		ParentID: parentID,
	}
	if err := s.videoCommentRepo.Create(c); err != nil {
		return nil, err
	}
	_ = s.videoRepo.IncrCommentCount(videoID, 1)
	return c, nil
}

// DeleteVideoComment 删除评论（只允许作者删除）
func (s *VideoService) DeleteVideoComment(commentID, callerID uint) error {
	if s.videoCommentRepo == nil {
		return fmt.Errorf("comment feature not available")
	}
	c, err := s.videoCommentRepo.GetByID(commentID)
	if err != nil {
		return err
	}
	if c.UserID != callerID {
		return ErrPermissionDenied
	}
	if err := s.videoCommentRepo.Delete(commentID); err != nil {
		return err
	}
	_ = s.videoRepo.IncrCommentCount(c.VideoID, -1)
	return nil
}

// RecalcVideoHotScores 重新计算所有公开视频热度分（定时任务调用）
// HotScore = view_count×0.5 + like_count×0.3 + comment_count×0.2，叠加时间衰减
func (s *VideoService) RecalcVideoHotScores() error {
	videos, err := s.videoRepo.ListPublicForHotCalc()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, v := range videos {
		ageDays := 1.0
		if v.PublishedAt != nil {
			ageDays = now.Sub(*v.PublishedAt).Hours()/24 + 1
		}
		// 简单时间衰减：分母随天数增长
		decay := 1.0 / (1.0 + ageDays*0.1)
		score := (float64(v.ViewCount)*0.5 + float64(v.LikeCount)*0.3 + float64(v.CommentCount)*0.2) * decay
		if err := s.videoRepo.UpdateHotScore(v.ID, score); err != nil {
			logger.Printf("RecalcVideoHotScores: failed to update video %d: %v", v.ID, err)
		}
	}
	return nil
}

// DeleteVideo 删除视频
func (s *VideoService) DeleteVideo(id uint) error {
	return s.videoRepo.DeleteByID(id)
}

// StartGeneration 开始生成视频（调用真实视频 Provider）
func (s *VideoService) StartGeneration(id uint) (string, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return "", err
	}

	// 租户状态校验
	if err := s.checkTenantAccess(video.NovelID); err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		s.videoRepo.Update(video)
		return "", err
	}

	// 选择 provider：优先 kling，其次 seedance，均无则返回错误
	providerName := "kling"
	provider, ok := s.videoProviders[providerName]
	if !ok {
		providerName = "seedance"
		provider, ok = s.videoProviders[providerName]
	}
	if !ok {
		// 无可用 provider：标记失败并返回错误
		video.Status = "failed"
		video.ErrorMessage = "no video provider configured (set KLING_API_KEY or SEEDANCE_API_KEY)"
		s.videoRepo.Update(video)
		return "", fmt.Errorf("no video provider configured")
	}

	// 构建生成请求
	req := &ai.VideoGenerateRequest{
		Prompt:      fmt.Sprintf("%s — cinematic, high quality", video.Title),
		AspectRatio: video.AspectRatio,
		Duration:    5.0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		video.Status = "failed"
		video.ErrorMessage = err.Error()
		video.RetryCount++
		s.videoRepo.Update(video)
		return "", fmt.Errorf("video generation start failed: %w", err)
	}

	// 持久化任务 ID 和 provider 信息
	video.Status = "generating"
	video.ProviderName = providerName
	video.TaskID = task.TaskID
	video.ErrorMessage = ""
	s.videoRepo.Update(video)

	return task.TaskID, nil
}

// VideoProvider is a minimal video provider descriptor used by the listing endpoint.
type VideoProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// ListVideoProviders returns configured video providers with display names.
func (s *VideoService) ListVideoProviders() []VideoProvider {
	result := make([]VideoProvider, 0, len(s.videoProviders))
	for name := range s.videoProviders {
		result = append(result, VideoProvider{Name: name, DisplayName: capableProviderDisplayName(name, "")})
	}
	return result
}

// GetStoryboard 获取分镜列表
func (s *VideoService) GetStoryboard(videoID uint) ([]*model.StoryboardShot, error) {
	return s.storyboardRepo.ListByVideo(videoID)
}

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
【上次审查参考】
上次综合评分：%.1f / 10
本次是对同一脚本应用改动后的复核审查。请根据当前脚本内容客观评分，若改动带来实质提升则分数应上升，若引入新问题则下降，若无显著变化则保持相近。在 summary 中用一句话说明相比上次的主要变化方向。
`, previousScore))
	}

	sb.WriteString(`
【输出格式】
返回JSON对象，不要包含markdown代码块或任何额外说明：
{
  "overall_score": 7.5,
  "narrative_score": 8.0,
  "visual_score": 6.5,
  "pacing_score": 7.0,
  "narration_score": 8.0,
  "summary": "总体评价（100字以内）",
  "strengths": ["亮点描述1", "亮点描述2"],
  "weaknesses": ["主要问题描述1", "主要问题描述2"],
  "global_suggestions": ["整体改进建议1", "整体改进建议2", "整体改进建议3"],
  "shot_feedback": [
    {
      "shot_no": 3,
      "issues": ["旁白包含镜头语言：'镜头推近'"],
      "suggestion": "改为描述人物状态而非摄像机动作",
      "severity": "warning",
      "suggested_narration": "凌云眼神逐渐坚定，握紧了手中的剑，沉声道：'今日，我必踏上那座山峰。'",
      "suggested_description": "CU 凌云面部特写，眼神坚毅，手握剑柄，浅景深。"
    }
  ]
}
规则：
- shot_feedback 只需列出有问题的镜头（不超过10个最典型的），无问题镜头无需列出
- severity 取值：error（严重）、warning（中等）、info（轻微）
- suggested_narration：针对旁白问题，给出修改后的完整旁白文本；无旁白问题则省略此字段
- suggested_description：针对视觉描述问题，给出修改后的完整画面描述；无视觉问题则省略此字段`)

	return sb.String()
}

// parseStoryboardReview 解析 AI 审查结果为结构化报告
func parseStoryboardReview(result string) (*model.StoryboardReview, error) {
	cleaned := extractJSONObject(result)

	var review model.StoryboardReview
	if err := json.Unmarshal([]byte(cleaned), &review); err != nil {
		return nil, fmt.Errorf("审查报告解析失败: %w; AI原始响应(前300字符): %.300s", err, result)
	}
	return &review, nil
}

// OptimizeStoryboardFromReview 根据 AI 审查报告一键优化分镜。
// 将当前分镜 + 全局建议 + 逐镜反馈发送给 AI，AI 返回需要修改的镜头列表，
// 逐个调用 UpdateShotPartial 落库。返回实际修改的镜头数。
func (s *VideoService) OptimizeStoryboardFromReview(tenantID, videoID uint, review *model.StoryboardReview, provider string) (int, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return 0, fmt.Errorf("获取分镜失败: %w", err)
	}
	if len(shots) == 0 {
		return 0, fmt.Errorf("该视频暂无分镜")
	}
	if len(review.GlobalSuggestions) == 0 && len(review.ShotFeedback) == 0 {
		return 0, fmt.Errorf("审查报告中无改进建议")
	}

	var novelID uint
	if video, err := s.videoRepo.GetByID(videoID); err == nil {
		novelID = video.NovelID
	}

	prompt := buildStoryboardOptimizePrompt(shots, review)
	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "storyboard_optimize", prompt, provider)
	if err != nil {
		return 0, fmt.Errorf("AI优化失败: %w", err)
	}

	updates, err := parseOptimizedShots(result)
	if err != nil {
		return 0, fmt.Errorf("优化结果解析失败: %w", err)
	}

	// 建立 shot_no → ID 索引
	shotIndex := make(map[int]uint, len(shots))
	for _, sh := range shots {
		shotIndex[sh.ShotNo] = sh.ID
	}

	applied := 0
	for _, upd := range updates {
		id, ok := shotIndex[upd.ShotNo]
		if !ok {
			logger.Printf("OptimizeStoryboardFromReview: unknown shot_no %d, skipping", upd.ShotNo)
			continue
		}
		fields := upd.toFieldMap()
		if len(fields) == 0 {
			continue
		}
		if _, err := s.UpdateShotPartial(id, fields); err != nil {
			logger.Printf("OptimizeStoryboardFromReview: UpdateShotPartial shot_no=%d: %v", upd.ShotNo, err)
			continue
		}
		applied++
	}
	return applied, nil
}

// ShotApplyDiff 用户选中要应用的单镜修改（来自审查报告的 suggested 字段）。
type ShotApplyDiff struct {
	ShotID      uint   `json:"shot_id"`
	ShotNo      int    `json:"shot_no"`
	Narration   string `json:"narration,omitempty"`
	Description string `json:"description,omitempty"`
}

// ApplyStoryboardDiffs 将用户选中的差异直接写入 DB（无 AI 调用，同步执行）。
// recordID > 0 时：在写入前保存回滚快照，并将记录状态更新为 applied。
func (s *VideoService) ApplyStoryboardDiffs(videoID uint, diffs []ShotApplyDiff, recordID uint) (int, error) {
	if len(diffs) == 0 {
		return 0, nil
	}
	// 若 shot_id 为 0，尝试通过 shot_no 反查
	shots, _ := s.storyboardRepo.ListByVideo(videoID)
	noToID := make(map[int]uint, len(shots))
	shotByID := make(map[uint]*model.StoryboardShot, len(shots))
	for _, sh := range shots {
		noToID[sh.ShotNo] = sh.ID
		shotByID[sh.ID] = sh
	}

	// 应用前采集回滚快照（仅受影响的镜头）
	var snapshot []model.ShotRollbackItem
	for _, d := range diffs {
		id := d.ShotID
		if id == 0 {
			id = noToID[d.ShotNo]
		}
		if id == 0 {
			continue
		}
		if sh, ok := shotByID[id]; ok {
			snapshot = append(snapshot, model.ShotRollbackItem{
				ShotID:      sh.ID,
				ShotNo:      sh.ShotNo,
				Narration:   sh.Narration,
				Description: sh.Description,
			})
		}
	}

	applied := 0
	for _, d := range diffs {
		id := d.ShotID
		if id == 0 {
			id = noToID[d.ShotNo]
		}
		if id == 0 {
			logger.Printf("ApplyStoryboardDiffs: cannot resolve shot_no=%d, skipping", d.ShotNo)
			continue
		}
		fields := map[string]interface{}{}
		if d.Narration != "" {
			fields["narration"] = d.Narration
		}
		if d.Description != "" {
			fields["description"] = d.Description
		}
		if len(fields) == 0 {
			continue
		}
		if _, err := s.UpdateShotPartial(id, fields); err != nil {
			logger.Printf("ApplyStoryboardDiffs: shot_id=%d: %v", id, err)
			continue
		}
		applied++
	}

	// 更新审查记录状态
	if recordID > 0 && s.reviewRecordRepo != nil && applied > 0 {
		rec, err := s.reviewRecordRepo.GetByID(recordID)
		if err == nil {
			now := time.Now()
			snapshotJSON, _ := json.Marshal(snapshot)
			diffsJSON, _ := json.Marshal(diffs)
			rec.Status = "applied"
			rec.AppliedAt = &now
			rec.RollbackSnapshotJSON = string(snapshotJSON)
			rec.AppliedDiffsJSON = string(diffsJSON)
			if saveErr := s.reviewRecordRepo.Update(rec); saveErr != nil {
				logger.Printf("ApplyStoryboardDiffs: update record failed: %v", saveErr)
			}
		}
	}

	return applied, nil
}

// RollbackReview 将分镜内容回滚到某次审查应用之前的状态。
func (s *VideoService) RollbackReview(tenantID, videoID, recordID uint) (int, error) {
	if s.reviewRecordRepo == nil {
		return 0, fmt.Errorf("审查记录功能未启用")
	}
	rec, err := s.reviewRecordRepo.GetByID(recordID)
	if err != nil {
		return 0, fmt.Errorf("审查记录不存在: %w", err)
	}
	if rec.VideoID != videoID {
		return 0, fmt.Errorf("记录与视频不匹配")
	}
	if rec.Status != "applied" {
		return 0, fmt.Errorf("该记录状态为 %s，无法回滚（仅 applied 状态可回滚）", rec.Status)
	}
	if rec.RollbackSnapshotJSON == "" {
		return 0, fmt.Errorf("该记录无回滚快照数据")
	}

	var snapshot []model.ShotRollbackItem
	if err := json.Unmarshal([]byte(rec.RollbackSnapshotJSON), &snapshot); err != nil {
		return 0, fmt.Errorf("回滚快照解析失败: %w", err)
	}

	restored := 0
	for _, item := range snapshot {
		fields := map[string]interface{}{
			"narration":   item.Narration,
			"description": item.Description,
		}
		if _, err := s.UpdateShotPartial(item.ShotID, fields); err != nil {
			logger.Printf("RollbackReview: shot_id=%d: %v", item.ShotID, err)
			continue
		}
		restored++
	}

	rec.Status = "rolled_back"
	if saveErr := s.reviewRecordRepo.Update(rec); saveErr != nil {
		logger.Printf("RollbackReview: update record failed: %v", saveErr)
	}

	return restored, nil
}

// ListReviewRecords 返回某视频的所有审查历史（含解析后的 review 数据）。
func (s *VideoService) ListReviewRecords(videoID uint) ([]*model.StoryboardReviewRecord, error) {
	if s.reviewRecordRepo == nil {
		return []*model.StoryboardReviewRecord{}, nil
	}
	list, err := s.reviewRecordRepo.ListByVideo(videoID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []*model.StoryboardReviewRecord{}
	}
	return list, nil
}

// shotOptimizeUpdate 是 AI 返回的单镜优化结果（仅含需要修改的字段）。
type shotOptimizeUpdate struct {
	ShotNo        int     `json:"shot_no"`
	Description   string  `json:"description,omitempty"`
	Narration     string  `json:"narration,omitempty"`
	Dialogue      string  `json:"dialogue,omitempty"`
	CameraType    string  `json:"camera_type,omitempty"`
	CameraAngle   string  `json:"camera_angle,omitempty"`
	ShotSize      string  `json:"shot_size,omitempty"`
	Duration      float64 `json:"duration,omitempty"`
	EmotionalTone string  `json:"emotional_tone,omitempty"`
	Transition    string  `json:"transition,omitempty"`
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

// GetShot 根据 ID 获取单个分镜
func (s *VideoService) GetShot(id uint) (*model.StoryboardShot, error) {
	return s.storyboardRepo.GetByID(id)
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

// GenerateSingleShot 触发单个分镜生成（异步）
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

func (s *VideoService) GenerateSingleShot(videoID, shotID uint, provider ...string) (*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, err
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("shot %d does not belong to video %d", shotID, videoID)
	}

	// Resolve provider and aspect ratio from novel project config (caller override wins)
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if effectiveProvider == "" && novel.VideoModel != "" {
				effectiveProvider = novel.VideoModel
			}
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] failed to update shot status to generating: %v", err)
	}
	if video.Mode == "slideshow" {
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	// AI 视频模式：若没有可用的视频提供商，自动降级为图片解说模式
	if len(s.videoProviders) == 0 {
		logger.Printf("GenerateSingleShot: no video provider available, falling back to slideshow for shot %d (video %d)", shotID, videoID)
		return shot, s.GenerateSlideshowShotVideo(shot, aspectRatio)
	}
	return shot, s.GenerateShotVideo(shot, aspectRatio, effectiveProvider)
}

// maxConcurrentShots 限制同时提交给视频提供商的并发数，防止触发 API 429
const maxConcurrentShots = 3

// downloadHTTPClient 用于下载生成的图片/视频文件。
// 设置 5 分钟超时，防止 CDN 接受连接后挂起导致 goroutine 永久阻塞（批量生成卡在 99% 的根本原因）。
var downloadHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// BatchGenerateShots 批量触发指定分镜生成（同步等待所有完成，支持并发限制）
// 图片解说模式(Mode=="slideshow")只生成图片，不生成 Ken Burns 短片。
func (s *VideoService) BatchGenerateShots(videoID uint, shotIDs []uint, qualityTierOverride string, progressFn func(int), provider ...string) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	if qualityTierOverride != "" {
		video.QualityTier = qualityTierOverride
	}

	// Resolve effective provider and aspect ratio from novel config
	effectiveProvider := ""
	if len(provider) > 0 {
		effectiveProvider = provider[0]
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
			if effectiveProvider == "" && novel.VideoModel != "" {
				effectiveProvider = novel.VideoModel
			}
			if aspectRatio == "" && novel.VideoConf().VideoAspectRatio != "" {
				aspectRatio = novel.VideoConf().VideoAspectRatio
			}
		}
	}

	mode := video.Mode
	if mode == "" {
		mode = "video"
	}
	logger.Printf("BatchGenerateShots: videoID=%d total=%d mode=%s provider=%s aspectRatio=%s", videoID, len(shotIDs), mode, effectiveProvider, aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShots, batchErr := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErr != nil {
		return nil, batchErr
	}
	shotMap := make(map[uint]*model.StoryboardShot, len(allShots))
	for _, sh := range allShots {
		shotMap[sh.ID] = sh
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32
	for _, sid := range shotIDs {
		shot, ok := shotMap[sid]
		if !ok || shot.VideoID != videoID {
			if progressFn != nil && total > 0 {
				pct := int(done.Add(1)) * 99 / total
				progressFn(pct)
			}
			continue
		}
		shot.Status = "generating"
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", shot.ShotNo, err)
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				n := int(done.Add(1))
				if progressFn != nil && total > 0 {
					pct := n * 99 / total
					progressFn(pct)
				}
				logger.Printf("BatchGenerateShots: shot %d done (%d/%d)", sh.ShotNo, n, total)
			}()
			logger.Printf("BatchGenerateShots: shot %d start (mode=%s)", sh.ShotNo, mode)
			const maxRetries = 3
			var genErr error
			if video.Mode == "slideshow" || len(s.videoProviders) == 0 {
				// ── 两阶段异步模式 ──────────────────────────────────────────────────
				// 阶段一（同步，占用 sem）：AI 图片生成 → 下载到本地
				// 阶段二（异步，释放 sem 后后台执行）：Ken Burns 编码 → OSS 上传，支持自动重试
				var localImage string
				var clipDur float64
				for attempt := 1; attempt <= maxRetries; attempt++ {
					localImage, clipDur, genErr = s.generateShotImageOnly(sh, aspectRatio)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d image attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr == nil {
					// 图片就绪：立即标记 completed（progress=50）供前端展示
					if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
						"status": "completed", "progress": 50,
					}); err != nil {
						logger.Printf("[VideoService] BatchGenerateShots: failed to update shot %d status: %v", sh.ShotNo, err)
					}
					// Ken Burns + OSS 在后台异步执行，完成后 progress=100
					go s.generateClipAndUploadWithRetry(context.Background(), sh.ID, localImage, clipDur, aspectRatio)
					logger.Printf("BatchGenerateShots: shot %d image ready, clip async", sh.ShotNo)
				} else {
					logger.Printf("BatchGenerateShots: shot %d image failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				}
			} else {
				// ── AI 视频模式：原有同步逻辑（提交 → provider 轮询）──────────────
				for attempt := 1; attempt <= maxRetries; attempt++ {
					genErr = s.GenerateShotVideo(sh, aspectRatio, effectiveProvider)
					if genErr == nil {
						break
					}
					logger.Printf("BatchGenerateShots: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
					if attempt < maxRetries {
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if genErr != nil {
					logger.Printf("BatchGenerateShots: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
				} else {
					logger.Printf("BatchGenerateShots: shot %d submitted successfully (taskID=%s)", sh.ShotNo, sh.ShotTaskID)
				}
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShots: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// BatchGenerateShotImages 批量为分镜生成参考图片（幂等：已有 ImageURL 的分镜自动跳过）。
// 只执行阶段一（AI 图片生成），不启动 Ken Burns 编码。
func (s *VideoService) BatchGenerateShotImages(videoID uint, shotIDs []uint, progressFn func(int)) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil && novel.VideoConf().VideoAspectRatio != "" {
			aspectRatio = novel.VideoConf().VideoAspectRatio
		}
	}

	logger.Printf("BatchGenerateShotImages: videoID=%d total=%d aspectRatio=%s", videoID, len(shotIDs), aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShotsImg, batchErrImg := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErrImg != nil {
		return nil, batchErrImg
	}
	shotMapImg := make(map[uint]*model.StoryboardShot, len(allShotsImg))
	for _, sh := range allShotsImg {
		shotMapImg[sh.ID] = sh
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32

	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	for _, sid := range shotIDs {
		shot, ok := shotMapImg[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.ImageURL != "" {
			// Already has image — skip (idempotent).
			advanceProgress()
			continue
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShotImages: shot %d done", sh.ShotNo)
			}()
			const maxRetries = 3
			var localImage string
			var genErr error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				localImage, _, genErr = s.generateShotImageOnly(sh, aspectRatio)
				if genErr == nil {
					break
				}
				logger.Printf("BatchGenerateShotImages: shot %d attempt %d/%d failed: %v", sh.ShotNo, attempt, maxRetries, genErr)
				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt*2) * time.Second)
				}
			}
			if localImage != "" {
				os.Remove(localImage) //nolint:errcheck  // temp file not needed; ImageURL is in DB
			}
			if genErr == nil {
				if err := s.storyboardRepo.UpdateFields(sh.ID, map[string]interface{}{
					"status": "completed", "progress": 50,
				}); err != nil {
					logger.Printf("[VideoService] BatchGenerateShotImages: failed to update shot %d status: %v", sh.ShotNo, err)
				}
				logger.Printf("BatchGenerateShotImages: shot %d image ready", sh.ShotNo)
			} else {
				logger.Printf("BatchGenerateShotImages: shot %d failed after %d attempts: %v", sh.ShotNo, maxRetries, genErr)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotImages: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// BatchGenerateShotClips 批量为已有图片的分镜生成 Ken Burns 动效视频（幂等：已有 VideoURL 的分镜自动跳过）。
// 只执行阶段二（Ken Burns 编码 + OSS 上传），不重新生成图片。
// 分镜必须已有 ImageURL；若没有图片则跳过并记录日志。
func (s *VideoService) BatchGenerateShotClips(videoID uint, shotIDs []uint, progressFn func(int)) ([]*model.StoryboardShot, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return nil, err
	}
	aspectRatio := video.AspectRatio
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil && novel.VideoConf().VideoAspectRatio != "" {
			aspectRatio = novel.VideoConf().VideoAspectRatio
		}
	}

	logger.Printf("BatchGenerateShotClips: videoID=%d total=%d aspectRatio=%s", videoID, len(shotIDs), aspectRatio)

	// 批量预取所有分镜（单次 IN 查询，避免 N 次 GetByID 往返）
	allShotsClip, batchErrClip := s.storyboardRepo.BatchGetByIDs(shotIDs)
	if batchErrClip != nil {
		return nil, batchErrClip
	}
	shotMapClip := make(map[uint]*model.StoryboardShot, len(allShotsClip))
	for _, sh := range allShotsClip {
		shotMapClip[sh.ID] = sh
	}

	var queued []*model.StoryboardShot
	sem := make(chan struct{}, maxConcurrentShots)
	var wg sync.WaitGroup
	total := len(shotIDs)
	var done atomic.Int32

	advanceProgress := func() {
		n := int(done.Add(1))
		if progressFn != nil && total > 0 {
			progressFn(n * 99 / total)
		}
	}

	for _, sid := range shotIDs {
		shot, ok := shotMapClip[sid]
		if !ok || shot.VideoID != videoID {
			advanceProgress()
			continue
		}
		if shot.VideoURL != "" {
			// Already has clip — skip (idempotent).
			advanceProgress()
			continue
		}
		if shot.ImageURL == "" {
			logger.Printf("BatchGenerateShotClips: shot %d skipped — no image", shot.ShotNo)
			advanceProgress()
			continue
		}
		queued = append(queued, shot)
		sem <- struct{}{}
		wg.Add(1)
		go func(sh *model.StoryboardShot) {
			defer func() {
				<-sem
				wg.Done()
				advanceProgress()
				logger.Printf("BatchGenerateShotClips: shot %d done", sh.ShotNo)
			}()
			duration := sh.Duration
			if duration <= 0 {
				duration = 5.0
			}
			localImage, dlErr := downloadToTemp(sh.ImageURL, fmt.Sprintf("inkframe-img-%d-", sh.ID), ".jpg")
			if dlErr != nil {
				logger.Printf("BatchGenerateShotClips: shot %d download failed: %v", sh.ShotNo, dlErr)
				return
			}
			defer os.Remove(localImage)

			// Ken Burns encode (with still-frame fallback), same logic as generateClipAndUploadWithRetry.
			var clipPath string
			var lastErr error
			for attempt := 1; attempt <= maxClipRetries; attempt++ {
				clipPath, lastErr = s.generateKenBurnsPureGo(context.Background(), sh, localImage, duration, aspectRatio)
				if lastErr != nil {
					clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
				}
				if lastErr == nil {
					break
				}
				logger.Printf("BatchGenerateShotClips: shot %d attempt %d/%d: %v", sh.ShotNo, attempt, maxClipRetries, lastErr)
				if attempt < maxClipRetries {
					time.Sleep(time.Duration(attempt*5) * time.Second)
				}
			}

			fields := map[string]interface{}{"progress": 100}
			if lastErr != nil {
				logger.Printf("BatchGenerateShotClips: shot %d clip failed: %v", sh.ShotNo, lastErr)
			} else if ossURL := s.uploadClipToStorage(context.Background(), sh, clipPath); ossURL != "" {
				fields["video_url"] = ossURL
				fields["clip_path"] = ""
				os.Remove(clipPath) //nolint:errcheck
				logger.Printf("BatchGenerateShotClips: shot %d clip → %s", sh.ShotNo, ossURL)
			} else {
				fields["clip_path"] = "file://" + clipPath
			}
			if err := s.storyboardRepo.UpdateFields(sh.ID, fields); err != nil {
				logger.Printf("[VideoService] BatchGenerateShotClips: failed to update shot %d fields: %v", sh.ShotNo, err)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("BatchGenerateShotClips: all %d shots done for videoID=%d", len(queued), videoID)
	return queued, nil
}

// GetStatus 获取视频生成状态（从 provider 同步最新进度）
func (s *VideoService) GetStatus(id uint) (interface{}, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	// 超时检查：生成中超过 30 分钟
	if video.Status == "generating" && time.Since(video.UpdatedAt) > 30*time.Minute {
		video.Status = "failed"
		video.ErrorMessage = "generation timed out (>30min)"
		s.videoRepo.Update(video)
	}

	// 自动重试：失败且重试次数 < 3
	if video.Status == "failed" && video.RetryCount < 3 && video.TaskID != "" {
		video.RetryCount++
		video.Status = "generating"
		video.ErrorMessage = ""
		s.videoRepo.Update(video)
		go func() { s.StartGeneration(id) }() //nolint:errcheck
	}

	// 如果有外部任务 ID 且状态为 generating，则同步 provider 状态
	if video.TaskID != "" && video.Status == "generating" && video.ProviderName != "" {
		if provider, ok := s.videoProviders[video.ProviderName]; ok {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			taskStatus, err := provider.GetVideoStatus(ctx, video.TaskID)
			if err == nil {
				video.Progress = taskStatus.Progress
				switch taskStatus.Status {
				case "completed":
					// 获取视频 URL
					if videoURL, urlErr := provider.GetVideoURL(ctx, video.TaskID); urlErr == nil {
						video.VideoPath = videoURL
						video.Status = "completed"
					}
				case "failed":
					video.Status = "failed"
					video.ErrorMessage = taskStatus.Error
				}
				s.videoRepo.Update(video)
			}
		}
	}

	return map[string]interface{}{
		"status":        video.Status,
		"progress":      video.Progress,
		"task_id":       video.TaskID,
		"provider":      video.ProviderName,
		"error_message": video.ErrorMessage,
		"video_url":     video.VideoPath,
	}, nil
}

// checkTenantAccess 校验 novel 关联租户状态
func (s *VideoService) checkTenantAccess(novelID uint) error {
	if s.tenantRepo == nil || s.novelRepo == nil {
		return nil
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil // 不阻塞，让其他逻辑处理
	}
	tenant, err := s.tenantRepo.GetByID(novel.TenantID)
	if err != nil {
		return nil
	}
	if tenant.Status != "active" {
		return fmt.Errorf("tenant account is %s", tenant.Status)
	}
	if tenant.ExpiresAt != nil && !tenant.ExpiresAt.IsZero() && time.Now().After(*tenant.ExpiresAt) {
		return fmt.Errorf("tenant account has expired")
	}
	return nil
}

// generateShotReferenceImage 为分镜生成参考帧图像，返回图片URL和错误。
func (s *VideoService) generateShotReferenceImage(shot *model.StoryboardShot) (string, error) {
	if s.aiService == nil {
		return "", fmt.Errorf("AI service not initialized")
	}

	// charBestImage 返回角色的最佳参考图 URL。
	// 优先级：ThreeViewSheet（三视图合图，一致性最强）> Portrait（肖像）
	charBestImage := func(c *model.Character) string {
		if c.ThreeViewSheet != "" {
			return c.ThreeViewSheet
		}
		return c.Portrait
	}

	// 精准匹配：批量加载 shot.CharacterIDs 中的角色参考图（单次 IN 查询，避免 N+1）
	var characterPortrait string
	var refSource string // for logging
	if len(shot.CharacterIDs) > 0 {
		ids := []uint(shot.CharacterIDs)
		batchChars, batchErr := s.characterRepo.ListByIDs(ids)
		if batchErr == nil {
			for _, char := range batchChars {
				img := charBestImage(char)
				if img == "" {
					continue
				}
				if char.ThreeViewSheet != "" {
					characterPortrait = img
					refSource = fmt.Sprintf("charID=%d ThreeViewSheet", char.ID)
					break // 三视图是最优选择，找到即停
				}
				if characterPortrait == "" {
					characterPortrait = img
					refSource = fmt.Sprintf("charID=%d Portrait/ImageURL", char.ID)
				}
			}
		}
	}

	// cachedNovelChars 延迟加载：降级一、降级二共用，避免重复 ListByNovel 查询
	var cachedNovelChars []*model.Character

	// 降级一：若 CharacterIDs 未命中，从 shot.Characters JSON 内联名称匹配
	// （CharacterIDs 由 autoMatchShotCharacters 在分镜生成时设置，若名称有偏差则可能为空）
	if characterPortrait == "" && shot.Characters != "" {
		var shotChars []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err == nil && len(shotChars) > 0 {
			if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil && video.NovelID > 0 {
				if cachedNovelChars == nil {
					cachedNovelChars, _ = s.characterRepo.ListByNovel(video.NovelID)
				}
				if len(cachedNovelChars) > 0 {
					nameMap := make(map[string]*model.Character, len(cachedNovelChars))
					for _, c := range cachedNovelChars {
						nameMap[strings.ToLower(c.Name)] = c
					}
					for _, sc := range shotChars {
						nameLow := strings.ToLower(sc.Name)
						char, ok := nameMap[nameLow]
						if !ok {
							// 子串模糊匹配
							for n, c := range nameMap {
								if strings.Contains(nameLow, n) || strings.Contains(n, nameLow) {
									char = c
									ok = true
									break
								}
							}
						}
						if ok && char != nil {
							img := charBestImage(char)
							if img != "" {
								if char.ThreeViewSheet != "" {
									characterPortrait = img
									refSource = fmt.Sprintf("inline name=%q ThreeViewSheet", sc.Name)
									break
								}
								if characterPortrait == "" {
									characterPortrait = img
									refSource = fmt.Sprintf("inline name=%q fallback", sc.Name)
								}
							}
						}
					}
				}
			}
		}
	}

	// 降级二：通过 ChapterID 找到 NovelID，取第一个有参考图的角色；同时获取章节序号用于 OSS 路径
	var chapterNo int
	if shot.ChapterID != nil && s.chapterRepo != nil {
		if chapter, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil && chapter != nil {
			chapterNo = chapter.ChapterNo
			if characterPortrait == "" {
				if cachedNovelChars == nil {
					cachedNovelChars, _ = s.characterRepo.ListByNovel(chapter.NovelID)
				}
				for _, c := range cachedNovelChars {
					img := charBestImage(c)
					if img != "" {
						characterPortrait = img
						refSource = fmt.Sprintf("novel first char=%q", c.Name)
						break
					}
				}
			}
		}
	}
	logger.Printf("generateShotReferenceImage: shot %d charIDs=%v refSource=%q portrait=%v",
		shot.ShotNo, shot.CharacterIDs, refSource, characterPortrait != "")

	promptText := shot.Prompt
	if promptText == "" {
		promptText = shot.Description
	}

	// 场景锚点：注入锁定词，使用锚点参考图替代角色图（布景优先于人物）
	var sceneRefImage string
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, refURL, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil {
			if fragment != "" {
				promptText = fragment + ", " + promptText
			}
			sceneRefImage = refURL
		}
	}

	// 角色三视图/肖像优先；场景锚点参考图仅在无角色参考图时作保底
	// 场景上下文已通过 fragment 注入 promptText，无需用参考图再次覆盖
	refImage := characterPortrait
	if refImage == "" {
		refImage = sceneRefImage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 获取视频的 ArtStyle、TenantID、质量档位和角色一致性权重
	artStyle := ""
	var tenantID uint
	charConsistencyWeight := 1.0 // 默认严格一致
	qualityTier := "preview"     // 默认质量档位
	if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
		artStyle = video.ArtStyle
		tenantID = video.TenantID
		if video.QualityTier != "" {
			qualityTier = video.QualityTier
		}
		if video.NovelID > 0 && s.novelRepo != nil {
			if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
				if tenantID == 0 {
					tenantID = novel.TenantID
				}
				if novel.VideoConf().CharConsistencyWeight > 0 {
					charConsistencyWeight = novel.VideoConf().CharConsistencyWeight
				}
				// 降级：视频未设置画面风格时使用项目设置中的画面风格
				if artStyle == "" && novel.ImageStyle != "" {
					artStyle = novel.ImageStyle
				}
				// 注入 OSS 路径提示（项目名+章节序号）
				if novel.Title != "" {
					ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title, ChapterNo: chapterNo})
				}
			}
		}
	}

	// 根据质量档位注入分辨率提示，引导图像提供者选择适当尺寸。
	// 对于支持 size 参数的提供者（如 DALL-E 3），此 hint 也作为备注说明。
	imgWidth, _, _ := qualityTierImageParams(qualityTier)
	if imgWidth > 0 {
		promptText = fmt.Sprintf("--w %d ", imgWidth) + promptText
	}
	logger.Printf("generateShotReferenceImage: shot %d qualityTier=%s imgWidth=%d", shot.ShotNo, qualityTier, imgWidth)

	// 镜头类型注解：根据景别选择光学特征，提升图像构图的电影感
	lensTypeMap := map[string]string{
		"extreme_close_up": "macro lens 100mm, extreme shallow DOF, bokeh",
		"close_up":         "portrait lens 85mm, shallow depth of field, subject isolation",
		"medium":           "standard lens 50mm, natural perspective",
		"wide":             "wide angle lens 24mm, deep focus, environmental context",
		"extreme_wide":     "ultra wide lens 16mm, expansive environment, dramatic perspective",
	}
	lensType := lensTypeMap[shot.ShotSize]
	if lensType == "" {
		lensType = "standard lens 50mm"
	}
	cinematicImgPrefix := "cinematic film photography, 35mm anamorphic lens, professional lighting setup, " + lensType + ", "
	promptText = cinematicImgPrefix + promptText

	imageURL, err := s.aiService.GenerateCharacterThreeView(ctx, tenantID, "", promptText, refImage, artStyle, "", charConsistencyWeight)
	if err != nil {
		logger.Printf("generateShotReferenceImage: image gen failed for shot %d: %v", shot.ShotNo, err)
		return "", err
	}
	if imageURL == "" {
		logger.Printf("generateShotReferenceImage: image gen returned empty URL for shot %d", shot.ShotNo)
		return "", fmt.Errorf("image provider returned empty URL")
	}

	// 首图锁定：场景锚点无参考图时，将本次生成结果存为参考图
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if err := s.sceneAnchorSvc.AutoSetRefImage(*shot.SceneAnchorID, imageURL); err != nil {
			logger.Printf("[VideoService] AutoSetRefImage: %v", err)
		}
	}

	return imageURL, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 分镜语音段落 (ShotVoiceSegment) 服务方法
// ─────────────────────────────────────────────────────────────────────────────

// VoiceSegmentInput 创建/更新语音段落时的输入
type VoiceSegmentInput struct {
	Text    string `json:"text"`
	Speaker string `json:"speaker"`  // 空串=旁白，非空=角色名（对白）
	VoiceID string `json:"voice_id"` // TTS 声音 ID，空串=自动
}

// GetVoiceSegment 按 ID 获取单个语音段落
func (s *VideoService) GetVoiceSegment(segID uint) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	return s.segmentRepo.GetByID(segID)
}

// ListVoiceSegments 获取分镜的所有语音段落
func (s *VideoService) ListVoiceSegments(shotID uint) ([]*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	return s.segmentRepo.ListByShotID(shotID)
}

// AppendVoiceSegment 追加段落到分镜末尾
func (s *VideoService) AppendVoiceSegment(shotID uint, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	maxSeq, err := s.segmentRepo.MaxSeqNo(shotID)
	if err != nil {
		return nil, err
	}
	seg := &model.ShotVoiceSegment{
		ShotID:  shotID,
		SeqNo:   maxSeq + 1,
		Text:    input.Text,
		Speaker: input.Speaker,
		VoiceID: input.VoiceID,
	}
	return seg, s.segmentRepo.Create(seg)
}

// InsertVoiceSegment 在 afterSeqNo 之后插入新段落（afterSeqNo=0 表示插入到最前）
func (s *VideoService) InsertVoiceSegment(shotID uint, afterSeqNo int, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	newSeqNo := afterSeqNo + 1
	// 把 seq_no >= newSeqNo 的段落全部后移 1 位
	if err := s.segmentRepo.ShiftSeqNos(shotID, newSeqNo); err != nil {
		return nil, err
	}
	seg := &model.ShotVoiceSegment{
		ShotID:  shotID,
		SeqNo:   newSeqNo,
		Text:    input.Text,
		Speaker: input.Speaker,
		VoiceID: input.VoiceID,
	}
	return seg, s.segmentRepo.Create(seg)
}

// UpdateVoiceSegment 更新段落文本/说话人/声音
func (s *VideoService) UpdateVoiceSegment(segID uint, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return nil, err
	}
	seg.Text = input.Text
	seg.Speaker = input.Speaker
	seg.VoiceID = input.VoiceID
	return seg, s.segmentRepo.Update(seg)
}

// DeleteVoiceSegment 删除段落并将后续段落 seq_no 前移（保持连续）
func (s *VideoService) DeleteVoiceSegment(segID uint) error {
	if s.segmentRepo == nil {
		return fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return err
	}
	if err := s.segmentRepo.Delete(segID); err != nil {
		return err
	}
	return s.segmentRepo.CompactSeqNosAfter(seg.ShotID, seg.SeqNo)
}

// mp3Duration estimates the duration in seconds of MPEG1 Layer3 audio data by
// counting frames. Returns 0 if the data cannot be parsed.
func mp3Duration(data []byte) float64 {
	if len(data) < 4 {
		return 0
	}
	// Skip ID3v2 tag if present
	offset := 0
	if len(data) >= 10 && data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		sz := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
		offset = 10 + sz
	}
	bitrateTable := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	sampleRates := [4]int{44100, 48000, 32000, 0}
	var frames, sampleRate int
	for i := offset; i+3 < len(data); {
		if data[i] != 0xFF || (data[i+1]&0xE0) != 0xE0 {
			i++
			continue
		}
		// MPEG1 (bits 4-3 = 11) + Layer3 (bits 2-1 = 01)
		if (data[i+1]&0x18) != 0x18 || (data[i+1]&0x06) != 0x02 {
			i++
			continue
		}
		bitrateIdx := int(data[i+2]>>4) & 0x0F
		srIdx := int(data[i+2]>>2) & 0x03
		padding := int(data[i+2]>>1) & 0x01
		bitrate := bitrateTable[bitrateIdx] * 1000
		sr := sampleRates[srIdx]
		if bitrate == 0 || sr == 0 {
			i++
			continue
		}
		frameLen := 144*bitrate/sr + padding
		if frameLen <= 4 || i+frameLen > len(data) {
			break
		}
		frames++
		if sampleRate == 0 {
			sampleRate = sr
		}
		i += frameLen
	}
	if frames == 0 || sampleRate == 0 {
		return 0
	}
	return float64(frames) * 1152.0 / float64(sampleRate)
}

// alignShotDurationToTTS 检查分镜的 TTS 音频时长，若音频更长则延伸分镜时长以确保配音完整。
// 返回调整后的时长（秒）；无法读取音频时返回原 shot.Duration。
// 注意：此函数仅用于当次生成，不持久化回数据库。
func alignShotDurationToTTS(shot *model.StoryboardShot) float64 {
	if shot.AudioPath == "" {
		return shot.Duration
	}
	data, err := readLocalOrRemoteFile(shot.AudioPath)
	if err != nil || len(data) == 0 {
		return shot.Duration
	}
	ext := audioExtension(shot.AudioPath)
	var audioDur float64
	if ext == ".mp3" {
		audioDur = mp3Duration(data)
	} else {
		micros := parseAudioDurationMicros(data, ext)
		if micros > 0 {
			audioDur = float64(micros) / 1_000_000.0
		}
	}
	if audioDur <= 0 {
		return shot.Duration
	}
	const buffer = 0.3
	needed := audioDur + buffer
	if needed > shot.Duration {
		logger.Printf("[VideoService] alignShotDurationToTTS: shot %d duration %.1fs → %.1fs (TTS=%.1fs)",
			shot.ShotNo, shot.Duration, needed, audioDur)
		return needed
	}
	return shot.Duration
}

// GenerateSegmentAudio 为单条语音段落生成 TTS 音频
func (s *VideoService) GenerateSegmentAudio(segID uint, tenantID uint, defaultVoice string) error {
	if s.segmentRepo == nil {
		return fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return fmt.Errorf("segment %d not found: %w", segID, err)
	}
	text := seg.Text
	if text == "" {
		return nil
	}

	// 预加载 shot + video 一次，同时用于：① 角色声音查找 ② OSS 存储 key（避免重复查询）
	var novelID, chapterID uint
	if s.storyboardRepo != nil && s.videoRepo != nil {
		if shot, e := s.storyboardRepo.GetByID(seg.ShotID); e == nil {
			if video, e := s.videoRepo.GetByID(shot.VideoID); e == nil {
				novelID = video.NovelID
				if video.ChapterID != nil {
					chapterID = *video.ChapterID
				}
			}
		}
	}

	// 确定 TTS 声音：段落级 > 角色声音查找（带缓存）> 默认
	voice := seg.VoiceID
	speed := 1.0
	style := ""
	if voice == "" && seg.Speaker != "" && s.characterRepo != nil && novelID > 0 {
		if chars, e := s.listCharsByNovelCached(novelID); e == nil {
			for _, c := range chars {
				if strings.EqualFold(c.Name, seg.Speaker) {
					voice = c.VoiceID
					break
				}
			}
		}
	}
	if voice == "" {
		voice = defaultVoice
	}
	if voice == "" {
		voice = "alloy"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	audioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, text, voice, speed, style)
	if err != nil {
		return fmt.Errorf("TTS failed for segment %d: %w", segID, err)
	}
	if audioURL == "" {
		return fmt.Errorf("TTS returned empty URL for segment %d", segID)
	}

	// Download audio bytes (needed for OSS upload and duration calculation)
	var audioData []byte
	if strings.HasPrefix(audioURL, "file://") {
		audioData, err = os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
	} else {
		if resp, e := http.Get(audioURL); e == nil { //nolint:gosec
			audioData, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		} else {
			err = e
		}
	}
	if err != nil {
		logger.Printf("warn: could not read audio for segment %d: %v", segID, err)
	}

	// 上传到持久存储（如果配置了 storageSvc）
	// key 格式：novels/{novelID}/chapters/{chapterID}/audio/seg-{segID}.mp3
	if s.storageSvc != nil && len(audioData) > 0 {
		filename := fmt.Sprintf("seg-%d.mp3", segID)
		key := storage.BuildKey(novelID, chapterID, "audio", filename)
		if ossURL, e := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(audioData), int64(len(audioData)), "audio/mpeg"); e == nil {
			if strings.HasPrefix(audioURL, "file://") {
				os.Remove(strings.TrimPrefix(audioURL, "file://")) //nolint:errcheck
			}
			audioURL = ossURL
		} else {
			logger.Printf("GenerateSegmentAudio: OSS upload failed for segment %d: %v", segID, e)
		}
	}

	// Persist audio path + measured duration
	fields := map[string]interface{}{"audio_path": audioURL}
	if d := mp3Duration(audioData); d > 0 {
		fields["duration_secs"] = d
	}
	if err := s.segmentRepo.UpdateFields(segID, fields); err != nil {
		logger.Printf("[VideoService] GenerateSegmentAudio: failed to update segment %d fields: %v", segID, err)
	}

	// 配音生成完成后，同步更新分镜时长：取视频时长与所有配音段落累计时长中的较大值
	s.syncShotDurationAfterVoice(seg.ShotID)

	return nil
}

// syncShotDurationAfterVoice 累加该分镜所有配音段落的 duration_secs，
// 若合计时长超过当前分镜时长，则更新分镜 duration 为二者中较大值。
func (s *VideoService) syncShotDurationAfterVoice(shotID uint) {
	if s.segmentRepo == nil {
		return
	}
	segs, err := s.segmentRepo.ListByShotID(shotID)
	if err != nil || len(segs) == 0 {
		return
	}
	var totalVoice float64
	for _, sg := range segs {
		if sg.DurationSecs > 0 {
			totalVoice += sg.DurationSecs
		}
	}
	if totalVoice <= 0 {
		return
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil || shot == nil {
		return
	}
	if totalVoice <= shot.Duration {
		return // 配音比当前时长短，不需要更新
	}
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"duration": totalVoice}); err != nil {
		logger.Printf("[VideoService] syncShotDurationAfterVoice: failed to update shot %d duration: %v", shotID, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 分镜插入 / 复制 / 删除
// ─────────────────────────────────────────────────────────────────────────────

// InsertShot 在 afterShotNo 之后插入一个空分镜（afterShotNo=0 表示插入到最前）
func (s *VideoService) InsertShot(videoID uint, afterShotNo int, narration, description string, duration float64) (*model.StoryboardShot, error) {
	newShotNo := afterShotNo + 1
	// 后移所有 shot_no >= newShotNo 的分镜
	if err := s.storyboardRepo.ShiftShotNos(videoID, newShotNo, 1); err != nil {
		return nil, fmt.Errorf("shift shot numbers: %w", err)
	}
	if duration <= 0 {
		duration = 5.0
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

// RefineShotImage 基于用户修改建议重新生成分镜图片。
// suggestion 追加到原有 Prompt/Description 后，角色参考图查找逻辑保持不变。
// 成功后自动更新 DB 中的 ImageURL 并返回新 URL。
func (s *VideoService) RefineShotImage(shotID uint, suggestion string) (string, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return "", fmt.Errorf("shot %d not found: %w", shotID, err)
	}

	// 构建含修改建议的提示词（操作副本，不改 DB 原始字段）
	shotCopy := *shot
	basePrompt := shot.Prompt
	if basePrompt == "" {
		basePrompt = shot.Description
	}
	if suggestion != "" {
		shotCopy.Prompt = basePrompt + ". Modification: " + suggestion
	} else {
		shotCopy.Prompt = basePrompt
	}

	newURL, err := s.generateShotReferenceImage(&shotCopy)
	if err != nil {
		return "", fmt.Errorf("refine image for shot %d: %w", shotID, err)
	}

	// 持久化新图片 URL
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"image_url": newURL}); err != nil {
		logger.Printf("[VideoService] RefineShot: failed to update shot %d image URL: %v", shotID, err)
	}
	return newURL, nil
}

// resolveArtStyle 返回视频的画面风格：优先用 video.ArtStyle，降级到 novel.ImageStyle
func (s *VideoService) resolveArtStyle(videoID uint) string {
	if s.videoRepo == nil {
		return ""
	}
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return ""
	}
	if video.ArtStyle != "" {
		return video.ArtStyle
	}
	if video.NovelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
			return novel.ImageStyle
		}
	}
	return ""
}

// extractLastFrame 使用 FFmpeg 提取视频最后一帧，返回本地 jpeg 路径
func (s *VideoService) extractLastFrame(clipPath string) (string, error) {
	// 处理 file:// 前缀
	localPath := strings.TrimPrefix(clipPath, "file://")

	tmpJpeg := fmt.Sprintf("%s/inkframe-lastframe-%d.jpg", inkframeTempDir(), time.Now().UnixNano())
	if _, err := runFFmpegCtx(context.Background(), "-y",
		"-sseof", "-0.1",
		"-i", localPath,
		"-vframes", "1",
		"-f", "image2",
		tmpJpeg,
	); err != nil {
		return "", fmt.Errorf("extractLastFrame failed: %w", err)
	}
	return tmpJpeg, nil
}

// emotionToKlingParams 根据情绪/摄像机类型映射最优的 Kling 生成参数。
// 动作/史诗场景使用 pro 模式 + 10 秒时长，获得更高画质；
// 风景/全景使用高 CFG + 10 秒；对话/温情使用 5 秒防止内容填充。
func emotionToKlingParams(emotion, cameraType string) (mode string, cfgScale float64, duration float64) {
	// 将情绪标签规范化到英文
	e := strings.ToLower(emotion)
	ct := strings.ToLower(cameraType)

	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") ||
		strings.Contains(e, "action") || strings.Contains(e, "fight"):
		return "pro", 0.45, 10

	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "壮观") ||
		strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "pro", 0.5, 10

	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") ||
		strings.Contains(e, "危险") || strings.Contains(e, "恐惧"):
		return "std", 0.7, 5

	case strings.Contains(e, "landscape") || strings.Contains(e, "scenery") ||
		strings.Contains(e, "风景") || strings.Contains(e, "空镜") ||
		ct == "crane" || (ct == "pan" && strings.Contains(e, "wide")):
		return "std", 0.8, 10

	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "std", 0.6, 5

	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "离别") || strings.Contains(e, "grief"):
		return "std", 0.65, 5

	default:
		return "std", 0.5, 5
	}
}

// GenerateShotVideo 为单个分镜提交视频生成任务
func (s *VideoService) GenerateShotVideo(shot *model.StoryboardShot, videoAspectRatio string, providerOverride ...string) error {
	// 并发限流：若配置了 video_concurrency，则在此处等待令牌
	s.videoSemMu.RLock()
	vsem := s.videoSem
	s.videoSemMu.RUnlock()
	if vsem != nil {
		vsem <- struct{}{}
		defer func() { <-vsem }()
	}

	providerName := "kling"
	if len(providerOverride) > 0 && providerOverride[0] != "" {
		providerName = providerOverride[0]
	}
	provider, ok := s.videoProviders[providerName]
	if !ok {
		// fallback to any available
		for name, p := range s.videoProviders {
			providerName = name
			provider = p
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("no video provider configured")
	}

	if videoAspectRatio == "" {
		videoAspectRatio = "16:9"
	}

	logger.Printf("GenerateShotVideo: shot %d provider=%s aspect=%s duration=%ds", shot.ShotNo, providerName, videoAspectRatio, shot.Duration)

	// 始终生成角色一致性参考帧（携带角色三视图），确保批量重新生成时角色参考图不被跳过。
	// 生成成功则覆盖 ReferenceImageURL；失败时降级使用已有的 ReferenceImageURL（非致命）。
	referenceImage := shot.ReferenceImageURL
	logger.Printf("GenerateShotVideo: shot %d generating reference image (charIDs=%v)", shot.ShotNo, shot.CharacterIDs)
	frameURL, frameErr := s.generateShotReferenceImage(shot)
	if frameErr != nil {
		logger.Printf("GenerateShotVideo: shot %d reference image failed (non-fatal, using existing=%q): %v", shot.ShotNo, referenceImage, frameErr)
	} else {
		logger.Printf("GenerateShotVideo: shot %d reference image ok: %s", shot.ShotNo, frameURL)
	}
	if frameURL != "" {
		shot.FrameImageURL = frameURL
		referenceImage = frameURL
	}

	// 场景锚点：将锁定词注入视频生成 prompt
	// 优先使用运镜提示词（MotionPrompt），若为空则降级到静态画面描述（Prompt）
	videoPrompt := shot.MotionPrompt
	if videoPrompt == "" {
		videoPrompt = shot.Prompt
	}
	if s.sceneAnchorSvc != nil && shot.SceneAnchorID != nil {
		if fragment, _, err := s.sceneAnchorSvc.BuildPromptFragment(*shot.SceneAnchorID); err == nil && fragment != "" {
			videoPrompt = fragment + ", " + videoPrompt
		}
	}

	// 画面风格：注入视频 prompt（video.ArtStyle 优先，降级到 novel.ImageStyle）
	if videoArtStyle := s.resolveArtStyle(shot.VideoID); videoArtStyle != "" {
		videoPrompt = videoArtStyle + " style, " + videoPrompt
	}

	// TTS 对齐：若分镜有配音，确保视频时长不短于音频时长+缓冲。
	// alignShotDurationToTTS 仅返回调整值，不持久化到 DB。
	shotDuration := alignShotDurationToTTS(shot)

	// 动态 Kling 参数（根据情绪和摄像机类型选择最优配置）
	klingMode, klingCFG, klingDefaultDur := emotionToKlingParams(shot.EmotionalTone, shot.CameraType)
	if shotDuration <= 0 {
		shotDuration = klingDefaultDur
	}

	// 检查项目配置：KlingProForAction、HD、3D
	var hdEnabled, threeDEnabled bool
	var threeDStyle, klingModelOverride string
	if vid, vidErr := s.videoRepo.GetByID(shot.VideoID); vidErr == nil && vid.NovelID > 0 && s.novelRepo != nil {
		if novel, novelErr := s.novelRepo.GetByID(vid.NovelID); novelErr == nil {
			vc := novel.VideoConf()
			if klingMode == "pro" && !vc.KlingProForAction {
				klingMode = "std"
			}
			hdEnabled = vc.HDEnabled || strings.Contains(vid.VisualMode, "hd")
			threeDEnabled = vc.ThreeDEnabled || strings.Contains(vid.VisualMode, "3d")
			threeDStyle = vid.ThreeDStyle
			klingModelOverride = vc.KlingModel
		}
	}
	if threeDStyle == "" {
		threeDStyle = "cg"
	}
	// HD 模式：升级为更高清的模型并强制 pro
	if hdEnabled {
		if klingModelOverride == "" || klingModelOverride == "kling-v1" {
			klingModelOverride = "kling-v1-6"
		}
		klingMode = "pro"
	}

	// 电影级动态前缀——注入运镜词+情绪氛围词，移除 "film still" 静态词避免抑制视频动态感
	cinematicPrefix := buildCinematicPrefix(shot.CameraType, shot.EmotionalTone)
	// 3D 风格前缀
	if threeDEnabled {
		cinematicPrefix = resolve3DStylePrefix(threeDStyle) + ", " + cinematicPrefix
	}
	// 视频生成专属负向词：补充 static/still/frozen/slideshow 防止模型生成静止画面
	negativeBase := "blurry, low quality, watermark, text overlay, deformed, ugly, " +
		"bad anatomy, duplicate, morbid, mutilated, out of frame, extra limbs, " +
		"gross proportions, malformed limbs, " +
		"static image, still frame, frozen, no motion, slideshow, photo, " +
		"flickering, temporal inconsistency, abrupt scene change, jump cut"

	videoPromptFinal := cinematicPrefix + videoPrompt
	negativePrompt := negativeBase
	if shot.NegativePrompt != "" {
		negativePrompt = negativeBase + ", " + shot.NegativePrompt
	}

	req := &ai.VideoGenerateRequest{
		Prompt:         videoPromptFinal,
		NegativePrompt: negativePrompt,
		Duration:       shotDuration,
		AspectRatio:    videoAspectRatio,
		ImageURL:       referenceImage, // image-to-video（空时退化为 text-to-video）
		CFGScale:       klingCFG,
		Mode:           klingMode,
		Model:          klingModelOverride,
	}

	logger.Printf("GenerateShotVideo: shot %d submitting to %s (hasRef=%v mode=%s cfg=%.2f prompt=%q)", shot.ShotNo, providerName, referenceImage != "", klingMode, klingCFG, videoPromptFinal)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := provider.GenerateVideo(ctx, req)
	if err != nil {
		logger.Printf("GenerateShotVideo: shot %d submit failed: %v", shot.ShotNo, err)
		return fmt.Errorf("shot video generation failed: %w", err)
	}

	logger.Printf("GenerateShotVideo: shot %d submitted taskID=%s", shot.ShotNo, task.TaskID)
	shot.ShotTaskID = task.TaskID
	shot.ShotProviderName = providerName
	shot.Status = "processing"
	return s.storyboardRepo.Update(shot)
}

// buildCinematicPrefix 根据摄像机类型和情绪生成动态电影级 prompt 前缀。
// 刻意移除了 "film still"（静帧含义），改用 "cinematic sequence" 强化动态感。
func buildCinematicPrefix(cameraType, emotionalTone string) string {
	motion := cameraMotionToken(cameraType)
	atmos := emotionAtmosphereToken(emotionalTone)
	base := "cinematic sequence, professional cinematography, anamorphic lens, natural film grain, high dynamic range"
	if motion != "" {
		base = motion + ", " + base
	}
	if atmos != "" {
		base += ", " + atmos
	}
	return base + ", "
}

// cameraMotionToken 把 CameraType 映射为视频 prompt 运镜描述词。
func cameraMotionToken(cameraType string) string {
	switch strings.ToLower(cameraType) {
	case "pan":
		return "smooth camera pan"
	case "tilt":
		return "camera tilt movement"
	case "zoom":
		return "cinematic zoom"
	case "dolly":
		return "dolly shot, camera pushing forward"
	case "tracking", "track":
		return "smooth tracking shot following subject"
	case "crane", "crane_up":
		return "crane shot, camera rising dramatically"
	case "crane_down":
		return "crane shot, camera descending"
	case "arc":
		return "arc shot, camera orbiting subject"
	case "handheld":
		return "handheld camera, subtle natural shake"
	case "whip_pan":
		return "whip pan transition, fast swipe"
	default: // "static" or unknown — no motion token
		return ""
	}
}

// emotionAtmosphereToken 把情绪基调映射为氛围关键词，注入 prompt 以影响画面色调与动态能量。
func emotionAtmosphereToken(emotion string) string {
	e := strings.ToLower(emotion)
	switch {
	case strings.Contains(e, "battle") || strings.Contains(e, "combat") ||
		strings.Contains(e, "战斗") || strings.Contains(e, "打斗") || strings.Contains(e, "action"):
		return "intense action atmosphere, dynamic motion blur, adrenaline energy"
	case strings.Contains(e, "epic") || strings.Contains(e, "史诗") ||
		strings.Contains(e, "宏大") || strings.Contains(e, "climax") || strings.Contains(e, "高潮"):
		return "epic grand atmosphere, sweeping cinematic motion, heroic scale"
	case strings.Contains(e, "dramatic") || strings.Contains(e, "紧张") ||
		strings.Contains(e, "suspense") || strings.Contains(e, "danger") || strings.Contains(e, "tension"):
		return "dramatic tense atmosphere, deep shadows, ominous mood"
	case strings.Contains(e, "romantic") || strings.Contains(e, "浪漫") ||
		strings.Contains(e, "tender") || strings.Contains(e, "温情"):
		return "soft romantic atmosphere, warm golden bokeh, intimate mood"
	case strings.Contains(e, "sad") || strings.Contains(e, "悲") ||
		strings.Contains(e, "grief") || strings.Contains(e, "离别") || strings.Contains(e, "melancholy"):
		return "melancholic somber atmosphere, cool desaturated tones, slow motion feel"
	case strings.Contains(e, "landscape") || strings.Contains(e, "风景") ||
		strings.Contains(e, "scenery") || strings.Contains(e, "空镜"):
		return "breathtaking scenic vista, sweeping majestic atmosphere"
	case strings.Contains(e, "peaceful") || strings.Contains(e, "平静") || strings.Contains(e, "calm"):
		return "serene tranquil atmosphere, soft diffused light, gentle motion"
	case strings.Contains(e, "funny") || strings.Contains(e, "humorous") || strings.Contains(e, "幽默"):
		return "lively energetic atmosphere, bright warm tones"
	default:
		return ""
	}
}

// resolve3DStylePrefix 返回对应 3D 风格的提示词前缀。
func resolve3DStylePrefix(style string) string {
	switch style {
	case "pixar":
		return "Pixar-style 3D animation, stylized characters, warm appealing lighting, Disney Pixar quality render"
	case "anime3d":
		return "3D anime style, cel-shaded 3D, vibrant colors, smooth 3D animation, Japanese anime 3D render"
	case "realistic3d":
		return "ultra-realistic 3D render, Unreal Engine 5, ray tracing global illumination, cinematic 3D, 8K 3D rendering"
	default: // "cg"
		return "3D CGI animation, ray tracing, volumetric lighting, subsurface scattering, photorealistic 3D render, high-fidelity 3D"
	}
}

// PollShotStatus 轮询单个分镜视频生成状态
func (s *VideoService) PollShotStatus(shot *model.StoryboardShot) error {
	// 超时检查
	if shot.Status == "processing" && time.Since(shot.UpdatedAt) > 30*time.Minute {
		logger.Printf("PollShotStatus: shot %d timed out (taskID=%s), marking failed", shot.ShotNo, shot.ShotTaskID)
		shot.Status = "failed"
		shot.RetryCount++
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] PollShotStatus: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		return nil
	}

	provider, ok := s.videoProviders[shot.ShotProviderName]
	if !ok {
		return fmt.Errorf("provider %s not found", shot.ShotProviderName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	taskStatus, err := provider.GetVideoStatus(ctx, shot.ShotTaskID)
	if err != nil {
		return err
	}

	logger.Printf("PollShotStatus: shot %d taskID=%s status=%s", shot.ShotNo, shot.ShotTaskID, taskStatus.Status)

	switch taskStatus.Status {
	case "completed", "succeed":
		videoURL, urlErr := provider.GetVideoURL(ctx, shot.ShotTaskID)
		if urlErr != nil {
			return urlErr
		}

		// 立即下载到本地，防止临时签名 URL 在拼接时过期
		logger.Printf("PollShotStatus: shot %d completed, downloading clip", shot.ShotNo)
		localClip := fmt.Sprintf("%s/inkframe-shot-%d.mp4", inkframeTempDir(), shot.ID)
		if dlErr := downloadFile(videoURL, localClip); dlErr != nil {
			logger.Printf("PollShotStatus: download shot %d clip failed (%v), storing URL as fallback", shot.ID, dlErr)
			shot.ClipPath = videoURL
		} else {
			logger.Printf("PollShotStatus: shot %d clip saved to %s", shot.ShotNo, localClip)
			shot.ClipPath = "file://" + localClip
		}
		shot.Status = "completed"

		// 一致性评分（可选）
		if s.consistencyService != nil && shot.ChapterID != nil {
			chapter, _ := s.chapterRepo.GetByID(*shot.ChapterID)
			if chapter != nil {
				chars, _ := s.characterRepo.ListByNovel(chapter.NovelID)
				for _, c := range chars {
					if c.Portrait != "" {
						score, scoreErr := s.consistencyService.CalculateConsistencyScore(c.Portrait, []string{videoURL})
						if scoreErr == nil {
							shot.ConsistencyScore = score.OverallScore
							// 一致性过低时自动重试
							if score.OverallScore < 0.5 && shot.RetryCount < 2 {
								shot.Status = "pending"
								shot.RetryCount++
								shot.ClipPath = ""
								shot.ConsistencyScore = 0
								shot.ShotTaskID = "" // 必须清除，否则重试不会重新提交
							}
						}
						break
					}
				}
			}
		}
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] PollShotStatus: failed to update shot %d status: %v", shot.ShotNo, err)
		}

		// 时序连贯：提取本镜最后一帧，存入下一镜 ReferenceImageURL
		if shot.Status == "completed" && strings.HasPrefix(shot.ClipPath, "file://") {
			if lastFramePath, frameErr := s.extractLastFrame(shot.ClipPath); frameErr == nil {
				// 查找下一镜（同 VideoID，ShotNo+1）
				nextShots, listErr := s.storyboardRepo.ListByVideo(shot.VideoID)
				if listErr == nil {
					for _, ns := range nextShots {
						if ns.ShotNo == shot.ShotNo+1 && ns.ShotTaskID == "" {
							ns.ReferenceImageURL = "file://" + lastFramePath
							if err := s.storyboardRepo.Update(ns); err != nil {
								logger.Printf("[VideoService] PollShotStatus: failed to update next shot %d reference image: %v", ns.ShotNo, err)
							}
							break
						}
					}
				}
			} else {
				logger.Printf("PollShotStatus: extractLastFrame for shot %d failed: %v", shot.ShotNo, frameErr)
			}
		}

	case "failed":
		logger.Printf("PollShotStatus: shot %d failed (retry=%d)", shot.ShotNo, shot.RetryCount)
		shot.Status = "failed"
		shot.RetryCount++
		if shot.RetryCount < 2 {
			shot.Status = "pending"
			shot.ShotTaskID = ""
			logger.Printf("PollShotStatus: shot %d will be retried", shot.ShotNo)
		}
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] PollShotStatus: failed to update shot %d status after failure: %v", shot.ShotNo, err)
		}
	}

	return nil
}

// GenerateShotAudio 为单个分镜生成 TTS 音频，优先使用角色声音设置。
// 朗读文本优先级：Dialogue（角色台词）> Narration（旁白文案）> Description（英文画面描述，兜底）。
// narrationVoice 为旁白音色 ID（空串则自动从项目配置加载，仍空则降级到 "alloy"）。
// 若注入了 storageSvc，将音频上传至 OSS 或 DB，并更新 shot.AudioPath 为持久 URL。
func (s *VideoService) GenerateShotAudio(shot *model.StoryboardShot, tenantID uint, narrationVoice string) error {
	// 并发限流：防止批量生成时同时发起过多 TTS 请求触发 API 限速（429）
	if s.audioSem != nil {
		s.audioSem <- struct{}{}
		defer func() { <-s.audioSem }()
	}

	text := shot.Dialogue
	// 去掉 "角色名：内容" 格式中的角色名前缀，避免 TTS 朗读出角色名
	for _, sep := range []string{"：", ":"} {
		if idx := strings.Index(text, sep); idx > 0 && idx < 20 {
			text = strings.TrimSpace(text[idx+len(sep):])
			break
		}
	}
	if text == "" {
		text = shot.Narration // 旁白文案（中文，无镜头语言）
	}
	if text == "" {
		text = shot.Description // 兜底：英文画面描述（旧数据兼容）
	}
	if text == "" {
		return nil
	}

	// 预加载 video 一次（供旁白音色加载 + resolveVoice + uploadAudio 三处共用，避免重复 DB 查询）
	var novelID, chapterID uint
	if s.videoRepo != nil {
		if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
			novelID = video.NovelID
			if video.ChapterID != nil {
				chapterID = *video.ChapterID
			}
			if narrationVoice == "" && s.novelRepo != nil && video.NovelID > 0 {
				if novel, err := s.novelRepo.GetByID(video.NovelID); err == nil {
					narrationVoice = novel.VideoConf().NarrationVoice
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	voice, speed, style := s.resolveVoiceForShot(shot, narrationVoice, novelID)
	logger.Printf("GenerateShotAudio: shot %d voice=%s speed=%.1f style=%q text=%q", shot.ShotNo, voice, speed, style, text)

	audioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, text, voice, speed, style)
	if err != nil {
		logger.Printf("GenerateShotAudio: shot %d TTS failed: %v", shot.ShotNo, err)
		return fmt.Errorf("TTS generation failed: %w", err)
	}
	if audioURL == "" {
		return fmt.Errorf("TTS returned empty audio URL")
	}
	logger.Printf("GenerateShotAudio: shot %d TTS ok url=%s", shot.ShotNo, audioURL)

	// 先计算配音时长（必须在上传前读取，上传后 audioURL 会被替换为 OSS URL）
	localAudioURL := audioURL
	if strings.HasPrefix(localAudioURL, "file://") {
		if data, e := os.ReadFile(strings.TrimPrefix(localAudioURL, "file://")); e == nil {
			if d := mp3Duration(data); d > 0 && d > shot.Duration {
				shot.Duration = d
			}
		}
	}

	// 若配置了存储服务，将音频上传至持久存储
	if s.storageSvc != nil {
		persistURL, uploadErr := s.uploadAudioToStorage(ctx, shot, audioURL, novelID, chapterID)
		if uploadErr != nil {
			logger.Printf("GenerateShotAudio: storage upload failed (falling back to local): %v", uploadErr)
		} else {
			audioURL = persistURL
			logger.Printf("GenerateShotAudio: shot %d audio stored at %s", shot.ShotNo, audioURL)
			// 删除本次新建的 /tmp 临时文件（修复：之前错误地删除旧 shot.AudioPath）
			if strings.HasPrefix(localAudioURL, "file://") {
				os.Remove(strings.TrimPrefix(localAudioURL, "file://")) //nolint:errcheck
			}
		}
	}

	shot.AudioPath = audioURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateShotAudio: failed to update shot %d audio path: %v", shot.ShotNo, err)
	}
	return nil
}

// uploadAudioToStorage 读取 TTS 输出（file:// 路径或 HTTP URL），上传并返回持久 URL。
// novelID/chapterID 由调用方提供，避免重复查询 video 记录。
func (s *VideoService) uploadAudioToStorage(ctx context.Context, shot *model.StoryboardShot, audioURL string, novelID, chapterID uint) (string, error) {
	var data []byte
	var readErr error

	if strings.HasPrefix(audioURL, "file://") {
		data, readErr = os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
	} else if strings.HasPrefix(audioURL, "http://") || strings.HasPrefix(audioURL, "https://") {
		resp, err := http.Get(audioURL) //nolint:gosec
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		data, readErr = io.ReadAll(resp.Body)
	} else {
		return "", fmt.Errorf("unsupported audio URL scheme: %s", audioURL)
	}
	if readErr != nil {
		return "", readErr
	}

	filename := fmt.Sprintf("shot-%d.mp3", shot.ID)
	key := storage.BuildKey(novelID, chapterID, "audio", filename)
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}

// GenerateShotSRT 根据分镜的台词/旁白和时长生成单条 SRT 字幕内容。
// 时间码从 00:00:00,000 开始，结束时间 = shot.Duration。
// 文本优先级：Dialogue > Narration > Description（兜底兼容旧数据）。
func GenerateShotSRT(shot *model.StoryboardShot) string {
	var text string
	if shot.Subtitle != "" {
		text = shot.Subtitle
	} else if shot.Dialogue != "" {
		// 去除"角色名："前缀，字幕只显示台词内容
		text = stripDialogueSpeakerPrefix(shot.Dialogue)
	} else if shot.Narration != "" {
		text = shot.Narration
	} else {
		text = shot.Description
	}
	if text == "" {
		return ""
	}
	dur := shot.Duration
	if dur <= 0 {
		dur = 5.0
	}
	end := formatSRTTimecode(dur)
	return fmt.Sprintf("1\n00:00:00,000 --> %s\n%s\n", end, text)
}

// formatSRTTimecode 将秒数格式化为 SRT 时间码 HH:MM:SS,mmm
func formatSRTTimecode(secs float64) string {
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	s := int(secs) % 60
	ms := int((secs-float64(int(secs)))*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// resolveVoiceForShot 解析分镜对应角色的配音设置（voice, speed, style）。
// 优先级：① 对话文本「角色名：」前缀 → ② shot.CharacterIDs 第一个角色 → ③ narrationVoice → ④ alloy。
// novelID 由调用方提供（避免此函数重复查询 video 记录）。
func (s *VideoService) resolveVoiceForShot(shot *model.StoryboardShot, narrationVoice string, novelID uint) (voice string, speed float64, style string) {
	voice = "alloy"
	if narrationVoice != "" {
		voice = narrationVoice
	}
	speed = 1.0

	if novelID == 0 || s.characterRepo == nil {
		return
	}

	// 步骤一：从对话中解析发言角色（格式：角色名：对话内容 或 角色名:对话内容）
	speakerName := ""
	for _, sep := range []string{"：", ":"} {
		if idx := strings.Index(shot.Dialogue, sep); idx > 0 && idx < 20 {
			speakerName = strings.TrimSpace(shot.Dialogue[:idx])
			break
		}
	}

	applyCharVoice := func(c *model.Character) {
		if c.VoiceID != "" {
			voice = c.VoiceID
		}
		if c.VoiceSpeed > 0 {
			speed = c.VoiceSpeed
		}
		style = c.VoiceStyle
	}

	if speakerName != "" {
		// 使用带 TTL 缓存的角色列表（批量配音时避免 N+1 查询）
		characters, err := s.listCharsByNovelCached(novelID)
		if err != nil {
			return
		}
		for _, c := range characters {
			if strings.EqualFold(c.Name, speakerName) {
				applyCharVoice(c)
				return
			}
		}
	}

	// 步骤二：仅当分镜有角色台词（Dialogue 非空）时，才使用第一个绑定角色的音色。
	// 旁白/描述文本不应被角色音色覆盖，应沿用 narrationVoice。
	if len(shot.CharacterIDs) > 0 && shot.Dialogue != "" {
		char, err := s.characterRepo.GetByID(shot.CharacterIDs[0])
		if err == nil && char != nil {
			applyCharVoice(char)
		}
	}

	return
}

// downloadFile 下载 HTTP URL 到本地路径
func downloadFile(url, dest string) error {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// StitchVideo 将所有 completed 分镜拼接为最终视频
func (s *VideoService) StitchVideo(videoID uint) (string, error) {
	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "completed")
	if err != nil {
		return "", err
	}
	if len(shots) == 0 {
		return "", fmt.Errorf("no completed shots to stitch")
	}

	tmpDir := fmt.Sprintf("%s/inkframe-%d", inkframeTempDir(), videoID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var localShotFiles []string // 记录 PollShotStatus 下载的本地文件，拼接后清理
	defer func() {
		for _, f := range localShotFiles {
			os.Remove(f) //nolint:errcheck
		}
	}()
	var concatLines []string
	for i, shot := range shots {
		// 跳过无视频片段的镜头（仅有图片，Ken Burns 未生成）
		if shot.ClipPath == "" {
			logger.Printf("StitchVideo: shot %d has no clip, skipping", shot.ShotNo)
			continue
		}

		clipFile := fmt.Sprintf("%s/clip_%d.mp4", tmpDir, i)
		finalClip := clipFile

		// 如果已是本地文件（PollShotStatus 立即下载过），直接使用，无需再下载
		if strings.HasPrefix(shot.ClipPath, "file://") {
			clipFile = strings.TrimPrefix(shot.ClipPath, "file://")
			finalClip = clipFile
			localShotFiles = append(localShotFiles, clipFile)
		} else {
			// 仍是远程 URL（fallback），下载到 tmpDir
			if err := downloadFile(shot.ClipPath, clipFile); err != nil {
				// URL 可能已过期，尝试从 provider 重新获取
				if shot.ShotTaskID != "" && shot.ShotProviderName != "" {
					if p, ok := s.videoProviders[shot.ShotProviderName]; ok {
						rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
						freshURL, fErr := p.GetVideoURL(rCtx, shot.ShotTaskID)
						rCancel()
						if fErr == nil {
							if err2 := downloadFile(freshURL, clipFile); err2 != nil {
								return "", fmt.Errorf("download shot %d clip failed (fresh URL also failed): %w", shot.ShotNo, err2)
							}
						} else {
							return "", fmt.Errorf("download shot %d clip failed and refresh URL failed: %w", shot.ShotNo, err)
						}
					} else {
						return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
					}
				} else {
					return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
				}
			}
		}

		// Merge audio if present
		if shot.AudioPath != "" {
			audioPath := strings.TrimPrefix(shot.AudioPath, "file://")
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			if _, err := runFFmpegCtx(context.Background(), "-y",
				"-i", clipFile,
				"-i", audioPath,
				"-c:v", "copy",
				"-c:a", "aac",
				"-shortest",
				mergedFile,
			); err != nil {
				logger.Printf("StitchVideo: merge audio for shot %d failed: %v, using clip without audio", shot.ShotNo, err)
			} else {
				finalClip = mergedFile
			}
		}

		concatLines = append(concatLines, fmt.Sprintf("file '%s'", finalClip))
	}

	listFile := fmt.Sprintf("%s/list.txt", tmpDir)
	if err := os.WriteFile(listFile, []byte(strings.Join(concatLines, "\n")), 0644); err != nil {
		return "", err
	}

	stitchedPath := fmt.Sprintf("%s/inkframe-%d-stitched.mp4", inkframeTempDir(), videoID)
	if _, err := runFFmpegCtx(context.Background(), "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		stitchedPath,
	); err != nil {
		return "", fmt.Errorf("ffmpeg stitch failed: %w", err)
	}

	// BGM 混音（非致命：失败时使用无BGM版本）
	outputPath := fmt.Sprintf("%s/inkframe-%d-output.mp4", inkframeTempDir(), videoID)
	if s.bgmService != nil {
		bgmURL := s.bgmService.SelectBGM("")
		if bgmURL != "" {
			if mixErr := s.bgmService.MixBGM(stitchedPath, bgmURL, outputPath); mixErr != nil {
				logger.Printf("StitchVideo: BGM mixing failed (video %d): %v, using stitched without BGM", videoID, mixErr)
				outputPath = stitchedPath
			}
		} else {
			outputPath = stitchedPath
		}
	} else {
		outputPath = stitchedPath
	}

	// Update video record
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return outputPath, nil
	}
	video.VideoPath = outputPath
	video.Status = "completed"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[VideoService] StitchVideo: failed to update video %d status to completed: %v", videoID, err)
	}

	return outputPath, nil
}

// inkframeTempDir 返回可写的临时目录（绝对路径，优先用工作目录下的 tmp/，fallback 到系统临时目录）
// 必须返回绝对路径，否则嵌入式 WASM ffmpeg 无法通过 WASI 文件系统访问。
func inkframeTempDir() string {
	dir := "tmp"
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0755); err == nil {
		return dir
	}
	return os.TempDir()
}

// downloadToTemp 将 URL 下载到临时文件，返回本地路径
func downloadToTemp(url, prefix, ext string) (string, error) {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmpFile, err := os.CreateTemp(inkframeTempDir(), prefix+"*"+ext)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

// generateKenBurnsClip 使用 FFmpeg zoompan 滤镜将静图制作成 Ken Burns 动效短片
// generateStillFrameClip 用 FFmpeg 将静态图片编码为固定时长的视频（无动效，Ken Burns 降级方案）。
func (s *VideoService) generateStillFrameClip(imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = 5.0
	}
	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}
	parts := strings.SplitN(resolution, ":", 2)
	w, h := parts[0], parts[1]
	vf := fmt.Sprintf("scale=%s:%s:force_original_aspect_ratio=decrease,pad=%s:%s:(ow-iw)/2:(oh-ih)/2,setsar=1", w, h, w, h)
	outPath := fmt.Sprintf("%s/inkframe-still-%d.mp4", inkframeTempDir(), time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := runFFmpegCtx(ctx, "-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.2f", duration),
		"-i", imagePath,
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", "24",
		outPath,
	); err != nil {
		return "", fmt.Errorf("ffmpeg still frame: %w", err)
	}
	return outPath, nil
}

func (s *VideoService) generateKenBurnsClip(shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = 5.0
	}
	fps := 30
	totalFrames := int(duration * float64(fps))

	resolution := "1920:1080"
	switch aspectRatio {
	case "9:16":
		resolution = "1080:1920"
	case "1:1":
		resolution = "1080:1080"
	case "4:3":
		resolution = "1440:1080"
	}

	// 根据摄像机类型选择 zoompan 动效
	var zoompan string
	switch shot.CameraType {
	case "zoom", "push":
		// 推镜/变焦：明显放大，模拟向前推进
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.002,1.5)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	case "pull":
		// 拉镜：缩小，模拟向后拉远（从1.4缩到1.0）
		zoompan = fmt.Sprintf("zoompan=z='max(1.4-t*0.4/%.1f,1.0)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", duration, totalFrames)
	case "pan", "track":
		// 摇镜/移镜：水平平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f))':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	case "crane_up":
		// 升镜：向上平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(ih-(ih/zoom)-t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "crane_down":
		// 降镜：向下平移
		zoompan = fmt.Sprintf("zoompan=z=1.3:d=%d:x='iw/2-(iw/zoom/2)':y='trunc(t*((ih-(ih/zoom))/%.1f))'", totalFrames, duration)
	case "whip_pan":
		// 甩镜：快速水平扫过
		zoompan = fmt.Sprintf("zoompan=z=1.2:d=%d:x='trunc(iw/2-(iw/zoom/2)+t*((iw-(iw/zoom))/%.1f)*2)':y='ih/2-(ih/zoom/2)'", totalFrames, duration)
	default:
		// static / follow / arc / tilt / 旧值：默认轻微放大（Ken Burns 经典效果）
		zoompan = fmt.Sprintf("zoompan=z='min(zoom+0.0008,1.2)':d=%d:x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)'", totalFrames)
	}

	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%d.mp4", inkframeTempDir(), shot.ID, time.Now().UnixNano())
	// pre-scale 到恰好等于输出分辨率：zoompan 的 zoom≤1.2 只需输入≥输出即可，更大对效果无益
	// 但会让 WASM 每帧计算量成倍增加（3840 vs 1920 = 4x 像素量）。
	// 1920:-1 for 16:9, 1080:-1 for 9:16/1:1 — 均与最终输出宽度对齐。
	preScale := "1920:-1"
	if aspectRatio == "9:16" || aspectRatio == "1:1" {
		preScale = "1080:-1"
	}
	vf := fmt.Sprintf("scale=%s,%s,scale=%s,setsar=1", preScale, zoompan, resolution)

	// WASM FFmpeg 的纯计算阶段不响应 context 取消，用 goroutine + channel 在 Go 层强制超时，
	// 超时后解除调用方阻塞（WASM goroutine 在后台继续跑完或被 GC 回收）。
	// 30s：以 1920:-1 输入跑 zoompan，普通 CPU 通常在 10-25s 内完成；超时则快速降级 still frame。
	const kenBurnsTimeout = 30 * time.Second
	type result struct {
		err []byte
		rc  error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := runFFmpegCtx(context.Background(), "-y",
			"-loop", "1",
			"-t", fmt.Sprintf("%.2f", duration),
			"-i", imagePath,
			"-vf", vf,
			"-c:v", "libx264",
			"-pix_fmt", "yuv420p",
			"-r", fmt.Sprintf("%d", fps),
			"-threads", "1",
			outPath,
		)
		ch <- result{out, err}
	}()
	select {
	case res := <-ch:
		if res.rc != nil {
			return "", fmt.Errorf("ffmpeg ken burns: %w", res.rc)
		}
	case <-time.After(kenBurnsTimeout):
		return "", fmt.Errorf("ffmpeg ken burns: timed out after %s", kenBurnsTimeout)
	}
	return outPath, nil
}

// generateShotImageOnly 执行图片解说模式的第一阶段：生成图片 + 下载到本地临时文件。
// 返回本地文件路径和实际视频时长；调用方负责在使用完毕后删除该文件。
// shot.Status 会在此函数内被设置为 "generating"；完成后调用方应更新为 "completed"。
func (s *VideoService) generateShotImageOnly(shot *model.StoryboardShot, aspectRatio string) (localImage string, duration float64, err error) {
	duration = shot.Duration
	if duration <= 0 {
		duration = 5.0
	}
	// 视频时长不能低于音频时长
	if shot.AudioPath != "" {
		if data, readErr := readLocalOrRemoteFile(shot.AudioPath); readErr == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				if audioDur := float64(micros) / 1_000_000; audioDur > duration {
					logger.Printf("generateShotImageOnly: shot %d extending duration %.2f→%.2fs to cover audio", shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}
	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return "", 0, fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return "", 0, fmt.Errorf("image generation failed for shot %d (empty URL)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] generateShotImageOnly: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	localImage, err = downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		return "", 0, fmt.Errorf("download image for shot %d: %w", shot.ShotNo, err)
	}
	return localImage, duration, nil
}

// generateClipAndUploadWithRetry 在后台 goroutine 中执行 Ken Burns 编码 + OSS 上传，
// 支持最多 maxClipRetries 次自动重试（指数退避）。
// 无论成功与否，最终均将 progress 更新为 100，并清理本地临时文件。
const maxClipRetries = 3

func (s *VideoService) generateClipAndUploadWithRetry(ctx context.Context, shotID uint, localImage string, duration float64, aspectRatio string) {
	defer os.Remove(localImage)

	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		logger.Printf("generateClipAndUploadWithRetry: shot %d not found: %v", shotID, err)
		return
	}

	var clipPath string
	var lastErr error

	for attempt := 1; attempt <= maxClipRetries; attempt++ {
		// 优先纯 Go Ken Burns；失败时降级为静止画面
		clipPath, lastErr = s.generateKenBurnsPureGo(ctx, shot, localImage, duration, aspectRatio)
		if lastErr != nil {
			logger.Printf("generateClipAndUploadWithRetry: shot %d ken burns attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
			clipPath, lastErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
		}
		if lastErr == nil {
			break
		}
		logger.Printf("generateClipAndUploadWithRetry: shot %d still frame attempt %d/%d: %v", shot.ShotNo, attempt, maxClipRetries, lastErr)
		if attempt < maxClipRetries {
			select {
			case <-time.After(time.Duration(attempt*5) * time.Second):
			case <-ctx.Done():
				logger.Printf("[VideoService] generateClipAndUploadWithRetry: context cancelled for shot %d, stopping retries", shotID)
				return
			}
		}
	}

	fields := map[string]interface{}{"progress": 100}
	if lastErr != nil {
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip failed after %d attempts, keeping image-only: %v", shot.ShotNo, maxClipRetries, lastErr)
	} else if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		fields["video_url"] = ossURL
		fields["clip_path"] = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip → %s", shot.ShotNo, ossURL)
	} else {
		fields["clip_path"] = "file://" + clipPath
		logger.Printf("generateClipAndUploadWithRetry: shot %d clip done (local only)", shot.ShotNo)
	}
	if err := s.storyboardRepo.UpdateFields(shotID, fields); err != nil {
		logger.Printf("[VideoService] generateClipAndUploadWithRetry: failed to update shot %d fields: %v", shotID, err)
	}
}

// GenerateSlideshowShotVideo 为单个分镜生成图片并应用 Ken Burns 动效（图片解说模式）
// 此函数保持同步语义，供 runSlideshowPipeline 的顺序流水线使用。
// BatchGenerateShots 中的批量生成改用 generateShotImageOnly + generateClipAndUploadWithRetry 两阶段异步模式。
func (s *VideoService) GenerateSlideshowShotVideo(shot *model.StoryboardShot, aspectRatio string) error {
	duration := shot.Duration
	if duration <= 0 {
		duration = 5.0
	}

	// 视频时长不能低于音频时长：读取已生成的 TTS 音频，若音频更长则扩展 duration。
	if shot.AudioPath != "" {
		if data, err := readLocalOrRemoteFile(shot.AudioPath); err == nil && len(data) > 0 {
			ext := audioExtension(shot.AudioPath)
			if micros := parseAudioDurationMicros(data, ext); micros > 0 {
				audioDur := float64(micros) / 1_000_000
				if audioDur > duration {
					logger.Printf("GenerateSlideshowShotVideo: shot %d extending duration %.2f→%.2fs to cover audio",
						shot.ShotNo, duration, audioDur)
					duration = audioDur
					shot.Duration = audioDur
				}
			}
		}
	}

	logger.Printf("GenerateSlideshowShotVideo: shot %d aspect=%s duration=%.1fs", shot.ShotNo, aspectRatio, duration)

	shot.GenerationMode = "static"
	shot.Status = "generating"
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to generating: %v", shot.ShotNo, err)
	}

	// 1. 生成图片
	imageURL, imgErr := s.generateShotReferenceImage(shot)
	if imageURL == "" {
		errMsg := "image provider returned empty URL"
		if imgErr != nil {
			errMsg = imgErr.Error()
		}
		logger.Printf("GenerateSlideshowShotVideo: image gen failed for shot %d: %s", shot.ShotNo, errMsg)
		shot.Status = "failed"
		shot.ErrorMessage = errMsg
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d status to failed: %v", shot.ShotNo, err)
		}
		if imgErr != nil {
			return fmt.Errorf("image generation failed for shot %d: %w", shot.ShotNo, imgErr)
		}
		return fmt.Errorf("image generation failed for shot %d (empty URL returned)", shot.ShotNo)
	}
	shot.ImageURL = imageURL
	logger.Printf("GenerateSlideshowShotVideo: shot %d storing image_url=%q (len=%d)", shot.ShotNo, imageURL, len(imageURL))
	// 保存图片 URL（后续步骤失败时图片仍可用）
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateSlideshowShotVideo: failed to update shot %d image URL: %v", shot.ShotNo, err)
	}

	// 2. 下载图片到本地（Volcengine 返回的 URL 后缀为 .image，FFmpeg 无法识别格式，需重命名为 .jpg）
	logger.Printf("GenerateSlideshowShotVideo: shot %d downloading image for ffmpeg", shot.ShotNo)
	localImage, err := downloadToTemp(imageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
	if err != nil {
		logger.Printf("GenerateSlideshowShotVideo: download image failed for shot %d, marking completed with image only: %v", shot.ShotNo, err)
		shot.Status = "completed"
		shot.Progress = 100
		shot.ErrorMessage = fmt.Sprintf("ken burns skipped: %v", err)
		return s.storyboardRepo.Update(shot)
	}
	defer os.Remove(localImage)

	// 3. Ken Burns 动效（缓慢推拉/平移，让静态图更生动）；失败时降级为静止画面
	// 首选纯 Go 实现（逐帧计算 + WASM 编码）：速度快、可被 context 取消。
	// 若失败则降级为静止画面（跳过旧的 WASM zoompan 方案以避免长时间阻塞）。
	logger.Printf("GenerateSlideshowShotVideo: shot %d starting ken burns (pure Go)", shot.ShotNo)
	clipPath, err := s.generateKenBurnsPureGo(context.Background(), shot, localImage, duration, aspectRatio)
	if err != nil {
		logger.Printf("GenerateSlideshowShotVideo: ken burns failed for shot %d, falling back to still frame: %v", shot.ShotNo, err)
		// 降级：用静止画面代替 Ken Burns
		clipPath, err = s.generateStillFrameClip(localImage, duration, aspectRatio)
		if err != nil {
			logger.Printf("GenerateSlideshowShotVideo: still frame fallback also failed for shot %d, completing with image only: %v", shot.ShotNo, err)
			shot.Status = "completed"
			shot.Progress = 100
			shot.ErrorMessage = fmt.Sprintf("ffmpeg unavailable: %v", err)
			return s.storyboardRepo.Update(shot)
		}
		shot.ErrorMessage = "ken burns skipped, used still frame"
	}

	// 优先上传到 OSS（持久存储），成功后清除本地 file:// 路径并删除临时文件。
	// 失败时降级保留 file:// 路径（本地可访问但重启后失效）。
	if ossURL := s.uploadClipToStorage(context.Background(), shot, clipPath); ossURL != "" {
		shot.VideoURL = ossURL
		shot.ClipPath = ""
		os.Remove(clipPath) //nolint:errcheck
		logger.Printf("GenerateSlideshowShotVideo: shot %d clip uploaded → %s", shot.ShotNo, ossURL)
	} else {
		shot.ClipPath = "file://" + clipPath
		logger.Printf("GenerateSlideshowShotVideo: shot %d completed clip=%s (local only)", shot.ShotNo, clipPath)
	}
	shot.Status = "completed"
	shot.Progress = 100
	return s.storyboardRepo.Update(shot)
}

// uploadClipToStorage 将本地 MP4 文件上传到持久存储（OSS），返回持久 URL。
// storageSvc 为 nil 或上传失败时返回 ""（调用方保留 file:// 本地路径）。
// OSS key 格式：novels/{title}/chapters/{no}/videos/{uuid}.mp4
//
//	章节 ID 未知时降级：videos/{uuid}.mp4
func (s *VideoService) uploadClipToStorage(ctx context.Context, shot *model.StoryboardShot, clipPath string) string {
	if s.storageSvc == nil {
		return ""
	}
	data, err := os.ReadFile(clipPath)
	if err != nil {
		logger.Printf("uploadClipToStorage: read %s: %v", clipPath, err)
		return ""
	}

	filename := uuid.New().String() + ".mp4"
	key := fmt.Sprintf("videos/%s", filename) // fallback key

	if shot.ChapterID != nil {
		if ch, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil {
			if novel, err := s.novelRepo.GetByID(ch.NovelID); err == nil && novel.Title != "" {
				if sanitized := sanitizeStorageName(novel.Title); sanitized != "" {
					key = fmt.Sprintf("novels/%s/chapters/%d/videos/%s", sanitized, ch.ChapterNo, filename)
				}
			}
		}
	}

	ossURL, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "video/mp4")
	if err != nil {
		logger.Printf("uploadClipToStorage: upload failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}
	return ossURL
}

// runSlideshowPipeline 异步处理图片解说模式的所有分镜，完成后自动拼接
func (s *VideoService) runSlideshowPipeline(videoID uint) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Printf("runSlideshowPipeline: get video %d failed: %v", videoID, err)
		return
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil || len(shots) == 0 {
		logger.Printf("runSlideshowPipeline: no pending shots for video %d", videoID)
		return
	}

	for _, shot := range shots {
		if err := s.GenerateSlideshowShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("runSlideshowPipeline: shot %d failed: %v", shot.ShotNo, err)
		}
		go s.GenerateShotAudio(shot, video.TenantID, "") //nolint:errcheck
	}

	// 拼接
	if _, err := s.StitchVideo(videoID); err != nil {
		logger.Printf("runSlideshowPipeline: stitch video %d failed: %v", videoID, err)
		if v, _ := s.videoRepo.GetByID(videoID); v != nil {
			v.Status = "failed"
			v.ErrorMessage = err.Error()
			if updErr := s.videoRepo.Update(v); updErr != nil {
				logger.Printf("[VideoService] runSlideshowPipeline: failed to update video %d status to failed: %v", videoID, updErr)
			}
		}
	}
}

// GenerateAllShotVideos 提交所有待生成分镜的视频任务
func (s *VideoService) GenerateAllShotVideos(videoID uint) error {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	// 图片解说模式：异步生成图片，完成后自动拼接
	if video.Mode == "slideshow" {
		shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if err != nil || len(shots) == 0 {
			return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
		}
		video.Status = "generating"
		video.ErrorMessage = ""
		if err := s.videoRepo.Update(video); err != nil {
			logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
		}
		go s.runSlideshowPipeline(videoID)
		return nil
	}

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
	if err != nil {
		return err
	}
	if len(shots) == 0 {
		return fmt.Errorf("no pending shots found for video %d (generate storyboard first)", videoID)
	}

	// 更新状态，让用户可以通过 GetStatus 感知进度
	video.Status = "generating"
	video.ErrorMessage = ""
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[VideoService] GenerateAllShotVideos: failed to update video %d status to generating: %v", videoID, err)
	}

	for _, shot := range shots {
		if err := s.GenerateShotVideo(shot, video.AspectRatio); err != nil {
			logger.Printf("GenerateAllShotVideos: shot %d failed: %v", shot.ShotNo, err)
			continue
		}
		// TTS audio in parallel
		go s.GenerateShotAudio(shot, video.TenantID, "") //nolint:errcheck
	}
	return nil
}

// PollAndStitchVideo 后台轮询所有分镜状态，完成后拼接
func (s *VideoService) PollAndStitchVideo(videoID uint) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Hour)
	noProgressCount := 0

	for {
		if time.Now().After(deadline) {
			logger.Printf("PollAndStitchVideo: videoID %d timed out after 2h", videoID)
			video, _ := s.videoRepo.GetByID(videoID)
			if video != nil && video.Status != "completed" {
				video.Status = "failed"
				video.ErrorMessage = "stitch pipeline timed out (>2h)"
				if err := s.videoRepo.Update(video); err != nil {
					logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (timeout): %v", videoID, err)
				}
			}
			return
		}

		<-ticker.C

		// Retry pending shots (from consistency/failed retry)
		pending, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		for _, shot := range pending {
			if shot.ShotTaskID == "" {
				video, _ := s.videoRepo.GetByID(videoID)
				aspectRatio := "16:9"
				if video != nil {
					aspectRatio = video.AspectRatio
				}
				s.GenerateShotVideo(shot, aspectRatio) //nolint:errcheck
			}
		}

		// Poll processing shots
		processing, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		for _, shot := range processing {
			s.PollShotStatus(shot) //nolint:errcheck
		}

		// Check if all completed
		allShots, _ := s.storyboardRepo.ListByVideo(videoID)
		if len(allShots) == 0 {
			continue
		}
		completedCount := 0
		failedCount := 0
		for _, shot := range allShots {
			switch shot.Status {
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			}
		}

		if completedCount+failedCount == len(allShots) {
			if completedCount > 0 {
				if _, err := s.StitchVideo(videoID); err != nil {
					logger.Printf("PollAndStitchVideo: stitch failed: %v", err)
					video, _ := s.videoRepo.GetByID(videoID)
					if video != nil {
						video.Status = "failed"
						video.ErrorMessage = err.Error()
						if updErr := s.videoRepo.Update(video); updErr != nil {
							logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (stitch error): %v", videoID, updErr)
						}
					}
				}
			} else {
				video, _ := s.videoRepo.GetByID(videoID)
				if video != nil {
					video.Status = "failed"
					video.ErrorMessage = "all shots failed"
					if updErr := s.videoRepo.Update(video); updErr != nil {
						logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (all shots failed): %v", videoID, updErr)
					}
				}
			}
			return
		}

		// Stall detection (no progress after 5 ticks): re-query to get fresh counts
		processingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		pendingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if len(processingNow) == 0 && len(pendingNow) == 0 {
			noProgressCount++
			if noProgressCount >= 5 {
				logger.Printf("PollAndStitchVideo: videoID %d stalled, stopping", videoID)
				return
			}
		} else {
			noProgressCount = 0
		}
	}
}

// SynthesizeVideo 完整合成流水线（拼接→BGM→字幕→上传OSS），异步执行，返回 task_id。
func (s *VideoService) SynthesizeVideo(ctx context.Context, videoID uint, tenantID uint) (string, error) {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return "", fmt.Errorf("video not found: %w", err)
	}

	// 创建异步任务
	var taskID string
	if s.taskSvc != nil {
		task, err := s.taskSvc.Create(tenantID, "video_synthesis", "视频合成", "video", videoID)
		if err != nil {
			return "", fmt.Errorf("create task: %w", err)
		}
		taskID = task.TaskID
	} else {
		taskID = fmt.Sprintf("synth-%d", videoID)
	}

	go func() {
		synthCtx := context.Background()
		if s.taskSvc != nil {
			_ = s.taskSvc.SetRunning(taskID)
		}

		// 1. 拼接视频（含BGM）
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 10)
		}
		stitchedPath, err := s.StitchVideo(videoID)
		if err != nil {
			if s.taskSvc != nil {
				_ = s.taskSvc.Fail(taskID, "stitch failed: "+err.Error())
			}
			return
		}

		finalPath := stitchedPath

		// 2. 字幕烧录（可选）
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 40)
		}
		novelCfg := s.GetNovelVideoConfig(video.NovelID)
		if novelCfg != nil && novelCfg.SubtitleStyle != "" && novelCfg.SubtitleStyle != "none" {
			shots, err := s.storyboardRepo.ListByVideo(videoID)
			if err == nil && len(shots) > 0 {
				subtitleSvc := NewSubtitleService()
				fontName := "Noto Sans CJK SC"
				if novelCfg.SubtitleFont != "" {
					fontName = novelCfg.SubtitleFont
				}
				shotSlice := make([]model.StoryboardShot, len(shots))
				for i, sh := range shots {
					if sh != nil {
						shotSlice[i] = *sh
					}
				}
				assContent := subtitleSvc.GenerateASS(shotSlice, fontName)
				assPath := fmt.Sprintf("%s/inkframe-%d-subtitles.ass", inkframeTempDir(), videoID)
				if writeErr := os.WriteFile(assPath, []byte(assContent), 0644); writeErr == nil {
					burnedPath := fmt.Sprintf("%s/inkframe-%d-burned.mp4", inkframeTempDir(), videoID)
					if burnErr := subtitleSvc.BurnSubtitles(stitchedPath, assPath, burnedPath); burnErr == nil {
						finalPath = burnedPath
					} else {
						logger.Printf("SynthesizeVideo: subtitle burn failed for video %d: %v", videoID, burnErr)
					}
					os.Remove(assPath)
				}
			}
		}

		// 3. 提取封面
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 60)
		}
		coverPath := fmt.Sprintf("%s/inkframe-%d-cover.jpg", inkframeTempDir(), videoID)
		coverURL := ""
		if _, err := runFFmpegCtx(synthCtx, "-y", "-ss", "2", "-i", finalPath,
			"-frames:v", "1", "-vf", "scale=640:-1", coverPath); err == nil {
			defer os.Remove(coverPath)
		}

		// 4. 上传视频和封面到 OSS
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 70)
		}
		finalVideoURL := ""
		novel, _ := s.novelRepo.GetByID(video.NovelID)
		novelTitle := ""
		if novel != nil {
			novelTitle = sanitizeStorageName(novel.Title)
		}

		if s.storageSvc != nil {
			// 上传视频
			videoUUID := uuid.New().String()
			var videoKey string
			if novelTitle != "" {
				videoKey = fmt.Sprintf("novels/%s/videos/%s.mp4", novelTitle, videoUUID)
			} else {
				videoKey = fmt.Sprintf("videos/%s.mp4", videoUUID)
			}
			if vf, err := os.Open(finalPath); err == nil {
				defer vf.Close()
				if fi, err := vf.Stat(); err == nil {
					if ossURL, err := s.storageSvc.Upload(synthCtx, videoKey, vf, fi.Size(), "video/mp4"); err == nil {
						finalVideoURL = ossURL
					} else {
						logger.Printf("SynthesizeVideo: upload video failed for video %d: %v", videoID, err)
					}
				}
			}

			// 上传封面
			if cf, err := os.Open(coverPath); err == nil {
				defer cf.Close()
				if fi, err := cf.Stat(); err == nil {
					coverKey := videoKey[:len(videoKey)-4] + "_cover.jpg"
					if ossURL, err := s.storageSvc.Upload(synthCtx, coverKey, cf, fi.Size(), "image/jpeg"); err == nil {
						coverURL = ossURL
					} else {
						logger.Printf("SynthesizeVideo: upload cover failed for video %d: %v", videoID, err)
					}
				}
			}
		}

		// 5. 更新数据库
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 90)
		}
		if finalVideoURL != "" {
			video.FinalVideoURL = finalVideoURL
		}
		if coverURL != "" {
			video.CoverURL = coverURL
		}
		video.Status = "completed"
		if err := s.videoRepo.Update(video); err != nil {
			logger.Printf("SynthesizeVideo: update video %d failed: %v", videoID, err)
		}

		if s.taskSvc != nil {
			result := map[string]string{"final_video_url": finalVideoURL, "cover_url": coverURL}
			_ = s.taskSvc.Complete(taskID, result)
		}

		// 清理临时文件
		os.Remove(finalPath)
		if finalPath != stitchedPath {
			os.Remove(stitchedPath)
		}
	}()

	return taskID, nil
}

// ModelService 模型服务
