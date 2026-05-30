package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ElevenLabsSFXProvider ElevenLabs 文生音效提供者
// API: POST https://api.elevenlabs.io/v1/sound-generation
// 认证: xi-api-key header
//
// 与 Kling SFX 的区别：
//   - 同步接口，响应体直接为 MP3 二进制数据（无需轮询）
//   - 支持更长时长（0.5~22.0 秒，默认 5.0 秒）
//   - 音频保存为本地临时文件，返回 file:// URL
type ElevenLabsSFXProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

const (
	elevenLabsSFXDefaultEndpoint  = "https://api.elevenlabs.io"
	elevenLabsSFXMinDuration      = 0.5
	elevenLabsSFXMaxDuration      = 22.0
	elevenLabsSFXDefaultDuration  = 5.0
	elevenLabsSFXDefaultInfluence = 0.3
)

// NewElevenLabsSFXProvider 创建 ElevenLabs 文生音效提供者
// apiKey:   ElevenLabs API Key (xi-api-key 鉴权)
// endpoint: 留空使用默认 https://api.elevenlabs.io
func NewElevenLabsSFXProvider(apiKey, endpoint string) *ElevenLabsSFXProvider {
	ep := endpoint
	if ep == "" {
		ep = elevenLabsSFXDefaultEndpoint
	}
	return &ElevenLabsSFXProvider{
		apiKey:   apiKey,
		endpoint: ep,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *ElevenLabsSFXProvider) GetName() string { return "elevenlabs-sfx" }

func (p *ElevenLabsSFXProvider) GetModels() []string {
	return []string{"sound-generation"}
}

func (p *ElevenLabsSFXProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("elevenlabs-sfx: API key not configured")
	}
	return nil
}

func (p *ElevenLabsSFXProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("elevenlabs-sfx: text generation not supported")
}

func (p *ElevenLabsSFXProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("elevenlabs-sfx: streaming not supported")
}

func (p *ElevenLabsSFXProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("elevenlabs-sfx: embeddings not supported")
}

func (p *ElevenLabsSFXProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("elevenlabs-sfx: image generation not supported")
}

// AudioGenerate 同步生成音效并保存为本地临时文件，返回 file:// URL。
//
// req.Text:     音效描述提示词（最多 450 字符），如 "fireworks on new year's eve"
// req.Duration: 音效时长（0.5~22.0 秒），留空/0 默认 5.0 秒
func (p *ElevenLabsSFXProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	if req.Text == "" {
		return nil, fmt.Errorf("elevenlabs-sfx: prompt (Text) is required")
	}

	// 截断超长提示词（API 限制 450 字符）
	text := req.Text
	if len([]rune(text)) > 450 {
		runes := []rune(text)
		text = string(runes[:450])
	}

	duration := req.Duration
	if duration <= 0 {
		duration = elevenLabsSFXDefaultDuration
	}
	if duration < elevenLabsSFXMinDuration {
		duration = elevenLabsSFXMinDuration
	} else if duration > elevenLabsSFXMaxDuration {
		duration = elevenLabsSFXMaxDuration
	}

	reqBody := map[string]interface{}{
		"text":             text,
		"duration_seconds": duration,
		"prompt_influence": elevenLabsSFXDefaultInfluence,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		p.endpoint+"/v1/sound-generation", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs-sfx: create request: %w", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/mpeg")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs-sfx: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("elevenlabs-sfx: HTTP %d: %s", resp.StatusCode, string(b))
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs-sfx: read response body: %w", err)
	}
	if len(audioData) == 0 {
		return nil, fmt.Errorf("elevenlabs-sfx: empty audio response")
	}

	// 保存到临时文件，返回 file:// URL（与 TTS providers 保持一致）
	tmpFile, err := os.CreateTemp("", "inkframe-sfx-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("elevenlabs-sfx: create temp file: %w", err)
	}
	if _, err := tmpFile.Write(audioData); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("elevenlabs-sfx: write temp file: %w", err)
	}
	tmpFile.Close()

	return &AudioResponse{
		URL:       "file://" + tmpFile.Name(),
		Format:    "mp3",
		Duration:  duration,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// Ensure interface compliance.
var _ AIProvider = (*ElevenLabsSFXProvider)(nil)
