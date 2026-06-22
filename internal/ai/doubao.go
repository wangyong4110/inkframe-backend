package ai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrPrefixSensitiveContent 是图像生成内容审核拦截时错误消息的前缀。
// 上层调用（ai_service.go）检测到此前缀后会净化 prompt 并重试。
const ErrPrefixSensitiveContent = "[SENSITIVE_CONTENT] "

// DoubaoProvider 豆包 AI 提供者（字节跳动火山引擎 Ark 平台）
// 文本生成使用豆包大模型，图像生成使用 Seedream 模型
// API 兼容 OpenAI 格式：https://ark.volces.com/api/v3
type DoubaoProvider struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

// NewDoubaoProvider 创建豆包提供者。timeout<=0 时使用默认值 DefaultProviderTimeout。
func NewDoubaoProvider(apiKey, endpoint, model string, timeout time.Duration) *DoubaoProvider {
	if endpoint == "" {
		endpoint = "https://ark.volces.com/api/v3"
	}
	if model == "" {
		model = "doubao-pro-32k"
	}
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	return &DoubaoProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *DoubaoProvider) GetName() string { return "doubao" }

func (p *DoubaoProvider) GetModels() []string {
	return []string{
		// 豆包文本模型
		"doubao-pro-4k",
		"doubao-pro-32k",
		"doubao-pro-128k",
		"doubao-lite-4k",
		"doubao-lite-32k",
		"doubao-lite-128k",
		// Seedream 图像模型（火山方舟 Ark 平台，支持主体一致性多图参考）
		"doubao-seedream-5-0-260128",   // Seedream 5.0 lite，2K/3K/4K，支持流式/组图/联网搜索
		"doubao-seedream-4-5-251128",   // Seedream 4.5，2K/4K
		"doubao-seedream-4-0-250828",   // Seedream 4.0，1K/2K/4K（默认）
		"seededit-3-0-t2i-250428",      // SeedEdit 3.0，指令式图像编辑
	}
}

