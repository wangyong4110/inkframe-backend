package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// AIProvider AI提供者接口
type AIProvider interface {
	// Generate 生成文本
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)

	// GenerateStream 流式生成文本
	GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error)

	// Embed 生成向量
	Embed(ctx context.Context, text string) ([]float32, error)

	// ImageGenerate 生成图像
	ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error)

	// AudioGenerate 生成音频
	AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error)

	// GetName 获取提供者名称
	GetName() string

	// GetModels 获取支持的模型列表
	GetModels() []string

	// HealthCheck 健康检查
	HealthCheck(ctx context.Context) error
}

// GenerateRequest 生成请求
type GenerateRequest struct {
	Model       string            `json:"model"`
	Messages    []ChatMessage     `json:"messages"`
	SystemPrompt string           `json:"system_prompt"`
	Temperature float64           `json:"temperature"`
	MaxTokens   int               `json:"max_tokens"`
	TopP        float64           `json:"top_p"`
	TopK        int               `json:"top_k"`
	Stop        []string          `json:"stop"`
	Stream      bool              `json:"stream"`
	Extra       map[string]interface{} `json:"extra"`
}

// ChatMessage 聊天消息
type ChatMessage struct {
	Role      string   `json:"role"`                 // system, user, assistant
	Content   string   `json:"content"`
	Name      string   `json:"name,omitempty"`
	ImageURLs []string `json:"image_urls,omitempty"` // Vision多模态：图片URL列表
}

// GenerateResponse 生成响应
type GenerateResponse struct {
	Content    string   `json:"content"`
	Model     string   `json:"model"`
	Tokens    int      `json:"tokens"`
	InputTokens int   `json:"input_tokens"`
	StopReason string  `json:"stop_reason"`
	FinishTime int64   `json:"finish_time"` // 耗时(ms)
	Error     string   `json:"error,omitempty"`
}

// ImageGenerateRequest 图像生成请求
type ImageGenerateRequest struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	NegativePrompt string   `json:"negative_prompt,omitempty"`
	Size           string   `json:"size"`  // 512x512, 1024x1024, etc.
	Steps          int      `json:"steps"`
	CFGScale       float64  `json:"cfg_scale"`
	Seed           int64    `json:"seed"`
	Style          string   `json:"style"` // realistic, anime, cartoon, etc.
	ReferenceImage string   `json:"reference_image,omitempty"` // 用于一致性控制
	ControlNets    []ControlNet `json:"control_nets,omitempty"`
}

// ControlNet 控制网
type ControlNet struct {
	Type    string `json:"type"`    // canny, depth, pose, etc.
	Image   string `json:"image"`   // 图像URL或base64
	Weight  float64 `json:"weight"`
}

