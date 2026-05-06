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
// 四层降级：本地 SFX 库 → Freesound API → Jamendo API → ElevenLabs（AI生成）。
// 各层均可选，未配置时透明跳过。
type SFXService struct {
	aiSvc           *AIService
	storageSvc      storage.Service
	storyboardRepo  *repository.StoryboardRepository
	sfxDir          string            // 本地音效目录
	freesoundKey    string            // Freesound API Token（可选）
	jamendoClientID string            // Jamendo client_id（可选）
	elevenKey       string            // ElevenLabs API Key（可选）
	httpClient      *http.Client
	localLib        map[string]string // 内置标签 → 文件名（不含目录）
}

// SFXServiceConfig SFXService 构造参数
type SFXServiceConfig struct {
	SFXDir          string // 本地音效目录（环境变量 SFX_DIR）
	FreesoundKey    string // 环境变量 FREESOUND_API_KEY
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

// AutoGenerateSFX 为单个镜头自动选取/生成音效，并将结果写入数据库。
// 若该镜头已有 SFXURL，直接跳过（幂等）。
func (s *SFXService) AutoGenerateSFX(ctx context.Context, shot *model.StoryboardShot) error {
	if shot.SFXURL != "" {
		return nil // 已有音效，幂等跳过
	}

	// 1. 优先使用分镜脚本生成时已提取的 sfx_tags，避免重复 LLM 调用
	var tags []string
	if shot.SFXTags != "" {
		if err := json.Unmarshal([]byte(shot.SFXTags), &tags); err != nil || len(tags) == 0 {
			tags = nil // 解析失败则继续走 LLM
		}
	}
	if len(tags) == 0 {
		var err error
		tags, err = s.extractTags(ctx, shot)
		if err != nil {
			logger.Printf("[SFXService] shot %d LLM tag extract failed (%v), using rule fallback", shot.ID, err)
			tags = s.fallbackTags(shot)
		}
	}

	tagsJSON, _ := json.Marshal(tags)
	logger.Printf("[SFXService] shot %d tags=%s", shot.ID, tagsJSON)

	// 2. 四层降级查找音效：本地库 → Freesound → Jamendo → ElevenLabs
	maxDur := float64(shot.Duration)
	if maxDur <= 0 {
		maxDur = 0 // 不限时长
	}
	sfxURL := s.searchLocalLib(tags)
	if sfxURL != "" {
		logger.Printf("[SFXService] shot %d local hit: %s", shot.ID, sfxURL)
	}
	if sfxURL == "" {
		if s.freesoundKey == "" {
			logger.Printf("[SFXService] shot %d skip Freesound (no key)", shot.ID)
		} else {
			sfxURL = s.searchFreesound(ctx, tags, maxDur)
			if sfxURL != "" {
				logger.Printf("[SFXService] shot %d Freesound hit: %s", shot.ID, sfxURL)
			} else {
				logger.Printf("[SFXService] shot %d Freesound miss", shot.ID)
			}
		}
	}
	if sfxURL == "" {
		if s.jamendoClientID == "" {
			logger.Printf("[SFXService] shot %d skip Jamendo (no client_id)", shot.ID)
		} else {
			sfxURL = s.searchJamendo(ctx, tags, maxDur)
			if sfxURL != "" {
				logger.Printf("[SFXService] shot %d Jamendo hit: %s", shot.ID, sfxURL)
			} else {
				logger.Printf("[SFXService] shot %d Jamendo miss", shot.ID)
			}
		}
	}
	if sfxURL == "" {
		var genErr error
		sfxURL, genErr = s.generateElevenLabs(ctx, shot)
		if genErr != nil {
			logger.Printf("[SFXService] shot %d ElevenLabs failed: %v", shot.ID, genErr)
		} else {
			logger.Printf("[SFXService] shot %d ElevenLabs hit: %s", shot.ID, sfxURL)
		}
	}

	if sfxURL == "" {
		return fmt.Errorf("no SFX found for shot %d (tags: %s)", shot.ID, tagsJSON)
	}

	// 3. 混音音量：有台词降低，有配音轨道适中，纯旁白最高
	vol := 0.4
	if shot.Dialogue != "" {
		vol = 0.2 // 台词场景：音效退后，让台词清晰
	} else if shot.AudioPath != "" {
		vol = 0.3 // 有旁白配音：略降
	}

	// 4. 写入数据库
	return s.storyboardRepo.UpdateSFX(shot.ID, sfxURL, string(tagsJSON), vol)
}

// BatchAutoGenerateSFX 批量处理视频所有镜头，最多 5 并发。
// progressFn 每完成一个镜头调用一次（0-100）。
// 返回成功/失败数量，不因单个失败而中止整批。
func (s *SFXService) BatchAutoGenerateSFX(
	ctx context.Context,
	shots []*model.StoryboardShot,
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
			err := s.AutoGenerateSFX(ctx, s2)
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
func (s *SFXService) extractTags(ctx context.Context, shot *model.StoryboardShot) ([]string, error) {
	prompt := sfxPrompt(shot)
	result, err := s.aiSvc.Generate(0, "sfx_extract", prompt)
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
	// 尝试从 SFXTags 字段构建
	if shot.SFXTags != "" {
		var tags []string
		if json.Unmarshal([]byte(shot.SFXTags), &tags) == nil && len(tags) > 0 {
			// 将 underscore 标签转为自然语言短语
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
	// 降级：截取镜头描述（视觉提示词，质量较差但聊胜于无）
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
