package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai/aliyun"
)

// ProviderNameMinimaxVideo MiniMax 视频提供者名称
const ProviderNameMinimaxVideo = "minimax-video"

const minimaxVideoBaseURL = "https://api.minimaxi.com"

func init() {
	RegisterVideoEngineTraits(ProviderNameMinimaxVideo, VideoEngineTraits{
		// duration 只能是 6 或 10（具体可选值随 model/resolution 组合变化），不支持任意时长。
		SnapsFixedDuration: true,
	})
}

// minimaxT2VOnlyModels 仅支持文生视频（无 first_frame_image 时可用）的模型。
var minimaxT2VOnlyModels = map[string]bool{
	"T2V-01-Director": true,
	"T2V-01":          true,
}

// minimaxI2VOnlyModels 仅支持图生视频（必须提供 first_frame_image）的模型。
var minimaxI2VOnlyModels = map[string]bool{
	"MiniMax-Hailuo-2.3-Fast": true,
	"I2V-01-Director":         true,
	"I2V-01-live":             true,
	"I2V-01":                  true,
}

// MinimaxVideoProvider MiniMax Hailuo 视频生成提供者（文生视频 + 图生视频，同一 API）
//
// 三步式调用（提交 → 轮询状态 → 取文件下载地址），三个独立端点：
//   - 提交：  POST https://api.minimaxi.com/v1/video_generation
//   - 查询：  GET  https://api.minimaxi.com/v1/query/video_generation?task_id=xxx
//   - 取文件：GET  https://api.minimaxi.com/v1/files/retrieve?file_id=xxx（download_url 有效期 1 小时）
//
// 使用方式：
//   - req.ImageURL 非空 → 图生视频（first_frame_image），否则 → 文生视频（仅 prompt）
//   - req.Prompt        → prompt（文生视频必填；图生视频可选，用于运镜/描述补充）
//   - req.Duration      → duration（秒，整数；不同 model/resolution 组合支持的取值不同，未做本地校验，交由 API 侧校验）
//   - req.Resolution    → resolution（720P/768P/1080P/512P，同上）
//   - req.Model         → model；留空时默认 MiniMax-Hailuo-2.3（文生/图生均支持）
//   - req.CallbackURL   → callback_url（任务状态变更 Webhook）
//   - req.Watermark     → aigc_watermark
type MinimaxVideoProvider struct {
	apiKey string
	client *http.Client
}

// NewMinimaxVideoProvider 创建 MiniMax 视频生成提供者
func NewMinimaxVideoProvider(apiKey string) *MinimaxVideoProvider {
	return &MinimaxVideoProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 60 * time.Second}, // 三个端点均为快速调用（提交/查询/取文件），非长轮询
	}
}

func (p *MinimaxVideoProvider) GetName() string { return ProviderNameMinimaxVideo }

func (p *MinimaxVideoProvider) doRequest(ctx context.Context, method, path string, query url.Values, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}

	fullURL := minimaxVideoBaseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// minimaxBaseResp MiniMax 各接口通用的状态字段
type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// errString 格式化 base_resp 错误，1002（限流）附加 "rate limit" 关键字方便上层识别。
func (b minimaxBaseResp) errString(op string) error {
	suffix := ""
	if b.StatusCode == 1002 {
		suffix = " (rate limit)"
	}
	return fmt.Errorf("minimax-video: %s API error %d: %s%s", op, b.StatusCode, b.StatusMsg, suffix)
}

