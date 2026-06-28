package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// HunyuanImageProvider 腾讯混元图像生成提供者（TokenHub）
//
// 支持两种模式：
//
//	hy-image-lite  — 同步，极速版；POST /v1/api/image/lite
//	hy-image-v3.0  — 异步，高质量；POST /v1/api/image/submit → 轮询 /v1/api/image/query
//
// 文档：https://cloud.tencent.com/document/product/1668
// 鉴权：Authorization: Bearer YOUR_API_KEY（TokenHub 控制台创建）
type HunyuanImageProvider struct {
	apiKey  string
	baseURL string // 默认 https://tokenhub.tencentmaas.com/v1
	client  *http.Client
}

const (
	hunyuanImageBaseURL      = "https://tokenhub.tencentmaas.com/v1"
	hunyuanImageLitePath     = "/api/image/lite"
	hunyuanImageSubmitPath   = "/api/image/submit"
	hunyuanImageQueryPath    = "/api/image/query"
	hunyuanImageModelLite    = "hy-image-lite"
	hunyuanImageModelV3      = "hy-image-v3.0"
)

func NewHunyuanImageProvider(apiKey, baseURL string) *HunyuanImageProvider {
	if baseURL == "" {
		baseURL = hunyuanImageBaseURL
	}
	return &HunyuanImageProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *HunyuanImageProvider) GetName() string    { return "hunyuan-image" }
func (p *HunyuanImageProvider) GetModels() []string {
	return []string{hunyuanImageModelLite, hunyuanImageModelV3}
}

func (p *HunyuanImageProvider) HealthCheck(_ context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("hunyuan-image: API key not configured")
	}
	return nil
}

func (p *HunyuanImageProvider) Generate(_ context.Context, _ *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("hunyuan-image: text generation not supported")
}
func (p *HunyuanImageProvider) GenerateStream(_ context.Context, _ *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("hunyuan-image: streaming not supported")
}
func (p *HunyuanImageProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("hunyuan-image: embeddings not supported")
}
func (p *HunyuanImageProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("hunyuan-image: audio generation not supported")
}

// ImageGenerate 根据 req.Model 选择极速版（lite）或高质量版（v3.0）。
//
// req.Model:          "hy-image-lite"（默认）或 "hy-image-v3.0"
// req.Prompt:         文本描述
// req.ReferenceImages / req.ReferenceImage: 参考图 URL（v3.0 支持）
// req.Extra:          透传额外参数（如 logo_add、style 等）
func (p *HunyuanImageProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = hunyuanImageModelLite
	}

	switch model {
	case hunyuanImageModelV3:
		return p.generateV3(ctx, req, start)
	default:
		return p.generateLite(ctx, req, start)
	}
}

// ── Lite（同步）─────────────────────────────────────────────────────────────

func (p *HunyuanImageProvider) generateLite(ctx context.Context, req *ImageGenerateRequest, start time.Time) (*ImageResponse, error) {
	body := map[string]interface{}{
		"model":        hunyuanImageModelLite,
		"prompt":       req.Prompt,
		"rsp_img_type": "url",
	}
	if req.Size != "" {
		body["resolution"] = req.Size
	}
	for k, v := range req.Extra {
		body[k] = v
	}

	respBytes, err := p.post(ctx, p.baseURL+hunyuanImageLitePath, body)
	if err != nil {
		return nil, err
	}

	var result struct {
		RequestID string `json:"request_id"`
		Data      []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("hunyuan-image(lite): decode response: %w", err)
	}
	if len(result.Data) == 0 || result.Data[0].URL == "" {
		return nil, fmt.Errorf("hunyuan-image(lite): no image URL in response")
	}

	logger.Printf("hunyuan-image(lite): generated in %v, url=%s", time.Since(start).Round(time.Millisecond), result.Data[0].URL)
	return &ImageResponse{
		URL:       result.Data[0].URL,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// ── V3.0（异步：submit → 轮询 query）────────────────────────────────────────

func (p *HunyuanImageProvider) generateV3(ctx context.Context, req *ImageGenerateRequest, start time.Time) (*ImageResponse, error) {
	// 收集参考图
	var images []string
	if len(req.ReferenceImages) > 0 {
		images = req.ReferenceImages
	} else if req.ReferenceImage != "" {
		images = []string{req.ReferenceImage}
	}

	submitBody := map[string]interface{}{
		"model":  hunyuanImageModelV3,
		"prompt": req.Prompt,
	}
	if len(images) > 0 {
		submitBody["images"] = images
	}
	if req.Size != "" {
		submitBody["resolution"] = req.Size
	}
	for k, v := range req.Extra {
		submitBody[k] = v
	}

	respBytes, err := p.post(ctx, p.baseURL+hunyuanImageSubmitPath, submitBody)
	if err != nil {
		return nil, err
	}

	var submitResp struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(respBytes, &submitResp); err != nil {
		return nil, fmt.Errorf("hunyuan-image(v3.0): decode submit response: %w", err)
	}
	if submitResp.ID == "" {
		return nil, fmt.Errorf("hunyuan-image(v3.0): no job ID in submit response: %s", string(respBytes))
	}
	logger.Printf("hunyuan-image(v3.0): job submitted id=%s status=%s", submitResp.ID, submitResp.Status)

	// 轮询，最多等 3 分钟
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}

		queryBody := map[string]interface{}{
			"model": hunyuanImageModelV3,
			"id":    submitResp.ID,
		}
		qBytes, err := p.post(ctx, p.baseURL+hunyuanImageQueryPath, queryBody)
		if err != nil {
			logger.Printf("hunyuan-image(v3.0): query error (will retry): %v", err)
			continue
		}

		var queryResp struct {
			Status string `json:"status"`
			Data   []struct {
				URL           string `json:"url"`
				RevisedPrompt string `json:"revised_prompt"`
			} `json:"data"`
		}
		if err := json.Unmarshal(qBytes, &queryResp); err != nil {
			logger.Printf("hunyuan-image(v3.0): decode query response: %v", err)
			continue
		}

		logger.Printf("hunyuan-image(v3.0): job %s status=%s elapsed=%v", submitResp.ID, queryResp.Status, time.Since(start).Round(time.Millisecond))

		switch queryResp.Status {
		case "completed":
			if len(queryResp.Data) == 0 || queryResp.Data[0].URL == "" {
				return nil, fmt.Errorf("hunyuan-image(v3.0): completed but no image URL")
			}
			return &ImageResponse{
				URL:       queryResp.Data[0].URL,
				LatencyMs: time.Since(start).Milliseconds(),
			}, nil
		case "failed":
			return nil, fmt.Errorf("hunyuan-image(v3.0): job %s failed", submitResp.ID)
		// "queued" / "processing" — 继续等待
		}
	}
	return nil, fmt.Errorf("hunyuan-image(v3.0): job %s timed out after 3 minutes", submitResp.ID)
}

// ── 内部 HTTP 工具 ───────────────────────────────────────────────────────────

func (p *HunyuanImageProvider) post(ctx context.Context, url string, body map[string]interface{}) ([]byte, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("hunyuan-image: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hunyuan-image: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hunyuan-image: read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respBody)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("hunyuan-image: HTTP %d: %s", resp.StatusCode, msg)
	}
	return respBody, nil
}

var _ AIProvider = (*HunyuanImageProvider)(nil)
