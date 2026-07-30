package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

// AzureProvider implements AIProvider for Azure OpenAI Service.
//
// URL:  {endpoint}/deployments/{deployment}/chat/completions?api-version={apiVersion}
// Auth: api-key header (not Bearer token)
//
// Deployment name is resolved dynamically from GenerateRequest.Model at call time,
// falling back to the defaultDeployment set at construction. This lets one provider
// record serve multiple deployments—each AIModel.Name maps to one deployment.
type AzureProvider struct {
	apiKey            string
	endpoint          string
	defaultDeployment string // optional; used when req.Model is empty
	apiVersion        string
	client            *http.Client
}

// NewAzureProvider creates an Azure OpenAI provider.
//   - endpoint:          cognitive-services base URL, e.g. https://…openai.azure.com/openai
//   - defaultDeployment: fallback deployment name when req.Model is empty; may be ""
//   - apiVersion:        Azure REST API version, e.g. 2025-01-01-preview
func NewAzureProvider(apiKey, endpoint, defaultDeployment, apiVersion string, timeout time.Duration) *AzureProvider {
	endpoint = strings.TrimSpace(endpoint)
	apiKey = strings.TrimSpace(apiKey)
	// Tolerate users pasting a full chat-completions URL instead of the base endpoint.
	// e.g. https://…azure.com/openai/deployments/gpt-4.1/chat/completions
	//   → endpoint = "https://…azure.com/openai", defaultDeployment = "gpt-4.1"
	if idx := strings.Index(endpoint, "/deployments/"); idx != -1 {
		rest := endpoint[idx+len("/deployments/"):]
		if dep := strings.SplitN(rest, "/", 2)[0]; dep != "" && defaultDeployment == "" {
			defaultDeployment = dep
		}
		endpoint = endpoint[:idx]
	}
	if endpoint == "" {
		endpoint = "https://YOUR-RESOURCE.openai.azure.com/openai"
	}
	if apiVersion == "" {
		apiVersion = "2025-01-01-preview"
	}
	if timeout <= 0 {
		timeout = ai.DefaultProviderTimeout
	}
	return &AzureProvider{
		apiKey:            apiKey,
		endpoint:          endpoint,
		defaultDeployment: defaultDeployment,
		apiVersion:        apiVersion,
		client:            &http.Client{Timeout: timeout},
	}
}

func (p *AzureProvider) GetName() string { return "azure" }

func (p *AzureProvider) GetModels() []string {
	if p.defaultDeployment != "" {
		return []string{p.defaultDeployment}
	}
	return nil
}

// deploymentOf returns the deployment name to use for a given request.
// req.Model takes priority over the provider-level defaultDeployment.
func (p *AzureProvider) deploymentOf(model string) (string, error) {
	dep := model
	if dep == "" {
		dep = p.defaultDeployment
	}
	if dep == "" {
		return "", fmt.Errorf("azure: deployment name required — set the model name in AIModel configuration to match your Azure deployment name")
	}
	return dep, nil
}

func (p *AzureProvider) chatURL(deployment, apiVersion string) string {
	if apiVersion == "" {
		apiVersion = p.apiVersion
	}
	return fmt.Sprintf("%s/deployments/%s/chat/completions?api-version=%s",
		p.endpoint, deployment, apiVersion)
}

func (p *AzureProvider) HealthCheck(ctx context.Context) error {
	dep := p.defaultDeployment
	if dep == "" {
		// No default deployment: just verify the endpoint is reachable and the key works
		// by hitting the list-deployments endpoint.
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
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("azure api-key invalid (401)")
		}
		// 404 is fine (endpoint may not support listing); anything else that's not 5xx is ok
		if resp.StatusCode >= 500 {
			return fmt.Errorf("azure endpoint error: status %d", resp.StatusCode)
		}
		return nil
	}

	// Verify the specific deployment exists.
	url := fmt.Sprintf("%s/deployments/%s?api-version=%s", p.endpoint, dep, p.apiVersion)
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
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("azure deployment %q not found (404) — check endpoint and deployment name", dep)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("azure api-key invalid (401)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("azure health check failed: status %d", resp.StatusCode)
	}
	return nil
}

func (p *AzureProvider) Generate(ctx context.Context, req *ai.GenerateRequest) (*ai.GenerateResponse, error) {
	dep, err := p.deploymentOf(req.Model)
	if err != nil {
		return nil, err
	}

	start := time.Now()

	body, err := json.Marshal(p.buildChatRequest(req))
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatURL(dep, req.APIVersion), bytes.NewReader(body))
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
		return &ai.GenerateResponse{
			Error:      fmt.Sprintf("Azure API error %d: %s", resp.StatusCode, string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	var parsed ai.OpenAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return &ai.GenerateResponse{Error: "no choices returned", FinishTime: time.Since(start).Milliseconds()}, nil
	}

	return &ai.GenerateResponse{
		Content:     parsed.Choices[0].Message.Content,
		Model:       dep,
		InputTokens: parsed.Usage.PromptTokens,
		Tokens:      parsed.Usage.CompletionTokens,
		StopReason:  parsed.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *AzureProvider) GenerateStream(ctx context.Context, req *ai.GenerateRequest) (<-chan *ai.GenerateResponse, error) {
	dep, err := p.deploymentOf(req.Model)
	if err != nil {
		return nil, err
	}

	ch := make(chan *ai.GenerateResponse, 100)
	go func() {
		defer close(ch)

		payload := p.buildChatRequest(req)
		payload["stream"] = true

		body, err := json.Marshal(payload)
		if err != nil {
			ch <- &ai.GenerateResponse{Error: err.Error()}
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(dep, req.APIVersion), bytes.NewReader(body))
		if err != nil {
			ch <- &ai.GenerateResponse{Error: err.Error()}
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("api-key", p.apiKey)

		resp, err := p.client.Do(httpReq)
		if err != nil {
			ch <- &ai.GenerateResponse{Error: err.Error()}
			return
		}
		defer resp.Body.Close()

		reader := ai.NewSSEReader(resp.Body)
		for {
			event, err := reader.Read()
			if err != nil {
				if err != io.EOF {
					ch <- &ai.GenerateResponse{Error: err.Error()}
				}
				break
			}
			if event.Data == "[DONE]" {
				break
			}
			var chunk ai.OpenAIStreamChunk
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) > 0 {
				ch <- &ai.GenerateResponse{Content: chunk.Choices[0].Delta.Content}
			}
		}
	}()
	return ch, nil
}

func (p *AzureProvider) buildChatRequest(req *ai.GenerateRequest) map[string]interface{} {
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
