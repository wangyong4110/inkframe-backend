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

// sfxTagItem 结构化音效标签，包含搜索词和分类类型。
// SFXType: action=动作音（单次触发）/ ambient=环境底层音（循环）/ emotion=情绪点缀（冲击/rise）
type sfxTagItem struct {
	Tag     string `json:"tag"`
	SFXType string `json:"type"` // action / ambient / emotion
}

// parseSFXTags 解析 sfx_tags 字段，兼容旧版纯字符串数组和新版结构化格式。
func parseSFXTags(raw string) []sfxTagItem {
	if raw == "" {
		return nil
	}
	// 尝试新格式 [{"tag":"...","type":"..."}]
	var items []sfxTagItem
	if err := json.Unmarshal([]byte(raw), &items); err == nil && len(items) > 0 && items[0].Tag != "" {
		return items
	}
	// 兼容旧格式 ["...","..."]
	var strs []string
	if err := json.Unmarshal([]byte(raw), &strs); err == nil {
		items = make([]sfxTagItem, 0, len(strs))
		for _, s := range strs {
			items = append(items, sfxTagItem{Tag: s, SFXType: guessSFXType(s)})
		}
		return items
	}
	return nil
}

// guessSFXType 根据标签词汇推断音效类型（旧数据迁移用）。
func guessSFXType(tag string) string {
	lower := strings.ToLower(tag)
	ambientKW := []string{"loop", "ambient", "continuous", "sustained", "rain", "wind", "forest", "river", "crowd", "city", "room", "birds", "insects", "fire"}
	for _, kw := range ambientKW {
		if strings.Contains(lower, kw) {
			return "ambient"
		}
	}
	emotionKW := []string{"heartbeat", "clock", "tick", "rise", "stinger", "boom", "impact", "sub-bass", "breath"}
	for _, kw := range emotionKW {
		if strings.Contains(lower, kw) {
			return "emotion"
		}
	}
	return "action"
}

// shotSizeGuide 根据景别返回音效设计侧重说明。
func shotSizeGuide(shotSize string) string {
	switch shotSize {
	case "extreme_close_up":
		return "极近景/特写：强调微观细节音（衣物摩擦、皮肤/毛发接触、呼吸、心跳），禁止远景环境音"
	case "close_up":
		return "近景：突出物体近距离动作音，环境音压低至 subtle"
	case "wide":
		return "远景/全景：以环境底层音为主，动作音选 distant/reverb 版本"
	default: // medium
		return "中景：动作音与环境底层音并重，保持自然比例"
	}
}

// cameraMotionGuide 根据运镜类型返回额外音效提示。
func cameraMotionGuide(cameraType string) string {
	switch cameraType {
	case "pan":
		return "横移镜头：可加一条极短的 whoosh 扫场音（0.3–0.5s）"
	case "zoom":
		return "推拉镜头：快速推进可加 zoom in swoosh，拉远可不加额外音"
	case "tracking":
		return "跟随镜头：动作音随角色移动节奏，环境音保持稳定"
	default:
		return ""
	}
}

