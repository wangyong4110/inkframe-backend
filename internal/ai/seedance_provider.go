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

// SeedanceProvider Seedance 视频生成提供者（字节跳动火山引擎 Ark 平台）
// 任务型异步接口：提交任务 → 轮询状态 → 获取视频 URL
// API: https://ark.volces.com/api/v3/contents/generations/tasks
type SeedanceProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

// NewSeedanceProvider 创建 Seedance 视频生成提供者
func NewSeedanceProvider(apiKey, endpoint string) *SeedanceProvider {
	if endpoint == "" {
		endpoint = "https://ark.volces.com/api/v3"
	}
	return &SeedanceProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *SeedanceProvider) GetName() string { return "seedance" }

func (p *SeedanceProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.endpoint+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// GenerateVideo 提交 Seedance 视频生成任务
// 支持文生视频（t2v）和图生视频（i2v）
func (p *SeedanceProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	model := req.Model
	if model == "" {
		if req.ImageURL != "" {
			model = "seedance-1-0-i2v-250428"
		} else {
			model = "seedance-1-0-t2v-250428"
		}
	}

	content := make([]map[string]interface{}, 0, 2)

	// 图生视频：先传图片
	if req.ImageURL != "" {
		content = append(content, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]string{"url": req.ImageURL},
		})
	}

	// 文本提示词
	prompt := req.Prompt
	if req.CameraMovement != "" {
		prompt = fmt.Sprintf("%s camera_movement: %s", prompt, req.CameraMovement)
	}
	content = append(content, map[string]interface{}{
		"type": "text",
		"text": prompt,
	})

	apiReq := map[string]interface{}{
		"model":   model,
		"content": content,
	}
	if req.AspectRatio != "" {
		apiReq["aspect_ratio"] = req.AspectRatio
	}
	if req.Duration > 0 {
		apiReq["duration"] = req.Duration
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/contents/generations/tasks", apiReq)
	if err != nil {
		return nil, fmt.Errorf("seedance create task failed: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return nil, fmt.Errorf("seedance create task failed: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("seedance parse create response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("seedance API error: code=%s, message=%s", result.Error.Code, result.Error.Message)
	}

	return &VideoTask{
		TaskID:   result.ID,
		Status:   result.Status,
		Provider: p.GetName(),
	}, nil
}

// GetVideoStatus 查询 Seedance 任务状态
func (p *SeedanceProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	path := "/contents/generations/tasks/" + taskID
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("seedance get status failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("seedance get status failed: status %d", status)
	}

	var result struct {
		ID      string `json:"id"`
		Status  string `json:"status"` // created, running, succeeded, failed, cancelled
		Error   *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("seedance parse status response: %w", err)
	}

	taskStatus := &VideoTaskStatus{
		TaskID: result.ID,
		Status: mapSeedanceStatus(result.Status),
	}
	switch result.Status {
	case "created":
		taskStatus.Progress = 5
	case "running":
		// Seedance API 不返回精确进度；使用 10-90% 范围表示进行中
		taskStatus.Progress = 50
	case "succeeded":
		taskStatus.Progress = 100
	case "failed", "cancelled":
		taskStatus.Progress = 0
	}
	if result.Error != nil {
		taskStatus.Error = result.Error.Message
	}
	return taskStatus, nil
}

// GetVideoURL 获取已完成 Seedance 任务的视频 URL
func (p *SeedanceProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	path := "/contents/generations/tasks/" + taskID
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("seedance get video failed: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("seedance get video failed: status %d", status)
	}

	var result struct {
		Status  string `json:"status"`
		Content []struct {
			Type     string `json:"type"`
			VideoURL *struct {
				URL string `json:"url"`
			} `json:"video_url,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("seedance parse video response: %w", err)
	}

	if result.Status != "succeeded" {
		return "", fmt.Errorf("seedance task not completed: status=%s", result.Status)
	}

	for _, item := range result.Content {
		if item.Type == "video_url" && item.VideoURL != nil && item.VideoURL.URL != "" {
			return item.VideoURL.URL, nil
		}
	}
	return "", fmt.Errorf("seedance: no video URL in response")
}

// mapSeedanceStatus 将 Seedance 状态映射为通用状态
func mapSeedanceStatus(s string) string {
	switch s {
	case "created":
		return "pending"
	case "running":
		return "processing"
	case "succeeded":
		return "completed"
	case "failed", "cancelled":
		return "failed"
	default:
		return s
	}
}
