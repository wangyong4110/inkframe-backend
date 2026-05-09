package service

import (
	"bytes"
	"context"
	"encoding/binary"
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

// sfxHit holds the result of a single SFX search.
type sfxHit struct {
	url          string
	source       string
	durationSecs float64 // 音效时长（秒）；0 = 未知
}

// sfxCacheEntry caches API search results to avoid duplicate requests for the same query.
type sfxCacheEntry struct {
	url          string
	source       string
	durationSecs float64
	expiresAt    time.Time
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
// 使用专业音效设计框架引导 AI：分层设计（动作音→环境底层→情绪点缀），
// 并遵循 Freesound 实际有效的 [物体+材质+动作+音色描述符] 四元格式。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string) error {
	hasSpeech := shot.Dialogue != "" || shot.Narration != ""

	var sceneCtx strings.Builder
	if shot.ShotNo > 0 {
		fmt.Fprintf(&sceneCtx, "镜头编号：%d\n", shot.ShotNo)
	}
	if shot.Description != "" {
		fmt.Fprintf(&sceneCtx, "画面描述：%s\n", shot.Description)
	}
	if shot.EmotionalTone != "" {
		fmt.Fprintf(&sceneCtx, "情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Dialogue != "" {
		fmt.Fprintf(&sceneCtx, "台词（参考场景环境，不为对白本身设计音效）：%s\n", shot.Dialogue)
	}
	if shot.Narration != "" {
		fmt.Fprintf(&sceneCtx, "旁白：%s\n", shot.Narration)
	}
	if userContext != "" {
		fmt.Fprintf(&sceneCtx, "\n额外场景背景（优先参考）：\n%s\n", userContext)
	}

	speechGuide := ""
	if hasSpeech {
		speechGuide = "\n⚠️ 本镜头含台词/旁白：只输出 1 条 subtle 环境底层音，禁止动作音/冲击音，避免掩盖人声。\n"
	}

	prompt := `你是有10年经验的专业影视音效设计师，精通 Freesound、SoundSnap 等专业音效库的搜索逻辑。

## 音效分层原则（按优先级顺序）
1. **动作音**（单次触发型）：画面中具体发生的物理动作 → 与画面强同步，最重要
2. **环境底层**（循环持续型）：场景持续存在的背景声，建立空间感与沉浸感
3. **情绪点缀**（单次触发型）：场景转折/情感强调时的冲击音、rise音或 sub-bass

## Freesound 有效搜索词格式
**四元格式**：[物体/来源] [材质/空间] [动作类型] [音色描述符]

音色描述符词库（根据场景选用）：
- 触发类型：single / one-shot / short / burst
- 持续类型：loop / continuous / sustained
- 空间感：indoor / outdoor / reverb / dry / echo / distant / close-up
- 质感：heavy / light / sharp / soft / subtle / muffled / crisp

✅ 高命中率示例（请模仿此精度）：
- "wooden footsteps stone corridor reverb" — 脚步指定材质+空间+音色
- "metal sword unsheath dry single sharp" — 武器动作指定材质+类型+质感
- "heavy rain outdoor rooftop loop" — 雨声指定强度+位置+类型
- "fire wood crackle burning close loop" — 火声指定材料+音色+距离
- "crowd cheer outdoor distant reverb" — 人群指定类型+距离+空间
- "thunder rumble distant outdoor single" — 雷声指定质感+距离+类型
- "spiritual energy crackle electric whoosh" — 玄幻场景的灵气/能量音效
- "magic spell cast energy swoosh single" — 施法/释放技能音效
- "qi explosion impact cinematic boom" — 内力/真气爆发冲击音

❌ 低命中率（避免）：
- 单词：sword / rain / fire / ambient（太笼统）
- 描述句：sword fighting sound / background rain noise（Freesound 不支持）
` + speechGuide + `
## 输出规则
- 输出 0~3 条搜索词（按优先级降序）
- 每条 3~7 个英文单词
- 纯情感特写/空镜：最多 1 条 subtle 环境音
- 仅输出 JSON 字符串数组，禁止输出任何其他内容

## 分镜信息
` + sceneCtx.String()

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
		tag string
		hit sfxHit
		vol float64
	}
	var results []sfxResult

	for _, tag := range tags {
		hit := s.searchOneTag(ctx, tenantID, tag, maxDur, shot)
		if hit.url != "" {
			// 音量按音效类型决定，再根据台词场景进一步压低
			vol := sfxCategoryVolume(tag) * (baseVol / 0.4)
			if vol < 0.1 {
				vol = 0.1
			}
			results = append(results, sfxResult{tag: tag, hit: hit, vol: vol})
			logger.Printf("[SFXService] shot %d tag=%q source=%s url=%s dur=%.1fs vol=%.2f",
				shot.ID, tag, hit.source, hit.url, hit.durationSecs, vol)
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
				ShotID:       shot.ID,
				SeqNo:        i + 1,
				Tag:          r.tag,
				URL:          r.hit.url,
				Volume:       r.vol,
				Source:       r.hit.source,
				DurationSecs: r.hit.durationSecs, // 精确音效时长（来自 API 或文件头解析）
				// StartOffset: 0 默认（AI 生成时均从分镜起始播放）
			})
		}
		if err := s.sfxItemRepo.BatchCreate(items); err != nil {
			return fmt.Errorf("save sfx items shot %d: %w", shot.ID, err)
		}
		// 同步更新旧字段（向后兼容时间线播放）
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].hit.url, string(tagsJSON), results[0].vol)
	} else {
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].hit.url, string(tagsJSON), baseVol)
	}
	return nil
}

