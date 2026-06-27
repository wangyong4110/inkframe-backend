package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// KlingProvider 快手可灵视频生成提供者
// v1 API: https://api-beijing.klingai.com/v1/videos/image2video  (kling-v1/v1-5/v1-6)
// 3.x API: https://api-beijing.klingai.com/text-to-video/{model} (kling-3.0-turbo 等)
// 鉴权：HS256 JWT（accessKey + secretKey）
type KlingProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

// kling3Req 是 Kling 3.x 文生视频接口的请求体结构。
type kling3Req struct {
	Prompt   string      `json:"prompt"`
	Options  kling3Opts  `json:"options,omitempty"`
	Settings kling3Sets  `json:"settings"`
}

type kling3Opts struct {
	ExternalTaskID string           `json:"external_task_id,omitempty"`
	WatermarkInfo  *kling3Watermark `json:"watermark_info,omitempty"`
}

type kling3Watermark struct {
	Enabled bool `json:"enabled"`
}

type kling3Sets struct {
	Duration    int    `json:"duration"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// is3xModel 判断模型名是否属于 Kling 3.x 新 API（路径格式为 /text-to-video/{model}）。
func is3xModel(model string) bool {
	return strings.Contains(model, "3.0") || strings.Contains(model, "3.1") || strings.Contains(model, "3.5")
}

// kling3StatusToInternal 将 Kling 3.x 状态值映射为内部统一状态。
func kling3StatusToInternal(s string) string {
	switch s {
	case "submitted":
		return "pending"
	case "processing":
		return "processing"
	case "succeeded":
		return "completed"
	case "failed":
		return "failed"
	default:
		return s
	}
}

// NewKlingProvider 创建 Kling 视频生成提供者
func NewKlingProvider(accessKey, secretKey, endpoint string) *KlingProvider {
	return &KlingProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, "https://api-beijing.klingai.com"),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (p *KlingProvider) GetName() string {
	return "kling"
}

func (p *KlingProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	return klingDoRequest(ctx, p.accessKey, p.secretKey, p.endpoint, p.client, method, path, body)
}

// generate3xTurbo 使用 Kling 3.x 新 API 提交文生视频任务。
// 生成唯一 external_task_id，提交时携带并存储为任务标识，查询时通过 /tasks?external_task_ids= 轮询。
func (p *KlingProvider) generate3xTurbo(ctx context.Context, req *VideoGenerateRequest, model string) (*VideoTask, error) {
	dur := int(req.Duration)
	switch {
	case dur <= 0:
		dur = 5
	case dur <= 4:
		dur = 3
	case dur <= 7:
		dur = 5
	default:
		dur = 10
	}

	// 生成唯一外部任务 ID，用于后续查询（/tasks?external_task_ids=）
	extID := fmt.Sprintf("ik3-%d", time.Now().UnixNano())

	body := kling3Req{
		Prompt: req.Prompt,
		Options: kling3Opts{
			ExternalTaskID: extID,
			WatermarkInfo:  &kling3Watermark{Enabled: false},
		},
		Settings: kling3Sets{
			Duration:    dur,
			Resolution:  "720p",
			AspectRatio: req.AspectRatio,
		},
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/text-to-video/"+model, body)
	if err != nil {
		return nil, fmt.Errorf("kling 3.x generate request failed: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return nil, fmt.Errorf("kling 3.x generate failed: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("kling 3.x parse response failed: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("kling 3.x API error: code=%d, message=%s", result.Code, result.Message)
	}

	return &VideoTask{
		TaskID:   "t2v3:" + extID,
		Status:   kling3StatusToInternal(result.Data.Status),
		Provider: p.GetName(),
	}, nil
}

// GenerateVideo 提交视频生成任务（文生/图生视频）
func (p *KlingProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	// 3.x 模型（如 kling-3.0-turbo）仅支持文生视频（T2V），不接受参考图。
	// 有参考图时（I2V 时序链接或场景锚点）自动降级到 kling-v1-6 以保持镜头连贯性。
	if is3xModel(req.Model) {
		if req.ImageURL == "" && len(req.ImageURLs) == 0 {
			return p.generate3xTurbo(ctx, req, req.Model)
		}
		// 有参考图，退化到 v1.x I2V 端点
		req.Model = "kling-v1-6"
	}

	// 合并所有参考图：ImageURL（主参考图）置首位，ImageURLs 追加并去重
	allImages := make([]string, 0, 1+len(req.ImageURLs))
	if req.ImageURL != "" {
		allImages = append(allImages, req.ImageURL)
	}
	for _, u := range req.ImageURLs {
		if u != "" && u != req.ImageURL {
			allImages = append(allImages, u)
		}
	}
	// Kling 多图最多支持 4 张
	if len(allImages) > 4 {
		allImages = allImages[:4]
	}

	model := req.Model
	if model == "" {
		if len(allImages) > 1 {
			// 多图模式需要 kling-v1-6 及以上
			model = "kling-v1-6"
		} else {
			model = "kling-v1"
		}
	}

	cfgScale := req.CFGScale
	if cfgScale <= 0 {
		cfgScale = 0.5
	}
	mode := req.Mode
	if mode == "" {
		mode = "std"
	}

	// Kling only accepts 5 or 10 seconds
	duration := req.Duration
	if duration <= 7 {
		duration = 5
	} else {
		duration = 10
	}

	klingReq := map[string]interface{}{
		"model":           model,
		"prompt":          req.Prompt,
		"negative_prompt": req.NegativePrompt,
		"cfg_scale":    cfgScale,
		"mode":         mode,
		"aspect_ratio": req.AspectRatio,
		"duration":     int(duration), // Kling API 期望整数而非字符串
	}

	var submitPath string
	switch len(allImages) {
	case 0:
		// text-to-video，不设置 image 字段
		submitPath = "/v1/videos/image2video"
	case 1:
		// 单图：使用 image_url（兼容所有版本）
		klingReq["image_url"] = allImages[0]
		submitPath = "/v1/videos/image2video"
	default:
		// 多图：使用 multi-image2video 端点，image_list 格式为 [{image: url}]
		imageList := make([]map[string]string, len(allImages))
		for i, img := range allImages {
			imageList[i] = map[string]string{"image": img}
		}
		klingReq["image_list"] = imageList
		// multi-image2video 使用 model_name 字段
		klingReq["model_name"] = model
		delete(klingReq, "model")
		delete(klingReq, "cfg_scale") // multi-image2video 不支持 cfg_scale
		submitPath = "/v1/videos/multi-image2video"
	}

	if req.CameraMovement != "" {
		klingReq["camera_control"] = map[string]interface{}{
			"type": req.CameraMovement,
		}
	}

	respBody, status, err := p.doRequest(ctx, "POST", submitPath, klingReq)
	if err != nil {
		return nil, fmt.Errorf("kling generate request failed: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return nil, fmt.Errorf("kling generate failed: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
			Status string `json:"task_status"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("kling parse response failed: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("kling API error: code=%d, message=%s", result.Code, result.Message)
	}

	taskID := result.Data.TaskID
	if submitPath == "/v1/videos/multi-image2video" {
		// 用前缀标记，GetVideoStatus/GetVideoURL 据此选择正确的查询端点
		taskID = "multi:" + taskID
	}

	return &VideoTask{
		TaskID:   taskID,
		Status:   result.Data.Status,
		Provider: p.GetName(),
	}, nil
}

