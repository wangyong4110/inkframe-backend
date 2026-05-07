package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// sfxCacheEntry caches API search results to avoid duplicate requests for the same query.
type sfxCacheEntry struct {
	url       string
	source    string
	expiresAt time.Time
}

const sfxCacheTTL = 24 * time.Hour

// SFXService 自动音效生成服务。
// 三层降级：本地 SFX 库 → Freesound API → ElevenLabs（AI生成）。
// Pixabay（music 类型）和 Jamendo 均为音乐平台，不提供逐帧音效，已从降级链中移除。
type SFXService struct {
	aiSvc            *AIService
	storageSvc       storage.Service
	storyboardRepo   *repository.StoryboardRepository
	sfxItemRepo      *repository.ShotSFXItemRepository
	sfxDir           string // 本地音效目录
	freesoundKey     string // Freesound API Token（可选）
	elevenKey        string // ElevenLabs API Key（可选）
	httpClient       *http.Client
	localLib         map[string]string // 内置标签 → 文件名（不含目录）
	localUploadCache sync.Map          // local file path → OSS URL（进程内缓存）
	queryCache       sync.Map          // "freesound:query" → sfxCacheEntry
}

// WithSFXItemRepo 注入音效条目仓库（可选；注入后才启用多 item 存储）
func (s *SFXService) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *SFXService {
	s.sfxItemRepo = r
	return s
}

// SFXServiceConfig SFXService 构造参数
type SFXServiceConfig struct {
	SFXDir          string // 本地音效目录（环境变量 SFX_DIR）
	FreesoundKey    string // 环境变量 FREESOUND_API_KEY
	PixabayKey      string // 保留字段（Pixabay/Jamendo 均为音乐平台，SFX 不使用）
	JamendoClientID string // 保留字段（Jamendo 为音乐平台，SFX 不使用）
	ElevenLabsKey   string // 环境变量 ELEVENLABS_API_KEY
}

// NewSFXService 创建 SFX 服务实例
func NewSFXService(
	aiSvc *AIService,
	storageSvc storage.Service,
	storyboardRepo *repository.StoryboardRepository,
	cfg SFXServiceConfig,
) *SFXService {
	return &SFXService{
		aiSvc:          aiSvc,
		storageSvc:     storageSvc,
		storyboardRepo: storyboardRepo,
		sfxDir:         cfg.SFXDir,
		freesoundKey:   cfg.FreesoundKey,
		elevenKey:      cfg.ElevenLabsKey,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		localLib:        buildDefaultSFXLib(),
	}
}

// buildDefaultSFXLib 内置音效标签 → 文件名映射表。
// 文件名相对于 SFXDir，不含目录。
// 用户只需把对应 WAV/MP3 放入 SFXDir 即可生效。
func buildDefaultSFXLib() map[string]string {
	return map[string]string{
		// 环境音 — 自然
		"rain_heavy":     "rain_heavy.wav",
		"rain_light":     "rain_light.wav",
		"wind_night":     "wind_night.wav",
		"wind_strong":    "wind_strong.wav",
		"thunder":        "thunder.wav",
		"forest_ambient": "forest_ambient.wav",
		"river_flowing":  "river_flowing.wav",
		"fire_crackle":   "fire_crackle.wav",
		// 环境音 — 室内/城市
		"crowd_outdoor":  "crowd_outdoor.wav",
		"crowd_indoor":   "crowd_indoor.wav",
		"crowd_murmur":   "crowd_murmur.wav",
		"city_ambient":   "city_ambient.wav",
		"ambient_room":   "ambient_room.wav",
		// 动作音 — 武侠/战斗
		"sword_clash":       "sword_clash.wav",
		"sword_draw":        "sword_draw.wav",
		"arrow_whoosh":      "arrow_whoosh.wav",
		"explosion":         "explosion.wav",
		"punch_impact":      "punch_impact.wav",
		"horse_gallop":      "horse_gallop.wav",
		// 动作音 — 日常
		"footsteps_stone":   "footsteps_stone.wav",
		"footsteps_running": "footsteps_running.wav",
		"footsteps_wood":    "footsteps_wood.wav",
		"door_open":         "door_open.wav",
		"door_knock":        "door_knock.wav",
		"bell_ring":         "bell_ring.wav",
		// 情绪音
		"heartbeat":     "heartbeat.wav",
		"clock_ticking": "clock_ticking.wav",
	}
}

