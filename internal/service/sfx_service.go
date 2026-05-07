package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// SFXService 自动音效生成服务。
// 五层降级：本地 SFX 库 → Freesound API → Pixabay API → Jamendo API → ElevenLabs（AI生成）。
// 各层均可选，未配置时透明跳过。
type SFXService struct {
	aiSvc           *AIService
	storageSvc      storage.Service
	storyboardRepo  *repository.StoryboardRepository
	sfxItemRepo     *repository.ShotSFXItemRepository
	sfxDir          string            // 本地音效目录
	freesoundKey    string            // Freesound API Token（可选）
	pixabayKey      string            // Pixabay API Key（可选）
	jamendoClientID string            // Jamendo client_id（可选）
	elevenKey       string            // ElevenLabs API Key（可选）
	httpClient      *http.Client
	localLib        map[string]string // 内置标签 → 文件名（不含目录）
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
	PixabayKey      string // 环境变量 PIXABAY_API_KEY
	JamendoClientID string // 环境变量 JAMENDO_CLIENT_ID
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
		aiSvc:           aiSvc,
		storageSvc:      storageSvc,
		storyboardRepo:  storyboardRepo,
		sfxDir:          cfg.SFXDir,
		freesoundKey:    cfg.FreesoundKey,
		pixabayKey:      cfg.PixabayKey,
		jamendoClientID: cfg.JamendoClientID,
		elevenKey:       cfg.ElevenLabsKey,
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
		"heartbeat":    "heartbeat.wav",
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
		// 该镜头无需音效
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
	type result struct {
		shotNo int
		err    error
	}
	results := make(chan result, len(shots))

	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		go func(sh *model.StoryboardShot) {
			defer func() { <-sem }()
			err := s.analyzeSingleShotSFX(ctx, sh, tenantID, userContext)
			results <- result{shotNo: sh.ShotNo, err: err}
		}(shot)
	}
	// 等待所有 goroutine 完成
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
	close(results)

	updated, failed := 0, 0
	for r := range results {
		if r.err != nil {
			logger.Printf("[SFXService] AnalyzeSFXForVideo: shot %d failed: %v", r.shotNo, r.err)
			failed++
		} else {
			updated++
		}
	}
	logger.Printf("[SFXService] AnalyzeSFXForVideo: updated=%d failed=%d", updated, failed)
	return nil
}

// AutoGenerateSFX 为单个镜头自动选取/生成音效，每个 tag 独立搜索，写入多条 ShotSFXItem。
// 若该镜头已有音效条目，直接跳过（幂等）。
func (s *SFXService) AutoGenerateSFX(ctx context.Context, shot *model.StoryboardShot, tenantID uint) error {
	// 幂等检测：优先用新表；无仓库时降级用旧字段
	if s.sfxItemRepo != nil {
		count, _ := s.sfxItemRepo.CountByShotID(shot.ID)
		if count > 0 {
			return nil
		}
	} else if shot.SFXURL != "" {
		return nil
	}

	// 1. 提取标签
	var tags []string
	if shot.SFXTags != "" {
		if err := json.Unmarshal([]byte(shot.SFXTags), &tags); err != nil || len(tags) == 0 {
			tags = nil
		}
	}
	if len(tags) == 0 {
		var err error
		tags, err = s.extractTags(ctx, shot, tenantID)
		if err != nil {
			logger.Printf("[SFXService] shot %d LLM tag extract failed (%v), using rule fallback", shot.ID, err)
			tags = s.fallbackTags(shot)
		}
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
	}
	var results []sfxResult

	for _, tag := range tags {
		u, src := s.searchOneTag(ctx, []string{tag}, maxDur, shot)
		if u != "" {
			results = append(results, sfxResult{tag: tag, url: u, source: src})
			logger.Printf("[SFXService] shot %d tag=%q source=%s url=%s", shot.ID, tag, src, u)
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
			// 主音效音量 = baseVol；后续音效递减 0.1（最低 0.1）
			vol := baseVol - float64(i)*0.1
			if vol < 0.1 {
				vol = 0.1
			}
			items = append(items, &model.ShotSFXItem{
				ShotID: shot.ID,
				SeqNo:  i + 1,
				Tag:    r.tag,
				URL:    r.url,
				Volume: vol,
				Source: r.source,
			})
		}
		if err := s.sfxItemRepo.BatchCreate(items); err != nil {
			return fmt.Errorf("save sfx items shot %d: %w", shot.ID, err)
		}
		// 同步更新旧字段（向后兼容时间线播放）
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].url, string(tagsJSON), baseVol)
	} else {
		_ = s.storyboardRepo.UpdateSFX(shot.ID, results[0].url, string(tagsJSON), baseVol)
	}
	return nil
}

