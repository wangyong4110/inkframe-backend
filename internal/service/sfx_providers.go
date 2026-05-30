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
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

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
// 优先使用 DB 中配置的 elevenlabs-sfx 提供商（模型管理页面），其次使用环境变量 ELEVENLABS_API_KEY。
func (s *SFXService) generateElevenLabsForTag(ctx context.Context, tenantID uint, item sfxTagItem, shot *model.StoryboardShot) (string, float64, error) {
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

	tagHash := fmt.Sprintf("%x", len(item.Tag)*31+len(item.SFXType))
	ossKey := fmt.Sprintf("sfx/video_%d/shot_%d_%s.mp3", shot.VideoID, shot.ID, tagHash)

	// 优先：通过 AIService 从 DB 加载 elevenlabs-sfx 密钥
	if s.aiSvc != nil {
		rawURL, d, dbErr := s.aiSvc.GenerateSFXWithProvider(ctx, tenantID, "elevenlabs-sfx", prompt, dur)
		if dbErr != nil {
			logger.Printf("[SFXService] generateElevenLabsForTag: DB path failed (tenant=%d): %v", tenantID, dbErr)
		}
		if dbErr == nil && rawURL != "" {
			// ElevenLabsSFXProvider 返回 file:// 临时路径，需上传到 OSS
			if strings.HasPrefix(rawURL, "file://") && s.storageSvc != nil {
				localPath := strings.TrimPrefix(rawURL, "file://")
				if u, err2 := uploadLocalFileToOSS(ctx, s.storageSvc, localPath, ossKey); err2 == nil {
					return u, d, nil
				}
				// 上传失败则直接返回本地路径（降级）
				return rawURL, d, nil
			}
			return rawURL, d, nil
		}
	}

	// 降级：使用环境变量 ELEVENLABS_API_KEY 直接调用 HTTP
	if s.elevenKey == "" {
		return "", 0, fmt.Errorf("elevenlabs key not configured")
	}
	if s.storageSvc == nil {
		return "", 0, fmt.Errorf("storage not configured for elevenlabs upload")
	}

	body, _ := json.Marshal(map[string]interface{}{
		"text":             prompt,
		"duration_seconds": dur,
		"prompt_influence": 0.7,
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

	u, err := s.storageSvc.Upload(ctx, ossKey, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
	return u, float64(dur), err
}

// uploadLocalFileToOSS 读取本地文件并上传到 OSS，上传后删除临时文件。
func uploadLocalFileToOSS(ctx context.Context, svc storage.Service, localPath, ossKey string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return "", err
	}
	u, err := svc.Upload(ctx, ossKey, f, fi.Size(), "audio/mpeg")
	f.Close()
	os.Remove(localPath) //nolint:errcheck
	return u, err
}

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
			// 如果 poll 返回原始音频字节（非 JSON），直接用于上传，跳过 JSON 解析
			if err2 := json.Unmarshal(polledData, &jsonResp); err2 != nil {
				audioData = polledData
				goto uploadAudio
			}
			data = polledData
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

uploadAudio:
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
// returning the final JSON body. Polls every 3s, times out after 10 minutes.
// Uses context.Background() for the poll loop to avoid inheriting a short-lived
// parent deadline; explicit parent cancellation is forwarded via a goroutine.
func (s *SFXService) pollAudioLDMTask(parentCtx context.Context, taskID string) ([]byte, error) {
	// Derive base URL from endpoint (strip path, use scheme+host only)
	parsedURL, err := url.Parse(s.audioLDMEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	pollURL := fmt.Sprintf("%s://%s/result/%s", parsedURL.Scheme, parsedURL.Host, taskID)

	// Use background context so a short-lived parent deadline doesn't kill the poll.
	pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Forward explicit parent cancellation (e.g. user abort) into pollCtx.
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-pollCtx.Done():
		}
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("timeout waiting for audioldm task")
		case <-ticker.C:
			// Each request gets its own 15s timeout independent of pollCtx.
			reqCtx, reqCancel := context.WithTimeout(context.Background(), 15*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, pollURL, nil)
			if err != nil {
				reqCancel()
				return nil, err
			}
			if s.audioLDMKey != "" {
				req.Header.Set("Authorization", "Bearer "+s.audioLDMKey)
			}
			resp, err := (&http.Client{}).Do(req)
			reqCancel()
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
			if status.Status != "completed" {
				return nil, fmt.Errorf("audioldm task failed with status=%q", status.Status)
			}
			// Download the actual audio bytes from /download/{taskID}
			downloadURL := fmt.Sprintf("%s://%s/download/%s", parsedURL.Scheme, parsedURL.Host, taskID)
			logger.Printf("[SFXService] AudioLDM poll task_id=%s completed, downloading from %s", taskID, downloadURL)
			dlReqCtx, dlReqCancel := context.WithTimeout(context.Background(), 30*time.Second)
			dlReq, err := http.NewRequestWithContext(dlReqCtx, http.MethodGet, downloadURL, nil)
			if err != nil {
				dlReqCancel()
				return nil, fmt.Errorf("audioldm download request: %w", err)
			}
			if s.audioLDMKey != "" {
				dlReq.Header.Set("Authorization", "Bearer "+s.audioLDMKey)
			}
			dlResp, err := (&http.Client{}).Do(dlReq)
			dlReqCancel()
			if err != nil {
				return nil, fmt.Errorf("audioldm download: %w", err)
			}
			audioBytes, _ := io.ReadAll(io.LimitReader(dlResp.Body, 64*1024*1024))
			dlResp.Body.Close()
			if dlResp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("audioldm download HTTP %d", dlResp.StatusCode)
			}
			return audioBytes, nil
		}
	}
}