// sfxShotBrief 发给 AI 进行音效分析的精简分镜信息
type sfxShotBrief struct {
	ShotID        uint    `json:"shot_id"`
	ShotNo        int     `json:"shot_no"`
	Description   string  `json:"description"`
	Narration     string  `json:"narration,omitempty"`
	Dialogue      string  `json:"dialogue,omitempty"`
	EmotionalTone string  `json:"emotional_tone,omitempty"`
	Duration      float64 `json:"duration"`
}

// analyzeSingleShotSFX 为单个分镜调用 AI 生成自然语言音效搜索词，更新 sfx_tags 字段。
// 参考7类音效框架引导 AI 选词，输出平铺 JSON 数组；无需音效时输出空数组。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string) error {
	prompt := `你是专业影视音效设计师，为分镜场景设计精准音效搜索词。

请根据分镜信息，从以下音效类型中选择适合该场景的词组（共输出 0-5 条英文搜索词）：

音效类型参考（按需选择，不限类别数量）：
• 环境音效：风声、雨声、鸟鸣、虫鸣、车流、人群、空调白噪音、咖啡厅背景声等
• 动作音效：脚步声、开关门、物体碰撞摔落、剑鸣刀击、马蹄、衣物摩擦等
• UI/界面音效：按钮点击、提示音、翻页、错误/成功提示（科技/游戏场景）
• 特效音效：爆炸枪声、Whoosh移动感、Boom冲击感、Riser过渡音、慢动作拉伸音
• 情绪音效：喜剧滑稽音、心跳悬疑音、恐怖诡异噪音、胜利/失败音效
• 人声音效：笑声、掌声、欢呼、人群反应、耳语回声（不含角色对白）
• 转场音效：Swoosh扫过、咔哒声、电影胶片转动、时钟滴答

【输出规则】
- 仅输出 JSON 字符串数组，每条为2-6个英文单词的可搜索词组
- 纯静止镜头或纯对话镜头不需要音效时，输出空数组 []
- 示例：["heavy rain tile roof ambient", "wooden door creak open", "sword clash metal impact"]

分镜信息：
`
	if shot.ShotNo > 0 {
		prompt += fmt.Sprintf("镜头编号：%d\n", shot.ShotNo)
	}
	if shot.Description != "" {
		prompt += fmt.Sprintf("画面描述：%s\n", shot.Description)
	}
	if shot.EmotionalTone != "" {
		prompt += fmt.Sprintf("情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Narration != "" {
		prompt += fmt.Sprintf("旁白：%s\n", shot.Narration)
	}
	if shot.Dialogue != "" {
		prompt += fmt.Sprintf("台词（仅参考场景环境，无需为台词本身设计音效）：%s\n", shot.Dialogue)
	}
	if userContext != "" {
		prompt += "\n额外场景背景（优先参考）：\n" + userContext
	}

	result, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_analyze", prompt, "")
	if err != nil {
		return fmt.Errorf("AI call: %w", err)
	}

	raw := extractJSON(result)
	var queries []string
	if err := json.Unmarshal([]byte(raw), &queries); err != nil {
		return fmt.Errorf("parse JSON: %w (raw=%q)", err, raw)
	}
	if len(queries) == 0 {
		shot.SFXTags = ""
		_ = s.storyboardRepo.UpdateSFXTags(shot.ID, "")
		return nil
	}

	tagsJSON, _ := json.Marshal(queries)
	shot.SFXTags = string(tagsJSON)
	if err := s.storyboardRepo.UpdateSFXTags(shot.ID, string(tagsJSON)); err != nil {
		return fmt.Errorf("update sfx_tags: %w", err)
	}
	return nil
}

