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

// AliyunTTSProvider 阿里云 CosyVoice 语音合成提供者（DashScope 新版 API）
// 端点：POST https://dashscope.aliyuncs.com/api/v1/services/audio/tts/SpeechSynthesizer
// 使用非流式模式，响应直接返回音频 URL（有效期 24 小时）
//
// 音色列表（voice）：
//
//	longxiaochun      龙小淳（女声，知性温暖）
//	longxiaoxia       龙晓夏（女声，活泼朝气）
//	longxiaobai       龙小白（男声，年轻开朗）
//	longfei           龙飞（男声，沉稳自信）
//	longjielidou      龙姐励豆（女声，温暖励志）
//	longmiaomiao      龙淼淼（女声，儿童活泼）
//	longshu           龙叔（男声，叙述沉稳）
//	longwan           龙婉（女声，甜美温柔）
//	longcheng         龙橙（男声，清晰专业）
//	longhua           龙华（男声，成熟稳重）
//	longxiang         龙祥（男声，磁性低沉）
//	loongbella        贝拉（英文女声）
//	loongbobby        鲍比（英文男声）
type AliyunTTSProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

const aliyunTTSDefaultModel = "cosyvoice-v3-flash"

// NewAliyunTTSProvider 创建阿里云 CosyVoice 语音合成提供者
// endpoint 为可选工作区专属域名，如 https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com
// 留空则使用通用域名 https://dashscope.aliyuncs.com
func NewAliyunTTSProvider(apiKey, endpoint string) *AliyunTTSProvider {
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com"
	}
	return &AliyunTTSProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *AliyunTTSProvider) GetName() string { return "aliyun-tts" }

func (p *AliyunTTSProvider) GetModels() []string {
	return []string{
		"longxiaochun", "longxiaoxia", "longxiaobai", "longfei",
		"longjielidou", "longmiaomiao", "longshu", "longwan",
		"longcheng", "longhua", "longxiang", "loongbella", "loongbobby",
	}
}

func (p *AliyunTTSProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("aliyun-tts: api_key not configured")
	}
	return nil
}

func (p *AliyunTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: text generation not supported")
}

func (p *AliyunTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: streaming not supported")
}

func (p *AliyunTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("aliyun-tts: embeddings not supported")
}

func (p *AliyunTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: image generation not supported")
}

// AudioGenerate 调用阿里云 CosyVoice 语音合成（非流式），返回音频 URL。
//
// req.Voice:  音色名称（见音色列表），留空默认 "longxiaochun"
// req.Speed:  语速（0.5~2.0，1.0=正常），对应 API 的 rate 参数
// req.Pitch:  音调（0.5~2.0，1.0=正常），对应 API 的 pitch 参数
// req.Model:  覆盖模型（如 "cosyvoice-v3.5-plus"），留空使用 cosyvoice-v3-flash
func (p *AliyunTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voice := req.Voice
	if voice == "" {
		voice = "longxiaochun"
	}
	model := req.Model
	if model == "" {
		model = aliyunTTSDefaultModel
	}

	// input 字段：新版 API 将速率/音调等直接放在 input 里
	input := map[string]interface{}{
		"text":        req.Text,
		"voice":       voice,
		"format":      "mp3",
		"sample_rate": 24000,
	}
	if req.Speed > 0 && req.Speed != 1.0 {
		input["rate"] = clampFloat(req.Speed, 0.5, 2.0)
	}
	if req.Pitch > 0 && req.Pitch != 1.0 {
		input["pitch"] = clampFloat(req.Pitch, 0.5, 2.0)
	}
	if req.Emotion != "" {
		input["instruction"] = req.Emotion
	}

	body, err := json.Marshal(map[string]interface{}{
		"model": model,
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		p.endpoint+"/api/v1/services/audio/tts/SpeechSynthesizer", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	// 不设 X-DashScope-SSE 头 → 非流式，响应体直接包含 URL

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aliyun-tts: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var result struct {
		Output struct {
			FinishReason string `json:"finish_reason"`
			Audio        struct {
				URL  string `json:"url"`
				Data string `json:"data"`
			} `json:"audio"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("aliyun-tts: parse response: %w", err)
	}
	if result.Code != "" {
		return nil, fmt.Errorf("aliyun-tts: API error %s: %s", result.Code, result.Message)
	}
	if result.Output.Audio.URL == "" {
		return nil, fmt.Errorf("aliyun-tts: no audio URL in response")
	}

	return &AudioResponse{
		URL:       result.Output.Audio.URL,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ AIProvider = (*AliyunTTSProvider)(nil)
