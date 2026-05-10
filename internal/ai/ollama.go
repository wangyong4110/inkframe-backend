package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// OllamaDefaultEndpoint Ollama OpenAI-compatible API 默认地址
	OllamaDefaultEndpoint = "http://localhost:11434/v1"
	// OllamaDefaultTimeout 本地推理可能较慢，使用较长超时
	OllamaDefaultTimeout = 600 * time.Second
)

// OllamaProvider Ollama 本地 LLM 提供者（兼容 OpenAI API）
// Ollama 通过 `ollama serve` 暴露 OpenAI-compatible REST API，无需 API Key。
// 支持文本生成和向量嵌入；不支持图像/音频生成。
type OllamaProvider struct {
	endpoint string
	model    string
	client   *http.Client
}

// NewOllamaProvider 创建 Ollama provider。
// endpoint: Ollama API 地址（含 /v1），空时使用 http://localhost:11434/v1
// model:    默认推理模型，如 llama3.2 / qwen2.5:7b / deepseek-r1:1.5b
// timeout:  HTTP 超时，0 使用默认 600s
func NewOllamaProvider(endpoint, model string, timeout time.Duration) *OllamaProvider {
	if endpoint == "" {
		endpoint = OllamaDefaultEndpoint
	}
	if model == "" {
		model = "llama3.2"
	}
	if timeout <= 0 {
		timeout = OllamaDefaultTimeout
	}
	return &OllamaProvider{
		endpoint: strings.TrimRight(endpoint, "/"),
		model:    model,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *OllamaProvider) GetName() string { return "ollama" }

// GetModels 从 Ollama /api/tags 获取已安装模型列表。
// 若请求失败则返回当前配置的默认模型。
func (p *OllamaProvider) GetModels() []string {
	baseURL := strings.TrimSuffix(p.endpoint, "/v1")
	req, err := http.NewRequest("GET", baseURL+"/api/tags", nil)
	if err != nil {
		return []string{p.model}
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return []string{p.model}
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Models) == 0 {
		return []string{p.model}
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names
}

// HealthCheck 通过 /api/tags 检查 Ollama 服务是否运行
func (p *OllamaProvider) HealthCheck(ctx context.Context) error {
	baseURL := strings.TrimSuffix(p.endpoint, "/v1")
	hc := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w", p.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *OllamaProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = p.model
	}

	ollamaReq := map[string]interface{}{
		"model":       model,
		"messages":    p.buildMessages(req),
		"temperature": req.Temperature,
		"stream":      false,
	}
	if req.MaxTokens > 0 {
		ollamaReq["max_tokens"] = req.MaxTokens
	}
	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

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
			Error:      fmt.Sprintf("ollama error %d: %s", resp.StatusCode, string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var chatResp OpenAIChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("ollama response parse error: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return &GenerateResponse{Error: "no choices returned", FinishTime: time.Since(start).Milliseconds()}, nil
	}

	return &GenerateResponse{
		Content:     chatResp.Choices[0].Message.Content,
		Model:       chatResp.Model,
		InputTokens: chatResp.Usage.PromptTokens,
		StopReason:  chatResp.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *OllamaProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)

	go func() {
		defer close(ch)

		model := req.Model
		if model == "" {
			model = p.model
		}

		ollamaStreamReq := map[string]interface{}{
			"model":       model,
			"messages":    p.buildMessages(req),
			"temperature": req.Temperature,
			"stream":      true,
		}
		if req.MaxTokens > 0 {
			ollamaStreamReq["max_tokens"] = req.MaxTokens
		}
		body, err := json.Marshal(ollamaStreamReq)
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := p.client.Do(httpReq)
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		defer resp.Body.Close()

		reader := NewSSEReader(resp.Body)
		for {
			event, readErr := reader.Read()
			if readErr != nil {
				if readErr != io.EOF {
					ch <- &GenerateResponse{Error: readErr.Error()}
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

// Embed 使用本地 embedding 模型生成向量。
// 若当前默认模型非 embedding 模型，自动尝试 nomic-embed-text。
func (p *OllamaProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	model := p.model
	if !strings.Contains(strings.ToLower(model), "embed") {
		model = "nomic-embed-text"
	}

	body, err := json.Marshal(map[string]interface{}{
		"model": model,
		"input": text,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

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
		return nil, fmt.Errorf("ollama embed error %d: %s", resp.StatusCode, string(respBody))
	}

	var embedResp OpenAIEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, fmt.Errorf("ollama embed parse error: %w", err)
	}
	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return embedResp.Data[0].Embedding, nil
}

// ImageGenerate Ollama 不支持图像生成
func (p *OllamaProvider) ImageGenerate(_ context.Context, _ *ImageGenerateRequest) (*ImageResponse, error) {
	return &ImageResponse{Error: "ollama does not support image generation"}, nil
}

// AudioGenerate Ollama 不支持音频生成
func (p *OllamaProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("ollama does not support audio generation")
}

// buildMessages 将 GenerateRequest 转换为 OpenAI 格式 messages 列表
func (p *OllamaProvider) buildMessages(req *GenerateRequest) []map[string]interface{} {
	msgs := make([]map[string]interface{}, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, map[string]interface{}{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}
	for _, msg := range req.Messages {
		msgs = append(msgs, map[string]interface{}{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}
	return msgs
}