// AnalyzeSFXForVideo 并行为每个分镜单独调用 AI 生成自然语言音效搜索词，写入 sfx_tags 字段。
// 每个分镜独立分析，并发度最多 5，单个失败不影响其余镜头。
func (s *SFXService) AnalyzeSFXForVideo(ctx context.Context, shots []*model.StoryboardShot, tenantID uint, userContext string) error {
	if len(shots) == 0 {
		return nil
	}
	logger.Printf("[SFXService] AnalyzeSFXForVideo: parallel analysis for %d shots", len(shots))

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var updated, failed atomic.Int32

	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sh *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.analyzeSingleShotSFX(ctx, sh, tenantID, userContext)
			if err != nil {
				logger.Printf("[SFXService] AnalyzeSFXForVideo: shot %d failed: %v", sh.ShotNo, err)
				failed.Add(1)
			} else {
				updated.Add(1)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("[SFXService] AnalyzeSFXForVideo: updated=%d failed=%d", updated.Load(), failed.Load())
	return nil
}

// AutoGenerateSFX 为单个镜头自动选取/生成音效，每个 tag 独立搜索，写入多条 ShotSFXItem。
// 每次调用都会先清除旧音效条目再写入新结果，确保重新生成时能替换旧音效。
func (s *SFXService) AutoGenerateSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint) error {
	// 清除旧音效条目（先删后建，保证重新生成时能替换）
	if s.sfxItemRepo != nil {
		if err := s.sfxItemRepo.DeleteByShotID(shot.ID); err != nil {
			logger.Printf("[SFXService] shot %d: clear old sfx items failed: %v", shot.ID, err)
		}
	}

	// 1. 提取搜索词（优先用已有 sfx_tags；否则调用 AI 分析）
	var tags []string
	if shot.SFXTags != "" {
		if err := json.Unmarshal([]byte(shot.SFXTags), &tags); err != nil || len(tags) == 0 {
			tags = nil
		}
	}
	if len(tags) == 0 {
		// 调用统一的 AI 分析函数（与批量分析路径一致，避免两套 Prompt）
		if err := s.analyzeSingleShotSFX(ctx, shot, tenantID, ""); err != nil {
			logger.Printf("[SFXService] shot %d AI analyze failed (%v), using rule fallback", shot.ID, err)
			tags = s.fallbackTags(shot)
		} else if shot.SFXTags != "" {
			_ = json.Unmarshal([]byte(shot.SFXTags), &tags)
		}
	}
	if len(tags) == 0 {
		tags = s.fallbackTags(shot)
	}
	// 最多取前 5 个 tag，避免过多网络请求
	if len(tags) > 5 {
		tags = tags[:5]
	}

	tagsJSON, _ := json.Marshal(tags)
	logger.Printf("[SFXService] shot %d tags=%s", shot.ID, tagsJSON)

	maxDur := float64(shot.Duration)
	if maxDur <= 0 {
		maxDur = 0
	}

	// 基础音量：台词场景压低，有旁白略降
	baseVol := 0.4
	if shot.Dialogue != "" {
		baseVol = 0.2
	} else if shot.AudioPath != "" {
		baseVol = 0.3
	}

	// 2. 逐 tag 搜索，收集结果
	type sfxResult struct {
		tag    string
		url    string
		source string
		vol    float64
	}
	var results []sfxResult

	for _, tag := range tags {
		u, src := s.searchOneTag(ctx, tenantID, tag, maxDur, shot)
		if u != "" {
			// 音量按音效类型决定，再根据台词场景进一步压低
			vol := sfxCategoryVolume(tag) * (baseVol / 0.4)
			if vol < 0.1 {
				vol = 0.1
			}
			results = append(results, sfxResult{tag: tag, url: u, source: src, vol: vol})
			logger.Printf("[SFXService] shot %d tag=%q source=%s url=%s vol=%.2f", shot.ID, tag, src, u, vol)
		} else {
			logger.Printf("[SFXService] shot %d tag=%q: no result", shot.ID, tag)
		}
	}

	if len(results) == 0 {
		return fmt.Errorf("no SFX found for shot %d (tags: %s)", shot.ID, tagsJSON)
	}

	// 3. 写入数据库
	if s.sfxItemRepo != nil {
		items := make([]*model.ShotSFXItem, 0, len(results))
		for i, r := range results {
			items = append(items, &model.ShotSFXItem{
				ShotID: shot.ID,
				SeqNo:  i + 1,
				Tag:    r.tag,
				URL:    r.url,
				Volume: r.vol,
				Source: r.source,
			})
		}
		if err := s.sfxItemRepo.BatchCreate(items); err != nil {
			return fmt.Errorf("save sfx items shot %d: %w", shot.ID, err)
		}
		// 同步更新旧字段（向后兼容时间线播放）
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].url, string(tagsJSON), results[0].vol)
	} else {
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].url, string(tagsJSON), baseVol)
	}
	return nil
}