// analyzeSingleShotSFX 为单个分镜调用 AI 生成结构化音效搜索词，更新 sfx_tags 字段。
// 输出格式：[{"tag":"...","type":"action|ambient|emotion"}, ...]
// 景别/运镜/时长全部传入，AI 根据场景信息做专业分层设计。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string) error {
	// 构建分镜上下文
	var sceneCtx strings.Builder
	fmt.Fprintf(&sceneCtx, "镜头编号：%d\n", shot.ShotNo)
	fmt.Fprintf(&sceneCtx, "时长：%.1f 秒\n", shot.Duration)
	if shot.ShotSize != "" {
		fmt.Fprintf(&sceneCtx, "景别：%s\n", shot.ShotSize)
	}
	if shot.CameraType != "" && shot.CameraType != "static" {
		fmt.Fprintf(&sceneCtx, "运镜：%s\n", shot.CameraType)
	}
	if shot.Description != "" {
		fmt.Fprintf(&sceneCtx, "画面描述（视觉，仅用于推断声音来源，不要把视觉词写进标签）：%s\n", shot.Description)
	}
	if shot.Scene != "" {
		fmt.Fprintf(&sceneCtx, "场景环境：%s\n", shot.Scene)
	}
	if shot.EmotionalTone != "" {
		fmt.Fprintf(&sceneCtx, "情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Dialogue != "" {
		fmt.Fprintf(&sceneCtx, "⚠️ 有人物台词（对白）：环境底层音必须非常 subtle，禁止动作冲击音，避免掩盖人声\n")
	}
	if userContext != "" {
		fmt.Fprintf(&sceneCtx, "额外背景（优先参考）：%s\n", userContext)
	}

	// 景别 & 运镜引导
	sizeGuide := shotSizeGuide(shot.ShotSize)
	motionGuide := cameraMotionGuide(shot.CameraType)
	motionSection := ""
	if motionGuide != "" {
		motionSection = "\n运镜音提示：" + motionGuide
	}

	prompt := `你是有15年经验的好莱坞级影视音效设计师，负责为分镜脚本设计精准的 Freesound/SoundSnap 搜索词。

## 核心原则：声音必须真实可被听见
搜索词只能描述听觉现象——物体发出的真实声音。
绝对禁止视觉概念（光线/色彩/时段/情绪抽象）出现在搜索词里。

## 三层分层设计框架
| 层次 | 类型标记 | 触发方式 | 优先级 |
|------|----------|----------|--------|
| 动作音 | action | 单次触发（one-shot） | 最高，与画面强同步 |
| 环境底层音 | ambient | 循环（loop），贯穿全镜 | 中，建立空间感 |
| 情绪点缀音 | emotion | 单次触发，场景转折/强调 | 低，谨慎使用 |

## 景别设计规则
` + sizeGuide + motionSection + `

## Freesound 高命中格式：[物体/来源] [材质/空间] [动作] [音色描述符]

音色描述符参考：
- 触发音：single / one-shot / short / burst
- 循环音：loop / continuous
- 空间感：indoor / outdoor / reverb / dry / distant / close-up
- 质感：heavy / light / sharp / soft / subtle / muffled / crisp

✅ 正确示例：
- {"tag":"wooden door creak open indoor single","type":"action"} → 门开，指定材质+空间+动作
- {"tag":"metal sword unsheath sharp dry single","type":"action"} → 出剑，指定材质+质感
- {"tag":"forest birds chirping outdoor loop","type":"ambient"} → 森林环境，无视觉词
- {"tag":"rain tile roof pattering loop","type":"ambient"} → 雨声，指定落点+质感
- {"tag":"fire wood crackle close loop","type":"ambient"} → 火焰，指定材料+距离
- {"tag":"heartbeat tense pulse close single","type":"emotion"} → 心跳特写强调
- {"tag":"wolf paw dirt footstep soft single","type":"action"} → 狼爪脚步，指定材质
- {"tag":"qi energy crackle electric whoosh single","type":"action"} → 玄幻能量音

❌ 错误示例（绝对禁止）：
- "morning sunlight soft loop" → ❌ sunlight/morning 是视觉词
- "warm indoor ambience loop" → ❌ warm/ambience 是 BGM 词汇
- "forest atmosphere soundscape" → ❌ atmosphere/soundscape 是 BGM 词汇
- "room tone wooden house" → ❌ room tone 是音乐制作术语
- "bright cheerful music loop" → ❌ 音乐不是音效
- "sword" / "rain" / "fire" → ❌ 单词太笼统，Freesound 搜索无意义

## 输出规则
- 输出 0~3 条，按 action → ambient → emotion 优先级排列
- 每条 tag 为 3~7 个英文单词，必须是真实声音
- 极近景特写（无动作）：只输出 0~1 条 action（细节音），不加 ambient
- 纯情感特写/空镜：最多 1 条极 subtle 的 ambient
- 仅输出 JSON 数组，禁止任何额外文字

## 分镜信息
` + sceneCtx.String() + `
请输出：`

	result, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_analyze", prompt, "",
		StoryboardOverrides{TimeoutSeconds: 30})
	if err != nil {
		return fmt.Errorf("AI call: %w", err)
	}

	raw := extractJSON(result)

	// 解析结构化格式
	var items []sfxTagItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil || len(items) == 0 || items[0].Tag == "" {
		// 兼容旧版纯字符串输出
		var strs []string
		if err2 := json.Unmarshal([]byte(raw), &strs); err2 != nil {
			return fmt.Errorf("parse JSON: %w (raw=%q)", err, raw)
		}
		items = make([]sfxTagItem, 0, len(strs))
		for _, s2 := range strs {
			items = append(items, sfxTagItem{Tag: s2, SFXType: guessSFXType(s2)})
		}
	}

	// 过滤空 tag
	filtered := items[:0]
	for _, it := range items {
		if strings.TrimSpace(it.Tag) != "" {
			if it.SFXType == "" {
				it.SFXType = guessSFXType(it.Tag)
			}
			filtered = append(filtered, it)
		}
	}

	tagsJSON, _ := json.Marshal(filtered)
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

