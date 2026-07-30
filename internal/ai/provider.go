package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultProviderTimeout 是 provider HTTP client 的默认超时时间。
const DefaultProviderTimeout = 300 * time.Second

// ErrVideoConcurrentLimit 视频 API 并发限制错误（由 volcengine 等 provider 返回）。
var ErrVideoConcurrentLimit = fmt.Errorf("video API concurrent limit reached")

// ResolveTimeout 将秒数配置转换为 time.Duration；0 或负数返回默认值。
func ResolveTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultProviderTimeout
	}
	return time.Duration(seconds) * time.Second
}

// TextProvider 文本生成接口
type TextProvider interface {
	ProviderMeta
	// Generate 生成文本
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
	// GenerateStream 流式生成文本
	GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error)
}

// EmbeddingProvider 向量生成接口
type EmbeddingProvider interface {
	ProviderMeta
	// Embed 生成向量
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ImageProvider 图像生成接口
type ImageProvider interface {
	ProviderMeta
	// ImageGenerate 生成图像
	ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error)
}

// AudioProvider 音频生成接口
type AudioProvider interface {
	ProviderMeta
	// AudioGenerate 生成音频
	AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error)
}

// ProviderMeta 所有 AI 提供者必须实现的元数据接口
type ProviderMeta interface {
	// GetName 获取提供者名称
	GetName() string
	// HealthCheck 健康检查
	HealthCheck(ctx context.Context) error
}

// GenerateRequest 生成请求
type GenerateRequest struct {
	Model        string                 `json:"model"`
	APIVersion   string                 `json:"api_version,omitempty"` // 模型级 API 版本覆盖（目前仅 Azure 使用）
	Messages     []ChatMessage          `json:"messages"`
	SystemPrompt string                 `json:"system_prompt"`
	Temperature  float64                `json:"temperature"`
	MaxTokens    int                    `json:"max_tokens"`
	TopP         float64                `json:"top_p"`
	TopK         int                    `json:"top_k"`
	Stop         []string               `json:"stop"`
	Stream       bool                   `json:"stream"`
	Extra        map[string]interface{} `json:"extra"`
}

// ChatMessage 聊天消息
type ChatMessage struct {
	Role      string   `json:"role"` // system, user, assistant
	Content   string   `json:"content"`
	Name      string   `json:"name,omitempty"`
	ImageURLs []string `json:"image_urls,omitempty"` // Vision多模态：图片URL列表
}

// GenerateResponse 生成响应
type GenerateResponse struct {
	Content     string `json:"content"`
	Model       string `json:"model"`
	Tokens      int    `json:"tokens"`
	InputTokens int    `json:"input_tokens"`
	StopReason  string `json:"stop_reason"`
	FinishTime  int64  `json:"finish_time"` // 耗时(ms)
	Error       string `json:"error,omitempty"`
	// Fix 4: 实际执行的模型 DB ID（fallback 时与 PrimaryModelID 不同，用于 usage log 精确记录）
	ActualModelID uint `json:"actual_model_id,omitempty"`
}

func (r *GenerateResponse) GetError() string { return r.Error }

// ImageGenerateRequest 图像生成请求
type ImageGenerateRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt"`
	NegativePrompt  string                 `json:"negative_prompt,omitempty"`
	Size            string                 `json:"size"` // 512x512, 1024x1024, 2K, 4K 等
	Seed            int64                  `json:"seed"`
	Style           string                 `json:"style"`                      // realistic, anime, cartoon, etc.
	ReferenceImage  string                 `json:"reference_image,omitempty"`  // 单张参考图 base64（向后兼容）
	ReferenceImages []string               `json:"reference_images,omitempty"` // 多张参考图 base64（最多14张，优先级高于 ReferenceImage）
	ReferenceURL    string                 `json:"reference_url,omitempty"`    // 单张参考图原始 HTTP URL（支持 URL 的接口优先使用）
	ReferenceURLs   []string               `json:"reference_urls,omitempty"`   // 多张参考图原始 HTTP URL
	ControlNets     []ControlNet           `json:"control_nets,omitempty"`
	Extra           map[string]interface{} `json:"extra,omitempty"` // 提供者特定扩展参数

	// Seedream 5.0/4.5/4.0 扩展参数
	// SequentialImageGeneration：组图模式，"auto"=模型自动判断，"disabled"=只生单张（默认）
	SequentialImageGeneration string `json:"sequential_image_generation,omitempty"`
	// SequentialImageGenerationMaxImages：组图最多生成张数，范围 [1,15]，默认 15
	SequentialImageGenerationMaxImages int `json:"sequential_image_generation_max_images,omitempty"`
	// OutputFormat：生成图片格式，"jpeg"（默认）或 "png"，仅 Seedream 5.0 lite 支持
	OutputFormat string `json:"output_format,omitempty"`
	// Watermark：是否添加水印，nil=使用 provider 默认值（false），false=不加水印，true=加水印
	Watermark *bool `json:"watermark,omitempty"`
	// ResponseFormat：返回格式，"url"（默认）或 "b64_json"
	ResponseFormat string `json:"response_format,omitempty"`

	// CFGScale 图像生成引导比例（仅部分 volcengine/jimeng 接口使用）
	CFGScale float64 `json:"cfg_scale,omitempty"`
}

