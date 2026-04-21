package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// KlingProvider 快手可灵视频生成提供者
// API: https://api.klingai.com/v1/videos/image2video
type KlingProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

// NewKlingProvider 创建 Kling 视频生成提供者
func NewKlingProvider(apiKey, endpoint string) *KlingProvider {
	if endpoint == "" {
		endpoint = "https://api.klingai.com"
	}
	return &KlingProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
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
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

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
	model := req.Model
	if model == "" {
		model = "kling-v1"
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
		"cfg_scale":       cfgScale,
		"mode":            mode,
		"aspect_ratio":    req.AspectRatio,
		"duration":        fmt.Sprintf("%.0f", duration),
	}

	if req.ImageURL != "" {
		klingReq["image_url"] = req.ImageURL
	}

	if req.CameraMovement != "" {
		klingReq["camera_control"] = map[string]interface{}{
			"type": req.CameraMovement,
		}
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/v1/videos/image2video", klingReq)
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

	return &VideoTask{
		TaskID:   result.Data.TaskID,
		Status:   result.Data.Status,
		Provider: p.GetName(),
	}, nil
}

// GetVideoStatus 查询视频任务状态
func (p *KlingProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	path := fmt.Sprintf("/v1/videos/image2video/%s", taskID)
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
	path := fmt.Sprintf("/v1/videos/image2video/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("kling get video request failed: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("kling get video failed: status %d", status)
	}

	var result struct {
		Code int    `json:"code"`
		Data struct {
			TaskStatus string `json:"task_status"`
			Works      []struct {
				Video struct {
					URL string `json:"url"`
				} `json:"video"`
			} `json:"task_result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("kling parse video URL failed: %w", err)
	}

	if result.Data.TaskStatus != "succeed" {
		return "", fmt.Errorf("kling task not completed: status=%s", result.Data.TaskStatus)
	}

	if len(result.Data.Works) == 0 {
		return "", fmt.Errorf("kling: no video works returned")
	}

	return result.Data.Works[0].Video.URL, nil
}
