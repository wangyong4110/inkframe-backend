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

// OpenAICompatProvider 通用 OpenAI 兼容接口提供者。
// 适用于所有遵循 OpenAI REST 规范（/chat/completions）的第三方服务。
type OpenAICompatProvider struct {
	name     string // provider 唯一标识，如 "xai"、"mistral"
	apiKey   string
	endpoint string
	model    string
	models   []string
	client   *http.Client
}

// NewOpenAICompatProvider 通用构造器。
// providerName  — GetName() 返回值，也用于日志
// defaultModels — GetModels() 返回值；首个元素也作为 defaultModel
func NewOpenAICompatProvider(providerName, apiKey, endpoint, defaultModel string, defaultModels []string, timeout time.Duration) *OpenAICompatProvider {
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	return &OpenAICompatProvider{
		name:     providerName,
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    defaultModel,
		models:   defaultModels,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *OpenAICompatProvider) GetName() string    { return p.name }
func (p *OpenAICompatProvider) GetModels() []string { return p.models }

func (p *OpenAICompatProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.endpoint+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}
	return nil
}

func (p *OpenAICompatProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = p.model
	}

	messages := make([]map[string]string, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.SystemPrompt})
	}
	for _, m := range req.Messages {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
	}

	apiReq := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}
	if req.Temperature > 0 {
		apiReq["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		apiReq["max_tokens"] = req.MaxTokens
	}
	if req.TopP > 0 {
		apiReq["top_p"] = req.TopP
	}
	// 透传 Extra 字段（如 thinking/reasoning_effort/enable_thinking 等模型扩展参数）
	for k, v := range req.Extra {
		apiReq[k] = v
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return &GenerateResponse{
			Error:      fmt.Sprintf("%s API 错误: %s", p.name, string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var result OpenAIChatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	if len(result.Choices) == 0 {
		return &GenerateResponse{Error: "no choices returned", FinishTime: time.Since(start).Milliseconds()}, nil
	}

	content := result.Choices[0].Message.Content
	// 推理模型（如 DeepSeek R1）思考阶段 content 为空时，回退到 reasoning_content
	if content == "" {
		content = result.Choices[0].Message.ReasoningContent
	}
	return &GenerateResponse{
		Content:     content,
		Model:       result.Model,
		InputTokens: result.Usage.PromptTokens,
		Tokens:      result.Usage.CompletionTokens,
		StopReason:  result.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAICompatProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)

	go func() {
		defer close(ch)

		model := req.Model
		if model == "" {
			model = p.model
		}

		messages := make([]map[string]string, 0, len(req.Messages)+1)
		if req.SystemPrompt != "" {
			messages = append(messages, map[string]string{"role": "system", "content": req.SystemPrompt})
		}
		for _, m := range req.Messages {
			messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
		}

		apiReq := map[string]interface{}{
			"model":    model,
			"messages": messages,
			"stream":   true,
		}
		if req.Temperature > 0 {
			apiReq["temperature"] = req.Temperature
		}
		if req.MaxTokens > 0 {
			apiReq["max_tokens"] = req.MaxTokens
		}
		// 透传 Extra 字段（如 thinking/reasoning_effort 等模型扩展参数）
		for k, v := range req.Extra {
			apiReq[k] = v
		}

		body, _ := json.Marshal(apiReq)
		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

		resp, err := p.client.Do(httpReq)
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		defer resp.Body.Close()

		reader := NewSSEReader(resp.Body)
		for {
			event, err := reader.Read()
			if err != nil {
				if err != io.EOF {
					ch <- &GenerateResponse{Error: err.Error()}
				}
				break
			}
			if event.Data == "[DONE]" {
				break
			}
			var chunk OpenAIStreamChunk
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) > 0 {
				content := chunk.Choices[0].Delta.Content
				// 思考模型（如混元 Hy3、DeepSeek-R1）流式思考阶段 content 为空，回退到 reasoning_content
				if content == "" {
					content = chunk.Choices[0].Delta.ReasoningContent
				}
				if content != "" {
					ch <- &GenerateResponse{Content: content}
				}
			}
		}
	}()

	return ch, nil
}

func (p *OpenAICompatProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("%s 暂不支持 Embedding", p.name)
}

func (p *OpenAICompatProvider) ImageGenerate(_ context.Context, _ *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("%s 暂不支持图像生成", p.name)
}

func (p *OpenAICompatProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("%s 暂不支持音频生成", p.name)
}

// ─── 各 Provider 的具体构造器 ──────────────────────────────────────────────

// NewXAIProvider 创建 xAI (Grok) provider。
// API: https://api.x.ai/v1  OpenAI 兼容
func NewXAIProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.x.ai/v1"
	}
	if model == "" {
		model = "grok-3-mini"
	}
	return NewOpenAICompatProvider("xai", apiKey, endpoint, model,
		[]string{"grok-4", "grok-4-0709", "grok-3-mini", "grok-3-mini-fast"},
		timeout)
}