// ImageResponse 图像响应
type ImageResponse struct {
	URL       string   `json:"url"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Seed      int64    `json:"seed"`
	Steps     int      `json:"steps"`
	LatencyMs int64    `json:"latency_ms"`
	Error     string   `json:"error,omitempty"`
}

// AudioGenerateRequest 音频生成请求
type AudioGenerateRequest struct {
	Model      string `json:"model"`
	Text       string `json:"text"`
	Voice      string `json:"voice"`      // 声音ID
	Speed      float64 `json:"speed"`     // 语速
	Pitch      float64 `json:"pitch"`     // 音调
	Emotion    string `json:"emotion"`    // 情感
	Language   string `json:"language"`   // 语言
}

// AudioResponse 音频响应
type AudioResponse struct {
	URL       string `json:"url"`
	Duration  float64 `json:"duration"` // 秒
	Format    string `json:"format"`    // mp3, wav, etc.
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// ImageProviderEntry 图像生成提供者入口
type ImageProviderEntry struct {
	ProviderName string
	Model        string
	Size         string
}

// ModelManager 模型管理器
type ModelManager struct {
	providers       map[string]AIProvider
	defaultProvider string
	imageProviders  []ImageProviderEntry
}

func NewModelManager() *ModelManager {
	return &ModelManager{
		providers: make(map[string]AIProvider),
	}
}

// RegisterImageProvider 注册图像生成提供者候选列表（按调用顺序尝试）。
// provider 不需要在注册时已存在；实际可用性在请求时由 GetProvider 决定。
func (m *ModelManager) RegisterImageProvider(name, model, size string) {
	m.imageProviders = append(m.imageProviders, ImageProviderEntry{name, model, size})
}

// GetImageProviders 返回已注册的图像生成提供者列表
func (m *ModelManager) GetImageProviders() []ImageProviderEntry {
	return m.imageProviders
}

// RegisterProvider 注册AI提供者
func (m *ModelManager) RegisterProvider(name string, provider AIProvider) {
	m.providers[name] = provider
}

// SetDefault 设置默认提供者
func (m *ModelManager) SetDefault(name string) error {
	if _, ok := m.providers[name]; !ok {
		return fmt.Errorf("provider not found: %s", name)
	}
	m.defaultProvider = name
	return nil
}

// GetProvider 获取提供者
func (m *ModelManager) GetProvider(name string) (AIProvider, error) {
	if name == "" {
		name = m.defaultProvider
	}
	provider, ok := m.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", name)
	}
	return provider, nil
}

// ListProviders 列出所有提供者
func (m *ModelManager) ListProviders() []string {
	names := []string{}
	for name := range m.providers {
		names = append(names, name)
	}
	return names
}

// GenerateRequestBuilder 生成请求构建器
type GenerateRequestBuilder struct {
	req *GenerateRequest
}

func NewGenerateRequestBuilder() *GenerateRequestBuilder {
	return &GenerateRequestBuilder{
		req: &GenerateRequest{
			Temperature: 0.7,
			MaxTokens:   4096,
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

// TaskTypeToModelMapping 任务类型到模型的默认映射
var TaskTypeToModelMapping = map[string]map[string]string{
	"outline": { // 大纲生成
		"openai":    "gpt-4-turbo",
		"anthropic": "claude-3-opus",
		"google":    "gemini-pro",
	},
	"chapter": { // 章节生成
		"openai":    "gpt-4-turbo",
		"anthropic": "claude-3-sonnet",
		"google":    "gemini-pro",
	},
	"character": { // 角色生成
		"openai":    "gpt-4",
		"anthropic": "claude-3-opus",
		"google":    "gemini-pro",
	},
	"worldview": { // 世界观生成
		"openai":    "gpt-4",
		"anthropic": "claude-3-opus",
		"google":    "gemini-pro",
	},
	"dialogue": { // 对话生成
		"openai":    "gpt-3.5-turbo",
		"anthropic": "claude-3-haiku",
		"google":    "gemini-pro",
	},
	"description": { // 描述生成
		"openai":    "gpt-4",
		"anthropic": "claude-3-sonnet",
		"google":    "gemini-pro",
	},
	"summary": { // 摘要生成
		"openai":    "gpt-3.5-turbo",
		"anthropic": "claude-3-haiku",
		"google":    "gemini-pro",
	},
	"image": { // 图像生成
		"openai":    "dall-e-3",
		"stability": "stable-diffusion-xl",
	},
	"voice": { // 语音生成
		"elevenlabs": "elevenlabs",
		"azure":      "azure-tts",
	},
}

// GetRecommendedModel 获取推荐模型
func GetRecommendedModel(taskType, provider string) string {
	if mapping, ok := TaskTypeToModelMapping[taskType]; ok {
		if model, ok := mapping[provider]; ok {
			return model
		}
	}
	return ""
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
				"gpt-4":           0.03,  // 输入
				"gpt-4-output":    0.06,  // 输出
				"gpt-4-turbo":     0.01,
				"gpt-3.5-turbo":   0.0005,
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
	logs []UsageLogEntry
}

type UsageLogEntry struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	TaskType    string    `json:"task_type"`
	InputTokens int       `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	LatencyMs   int64     `json:"latency_ms"`
	Cost        float64   `json:"cost"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// Log 记录使用
func (l *UsageLogger) Log(entry UsageLogEntry) {
	l.logs = append(l.logs, entry)
}

// GetStats 获取统计信息
func (l *UsageLogger) GetStats(provider, model string) *UsageStats {
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
	TotalRequests   int     `json:"total_requests"`
	SuccessCount    int     `json:"success_count"`
	TotalInputTokens  int    `json:"total_input_tokens"`
	TotalOutputTokens int   `json:"total_output_tokens"`
	TotalCost       float64 `json:"total_cost"`
	TotalLatency    int64   `json:"total_latency"`
	AverageLatency  int64   `json:"average_latency"`
	SuccessRate     float64 `json:"success_rate"`
}

// ModelHealthChecker 模型健康检查器
type ModelHealthChecker struct {
	providers map[string]AIProvider
	checkInterval time.Duration
	lastCheck map[string]*HealthStatus
}

type HealthStatus struct {
	Healthy   bool      `json:"healthy"`
	LatencyMs int64     `json:"latency_ms"`
	Message   string    `json:"message"`
	CheckTime time.Time `json:"check_time"`
}

func NewModelHealthChecker() *ModelHealthChecker {
	return &ModelHealthChecker{
		providers:  make(map[string]AIProvider),
		checkInterval: 5 * time.Minute,
		lastCheck:    make(map[string]*HealthStatus),
	}
}

func (h *ModelHealthChecker) Register(name string, provider AIProvider) {
	h.providers[name] = provider
}

func (h *ModelHealthChecker) Check(name string) (*HealthStatus, error) {
	provider, ok := h.providers[name]
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

	h.lastCheck[name] = status
	return status, nil
}

func (h *ModelHealthChecker) CheckAll() map[string]*HealthStatus {
	results := make(map[string]*HealthStatus)
	for name := range h.providers {
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

// JSON格式化工具
func FormatJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// RetryProvider 带指数退避重试的 AI Provider 包装器
type RetryProvider struct {
	provider   AIProvider
	maxRetries int
	baseDelay  time.Duration // 基础等待时间，默认 500ms
}

// NewRetryProvider 创建重试包装器（最多重试 maxRetries 次，基础延迟 baseDelay）
func NewRetryProvider(provider AIProvider, maxRetries int, baseDelay time.Duration) *RetryProvider {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	return &RetryProvider{
		provider:   provider,
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
	}
}

// isRetryable 判断错误是否值得重试
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	retryKeywords := []string{"timeout", "connection refused", "temporary", "429", "502", "503", "rate limit", "overloaded"}
	for _, kw := range retryKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// isRetryableStatus 判断 HTTP 状态码是否值得重试
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

func (p *RetryProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	var lastErr error
	for attempt := 0; attempt < p.maxRetries; attempt++ {
		if attempt > 0 {
			delay := p.baseDelay * time.Duration(1<<uint(attempt-1))
			log.Printf("RetryProvider.Generate: attempt %d, waiting %v", attempt+1, delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := p.provider.Generate(ctx, req)
		if err != nil {
			if isRetryable(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		// 检查响应中是否包含可重试错误
		if resp != nil && resp.Error != "" {
			if isRetryable(fmt.Errorf(resp.Error)) {
				lastErr = fmt.Errorf(resp.Error)
				continue
			}
			return resp, nil
		}
		return resp, nil
	}
	return nil, fmt.Errorf("RetryProvider.Generate failed after %d attempts: %w", p.maxRetries, lastErr)
}

func (p *RetryProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	var lastErr error
	for attempt := 0; attempt < p.maxRetries; attempt++ {
		if attempt > 0 {
			delay := p.baseDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch, err := p.provider.GenerateStream(ctx, req)
		if err != nil {
			if isRetryable(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		return ch, nil
	}
	return nil, fmt.Errorf("RetryProvider.GenerateStream failed after %d attempts: %w", p.maxRetries, lastErr)
}

func (p *RetryProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	for attempt := 0; attempt < p.maxRetries; attempt++ {
		if attempt > 0 {
			delay := p.baseDelay * time.Duration(1<<uint(attempt-1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		result, err := p.provider.Embed(ctx, text)
		if err != nil {
			if isRetryable(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("RetryProvider.Embed failed after %d attempts: %w", p.maxRetries, lastErr)
}

func (p *RetryProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return p.provider.ImageGenerate(ctx, req)
}

func (p *RetryProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	return p.provider.AudioGenerate(ctx, req)
}

func (p *RetryProvider) GetName() string      { return p.provider.GetName() }
func (p *RetryProvider) GetModels() []string  { return p.provider.GetModels() }
func (p *RetryProvider) HealthCheck(ctx context.Context) error { return p.provider.HealthCheck(ctx) }

// SwitchProvider 在 ModelManager 中动态切换某个任务类型的默认 Provider
func (m *ModelManager) SwitchProvider(providerName string) error {
	if _, ok := m.providers[providerName]; !ok {
		return fmt.Errorf("provider not found: %s", providerName)
	}
	m.defaultProvider = providerName
	return nil
}

// WrapWithRetry 将已注册的 Provider 包装为 RetryProvider
func (m *ModelManager) WrapWithRetry(name string, maxRetries int, baseDelay time.Duration) error {
	provider, ok := m.providers[name]
	if !ok {
		return fmt.Errorf("provider not found: %s", name)
	}
	// 避免重复包装
	if _, already := provider.(*RetryProvider); already {
		return nil
	}
	m.providers[name] = NewRetryProvider(provider, maxRetries, baseDelay)
	return nil
}
