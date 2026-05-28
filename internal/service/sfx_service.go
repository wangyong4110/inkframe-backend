package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
// 降级链：AI 文生音效 → 本地库 → AudioLDM（本地模型）→ Freesound API → Pixabay Audio → BBC Sound Effects（爬取） → ElevenLabs（AI生成）。
type SFXService struct {
	aiSvc            *AIService
	storageSvc       storage.Service
	storyboardRepo   *repository.StoryboardRepository
	sfxItemRepo      *repository.ShotSFXItemRepository
	assetRepo        *repository.AssetRepository
	tagRepo          *repository.TagRepository
	sfxDir           string // 本地音效目录
	freesoundKey     string // Freesound API Token（可选）
	pixabayKey       string // Pixabay API Key（可选）
	elevenKey        string // ElevenLabs API Key（可选）
	audioLDMEndpoint string // 本地 AudioLDM HTTP API 地址（可选，如 http://localhost:8000/generate）
	audioLDMKey      string // AudioLDM API 鉴权 Token（可选，本地部署通常留空）
	httpClient       *http.Client
	localLib         map[string]string // 内置标签 → 文件名（不含目录）
	localUploadCache sync.Map          // local file path → OSS URL（进程内缓存）
	queryCache       sync.Map          // "source:query" → sfxCacheEntry
}

// WithSFXItemRepo 注入音效条目仓库（可选；注入后才启用多 item 存储）
func (s *SFXService) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *SFXService {
	s.sfxItemRepo = r
	return s
}

// WithAssetRepo 注入素材库仓库（可选；注入后生成的音效将自动存入素材库）
func (s *SFXService) WithAssetRepo(assetRepo *repository.AssetRepository, tagRepo *repository.TagRepository) *SFXService {
	s.assetRepo = assetRepo
	s.tagRepo = tagRepo
	return s
}

