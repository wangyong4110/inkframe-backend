package ai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

// NewQianwenProvider 创建通义千问提供者。timeout<=0 时使用默认值 DefaultProviderTimeout。
func NewQianwenProvider(apiKey, endpoint, model string, timeout time.Duration) *QianwenProvider {
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if model == "" {
		model = "qwen-plus"
	}
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	return &QianwenProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: timeout},
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
	// Qwen3 系列默认开启 thinking 模式（推理前会生成大量 reasoning token，严重拖慢速度）。
	// 对于小说/剧本等创作任务，thinking 模式无收益，统一关闭。
	if strings.HasPrefix(strings.ToLower(model), "qwen3") {
		apiReq["enable_thinking"] = false
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

	content := result.Choices[0].Message.Content
	// Qwen3 thinking 模式下 content 可能为空，回退到 reasoning_content
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
		// Qwen3 系列默认开启 thinking 模式，关闭以避免额外延迟
		if strings.HasPrefix(strings.ToLower(model), "qwen3") {
			apiReq["enable_thinking"] = false
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

// dashscopeBaseURL 从 compatible-mode 端点反推 DashScope 根地址。
// 例如 https://dashscope.aliyuncs.com/compatible-mode/v1 → https://dashscope.aliyuncs.com
func (p *QianwenProvider) dashscopeBaseURL() string {
	base := strings.TrimRight(p.endpoint, "/")
	for _, suffix := range []string{"/compatible-mode/v1", "/compatible-mode"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix)
		}
	}
	return base
}

// ImageGenerate 使用通义万象（Wan）模型生成图像。
//
// wanx2.1-* 及以上版本使用 DashScope 原生异步任务 API（提交 → 轮询），
// wanx-v1 等旧版模型回退到 OpenAI 兼容同步端点。
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
	// DashScope 原生 API 使用 "*" 分隔符，兼容 "x" 写法
	size = strings.ReplaceAll(size, "x", "*")

	// wanx2.1 及以上使用原生异步 API；wanx-v1 走旧兼容端点
	if strings.HasPrefix(model, "wanx2.") || strings.HasPrefix(model, "wanx3.") {
		return p.wanxImageGenerateAsync(ctx, start, model, size, req)
	}
	return p.wanxImageGenerateCompat(ctx, start, model, size, req)
}

