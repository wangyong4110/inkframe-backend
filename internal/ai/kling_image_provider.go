package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// KlingImageProvider 可灵图像生成提供者
// 官方文档：https://klingai.com/document-api/apiReference/model/imageGeneration
//
// 异步任务：
//  1. POST /v1/images/generations        — 提交任务，返回 task_id
//  2. GET  /v1/images/generations/{id}   — 轮询，直到 task_status=succeed
//
// 支持模型：kling-v1 / kling-v1-5 / kling-v2 / kling-v2-new / kling-v2-1 / kling-v3
// 返回 task_result.images[0].url（第一张生成图）
type KlingImageProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

const (
	klingImageDefaultEndpoint = "https://api-beijing.klingai.com"
	klingImagePollInterval    = 2 * time.Second
	klingImageMaxWait         = 3 * time.Minute
	klingImageDefaultModel    = "kling-v1"
	klingImageDefaultAspect   = "1:1"
)

// klingImageQueryResponse 图像生成任务查询响应
type klingImageQueryResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		TaskID        string `json:"task_id"`
		TaskStatus    string `json:"task_status"`
		TaskStatusMsg string `json:"task_status_msg"`
		TaskResult    struct {
			Images []struct {
				Index int    `json:"index"`
				URL   string `json:"url"`
			} `json:"images"`
		} `json:"task_result"`
	} `json:"data"`
}

// NewKlingImageProvider 创建可灵图像生成提供者
func NewKlingImageProvider(accessKey, secretKey, endpoint string) *KlingImageProvider {
	return &KlingImageProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, klingImageDefaultEndpoint),
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *KlingImageProvider) GetName() string { return "kling-image" }

func (p *KlingImageProvider) GetModels() []string {
	return []string{"kling-v1", "kling-v1-5", "kling-v2", "kling-v2-new", "kling-v2-1", "kling-v3"}
}

func (p *KlingImageProvider) HealthCheck(ctx context.Context) error {
	if p.accessKey == "" || p.secretKey == "" {
		return fmt.Errorf("kling-image: Access Key and Secret Key not configured")
	}
	return nil
}

func (p *KlingImageProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("kling-image: text generation not supported")
}

func (p *KlingImageProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("kling-image: streaming not supported")
}

func (p *KlingImageProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("kling-image: embeddings not supported")
}

func (p *KlingImageProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, fmt.Errorf("kling-image: audio generation not supported")
}

