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
	"time"
)

// QianwenProvider 通义千问 AI 提供者（阿里云百炼/DashScope）
// 文本生成兼容 OpenAI 格式：https://dashscope.aliyuncs.com/compatible-mode/v1
// 图像生成使用 Wan（万象）模型
type QianwenProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

// NewQianwenProvider 创建通义千问提供者
func NewQianwenProvider(apiKey, endpoint, model string) *QianwenProvider {
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if model == "" {
		model = "qwen-plus"
	}
	return &QianwenProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *QianwenProvider) GetName() string { return "qianwen" }

func (p *QianwenProvider) GetModels() []string {
	return []string{
		// 通义千问文本模型
		"qwen-turbo",
		"qwen-plus",
		"qwen-max",
		"qwen-long",
		"qwen-max-longcontext",
		// 千问视觉模型
		"qwen-vl-plus",
		"qwen-vl-max",
		// Wan（万象）图像模型
		"wanx-v1",
		"wanx2.1-t2i-turbo",
		"wanx2.1-t2i-plus",
		// CosyVoice 语音合成模型
		"cosyvoice-v1",
		"cosyvoice-v2",
	}
}

func (p *QianwenProvider) HealthCheck(ctx context.Context) error {
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

func (p *QianwenProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
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
			Error:      fmt.Sprintf("千问 API 错误: %s", string(respBody)),
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

func (p *QianwenProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
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

func (p *QianwenProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	apiReq := map[string]interface{}{
		"model": "text-embedding-v3",
		"input": text,
	}
	body, _ := json.Marshal(apiReq)

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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("千问 embedding 错误: %s", string(respBody))
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

// ImageGenerate 使用 Wan（万象）模型生成图像
// 通过 DashScope OpenAI 兼容端点调用
func (p *QianwenProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "wanx2.1-t2i-turbo"
	}

	size := req.Size
	if size == "" {
		size = "1024*1024"
	}

	apiReq := map[string]interface{}{
		"model":  model,
		"prompt": req.Prompt,
		"n":      1,
		"size":   size,
	}
	if req.NegativePrompt != "" {
		apiReq["negative_prompt"] = req.NegativePrompt
	}

	body, _ := json.Marshal(apiReq)
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
			Error:     fmt.Sprintf("Wan 图像生成错误: %s", string(respBody)),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var result DALLEResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	if len(result.Data) == 0 {
		return &ImageResponse{Error: "no image returned", LatencyMs: time.Since(start).Milliseconds()}, nil
	}

	return &ImageResponse{
		URL:       result.Data[0].URL,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// AudioGenerate 使用 CosyVoice 模型合成语音。
// DashScope 兼容 OpenAI /audio/speech 接口，model 默认 cosyvoice-v1，voice 默认 longxiaochun。
// 常用发音人：longxiaochun（女）、longhua（男）、longshu（男）、longxiaoxia（女）等。
func (p *QianwenProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "cosyvoice-v1"
	}
	voice := req.Voice
	if voice == "" {
		voice = "longxiaochun"
	}
	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}

	ttsReq := map[string]interface{}{
		"model":           model,
		"input":           req.Text,
		"voice":           voice,
		"speed":           speed,
		"response_format": "mp3",
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
		return nil, fmt.Errorf("千问 TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("千问 TTS 错误 %d: %s", resp.StatusCode, string(respBody))
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("千问 TTS 读取响应失败: %w", err)
	}
	if len(audioData) == 0 {
		return nil, fmt.Errorf("千问 TTS 返回空音频数据")
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("千问 TTS 写入临时文件失败: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}
