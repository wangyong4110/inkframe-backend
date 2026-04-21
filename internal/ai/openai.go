package ai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

func NewOpenAIProvider(apiKey, endpoint, model string) *OpenAIProvider {
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4"
	}

	return &OpenAIProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
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

func (p *OpenAIProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
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
		return &GenerateResponse{
			Error:     fmt.Sprintf("API error: %s", string(respBody)),
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	// 解析响应
	var openaiResp OpenAIChatResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, err
	}

	if len(openaiResp.Choices) == 0 {
		return &GenerateResponse{
			Error:     "no choices returned",
			FinishTime: time.Since(start).Milliseconds(),
		}, nil
	}

	return &GenerateResponse{
		Content:    openaiResp.Choices[0].Message.Content,
		Model:      openaiResp.Model,
		Tokens:       0,
		InputTokens: openaiResp.Usage.PromptTokens,
		StopReason: openaiResp.Choices[0].FinishReason,
		FinishTime: time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	ch := make(chan *GenerateResponse, 100)

	go func() {
		defer close(ch)

		openaiReq := p.buildRequest(req)
		openaiReq["stream"] = true

		body, marshalErr := json.Marshal(openaiReq)
		if marshalErr != nil {
			ch <- &GenerateResponse{Error: marshalErr.Error()}
			return
		}

		url := p.endpoint + "/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
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
				ch <- &GenerateResponse{
					Content: chunk.Choices[0].Delta.Content,
				}
			}
		}
	}()

	return ch, nil
}

func (p *OpenAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	req := map[string]interface{}{
		"model": "text-embedding-ada-002",
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

	var embedResp OpenAIEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, err
	}

	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return embedResp.Data[0].Embedding, nil
}

func (p *OpenAIProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
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
			return &ImageResponse{
				Error:     fmt.Sprintf("DALL-E error: %s", string(respBody)),
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}

		var dalleResp DALLEResponse
		if err := json.Unmarshal(respBody, &dalleResp); err != nil {
			return nil, err
		}

		if len(dalleResp.Data) == 0 {
			return &ImageResponse{
				Error:     "no image returned",
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}

		return &ImageResponse{
			URL:       dalleResp.Data[0].URL,
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// Stable Diffusion (需要第三方服务)
	return &ImageResponse{
		Error:     "SD integration requires external service",
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	// OpenAI TTS API
	start := time.Now()

	ttsReq := map[string]interface{}{
		"model": "tts-1",
		"input": req.Text,
		"voice": req.Voice,
		"speed": req.Speed,
	}

	body, _ := json.Marshal(ttsReq)

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
		return &AudioResponse{
			Error:     fmt.Sprintf("TTS error: %s", string(respBody)),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// Read audio bytes and save to temp file
	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return &AudioResponse{
			Error:     fmt.Sprintf("TTS read body error: %s", err.Error()),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return &AudioResponse{
			Error:     fmt.Sprintf("TTS write file error: %s", err.Error()),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 10.0, // estimate
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *OpenAIProvider) buildRequest(req *GenerateRequest) map[string]interface{} {
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
		"max_tokens":  req.MaxTokens,
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

// OpenAI API 响应结构
type OpenAIChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		Message      struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		Delta        struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type OpenAIEmbedResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type DALLEResponse struct {
	Created int `json:"created"`
	Data    []struct {
		URL        string `json:"url"`
		RevvedURL string `json:"revised_prompt"`
	} `json:"data"`
}

// SSEReader SSE流式读取器
type SSEReader struct {
	reader *io.Reader
}

type SSEEvent struct {
	Event string
	Data  string
}

func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{reader: &r}
}

func (r *SSEReader) Read() (*SSEEvent, error) {
	// 简化实现
	buf := make([]byte, 1024)
	n, err := (*r.reader).Read(buf)
	if err != nil {
		return nil, err
	}

	content := string(buf[:n])
	if strings.Contains(content, "data: ") {
		start := strings.Index(content, "data: ") + 6
		end := strings.Index(content, "\n")
		if end == -1 {
			end = len(content)
		}
		return &SSEEvent{
			Data: strings.TrimSpace(content[start:end]),
		}, nil
	}

	return nil, io.EOF
}