// SFXServiceConfig SFXService 构造参数
type SFXServiceConfig struct {
	SFXDir          string // 本地音效目录（环境变量 SFX_DIR）
	FreesoundKey    string // Freesound API Token（环境变量 FREESOUND_API_KEY）
	PixabayKey      string // Pixabay API Key（环境变量 PIXABAY_API_KEY；启用后搜索 Pixabay 音效库）
	JamendoClientID string // 保留字段（Jamendo 为音乐平台，SFX 不使用）
	ElevenLabsKey      string // 环境变量 ELEVENLABS_API_KEY
	AudioLDMEndpoint   string // 本地 AudioLDM API 地址（环境变量 AUDIOLDM_ENDPOINT，如 http://localhost:8000/generate）
	AudioLDMKey        string // AudioLDM 鉴权 Token（环境变量 AUDIOLDM_KEY，本地通常留空）
	ProxyURL           string // 爬取代理（环境变量 CRAWL_PROXY_URL）；为空则使用系统 HTTPS_PROXY
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
		pixabayKey:     cfg.PixabayKey,
		elevenKey:        cfg.ElevenLabsKey,
		audioLDMEndpoint: cfg.AudioLDMEndpoint,
		audioLDMKey:      cfg.AudioLDMKey,
		httpClient:       buildCrawlHTTPClient(cfg.ProxyURL, 30*time.Second),
		localLib:       buildDefaultSFXLib(),
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
// Tag:    搜索词（英文时为 Freesound/Pixabay/BBC/Aigei 格式；中文时直接用于 Kling SFX）
// Prompt: AI 生成提示词（用于 Kling SFX/ElevenLabs；通常为中文自然语言描述，为空时退化为 Tag）
type sfxTagItem struct {
	Tag     string `json:"tag"`
	SFXType string `json:"type"`             // action / ambient / emotion
	Prompt  string `json:"prompt,omitempty"` // AI 生成提示词（Kling SFX / ElevenLabs 专用）
}

// SFXTagItemPublic 是 sfxTagItem 的公开版本，供 handler 层使用。
type SFXTagItemPublic struct {
	Tag     string `json:"tag"`
	SFXType string `json:"type"`
	Prompt  string `json:"prompt,omitempty"`
}

// UpdateShotSFXTagsPublic 更新单个镜头的 sfx_tags（handler 层专用，接受公开类型）。
func (s *SFXService) UpdateShotSFXTagsPublic(shotID uint, tags []SFXTagItemPublic) error {
	items := make([]sfxTagItem, 0, len(tags))
	for _, t := range tags {
		items = append(items, sfxTagItem{Tag: t.Tag, SFXType: t.SFXType, Prompt: t.Prompt})
	}
	return s.UpdateShotSFXTags(shotID, items)
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
// 输出格式：[{"tag":"...","type":"action|ambient|emotion","prompt":"..."}, ...]
// tag 字段始终输出英文（Freesound 四元格式），prompt 字段为中文自然语言（供 Kling SFX / AudioLDM 使用）。
func (s *SFXService) analyzeSingleShotSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, userContext string, promptLanguage string) error {
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

	langInstruction := `
## Tag 格式（英文，Freesound 四元格式）：[物体/来源] [材质/空间] [动作] [音色描述符]
音色描述符：single/one-shot/burst（触发音）；loop/continuous（循环音）；indoor/outdoor/reverb/dry/distant（空间感）；heavy/light/sharp/soft/subtle/crisp（质感）
示例：
- {"tag":"wooden door creak open indoor single","type":"action","prompt":"室内木门缓慢打开，嘎吱声"}
- {"tag":"forest birds chirping outdoor loop","type":"ambient","prompt":"森林清晨，鸟鸣环境音循环"}
- {"tag":"heartbeat tense pulse close single","type":"emotion","prompt":"紧张特写，心跳加速"}
❌ tag 禁止：视觉词（sunlight/morning/warm/bright）、情绪形容词（epic/mystical/dramatic）、BGM词（ambience/atmosphere/soundscape）、单词笼统词（sword/rain/fire）
✅ prompt 字段用中文自然语言描述声音场景（供 AI 文生音效使用，可与 tag 描述同一声音）
`

	prompt := `你是有15年经验的好莱坞级影视音效设计师，负责为分镜脚本设计精准的音效搜索词。

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
` + sizeGuide + motionSection + langInstruction + `

## 输出规则
- 输出 0~3 条，按 action → ambient → emotion 优先级排列
- 极近景特写（无动作）：只输出 0~1 条 action（细节音），不加 ambient
- 纯情感特写/空镜：最多 1 条极 subtle 的 ambient
- 每条必须包含 tag（英文搜索词）、type、prompt（中文描述，供 AI 生成使用）三个字段
- 仅输出 JSON 数组，禁止任何额外文字

## 分镜信息
` + sceneCtx.String() + `
请输出：`

	// MaxTokens=3000：推理模型（如 DeepSeek-R1）会先输出思考过程再输出 JSON，
	// 3000 token 足以容纳思考过程（~500-800 tok）+ JSON 输出（~100-200 tok）。
	// jsonOnlySystemPrompt（由 ai_service 注入）会抑制大多数推理模型的思考输出。
	// TimeoutSeconds=30：正常请求 10-15s 完成，30s 为宽裕上限。
	callResult := func() (string, error) {
		return s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_analyze", prompt, "",
			StoryboardOverrides{TimeoutSeconds: 30, MaxTokens: 3000})
	}
	result, err := callResult()
	if err != nil {
		return fmt.Errorf("AI call: %w", err)
	}

	raw := extractJSON(result)
	// DeepSeek-chat (V3) 有时在 content 里先输出推理过程再输出 JSON。
	// 若 extractJSON 未能提取到有效数组（result 不以 [ 开头），尝试直接定位第一个 [ 字符。
	if len(raw) == 0 || raw[0] != '[' {
		if idx := strings.Index(result, "["); idx != -1 {
			raw = extractJSON(result[idx:])
		}
	}
	// 响应异常短（< 80 字节）说明模型输出不完整或被截断，重试一次。
	if len(strings.TrimSpace(raw)) < 80 {
		logger.Printf("[SFXService] shot %d: response too short (%d bytes), retrying", shot.ShotNo, len(raw))
		if r2, err2 := callResult(); err2 == nil {
			if raw2 := extractJSON(r2); len(strings.TrimSpace(raw2)) > len(raw) {
				raw = raw2
			}
		}
	}

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

	// 为每个 tag 填充中文 Prompt（供 Kling SFX / ElevenLabs AI 文生音效使用）
	shotPrompt := buildShotAIPrompt(shot)
	for i := range filtered {
		if filtered[i].Prompt == "" {
			filtered[i].Prompt = shotPrompt
		}
	}

	tagsJSON, _ := json.Marshal(filtered)
	shot.SFXTags = string(tagsJSON)
	if err := s.storyboardRepo.UpdateSFXTags(shot.ID, string(tagsJSON)); err != nil {
		return fmt.Errorf("update sfx_tags: %w", err)
	}
	return nil
}