func (p *DoubaoProvider) HealthCheck(ctx context.Context) error {
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

func (p *DoubaoProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
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
			Error:      fmt.Sprintf("豆包 API 错误: %s", string(respBody)),
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

func (p *DoubaoProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
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

func (p *DoubaoProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	apiReq := map[string]interface{}{
		"model": "doubao-embedding",
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
		return nil, fmt.Errorf("豆包 embedding 错误: %s", string(respBody))
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

// ImageGenerate 使用 Seedream 模型生成图像
// Seedream API 兼容 OpenAI images/generations 格式
// 支持参考图：当 req.ReferenceImage 非空时，传入 image_url 字段（Seedream 3.5+/4.0 参考图生成）
func (p *DoubaoProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = "doubao-seedream-4-0-250828"
	}

	size := seedreamSize(req.Size)

	apiReq := map[string]interface{}{
		"model":                       model,
		"prompt":                      req.Prompt,
		"size":                        size,
		"watermark":                   false,
		"sequential_image_generation": "disabled", // 单张输出，禁止自动组图
	}
	if req.NegativePrompt != "" {
		apiReq["negative_prompt"] = req.NegativePrompt
	}
	// 参考图：Seedream 4.0+ 官方 API 使用 "image" 字段，支持 URL 或 Base64 data URI。
	// - URL：直接传入 https:// 地址
	// - Base64：必须带 data URI 前缀，格式 "data:image/<格式>;base64,<数据>"
	// - 多图（Seedream 4.0/4.5/5.0 最多支持 14 张）：传入 []string
	// 相对路径（/api/media/...）由上层 ai_service 预先 fetchImageAsBase64 转换为裸 base64 字符串。
	var allRefImages []string
	// 优先使用 ReferenceImages（多图），其次 ReferenceImage（单图）
	if len(req.ReferenceImages) > 0 {
		for _, r := range req.ReferenceImages {
			if formatted := seedreamFormatImage(r); formatted != "" {
				allRefImages = append(allRefImages, formatted)
			}
		}
	} else if req.ReferenceImage != "" {
		if formatted := seedreamFormatImage(req.ReferenceImage); formatted != "" {
			allRefImages = append(allRefImages, formatted)
		}
	}
	if len(allRefImages) == 1 {
		apiReq["image"] = allRefImages[0]
	} else if len(allRefImages) > 1 {
		apiReq["image"] = allRefImages
	}

	// 调试日志：确认参考图是否正确注入
	{
		imgTypes := make([]string, len(allRefImages))
		for i, img := range allRefImages {
			if strings.HasPrefix(img, "data:") {
				imgTypes[i] = "base64"
			} else if strings.HasPrefix(img, "http") {
				imgTypes[i] = "url"
			} else {
				imgTypes[i] = "unknown"
			}
		}
		log.Printf("[doubao] ImageGenerate model=%s refImages=%d types=%v prompt=%.80s", model, len(allRefImages), imgTypes, req.Prompt)
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
		errMsg := string(respBody)
		// 标记内容审核拦截，方便上层检测并做提示词净化重试
		if strings.Contains(errMsg, "InputTextSensitiveContentDetected") {
			errMsg = ErrPrefixSensitiveContent + errMsg
		}
		return &ImageResponse{
			Error:     fmt.Sprintf("Seedream 错误: %s", errMsg),
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

// AudioGenerate 使用火山方舟 TTS（音频合成）API，兼容 OpenAI /audio/speech 格式。
// model 字段使用配置的推理接入点 ID（如 2eTp7Le-...），endpoint 需指向 Ark API 地址。
func (p *DoubaoProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = p.model
	}

	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}

	ttsReq := map[string]interface{}{
		"model":           model,
		"input":           req.Text,
		"response_format": "mp3",
		"speed":           speed,
	}
	if req.Voice != "" {
		ttsReq["voice"] = req.Voice
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
		return nil, fmt.Errorf("豆包 TTS 错误 %d: %s", resp.StatusCode, string(respBody))
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("豆包 TTS 读取响应失败: %w", err)
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("豆包 TTS 写入临时文件失败: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 10.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// seedreamSize 将尺寸字符串规范化为 Seedream 接受的 "WIDTHxHEIGHT" 格式。
// Seedream 不接受 "W:H" 比例字符串，需转换为实际像素。
func seedreamSize(size string) string {
	if size == "" {
		return "1024x1024"
	}
	// 已经是 WxH 格式，直接返回
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err == nil && w > 0 && h > 0 {
		return size
	}
	// "1k"/"2k"/"4k" 快捷方式，直接透传
	switch size {
	case "1k", "2k", "4k":
		return size
	}
	// W:H 比例字符串 → 以 1024 为短边换算
	var rw, rh int
	if _, err := fmt.Sscanf(size, "%d:%d", &rw, &rh); err == nil && rw > 0 && rh > 0 {
		const base = 1024
		if rw >= rh {
			return fmt.Sprintf("%dx%d", base*rw/rh, base)
		}
		return fmt.Sprintf("%dx%d", base, base*rh/rw)
	}
	return "1024x1024"
}

// seedreamFormatImage 将图片输入格式化为 Seedream API 接受的 "image" 字段值。
// - https:// URL：直接返回
// - 裸 base64 数据（由 fetchImageAsBase64 返回）：自动检测格式并添加 data URI 前缀
// - 相对路径或过短字符串（未被上层解析）：返回空字符串，跳过
func seedreamFormatImage(img string) string {
	if img == "" {
		return ""
	}
	if strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://") {
		return img
	}
	if strings.HasPrefix(img, "data:") {
		return img // 已有 data URI 前缀
	}
	if len(img) < 64 {
		return "" // 太短，可能是残留相对路径，跳过
	}
	// 裸 base64：检测图片格式并添加 data URI 前缀
	return "data:" + seedreamDetectMime(img) + ";base64," + img
}

// seedreamDetectMime 根据 base64 编码数据的前几个字符推断图片 MIME 类型。
// base64 编码的前缀与原始字节魔数对应：
//   - JPEG  FF D8 FF → /9j/
//   - PNG   89 50 4E 47 → iVBOR
//   - GIF   47 49 46 38 → R0lG
//   - WebP  52 49 46 46 → UklG
func seedreamDetectMime(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "iVBOR"):
		return "image/png"
	case strings.HasPrefix(b64, "R0lG"):
		return "image/gif"
	case strings.HasPrefix(b64, "UklG"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
