package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// FunMusicProvider 阿里云百炼 Fun-Music 音乐生成提供者
// 端点：POST https://dashscope.aliyuncs.com/api/v1/services/audio/music/generation
// 使用非流式模式，响应直接返回音频 URL（有效期 24 小时）
//
// 使用方式：
//   - req.Text  → input.prompt（音乐描述，如"夏日清新民谣，木吉他与口琴伴奏"）
//   - req.Voice → input.gender（"male"/"female"，默认 female）
//   - req.Model → model（默认 fun-music-v1）
type FunMusicProvider struct {
	apiKey string
	client *http.Client
}

const (
	funMusicEndpoint     = "https://dashscope.aliyuncs.com/api/v1/services/audio/music/generation"
	funMusicDefaultModel = "fun-music-v1"
)

// NewFunMusicProvider 创建 Fun-Music 音乐生成提供者
func NewFunMusicProvider(apiKey string) *FunMusicProvider {
	return &FunMusicProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 300 * time.Second}, // 生成耗时较长，超时设为 5 分钟
	}
}

func (p *FunMusicProvider) GetName() string    { return "fun-music" }
func (p *FunMusicProvider) GetModels() []string { return []string{funMusicDefaultModel} }

func (p *FunMusicProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("fun-music: api_key not configured")
	}
	return nil
}

func (p *FunMusicProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("fun-music: text generation not supported")
}

func (p *FunMusicProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("fun-music: streaming not supported")
}

func (p *FunMusicProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("fun-music: embeddings not supported")
}

func (p *FunMusicProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("fun-music: image generation not supported")
}

// AudioGenerate 调用 Fun-Music 音乐生成（非流式），返回音频 URL。
//
// req.Text:  音乐描述 prompt（如"夏日清新民谣，木吉他与口琴"）
// req.Voice: 演唱性别，"male" 或 "female"（默认 female）
// req.Model: 模型名，留空使用 fun-music-v1
func (p *FunMusicProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = funMusicDefaultModel
	}

	gender := req.Voice
	if gender != "male" && gender != "female" {
		gender = "female"
	}

	input := map[string]interface{}{
		"prompt": req.Text,
		"gender": gender,
		"format": "mp3",
	}

	body, err := json.Marshal(map[string]interface{}{
		"model": model,
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("fun-music: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", funMusicEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	// 不设 X-DashScope-SSE → 非流式，响应体直接包含 URL

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fun-music: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fun-music: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fun-music: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var result struct {
		Output struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
			ExtraInfo struct {
				Lyrics string `json:"lyrics"`
			} `json:"extra_info"`
			FinishReason string `json:"finish_reason"`
		} `json:"output"`
		Usage struct {
			Duration int `json:"duration"`
		} `json:"usage"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("fun-music: parse response: %w", err)
	}
	if result.Code != "" {
		return nil, fmt.Errorf("fun-music: API error %s: %s", result.Code, result.Message)
	}
	if result.Output.Audio.URL == "" {
		return nil, fmt.Errorf("fun-music: no audio URL in response")
	}

	return &AudioResponse{
		URL:       result.Output.Audio.URL,
		Format:    "mp3",
		Duration:  float64(result.Usage.Duration),
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

var _ AIProvider = (*FunMusicProvider)(nil)