// AnalyzeSFXForVideo 并行为每个分镜单独调用 AI 生成结构化音效搜索词，写入 sfx_tags 字段。
// promptLanguage：项目提示词语言（"zh"=中文，"en"=英文）；影响 AI 输出标签语言。
// 每个分镜独立分析，并发度最多 15，单个失败不影响其余镜头。
func (s *SFXService) AnalyzeSFXForVideo(ctx context.Context, shots []*model.StoryboardShot, tenantID uint, userContext string, promptLanguage string) error {
	if len(shots) == 0 {
		return nil
	}
	logger.Printf("[SFXService] AnalyzeSFXForVideo: parallel analysis for %d shots (lang=%s)", len(shots), promptLanguage)

	const maxConcurrency = 15
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var updated, failed atomic.Int32

	var skipped atomic.Int32
	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		// 已有有效结构化 tags（非空且含 tag 字段）则跳过，避免重复调用 AI
		if existing := parseSFXTags(shot.SFXTags); len(existing) > 0 && existing[0].Tag != "" {
			skipped.Add(1)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sh *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.analyzeSingleShotSFX(ctx, sh, tenantID, userContext, promptLanguage)
			if err != nil {
				logger.Printf("[SFXService] AnalyzeSFXForVideo: shot %d failed: %v", sh.ShotNo, err)
				failed.Add(1)
			} else {
				updated.Add(1)
			}
		}(shot)
	}
	wg.Wait()
	logger.Printf("[SFXService] AnalyzeSFXForVideo: updated=%d failed=%d skipped=%d(already tagged)",
		updated.Load(), failed.Load(), skipped.Load())
	return nil
}