// searchOneTag 对单个 tag 执行三层降级搜索，返回 (url, source)。
// 顺序：本地库 → Freesound（CC0 专业音效库） → ElevenLabs（AI生成）
// Jamendo 为音乐平台，返回完整音乐曲目而非单次音效，不适合 SFX 场景。
func (s *SFXService) searchOneTag(ctx context.Context, tenantID uint, tag string, maxDur float64, shot *model.StoryboardShot) (string, string) {
	if u := s.searchLocalLib(ctx, tenantID, tag); u != "" {
		logger.Printf("[SFXService] shot %d local hit: %s", shot.ID, u)
		return u, "local"
	}
	if s.freesoundKey == "" {
		logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
	} else if u := s.searchFreesound(ctx, tag, maxDur); u != "" {
		logger.Printf("[SFXService] shot %d Freesound hit: %s", shot.ID, u)
		return u, "freesound"
	} else {
		logger.Printf("[SFXService] shot %d Freesound miss for %q", shot.ID, tag)
	}
	if u, err := s.generateElevenLabs(ctx, shot); err == nil && u != "" {
		logger.Printf("[SFXService] shot %d ElevenLabs hit: %s", shot.ID, u)
		return u, "elevenlabs"
	} else if err != nil {
		logger.Printf("[SFXService] shot %d ElevenLabs failed: %v", shot.ID, err)
	}
	return "", ""
}

// BatchAutoGenerateSFX 批量处理视频所有镜头，最多 5 并发。
// progressFn 每完成一个镜头时实时调用（0-100）。
// 返回成功/失败数量，不因单个失败而中止整批。
func (s *SFXService) BatchAutoGenerateSFX(
	ctx context.Context,
	shots []*model.StoryboardShot,
	tenantID uint,
	userContext string,
	progressFn func(int),
) (success, fail int) {
	total := len(shots)
	if total == 0 {
		return
	}
	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var doneCount atomic.Int32
	var successCount, failCount atomic.Int32

	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(s2 *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.AutoGenerateSFX(ctx, s2, tenantID)
			if err != nil {
				logger.Printf("[SFXService] shot %d: %v", s2.ID, err)
				failCount.Add(1)
			} else {
				successCount.Add(1)
			}
			// progressFn 在 goroutine 内实时调用，而非事后批量触发
			n := int(doneCount.Add(1))
			if progressFn != nil {
				progressFn(n * 100 / total)
			}
		}(shot)
	}
	wg.Wait()
	return int(successCount.Load()), int(failCount.Load())
}

// --- 内部方法 ---

// fallbackTags 基于规则从描述 / 情绪基调 / 镜头类型推断标签（LLM 不可用时的降级）
func (s *SFXService) fallbackTags(shot *model.StoryboardShot) []string {
	desc := strings.ToLower(shot.Description + " " + shot.EmotionalTone + " " + shot.Scene)
	rules := [][2]string{
		{"雨", "rain heavy ambient"}, {"雪", "wind cold night"}, {"风", "wind strong"},
		{"雷", "thunder storm"}, {"森林", "forest ambient birds"}, {"河", "river flowing water"},
		{"城市", "city street ambient"}, {"人群", "crowd outdoor noise"}, {"室内", "room interior ambient"},
		{"战斗", "sword clash metal impact"}, {"奔跑", "footsteps running fast"},
		{"马", "horse gallop hooves"}, {"爆炸", "explosion blast impact"}, {"箭", "arrow whoosh flight"},
		{"门", "door open creak"}, {"火", "fire crackle burning"},
		{"紧张", "heartbeat suspense"}, {"钟", "clock ticking"}, {"铃", "bell ring"},
	}
	seen := map[string]bool{}
	var tags []string
	for _, r := range rules {
		if strings.Contains(desc, r[0]) && !seen[r[1]] {
			tags = append(tags, r[1])
			seen[r[1]] = true
		}
	}
	if len(tags) == 0 {
		tags = []string{"room interior ambient"}
	}
	if len(tags) > 5 {
		tags = tags[:5]
	}
	return tags
}

// normalizeTag 将标签统一为小写下划线格式（兼容空格和连字符）
func normalizeTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	tag = strings.ReplaceAll(tag, " ", "_")
	tag = strings.ReplaceAll(tag, "-", "_")
	return tag
}

