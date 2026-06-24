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

// QwenTTSProvider 千问 TTS 语音合成提供者（阿里云百炼）
// 端点：POST {endpoint}/api/v1/services/aigc/multimodal-generation/generation
// 使用非流式模式，响应直接返回音频 URL（有效期 24 小时）
//
// 支持的音色（voice）：Cherry、Ethan、Serena、Dylan、Aria、Ember、Luna 等
// 完整列表：https://help.aliyun.com/zh/model-studio/non-realtime-tts-user-guide
type QwenTTSProvider struct {
	apiKey   string
	endpoint string // 如 https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com
	client   *http.Client
}

const qwenTTSDefaultModel = "qwen3-tts-flash"

// NewQwenTTSProvider 创建千问 TTS 语音合成提供者
// endpoint 为可选工作区专属域名，如 https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com
// 留空则使用通用域名 https://dashscope.aliyuncs.com
func NewQwenTTSProvider(apiKey, endpoint string) *QwenTTSProvider {
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com"
	}
	return &QwenTTSProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *QwenTTSProvider) GetName() string { return "qwen-tts" }

func (p *QwenTTSProvider) GetModels() []string {
	return []string{"qwen3-tts-flash", "qwen3-tts-instruct-flash", "qwen-tts"}
}

func (p *QwenTTSProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("qwen-tts: api_key not configured")
	}
	return nil
}

func (p *QwenTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("qwen-tts: text generation not supported")
}

func (p *QwenTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("qwen-tts: streaming not supported")
}

func (p *QwenTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("qwen-tts: embeddings not supported")
}

func (p *QwenTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("qwen-tts: image generation not supported")
}

// AudioGenerate 调用千问 TTS 语音合成（非流式），返回音频 URL。
//
// req.Voice:  音色名称（如 Cherry、Ethan），留空默认 "Cherry"
// req.Model:  覆盖模型（如 "qwen3-tts-instruct-flash"），留空使用 qwen3-tts-flash
// req.Emotion: 通过 instructions 字段控制合成风格（仅 instruct 模型有效）
func (p *QwenTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voice := req.Voice
	if voice == "" {
		voice = "Cherry"
	}
	model := req.Model
	if model == "" {
		model = qwenTTSDefaultModel
	}

	input := map[string]interface{}{
		"text":          req.Text,
		"voice":         voice,
		"language_type": "Auto",
	}
	// instructions 仅在 instruct 模型中生效
	if req.Emotion != "" {
		input["instructions"] = req.Emotion
	}

	body, err := json.Marshal(map[string]interface{}{
		"model": model,
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("qwen-tts: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		p.endpoint+"/api/v1/services/aigc/multimodal-generation/generation",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("qwen-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("qwen-tts: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qwen-tts: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var result struct {
		Output struct {
			FinishReason string `json:"finish_reason"`
			Audio        struct {
				URL string `json:"url"`
			} `json:"audio"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("qwen-tts: parse response: %w", err)
	}
	if result.Code != "" {
		return nil, fmt.Errorf("qwen-tts: API error %s: %s", result.Code, result.Message)
	}
	if result.Output.Audio.URL == "" {
		return nil, fmt.Errorf("qwen-tts: no audio URL in response")
	}

	return &AudioResponse{
		URL:       result.Output.Audio.URL,
		Format:    "wav",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

var _ AIProvider = (*QwenTTSProvider)(nil)
