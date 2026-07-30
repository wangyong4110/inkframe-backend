package openai

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAIProvider OpenAI AI提供者
type OpenAIProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

// NewOpenAIProvider 创建 OpenAI provider。timeout<=0 时使用默认值 DefaultProviderTimeout。
func NewOpenAIProvider(apiKey, endpoint, model string, timeout time.Duration) *OpenAIProvider {
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4"
	}
	if timeout <= 0 {
		timeout = ai.DefaultProviderTimeout
	}
	return &OpenAIProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *OpenAIProvider) GetName() string {
	return "openai"
}

func (p *OpenAIProvider) GetModels() []string {
	return []string{
		"gpt-4",
		"gpt-4-turbo",
		"gpt-4-32k",
		"gpt-3.5-turbo",
		"gpt-3.5-turbo-16k",
		"dall-e-3",
		"dall-e-2",
	}
}

func (p *OpenAIProvider) HealthCheck(ctx context.Context) error {
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

func (p *OpenAIProvider) Generate(ctx context.Context, req *ai.GenerateRequest) (*ai.GenerateResponse, error) {
	start := time.Now()

	// 构建请求
	openaiReq := p.buildRequest(req)

	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, err
	}

	url := p.endpoint + "/chat/completions"
	if strings.Contains(req.Model, "davinci") || strings.Contains(req.Model, "babbage") {
		url = p.endpoint + "/completions"
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
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
		return nil, fmt.Errorf("openai API error status=%d body=%s", resp.StatusCode, string(respBody))
	}

	// 解析响应
	var openaiResp ai.OpenAIChatResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, err
	}

	if len(openaiResp.Choices) == 0 {
		return &ai.GenerateResponse{
			Error:      "no choices returned",
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	content := openaiResp.Choices[0].Message.Content
	if content == "" {
		content = openaiResp.Choices[0].Message.ReasoningContent
	}
	return &ai.GenerateResponse{
		Content:     content,
		Model:       openaiResp.Model,
		Tokens:      openaiResp.Usage.CompletionTokens,
		InputTokens: openaiResp.Usage.PromptTokens,
		StopReason:  openaiResp.Choices[0].FinishReason,
		FinishTime:  time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) GenerateStream(ctx context.Context, req *ai.GenerateRequest) (<-chan *ai.GenerateResponse, error) {
	ch := make(chan *ai.GenerateResponse, 100)

	go func() {
		defer close(ch)

		openaiReq := p.buildRequest(req)
		openaiReq["stream"] = true

		body, marshalErr := json.Marshal(openaiReq)
		if marshalErr != nil {
			ch <- &ai.GenerateResponse{Error: marshalErr.Error()}
			return
		}

		url := p.endpoint + "/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			ch <- &ai.GenerateResponse{Error: err.Error()}
			return
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

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
				ch <- &ai.GenerateResponse{
					Content: chunk.Choices[0].Delta.Content,
				}
			}
		}
	}()

	return ch, nil
}

func (p *OpenAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	// p.model carries whatever the caller configured for this provider instance (e.g. the
	// embedding model chosen in Model Management, via effectiveModelName in getTenantProvider).
	// Previously this was hardcoded to text-embedding-ada-002, silently ignoring that config.
	model := p.model
	if model == "" {
		model = "text-embedding-ada-002"
	}
	req := map[string]interface{}{
		"model": model,
		"input": text,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/embeddings", bytes.NewReader(body))
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
		return nil, fmt.Errorf("read embed response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding error: %s", string(respBody))
	}

	var embedResp ai.OpenAIEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, err
	}

	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return embedResp.Data[0].Embedding, nil
}

func (p *OpenAIProvider) ImageGenerate(ctx context.Context, req *ai.ImageGenerateRequest) (*ai.ImageResponse, error) {
	start := time.Now()

	imageReq := map[string]interface{}{
		"model": req.Model,
	}

	if strings.Contains(req.Model, "dall-e") {
		// DALL-E API
		imageReq["prompt"] = req.Prompt
		imageReq["n"] = 1
		imageReq["size"] = req.Size
		imageReq["response_format"] = "url"

		body, _ := json.Marshal(imageReq)

		log.Printf("[openai] ImageGenerate model=%s size=%s promptLen=%d prompt=%.200q",
			req.Model, req.Size, len(req.Prompt), req.Prompt)

		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/images/generations", bytes.NewReader(body))
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

		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			return &ai.ImageResponse{
				Error:     fmt.Sprintf("DALL-E error: %s", string(respBody)),
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}

		var dalleResp ai.DALLEResponse
		if err := json.Unmarshal(respBody, &dalleResp); err != nil {
			return nil, err
		}

		if len(dalleResp.Data) == 0 {
			return &ai.ImageResponse{
				Error:     "no image returned",
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}

		return &ai.ImageResponse{
			URL:       dalleResp.Data[0].URL,
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// Stable Diffusion (需要第三方服务)
	return &ai.ImageResponse{
		Error:     "SD integration requires external service",
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) AudioGenerate(ctx context.Context, req *ai.AudioGenerateRequest) (*ai.AudioResponse, error) {
	// OpenAI TTS API
	start := time.Now()

	ttsReq := map[string]interface{}{
		"model": "tts-1",
		"input": req.Text,
		"voice": req.Voice,
		"speed": req.Speed,
	}

	body, _ := json.Marshal(ttsReq)

	log.Printf("[openai] AudioGenerate model=tts-1 voice=%s speed=%v textLen=%d text=%.200q",
		req.Voice, req.Speed, len(req.Text), req.Text)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.endpoint+"/audio/speech", bytes.NewReader(body))
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

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS API error %d: %s", resp.StatusCode, string(respBody))
	}

	// Read audio bytes and save to temp file
	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("TTS read body error: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "inkframe-tts-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("TTS write file error: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(audioData); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return nil, fmt.Errorf("TTS write file error: %w", err)
	}
	tmpFile.Close()

	return &ai.AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 10.0, // estimate
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) buildRequest(req *ai.GenerateRequest) map[string]interface{} {
	// 判断是否有 Vision 消息
	hasVision := false
	for _, msg := range req.Messages {
		if len(msg.ImageURLs) > 0 {
			hasVision = true
			break
		}
	}

	// 构建消息列表（支持 Vision 多模态）
	messages := []map[string]interface{}{}

	if req.SystemPrompt != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}

	for _, msg := range req.Messages {
		if len(msg.ImageURLs) > 0 {
			// 多模态消息：content 为 array
			contentParts := []map[string]interface{}{}
			for _, imgURL := range msg.ImageURLs {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": imgURL,
					},
				})
			}
			if msg.Content != "" {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": msg.Content,
				})
			}
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": contentParts,
			})
		} else {
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	// Vision 请求自动升级到支持视觉的模型
	model := req.Model
	if model == "" {
		model = p.model
	}
	if hasVision && model != "gpt-4o" && model != "gpt-4-vision-preview" && model != "gpt-4-turbo" {
		model = "gpt-4o"
	}

	openaiReq := map[string]interface{}{
		"model":       model,
		"messages":    messages,
		"temperature": req.Temperature,
	}
	if req.MaxTokens > 0 {
		openaiReq["max_tokens"] = req.MaxTokens
	}

	if req.TopP > 0 {
		openaiReq["top_p"] = req.TopP
	}

	if req.TopK > 0 {
		openaiReq["presence_penalty"] = float64(req.TopK) / 100
	}

	if len(req.Stop) > 0 {
		openaiReq["stop"] = req.Stop
	}

	return openaiReq
}