// klingVideoTaskPath 根据 taskID 前缀返回正确的查询路径。
// "t2v3:<id>"  → /text-to-video/query/<id>   (Kling 3.x 文生视频)
// "multi:<id>" → /v1/videos/multi-image2video/<id>
// "<id>"       → /v1/videos/image2video/<id>
func klingVideoTaskPath(taskID string) (path string, rawID string) {
	if strings.HasPrefix(taskID, "t2v3:") {
		rawID = taskID[5:]
		return "/tasks?external_task_ids=" + rawID, rawID
	}
	if strings.HasPrefix(taskID, "multi:") {
		rawID = taskID[6:]
		return "/v1/videos/multi-image2video/" + rawID, rawID
	}
	return "/v1/videos/image2video/" + taskID, taskID
}

// GetVideoStatus 查询视频任务状态
func (p *KlingProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	path, _ := klingVideoTaskPath(taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("kling status request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("kling status failed: status %d", status)
	}

	// 3.x API 与 v1 API 状态字段名不同，用 RawMessage 统一解析
	var raw struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("kling parse status failed: %w", err)
	}

	if strings.HasPrefix(taskID, "t2v3:") {
		// /tasks 响应：data 为数组，取第一个元素
		var items []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Message string `json:"message"` // 失败原因
		}
		if err := json.Unmarshal(raw.Data, &items); err != nil || len(items) == 0 {
			return nil, fmt.Errorf("kling 3.x parse status data failed (empty or invalid): %w", err)
		}
		d := items[0]
		ts := &VideoTaskStatus{
			TaskID: d.ID,
			Status: kling3StatusToInternal(d.Status),
		}
		if d.Message != "" && d.Status == "failed" {
			ts.Error = d.Message
		}
		return ts, nil
	}

	// v1 响应：{ task_id, task_status, progress, task_status_msg }
	var d struct {
		TaskID     string  `json:"task_id"`
		Status     string  `json:"task_status"`
		Progress   float64 `json:"progress"`
		FailReason string  `json:"task_status_msg"`
	}
	if err := json.Unmarshal(raw.Data, &d); err != nil {
		return nil, fmt.Errorf("kling v1 parse status data failed: %w", err)
	}
	ts := &VideoTaskStatus{
		TaskID:   d.TaskID,
		Status:   d.Status,
		Progress: d.Progress,
	}
	if d.FailReason != "" {
		ts.Error = d.FailReason
	}
	return ts, nil
}