// ControlNet 控制网
type ControlNet struct {
	Type   string  `json:"type"`  // canny, depth, pose, etc.
	Image  string  `json:"image"` // 图像URL或base64
	Weight float64 `json:"weight"`
}

// ImageResponse 图像响应
type ImageResponse struct {
	URL       string   `json:"url"`                // 主图 URL（第一张）
	URLs      []string `json:"urls,omitempty"`     // 组图 URL 列表（Seedream 5.0/4.5/4.0 auto 模式）
	Sizes     []string `json:"sizes,omitempty"`    // 各图片宽高像素值，如 ["2048x2048", ...]
	B64JSON   string   `json:"b64_json,omitempty"` // base64 格式（response_format=b64_json）
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Seed      int64    `json:"seed"`
	Steps     int      `json:"steps"`
	LatencyMs int64    `json:"latency_ms"`
	Error     string   `json:"error,omitempty"`
}

func (r *ImageResponse) GetError() string { return r.Error }

// AudioGenerateRequest 音频生成请求
type AudioGenerateRequest struct {
	Model           string  `json:"model"`
	Text            string  `json:"text"`
	Voice           string  `json:"voice"`            // 声音ID（TTS）
	Speed           float64 `json:"speed"`            // 语速（TTS）
	Pitch           float64 `json:"pitch"`            // 音调（TTS）
	Loudness        float64 `json:"loudness"`         // 音量，[-10,10]；腾讯TTS直接映射；Doubao V3 映射 loudness_rate
	Emotion         string  `json:"emotion"`          // 情感（TTS）
	Language        string  `json:"language"`         // 语言（explicit_language：zh-cn/en/ja/es-mx/id/pt-br/ko）
	Dialect         string  `json:"dialect"`          // 方言（explicit_dialect，需配合支持方言的音色）
	SectionID       string  `json:"section_id"`       // 段落标识，用于跨包语义保持（V3）
	SilenceDuration int     `json:"silence_duration"` // 文末静音时长 ms，范围 [0,30000]（V3）
	DisableMarkdown bool    `json:"disable_markdown"` // 开启 Markdown 过滤（true=过滤语法符号）
	Duration        float64 `json:"duration"`         // 音效时长（秒，文生音效 API 使用，如 Kling SFX）
	Lyrics          string  `json:"lyrics"`           // 歌词（文生音乐 API 使用，如 MiniMax Music；用 \n 分隔每行）
	Instrumental    bool    `json:"instrumental"`     // 是否生成纯音乐（无人声），MiniMax Music 使用
	// 腾讯云 TTS 扩展参数
	FastVoiceType  string `json:"fast_voice_type,omitempty"` // 一句话复刻音色ID（VoiceType=200000000 时填入）
	EnableSubtitle bool   `json:"enable_subtitle,omitempty"` // 是否返回字/词级时间戳
	Codec          string `json:"codec,omitempty"`           // 音频格式：mp3（默认）/ wav / pcm
	SampleRate     int    `json:"sample_rate,omitempty"`     // 采样率：8000 / 16000（默认）/ 24000
	SegmentRate    int    `json:"segment_rate,omitempty"`    // 断句敏感阈值 [0,1,2]，越大越不断句
}

