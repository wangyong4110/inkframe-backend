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

// HappyHorseProvider 阿里云百炼 HappyHorse 视频生成提供者
// 支持三种模式：
//   - t2v（文生视频）：无参考图，使用 happyhorse-1.1-t2v
//   - i2v（图生视频，首帧）：1张图，使用 happyhorse-1.1-i2v，type=first_frame
//   - r2v（参考生视频）：多张参考图（1-9），使用 happyhorse-1.1-r2v，type=reference_image
//
// 异步接口：POST（含 X-DashScope-Async: enable）→ 轮询 GET /api/v1/tasks/{id}
// 文档：https://help.aliyun.com/zh/model-studio/happyhorse-text-to-video-api-reference
type HappyHorseProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

// NewHappyHorseProvider 创建 HappyHorse 提供者
// endpoint 为工作区专属域名（推荐），如 https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com
// 或通用域名 https://dashscope.aliyuncs.com（默认，性能低于专属域名）
func NewHappyHorseProvider(apiKey, endpoint string) *HappyHorseProvider {
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com"
	}
	return &HappyHorseProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *HappyHorseProvider) GetName() string { return "happyhorse" }

func (p *HappyHorseProvider) doRequest(ctx context.Context, method, path string, body interface{}, async bool) ([]byte, int, error) {
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
	if async {
		// 必须设置，否则报错 "does not support synchronous calls"
		req.Header.Set("X-DashScope-Async", "enable")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// GenerateVideo 提交 HappyHorse 视频生成任务
//
// 模式选择逻辑（基于传入图片数量）：
//   - 0 张图：文生视频（t2v），仅凭 prompt 生成
//   - 1 张图（ImageURL）：图生视频（i2v），以首帧为基础
//   - 多张图（ImageURLs 或 ImageURL + ImageURLs）：参考生视频（r2v），融合多角色/多道具
func (p *HappyHorseProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	// 收集所有参考图（去重，ImageURL 排首位）
	allImages := make([]string, 0, 1+len(req.ImageURLs))
	if req.ImageURL != "" {
		allImages = append(allImages, req.ImageURL)
	}
	for _, u := range req.ImageURLs {
		if u != "" && u != req.ImageURL {
			allImages = append(allImages, u)
		}
	}

	// 根据图片数量决定模式
	var mode string
	switch {
	case len(allImages) == 0:
		mode = "t2v"
	case len(allImages) == 1:
		mode = "i2v"
	default:
		mode = "r2v"
	}

	model := req.Model
	if model == "" {
		switch mode {
		case "t2v":
			model = "happyhorse-1.1-t2v"
		case "i2v":
			model = "happyhorse-1.1-i2v"
		case "r2v":
			model = "happyhorse-1.1-r2v"
		}
	}

	// 构建 input
	input := map[string]interface{}{
		"prompt": req.Prompt,
	}
	switch mode {
	case "i2v":
		// 首帧图生视频：media 数组，type=first_frame；不支持 ratio（比例自动跟随输入图）
		input["media"] = []map[string]interface{}{
			{"type": "first_frame", "url": allImages[0]},
		}
	case "r2v":
		// 多参考图生视频：media 数组，type=reference_image；支持 ratio
		media := make([]map[string]interface{}, 0, len(allImages))
		for _, u := range allImages {
			media = append(media, map[string]interface{}{
				"type": "reference_image",
				"url":  u,
			})
		}
		input["media"] = media
	case "t2v":
		// 文生视频：ratio 可选
		if req.AspectRatio != "" {
			// ratio 放在 parameters（见下），t2v input 里不需要额外字段
		}
	}

	// 构建 parameters（顶层，与 input 平级）
	params := map[string]interface{}{
		"watermark": false,
	}
	if req.Duration > 0 {
		params["duration"] = int(req.Duration)
	}
	// ratio：t2v 和 r2v 支持；i2v 不支持（自动跟随输入图片比例）
	if req.AspectRatio != "" && mode != "i2v" {
		params["ratio"] = req.AspectRatio
	}

	apiReq := map[string]interface{}{
		"model":      model,
		"input":      input,
		"parameters": params,
	}

	respBody, status, err := p.doRequest(ctx, "POST",
		"/api/v1/services/aigc/video-generation/video-synthesis",
		apiReq, true)
	if err != nil {
		return nil, fmt.Errorf("happyhorse submit task: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return nil, fmt.Errorf("happyhorse submit task: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		Output struct {
			TaskID     string `json:"task_id"`
			TaskStatus string `json:"task_status"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("happyhorse parse submit response: %w", err)
	}
	if result.Code != "" {
		return nil, fmt.Errorf("happyhorse API error: code=%s, message=%s", result.Code, result.Message)
	}

	return &VideoTask{
		TaskID:   result.Output.TaskID,
		Status:   mapHappyHorseStatus(result.Output.TaskStatus),
		Provider: p.GetName(),
	}, nil
}

// GetVideoStatus 查询 HappyHorse 任务状态
func (p *HappyHorseProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	respBody, status, err := p.doRequest(ctx, "GET", "/api/v1/tasks/"+taskID, nil, false)
	if err != nil {
		return nil, fmt.Errorf("happyhorse get status: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("happyhorse get status: status %d", status)
	}

	var result struct {
		Output struct {
			TaskID     string `json:"task_id"`
			TaskStatus string `json:"task_status"`
			Code       string `json:"code"`
			Message    string `json:"message"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("happyhorse parse status response: %w", err)
	}
	if result.Code != "" {
		return nil, fmt.Errorf("happyhorse API error: code=%s, message=%s", result.Code, result.Message)
	}

	taskStatus := &VideoTaskStatus{
		TaskID: result.Output.TaskID,
		Status: mapHappyHorseStatus(result.Output.TaskStatus),
	}
	switch result.Output.TaskStatus {
	case "PENDING":
		taskStatus.Progress = 5
	case "RUNNING":
		taskStatus.Progress = 50
	case "SUCCEEDED":
		taskStatus.Progress = 100
	case "FAILED", "CANCELED":
		taskStatus.Progress = 0
		if result.Output.Message != "" {
			taskStatus.Error = result.Output.Message
		}
	}
	return taskStatus, nil
}

// GetVideoURL 获取已完成 HappyHorse 任务的视频 URL（有效期 24 小时）
func (p *HappyHorseProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	respBody, status, err := p.doRequest(ctx, "GET", "/api/v1/tasks/"+taskID, nil, false)
	if err != nil {
		return "", fmt.Errorf("happyhorse get video: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("happyhorse get video: status %d", status)
	}

	var result struct {
		Output struct {
			TaskStatus string `json:"task_status"`
			VideoURL   string `json:"video_url"`
			Code       string `json:"code"`
			Message    string `json:"message"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("happyhorse parse video response: %w", err)
	}
	if result.Code != "" {
		return "", fmt.Errorf("happyhorse API error: code=%s, message=%s", result.Code, result.Message)
	}

	if result.Output.TaskStatus != "SUCCEEDED" {
		if result.Output.Message != "" {
			return "", fmt.Errorf("happyhorse task not completed: status=%s, message=%s",
				result.Output.TaskStatus, result.Output.Message)
		}
		return "", fmt.Errorf("happyhorse task not completed: status=%s", result.Output.TaskStatus)
	}
	if result.Output.VideoURL == "" {
		return "", fmt.Errorf("happyhorse: no video_url in response")
	}
	return result.Output.VideoURL, nil
}

func mapHappyHorseStatus(s string) string {
	switch s {
	case "PENDING":
		return "pending"
	case "RUNNING":
		return "processing"
	case "SUCCEEDED":
		return "completed"
	case "FAILED", "CANCELED":
		return "failed"
	default:
		return "pending"
	}
}
