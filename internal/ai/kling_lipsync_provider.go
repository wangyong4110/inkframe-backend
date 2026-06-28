package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// KlingLipSyncProvider 可灵数字人视频（口型对齐）提供者
//
// 使用 Kling Virtual Human API:
//   POST /v1/videos/virtual-human  → 提交任务（角色图 + 音频 → 口型视频）
//   GET  /v1/videos/virtual-human/{task_id} → 查询进度
//
// 与普通视频生成共用 AK/SK 鉴权（HS256 JWT）。
type KlingLipSyncProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

const (
	klingLipSyncDefaultEndpoint = "https://api-beijing.klingai.com"
	klingLipSyncPollInterval    = 3 * time.Second
	klingLipSyncMaxWait         = 10 * time.Minute
)

// NewKlingLipSyncProvider 创建可灵口型对齐提供者。
// accessKey / secretKey 与视频生成共用同一对密钥。
func NewKlingLipSyncProvider(accessKey, secretKey, endpoint string) *KlingLipSyncProvider {
	return &KlingLipSyncProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, klingLipSyncDefaultEndpoint),
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *KlingLipSyncProvider) GetName() string { return "kling-lipsync" }

func (p *KlingLipSyncProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	return klingDoRequest(ctx, p.accessKey, p.secretKey, p.endpoint, p.client, method, path, body)
}

// GenerateLipSync 提交数字人口型对齐任务。
// req.ImageURL: 角色参考图（人像照，建议正面面部清晰）
// req.AudioURL: TTS 音频 URL（mp3/wav，时长应 ≤60s）
func (p *KlingLipSyncProvider) GenerateLipSync(ctx context.Context, req *LipSyncRequest) (*LipSyncTask, error) {
	if req.ImageURL == "" {
		return nil, fmt.Errorf("kling-lipsync: image_url is required")
	}
	if req.AudioURL == "" {
		return nil, fmt.Errorf("kling-lipsync: audio_url is required")
	}

	model := req.Model
	if model == "" {
		model = "kling-v1-6"
	}
	mode := req.Mode
	if mode == "" {
		mode = "std"
	}

	body := map[string]interface{}{
		"human_image": req.ImageURL,
		"audio_url":   req.AudioURL,
		"model_name":  model,
		"mode":        mode,
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/v1/videos/virtual-human", body)
	if err != nil {
		return nil, fmt.Errorf("kling-lipsync: submit request: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		body := string(respBody)
		if len(body) > 300 {
			body = body[:300] + "..."
		}
		return nil, fmt.Errorf("kling-lipsync: submit failed status=%d body=%s", status, body)
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
		return nil, fmt.Errorf("kling-lipsync: parse submit response: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("kling-lipsync: API error code=%d: %s", result.Code, result.Message)
	}
	if result.Data.TaskID == "" {
		return nil, fmt.Errorf("kling-lipsync: empty task_id in response")
	}

	return &LipSyncTask{
		TaskID:   result.Data.TaskID,
		Status:   klingLipSyncStatusToInternal(result.Data.Status),
		Provider: p.GetName(),
	}, nil
}

// GetLipSyncStatus 查询口型对齐任务状态。
func (p *KlingLipSyncProvider) GetLipSyncStatus(ctx context.Context, taskID string) (*LipSyncTaskStatus, error) {
	path := fmt.Sprintf("/v1/videos/virtual-human/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("kling-lipsync: status request: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("kling-lipsync: status failed HTTP %d", status)
	}

	var raw struct {
		Code int `json:"code"`
		Data struct {
			TaskID        string  `json:"task_id"`
			TaskStatus    string  `json:"task_status"`
			Progress      float64 `json:"progress"`
			TaskStatusMsg string  `json:"task_status_msg"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("kling-lipsync: parse status response: %w", err)
	}

	ts := &LipSyncTaskStatus{
		TaskID:   raw.Data.TaskID,
		Status:   klingLipSyncStatusToInternal(raw.Data.TaskStatus),
		Progress: raw.Data.Progress,
	}
	if raw.Data.TaskStatusMsg != "" && strings.Contains(raw.Data.TaskStatus, "fail") {
		ts.Error = raw.Data.TaskStatusMsg
	}
	return ts, nil
}

// GetLipSyncURL 获取已完成的口型对齐视频 URL。
func (p *KlingLipSyncProvider) GetLipSyncURL(ctx context.Context, taskID string) (string, error) {
	path := fmt.Sprintf("/v1/videos/virtual-human/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("kling-lipsync: get URL request: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("kling-lipsync: get URL failed HTTP %d", status)
	}

	var raw struct {
		Code int `json:"code"`
		Data struct {
			TaskStatus string          `json:"task_status"`
			TaskResult json.RawMessage `json:"task_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return "", fmt.Errorf("kling-lipsync: parse URL response: %w", err)
	}

	switch raw.Data.TaskStatus {
	case "succeed", "success", "completed":
	default:
		return "", fmt.Errorf("kling-lipsync: task not completed status=%s", raw.Data.TaskStatus)
	}

	// task_result 与 image2video 格式相同: { videos: [{url: "..."}] }
	var result struct {
		Videos []struct {
			URL string `json:"url"`
		} `json:"videos"`
	}
	if err := json.Unmarshal(raw.Data.TaskResult, &result); err == nil && len(result.Videos) > 0 {
		if result.Videos[0].URL != "" {
			return result.Videos[0].URL, nil
		}
	}

	// 兼容旧格式: [{ video: { url: "..." } }]
	var works []struct {
		Video struct {
			URL string `json:"url"`
		} `json:"video"`
	}
	if err := json.Unmarshal(raw.Data.TaskResult, &works); err == nil && len(works) > 0 {
		if works[0].Video.URL != "" {
			return works[0].Video.URL, nil
		}
	}

	return "", fmt.Errorf("kling-lipsync: no video URL in task_result")
}

func klingLipSyncStatusToInternal(s string) string {
	switch s {
	case "submitted":
		return "pending"
	case "processing":
		return "processing"
	case "succeed", "success":
		return "completed"
	case "failed":
		return "failed"
	default:
		return s
	}
}

var _ LipSyncProvider = (*KlingLipSyncProvider)(nil)