// TTSSubtitle 语音合成时间戳（字/词级）
type TTSSubtitle struct {
	BeginIndex int    `json:"begin_index"` // 文本起始字符索引（含）
	EndIndex   int    `json:"end_index"`   // 文本结束字符索引（不含）
	BeginTime  int    `json:"begin_time"`  // 起始时间 ms
	EndTime    int    `json:"end_time"`    // 结束时间 ms
	Phoneme    string `json:"phoneme"`     // 拼音（如 ni2）
	Text       string `json:"text"`        // 对应文字
}

// AudioResponse 音频响应
type AudioResponse struct {
	URL       string        `json:"url"`
	Duration  float64       `json:"duration"` // 秒
	Format    string        `json:"format"`   // mp3, wav, etc.
	LatencyMs int64         `json:"latency_ms"`
	Subtitles []TTSSubtitle `json:"subtitles,omitempty"` // 时间戳（EnableSubtitle=true 时填充）
	Error     string        `json:"error,omitempty"`
}

func (r *AudioResponse) GetError() string { return r.Error }

// MultimodalEmbedItem 多模态 Embedding 输入元素
type MultimodalEmbedItem struct {
	Type     string `json:"type"`                // "text" | "image_url" | "video_url"
	Text     string `json:"text,omitempty"`      // type=text
	ImageURL string `json:"image_url,omitempty"` // type=image_url：HTTP URL 或 data URI
	VideoURL string `json:"video_url,omitempty"` // type=video_url：HTTP URL

	// video_url 专属可选参数（仅 doubao-embedding-vision-251215 及后续版本支持）
	VideoFPS            *float64 `json:"fps,omitempty"`              // 0.2-5，采帧率
	VideoMaxTokens      *int     `json:"max_video_tokens,omitempty"` // 10240-204800
	VideoMinFrameTokens *int     `json:"min_frame_tokens,omitempty"` // 16-128
	VideoMaxFrameTokens *int     `json:"max_frame_tokens,omitempty"` // 128-640
	VideoMinFrames      *int     `json:"min_frames,omitempty"`       // 5-16
}

// MultimodalSparseEmbeddingConfig 稀疏向量开关配置（仅纯文本输入支持）
type MultimodalSparseEmbeddingConfig struct {
	Enabled bool `json:"enabled"` // true=enabled，false=disabled（默认）
}

// MultimodalMultiEmbeddingConfig 多向量（multi-vector）输出配置
type MultimodalMultiEmbeddingConfig struct {
	Enabled     bool   `json:"enabled"`               // true=enabled，false=disabled（默认）
	Compression string `json:"compression,omitempty"` // "zstd" 或 "blosc2"；不传=不压缩
}

// MultimodalEmbedRequest 多模态 Embedding 请求
type MultimodalEmbedRequest struct {
	Model           string                           `json:"model"`
	Input           []MultimodalEmbedItem            `json:"input"`
	Dimensions      *int                             `json:"dimensions,omitempty"`       // 1024 或 2048，仅 vision-250615+ 支持
	Instructions    string                           `json:"instructions,omitempty"`     // 推理提示词
	SparseEmbedding *MultimodalSparseEmbeddingConfig `json:"sparse_embedding,omitempty"` // 稀疏向量开关
	MultiEmbedding  *MultimodalMultiEmbeddingConfig  `json:"multi_embedding,omitempty"`  // 多向量输出配置
}

// SparseEmbedPoint 稀疏向量中的一个非零元素
type SparseEmbedPoint struct {
	Index int     `json:"index"`
	Value float64 `json:"value"`
}

// MultimodalEmbedResponse 多模态 Embedding 响应
type MultimodalEmbedResponse struct {
	Embedding       []float32          `json:"embedding"`                  // 稠密向量
	SparseEmbedding []SparseEmbedPoint `json:"sparse_embedding,omitempty"` // 稀疏向量（sparse_embedding.enabled 时返回）
	MultiEmbedding  [][]float32        `json:"multi_embedding,omitempty"`  // 多向量（multi_embedding.enabled 时返回，解压后）
	TokensUsed      int                `json:"tokens_used"`
	TextTokens      int                `json:"text_tokens"`  // 文本/时间轴 token 数
	ImageTokens     int                `json:"image_tokens"` // 图片/视频帧 token 数
	VideoTokens     int                `json:"video_tokens"` // 视频 token 数（SDK ≥ v1.2.38 起填充）
	Model           string             `json:"model"`
}