// GenerateVideo 提交 MiniMax 视频生成任务（文生视频或图生视频，取决于 req.ImageURL 是否非空）
func (p *MinimaxVideoProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	isI2V := req.ImageURL != ""

	model := req.Model
	if model == "" {
		model = "MiniMax-Hailuo-2.3" // 文生/图生均支持
	}
	if isI2V && minimaxT2VOnlyModels[model] {
		return nil, fmt.Errorf("minimax-video: model %q is text-to-video only, but an ImageURL was provided", model)
	}
	if !isI2V && minimaxI2VOnlyModels[model] {
		return nil, fmt.Errorf("minimax-video: model %q requires ImageURL (image-to-video only)", model)
	}
	if !isI2V && req.Prompt == "" {
		return nil, fmt.Errorf("minimax-video: prompt is required for text-to-video (no ImageURL provided)")
	}

	apiReq := map[string]interface{}{
		"model": model,
	}
	if req.Prompt != "" {
		apiReq["prompt"] = req.Prompt
	}
	if isI2V {
		apiReq["first_frame_image"] = req.ImageURL
	}
	if req.Duration > 0 {
		apiReq["duration"] = int(req.Duration)
	}
	if req.Resolution != "" {
		apiReq["resolution"] = req.Resolution
	}
	if req.Watermark {
		apiReq["aigc_watermark"] = true
	}
	if req.CallbackURL != "" {
		apiReq["callback_url"] = req.CallbackURL
	}

	log.Printf("[minimax-video] GenerateVideo model=%s i2v=%v duration=%v resolution=%v promptLen=%d prompt=%.200q",
		model, isI2V, apiReq["duration"], apiReq["resolution"], len(req.Prompt), req.Prompt)

	respBody, status, err := p.doRequest(ctx, "POST", "/v1/video_generation", nil, apiReq)
	if err != nil {
		return nil, fmt.Errorf("minimax-video: submit request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax-video: submit failed: HTTP %d: %s", status, aliyun.truncate(string(respBody), 300))
	}

	var result struct {
		TaskID   string          `json:"task_id"`
		BaseResp minimaxBaseResp `json:"base_resp"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("minimax-video: parse submit response: %w", err)
	}
	if result.BaseResp.StatusCode != 0 {
		return nil, result.BaseResp.errString("submit")
	}
	if result.TaskID == "" {
		return nil, fmt.Errorf("minimax-video: no task_id in submit response")
	}

	return &VideoTask{
		TaskID:   result.TaskID,
		Status:   "pending",
		Provider: p.GetName(),
	}, nil
}

// minimaxQueryResult GET /v1/query/video_generation 响应
type minimaxQueryResult struct {
	TaskID      string          `json:"task_id"`
	Status      string          `json:"status"` // Preparing, Queueing, Processing, Success, Fail
	FileID      string          `json:"file_id"`
	VideoWidth  int             `json:"video_width"`
	VideoHeight int             `json:"video_height"`
	BaseResp    minimaxBaseResp `json:"base_resp"`
}

func (p *MinimaxVideoProvider) queryTask(ctx context.Context, taskID string) (*minimaxQueryResult, error) {
	query := url.Values{"task_id": {taskID}}
	respBody, status, err := p.doRequest(ctx, "GET", "/v1/query/video_generation", query, nil)
	if err != nil {
		return nil, fmt.Errorf("minimax-video: query task failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax-video: query task failed: HTTP %d: %s", status, aliyun.truncate(string(respBody), 300))
	}

	var result minimaxQueryResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("minimax-video: parse query response: %w", err)
	}
	if result.BaseResp.StatusCode != 0 {
		return nil, result.BaseResp.errString("query")
	}
	return &result, nil
}

// mapMinimaxVideoStatus 将 MiniMax 任务状态映射为系统统一状态
func mapMinimaxVideoStatus(s string) string {
	switch s {
	case "Preparing", "Queueing":
		return "pending"
	case "Processing":
		return "processing"
	case "Success":
		return "completed"
	case "Fail":
		return "failed"
	default:
		return s
	}
}

// GetVideoStatus 查询 MiniMax 视频任务状态
func (p *MinimaxVideoProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	result, err := p.queryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}

	ts := &VideoTaskStatus{
		TaskID: result.TaskID,
		Status: mapMinimaxVideoStatus(result.Status),
		FileID: result.FileID,
	}
	switch result.Status {
	case "Preparing":
		ts.Progress = 3
	case "Queueing":
		ts.Progress = 10
	case "Processing":
		ts.Progress = 50
	case "Success":
		ts.Progress = 100
	case "Fail":
		ts.Progress = 0
		ts.Error = "minimax-video: generation failed"
	}
	return ts, nil
}

// minimaxRetrieveFileResult GET /v1/files/retrieve 响应
type minimaxRetrieveFileResult struct {
	File struct {
		FileID      int64  `json:"file_id"`
		Bytes       int64  `json:"bytes"`
		Filename    string `json:"filename"`
		DownloadURL string `json:"download_url"`
	} `json:"file"`
	BaseResp minimaxBaseResp `json:"base_resp"`
}

// GetVideoURL 获取已完成 MiniMax 视频任务的下载地址。
//
// 三步式的最后一步：先查询任务状态换取 file_id，再用 file_id 调用文件检索接口换取
// download_url（有效期 1 小时，调用方需及时下载/转存）。
func (p *MinimaxVideoProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	task, err := p.queryTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if task.Status != "Success" {
		return "", fmt.Errorf("minimax-video: task not completed, status=%s", task.Status)
	}
	if task.FileID == "" {
		return "", fmt.Errorf("minimax-video: task succeeded but no file_id returned")
	}

	query := url.Values{"file_id": {task.FileID}}
	respBody, status, err := p.doRequest(ctx, "GET", "/v1/files/retrieve", query, nil)
	if err != nil {
		return "", fmt.Errorf("minimax-video: retrieve file failed: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("minimax-video: retrieve file failed: HTTP %d: %s", status, aliyun.truncate(string(respBody), 300))
	}

	var result minimaxRetrieveFileResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("minimax-video: parse retrieve file response: %w", err)
	}
	if result.BaseResp.StatusCode != 0 {
		return "", result.BaseResp.errString("retrieve file")
	}
	if result.File.DownloadURL == "" {
		return "", fmt.Errorf("minimax-video: no download_url in retrieve file response")
	}
	return result.File.DownloadURL, nil
}

var _ VideoProvider = (*MinimaxVideoProvider)(nil)
