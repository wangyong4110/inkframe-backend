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

// DeepSeekProvider DeepSeek AI 提供者
// API 兼容 OpenAI 格式：https://api.deepseek.com/v1
type DeepSeekProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

// NewDeepSeekProvider 创建 DeepSeek 提供者
func NewDeepSeekProvider(apiKey, endpoint, model string) *DeepSeekProvider {
	if endpoint == "" {
		endpoint = "https://api.deepseek.com/v1"
	}
	if model == "" {
		model = "deepseek-chat"
	}
	return &DeepSeekProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *DeepSeekProvider) GetName() string { return "deepseek" }

func (p *DeepSeekProvider) GetModels() []string {
	return []string{
		"deepseek-chat",
		"deepseek-reasoner",
	}
}

func (p *DeepSeekProvider) HealthCheck(ctx context.Context) error {
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

func (p *DeepSeekProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
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
			Error:      fmt.Sprintf("DeepSeek API 错误: %s", string(respBody)),
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

	return &GenerateResponse{
		Content:     result.Choices[0].Message.Content,
		Model:       result.Model,
		InputTokens: result.Usage.PromptTokens,
		Tokens:      result.Usage.TotalTokens,
		StopReason:  result.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *DeepSeekProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
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
				ch <- &GenerateResponse{Content: chunk.Choices[0].Delta.Content}
			}
		}
	}()

	return ch, nil
}

func (p *DeepSeekProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("DeepSeek 暂不支持 Embedding")
}

func (p *DeepSeekProvider) ImageGenerate(_ context.Context, _ *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("DeepSeek 暂不支持图像生成")
}

func (p *DeepSeekProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("DeepSeek 暂不支持音频生成")
}