// searchOneTag 对单个 tag 执行三层降级搜索，返回 sfxHit（url/source/durationSecs）。
// 顺序：本地库 → Freesound（CC0 专业音效库） → ElevenLabs（AI生成）
// Jamendo 为音乐平台，返回完整音乐曲目而非单次音效，不适合 SFX 场景。
func (s *SFXService) searchOneTag(ctx context.Context, tenantID uint, tag string, maxDur float64, shot *model.StoryboardShot) sfxHit {
	if u, dur := s.searchLocalLib(ctx, tenantID, tag); u != "" {
		logger.Printf("[SFXService] shot %d local hit: %s (%.1fs)", shot.ID, u, dur)
		return sfxHit{url: u, source: "local", durationSecs: dur}
	}
	if s.freesoundKey == "" {
		logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
	} else if hit := s.searchFreesound(ctx, tag, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Freesound hit: %s (%.1fs)", shot.ID, hit.url, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d Freesound miss for %q", shot.ID, tag)
	}
	if u, dur, err := s.generateElevenLabs(ctx, shot); err == nil && u != "" {
		logger.Printf("[SFXService] shot %d ElevenLabs hit: %s (%.1fs)", shot.ID, u, dur)
		return sfxHit{url: u, source: "elevenlabs", durationSecs: dur}
	} else if err != nil {
		logger.Printf("[SFXService] shot %d ElevenLabs failed: %v", shot.ID, err)
	}
	return sfxHit{}
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

