package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ProviderNameDoubaoVideo 豆包视频提供者名称
const ProviderNameDoubaoVideo = "doubao-video"

// doubaoVideoDefaultEndpoint 豆包视频 API 默认端点（火山引擎 Ark，华北2/北京）
const doubaoVideoDefaultEndpoint = "https://ark.cn-beijing.volces.com/api/v3"

// DoubaoVideoProvider 豆包 Seedance 视频生成提供者（火山引擎 Ark 平台）
//
// API: POST https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks
// 鉴权: Bearer Token（Ark API Key）
//
// 支持模型（model 字段填写 Model ID 或推理接入点 Endpoint ID）：
//   - doubao-seedance-2-0-*        Seedance 2.0 系列（多模态参考生视频/图生/文生）
//   - doubao-seedance-1-5-Pro-*    Seedance 1.5 Pro（图生/文生/有声/无声）
//   - doubao-seedance-1-0-pro-*    Seedance 1.0 Pro（图生/文生）
//   - doubao-seedance-1-0-pro-fast-* Seedance 1.0 Pro Fast
//
// 参数传入方式（新方式，推荐）：resolution/ratio/duration/seed/camera_fixed/watermark/generate_audio
// 作为 request body 顶层字段直接传入，不再使用旧版内联标志（--ratio/--dur 等）。
type DoubaoVideoProvider struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