// sfxItemConfig 根据标签类型和镜头特征决定单条音效的播放参数。
func sfxItemConfig(item sfxTagItem, hasSpeech bool, hasNarration bool) (vol float64, loop bool, fadeInMs, fadeOutMs int) {
	switch item.SFXType {
	case "ambient":
		// 环境底层音：循环播放，淡入/淡出避免硬切
		loop = true
		fadeInMs = 300
		fadeOutMs = 500
		if hasSpeech {
			vol = 0.15 // 有台词：大幅压低，不掩盖人声
		} else if hasNarration {
			vol = 0.25 // 有旁白：适度压低
		} else {
			vol = 0.35
		}
	case "emotion":
		// 情绪点缀音：单次触发，短淡出
		loop = false
		fadeInMs = 0
		fadeOutMs = 300
		if hasSpeech {
			vol = 0.25
		} else {
			vol = 0.45
		}
	default: // action
		// 动作音：单次触发，不受台词影响（关键声音必须听到）
		loop = false
		fadeInMs = 0
		fadeOutMs = 100
		vol = sfxCategoryVolume(item.Tag) // 按具体类别（爆炸/脚步/门声等）定音量
	}
	if vol < 0.1 {
		vol = 0.1
	}
	return
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

	// 1. 提取结构化标签（优先用已有 sfx_tags；否则调用 AI 分析）
	tagItems := parseSFXTags(shot.SFXTags)
	if len(tagItems) == 0 {
		if err := s.analyzeSingleShotSFX(ctx, shot, tenantID, ""); err != nil {
			logger.Printf("[SFXService] shot %d AI analyze failed (%v), using rule fallback", shot.ID, err)
			for _, t := range s.fallbackTags(shot) {
				tagItems = append(tagItems, sfxTagItem{Tag: t, SFXType: guessSFXType(t)})
			}
		} else {
			tagItems = parseSFXTags(shot.SFXTags)
		}
	}
	if len(tagItems) == 0 {
		for _, t := range s.fallbackTags(shot) {
			tagItems = append(tagItems, sfxTagItem{Tag: t, SFXType: guessSFXType(t)})
		}
	}
	// 最多取前 5 个（action 优先，ambient 最多 1 个）
	tagItems = deduplicateAndLimit(tagItems, 5)

	tagsJSON, _ := json.Marshal(tagItems)
	logger.Printf("[SFXService] shot %d tags=%s", shot.ID, tagsJSON)

	maxDur := float64(shot.Duration)
	if maxDur <= 0 {
		maxDur = 0
	}
	hasSpeech := shot.Dialogue != ""
	hasNarration := shot.Narration != "" || shot.AudioPath != ""

	// 2. 逐 tag 搜索
	type sfxResult struct {
		item sfxTagItem
		hit  sfxHit
		vol  float64
		loop bool
		fin  int
		fout int
	}
	var results []sfxResult

	for _, item := range tagItems {
		hit := s.searchOneTag(ctx, tenantID, item, maxDur, shot)
		if hit.url == "" {
			logger.Printf("[SFXService] shot %d tag=%q: no result", shot.ID, item.Tag)
			continue
		}
		vol, loop, fin, fout := sfxItemConfig(item, hasSpeech, hasNarration)
		results = append(results, sfxResult{item: item, hit: hit, vol: vol, loop: loop, fin: fin, fout: fout})
		logger.Printf("[SFXService] shot %d tag=%q type=%s source=%s dur=%.1fs vol=%.2f loop=%v",
			shot.ID, item.Tag, item.SFXType, hit.source, hit.durationSecs, vol, loop)
	}

	if len(results) == 0 {
		return fmt.Errorf("no SFX found for shot %d (tags: %s)", shot.ID, tagsJSON)
	}

	// 3. 写入数据库
	dbItems := make([]*model.ShotSFXItem, 0, len(results))
	for i, r := range results {
		dbItems = append(dbItems, &model.ShotSFXItem{
			ShotID:       shot.ID,
			SeqNo:        i + 1,
			Tag:          r.item.Tag,
			URL:          r.hit.url,
			Volume:       r.vol,
			Source:       r.hit.source,
			DurationSecs: r.hit.durationSecs,
			SFXType:      r.item.SFXType,
			LoopEnabled:  r.loop,
			FadeInMs:     r.fin,
			FadeOutMs:    r.fout,
			// StartOffset 默认 0；action 音的精确帧偏移由前端手动调整
		})
	}

	if s.sfxItemRepo != nil {
		if err := s.sfxItemRepo.BatchCreate(dbItems); err != nil {
			return fmt.Errorf("save sfx items shot %d: %w", shot.ID, err)
		}
	}
	// 同步更新旧字段（向后兼容时间线播放）
	allTagsJSON, _ := json.Marshal(func() []string {
		ss := make([]string, len(results))
		for i, r := range results {
			ss[i] = r.item.Tag
		}
		return ss
	}())
	_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].hit.url, string(allTagsJSON), results[0].vol)
	return nil
}