// MultimodalEmbedder 多模态 Embedding 扩展接口（可选，按需类型断言）
type MultimodalEmbedder interface {
	EmbedMultimodal(ctx context.Context, req *MultimodalEmbedRequest) (*MultimodalEmbedResponse, error)
}

// ImageProviderEntry 图像生成提供者入口
type ImageProviderEntry struct {
	ProviderName string
	Model        string
	Size         string
}

// GenerateRequestBuilder 生成请求构建器
type GenerateRequestBuilder struct {
	req *GenerateRequest
}

func NewGenerateRequestBuilder() *GenerateRequestBuilder {
	return &GenerateRequestBuilder{
		req: &GenerateRequest{
			Temperature: 0.7,
			TopP:        0.9,
			TopK:        40,
		},
	}
}

func (b *GenerateRequestBuilder) Model(model string) *GenerateRequestBuilder {
	b.req.Model = model
	return b
}

func (b *GenerateRequestBuilder) SystemPrompt(prompt string) *GenerateRequestBuilder {
	b.req.SystemPrompt = prompt
	return b
}

func (b *GenerateRequestBuilder) UserMessage(content string) *GenerateRequestBuilder {
	b.req.Messages = append(b.req.Messages, ChatMessage{
		Role:    "user",
		Content: content,
	})
	return b
}

func (b *GenerateRequestBuilder) AssistantMessage(content string) *GenerateRequestBuilder {
	b.req.Messages = append(b.req.Messages, ChatMessage{
		Role:    "assistant",
		Content: content,
	})
	return b
}

func (b *GenerateRequestBuilder) Temperature(t float64) *GenerateRequestBuilder {
	b.req.Temperature = t
	return b
}

func (b *GenerateRequestBuilder) MaxTokens(tokens int) *GenerateRequestBuilder {
	b.req.MaxTokens = tokens
	return b
}

func (b *GenerateRequestBuilder) TopP(p float64) *GenerateRequestBuilder {
	b.req.TopP = p
	return b
}

func (b *GenerateRequestBuilder) TopK(k int) *GenerateRequestBuilder {
	b.req.TopK = k
	return b
}

func (b *GenerateRequestBuilder) Stop(stop []string) *GenerateRequestBuilder {
	b.req.Stop = stop
	return b
}

func (b *GenerateRequestBuilder) Extra(key string, value interface{}) *GenerateRequestBuilder {
	if b.req.Extra == nil {
		b.req.Extra = make(map[string]interface{})
	}
	b.req.Extra[key] = value
	return b
}

func (b *GenerateRequestBuilder) Build() *GenerateRequest {
	return b.req
}

// CostEstimator 成本估算器
type CostEstimator struct {
	// 每1000 token的成本（美元）
	tokenCosts map[string]map[string]float64
}

func NewCostEstimator() *CostEstimator {
	return &CostEstimator{
		tokenCosts: map[string]map[string]float64{
			"openai": {
				"gpt-4":         0.03, // 输入
				"gpt-4-output":  0.06, // 输出
				"gpt-4-turbo":   0.01,
				"gpt-3.5-turbo": 0.0005,
			},
			"anthropic": {
				"claude-3-opus":   0.015,
				"claude-3-sonnet": 0.003,
				"claude-3-haiku":  0.00025,
			},
		},
	}
}

// EstimateCost 估算成本
func (e *CostEstimator) EstimateCost(provider, model string, inputTokens, outputTokens int) float64 {
	if providerCosts, ok := e.tokenCosts[provider]; ok {
		if cost, ok := providerCosts[model]; ok {
			return float64(inputTokens)/1000*cost + float64(outputTokens)/1000*cost*2
		}
	}
	return 0
}

// UsageLogger 使用日志记录器
type UsageLogger struct {
	mu   sync.Mutex
	logs []UsageLogEntry
}

type UsageLogEntry struct {
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	TaskType     string    `json:"task_type"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	LatencyMs    int64     `json:"latency_ms"`
	Cost         float64   `json:"cost"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

// Log 记录使用
func (l *UsageLogger) Log(entry UsageLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, entry)
}