// NewDoubaoVideoProvider 创建豆包视频提供者。endpoint 为空时使用 cn-beijing 默认端点。
func NewDoubaoVideoProvider(apiKey, endpoint string) *DoubaoVideoProvider {
	if endpoint == "" {
		endpoint = doubaoVideoDefaultEndpoint
	}
	return &DoubaoVideoProvider{
		apiKey:   apiKey,
		endpoint: endpoint,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *DoubaoVideoProvider) GetName() string { return ProviderNameDoubaoVideo }

func (p *DoubaoVideoProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
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

// GenerateVideo 提交豆包视频生成任务（文生/图生/多模态参考生视频）
//
// 图片数量与生成模式的对应关系：
//   - 0 张图片                      → 文生视频（T2V）
//   - 1 张图片（ImageURL 或 ImageURLs[0]）→ 图生视频-首帧（role=first_frame）
//   - 2 张图片                      → 图生视频-首尾帧（role=first_frame + last_frame）
//   - 3+ 张图片                     → 多模态参考（role=reference_image，Seedance 2.0）
//
// 视频/音频参考（Seedance 2.0）通过 VideoURLs/AudioURLs 传入，role 分别为 reference_video/reference_audio。
//
// 参数均以顶层字段形式传入（新方式）：resolution、ratio、duration、seed、camera_fixed、watermark、generate_audio。
func (p *DoubaoVideoProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	model := req.Model
	if model == "" {
		return nil, fmt.Errorf("doubao-video: model（Model ID 或 Endpoint ID）不能为空")
	}

	// 汇总所有参考图（去重，ImageURL 插入首位）
	allImages := doubaoCollectImages(req.ImageURL, req.ImageURLs)

	// 构建 content 数组
	// 若提供了草稿任务 ID（draft_task_id），则使用草稿续接模式（第二步）
	var content []map[string]interface{}
	if req.DraftTaskID != "" {
		content = doubaoMakeDraftContent(req.DraftTaskID, req.Prompt)
	} else {
		content = doubaoMakeContent(allImages, req.VideoURLs, req.AudioURLs, req.Prompt)
	}

	// 构建请求体（顶层参数，新方式）
	apiReq := map[string]interface{}{
		"model":   model,
		"content": content,
	}

	// ratio（宽高比）：优先使用 AspectRatio，不传则由模型自动选择
	if req.AspectRatio != "" {
		apiReq["ratio"] = req.AspectRatio
	}

	// resolution（分辨率）：480p / 720p / 1080p / 4k，默认模型自选
	if req.Resolution != "" {
		apiReq["resolution"] = req.Resolution
	}

	// duration（时长，整数秒）；-1 由模型自动选择（Seedance 2.0/1.5）
	if req.Duration != 0 {
		if req.Duration == -1 {
			apiReq["duration"] = -1
		} else {
			apiReq["duration"] = int(req.Duration)
		}
	}

	// seed；0 和 -1 均表示随机，仅在明确指定正整数时传入
	if req.Seed > 0 {
		apiReq["seed"] = req.Seed
	}

	// camera_fixed：CameraMovement="static" 映射为 true
	if req.CameraMovement == "static" {
		apiReq["camera_fixed"] = true
	}

	// watermark
	if req.Watermark {
		apiReq["watermark"] = true
	}

	// generate_audio（仅 Seedance 2.0/1.5 Pro 支持）
	if req.GenerateAudio != nil {
		apiReq["generate_audio"] = *req.GenerateAudio
	}

	// frames（Seedance 1.0 系列，与 duration 二选一；提供时忽略 duration）
	if req.Frames > 0 {
		apiReq["frames"] = req.Frames
		delete(apiReq, "duration")
	}

	// return_last_frame：请求在响应中附带最后一帧 URL
	if req.ReturnLastFrame {
		apiReq["return_last_frame"] = true
	}

	// draft：草稿/预览模式（仅 Seedance 1.5 Pro，快速低质预览）
	if req.Draft {
		apiReq["draft"] = true
	}

	// service_tier："flex" 为离线推理（价格减半，小时级延迟；Seedance 2.0 不支持）
	if req.ServiceTier != "" {
		apiReq["service_tier"] = req.ServiceTier
		if req.ExecutionExpiresAfter > 0 {
			apiReq["execution_expires_after"] = req.ExecutionExpiresAfter
		}
	}

	// callback_url：Webhook 回调地址
	if req.CallbackURL != "" {
		apiReq["callback_url"] = req.CallbackURL
	}

	// priority：请求队列优先级 0-9（仅 Seedance 2.0；离线推理 flex 模式不支持）
	if req.Priority > 0 {
		apiReq["priority"] = req.Priority
	}

	// safety_identifier：终端用户唯一标识，用于平台合规审计
	if req.SafetyIdentifier != "" {
		apiReq["safety_identifier"] = req.SafetyIdentifier
	}

	// tools.web_search：联网搜索工具（仅 Seedance 2.0，模型自主判断是否搜索）
	if req.WebSearchEnabled {
		apiReq["tools"] = []map[string]string{{"type": "web_search"}}
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/contents/generations/tasks", apiReq)
	if err != nil {
		return nil, fmt.Errorf("doubao-video 提交任务失败: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return nil, fmt.Errorf("doubao-video 提交任务失败: HTTP %d, body: %s", status, string(respBody))
	}

	var result struct {
		ID    string `json:"id"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("doubao-video 解析提交响应失败: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("doubao-video API 错误: code=%s, message=%s", result.Error.Code, result.Error.Message)
	}
	if result.ID == "" {
		return nil, fmt.Errorf("doubao-video: 未返回任务 ID，响应体: %s", string(respBody))
	}

	return &VideoTask{
		TaskID:   result.ID,
		Status:   "pending",
		Provider: p.GetName(),
	}, nil
}

// doubaoVideoTaskResp 豆包视频任务查询响应（GET /contents/generations/tasks/{id}）
//
// 文档：https://www.volcengine.com/docs/82379/1521309
// content 为对象（非数组）；created_at/updated_at 为 Unix 时间戳（整数秒）。
type doubaoVideoTaskResp struct {
	ID        string `json:"id"`
	Model     string `json:"model"`  // 任务使用的模型名称-版本
	Status    string `json:"status"` // queued, running, succeeded, failed, expired, cancelled
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`

	// 任务失败时非 null；成功时为 null
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`

	// 任务成功后填充；video_url 有效期 24 小时
	Content *struct {
		VideoURL     string `json:"video_url"`
		LastFrameURL string `json:"last_frame_url,omitempty"` // 仅 return_last_frame=true 时返回
	} `json:"content,omitempty"`

	// 视频元数据（任务完成后返回）
	Resolution      string `json:"resolution,omitempty"`       // 480p / 720p / 1080p / 4k
	Ratio           string `json:"ratio,omitempty"`            // 16:9 / 9:16 / 1:1 ...
	Duration        int    `json:"duration,omitempty"`         // 视频时长（秒）
	Frames          int    `json:"frames,omitempty"`           // 视频帧数（与 duration 二选一）
	FramesPerSecond int    `json:"framespersecond,omitempty"`  // 帧率
	GenerateAudio   *bool  `json:"generate_audio,omitempty"`   // 是否含音频（Seedance 2.0/1.5 Pro）
	Seed            int    `json:"seed,omitempty"`             // 实际使用的随机种子

	// Token 用量（可用于计费对账）
	Usage *struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		ToolUsage        *struct {
			WebSearch int `json:"web_search,omitempty"`
		} `json:"tool_usage,omitempty"`
	} `json:"usage,omitempty"`
}

// GetVideoStatus 查询豆包视频任务状态
func (p *DoubaoVideoProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	respBody, status, err := p.doRequest(ctx, "GET", "/contents/generations/tasks/"+taskID, nil)
	if err != nil {
		return nil, fmt.Errorf("doubao-video 查询状态失败: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("doubao-video 查询状态失败: HTTP %d", status)
	}

	var result doubaoVideoTaskResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("doubao-video 解析状态响应失败: %w", err)
	}

	ts := &VideoTaskStatus{
		TaskID: result.ID,
		Status: mapDoubaoVideoStatus(result.Status),
	}
	switch result.Status {
	case "queued":
		ts.Progress = 3
	case "running":
		ts.Progress = 50
	case "succeeded":
		ts.Progress = 100
		if result.Content != nil && result.Content.LastFrameURL != "" {
			ts.LastFrameURL = result.Content.LastFrameURL
		}
	case "failed", "cancelled", "expired":
		ts.Progress = 0
	}
	if result.Error != nil {
		ts.Error = fmt.Sprintf("code=%s: %s", result.Error.Code, result.Error.Message)
	}
	return ts, nil
}

// GetVideoURL 获取已完成豆包视频任务的视频 URL
//
// 响应中 content 为对象格式：{"video_url": "https://..."}
func (p *DoubaoVideoProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	respBody, status, err := p.doRequest(ctx, "GET", "/contents/generations/tasks/"+taskID, nil)
	if err != nil {
		return "", fmt.Errorf("doubao-video 获取视频 URL 失败: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("doubao-video 获取视频 URL 失败: HTTP %d", status)
	}

	var result doubaoVideoTaskResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("doubao-video 解析视频 URL 响应失败: %w", err)
	}
	if result.Error != nil {
		return "", fmt.Errorf("doubao-video 任务失败: code=%s, message=%s", result.Error.Code, result.Error.Message)
	}
	if result.Status != "succeeded" {
		return "", fmt.Errorf("doubao-video: 任务未完成，status=%s", result.Status)
	}
	if result.Content == nil || result.Content.VideoURL == "" {
		return "", fmt.Errorf("doubao-video: 任务成功但未返回视频 URL")
	}
	return result.Content.VideoURL, nil
}

// ── 辅助函数 ──────────────────────────────────────────────────────────────────

// doubaoCollectImages 汇总参考图列表（ImageURL 插入首位，去重）
func doubaoCollectImages(imageURL string, imageURLs []string) []string {
	all := make([]string, 0, 1+len(imageURLs))
	if imageURL != "" {
		all = append(all, imageURL)
	}
	for _, u := range imageURLs {
		if u != "" && u != imageURL {
			all = append(all, u)
		}
	}
	return all
}

// doubaoMakeContent 根据输入媒体类型构建 content 数组。
//
// 图片 role 规则：
//   - 1 张图片  → role=first_frame
//   - 2 张图片  → role=first_frame + last_frame
//   - 3+ 张图片 → role=reference_image（Seedance 2.0 多模态参考生视频）
//
// 视频 role=reference_video，音频 role=reference_audio（Seedance 2.0）。
func doubaoMakeContent(images, videos, audios []string, prompt string) []map[string]interface{} {
	content := make([]map[string]interface{}, 0, len(images)+len(videos)+len(audios)+1)

	// 图片
	for i, url := range images {
		item := map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]string{"url": url},
		}
		switch {
		case len(images) == 1:
			item["role"] = "first_frame"
		case len(images) == 2 && i == 0:
			item["role"] = "first_frame"
		case len(images) == 2 && i == 1:
			item["role"] = "last_frame"
		default: // 3+
			item["role"] = "reference_image"
		}
		content = append(content, item)
	}

	// 参考视频（Seedance 2.0）
	for _, url := range videos {
		if url != "" {
			content = append(content, map[string]interface{}{
				"type":      "video_url",
				"video_url": map[string]string{"url": url},
				"role":      "reference_video",
			})
		}
	}

	// 参考音频（Seedance 2.0）
	for _, url := range audios {
		if url != "" {
			content = append(content, map[string]interface{}{
				"type":      "audio_url",
				"audio_url": map[string]string{"url": url},
				"role":      "reference_audio",
			})
		}
	}

	// 文本 prompt（可选）
	if prompt != "" {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": prompt,
		})
	}

	return content
}

// doubaoMakeDraftContent 构建草稿续接模式（Draft 第二步）的 content 数组。
//
// 使用 draft_task 类型引用已有草稿任务 ID；可附加文本 prompt 修改最终效果。
// 仅 Seedance 1.5 Pro 支持草稿模式。
func doubaoMakeDraftContent(draftTaskID, prompt string) []map[string]interface{} {
	content := []map[string]interface{}{
		{
			"type":       "draft_task",
			"draft_task": map[string]string{"id": draftTaskID},
		},
	}
	if prompt != "" {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": prompt,
		})
	}
	return content
}

// ── 任务列表查询（Doubao 专有，非 VideoProvider 接口） ─────────────────────────

// DoubaoVideoListFilter 豆包视频任务列表筛选参数
type DoubaoVideoListFilter struct {
	// Status 过滤任务状态：queued / running / cancelled / succeeded / failed
	Status string
	// TaskIDs 精确匹配任务 ID（支持多个，以重复参数名传递）
	TaskIDs []string
	// Model 推理接入点 ID（精确匹配）
	Model string
	// ServiceTier 服务等级：default（在线）/ flex（离线）
	ServiceTier string
}

// DoubaoVideoListRequest 豆包视频任务列表请求
type DoubaoVideoListRequest struct {
	PageNum  int // 页码，默认 1，范围 [1, 500]
	PageSize int // 每页条数，默认 20，范围 [1, 500]
	Filter   DoubaoVideoListFilter
}

// DoubaoVideoListResponse 豆包视频任务列表响应
type DoubaoVideoListResponse struct {
	// Items 任务列表，每项与单任务查询结构一致
	Items []*doubaoVideoTaskResp `json:"items"`
	// Total 符合条件的总任务数
	Total int `json:"total"`
}

// ListVideoTasks 批量查询豆包视频任务列表
//
// 此方法为 DoubaoVideoProvider 专有，不属于 VideoProvider 接口。
// 服务层可通过类型断言调用：
//
//	if lister, ok := provider.(*ai.DoubaoVideoProvider); ok {
//	    resp, err := lister.ListVideoTasks(ctx, req)
//	}
//
// 注意：仅支持查询最近 7 天的任务；视频 URL 有效期 24 小时，需及时转存。
func (p *DoubaoVideoProvider) ListVideoTasks(ctx context.Context, req *DoubaoVideoListRequest) (*DoubaoVideoListResponse, error) {
	params := url.Values{}

	if req.PageNum > 0 {
		params.Set("page_num", strconv.Itoa(req.PageNum))
	}
	if req.PageSize > 0 {
		params.Set("page_size", strconv.Itoa(req.PageSize))
	}
	if req.Filter.Status != "" {
		params.Set("filter.status", req.Filter.Status)
	}
	for _, id := range req.Filter.TaskIDs {
		if id != "" {
			params.Add("filter.task_ids", id)
		}
	}
	if req.Filter.Model != "" {
		params.Set("filter.model", req.Filter.Model)
	}
	if req.Filter.ServiceTier != "" {
		params.Set("filter.service_tier", req.Filter.ServiceTier)
	}

	path := "/contents/generations/tasks"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("doubao-video 查询任务列表失败: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("doubao-video 查询任务列表失败: HTTP %d, body: %s", status, string(respBody))
	}

	var result DoubaoVideoListResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("doubao-video 解析任务列表失败: %w", err)
	}
	return &result, nil
}

// DeleteVideoTask 取消或删除豆包视频任务
//
// 此方法为 DoubaoVideoProvider 专有，不属于 VideoProvider 接口。
//
// 行为取决于任务当前状态：
//   - queued    → 取消排队，状态变为 cancelled
//   - succeeded / failed / expired → 删除任务记录，不可再查询
//   - running / cancelled → 不支持 DELETE，会返回 API 错误
//
// 接口无响应体，成功返回 nil。
func (p *DoubaoVideoProvider) DeleteVideoTask(ctx context.Context, taskID string) error {
	respBody, status, err := p.doRequest(ctx, "DELETE", "/contents/generations/tasks/"+taskID, nil)
	if err != nil {
		return fmt.Errorf("doubao-video 取消/删除任务失败: %w", err)
	}
	// 成功时通常返回 200 或 204（No Content）
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("doubao-video 取消/删除任务失败: HTTP %d, body: %s", status, string(respBody))
	}
	return nil
}

// mapDoubaoVideoStatus 将 Ark 平台任务状态映射为系统统一状态
//
// Ark 状态: queued → running → succeeded / failed / expired / cancelled
func mapDoubaoVideoStatus(s string) string {
	switch s {
	case "queued":
		return "pending"
	case "running":
		return "processing"
	case "succeeded":
		return "completed"
	case "failed", "cancelled", "expired":
		return "failed"
	default:
		return s
	}
}