// UpdateShotSFXTags 直接更新单个镜头的 sfx_tags 字段（用于前端手动插入/修改/删除标签）。
func (s *SFXService) UpdateShotSFXTags(shotID uint, tags []sfxTagItem) error {
	data, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	return s.storyboardRepo.UpdateSFXTags(shotID, string(data))
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

	// 1. 提取结构化标签
	// 优先级：已存 sfx_tags（含 LLM 分析结果及 Prompt 字段）> 实时 LLM 分析 > 规则兜底
	// 始终通过 LLM 分析填充结构化英文 tag 与中文 Prompt，确保 AI 文生音效和搜索库均能高质量命中
	tagItems := parseSFXTags(shot.SFXTags)
	if len(tagItems) == 0 {
		if err := s.analyzeSingleShotSFX(ctx, shot, tenantID, "", ""); err != nil {
			logger.Printf("[SFXService] shot %d AI analyze failed (%v), using rule fallback", shot.ID, err)
			// 规则兜底：英文搜索词 + 中文 AI 提示词
			shotPrompt := buildShotAIPrompt(shot)
			for _, t := range s.fallbackTags(shot) {
				tagItems = append(tagItems, sfxTagItem{Tag: t, SFXType: guessSFXType(t), Prompt: shotPrompt})
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
	// 自动存入素材库（异步，失败不影响主流程）
	if s.assetRepo != nil && s.tagRepo != nil {
		go s.saveToAssetLibrary(context.Background(), shot, dbItems, tenantID)
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

// searchOneTag 对单个结构化标签执行多层降级搜索。
// 优先级：AI 文生音效 → 本地库 → AudioLDM（本地模型）→ Freesound → Pixabay → BBC Sound Effects → ElevenLabs。
func (s *SFXService) searchOneTag(ctx context.Context, tenantID uint, item sfxTagItem, maxDur float64, shot *model.StoryboardShot) sfxHit {
	sfxDur := maxDur
	if sfxDur <= 0 {
		sfxDur = 5
	}
	// 1. AI 文生音效（Kling SFX 等 sfx 类型提供商）——优先尝试
	// 优先使用 Prompt（中文自然语言描述，信息更丰富），降级到英文搜索词 Tag
	if s.aiSvc != nil {
		aiPrompt := item.Prompt
		if aiPrompt == "" {
			aiPrompt = item.Tag
		}
		if u, dur, err := s.aiSvc.GenerateSFX(ctx, tenantID, aiPrompt, sfxDur); err == nil && u != "" {
			logger.Printf("[SFXService] shot %d AI-SFX hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
			return sfxHit{url: u, source: "ai-sfx", durationSecs: dur}
		} else if err != nil && !isNoProviderErr(err) {
			logger.Printf("[SFXService] shot %d AI-SFX failed tag=%q: %v", shot.ID, item.Tag, err)
		}
	}
	// 2. 本地音效库
	if u, dur := s.searchLocalLib(ctx, tenantID, item.Tag); u != "" {
		logger.Printf("[SFXService] shot %d local hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
		return sfxHit{url: u, source: "local", durationSecs: dur}
	}
	// 3. AudioLDM（本地部署模型，免费、无速率限制，优先于远程 API）
	if s.audioLDMEndpoint != "" {
		if u, dur, err := s.generateAudioLDMForTag(ctx, item, shot); err == nil && u != "" {
			logger.Printf("[SFXService] shot %d AudioLDM hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
			return sfxHit{url: u, source: "audioldm", durationSecs: dur}
		} else if err != nil {
			logger.Printf("[SFXService] shot %d AudioLDM failed tag=%q: %v", shot.ID, item.Tag, err)
		}
	}
	// 4. Freesound API（CC0，需 API Key）
	if s.freesoundKey == "" {
		logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
	} else if hit := s.searchFreesound(ctx, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Freesound hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d Freesound miss tag=%q", shot.ID, item.Tag)
	}
	// 5. Pixabay Audio（CC0，需 API Key）
	if s.pixabayKey == "" {
		logger.Printf("[SFXService] shot %d skip Pixabay (no key)", shot.ID)
	} else if hit := s.searchPixabay(ctx, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Pixabay hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d Pixabay miss tag=%q", shot.ID, item.Tag)
	}
	// 6. BBC Sound Effects（BBC RemArc Licence，无需 API Key，直接爬取）
	if hit := s.searchBBCSFX(ctx, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d BBC-SFX hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d BBC-SFX miss tag=%q", shot.ID, item.Tag)
	}
	// 7. 爱给网（aigei.com，免费音效，无需 API Key）
	if hit := s.searchAigei(ctx, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Aigei hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	} else {
		logger.Printf("[SFXService] shot %d Aigei miss tag=%q", shot.ID, item.Tag)
	}
	// 8. ElevenLabs：每个 tag 独立生成，避免多 tag 混音成一条不可分离的音频
	if u, dur, err := s.generateElevenLabsForTag(ctx, item, shot); err == nil && u != "" {
		logger.Printf("[SFXService] shot %d ElevenLabs hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
		return sfxHit{url: u, source: "elevenlabs", durationSecs: dur}
	} else if err != nil {
		logger.Printf("[SFXService] shot %d ElevenLabs failed tag=%q: %v", shot.ID, item.Tag, err)
	}
	return sfxHit{}
}

// isNoProviderErr 判断错误是否为"未配置该类型提供商"（静默跳过，无需打印告警）。
func isNoProviderErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no sfx providers configured")
}

// BatchAutoGenerateSFX 批量处理视频所有镜头，最多 10 并发。
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
	const maxConcurrency = 10
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

// buildShotAIPrompt 从分镜字段构建中文自然语言描述，供 AI 文生音效提供商（Kling SFX / ElevenLabs）使用。
// 包含场景环境、画面描述和情绪基调，最多 200 个字符。
func buildShotAIPrompt(shot *model.StoryboardShot) string {
	var parts []string
	if shot.Scene != "" {
		parts = append(parts, shot.Scene)
	}
	if shot.Description != "" {
		runes := []rune(shot.Description)
		if len(runes) > 120 {
			runes = runes[:120]
		}
		parts = append(parts, string(runes))
	}
	if shot.EmotionalTone != "" {
		parts = append(parts, shot.EmotionalTone)
	}
	prompt := strings.Join(parts, "。")
	if prompt == "" {
		return ""
	}
	runes := []rune(prompt)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return prompt
}

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

// searchPixabay 通过 Pixabay Audio API 搜索音效（需配置 PIXABAY_API_KEY）。
// 返回 CC0 授权音效的直链 URL，ambient 类型选时长最长，其余选时长最短。
func (s *SFXService) searchPixabay(ctx context.Context, item sfxTagItem, maxDuration float64) sfxHit {
	if s.pixabayKey == "" || item.Tag == "" {
		return sfxHit{}
	}
	query := strings.ReplaceAll(normalizeTag(item.Tag), "_", " ")
	cacheKey := fmt.Sprintf("pixabay:%s:%s", item.SFXType, query)

	return s.cachedQuery(cacheKey, func() sfxHit {
		apiURL := fmt.Sprintf(
			"https://pixabay.com/api/?key=%s&q=%s&media_type=music&page_size=5",
			s.pixabayKey, url.QueryEscape(query),
		)
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return sfxHit{}
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			logger.Printf("[SFXService] Pixabay request error for %q: %v", query, err)
			return sfxHit{}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			logger.Printf("[SFXService] Pixabay HTTP %d for %q: %s", resp.StatusCode, query, body)
			return sfxHit{}
		}

		var result struct {
			Hits []struct {
				Duration float64 `json:"duration"`
				Audio    string  `json:"audio"`
				Tags     string  `json:"tags"`
			} `json:"hits"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Hits) == 0 {
			return sfxHit{}
		}

		// 从候选中挑最佳：ambient 选最长，其余选最短（且不超过镜头时长）
		type candidate struct {
			url string
			dur float64
		}
		var candidates []candidate
		for _, h := range result.Hits {
			if h.Audio == "" {
				continue
			}
			if item.SFXType != "ambient" && maxDuration > 0 && h.Duration > maxDuration {
				continue
			}
			candidates = append(candidates, candidate{h.Audio, h.Duration})
		}
		if len(candidates) == 0 {
			// 无满足时长限制的结果，放宽限制取第一个
			for _, h := range result.Hits {
				if h.Audio != "" {
					return sfxHit{url: h.Audio, source: "pixabay", durationSecs: h.Duration}
				}
			}
			return sfxHit{}
		}
		best := candidates[0]
		for _, c := range candidates[1:] {
			if item.SFXType == "ambient" {
				if c.dur > best.dur {
					best = c
				}
			} else {
				if c.dur >= 0.1 && c.dur < best.dur {
					best = c
				}
			}
		}
		return sfxHit{url: best.url, source: "pixabay", durationSecs: best.dur}
	})
}

// searchBBCSFX 通过 BBC Sound Effects 公开搜索接口爬取音效（无需 API Key）。
// BBC Sound Effects 提供 33,000+ 专业音效，均在 BBC RemArc Licence 下免费使用。
// 爬取策略：搜索 → 取最佳候选（ambient 选最长，其余选最短）→ 返回 MP3 直链。
func (s *SFXService) searchBBCSFX(ctx context.Context, item sfxTagItem, maxDuration float64) sfxHit {
	if item.Tag == "" {
		return sfxHit{}
	}
	query := strings.ReplaceAll(normalizeTag(item.Tag), "_", " ")
	cacheKey := fmt.Sprintf("bbc:%s:%s", item.SFXType, query)

	return s.cachedQuery(cacheKey, func() sfxHit {
		apiURL := fmt.Sprintf(
			"https://sound-effects.bbcrewind.co.uk/api/sfx/search?q=%s&limit=5",
			url.QueryEscape(query),
		)
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return sfxHit{}
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InkFrame/1.0; +https://inkframe.io)")
		req.Header.Set("Accept", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			logger.Printf("[SFXService] BBC SFX request error for %q: %v", query, err)
			return sfxHit{}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return sfxHit{}
		}

		var result struct {
			Count   int `json:"count"`
			Results []struct {
				ID          string  `json:"id"`
				Description string  `json:"description"`
				Duration    float64 `json:"duration"`
				Formats     struct {
					MP3 string `json:"mp3"`
				} `json:"formats"`
			} `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
			return sfxHit{}
		}

		// 构造 MP3 URL：优先使用 formats.mp3，回退到 CDN 路径
		mp3URL := func(id, fmtMP3 string) string {
			if fmtMP3 != "" {
				return fmtMP3
			}
			if id != "" {
				return fmt.Sprintf("https://sound-effects-media.bbcrewind.co.uk/mp3/%s.mp3", id)
			}
			return ""
		}

		type candidate struct {
			url string
			dur float64
		}
		var candidates []candidate
		for _, r := range result.Results {
			u := mp3URL(r.ID, r.Formats.MP3)
			if u == "" {
				continue
			}
			if item.SFXType != "ambient" && maxDuration > 0 && r.Duration > maxDuration {
				continue
			}
			candidates = append(candidates, candidate{u, r.Duration})
		}
		if len(candidates) == 0 {
			// 放宽时长限制
			for _, r := range result.Results {
				if u := mp3URL(r.ID, r.Formats.MP3); u != "" {
					return sfxHit{url: u, source: "bbc-sfx", durationSecs: r.Duration}
				}
			}
			return sfxHit{}
		}
		best := candidates[0]
		for _, c := range candidates[1:] {
			if item.SFXType == "ambient" {
				if c.dur > best.dur {
					best = c
				}
			} else {
				if c.dur >= 0.1 && c.dur < best.dur {
					best = c
				}
			}
		}
		return sfxHit{url: best.url, source: "bbc-sfx", durationSecs: best.dur}
	})
}

// searchAigei 通过爱给网（aigei.com）搜索免费音效（无需 API Key）。
// API 路径：https://www.aigei.com/service/sound/search
func (s *SFXService) searchAigei(ctx context.Context, item sfxTagItem, maxDuration float64) sfxHit {
	if item.Tag == "" {
		return sfxHit{}
	}
	query := strings.ReplaceAll(normalizeTag(item.Tag), "_", " ")
	cacheKey := fmt.Sprintf("aigei:%s:%s", item.SFXType, query)
	return s.cachedQuery(cacheKey, func() sfxHit {
		apiURL := fmt.Sprintf(
			"https://www.aigei.com/service/sound/search?term=%s&pageSize=10&page=1&type=sound",
			url.QueryEscape(query),
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return sfxHit{}
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InkFrame/1.0)")
		req.Header.Set("Referer", "https://www.aigei.com/")
		req.Header.Set("Accept", "application/json, text/plain, */*")

		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return sfxHit{}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return sfxHit{}
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
		if err != nil {
			return sfxHit{}
		}

		// 响应结构：{ "data": { "list": [{"fileTitle":"...","fileTime":"0:30","playUrl":"...","downUrl":"..."}] } }
		var result struct {
			Data struct {
				List []struct {
					FileTitle string `json:"fileTitle"`
					FileTime  string `json:"fileTime"` // "M:SS" or seconds string
					PlayURL   string `json:"playUrl"`
					DownURL   string `json:"downUrl"`
				} `json:"list"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return sfxHit{}
		}

		type candidate struct {
			url string
			dur float64
		}
		var candidates []candidate
		for _, it := range result.Data.List {
			u := it.PlayURL
			if u == "" {
				u = it.DownURL
			}
			if u == "" {
				continue
			}
			dur := parseAigeiDuration(it.FileTime)
			if maxDuration > 0 && dur > maxDuration+2 {
				continue
			}
			candidates = append(candidates, candidate{url: u, dur: dur})
		}
		if len(candidates) == 0 {
			return sfxHit{}
		}
		best := candidates[0]
		for _, c := range candidates[1:] {
			if item.SFXType == "ambient" {
				if c.dur > best.dur {
					best = c
				}
			} else {
				if c.dur >= 0.1 && c.dur < best.dur {
					best = c
				}
			}
		}
		return sfxHit{url: best.url, source: "aigei", durationSecs: best.dur}
	})
}

// parseAigeiDuration 解析爱给网的时长字符串（"M:SS" 或数字秒）。
func parseAigeiDuration(s string) float64 {
	if s == "" {
		return 0
	}
	if idx := strings.Index(s, ":"); idx >= 0 {
		min, _ := strconv.ParseFloat(s[:idx], 64)
		sec, _ := strconv.ParseFloat(s[idx+1:], 64)
		return min*60 + sec
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
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
// generateAudioLDMForTag 调用本地 AudioLDM HTTP API 生成音效并上传至 OSS。
//
// 支持三种响应格式（自动检测）：
//  1. JSON {"url": "http://...", "duration": 5.0}          — 远端 URL，直接使用
//  2. JSON {"audio_base64": "BASE64WAV", "duration": 5.0}  — base64 编码音频，解码后上传
//  3. 原始音频字节（Content-Type: audio/*）                 — 直接上传
//
// 请求格式：POST endpoint  {"text": "...", "duration": 5.0}
// 若设置了 audioLDMKey，则附加 Authorization: Bearer {key}
func (s *SFXService) generateAudioLDMForTag(ctx context.Context, item sfxTagItem, shot *model.StoryboardShot) (string, float64, error) {
	if s.audioLDMEndpoint == "" {
		return "", 0, fmt.Errorf("audioldm endpoint not configured")
	}

	// AudioLDM2 在英文 prompt 上效果最好，且部分实现有 ASCII-only 校验。
	// 优先使用英文 tag（Freesound 四元格式），仅当 tag 为空时才降级到中文 prompt。
	prompt := item.Tag
	if prompt == "" {
		prompt = item.Prompt
	}

	dur := shot.Duration
	if item.SFXType != "ambient" && dur > 10 {
		dur = 10 // AudioLDM 通常支持最长 10s
	}
	if dur <= 0 {
		dur = 5
	}

	// AudioLDM2 标准 API 字段名：prompt（不是 text）、duration（秒，浮点）
	// 同时兼容部分实现用 text 字段的情况（两个字段都发送）
	body, _ := json.Marshal(map[string]interface{}{
		"prompt":   prompt,
		"text":     prompt, // 兼容旧版实现
		"duration": dur,
	})

	// 确保 endpoint 末尾有斜杠，避免 307 重定向浪费一次 RTT
	endpoint := s.audioLDMEndpoint
	if !strings.HasSuffix(endpoint, "/") {
		endpoint += "/"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.audioLDMKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.audioLDMKey)
	}

	// 本地调用通常较慢（模型推理），超时延长至 3 分钟
	ldmClient := &http.Client{Timeout: 3 * time.Minute}
	resp, err := ldmClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("audioldm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", 0, fmt.Errorf("audioldm HTTP %d: %s", resp.StatusCode, b)
	}

	ct := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024)) // 最大 32MB
	if err != nil {
		return "", 0, fmt.Errorf("audioldm read response: %w", err)
	}

	logger.Printf("[SFXService] AudioLDM 200 OK: tag=%q ct=%q bodyLen=%d", item.Tag, ct, len(data))

	var audioData []byte
	mimeType := "audio/wav"
	actualDur := dur

	if strings.HasPrefix(ct, "audio/") {
		// 格式 3：原始音频字节（Content-Type: audio/wav 等）
		audioData = data
		mimeType = ct
		logger.Printf("[SFXService] AudioLDM format=raw_audio mime=%q len=%d", ct, len(data))
	} else {
		// 尝试解析 JSON（格式 1、2 或异步任务）
		// 兼容多种 AudioLDM2 实现的字段名：url / audio_base64 / audio / audio_data
		var jsonResp struct {
			URL         string  `json:"url"`
			Duration    float64 `json:"duration"`
			AudioBase64 string  `json:"audio_base64"`
			Audio       string  `json:"audio"`      // 别名 1
			AudioData   string  `json:"audio_data"` // AudioLDM2 标准字段
			// 异步任务格式（部分实现）
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(data, &jsonResp); err != nil {
			return "", 0, fmt.Errorf("audioldm parse response: %w — body: %.300s", err, data)
		}

		// 格式 4：异步任务 — 服务返回 task_id + status=processing，需轮询结果
		if jsonResp.TaskID != "" && (jsonResp.Status == "processing" || jsonResp.Status == "pending") {
			logger.Printf("[SFXService] AudioLDM async task_id=%s, polling for result...", jsonResp.TaskID)
			polledData, pollErr := s.pollAudioLDMTask(ctx, jsonResp.TaskID)
			if pollErr != nil {
				return "", 0, fmt.Errorf("audioldm poll task_id=%s: %w", jsonResp.TaskID, pollErr)
			}
			// 替换 data，继续走后面的 URL / base64 解析逻辑
			data = polledData
			if err2 := json.Unmarshal(data, &jsonResp); err2 != nil {
				return "", 0, fmt.Errorf("audioldm poll parse: %w — body: %.300s", err2, data)
			}
		}

		if jsonResp.Duration > 0 {
			actualDur = jsonResp.Duration
		}
		if jsonResp.URL != "" {
			// 格式 1：已有 URL，直接返回（不需要 OSS）
			logger.Printf("[SFXService] AudioLDM format=url url=%s dur=%.1f", jsonResp.URL, actualDur)
			return jsonResp.URL, actualDur, nil
		}
		// 格式 2：base64 编码音频（audio_base64 / audio / audio_data）
		b64 := jsonResp.AudioBase64
		if b64 == "" {
			b64 = jsonResp.Audio
		}
		if b64 == "" {
			b64 = jsonResp.AudioData
		}
		if b64 == "" {
			return "", 0, fmt.Errorf("audioldm: no audio in response (checked url/audio_base64/audio/audio_data) — body: %.300s", data)
		}
		logger.Printf("[SFXService] AudioLDM format=base64 b64Len=%d", len(b64))
		audioData, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			// 尝试 URL-safe base64
			audioData, err = base64.URLEncoding.DecodeString(b64)
			if err != nil {
				return "", 0, fmt.Errorf("audioldm base64 decode: %w", err)
			}
		}
	}

	// 上传到 OSS
	if s.storageSvc == nil {
		return "", 0, fmt.Errorf("storage not configured; cannot save audioldm audio (len=%d)", len(audioData))
	}
	tagHash := fmt.Sprintf("%x", len(item.Tag)*31+len(item.SFXType))
	ext := ".wav"
	if strings.Contains(mimeType, "mpeg") || strings.Contains(mimeType, "mp3") {
		ext = ".mp3"
		mimeType = "audio/mpeg"
	}
	key := fmt.Sprintf("sfx/video_%d/shot_%d_%s_ldm%s", shot.VideoID, shot.ID, tagHash, ext)
	logger.Printf("[SFXService] AudioLDM uploading: key=%s audioLen=%d", key, len(audioData))
	u, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(audioData), int64(len(audioData)), mimeType)
	if err != nil {
		return "", 0, fmt.Errorf("audioldm upload: %w", err)
	}
	logger.Printf("[SFXService] AudioLDM upload success: url=%s", u)
	return u, actualDur, nil
}

// pollAudioLDMTask polls GET {base}/{taskID} until status != "processing"/"pending",
// returning the final JSON body. Polls every 2s, times out after 5 minutes.
func (s *SFXService) pollAudioLDMTask(ctx context.Context, taskID string) ([]byte, error) {
	// Derive base URL from endpoint (strip path, use scheme+host only)
	parsedURL, err := url.Parse(s.audioLDMEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	pollURL := fmt.Sprintf("%s://%s/%s", parsedURL.Scheme, parsedURL.Host, taskID)

	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	ldmClient := &http.Client{Timeout: 30 * time.Second}
	for {
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("timeout waiting for audioldm task")
		case <-ticker.C:
			req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, pollURL, nil)
			if err != nil {
				return nil, err
			}
			if s.audioLDMKey != "" {
				req.Header.Set("Authorization", "Bearer "+s.audioLDMKey)
			}
			resp, err := ldmClient.Do(req)
			if err != nil {
				logger.Printf("[SFXService] AudioLDM poll task_id=%s error: %v", taskID, err)
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				logger.Printf("[SFXService] AudioLDM poll task_id=%s HTTP %d", taskID, resp.StatusCode)
				continue
			}
			// Check status field
			var status struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &status); err != nil {
				// Not JSON — might be raw audio bytes, return as-is
				return body, nil
			}
			if status.Status == "processing" || status.Status == "pending" {
				logger.Printf("[SFXService] AudioLDM poll task_id=%s still %s", taskID, status.Status)
				continue
			}
			logger.Printf("[SFXService] AudioLDM poll task_id=%s done status=%q bodyLen=%d", taskID, status.Status, len(body))
			return body, nil
		}
	}
}

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

// mapSFXSource 将音效来源映射为素材库 Asset.Source 枚举值。
func mapSFXSource(source string) string {
	switch source {
	case "local":
		return "uploaded"
	case "freesound", "pixabay", "bbc-sfx", "aigei":
		return "crawled"
	default: // ai-sfx, elevenlabs
		return "platform"
	}
}

// saveToAssetLibrary 将已生成的音效批量存入素材库，并关联对应标签。
// 以 URL 作为 ExternalID 去重：同一 URL 不重复入库。
// 失败只记录日志，不影响主流程。
func (s *SFXService) saveToAssetLibrary(ctx context.Context, shot *model.StoryboardShot, items []*model.ShotSFXItem, tenantID uint) {
	videoID := shot.VideoID
	shotID := shot.ID

	for _, item := range items {
		if item.URL == "" {
			continue
		}
		// 去重：同 URL 已存在则跳过
		if exists, err := s.assetRepo.ExistsByExternalID(item.URL); err != nil {
			logger.Printf("[SFXService] AssetLib: ExistsByExternalID error (shot %d tag=%q): %v", shotID, item.Tag, err)
			continue
		} else if exists {
			continue
		}

		// 构建 Asset 记录
		asset := &model.Asset{
			TenantID:   tenantID,
			CreatorID:  tenantID, // personal scope 查询需要 creator_id 匹配
			Scope:      model.AssetScopePersonal,
			Type:       "audio",
			SubType:    "sfx",
			Title:      item.Tag,
			StorageURL: item.URL,
			ExternalID: item.URL,
			Source:     mapSFXSource(item.Source),
			Duration:   item.DurationSecs,
			VideoID:    &videoID,
			ShotID:     &shotID,
			Status:     "active",
		}
		if err := s.assetRepo.Create(asset); err != nil {
			logger.Printf("[SFXService] AssetLib: create asset failed (shot %d tag=%q): %v", shotID, item.Tag, err)
			continue
		}

		// 关联标签：搜索词 + SFX 类型
		tagNames := []string{item.Tag}
		if item.SFXType != "" && item.SFXType != item.Tag {
			tagNames = append(tagNames, item.SFXType)
		}
		for _, name := range tagNames {
			tag, err := s.tagRepo.FindOrCreate(name, "audio")
			if err != nil {
				logger.Printf("[SFXService] AssetLib: FindOrCreate tag %q failed: %v", name, err)
				continue
			}
			if err := s.tagRepo.AddToAsset(asset.ID, tag.ID, "ai", 1.0); err != nil {
				logger.Printf("[SFXService] AssetLib: AddToAsset failed (asset %d tag %q): %v", asset.ID, name, err)
			}
		}
		logger.Printf("[SFXService] AssetLib: saved asset %d tag=%q source=%s", asset.ID, item.Tag, item.Source)
	}
}