// matchLocalLibKey 尝试将英文短语匹配到 localLib 键，返回对应文件名。
// 三级匹配：1) 精确标准化匹配  2) 键的所有词均出现在短语中  3) 键的主词（>3字符）出现在短语中
func matchLocalLibKey(lib map[string]string, phrase string) (string, bool) {
	phraseWords := strings.Fields(strings.ToLower(phrase))
	phraseSet := make(map[string]bool, len(phraseWords))
	for _, w := range phraseWords {
		phraseSet[w] = true
	}

	// 1. 精确标准化匹配（针对 underscore_tag 输入）
	normalized := strings.Join(phraseWords, "_")
	if filename, ok := lib[normalized]; ok {
		return filename, true
	}

	// 2. 键的所有词均出现在短语中（如 "rain_heavy" ↔ "heavy rain tile roof"）
	for libKey, filename := range lib {
		keyWords := strings.Split(libKey, "_")
		allMatch := true
		for _, kw := range keyWords {
			if !phraseSet[kw] {
				allMatch = false
				break
			}
		}
		if allMatch {
			return filename, true
		}
	}

	// 3. 键的主词（第一个且长度 > 3 的词）出现在短语中（弱匹配）
	for libKey, filename := range lib {
		keyWords := strings.Split(libKey, "_")
		for _, kw := range keyWords {
			if len(kw) > 3 && phraseSet[kw] {
				return filename, true
			}
		}
	}
	return "", false
}

// searchLocalLib 在本地目录中查找首个匹配短语的音效文件。
// 找到后自动上传至 OSS（首次），返回可公开访问的 URL。
// file:// 协议 URL 无法在浏览器端访问，因此必须通过存储服务转换。
func (s *SFXService) searchLocalLib(ctx context.Context, tenantID uint, phrase string) string {
	if s.sfxDir == "" {
		return ""
	}
	filename, ok := matchLocalLibKey(s.localLib, phrase)
	if !ok {
		return ""
	}
	localPath := filepath.Join(s.sfxDir, filename)
	if _, err := os.Stat(localPath); err != nil {
		return ""
	}

	// 命中进程内缓存
	if cached, ok := s.localUploadCache.Load(localPath); ok {
		return cached.(string)
	}

	// 上传至 OSS（首次使用时）
	if s.storageSvc != nil {
		f, err := os.Open(localPath)
		if err == nil {
			defer f.Close()
			fi, _ := f.Stat()
			ossKey := fmt.Sprintf("sfx/local/%s", filepath.Base(localPath))
			ext := strings.ToLower(filepath.Ext(localPath))
			mime := "audio/wav"
			if ext == ".mp3" {
				mime = "audio/mpeg"
			}
			if u, err := s.storageSvc.Upload(ctx, ossKey, f, fi.Size(), mime); err == nil {
				s.localUploadCache.Store(localPath, u)
				return u
			} else {
				logger.Printf("[SFXService] local OSS upload failed (%s): %v", filename, err)
			}
		}
	}
	// storageSvc 未配置或上传失败：跳过本地文件，继续搜索外部 API
	return ""
}

// sfxCategoryVolume 根据音效类型返回建议混音音量（0.1–0.6）。
// 冲击音效音量较高，环境音效较低，避免掩盖人声。
func sfxCategoryVolume(tag string) float64 {
	lower := strings.ToLower(tag)
	// 冲击类：爆炸、打击、碰撞 → 较高音量
	for _, kw := range []string{"explosion", "blast", "impact", "clash", "punch", "crash", "bang", "boom", "thunder"} {
		if strings.Contains(lower, kw) {
			return 0.55
		}
	}
	// 动作类：脚步、门、武器 → 中等音量
	for _, kw := range []string{"footstep", "door", "sword", "arrow", "whoosh", "gallop", "swing", "click"} {
		if strings.Contains(lower, kw) {
			return 0.45
		}
	}
	// 环境类：自然音、人群 → 较低音量（避免掩盖旁白）
	for _, kw := range []string{"rain", "wind", "forest", "ambient", "crowd", "city", "river", "fire", "room"} {
		if strings.Contains(lower, kw) {
			return 0.3
		}
	}
	// 情绪/转场类：心跳、时钟 → 低音量
	for _, kw := range []string{"heartbeat", "clock", "tick", "breath"} {
		if strings.Contains(lower, kw) {
			return 0.25
		}
	}
	return 0.4 // 默认
}

// cachedQuery 用进程内缓存包装一次 API 搜索，TTL = sfxCacheTTL。
// cacheKey 格式建议："source:query"。
func (s *SFXService) cachedQuery(cacheKey string, fn func() (string, string)) (string, string) {
	if v, ok := s.queryCache.Load(cacheKey); ok {
		entry := v.(sfxCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.url, entry.source
		}
		s.queryCache.Delete(cacheKey)
	}
	u, src := fn()
	if u != "" {
		s.queryCache.Store(cacheKey, sfxCacheEntry{url: u, source: src, expiresAt: time.Now().Add(sfxCacheTTL)})
	}
	return u, src
}

