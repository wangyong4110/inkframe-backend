package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai/aliyun"
)

// MinimaxMusicProvider MiniMax 文生音乐提供者
// 端点：POST https://api.minimaxi.com/v1/music_generation
// 使用非流式、output_format=url 模式，响应直接返回音频 URL（有效期 24 小时，与 Fun-Music 一致，
// 调用方需要长期保留时应自行下载并转存到 OSS）。
//
// 使用方式：
//   - req.Text         → prompt（风格/情绪描述，如"流行音乐, 难过, 适合在下雨的晚上"）
//   - req.Lyrics        → lyrics（歌词，非纯音乐时必填；留空且非纯音乐时自动开启 lyrics_optimizer 由 prompt 生成）
//   - req.Instrumental  → is_instrumental（纯音乐/无人声）
//   - req.Model         → model（默认 music-3.0；music-cover 系列需要参考音频，不在本 provider 支持范围）
type MinimaxMusicProvider struct {
	apiKey string
	client *http.Client
}

const (
	minimaxMusicEndpoint     = "https://api.minimaxi.com/v1/music_generation"
	minimaxMusicDefaultModel = "music-3.0"
)

// minimaxMusicTextModels 是本 provider 支持的模型：仅文生音乐系列。
// music-cover 系列需要参考音频（audio_url/audio_base64/cover_feature_id），走不同的调用形态，
// 不通过 AudioGenerateRequest.Text 表达，因此不在此路由。
var minimaxMusicTextModels = map[string]bool{
	"music-3.0":      true,
	"music-2.6":      true,
	"music-3.0-free": true,
	"music-2.6-free": true,
}

// NewMinimaxMusicProvider 创建 MiniMax 文生音乐提供者
func NewMinimaxMusicProvider(apiKey string) *MinimaxMusicProvider {
	return &MinimaxMusicProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 300 * time.Second}, // 生成耗时较长，超时设为 5 分钟
	}
}

func (p *MinimaxMusicProvider) GetName() string { return "minimax-music" }
func (p *MinimaxMusicProvider) GetModels() []string {
	return []string{"music-3.0", "music-2.6", "music-3.0-free", "music-2.6-free"}
}

func (p *MinimaxMusicProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("minimax-music: api_key not configured")
	}
	return nil
}

func (p *MinimaxMusicProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("minimax-music: text generation not supported")
}

func (p *MinimaxMusicProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("minimax-music: streaming not supported")
}

func (p *MinimaxMusicProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("minimax-music: embeddings not supported")
}

func (p *MinimaxMusicProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("minimax-music: image generation not supported")
}

// AudioGenerate 调用 MiniMax 文生音乐（非流式，output_format=url），返回音频 URL。
//
// req.Text:         音乐描述 prompt（风格/情绪/场景）
// req.Lyrics:        歌词，用 \n 分隔每行；非纯音乐且为空时自动由 prompt 生成（lyrics_optimizer）
// req.Instrumental:  是否生成纯音乐（无人声）
// req.Model:         模型名，留空使用 music-3.0；非文生音乐模型（如 music-cover）会报错
func (p *MinimaxMusicProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = minimaxMusicDefaultModel
	}
	if !minimaxMusicTextModels[model] {
		return nil, fmt.Errorf("minimax-music: model %q not supported by this provider (only text-to-music models: music-3.0/-free, music-2.6/-free)", model)
	}

	body := map[string]interface{}{
		"model":           model,
		"prompt":          req.Text,
		"output_format":   "url",
		"is_instrumental": req.Instrumental,
		"audio_setting": map[string]interface{}{
			"sample_rate": 44100,
			"bitrate":     256000,
			"format":      "mp3",
		},
	}
	if req.Lyrics != "" {
		body["lyrics"] = req.Lyrics
	} else if !req.Instrumental {
		// 纯音乐可以不传歌词；非纯音乐但没给歌词时，让 MiniMax 根据 prompt 自动生成。
		body["lyrics_optimizer"] = true
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("minimax-music: marshal request: %w", err)
	}

	log.Printf("[minimax-music] AudioGenerate model=%s instrumental=%v promptLen=%d lyricsLen=%d prompt=%.200q",
		model, req.Instrumental, len(req.Text), len(req.Lyrics), req.Text)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", minimaxMusicEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("minimax-music: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("minimax-music: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax-music: HTTP %d: %s", resp.StatusCode, aliyun.truncate(string(respBody), 300))
	}

	var result struct {
		Data struct {
			Status int    `json:"status"` // 1=合成中, 2=已完成
			Audio  string `json:"audio"`  // output_format=url 时为音频 URL；=hex 时为十六进制音频数据
		} `json:"data"`
		ExtraInfo struct {
			MusicDuration int `json:"music_duration"` // 毫秒
		} `json:"extra_info"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("minimax-music: parse response: %w", err)
	}

	if result.BaseResp.StatusCode != 0 {
		// 限流（1002）附加 "rate limit" 关键字，命中 isRetryable 的关键字匹配，交给 RetryProvider 重试。
		suffix := ""
		if result.BaseResp.StatusCode == 1002 {
			suffix = " (rate limit)"
		}
		return nil, fmt.Errorf("minimax-music: API error %d: %s%s", result.BaseResp.StatusCode, result.BaseResp.StatusMsg, suffix)
	}
	if result.Data.Audio == "" {
		return nil, fmt.Errorf("minimax-music: no audio in response (status=%d)", result.Data.Status)
	}

	return &AudioResponse{
		URL:       result.Data.Audio,
		Format:    "mp3",
		Duration:  float64(result.ExtraInfo.MusicDuration) / 1000,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

var _ AIProvider = (*MinimaxMusicProvider)(nil)
