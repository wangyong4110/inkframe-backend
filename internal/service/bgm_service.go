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

	raw, err := s.aiSvc.Generate(0, "bgm_analyze", prompt)
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

// jamendoSearch 在 Jamendo API 中按自然语言搜索 BGM 曲目，返回 (url, name, artist)
func (s *BGMService) jamendoSearch(ctx context.Context, query string) (string, string, string) {
	params := url.Values{}
	params.Set("client_id", s.jamendoClientID)
	params.Set("format", "json")
	params.Set("limit", "5")
	params.Set("fuzzytags", query)
	params.Set("vocalinstrumental", "instrumental")
	params.Set("order", "popularity_month")
	params.Set("include", "musicinfo")

	apiURL := "https://api.jamendo.com/v3.0/tracks/?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", "", ""
	}
	resp, err := s.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", "", ""
	}
	defer resp.Body.Close()

	var result struct {
		Results []struct {
			Audio                string `json:"audio"`
			AudioDownload        string `json:"audiodownload"`
			AudioDownloadAllowed bool   `json:"audiodownload_allowed"`
			Name                 string `json:"name"`
			ArtistName           string `json:"artist_name"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
		return "", "", ""
	}
	for _, track := range result.Results {
		audioURL := ""
		if track.AudioDownloadAllowed && track.AudioDownload != "" {
			audioURL = track.AudioDownload
		} else if track.Audio != "" {
			audioURL = track.Audio
		}
		if audioURL != "" {
			return audioURL, track.Name, track.ArtistName
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
	progressFn func(int),
) ([]*model.VideoBGMSegment, error) {
	progress := func(pct int) {
		if progressFn != nil {
			progressFn(pct)
		}
	}

	// Step 1: AI analysis
	progress(5)
	segments, err := s.AnalyzeBGMForVideo(ctx, shots, bgmRepo, videoID)
	if err != nil {
		return nil, err
	}
	progress(50)

	// Step 2: Search Jamendo per segment
	for i, seg := range segments {
		if err := s.SearchBGMForSegment(ctx, seg); err != nil {
			logger.Printf("[BGMService] segment %d search error: %v", i+1, err)
		}
		// Persist URL update
		if bgmRepo != nil && seg.ID > 0 {
			_ = bgmRepo.Update(seg)
		}
		pct := 50 + (i+1)*50/len(segments)
		progress(pct)
	}

	return segments, nil
}

