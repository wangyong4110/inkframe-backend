package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	noCache      bool    // true = 不缓存此结果（CDN 临时链接未持久化到存储）
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
	httpClient       *http.Client
	localLib         map[string]string // 内置标签 → 文件名（不含目录）
	localUploadCache sync.Map          // local file path → OSS URL（进程内缓存）
	queryCache       sync.Map          // "source:query" → sfxCacheEntry
	elevenLabsSem    chan struct{}      // 限制 ElevenLabs 并发数（免费版最多 4 路）
}

// WithSFXItemRepo 注入音效条目仓库（可选；注入后才启用多 item 存储）
func (s *SFXService) WithSFXItemRepo(r *repository.ShotSFXItemRepository) *SFXService {
	s.sfxItemRepo = r
	return s
}

// ListSFXItems 返回分镜的所有音效条目（供合成路径使用）
func (s *SFXService) ListSFXItems(shotID uint) ([]*model.ShotSFXItem, error) {
	if s.sfxItemRepo == nil {
		return nil, nil
	}
	return s.sfxItemRepo.ListByShotID(shotID)
}

// WithAssetRepo 注入素材库仓库（可选；注入后生成的音效将自动存入素材库）
func (s *SFXService) WithAssetRepo(assetRepo *repository.AssetRepository, tagRepo *repository.TagRepository) *SFXService {
	s.assetRepo = assetRepo
	s.tagRepo = tagRepo
	return s
}

// SFXServiceConfig SFXService 构造参数
type SFXServiceConfig struct {
	SFXDir   string // 本地音效目录（环境变量 SFX_DIR）
	ProxyURL string // 爬取代理（环境变量 CRAWL_PROXY_URL）；为空则使用系统 HTTPS_PROXY
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
		httpClient:     buildCrawlHTTPClient(cfg.ProxyURL, 30*time.Second),
		localLib:       buildDefaultSFXLib(),
		elevenLabsSem:  make(chan struct{}, 3), // 保守限制 3 路并发，避免触发 ElevenLabs 429
	}
}