// ImageGenerate 提交图像生成任务并同步等待完成，返回第一张图片的 URL。
//
// req.Model:          模型名称（kling-v1/kling-v1-5/kling-v2/kling-v2-1/kling-v3），默认 kling-v1
// req.Prompt:         正向文本提示词（必填，最多 2500 字符）
// req.NegativePrompt: 负向文本提示词（可选，纯文生图有效）
// req.Size:           宽高比字符串，如 "16:9" "1:1" "1024x1024"；也可通过 Extra["aspect_ratio"] 覆盖
// req.ReferenceImage: 参考图 URL 或 Base64（图生图）
// req.Extra 可选键:
//
//	"aspect_ratio"   — 覆盖画面比例，枚举：16:9 9:16 1:1 4:3 3:4 3:2 2:3 21:9
//	"resolution"     — 生成清晰度：1k（默认）或 2k
//	"n"              — 生成数量（1-9，默认 1），返回第一张图
//	"image_reference"— 图片参考类型：subject 或 face（kling-v1-5 且有参考图时必填）
//	"image_fidelity" — 参考图强度（0-1，默认 0.5）
func (p *KlingImageProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	start := time.Now()

	if req.Prompt == "" {
		return nil, fmt.Errorf("kling-image: prompt is required")
	}

	model := req.Model
	if model == "" {
		model = klingImageDefaultModel
	}

	// 解析宽高比
	aspectRatio := klingImageDefaultAspect
	if extra := req.Extra; extra != nil {
		if v, ok := extra["aspect_ratio"].(string); ok && v != "" {
			aspectRatio = v
		}
	}
	if aspectRatio == klingImageDefaultAspect && req.Size != "" {
		aspectRatio = parseKlingAspectRatio(req.Size)
	}

	body := map[string]interface{}{
		"model_name":      model,
		"prompt":          req.Prompt,
		"aspect_ratio":    aspectRatio,
		"watermark_info":  map[string]bool{"enabled": false},
	}
	if req.NegativePrompt != "" {
		body["negative_prompt"] = req.NegativePrompt
	}
	if req.ReferenceImage != "" {
		body["image"] = req.ReferenceImage
	} else if len(req.ReferenceImages) > 0 {
		body["image"] = req.ReferenceImages[0]
	}

	// Extra 可选参数
	if extra := req.Extra; extra != nil {
		if v, ok := extra["resolution"].(string); ok && v != "" {
			body["resolution"] = v
		}
		if v, ok := extra["n"].(int); ok && v > 0 {
			body["n"] = v
		}
		if v, ok := extra["image_reference"].(string); ok && v != "" {
			body["image_reference"] = v
		}
		if v, ok := extra["image_fidelity"].(float64); ok {
			body["image_fidelity"] = v
		}
	}

	// Step 1: 提交任务
	taskID, err := p.submitImageTask(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("kling-image: submit task: %w", err)
	}

	// Step 2: 轮询直到完成
	pollCtx, cancel := context.WithTimeout(ctx, klingImageMaxWait)
	defer cancel()

	imageURL, err := p.pollImageUntilDone(pollCtx, taskID)
	if err != nil {
		return nil, fmt.Errorf("kling-image: poll task %s: %w", taskID, err)
	}

	return &ImageResponse{
		URL:       imageURL,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// submitImageTask 提交图像生成任务，返回 task_id
func (p *KlingImageProvider) submitImageTask(ctx context.Context, body map[string]interface{}) (string, error) {
	respBody, status, err := p.doRequest(ctx, "POST", "/v1/images/generations", body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return "", fmt.Errorf("HTTP %d: %s", status, string(respBody[:min(200, len(respBody))]))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("API error code=%d: %s", result.Code, result.Message)
	}
	if result.Data.TaskID == "" {
		return "", fmt.Errorf("empty task_id in response")
	}
	return result.Data.TaskID, nil
}

// pollImageUntilDone 轮询图像任务直到 succeed，返回第一张图片 URL
func (p *KlingImageProvider) pollImageUntilDone(ctx context.Context, taskID string) (string, error) {
	ticker := time.NewTicker(klingImagePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			resp, err := p.queryImageTask(ctx, taskID)
			if err != nil {
				return "", err
			}
			switch resp.Data.TaskStatus {
			case "succeed", "success":
				images := resp.Data.TaskResult.Images
				if len(images) == 0 {
					return "", fmt.Errorf("task succeeded but no images in result")
				}
				if images[0].URL == "" {
					return "", fmt.Errorf("task succeeded but image url is empty")
				}
				return images[0].URL, nil
			case "failed":
				msg := resp.Data.TaskStatusMsg
				if msg == "" {
					msg = "unknown error"
				}
				return "", fmt.Errorf("task failed: %s", msg)
			case "submitted", "processing":
				// 继续轮询
			default:
				// 未知状态，继续等待
			}
		}
	}
}

// queryImageTask 查询图像任务状态
func (p *KlingImageProvider) queryImageTask(ctx context.Context, taskID string) (*klingImageQueryResponse, error) {
	path := fmt.Sprintf("/v1/images/generations/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, string(respBody[:min(200, len(respBody))]))
	}

	var result klingImageQueryResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse query response: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("API error code=%d: %s", result.Code, result.Message)
	}
	return &result, nil
}

// doRequest 发送 HTTP 请求（复用 KlingProvider 鉴权逻辑）
func (p *KlingImageProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	kp := &KlingProvider{
		accessKey: p.accessKey,
		secretKey: p.secretKey,
		endpoint:  p.endpoint,
		client:    p.client,
	}
	return kp.doRequest(ctx, method, path, body)
}

// parseKlingAspectRatio 将 Size 字符串解析为 Kling 支持的 aspect_ratio 枚举值。
// 输入示例："16:9", "1:1", "1024x1024", "1920x1080"
func parseKlingAspectRatio(size string) string {
	validRatios := map[string]struct{}{
		"16:9": {}, "9:16": {}, "1:1": {},
		"4:3": {}, "3:4": {}, "3:2": {}, "2:3": {}, "21:9": {},
	}

	// 直接是合法比例字符串
	if _, ok := validRatios[size]; ok {
		return size
	}

	// WxH 格式 → 计算比例
	var w, h int
	if n, _ := fmt.Sscanf(strings.ToLower(size), "%dx%d", &w, &h); n == 2 && w > 0 && h > 0 {
		// 比较已知比例的近似值
		ratio := float64(w) / float64(h)
		closest := "1:1"
		minDiff := 999.0
		known := map[string]float64{
			"16:9": 16.0 / 9, "9:16": 9.0 / 16, "1:1": 1.0,
			"4:3": 4.0 / 3, "3:4": 3.0 / 4, "3:2": 1.5, "2:3": 2.0 / 3, "21:9": 21.0 / 9,
		}
		for k, v := range known {
			diff := ratio - v
			if diff < 0 {
				diff = -diff
			}
			if diff < minDiff {
				minDiff = diff
				closest = k
			}
		}
		return closest
	}

	return klingImageDefaultAspect
}

// Ensure interface compliance.
var _ AIProvider = (*KlingImageProvider)(nil)