// NewMistralProvider 创建 Mistral AI provider。
// API: https://api.mistral.ai/v1  OpenAI 兼容
func NewMistralProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.mistral.ai/v1"
	}
	if model == "" {
		model = "mistral-large-latest"
	}
	return NewOpenAICompatProvider("mistral", apiKey, endpoint, model,
		[]string{"mistral-large-latest", "mistral-medium-latest", "mistral-small-latest", "codestral-latest"},
		timeout)
}

// NewMetaProvider 创建 Meta AI (Llama) provider。
// API: https://api.llama.com/compat/v1  OpenAI 兼容
func NewMetaProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.llama.com/compat/v1"
	}
	if model == "" {
		model = "Llama-4-Scout-17B-16E-Instruct-FP8"
	}
	return NewOpenAICompatProvider("meta", apiKey, endpoint, model,
		[]string{
			"Llama-4-Scout-17B-16E-Instruct-FP8",
			"Llama-4-Maverick-17B-128E-Instruct-FP8",
			"Llama-3.3-70B-Instruct",
		},
		timeout)
}

// NewZhipuProvider 创建智谱AI (GLM / Z.AI) provider。
// API: https://open.bigmodel.cn/api/paas/v4  OpenAI 兼容
func NewZhipuProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://open.bigmodel.cn/api/paas/v4"
	}
	if model == "" {
		model = "glm-4-plus"
	}
	return NewOpenAICompatProvider("zhipu", apiKey, endpoint, model,
		[]string{"glm-4-plus", "glm-4-flash", "glm-4-air", "glm-4-airx", "glm-z1-flash"},
		timeout)
}

// NewMoonshotProvider 创建 Moonshot AI (Kimi) provider。
// API: https://api.moonshot.cn/v1  OpenAI 兼容
func NewMoonshotProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.moonshot.cn/v1"
	}
	if model == "" {
		model = "moonshot-v1-32k"
	}
	return NewOpenAICompatProvider("moonshot", apiKey, endpoint, model,
		[]string{"moonshot-v1-8k", "moonshot-v1-32k", "moonshot-v1-128k", "kimi-k2-0711-preview"},
		timeout)
}

// NewBaiduProvider 创建百度文心一言 (ERNIE) provider。
// API: https://qianfan.baidubce.com/v2  OpenAI 兼容（需在百度智能云控制台生成 API Key）
func NewBaiduProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://qianfan.baidubce.com/v2"
	}
	if model == "" {
		model = "ernie-4.5-8k"
	}
	return NewOpenAICompatProvider("baidu", apiKey, endpoint, model,
		[]string{"ernie-4.5-8k", "ernie-4.5-128k", "ernie-3.5-8k", "ernie-speed-128k", "ernie-lite-8k"},
		timeout)
}

// NewTencentProvider 创建腾讯混元 (Hunyuan) provider。
// API: https://api.hunyuan.cloud.tencent.com/v1  OpenAI 兼容
func NewTencentProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.hunyuan.cloud.tencent.com/v1"
	}
	if model == "" {
		model = "hunyuan-turbo"
	}
	return NewOpenAICompatProvider("tencent", apiKey, endpoint, model,
		[]string{"hunyuan-turbo", "hunyuan-pro", "hunyuan-lite", "hunyuan-standard"},
		timeout)
}

// NewYiProvider 创建零一万物 (Yi) provider。
// API: https://api.lingyiwanwu.com/v1  OpenAI 兼容
func NewYiProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://api.lingyiwanwu.com/v1"
	}
	if model == "" {
		model = "yi-lightning"
	}
	return NewOpenAICompatProvider("yi", apiKey, endpoint, model,
		[]string{"yi-lightning", "yi-large", "yi-medium", "yi-large-turbo"},
		timeout)
}

// NewHunyuanProvider 创建腾讯混元 TokenHub provider（新一代，兼容 OpenAI Chat Completions API）。
// API: https://tokenhub.tencentmaas.com/v1
// 鉴权: Bearer YOUR_API_KEY（TokenHub 控制台创建）
// 支持深度思考：在 GenerateRequest.Extra 中传入 thinking/reasoning_effort 参数：
//
//	extra["thinking"] = map[string]string{"type": "enabled"}  // 开启思考模式
//	extra["reasoning_effort"] = "high"                        // 推理深度：low/medium/high
func NewHunyuanProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAICompatProvider {
	if endpoint == "" {
		endpoint = "https://tokenhub.tencentmaas.com/v1"
	}
	if model == "" {
		model = "hy3-preview"
	}
	return NewOpenAICompatProvider("hunyuan", apiKey, endpoint, model,
		[]string{"hy3-preview"},
		timeout)
}