// wanxImageGenerateAsync 调用 DashScope 原生异步图像生成 API（wanx2.1+）。
// 流程：POST 提交任务 → 轮询 GET 获取结果（最长等待 3 分钟）。
func (p *QianwenProvider) wanxImageGenerateAsync(ctx context.Context, start time.Time, model, size string, req *ImageGenerateRequest) (*ImageResponse, error) {
	baseURL := p.dashscopeBaseURL()

	input := map[string]interface{}{"prompt": req.Prompt}
	if req.NegativePrompt != "" {
		input["negative_prompt"] = req.NegativePrompt
	}
	if req.ReferenceImage != "" {
		input["ref_img"] = req.ReferenceImage
	}
	params := map[string]interface{}{"size": size, "n": 1}

	body, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"input":      input,
		"parameters": params,
	})

	submitReq, err := http.NewRequestWithContext(ctx, "POST",
		baseURL+"/api/v1/services/aigc/text2image/image-synthesis", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	submitReq.Header.Set("Content-Type", "application/json")
	submitReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	submitReq.Header.Set("X-DashScope-Async", "enable")

	resp, err := p.client.Do(submitReq)
	if err != nil {
		return nil, fmt.Errorf("Wan image submit: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &ImageResponse{
			Error:     fmt.Sprintf("Wan 图像提交失败 (HTTP %d): %s", resp.StatusCode, string(respBody)),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var submitOut struct {
		Output  struct {
			TaskID     string `json:"task_id"`
			TaskStatus string `json:"task_status"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &submitOut); err != nil {
		return nil, fmt.Errorf("Wan image: parse submit: %w", err)
	}
	if submitOut.Code != "" {
		return &ImageResponse{
			Error:     fmt.Sprintf("Wan 图像提交错误 %s: %s", submitOut.Code, submitOut.Message),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}

	taskID := submitOut.Output.TaskID
	// 轮询，最多 60 次 × 3s = 3 分钟
	for range 60 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}

		pollReq, _ := http.NewRequestWithContext(ctx, "GET",
			baseURL+"/api/v1/tasks/"+taskID, nil)
		pollReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		pollResp, err := p.client.Do(pollReq)
		if err != nil {
			continue
		}
		pollBody, _ := io.ReadAll(pollResp.Body)
		pollResp.Body.Close()

		var taskOut struct {
			Output struct {
				TaskStatus string `json:"task_status"`
				Results    []struct {
					URL  string `json:"url"`
					Code string `json:"code"`
				} `json:"results"`
			} `json:"output"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(pollBody, &taskOut); err != nil {
			continue
		}
		if taskOut.Code != "" {
			return &ImageResponse{
				Error:     fmt.Sprintf("Wan 图像任务错误 %s: %s", taskOut.Code, taskOut.Message),
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}
		switch taskOut.Output.TaskStatus {
		case "SUCCEEDED":
			if len(taskOut.Output.Results) == 0 {
				return &ImageResponse{Error: "Wan: 任务成功但无图像结果", LatencyMs: time.Since(start).Milliseconds()}, nil
			}
			return &ImageResponse{URL: taskOut.Output.Results[0].URL, LatencyMs: time.Since(start).Milliseconds()}, nil
		case "FAILED":
			return &ImageResponse{
				Error:     fmt.Sprintf("Wan 图像任务失败: task_id=%s", taskID),
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		}
		// PENDING / RUNNING：继续轮询
	}
	return &ImageResponse{
		Error:     fmt.Sprintf("Wan 图像生成超时（3min）: task_id=%s", taskID),
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// wanxImageGenerateCompat 旧版 wanx-v1 走 OpenAI 兼容同步端点。
func (p *QianwenProvider) wanxImageGenerateCompat(ctx context.Context, start time.Time, model, size string, req *ImageGenerateRequest) (*ImageResponse, error) {
	apiReq := map[string]interface{}{
		"model":  model,
		"prompt": req.Prompt,
		"n":      1,
		"size":   size,
	}
	if req.NegativePrompt != "" {
		apiReq["negative_prompt"] = req.NegativePrompt
	}
	if req.ReferenceImage != "" {
		if strings.HasPrefix(req.ReferenceImage, "http://") || strings.HasPrefix(req.ReferenceImage, "https://") {
			apiReq["ref_image_url"] = req.ReferenceImage
		} else {
			apiReq["ref_image_base64"] = req.ReferenceImage
		}
	}

	body, _ := json.Marshal(apiReq)
	endpoint := strings.TrimRight(p.endpoint, "/")
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/images/generations", bytes.NewReader(body))
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
			Error:     fmt.Sprintf("Wan 图像生成错误 (HTTP %d): %s", resp.StatusCode, string(respBody)),
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
	return &ImageResponse{URL: result.Data[0].URL, LatencyMs: time.Since(start).Milliseconds()}, nil
}

// AudioGenerate 合成语音。
//
// 模型选取优先级：req.Model > p.model > "cosyvoice-v1"
// 模型路由：
//   - qwen*tts* 系列 → 调用 DashScope 原生 TTS API（SSE 流式）
//   - cosyvoice* 及其他 → 调用 OpenAI 兼容 /audio/speech 接口
//
// 常用 qwen-tts 发音人：longxiaochun（女）、longhua（男）、longshu（男）、longxiaoxia（女）
func (p *QianwenProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	// 模型优先级：请求指定 > 提供商配置 > cosyvoice-v1 兜底
	model := req.Model
	if model == "" {
		model = p.model
	}
	// p.model 可能是文本生成模型（如 qwen-plus），此时回退到 TTS 默认
	if model == "" || (!strings.Contains(strings.ToLower(model), "tts") && !strings.HasPrefix(strings.ToLower(model), "cosyvoice")) {
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

	// Qwen TTS 系列使用原生 DashScope API
	if strings.Contains(strings.ToLower(model), "tts") && strings.HasPrefix(strings.ToLower(model), "qwen") {
		return p.generateQwenTTS(ctx, req.Text, model, voice, speed, start)
	}
	// CosyVoice 及其他使用 OpenAI 兼容接口
	return p.generateCosyVoice(ctx, req.Text, model, voice, speed, start)
}

// generateCosyVoice 调用 DashScope OpenAI 兼容 /audio/speech 接口（cosyvoice 系列）。
func (p *QianwenProvider) generateCosyVoice(ctx context.Context, text, model, voice string, speed float64, start time.Time) (*AudioResponse, error) {
	ttsReq := map[string]interface{}{
		"model":           model,
		"input":           text,
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
		return nil, fmt.Errorf("千问 CosyVoice TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("千问 CosyVoice TTS 错误 %d: %s", resp.StatusCode, string(respBody))
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("千问 CosyVoice TTS 读取响应失败: %w", err)
	}
	if len(audioData) == 0 {
		return nil, fmt.Errorf("千问 CosyVoice TTS 返回空音频数据")
	}

	return saveTTSToTemp(audioData, text, start)
}

// generateQwenTTS 调用 DashScope 原生 TTS API（qwen-tts/qwen3-tts 系列，SSE 流式）。
//
// 端点路由（两者响应格式相同，均为 SSE + base64 音频块）：
//   - qwen-tts*  → /api/v1/services/aigc/text2audiospeech/synthesis（voice 在 parameters）
//   - qwen3-tts* → /api/v1/services/aigc/multimodal-generation/generation（voice 在 input）
func (p *QianwenProvider) generateQwenTTS(ctx context.Context, text, model, voice string, speed float64, start time.Time) (*AudioResponse, error) {
	const (
		qwenTTSEndpoint  = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text2audiospeech/synthesis"
		qwen3TTSEndpoint = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"
	)

	var endpoint string
	var reqBody map[string]interface{}

	if strings.HasPrefix(strings.ToLower(model), "qwen3") {
		// qwen3-tts-flash：voice 在 input 中，endpoint 使用 multimodal-generation
		endpoint = qwen3TTSEndpoint
		inputFields := map[string]interface{}{
			"text":  text,
			"voice": voice,
		}
		reqBody = map[string]interface{}{
			"model":      model,
			"input":      inputFields,
			"parameters": map[string]interface{}{"format": "mp3", "rate": speed},
		}
	} else {
		// qwen-tts（老版本）：voice 在 parameters 中
		endpoint = qwenTTSEndpoint
		reqBody = map[string]interface{}{
			"model": model,
			"input": map[string]string{"text": text},
			"parameters": map[string]interface{}{
				"voice":  voice,
				"format": "mp3",
				"rate":   speed,
			},
		}
	}

	body, _ := json.Marshal(reqBody)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("X-DashScope-SSE", "enable")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("千问 TTS 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("千问 TTS 错误 %d: %s", resp.StatusCode, string(respBody))
	}

	// 如果响应是二进制音频（Content-Type: audio/*），直接读取
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "audio/") || strings.Contains(ct, "octet-stream") {
		audioData, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("千问 TTS 读取音频失败: %w", err)
		}
		if len(audioData) == 0 {
			return nil, fmt.Errorf("千问 TTS 返回空音频数据")
		}
		return saveTTSToTemp(audioData, text, start)
	}

	// 解析 SSE 流，拼接 base64 音频块。
	// DashScope output.audio 字段有两种格式：
	//   旧格式：{"output":{"audio":"base64...","finish_reason":"null"}}
	//   新格式：{"output":{"audio":{"data":"base64..."},"finish_reason":"null"}}
	// 使用 json.RawMessage 兼容两种格式。
	type sseOutput struct {
		FinishReason string          `json:"finish_reason"`
		Audio        json.RawMessage `json:"audio"`
	}
	type sseData struct {
		Output sseOutput `json:"output"`
	}

	// 解码 base64 音频块（兼容标准编码和 URL 安全编码）
	decodeAudio := func(raw string) ([]byte, error) {
		raw = strings.TrimSpace(raw)
		if chunk, err := base64.StdEncoding.DecodeString(raw); err == nil {
			return chunk, nil
		}
		return base64.URLEncoding.DecodeString(raw)
	}

	var audioData []byte
	var rawSnippet strings.Builder // 用于错误诊断
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB per line
	for scanner.Scan() {
		line := scanner.Text()
		if rawSnippet.Len() < 512 {
			rawSnippet.WriteString(line)
			rawSnippet.WriteByte('\n')
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev sseData
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if len(ev.Output.Audio) > 0 && string(ev.Output.Audio) != "null" {
			raw := string(ev.Output.Audio)
			var audioB64 string
			// 尝试解析为字符串（旧格式）
			if err := json.Unmarshal(ev.Output.Audio, &audioB64); err == nil && audioB64 != "" {
				chunk, err := decodeAudio(audioB64)
				if err != nil {
					return nil, fmt.Errorf("千问 TTS base64 解码失败: %w", err)
				}
				audioData = append(audioData, chunk...)
			} else {
				// 尝试解析为对象（新格式：{"data":"base64..."}）
				var audioObj struct {
					Data string `json:"data"`
				}
				if err := json.Unmarshal(ev.Output.Audio, &audioObj); err == nil && audioObj.Data != "" {
					chunk, err := decodeAudio(audioObj.Data)
					if err != nil {
						return nil, fmt.Errorf("千问 TTS base64 解码失败: %w", err)
					}
					audioData = append(audioData, chunk...)
				} else {
					_ = raw // 格式未知，跳过
				}
			}
		}
		if ev.Output.FinishReason == "stop" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("千问 TTS 读取 SSE 流失败: %w", err)
	}
	if len(audioData) == 0 {
		snippet := strings.TrimSpace(rawSnippet.String())
		if snippet == "" {
			snippet = "(空响应)"
		} else if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return nil, fmt.Errorf("千问 TTS 未收到音频数据（响应片段: %s）", snippet)
	}

	return saveTTSToTemp(audioData, text, start)
}

// saveTTSToTemp 将音频字节写入 /tmp 临时文件并返回 AudioResponse。
func saveTTSToTemp(audioData []byte, text string, start time.Time) (*AudioResponse, error) {
	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("TTS 写入临时文件失败: %w", err)
	}
	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len([]rune(text))) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}