// searchOneTag 对单个 tag 执行五层降级搜索，返回 (url, source)。
// 顺序：本地库 → Freesound → Pixabay → Jamendo → ElevenLabs
func (s *SFXService) searchOneTag(ctx context.Context, tags []string, maxDur float64, shot *model.StoryboardShot) (string, string) {
	if u := s.searchLocalLib(tags); u != "" {
		logger.Printf("[SFXService] shot %d local hit: %s", shot.ID, u)
		return u, "local"
	}
	if s.freesoundKey == "" {
		logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
	} else if u := s.searchFreesound(ctx, tags, maxDur); u != "" {
		logger.Printf("[SFXService] shot %d Freesound hit: %s", shot.ID, u)
		return u, "freesound"
	} else {
		logger.Printf("[SFXService] shot %d Freesound miss", shot.ID)
	}
	if s.pixabayKey == "" {
		logger.Printf("[SFXService] shot %d skip Pixabay (no key)", shot.ID)
	} else if u := s.searchPixabay(ctx, tags, maxDur); u != "" {
		logger.Printf("[SFXService] shot %d Pixabay hit: %s", shot.ID, u)
		return u, "pixabay"
	} else {
		logger.Printf("[SFXService] shot %d Pixabay miss", shot.ID)
	}
	if s.jamendoClientID != "" {
		if u := s.searchJamendo(ctx, tags, maxDur); u != "" {
			logger.Printf("[SFXService] shot %d Jamendo hit: %s", shot.ID, u)
			return u, "jamendo"
		}
		logger.Printf("[SFXService] shot %d Jamendo miss", shot.ID)
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
// progressFn 每完成一个镜头调用一次（0-100）。
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
	type result struct{ ok bool }
	results := make(chan result, total)

	for _, shot := range shots {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		go func(s2 *model.StoryboardShot) {
			defer func() { <-sem }()
			err := s.AutoGenerateSFX(ctx, s2, tenantID)
			if err != nil {
				logger.Printf("[SFXService] shot %d: %v", s2.ID, err)
			}
			results <- result{ok: err == nil}
		}(shot)
	}
	// drain remaining sem slots so all goroutines finish
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
	close(results)

	done := 0
	for r := range results {
		done++
		if r.ok {
			success++
		} else {
			fail++
		}
		if progressFn != nil {
			progressFn(done * 100 / total)
		}
	}
	return
}

// --- 内部方法 ---

// sfxPrompt 构建 LLM 提取音效标签的 Prompt
func sfxPrompt(shot *model.StoryboardShot) string {
	var sb strings.Builder
	sb.WriteString("你是专业影视音效师。根据以下分镜信息，输出3-5个英文音效标签（JSON数组）。\n")
	sb.WriteString("要求：标签具体可搜索（如 \"rain_heavy\"）；优先环境音 > 动作音 > 情绪音；仅输出JSON数组。\n\n")
	if shot.Scene != "" {
		fmt.Fprintf(&sb, "场景类型：%s\n", shot.Scene)
	}
	if shot.EmotionalTone != "" {
		fmt.Fprintf(&sb, "情绪基调：%s\n", shot.EmotionalTone)
	}
	if shot.Description != "" {
		fmt.Fprintf(&sb, "镜头描述：%s\n", shot.Description)
	}
	if shot.Dialogue != "" {
		fmt.Fprintf(&sb, "台词（仅参考场景环境，无需为台词生成音效）：%s\n", shot.Dialogue)
	}
	sb.WriteString("\n输出格式示例：[\"rain_heavy\", \"footsteps_stone\", \"thunder\"]")
	return sb.String()
}

// extractTags 调用 LLM 提取音效标签列表
func (s *SFXService) extractTags(ctx context.Context, shot *model.StoryboardShot, tenantID uint) ([]string, error) {
	prompt := sfxPrompt(shot)
	result, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "sfx_extract", prompt, "")
	if err != nil {
		return nil, err
	}
	raw := extractJSON(result)
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, fmt.Errorf("parse SFX tags JSON: %w (raw=%q)", err, raw)
	}
	return tags, nil
}