// GetStats 获取统计信息
func (l *UsageLogger) GetStats(provider, model string) *UsageStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	stats := &UsageStats{}
	for _, log := range l.logs {
		if provider != "" && log.Provider != provider {
			continue
		}
		if model != "" && log.Model != model {
			continue
		}
		stats.TotalRequests++
		stats.TotalInputTokens += log.InputTokens
		stats.TotalOutputTokens += log.OutputTokens
		stats.TotalCost += log.Cost
		stats.TotalLatency += log.LatencyMs
		if log.Success {
			stats.SuccessCount++
		}
	}
	if stats.TotalRequests > 0 {
		stats.AverageLatency = stats.TotalLatency / int64(stats.TotalRequests)
		stats.SuccessRate = float64(stats.SuccessCount) / float64(stats.TotalRequests)
	}
	return stats
}

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests     int     `json:"total_requests"`
	SuccessCount      int     `json:"success_count"`
	TotalInputTokens  int     `json:"total_input_tokens"`
	TotalOutputTokens int     `json:"total_output_tokens"`
	TotalCost         float64 `json:"total_cost"`
	TotalLatency      int64   `json:"total_latency"`
	AverageLatency    int64   `json:"average_latency"`
	SuccessRate       float64 `json:"success_rate"`
}

// ModelHealthChecker 模型健康检查器
type ModelHealthChecker struct {
	mu            sync.RWMutex
	providers     map[string]ProviderMeta
	checkInterval time.Duration
	lastCheck     map[string]*HealthStatus
}

type HealthStatus struct {
	Healthy   bool      `json:"healthy"`
	LatencyMs int64     `json:"latency_ms"`
	Message   string    `json:"message"`
	CheckTime time.Time `json:"check_time"`
}

func NewModelHealthChecker() *ModelHealthChecker {
	return &ModelHealthChecker{
		providers:     make(map[string]ProviderMeta),
		checkInterval: 5 * time.Minute,
		lastCheck:     make(map[string]*HealthStatus),
	}
}

func (h *ModelHealthChecker) Register(name string, provider ProviderMeta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.providers[name] = provider
}

func (h *ModelHealthChecker) Check(name string) (*HealthStatus, error) {
	h.mu.RLock()
	provider, ok := h.providers[name]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", name)
	}

	start := time.Now()
	err := provider.HealthCheck(context.Background())
	latency := time.Since(start).Milliseconds()

	status := &HealthStatus{
		Healthy:   err == nil,
		LatencyMs: latency,
		Message:   "",
		CheckTime: time.Now(),
	}

	if err != nil {
		status.Message = err.Error()
	}

	h.mu.Lock()
	h.lastCheck[name] = status
	h.mu.Unlock()
	return status, nil
}

func (h *ModelHealthChecker) CheckAll() map[string]*HealthStatus {
	h.mu.RLock()
	names := make([]string, 0, len(h.providers))
	for name := range h.providers {
		names = append(names, name)
	}
	h.mu.RUnlock()

	results := make(map[string]*HealthStatus)
	for _, name := range names {
		status, _ := h.Check(name)
		results[name] = status
	}
	return results
}

// FallbackManager 故障转移管理器
type FallbackManager struct {
	primary   string
	fallbacks []string
}

func NewFallbackManager(primary string, fallbacks ...string) *FallbackManager {
	return &FallbackManager{
		primary:   primary,
		fallbacks: fallbacks,
	}
}

func (f *FallbackManager) GetPrimary() string {
	return f.primary
}

func (f *FallbackManager) GetFallbacks() []string {
	return f.fallbacks
}

func (f *FallbackManager) GetNext(current string) string {
	// 如果当前是primary，返回第一个fallback
	if current == f.primary && len(f.fallbacks) > 0 {
		return f.fallbacks[0]
	}
	// 查找当前在fallbacks中的位置
	for i, fb := range f.fallbacks {
		if fb == current && i+1 < len(f.fallbacks) {
			return f.fallbacks[i+1]
		}
	}
	return "" // 没有更多fallback
}

// IsTimeoutError 判断错误是否为客户端超时（HTTP Client.Timeout / context deadline）。
// 超时代表 AI 服务处理时间超过 HTTP client 设定值，重试只会让情况更糟；应 fail-fast。
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "client.timeout") ||
		strings.Contains(msg, "request timed out")
}

// JSON格式化工具
func FormatJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}