// GetVideoURL 获取已完成任务的视频 URL
func (p *KlingProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	path, _ := klingVideoTaskPath(taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("kling get video request failed: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("kling get video failed: status %d", status)
	}

	// 3.x API 与 v1 结果结构不同，按前缀分支解析
	var raw struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return "", fmt.Errorf("kling parse video URL failed: %w", err)
	}

	if strings.HasPrefix(taskID, "t2v3:") {
		// /tasks 响应：data 为数组，outputs[] 按 type 区分不同产物
		var items []struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Outputs []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"outputs"`
		}
		if err := json.Unmarshal(raw.Data, &items); err != nil || len(items) == 0 {
			return "", fmt.Errorf("kling 3.x parse video data failed: %w", err)
		}
		d := items[0]
		if d.Status != "succeeded" {
			return "", fmt.Errorf("kling 3.x task not completed: status=%s, message=%s", d.Status, d.Message)
		}
		for _, out := range d.Outputs {
			if out.Type == "video" && out.URL != "" {
				return out.URL, nil
			}
		}
		return "", fmt.Errorf("kling 3.x: no video output in result")
	}

	// v1 API — 兼容 image2video 和 multi-image2video 两种 task_result 结构
	var v1 struct {
		TaskStatus string          `json:"task_status"`
		TaskResult json.RawMessage `json:"task_result"`
	}
	if err := json.Unmarshal(raw.Data, &v1); err != nil {
		return "", fmt.Errorf("kling v1 parse video data failed: %w", err)
	}
	switch v1.TaskStatus {
	case "succeed", "success", "completed":
	default:
		return "", fmt.Errorf("kling task not completed: status=%s", v1.TaskStatus)
	}

	var multiResult struct {
		Videos []struct {
			URL string `json:"url"`
		} `json:"videos"`
	}
	if err := json.Unmarshal(v1.TaskResult, &multiResult); err == nil && len(multiResult.Videos) > 0 {
		if multiResult.Videos[0].URL != "" {
			return multiResult.Videos[0].URL, nil
		}
	}

	var works []struct {
		Video struct {
			URL string `json:"url"`
		} `json:"video"`
	}
	if err := json.Unmarshal(v1.TaskResult, &works); err == nil && len(works) > 0 {
		if works[0].Video.URL != "" {
			return works[0].Video.URL, nil
		}
	}

	return "", fmt.Errorf("kling: no video URL in result")
}