// fallbackTags 基于规则从描述 / 情绪基调 / 镜头类型推断标签（LLM 不可用时的降级）。
// 遵循 Freesound 四元格式：[物体] [材质/空间] [动作] [音色描述符]
func (s *SFXService) fallbackTags(shot *model.StoryboardShot) []string {
	desc := strings.ToLower(shot.Description + " " + shot.EmotionalTone + " " + shot.Scene + " " + shot.Narration)
	// [中文关键词] → [Freesound 有效搜索词]
	rules := [][2]string{
		// 天气/自然环境
		{"大雨", "heavy rain outdoor rooftop loop"},
		{"小雨", "light rain window glass indoor loop"},
		{"雨", "rain outdoor ambient loop"},
		{"雪", "blizzard wind cold outdoor loop"},
		{"风", "wind outdoor howling loop"},
		{"雷", "thunder rumble distant outdoor single"},
		{"闪电", "thunder lightning crack sharp single"},
		{"森林", "forest birds morning ambient outdoor loop"},
		{"鸟", "birds chirping outdoor morning ambient"},
		{"虫鸣", "crickets insects night outdoor loop"},
		{"河", "river stream flowing water outdoor loop"},
		{"海", "ocean waves beach outdoor loop"},
		{"火", "fire wood crackle burning close loop"},
		// 城市/室内环境
		{"城市", "city street traffic ambient outdoor loop"},
		{"集市", "crowd market outdoor bustling ambient"},
		{"人群", "crowd outdoor distant reverb ambient"},
		{"室内", "room indoor ambience subtle loop"},
		{"酒馆", "tavern crowd indoor ambient murmur"},
		{"宫殿", "palace hall reverb footsteps stone"},
		// 战斗/武侠/玄幻动作
		{"战斗", "sword metal clash impact dry single"},
		{"拔剑", "metal sword unsheath sharp single"},
		{"剑", "sword slash whoosh sharp single"},
		{"刀", "blade metal swing whoosh single"},
		{"弓箭", "arrow whoosh release outdoor single"},
		{"拳", "punch impact thud dry single"},
		{"爆炸", "explosion blast impact outdoor single"},
		{"真气", "qi energy crackle electric whoosh single"},
		{"灵气", "spiritual energy hum ethereal loop"},
		{"施法", "magic spell cast swoosh energy single"},
		{"突破", "power surge energy burst impact single"},
		{"马", "horse gallop hooves dirt outdoor"},
		// 日常动作
		{"奔跑", "footsteps running stone floor indoor"},
		{"脚步", "footsteps walking stone corridor reverb"},
		{"走", "footsteps slow walk indoor"},
		{"开门", "wooden door open creak single"},
		{"关门", "door close slam wood single"},
		{"门", "wooden door creak open indoor"},
		// 情绪/氛围
		{"紧张", "heartbeat pulse tense close-up single"},
		{"恐惧", "heartbeat fast tense horror single"},
		{"悬疑", "suspense stinger short single"},
		{"钟", "clock ticking mechanical indoor loop"},
		{"铃", "bell ring resonant single"},
		{"笑声", "crowd laugh indoor distant"},
		{"掌声", "applause crowd indoor"},
	}
	seen := map[string]bool{}
	var tags []string
	for _, r := range rules {
		if strings.Contains(desc, r[0]) && !seen[r[1]] {
			tags = append(tags, r[1])
			seen[r[1]] = true
			if len(tags) == 3 {
				break
			}
		}
	}
	if len(tags) == 0 {
		tags = []string{"room indoor ambience subtle loop"}
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

// parseWAVDuration 读取 WAV 文件的 RIFF 头，返回音频时长（秒）。
// 不支持的格式或读取失败时返回 0。
func parseWAVDuration(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	// RIFF header: 4 (RIFF) + 4 (size) + 4 (WAVE) = 12 bytes
	var riffID [4]byte
	var riffSize uint32
	var waveID [4]byte
	if binary.Read(f, binary.LittleEndian, &riffID) != nil ||
		binary.Read(f, binary.LittleEndian, &riffSize) != nil ||
		binary.Read(f, binary.LittleEndian, &waveID) != nil {
		return 0
	}
	if string(riffID[:]) != "RIFF" || string(waveID[:]) != "WAVE" {
		return 0
	}

	var byteRate uint32
	var dataSize uint32
	for {
		var chunkID [4]byte
		var chunkSize uint32
		if binary.Read(f, binary.LittleEndian, &chunkID) != nil ||
			binary.Read(f, binary.LittleEndian, &chunkSize) != nil {
			break
		}
		switch string(chunkID[:]) {
		case "fmt ":
			if chunkSize < 16 {
				return 0
			}
			var audioFmt, channels uint16
			var sampleRate, bRate uint32
			var blockAlign, bitsPerSample uint16
			binary.Read(f, binary.LittleEndian, &audioFmt)
			binary.Read(f, binary.LittleEndian, &channels)
			binary.Read(f, binary.LittleEndian, &sampleRate)
			binary.Read(f, binary.LittleEndian, &bRate)
			binary.Read(f, binary.LittleEndian, &blockAlign)
			binary.Read(f, binary.LittleEndian, &bitsPerSample)
			byteRate = bRate
			if remaining := int64(chunkSize) - 16; remaining > 0 {
				f.Seek(remaining, io.SeekCurrent)
			}
		case "data":
			dataSize = chunkSize
		default:
			// Skip unknown chunk (WAV requires even-byte alignment)
			skip := int64(chunkSize)
			if chunkSize%2 != 0 {
				skip++
			}
			f.Seek(skip, io.SeekCurrent)
		}
		if dataSize > 0 && byteRate > 0 {
			break
		}
	}
	if byteRate == 0 || dataSize == 0 {
		return 0
	}
	return float64(dataSize) / float64(byteRate)
}

// searchLocalLib 在本地目录中查找首个匹配短语的音效文件。
// 找到后自动上传至 OSS（首次），返回可公开访问的 URL 和音效时长（秒）。
// file:// 协议 URL 无法在浏览器端访问，因此必须通过存储服务转换。
func (s *SFXService) searchLocalLib(ctx context.Context, tenantID uint, phrase string) (string, float64) {
	if s.sfxDir == "" {
		return "", 0
	}
	filename, ok := matchLocalLibKey(s.localLib, phrase)
	if !ok {
		return "", 0
	}
	localPath := filepath.Join(s.sfxDir, filename)
	if _, err := os.Stat(localPath); err != nil {
		return "", 0
	}

	// 解析本地 WAV 时长
	dur := parseWAVDuration(localPath)

	// 命中进程内缓存
	if cached, ok := s.localUploadCache.Load(localPath); ok {
		return cached.(string), dur
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
				return u, dur
			} else {
				logger.Printf("[SFXService] local OSS upload failed (%s): %v", filename, err)
			}
		}
	}
	// storageSvc 未配置或上传失败：跳过本地文件，继续搜索外部 API
	return "", 0
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
func (s *SFXService) cachedQuery(cacheKey string, fn func() sfxHit) sfxHit {
	if v, ok := s.queryCache.Load(cacheKey); ok {
		entry := v.(sfxCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return sfxHit{url: entry.url, source: entry.source, durationSecs: entry.durationSecs}
		}
		s.queryCache.Delete(cacheKey)
	}
	hit := fn()
	if hit.url != "" {
		s.queryCache.Store(cacheKey, sfxCacheEntry{
			url: hit.url, source: hit.source, durationSecs: hit.durationSecs,
			expiresAt: time.Now().Add(sfxCacheTTL),
		})
	}
	return hit
}

// freesoundSearch 执行单次 Freesound API 搜索，返回首个结果的预览 MP3 URL 和时长（秒）。
func (s *SFXService) freesoundSearch(ctx context.Context, query string, maxDuration float64) (string, float64) {
	filter := `license:"Creative Commons 0"`
	if maxDuration > 0 {
		filter += fmt.Sprintf(" duration:[0.5 TO %.1f]", maxDuration)
	}
	apiURL := fmt.Sprintf(
		"https://freesound.org/apiv2/search/text/?query=%s&filter=%s&fields=id,name,previews,duration&sort=downloads_desc&page_size=1&token=%s",
		url.QueryEscape(query), url.QueryEscape(filter), s.freesoundKey,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", 0
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		logger.Printf("[SFXService] Freesound request error for %q: %v", query, err)
		return "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Printf("[SFXService] Freesound HTTP %d for %q: %s", resp.StatusCode, query, body)
		return "", 0
	}

	var result struct {
		Results []struct {
			Duration float64 `json:"duration"`
			Previews struct {
				PreviewHQMP3 string `json:"preview-hq-mp3"`
			} `json:"previews"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
		return "", 0
	}
	return result.Results[0].Previews.PreviewHQMP3, result.Results[0].Duration
}

// searchFreesound 通过 Freesound API 搜索 CC0 授权音效。
// 先用完整短语搜索，失败则拆词降级重试。结果缓存 sfxCacheTTL。
func (s *SFXService) searchFreesound(ctx context.Context, phrase string, maxDuration float64) sfxHit {
	if s.freesoundKey == "" || phrase == "" {
		return sfxHit{}
	}
	// 完整短语搜索
	query := strings.ReplaceAll(normalizeTag(phrase), "_", " ")
	cacheKey := "freesound:" + query
	hit := s.cachedQuery(cacheKey, func() sfxHit {
		u, dur := s.freesoundSearch(ctx, query, maxDuration)
		return sfxHit{url: u, source: "freesound", durationSecs: dur}
	})
	if hit.url != "" {
		return hit
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
		wHit := s.cachedQuery(ck, func() sfxHit {
			u, dur := s.freesoundSearch(ctx, w, maxDuration)
			return sfxHit{url: u, source: "freesound", durationSecs: dur}
		})
		if wHit.url != "" {
			return wHit
		}
	}
	return sfxHit{}
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
// 返回 URL、实际请求的时长（秒）和错误。
func (s *SFXService) generateElevenLabs(ctx context.Context, shot *model.StoryboardShot) (string, float64, error) {
	if s.elevenKey == "" {
		return "", 0, fmt.Errorf("elevenlabs key not configured")
	}
	if s.storageSvc == nil {
		return "", 0, fmt.Errorf("storage not configured for elevenlabs upload")
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
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", s.elevenKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("elevenlabs HTTP %d: %s", resp.StatusCode, bodyBytes)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}

	key := fmt.Sprintf("sfx/video_%d/shot_%d.mp3", shot.VideoID, shot.ID)
	u, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
	return u, float64(dur), err
}
