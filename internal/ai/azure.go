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

// AzureProvider implements AIProvider for Azure OpenAI Service.
// URL:  {endpoint}/deployments/{deployment}/chat/completions?api-version={apiVersion}
// Auth: api-key header (not Bearer token)
type AzureProvider struct {
	apiKey     string
	endpoint   string
	deployment string
	apiVersion string
	client     *http.Client
}

// NewAzureProvider creates an Azure OpenAI provider.
//   - endpoint:   base URL, e.g. https://…cognitiveservices.azure.com/openai
//   - deployment: deployment / model name, e.g. gpt-4.1
//   - apiVersion: Azure REST API version, e.g. 2025-01-01-preview
func NewAzureProvider(apiKey, endpoint, deployment, apiVersion string) *AzureProvider {
	if endpoint == "" {
		endpoint = "https://YOUR-RESOURCE.cognitiveservices.azure.com/openai"
	}
	if deployment == "" {
		deployment = "gpt-4"
	}
	if apiVersion == "" {
		apiVersion = "2025-01-01-preview"
	}
	return &AzureProvider{
		apiKey:     apiKey,
		endpoint:   endpoint,
		deployment: deployment,
		apiVersion: apiVersion,
		client:     &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *AzureProvider) GetName() string { return "azure" }

func (p *AzureProvider) GetModels() []string {
	return []string{p.deployment}
}

func (p *AzureProvider) chatURL() string {
	return fmt.Sprintf("%s/deployments/%s/chat/completions?api-version=%s",
		p.endpoint, p.deployment, p.apiVersion)
}

func (p *AzureProvider) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("%s/deployments?api-version=%s", p.endpoint, p.apiVersion)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("api-key", p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("azure health check failed: status %d", resp.StatusCode)
	}
	return nil
}

func (p *AzureProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	start := time.Now()

	body, err := json.Marshal(p.buildChatRequest(req))
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("api-key", p.apiKey)

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
			Error:      fmt.Sprintf("Azure API error %d: %s", resp.StatusCode, string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var parsed OpenAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return &GenerateResponse{Error: "no choices returned", FinishTime: time.Since(start).Milliseconds()}, nil
	}

	return &GenerateResponse{
		Content:     parsed.Choices[0].Message.Content,
		Model:       parsed.Model,
		InputTokens: parsed.Usage.PromptTokens,
		StopReason:  parsed.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *AzureProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)
	go func() {
		defer close(ch)

		payload := p.buildChatRequest(req)
		payload["stream"] = true

		body, err := json.Marshal(payload)
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(), bytes.NewReader(body))
		if err != nil {
			ch <- &GenerateResponse{Error: err.Error()}
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("api-key", p.apiKey)

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

func (p *AzureProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("azure: embedding not implemented")
}

func (p *AzureProvider) ImageGenerate(_ context.Context, _ *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("azure: image generation not implemented")
}

func (p *AzureProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("azure: audio generation not implemented")
}

func (p *AzureProvider) buildChatRequest(req *GenerateRequest) map[string]interface{} {
	messages := make([]map[string]interface{}, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		})
	}

	payload := map[string]interface{}{
		"messages": messages,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		payload["top_p"] = req.TopP
	}
	return payload
}
