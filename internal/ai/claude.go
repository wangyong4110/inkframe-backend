package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicProvider Anthropic AI提供者 (Claude)
type AnthropicProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

func NewAnthropicProvider(apiKey, endpoint, model string) *AnthropicProvider {
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1"
	}
	if model == "" {
		model = "claude-3-opus-20240229"
	}

	return &AnthropicProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (p *AnthropicProvider) GetName() string {
	return "anthropic"
}

func (p *AnthropicProvider) GetModels() []string {
	return []string{
		"claude-3-opus-20240229",
		"claude-3-sonnet-20240229",
		"claude-3-haiku-20240307",
	}
}

func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.endpoint+"/messages", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// Claude API需要发送消息才能测试，这里简化处理
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 200 或 400 都说明服务正常
	if resp.StatusCode >= 500 {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}
	return nil
}

func (p *AnthropicProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	start := time.Now()

	// 构建 Anthropic 请求（支持 Vision 多模态）
	messages := []map[string]interface{}{}
	for _, msg := range req.Messages {
		if len(msg.ImageURLs) > 0 {
			// 多模态消息：content 为 content blocks
			contentBlocks := []map[string]interface{}{}
			for _, imgURL := range msg.ImageURLs {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "image",
					"source": map[string]string{
						"type": "url",
						"url":  imgURL,
					},
				})
			}
			if msg.Content != "" {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": contentBlocks,
			})
		} else {
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	anthropicReq := map[string]interface{}{
		"model": func() string {
			if req.Model != "" {
				return req.Model
			}
			return p.model
		}(),
		"messages":      messages,
		"temperature":  req.Temperature,
		"max_tokens":    req.MaxTokens,
	}

	if req.SystemPrompt != "" {
		anthropicReq["system"] = req.SystemPrompt
	}

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &GenerateResponse{
			Error:      fmt.Sprintf("Claude API error: %s", string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var claudeResp ClaudeResponse
	if err := json.Unmarshal(respBody, &claudeResp); err != nil {
		return nil, err
	}

	if len(claudeResp.Content) == 0 {
		return &GenerateResponse{
			Error:      "no content in response",
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	return &GenerateResponse{
		Content:     claudeResp.Content[0].Text,
		Model:       claudeResp.Model,
		Tokens:      claudeResp.Usage.OutputTokens,
		InputTokens: claudeResp.Usage.InputTokens,
		StopReason:  claudeResp.StopReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *AnthropicProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)

	go func() {
		defer close(ch)

		messages := []map[string]interface{}{}
		for _, msg := range req.Messages {
			if len(msg.ImageURLs) > 0 {
				contentBlocks := []map[string]interface{}{}
				for _, imgURL := range msg.ImageURLs {
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type": "image",
						"source": map[string]string{
							"type": "url",
							"url":  imgURL,
						},
					})
				}
				if msg.Content != "" {
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type": "text",
						"text": msg.Content,
					})
				}
				messages = append(messages, map[string]interface{}{
					"role":    msg.Role,
					"content": contentBlocks,
				})
			} else {
				messages = append(messages, map[string]interface{}{
					"role":    msg.Role,
					"content": msg.Content,
				})
			}
		}

		anthropicReq := map[string]interface{}{
			"model":         p.model,
			"messages":      messages,
			"temperature":   req.Temperature,
			"max_tokens":    req.MaxTokens,
			"stream":        true,
		}

		if req.SystemPrompt != "" {
			anthropicReq["system"] = req.SystemPrompt
		}

		body, marshalErr := json.Marshal(anthropicReq)
		if marshalErr != nil {
			ch <- &GenerateResponse{Error: marshalErr.Error()}
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/messages", bytes.NewReader(body))
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", p.apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")

		resp, err := p.client.Do(httpReq)
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		defer resp.Body.Close()

		// 流式读取
		buf := make([]byte, 4096)
		reader := resp.Body
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				content := string(buf[:n])
				// 解析 SSE 格式的数据
				// Claude 返回的是 line-by-line JSON
				if len(content) > 10 {
					var chunk ClaudeStreamChunk
					if err := json.Unmarshal([]byte(content), &chunk); err == nil {
						if chunk.Type == "content_block_delta" {
							ch <- &GenerateResponse{
								Content: chunk.Delta.Text,
							}
						} else if chunk.Type == "message_stop" {
							break
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
	}()

	return ch, nil
}

func (p *AnthropicProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	// Anthropic 不提供 embedding API，需要使用其他服务
	return nil, fmt.Errorf("Anthropic does not provide embedding API")
}

func (p *AnthropicProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	// Anthropic 不提供图像生成 API
	return &ImageResponse{
		Error: "Anthropic does not provide image generation API",
	}, nil
}

func (p *AnthropicProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	// Anthropic 不提供语音生成 API
	return &AudioResponse{
		Error: "Anthropic does not provide audio generation API",
	}, nil
}

// Claude API 响应结构
type ClaudeResponse struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Role      string `json:"role"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	StopSeq    string `json:"stop_sequence"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type ClaudeStreamChunk struct {
	Type          string `json:"type"`
	Index         int    `json:"index"`
	ContentBlock  *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block,omitempty"`
	Delta         *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta,omitempty"`
	Usage         *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// GoogleProvider Google AI提供者 (Gemini)
type GoogleProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

func NewGoogleProvider(apiKey, endpoint, model string) *GoogleProvider {
	if endpoint == "" {
		endpoint = "https://generativelanguage.googleapis.com/v1beta"
	}
	if model == "" {
		model = "gemini-pro"
	}

	return &GoogleProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (p *GoogleProvider) GetName() string {
	return "google"
}

func (p *GoogleProvider) GetModels() []string {
	return []string{
		"gemini-pro",
		"gemini-pro-vision",
		"gemini-ultra",
	}
}

func (p *GoogleProvider) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("%s/models?key=%s", p.endpoint, p.apiKey)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

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

func (p *GoogleProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	start := time.Now()

	// 构建 Google 请求
	contents := []map[string]interface{}{}
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role": role,
			"parts": []map[string]string{
				{"text": msg.Content},
			},
		})
	}

	generationConfig := map[string]interface{}{
		"temperature":  req.Temperature,
		"maxOutputTokens": req.MaxTokens,
	}

	if req.TopP > 0 {
		generationConfig["topP"] = req.TopP
	}

	googleReq := map[string]interface{}{
		"contents":          contents,
		"generationConfig": generationConfig,
	}

	if req.SystemPrompt != "" {
		googleReq["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]string{
				{"text": req.SystemPrompt},
			},
		}
	}

	body, _ := json.Marshal(googleReq)

	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", p.endpoint, model, p.apiKey)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return &GenerateResponse{
			Error:     fmt.Sprintf("Gemini API error: %s", string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var geminiResp GeminiResponse
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return nil, err
	}

	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return &GenerateResponse{
			Error:     "no content returned",
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	return &GenerateResponse{
		Content:    geminiResp.Candidates[0].Content.Parts[0].Text,
		Model:      model,
		StopReason: geminiResp.Candidates[0].FinishReason,
		FinishTime: time.Since(start).Milliseconds(),
	}, nil
}

func (p *GoogleProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)

	go func() {
		defer close(ch)

		// 构建请求体（与 Generate 相同）
		contents := []map[string]interface{}{}
		for _, msg := range req.Messages {
			role := "user"
			if msg.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]interface{}{
				"role": role,
				"parts": []map[string]string{
					{"text": msg.Content},
				},
			})
		}

		generationConfig := map[string]interface{}{
			"temperature":     req.Temperature,
			"maxOutputTokens": req.MaxTokens,
		}
		if req.TopP > 0 {
			generationConfig["topP"] = req.TopP
		}

		googleReq := map[string]interface{}{
			"contents":         contents,
			"generationConfig": generationConfig,
		}
		if req.SystemPrompt != "" {
			googleReq["systemInstruction"] = map[string]interface{}{
				"parts": []map[string]string{{"text": req.SystemPrompt}},
			}
		}

		body, _ := json.Marshal(googleReq)

		model := p.model
		if req.Model != "" {
			model = req.Model
		}
		// 使用 SSE 流式端点（?alt=sse）
		url := fmt.Sprintf("%s/models/%s:streamGenerateContent?key=%s&alt=sse", p.endpoint, model, p.apiKey)

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
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

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			ch <- &GenerateResponse{Error: fmt.Sprintf("Gemini stream error %d: %s", resp.StatusCode, string(errBody))}
			return
		}

		// 解析 SSE 数据行（每行格式：data: {...json...}）
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var chunk GeminiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			for _, candidate := range chunk.Candidates {
				for _, part := range candidate.Content.Parts {
					if part.Text != "" {
						ch <- &GenerateResponse{Content: part.Text}
					}
				}
			}
		}
	}()

	return ch, nil
}

// GeminiStreamChunk Gemini 流式响应块
type GeminiStreamChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

func (p *GoogleProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	embedReq := map[string]interface{}{
		"model": "models/embedding-001",
		"content": map[string]interface{}{
			"parts": []map[string]string{
				{"text": text},
			},
		},
	}

	body, _ := json.Marshal(embedReq)

	url := fmt.Sprintf("%s/models/embedding-001:embedContent?key=%s", p.endpoint, p.apiKey)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding error: %s", string(respBody))
	}

	var embedResp struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}

	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, err
	}

	return embedResp.Embedding.Values, nil
}

func (p *GoogleProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	// Gemini Pro Vision 支持图像生成
	return &ImageResponse{
		Error: "Use Gemini Pro Vision for image generation",
	}, nil
}

func (p *GoogleProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	return &AudioResponse{
		Error: "Google does not provide standalone TTS API",
	}, nil
}

// Gemini API 响应结构
type GeminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason  string `json:"finishReason"`
		Index         int    `json:"index"`
		SafetyRatings []struct {
			Category    string `json:"category"`
			Probability string `json:"probability"`
		} `json:"safetyRatings"`
	} `json:"candidates"`
}
