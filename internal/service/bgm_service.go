package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// BGMService BGM背景音乐服务
type BGMService struct {
	bgmDir          string            // 本地 BGM 目录（优先）
	bgmURLMap       map[string]string // emotion → URL（CDN fallback，占位符）
	aiSvc           *AIService        // AI 分析（可选）
	jamendoClientID string            // Jamendo client_id（可选）
	httpClient      *http.Client
}

// NewBGMService 创建 BGM 服务
// bgmDir: 本地 BGM 文件目录，文件名格式 <emotion>.mp3 或 <emotion>.wav
// customURLs: 可选的 emotion→URL 映射覆盖
func NewBGMService(bgmDir string, customURLs ...map[string]string) *BGMService {
	urlMap := map[string]string{
		"tension":  "",
		"joy":      "",
		"sadness":  "",
		"romance":  "",
		"action":   "",
		"mystery":  "",
		"peaceful": "",
		"default":  "",
	}
	if len(customURLs) > 0 {
		for k, v := range customURLs[0] {
			urlMap[k] = v
		}
	}
	return &BGMService{
		bgmDir:     bgmDir,
		bgmURLMap:  urlMap,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// WithAIService 注入 AI 服务（启用 AI 分段分析）
func (s *BGMService) WithAIService(aiSvc *AIService) *BGMService {
	s.aiSvc = aiSvc
	return s
}

// WithJamendo 配置 Jamendo client_id
func (s *BGMService) WithJamendo(clientID string) *BGMService {
	s.jamendoClientID = clientID
	return s
}

// SelectBGM 根据情感选择 BGM，返回本地路径或 URL
// 返回空字符串表示没有可用的 BGM
func (s *BGMService) SelectBGM(emotion string) string {
	if emotion == "" {
		emotion = "default"
	}
	emotion = strings.ToLower(strings.TrimSpace(emotion))

	// 优先查找本地文件
	if s.bgmDir != "" {
		for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
			candidate := filepath.Join(s.bgmDir, emotion+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		// 尝试 default
		for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
			candidate := filepath.Join(s.bgmDir, "default"+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	// 回退到 URL map
	if url, ok := s.bgmURLMap[emotion]; ok && url != "" {
		return url
	}
	if url, ok := s.bgmURLMap["default"]; ok && url != "" {
		return url
	}

	return ""
}

// MixBGM 将 BGM 混入视频（BGM 音量 30%，对话优先）
// videoPath: 视频文件路径或 file:// URL
// bgmSource: BGM 本地路径或 HTTP URL
// outputPath: 输出文件路径
func (s *BGMService) MixBGM(videoPath, bgmSource, outputPath string) error {
	videoPath = strings.TrimPrefix(videoPath, "file://")
	bgmSource = strings.TrimPrefix(bgmSource, "file://")

	if videoPath == "" || bgmSource == "" || outputPath == "" {
		return fmt.Errorf("MixBGM: invalid arguments")
	}

	// BGM 需要下载的情况
	bgmLocalPath := bgmSource
	if strings.HasPrefix(bgmSource, "http://") || strings.HasPrefix(bgmSource, "https://") {
		tmpBGM := fmt.Sprintf("%s/inkframe-bgm-%d.mp3", inkframeTempDir(), os.Getpid())
		if err := downloadFile(bgmSource, tmpBGM); err != nil {
			return fmt.Errorf("MixBGM: download BGM failed: %w", err)
		}
		defer os.Remove(tmpBGM)
		bgmLocalPath = tmpBGM
	}

	// FFmpeg: 混合 BGM（30% 音量），原视频音频优先，BGM 循环填充
	// -stream_loop -1 让 BGM 循环
	// amix: 混合两路音频，duration=first 以视频时长为准
	if out, err := runFFmpegCtx(context.Background(), "-y",
		"-i", videoPath,
		"-stream_loop", "-1",
		"-i", bgmLocalPath,
		"-filter_complex", "[1:a]volume=0.3[bgm];[0:a][bgm]amix=inputs=2:duration=first[out]",
		"-map", "0:v",
		"-map", "[out]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-shortest",
		outputPath,
	); err != nil {
		logger.Printf("MixBGM: ffmpeg failed: %v\n%s", err, string(out))
		return fmt.Errorf("ffmpeg BGM mix failed: %w", err)
	}
	return nil
}

// ─── AI BGM 分段分析 ──────────────────────────────────────────────────────────

// bgmShotBrief 供 AI 分析的分镜摘要
type bgmShotBrief struct {
	ShotID        uint    `json:"shot_id"`
	ShotNo        int     `json:"shot_no"`
	Description   string  `json:"description"`
	EmotionalTone string  `json:"emotional_tone,omitempty"`
	Narration     string  `json:"narration,omitempty"`
	Duration      float64 `json:"duration"`
}

// bgmSegmentAnalysis AI 输出的单个 BGM 分段
type bgmSegmentAnalysis struct {
	StartShotNo   int      `json:"start_shot_no"`
	EndShotNo     int      `json:"end_shot_no"`
	Mood          string   `json:"mood"`
	Tempo         string   `json:"tempo"` // fast/medium/slow
	SearchQueries []string `json:"search_queries"`
}

// AnalyzeBGMForVideo 调用 AI 批量分析全部分镜，生成 BGM 分段计划并持久化。
// AI 负责：①将相邻情绪相近的分镜归组；②生成自然语言 Jamendo 搜索词。
// 返回分析出的分段列表（未下载音频）。
func (s *BGMService) AnalyzeBGMForVideo(
	ctx context.Context,
	shots []*model.StoryboardShot,
	bgmRepo *repository.VideoBGMSegmentRepository,
	videoID uint,
	tenantID uint,
	userPrompt string,
) ([]*model.VideoBGMSegment, error) {
	if s.aiSvc == nil {
		return nil, fmt.Errorf("BGMService: AI service not configured")
	}
	if len(shots) == 0 {
		return nil, nil
	}

	// 构建摘要列表
	briefs := make([]bgmShotBrief, 0, len(shots))
	for _, sh := range shots {
		narration := sh.Narration
		if len(narration) > 60 {
			narration = narration[:60] + "…"
		}
		briefs = append(briefs, bgmShotBrief{
			ShotID:        sh.ID,
			ShotNo:        sh.ShotNo,
			Description:   sh.Description,
			EmotionalTone: sh.EmotionalTone,
			Narration:     narration,
			Duration:      sh.Duration,
		})
	}

	briefsJSON, err := json.MarshalIndent(briefs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal briefs: %w", err)
	}

	prompt := fmt.Sprintf(`你是一位专业的影视配乐顾问，擅长为短视频/影视片段分析背景音乐需求。

以下是一段视频的所有分镜信息（JSON数组）：
%s

请分析这些分镜的情绪走向，将情绪/氛围相近的连续分镜归为一个BGM分段。
一首背景音乐应跨越多个分镜（建议每段跨3~8个分镜），避免频繁换曲。

要求：
1. 将所有分镜（shot_no从最小到最大，不遗漏）分组为若干BGM分段
2. 每段给出：start_shot_no、end_shot_no、mood（中文情绪描述，≤20字）、tempo（fast/medium/slow）
3. search_queries：给出3~5个英文自然语言搜索词，适合在 Jamendo 等音乐库搜索
   - 要具体描述音乐风格、乐器、情绪（如 "epic orchestral battle theme strings percussion"）
   - 不要用通用词（如 "music" "background" "film score"）
   - 可以包含流派关键词（如 "chinese traditional erhu melancholy" "xianxia fantasy ethereal flute"）
   - 每条搜索词6~12个英文单词

只输出合法 JSON 数组，格式如下（禁止输出其他内容）：
[
  {
    "start_shot_no": 1,
    "end_shot_no": 5,
    "mood": "紧张压抑的对峙",
    "tempo": "medium",
    "search_queries": [
      "tense confrontation orchestral dark strings suspense",
      "dramatic tension low brass cinematic thriller",
      "chinese xianxia cultivation power struggle intense"
    ]
  }
]`, string(briefsJSON))

	if userPrompt != "" {
		prompt += "\n\n额外背景信息（优先参考，用于调整BGM风格/情绪）：\n" + userPrompt
	}

	raw, err := s.aiSvc.GenerateWithProvider(tenantID, 0, "bgm_analyze", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("BGM AI analysis failed: %w", err)
	}

	// 解析 AI 输出
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("BGM AI returned no JSON: %s", raw[:min(len(raw), 200)])
	}

	var analyses []bgmSegmentAnalysis
	if err := json.Unmarshal([]byte(jsonStr), &analyses); err != nil {
		return nil, fmt.Errorf("BGM AI JSON parse failed: %w\nraw: %s", err, jsonStr[:min(len(jsonStr), 300)])
	}

	// 构建 model 列表
	segments := make([]*model.VideoBGMSegment, 0, len(analyses))
	for i, a := range analyses {
		qJSON, _ := json.Marshal(a.SearchQueries)
		segments = append(segments, &model.VideoBGMSegment{
			VideoID:       videoID,
			SeqNo:         i + 1,
			StartShotNo:   a.StartShotNo,
			EndShotNo:     a.EndShotNo,
			Mood:          a.Mood,
			Tempo:         a.Tempo,
			SearchQueries: string(qJSON),
			Volume:        0.3,
		})
	}

	// 持久化：先删旧的
	if bgmRepo != nil {
		if err := bgmRepo.DeleteByVideoID(videoID); err != nil {
			logger.Printf("[BGMService] DeleteByVideoID %d failed: %v", videoID, err)
		}
		if err := bgmRepo.BatchCreate(segments); err != nil {
			return segments, fmt.Errorf("persist BGM segments failed: %w", err)
		}
	}

	return segments, nil
}

// SearchBGMForSegment 为单个 BGM 分段在 Jamendo 搜索匹配曲目，更新 URL/TrackName/TrackArtist/Source。
func (s *BGMService) SearchBGMForSegment(ctx context.Context, seg *model.VideoBGMSegment) error {
	var queries []string
	if seg.SearchQueries != "" {
		_ = json.Unmarshal([]byte(seg.SearchQueries), &queries)
	}
	if len(queries) == 0 {
		if seg.Mood != "" {
			queries = []string{seg.Mood}
		} else {
			return nil
		}
	}

	// 本地文件优先
	if s.bgmDir != "" {
		if local := s.SelectBGM(seg.Mood); local != "" {
			seg.URL = local
			seg.Source = "local"
			return nil
		}
	}

	// Jamendo 搜索
	if s.jamendoClientID != "" {
		for _, q := range queries {
			if trackURL, name, artist := s.jamendoSearch(ctx, q); trackURL != "" {
				seg.URL = trackURL
				seg.TrackName = name
				seg.TrackArtist = artist
				seg.Source = "jamendo"
				return nil
			}
		}
		logger.Printf("[BGMService] segment %d (%s) Jamendo miss for all queries", seg.SeqNo, seg.Mood)
	}

	return nil
}

// JamendoTrack Jamendo 音轨信息（用于前端搜索结果展示）
type JamendoTrack struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	ArtistName           string   `json:"artist_name"`
	Duration             int      `json:"duration"`
	Audio                string   `json:"audio"`
	AudioDownload        string   `json:"audiodownload"`
	AudioDownloadAllowed bool     `json:"audiodownload_allowed"`
	Tags                 []string `json:"tags,omitempty"`
}

// PlayURL 返回优先可下载的播放 URL
func (t JamendoTrack) PlayURL() string {
	if t.AudioDownloadAllowed && t.AudioDownload != "" {
		return t.AudioDownload
	}
	return t.Audio
}

// JamendoSearchParams Jamendo 搜索参数
type JamendoSearchParams struct {
	Query  string // 自然语言模糊搜索（fuzzytags）
	Tags   string // 精确标签（空格分隔）
	Speed  string // slow / medium / fast / veryslow / veryfast
	BpmMin int    // BPM 下限，0=不限
	BpmMax int    // BPM 上限，0=不限
	Limit  int    // 结果数量，默认 10，最多 50
}

// JamendoSearch 在 Jamendo API 中搜索器乐曲目，返回完整音轨列表供前端展示/选择。
func (s *BGMService) JamendoSearch(ctx context.Context, p JamendoSearchParams) ([]JamendoTrack, error) {
	if s.jamendoClientID == "" {
		return nil, fmt.Errorf("Jamendo client_id not configured")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	params := url.Values{}
	params.Set("client_id", s.jamendoClientID)
	params.Set("format", "json")
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("vocalinstrumental", "instrumental")
	params.Set("order", "popularity_month")
	params.Set("include", "musicinfo")

	if p.Query != "" {
		params.Set("fuzzytags", p.Query)
	}
	if p.Tags != "" {
		params.Set("tags", p.Tags)
	}
	if p.Speed != "" {
		params.Set("speed", p.Speed)
	}
	if p.BpmMin > 0 && p.BpmMax > 0 {
		params.Set("bpm_between", fmt.Sprintf("%d_%d", p.BpmMin, p.BpmMax))
	} else if p.BpmMin > 0 {
		params.Set("bpm_between", fmt.Sprintf("%d_300", p.BpmMin))
	} else if p.BpmMax > 0 {
		params.Set("bpm_between", fmt.Sprintf("0_%d", p.BpmMax))
	}

	apiURL := "https://api.jamendo.com/v3.0/tracks/?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Jamendo request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jamendo HTTP %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			ArtistName           string `json:"artist_name"`
			Duration             int    `json:"duration"`
			Audio                string `json:"audio"`
			AudioDownload        string `json:"audiodownload"`
			AudioDownloadAllowed bool   `json:"audiodownload_allowed"`
			MusicInfo            struct {
				Tags struct {
					Genres   []string `json:"genres"`
					Vartags  []string `json:"vartags"`
				} `json:"tags"`
			} `json:"musicinfo"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("Jamendo response parse failed: %w", err)
	}

	tracks := make([]JamendoTrack, 0, len(result.Results))
	for _, r := range result.Results {
		tags := append(r.MusicInfo.Tags.Genres, r.MusicInfo.Tags.Vartags...)
		tracks = append(tracks, JamendoTrack{
			ID:                   r.ID,
			Name:                 r.Name,
			ArtistName:           r.ArtistName,
			Duration:             r.Duration,
			Audio:                r.Audio,
			AudioDownload:        r.AudioDownload,
			AudioDownloadAllowed: r.AudioDownloadAllowed,
			Tags:                 tags,
		})
	}
	return tracks, nil
}

// jamendoSearch 在 Jamendo API 中按自然语言搜索 BGM 曲目，返回 (url, name, artist)
// 内部使用，供自动批量生成流程调用。
func (s *BGMService) jamendoSearch(ctx context.Context, query string) (string, string, string) {
	tracks, err := s.JamendoSearch(ctx, JamendoSearchParams{Query: query, Limit: 5})
	if err != nil || len(tracks) == 0 {
		return "", "", ""
	}
	for _, t := range tracks {
		if u := t.PlayURL(); u != "" {
			return u, t.Name, t.ArtistName
		}
	}
	return "", "", ""
}

// GenerateBGMSegments AI分析 + Jamendo搜索，一步完成全部BGM分段生成。
func (s *BGMService) GenerateBGMSegments(
	ctx context.Context,
	shots []*model.StoryboardShot,
	bgmRepo *repository.VideoBGMSegmentRepository,
	videoID uint,
	tenantID uint,
	userPrompt string,
	progressFn func(int),
) ([]*model.VideoBGMSegment, error) {
	progress := func(pct int) {
		if progressFn != nil {
			progressFn(pct)
		}
	}

	// Step 1: AI analysis
	progress(5)
	segments, err := s.AnalyzeBGMForVideo(ctx, shots, bgmRepo, videoID, tenantID, userPrompt)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("BGM AI returned 0 segments for video %d (check AI provider config or prompt)", videoID)
	}
	logger.Printf("[BGMService] AI produced %d BGM segments for video %d", len(segments), videoID)
	progress(50)

	if s.jamendoClientID == "" && s.bgmDir == "" {
		logger.Printf("[BGMService] WARNING: neither JAMENDO_CLIENT_ID nor BGM_DIR configured; segments will be saved without audio URLs")
	}

	// Step 2: Search Jamendo per segment
	for i, seg := range segments {
		if err := s.SearchBGMForSegment(ctx, seg); err != nil {
			logger.Printf("[BGMService] segment %d search error: %v", i+1, err)
		}
		logger.Printf("[BGMService] segment %d (%s): url=%q source=%q", i+1, seg.Mood, seg.URL, seg.Source)
		// Persist URL update
		if bgmRepo != nil && seg.ID > 0 {
			if err := bgmRepo.Update(seg); err != nil {
				logger.Printf("[BGMService] segment %d Update failed: %v", i+1, err)
			}
		} else {
			logger.Printf("[BGMService] segment %d skipped Update: bgmRepo=%v seg.ID=%d", i+1, bgmRepo != nil, seg.ID)
		}
		pct := 50 + (i+1)*50/len(segments)
		progress(pct)
	}

	return segments, nil
}