// fallbackTags 基于规则从描述 / 情绪基调 / 镜头类型推断标签（LLM 不可用时的降级）
func (s *SFXService) fallbackTags(shot *model.StoryboardShot) []string {
	desc := strings.ToLower(shot.Description + " " + shot.EmotionalTone + " " + shot.Scene)
	rules := [][2]string{
		{"雨", "rain_heavy"}, {"雪", "wind_night"}, {"风", "wind_strong"},
		{"雷", "thunder"}, {"森林", "forest_ambient"}, {"河", "river_flowing"},
		{"城市", "city_ambient"}, {"人群", "crowd_outdoor"}, {"室内", "ambient_room"},
		{"战斗", "sword_clash"}, {"奔跑", "footsteps_running"},
		{"马", "horse_gallop"}, {"爆炸", "explosion"}, {"箭", "arrow_whoosh"},
		{"门", "door_open"}, {"火", "fire_crackle"},
		{"紧张", "heartbeat"}, {"钟", "clock_ticking"}, {"铃", "bell_ring"},
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
		tags = []string{"ambient_room"}
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

// searchLocalLib 在本地目录中查找首个匹配标签的音效文件，返回 file:// URL
func (s *SFXService) searchLocalLib(tags []string) string {
	if s.sfxDir == "" {
		return ""
	}
	for _, rawTag := range tags {
		tag := normalizeTag(rawTag)
		// 精确匹配
		if filename, ok := s.localLib[tag]; ok {
			p := filepath.Join(s.sfxDir, filename)
			if _, err := os.Stat(p); err == nil {
				return "file://" + p
			}
		}
		// 前缀匹配：标签 "rain_light" → 匹配库中所有 rain_* 条目
		prefix := strings.SplitN(tag, "_", 2)[0]
		for libTag, filename := range s.localLib {
			if strings.HasPrefix(libTag, prefix) {
				p := filepath.Join(s.sfxDir, filename)
				if _, err := os.Stat(p); err == nil {
					return "file://" + p
				}
			}
		}
	}
	return ""
}

// freesoundSearch 执行单次 Freesound API 搜索，返回首个结果的预览 MP3 URL。
// maxDuration > 0 时附加时长过滤（秒），并按下载量降序排列。
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
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer resp.Body.Close()

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

// searchFreesound 通过 Freesound API 搜索 CC0 授权音效，返回高质量预览 MP3 URL。
// maxDuration > 0 时对搜索结果施加时长上限。
// 先用前3个标签合并搜索，失败则逐个标签降级重试。
// 需要 FreesoundKey 配置。
func (s *SFXService) searchFreesound(ctx context.Context, tags []string, maxDuration float64) string {
	if s.freesoundKey == "" || len(tags) == 0 {
		return ""
	}
	// 第一次：取前3个标签合并搜索（效果更精准）
	n := len(tags)
	if n > 3 {
		n = 3
	}
	if u := s.freesoundSearch(ctx, strings.Join(tags[:n], " "), maxDuration); u != "" {
		return u
	}
	// 降级：逐个标签单独搜索（将下划线替换为空格，更符合 Freesound 关键词习惯）
	for _, tag := range tags {
		q := strings.ReplaceAll(normalizeTag(tag), "_", " ")
		if u := s.freesoundSearch(ctx, q, maxDuration); u != "" {
			return u
		}
	}
	return ""
}

// searchPixabay 通过 Pixabay API 搜索免费音效，返回 CDN 直链 MP3 URL。
// 先用标签合并搜索，失败则逐个标签降级重试。
// API 文档：https://pixabay.com/api/docs/ (media_type=music)
func (s *SFXService) searchPixabay(ctx context.Context, tags []string, maxDuration float64) string {
	if s.pixabayKey == "" || len(tags) == 0 {
		return ""
	}

	doSearch := func(query string) string {
		params := url.Values{}
		params.Set("key", s.pixabayKey)
		params.Set("q", query)
		params.Set("media_type", "music")
		params.Set("lang", "en")
		params.Set("order", "popular")
		params.Set("per_page", "10")
		params.Set("safesearch", "true")

		req, err := http.NewRequestWithContext(ctx, "GET", "https://pixabay.com/api/?"+params.Encode(), nil)
		if err != nil {
			return ""
		}
		resp, err := s.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return ""
		}
		defer resp.Body.Close()

		var result struct {
			Hits []struct {
				Duration int    `json:"duration"`
				AudioURL string `json:"audio_url"`
			} `json:"hits"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return ""
		}
		for _, hit := range result.Hits {
			if hit.AudioURL == "" {
				continue
			}
			if maxDuration > 0 && float64(hit.Duration) > maxDuration {
				continue
			}
			return hit.AudioURL
		}
		return ""
	}

	// 第一次：前 3 个标签合并搜索
	n := len(tags)
	if n > 3 {
		n = 3
	}
	query := strings.ReplaceAll(strings.Join(tags[:n], " "), "_", " ")
	if u := doSearch(query); u != "" {
		return u
	}
	// 降级：逐个标签单独搜索
	for _, tag := range tags {
		q := strings.ReplaceAll(normalizeTag(tag), "_", " ")
		if u := doSearch(q); u != "" {
			return u
		}
	}
	return ""
}

// searchJamendo 通过 Jamendo API 搜索免费器乐背景音效，返回可下载 MP3 URL。
// 先用标签合并搜索，失败则逐个标签降级重试。
// 需要 JamendoClientID 配置。
func (s *SFXService) searchJamendo(ctx context.Context, tags []string, maxDuration float64) string {
	if s.jamendoClientID == "" || len(tags) == 0 {
		return ""
	}

	// 内部搜索函数：单次请求
	doSearch := func(query string) string {
		params := url.Values{}
		params.Set("client_id", s.jamendoClientID)
		params.Set("format", "json")
		params.Set("limit", "5")
		params.Set("namesearch", query)
		params.Set("vocalinstrumental", "instrumental")
		params.Set("order", "popularity_month")
		if maxDuration > 0 {
			params.Set("durationbetween", fmt.Sprintf("1_%d", int(maxDuration)))
		}
		apiURL := "https://api.jamendo.com/v3.0/tracks/?" + params.Encode()
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return ""
		}
		resp, err := s.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return ""
		}
		defer resp.Body.Close()

		var result struct {
			Results []struct {
				Audio                string `json:"audio"`
				AudioDownload        string `json:"audiodownload"`
				AudioDownloadAllowed bool   `json:"audiodownload_allowed"`
				Duration             int    `json:"duration"`
			} `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
			return ""
		}
		for _, track := range result.Results {
			if track.AudioDownloadAllowed && track.AudioDownload != "" {
				return track.AudioDownload
			}
			if track.Audio != "" {
				return track.Audio
			}
		}
		return ""
	}

	// 先合并前3个标签搜索
	n := len(tags)
	if n > 3 {
		n = 3
	}
	query := strings.Join(tags[:n], " ")
	query = strings.ReplaceAll(query, "_", " ")
	if u := doSearch(query); u != "" {
		return u
	}
	// 降级：逐个标签单独搜索
	for _, tag := range tags {
		q := strings.ReplaceAll(normalizeTag(tag), "_", " ")
		if u := doSearch(q); u != "" {
			return u
		}
	}
	return ""
}

// elevenLabsPrompt 将 sfx_tags 转换为适合 ElevenLabs 音效生成的英文 Prompt。
// 优先使用 sfx_tags（语义更准确），降级用镜头描述的前缀。
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
	// 降级：截取镜头描述
	runes := []rune(shot.Description)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return shot.Description
}

// generateElevenLabs 调用 ElevenLabs Sound Generation API，
// 根据音效标签生成定制音效，上传至存储服务后返回公开 URL。
// 需要 ElevenLabsKey + storageSvc 配置。
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

	// 时长范围：ElevenLabs 支持 0.5-22 秒
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
		"prompt_influence": 0.3, // 0=随机创意，1=完全按Prompt
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
		return "", fmt.Errorf("elevenlabs HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	key := fmt.Sprintf("sfx/video_%d/shot_%d.mp3", shot.VideoID, shot.ID)
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}