// deduplicateAndLimit 对 tag 列表去重并限制数量：
// ambient 最多保留 1 条（防止多条循环音叠加成噪音），总数最多 limit 条。
func deduplicateAndLimit(items []sfxTagItem, limit int) []sfxTagItem {
	seen := map[string]bool{}
	ambientCount := 0
	out := make([]sfxTagItem, 0, limit)
	for _, it := range items {
		if seen[it.Tag] {
			continue
		}
		if it.SFXType == "ambient" {
			ambientCount++
			if ambientCount > 1 {
				continue // 最多 1 条环境底层音
			}
		}
		seen[it.Tag] = true
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// searchOneTag 对单个结构化标签执行三层降级搜索：本地库 → Freesound → ElevenLabs（AI生成）。
// ElevenLabs 是最后兜底，每个 tag 独立生成一条定制音效（非多 tag 混合）。
func (s *SFXService) searchOneTag(ctx context.Context, tenantID uint, item sfxTagItem, maxDur float64, shot *model.StoryboardShot) sfxHit {
	if u, dur := s.searchLocalLib(ctx, tenantID, item.Tag); u != "" {
		logger.Printf("[SFXService] shot %d local hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
		return sfxHit{url: u, source: "local", durationSecs: dur}
	}
	if s.freesoundKey == "" {
		logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
	} else if hit := s.searchFreesound(ctx, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Freesound hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d Freesound miss tag=%q", shot.ID, item.Tag)
	}
	// ElevenLabs：每个 tag 独立生成，避免多 tag 混音成一条不可分离的音频
	if u, dur, err := s.generateElevenLabsForTag(ctx, item, shot); err == nil && u != "" {
		logger.Printf("[SFXService] shot %d ElevenLabs hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
		return sfxHit{url: u, source: "elevenlabs", durationSecs: dur}
	} else if err != nil {
		logger.Printf("[SFXService] shot %d ElevenLabs failed tag=%q: %v", shot.ID, item.Tag, err)
	}
	return sfxHit{}
}

// BatchAutoGenerateSFX 批量处理视频所有镜头，最多 5 并发。
// 全部镜头处理完成后执行场景连续性修复：同一场景的连续镜头共用同一条 ambient 底层音，
// 避免每镜切换环境音导致的听感跳变。
// progressFn 每完成一个镜头时实时调用（0-100）。
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
			n := int(doneCount.Add(1))
			if progressFn != nil {
				progressFn(n * 100 / total)
			}
		}(shot)
	}
	wg.Wait()

	// 场景连续性修复（串行，不影响上面的并发结果）
	if s.sfxItemRepo != nil && len(shots) > 1 {
		s.applySceneContinuity(ctx, shots)
	}

	return int(successCount.Load()), int(failCount.Load())
}

// applySceneContinuity 将同一场景的连续镜头的 ambient 音效统一为该场景首镜的 ambient 音效。
// 场景相同判断：Scene 字段内容（前64字符）相同，或 ShotNo 连续且 Scene 为空时跳过。
// 效果：森林场景的镜头3→4→5 共用同一条 forest birds loop，不再每镜各自搜索不同 URL。
func (s *SFXService) applySceneContinuity(ctx context.Context, shots []*model.StoryboardShot) {
	type sceneGroup struct {
		key        string
		ambientURL string
		ambientVol float64
	}
	var current sceneGroup

	for _, shot := range shots {
		sceneKey := sceneKeyOf(shot)
		if sceneKey == "" {
			current = sceneGroup{} // 无场景信息，重置
			continue
		}

		items, err := s.sfxItemRepo.ListByShotID(shot.ID)
		if err != nil || len(items) == 0 {
			if sceneKey == current.key {
				continue // 该镜头无音效，保持当前 scene 状态
			}
			current = sceneGroup{key: sceneKey}
			continue
		}

		// 找本镜头的 ambient 条目
		var ambientItem *model.ShotSFXItem
		for _, it := range items {
			if it.SFXType == "ambient" && !it.Disabled {
				ambientItem = it
				break
			}
		}

		if sceneKey != current.key {
			// 场景切换：重置，以本镜头 ambient 为新场景基准
			current = sceneGroup{key: sceneKey}
			if ambientItem != nil {
				current.ambientURL = ambientItem.URL
				current.ambientVol = ambientItem.Volume
			}
			continue
		}

		// 同一场景：将本镜头 ambient 替换为首镜的 ambient URL
		if current.ambientURL != "" && ambientItem != nil && ambientItem.URL != current.ambientURL {
			ambientItem.URL = current.ambientURL
			ambientItem.Volume = current.ambientVol
			if err := s.sfxItemRepo.Update(ambientItem); err != nil {
				logger.Printf("[SFXService] scene continuity: update shot %d ambient failed: %v", shot.ID, err)
			} else {
				logger.Printf("[SFXService] scene continuity: shot %d ambient unified to scene %q", shot.ID, sceneKey[:min(len(sceneKey), 20)])
			}
		} else if current.ambientURL == "" && ambientItem != nil {
			// 首镜没 ambient 但后续镜头有，以后续镜头为基准
			current.ambientURL = ambientItem.URL
			current.ambientVol = ambientItem.Volume
		}
	}
}

// sceneKeyOf 提取镜头的场景标识键（取 Scene 字段前64字符，忽略空格差异）。
func sceneKeyOf(shot *model.StoryboardShot) string {
	s := strings.TrimSpace(shot.Scene)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
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
// 两级匹配（移除了原来不可靠的 level-3 单词弱匹配，防止误判）：
//  1. 精确标准化匹配（"rain_heavy" ↔ "rain heavy"）
//  2. 库键的所有词均出现在短语中（"rain_heavy" ↔ "heavy rain rooftop loop"）
func matchLocalLibKey(lib map[string]string, phrase string) (string, bool) {
	phraseWords := strings.Fields(strings.ToLower(phrase))
	phraseSet := make(map[string]bool, len(phraseWords))
	for _, w := range phraseWords {
		phraseSet[w] = true
	}

	// 1. 精确标准化匹配
	normalized := strings.Join(phraseWords, "_")
	if filename, ok := lib[normalized]; ok {
		return filename, true
	}

	// 2. 键的所有词均出现在短语中（精确子集匹配，防止误判）
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

// freesoundSearchResults 执行单次 Freesound API 搜索，按相关性排序，返回 top-N 结果。
// 对 action/emotion 类型限制时长 ≤ maxDuration；对 ambient 类型要求时长 ≥ 2s（用于循环）。
func (s *SFXService) freesoundSearchResults(ctx context.Context, query string, maxDuration float64, sfxType string) []struct {
	URL      string
	Duration float64
} {
	filter := `license:"Creative Commons 0"`
	switch sfxType {
	case "ambient":
		// 环境音需要足够长以供循环，不受镜头时长上限限制
		filter += " duration:[2.0 TO 120.0]"
	default:
		// 动作音/情绪音：时长不超过镜头时长，且最短 0.1s
		if maxDuration > 0 {
			filter += fmt.Sprintf(" duration:[0.1 TO %.1f]", maxDuration)
		} else {
			filter += " duration:[0.1 TO 30.0]"
		}
	}

	apiURL := fmt.Sprintf(
		// sort=score 按相关性排序（而非下载量），page_size=5 取前5个候选
		"https://freesound.org/apiv2/search/text/?query=%s&filter=%s&fields=id,name,previews,duration&sort=score&page_size=5&token=%s",
		url.QueryEscape(query), url.QueryEscape(filter), s.freesoundKey,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		logger.Printf("[SFXService] Freesound request error for %q: %v", query, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Printf("[SFXService] Freesound HTTP %d for %q: %s", resp.StatusCode, query, body)
		return nil
	}

	var result struct {
		Results []struct {
			Duration float64 `json:"duration"`
			Previews struct {
				PreviewHQMP3 string `json:"preview-hq-mp3"`
			} `json:"previews"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	out := make([]struct {
		URL      string
		Duration float64
	}, 0, len(result.Results))
	for _, r := range result.Results {
		if r.Previews.PreviewHQMP3 != "" {
			out = append(out, struct {
				URL      string
				Duration float64
			}{r.Previews.PreviewHQMP3, r.Duration})
		}
	}
	return out
}

// searchFreesound 通过 Freesound API 搜索 CC0 授权音效。
// 从 top-5 中挑选最佳：action 选时长最短的（单次触发），ambient 选时长最长的（循环素材）。
// 不再做单词拆分降级搜索（会产生不可控的误匹配）。
func (s *SFXService) searchFreesound(ctx context.Context, item sfxTagItem, maxDuration float64) sfxHit {
	if s.freesoundKey == "" || item.Tag == "" {
		return sfxHit{}
	}
	query := strings.ReplaceAll(normalizeTag(item.Tag), "_", " ")
	cacheKey := fmt.Sprintf("freesound:%s:%s", item.SFXType, query)

	return s.cachedQuery(cacheKey, func() sfxHit {
		results := s.freesoundSearchResults(ctx, query, maxDuration, item.SFXType)
		if len(results) == 0 {
			return sfxHit{}
		}
		// 选最佳结果
		best := results[0]
		for _, r := range results[1:] {
			switch item.SFXType {
			case "ambient":
				// 环境音：选时长最长的，循环接缝最少
				if r.Duration > best.Duration {
					best = r
				}
			default:
				// 动作音/情绪音：选时长最短的，避免尾音过长
				if r.Duration < best.Duration && r.Duration >= 0.1 {
					best = r
				}
			}
		}
		return sfxHit{url: best.URL, source: "freesound", durationSecs: best.Duration}
	})
}

// buildElevenLabsPrompt 将结构化标签转换为 ElevenLabs 自然语言描述。
// ElevenLabs 接受自然语言，不接受关键词堆砌；需要明确描述声音的物理特征和空间感。
func buildElevenLabsPrompt(item sfxTagItem, shot *model.StoryboardShot) string {
	tag := strings.ReplaceAll(normalizeTag(item.Tag), "_", " ")
	var sb strings.Builder

	switch item.SFXType {
	case "ambient":
		sb.WriteString("Ambient background sound: ")
		sb.WriteString(tag)
		sb.WriteString(". Continuous loop, smooth and consistent, no sudden changes.")
	case "emotion":
		sb.WriteString("Emotional accent sound effect: ")
		sb.WriteString(tag)
		sb.WriteString(". Short cinematic sting, impactful.")
	default: // action
		sb.WriteString("Sound effect: ")
		sb.WriteString(tag)
		sb.WriteString(". Single occurrence, realistic and precise.")
	}
	if shot.EmotionalTone != "" {
		sb.WriteString(" Mood: ")
		sb.WriteString(shot.EmotionalTone)
		sb.WriteString(".")
	}
	prompt := sb.String()
	runes := []rune(prompt)
	if len(runes) > 200 {
		prompt = string(runes[:200])
	}
	return prompt
}

// generateElevenLabsForTag 对单个结构化标签调用 ElevenLabs Sound Generation API。
// 每个 tag 独立生成，避免多标签混成一条不可分离的音频。
// prompt_influence=0.7（提高提示词约束力，降低模型自由发挥程度）。
func (s *SFXService) generateElevenLabsForTag(ctx context.Context, item sfxTagItem, shot *model.StoryboardShot) (string, float64, error) {
	if s.elevenKey == "" {
		return "", 0, fmt.Errorf("elevenlabs key not configured")
	}
	if s.storageSvc == nil {
		return "", 0, fmt.Errorf("storage not configured for elevenlabs upload")
	}

	prompt := buildElevenLabsPrompt(item, shot)

	// 时长：ambient 用镜头全长（循环），action/emotion 最多 5s
	dur := shot.Duration
	if item.SFXType != "ambient" && dur > 5 {
		dur = 5
	}
	if dur <= 0 {
		dur = 3
	}
	if dur > 22 {
		dur = 22
	}

	body, _ := json.Marshal(map[string]interface{}{
		"text":             prompt,
		"duration_seconds": dur,
		"prompt_influence": 0.7, // 0.3→0.7，提高提示词约束力
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

	// key 包含 tag 哈希以避免同镜头多条音效覆盖
	tagHash := fmt.Sprintf("%x", len(item.Tag)*31+len(item.SFXType))
	key := fmt.Sprintf("sfx/video_%d/shot_%d_%s.mp3", shot.VideoID, shot.ID, tagHash)
	u, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
	return u, float64(dur), err
}
