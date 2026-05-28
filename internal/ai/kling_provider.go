package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// KlingProvider 快手可灵视频生成提供者
// API: https://api.klingai.com/v1/videos/image2video
// 鉴权：HS256 JWT（accessKey + secretKey）
type KlingProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

// NewKlingProvider 创建 Kling 视频生成提供者
func NewKlingProvider(accessKey, secretKey, endpoint string) *KlingProvider {
	return &KlingProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, "https://api.klingai.com"),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (p *KlingProvider) GetName() string {
	return "kling"
}

func (p *KlingProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}

	url := p.endpoint + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}
	token, err := klingJWT(p.accessKey, p.secretKey)
	if err != nil {
		return nil, 0, fmt.Errorf("kling: JWT generation failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// GenerateVideo 提交图生视频任务
func (p *KlingProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
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
// "multi:<id>" → /v1/videos/multi-image2video/<id>
// "<id>"       → /v1/videos/image2video/<id>
func klingVideoTaskPath(taskID string) (path string, rawID string) {
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

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID     string  `json:"task_id"`
			Status     string  `json:"task_status"`
			Progress   float64 `json:"progress"`
			FailReason string  `json:"task_status_msg"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("kling parse status failed: %w", err)
	}

	taskStatus := &VideoTaskStatus{
		TaskID:   result.Data.TaskID,
		Status:   result.Data.Status,
		Progress: result.Data.Progress,
	}
	if result.Data.FailReason != "" {
		taskStatus.Error = result.Data.FailReason
	}

	return taskStatus, nil
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

	// 用 RawMessage 兼容两种 task_result 结构：
	//   image2video:       task_result 为数组 [{video:{url}}]
	//   multi-image2video: task_result 为对象 {videos:[{url}]}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			TaskStatus string          `json:"task_status"`
			TaskResult json.RawMessage `json:"task_result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return "", fmt.Errorf("kling parse video URL failed: %w", err)
	}

	switch envelope.Data.TaskStatus {
	case "succeed", "success", "completed":
	default:
		return "", fmt.Errorf("kling task not completed: status=%s", envelope.Data.TaskStatus)
	}

	var multiResult struct {
		Videos []struct {
			URL string `json:"url"`
		} `json:"videos"`
	}
	if err := json.Unmarshal(envelope.Data.TaskResult, &multiResult); err == nil && len(multiResult.Videos) > 0 {
		if multiResult.Videos[0].URL != "" {
			return multiResult.Videos[0].URL, nil
		}
	}

	var works []struct {
		Video struct {
			URL string `json:"url"`
		} `json:"video"`
	}
	if err := json.Unmarshal(envelope.Data.TaskResult, &works); err == nil && len(works) > 0 {
		if works[0].Video.URL != "" {
			return works[0].Video.URL, nil
		}
	}

	return "", fmt.Errorf("kling: no video URL in result")
}