// sfxProviderCreds 从 DB 取指定 sfx 供应商的凭据；aiSvc 为 nil 时返回空。
func (s *SFXService) sfxProviderCreds(tenantID uint, name string) (apiKey, endpoint string) {
	if s.aiSvc == nil {
		return "", ""
	}
	return s.aiSvc.GetSFXProviderCreds(tenantID, name)
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

// UpdateShotSFXTagsPublic 更新单个镜头的 sfx_tags（handler 层专用，接受公开类型）。
func (s *SFXService) UpdateShotSFXTagsPublic(shotID uint, tags []SFXTagItemPublic) error {
	items := make([]sfxTagItem, 0, len(tags))
	for _, t := range tags {
		items = append(items, sfxTagItem{Tag: t.Tag, SFXType: t.SFXType, Prompt: t.Prompt})
	}
	return s.UpdateShotSFXTags(shotID, items)
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
// provider 非空时强制使用指定提供商（如 "elevenlabs-sfx"），为空则走默认降级链。
func (s *SFXService) AutoGenerateSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint, provider string) error {
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
		hit := s.searchOneTag(ctx, tenantID, item, maxDur, shot, provider)
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
// 优先级：素材库 → AI 文生音效 → 本地库 → AudioLDM（本地模型）→ Freesound → Pixabay → BBC Sound Effects → ElevenLabs。
// 同一 tag 的结果在进程内按 24h TTL 缓存，批量生成时相同 tag 的分镜共享同一条音效。
// provider 非空时强制使用指定提供商，并在 cacheKey 中区分，避免跨提供商的缓存污染。
func (s *SFXService) searchOneTag(ctx context.Context, tenantID uint, item sfxTagItem, maxDur float64, shot *model.StoryboardShot, provider string) sfxHit {
	cacheKey := "onetag:" + provider + ":" + normalizeTag(item.Tag)
	return s.cachedQuery(cacheKey, func() sfxHit {
		return s.searchOneTagUncached(ctx, tenantID, item, maxDur, shot, provider)
	})
}

// searchOneTagUncached 是 searchOneTag 的无缓存实现。
func (s *SFXService) searchOneTagUncached(ctx context.Context, tenantID uint, item sfxTagItem, maxDur float64, shot *model.StoryboardShot, provider string) sfxHit {
	sfxDur := maxDur
	if sfxDur <= 0 {
		sfxDur = 5
	}
	// 强制提供商：跳过降级链，直接使用指定提供商生成
	if provider == "elevenlabs-sfx" {
		if u, dur, err := s.generateElevenLabsForTag(ctx, tenantID, item, shot); err == nil && u != "" {
			logger.Printf("[SFXService] shot %d ElevenLabs(forced) hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
			return sfxHit{url: u, source: "elevenlabs", durationSecs: dur}
		} else if err != nil {
			logger.Printf("[SFXService] shot %d ElevenLabs(forced) failed tag=%q: %v", shot.ID, item.Tag, err)
		}
		return sfxHit{}
	}
	// 0. 素材库（已保存的音效，优先复用避免重复生成）
	if s.assetRepo != nil {
		assets, _, err := s.assetRepo.Search(repository.AssetSearchParams{
			Type:     "audio",
			SubType:  "sfx",
			Q:        item.Tag,
			Scope:    "all",
			CallerID: tenantID,
			Sort:     "use_count",
			PageSize: 3,
		})
		if err == nil {
			for _, a := range assets {
				if a.StorageURL == "" {
					continue
				}
				// 跳过 platform（ai-sfx）来源的外部 https CDN URL——可能已过期
				if a.Source == "platform" && strings.HasPrefix(a.StorageURL, "https://") {
					continue
				}
				logger.Printf("[SFXService] shot %d asset-lib hit tag=%q (%.1fs)", shot.ID, item.Tag, a.Duration)
				_ = s.assetRepo.IncrUseCount(a.ID)
				return sfxHit{url: a.StorageURL, source: mapSFXSource(a.Source), durationSecs: a.Duration}
			}
		}
	}
	// 1. AI 文生音效（Kling SFX 等 sfx 类型提供商）——优先尝试
	// 优先使用 Prompt（中文自然语言描述，信息更丰富），降级到英文搜索词 Tag
	if s.aiSvc != nil {
		aiPrompt := item.Prompt
		if aiPrompt == "" {
			aiPrompt = item.Tag
		}
		if u, dur, err := s.aiSvc.GenerateSFX(ctx, tenantID, aiPrompt, sfxDur); err == nil && u != "" {
			// Kling SFX 等返回 CDN 临时链接（24~48h 后过期）；
			// 生成后立即下载并上传存储，保证长期可访问。
			// 上传成功 → 永久 URL，可以缓存；上传失败 → 继续使用 CDN URL，但标记不缓存。
			noCacheFlag := false
			if s.storageSvc != nil && strings.HasPrefix(u, "https://") {
				ossKey := fmt.Sprintf("sfx/video_%d/shot_%d_ai.mp3", shot.VideoID, shot.ID)
				if ossURL, uploadErr := downloadURLAndUploadToOSS(ctx, s.storageSvc, u, ossKey); uploadErr == nil {
					u = ossURL
				} else {
					logger.Printf("[SFXService] shot %d AI-SFX upload failed (using CDN URL, noCache): %v", shot.ID, uploadErr)
					noCacheFlag = true
				}
			} else if strings.HasPrefix(u, "https://") {
				// 无存储服务，CDN 临时链接不缓存
				noCacheFlag = true
			}
			logger.Printf("[SFXService] shot %d AI-SFX hit tag=%q (%.1fs) noCache=%v", shot.ID, item.Tag, dur, noCacheFlag)
			return sfxHit{url: u, source: "ai-sfx", durationSecs: dur, noCache: noCacheFlag}
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
	if u, dur, err := s.generateAudioLDMForTag(ctx, tenantID, item, shot); err == nil && u != "" {
		logger.Printf("[SFXService] shot %d AudioLDM hit tag=%q (%.1fs)", shot.ID, item.Tag, dur)
		return sfxHit{url: u, source: "audioldm", durationSecs: dur}
	} else if err != nil && !strings.Contains(err.Error(), "audioldm not configured") {
		logger.Printf("[SFXService] shot %d AudioLDM failed tag=%q: %v", shot.ID, item.Tag, err)
	}
	// 4. Freesound API（CC0，需 API Key）
	if hit := s.searchFreesound(ctx, tenantID, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Freesound hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
	}
	// 5. Pixabay Audio（CC0，需 API Key）
	if hit := s.searchPixabay(ctx, tenantID, item, maxDur); hit.url != "" {
		logger.Printf("[SFXService] shot %d Pixabay hit tag=%q (%.1fs)", shot.ID, item.Tag, hit.durationSecs)
		return hit
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
	if u, dur, err := s.generateElevenLabsForTag(ctx, tenantID, item, shot); err == nil && u != "" {
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
// provider 非空时强制使用指定提供商（如 "elevenlabs-sfx"），为空则走默认降级链。
// progressFn 每完成一个镜头时实时调用（0-100）。
func (s *SFXService) BatchAutoGenerateSFX(
	ctx context.Context,
	shots []*model.StoryboardShot,
	tenantID uint,
	userContext string,
	provider string,
	progressFn func(int),
) (success, fail int, failedShotIDs []uint) {
	total := len(shots)
	if total == 0 {
		return
	}
	const maxConcurrency = 10
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var doneCount atomic.Int32
	var successCount, failCount atomic.Int32
	var mu sync.Mutex

	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(s2 *model.StoryboardShot) {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.AutoGenerateSFX(ctx, s2, tenantID, provider)
			if err != nil {
				logger.Printf("[SFXService] shot %d: %v", s2.ID, err)
				failCount.Add(1)
				mu.Lock()
				failedShotIDs = append(failedShotIDs, s2.ID)
				mu.Unlock()
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

	return int(successCount.Load()), int(failCount.Load()), failedShotIDs
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
	if hit.url != "" && !hit.noCache {
		s.queryCache.Store(cacheKey, sfxCacheEntry{
			url: hit.url, source: hit.source, durationSecs: hit.durationSecs,
			expiresAt: time.Now().Add(sfxCacheTTL),
		})
	}
	return hit
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

// saveToAssetLibrary 将已生成的音效批量存入公共素材库，并关联对应标签。
// 以 URL 作为 ExternalID 去重：同一 URL 不重复入库。
// 失败只记录日志，不影响主流程。
func (s *SFXService) saveToAssetLibrary(ctx context.Context, shot *model.StoryboardShot, items []*model.ShotSFXItem, _ uint) {
	shotID := shot.ID

	for _, item := range items {
		if item.URL == "" {
			continue
		}
		// ai-sfx 来源：仅当 URL 已持久化（相对路径 /api/ 或 /uploads/，而非外部 CDN https://）时才入库。
		// CDN 临时链接过期后会导致素材库命中返回失效 URL，跳过入库避免污染。
		if item.Source == "ai-sfx" && strings.HasPrefix(item.URL, "https://") {
			continue
		}
		// 去重：同 URL 已存在则跳过
		if exists, err := s.assetRepo.ExistsByExternalID(item.URL); err != nil {
			logger.Printf("[SFXService] AssetLib: ExistsByExternalID error (shot %d tag=%q): %v", shotID, item.Tag, err)
			continue
		} else if exists {
			continue
		}

		// 构建 Asset 记录（公共素材库，所有租户可复用）
		asset := &model.Asset{
			TenantID:   0,
			CreatorID:  0,
			Scope:      model.AssetScopePublic,
			Type:       "audio",
			SubType:    "sfx",
			Title:      item.Tag,
			StorageURL: item.URL,
			ExternalID: item.URL,
			Source:     mapSFXSource(item.Source),
			Duration:   item.DurationSecs,
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
