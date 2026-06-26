package ai

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// QianwenTTSRouter dispatches AudioGenerate to AliyunTTSProvider or QwenTTSProvider
// based on the voice ID.  Aliyun CosyVoice IDs start with "long" / "loong"; everything
// else is routed to the Qwen TTS service.  Both services share the same DashScope API key.
type QianwenTTSRouter struct {
	aliyun *AliyunTTSProvider
	qwen   *QwenTTSProvider
}

// ttsBaseURL 提取 scheme+host，丢弃路径部分（如 /compatible-mode/v1）。
// qianwen provider 的 APIEndpoint 是 LLM 兼容路径，TTS 子 provider 需要纯 base URL。
func ttsBaseURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return u.Scheme + "://" + u.Host
}

func NewQianwenTTSRouter(apiKey, endpoint string) *QianwenTTSRouter {
	base := ttsBaseURL(endpoint)
	return &QianwenTTSRouter{
		aliyun: NewAliyunTTSProvider(apiKey, base),
		qwen:   NewQwenTTSProvider(apiKey, base),
	}
}

func (r *QianwenTTSRouter) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	v := strings.ToLower(req.Voice)
	if strings.HasPrefix(v, "long") || strings.HasPrefix(v, "loong") {
		return r.aliyun.AudioGenerate(ctx, req)
	}
	return r.qwen.AudioGenerate(ctx, req)
}

func (r *QianwenTTSRouter) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("QianwenTTSRouter: Generate not supported; use QianwenProvider for LLM")
}
func (r *QianwenTTSRouter) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("QianwenTTSRouter: GenerateStream not supported")
}
func (r *QianwenTTSRouter) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("QianwenTTSRouter: Embed not supported")
}
func (r *QianwenTTSRouter) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("QianwenTTSRouter: ImageGenerate not supported")
}
func (r *QianwenTTSRouter) GetName() string   { return "qianwen-tts-router" }
func (r *QianwenTTSRouter) GetModels() []string { return nil }
func (r *QianwenTTSRouter) HealthCheck(ctx context.Context) error {
	return r.qwen.HealthCheck(ctx)
}