// freesoundSearch 执行单次 Freesound API 搜索，返回首个结果的预览 MP3 URL。
func (s *SFXService) freesoundSearch(ctx context.Context, query string, maxDuration float64) string {
	filter := `license:"Creative Commons 0"`
	if maxDuration > 0 {
		filter += fmt.Sprintf(" duration:[0.5 TO %.1f]", maxDuration)
	}
	apiURL := fmt.Sprintf(
		"https://freesound.org/apiv2/search/text/?query=%s&filter=%s&fields=id,name,previews&sort=downloads_desc&page_size=1&token=%s",
		url.QueryEscape(query), url.QueryEscape(filter), s.freesoundKey,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return ""
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		logger.Printf("[SFXService] Freesound request error for %q: %v", query, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Printf("[SFXService] Freesound HTTP %d for %q: %s", resp.StatusCode, query, body)
		return ""
	}

	var result struct {
		Results []struct {
			Previews struct {
				PreviewHQMP3 string `json:"preview-hq-mp3"`
			} `json:"previews"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
		return ""
	}
	return result.Results[0].Previews.PreviewHQMP3
}

// searchFreesound 通过 Freesound API 搜索 CC0 授权音效。
// 先用完整短语搜索，失败则拆词降级重试。结果缓存 sfxCacheTTL。
func (s *SFXService) searchFreesound(ctx context.Context, phrase string, maxDuration float64) string {
	if s.freesoundKey == "" || phrase == "" {
		return ""
	}
	// 完整短语搜索
	query := strings.ReplaceAll(normalizeTag(phrase), "_", " ")
	cacheKey := "freesound:" + query
	u, _ := s.cachedQuery(cacheKey, func() (string, string) {
		return s.freesoundSearch(ctx, query, maxDuration), "freesound"
	})
	if u != "" {
		return u
	}
	// 降级：拆分关键词，逐词搜索（取前 3 个有意义的词）
	words := strings.Fields(query)
	for i, w := range words {
		if i >= 3 {
			break
		}
		if len(w) <= 3 {
			continue
		}
		ck := "freesound:" + w
		wu, _ := s.cachedQuery(ck, func() (string, string) {
			return s.freesoundSearch(ctx, w, maxDuration), "freesound"
		})
		if wu != "" {
			return wu
		}
	}
	return ""
}

// elevenLabsPrompt 将 sfx_tags 转换为适合 ElevenLabs 音效生成的英文 Prompt。
func elevenLabsPrompt(shot *model.StoryboardShot) string {
	if shot.SFXTags != "" {
		var tags []string
		if json.Unmarshal([]byte(shot.SFXTags), &tags) == nil && len(tags) > 0 {
			parts := make([]string, 0, len(tags))
			for _, t := range tags {
				parts = append(parts, strings.ReplaceAll(normalizeTag(t), "_", " "))
			}
			prompt := "Sound effects: " + strings.Join(parts, ", ")
			if shot.EmotionalTone != "" {
				prompt += ". Mood: " + shot.EmotionalTone
			}
			return prompt
		}
	}
	runes := []rune(shot.Description)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return shot.Description
}

// generateElevenLabs 调用 ElevenLabs Sound Generation API，生成定制音效并上传至存储服务。
func (s *SFXService) generateElevenLabs(ctx context.Context, shot *model.StoryboardShot) (string, error) {
	if s.elevenKey == "" {
		return "", fmt.Errorf("elevenlabs key not configured")
	}
	if s.storageSvc == nil {
		return "", fmt.Errorf("storage not configured for elevenlabs upload")
	}

	prompt := elevenLabsPrompt(shot)
	runes := []rune(prompt)
	if len(runes) > 200 {
		prompt = string(runes[:200])
	}

	dur := shot.Duration
	if dur <= 0 {
		dur = 5
	}
	if dur > 22 {
		dur = 22
	}

	body, _ := json.Marshal(map[string]interface{}{
		"text":             prompt,
		"duration_seconds": dur,
		"prompt_influence": 0.3,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.elevenlabs.io/v1/sound-generation", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", s.elevenKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("elevenlabs HTTP %d: %s", resp.StatusCode, bodyBytes)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	key := fmt.Sprintf("sfx/video_%d/shot_%d.mp3", shot.VideoID, shot.ID)
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}
