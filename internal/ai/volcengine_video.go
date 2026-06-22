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

// ProviderNameJimengVideo 即梦视频3.0提供者名称
const ProviderNameJimengVideo = "jimeng-video"

// 即梦视频3.0 req_key 常量
const (
	jimengReqKeyT2V     = "jimeng_t2v_v30"            // 文生视频
	jimengReqKeyI2V     = "jimeng_i2v_first_v30"      // 图生视频（首帧）
	jimengReqKeyI2VTail = "jimeng_i2v_first_tail_v30" // 图生视频（首尾帧）
)

// taskID 前缀，用于在 GetVideoStatus/GetVideoURL 中恢复 req_key
const (
	jimengPrefixT2V     = "t2v:"
	jimengPrefixI2V     = "i2v:"
	jimengPrefixI2VTail = "i2v-tail:"
)

// JimengVideoProvider 即梦视频3.0提供者
//
// 鉴权：与 volcengine_visual.go 完全相同的 HMAC-SHA256 AK/SK 签名
// API：两步异步接口
//   - 提交：CVSync2AsyncSubmitTask（req_key=jimeng_t2v_v30 或 jimeng_i2v_v30）
//   - 查询：CVSync2AsyncGetResult
//
// 文档：https://www.volcengine.com/docs/85621/1792704（T2V）
//
//	https://www.volcengine.com/docs/85621/1785204（I2V 首帧）
//	https://www.volcengine.com/docs/85621/1791184（I2V 首尾帧）
type JimengVideoProvider struct {
	accessKey string
	secretKey string
	client    *http.Client
}

// NewJimengVideoProvider 创建即梦视频3.0提供者
func NewJimengVideoProvider(accessKey, secretKey string) *JimengVideoProvider {
	return &JimengVideoProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *JimengVideoProvider) GetName() string { return ProviderNameJimengVideo }

// GenerateVideo 提交即梦视频生成任务（非阻塞，返回 task_id）
//
// 模式选择：
//   - 无图片                              → T2V 文生视频（req_key=jimeng_t2v_v30）
//   - ImageURL 非空，ImageURLs 为空       → I2V 首帧（req_key=jimeng_i2v_first_v30）
//   - ImageURL + ImageURLs[0] 均非空      → I2V 首尾帧（req_key=jimeng_i2v_first_tail_v30）
//   - ImageURL 为空，len(ImageURLs) >= 2  → I2V 首尾帧（ImageURLs[0]=首帧，ImageURLs[1]=尾帧）
//
// Duration → frames：≤7s → 121（5s），>7s → 241（10s）
func (p *JimengVideoProvider) GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error) {
	// 收集所有图片（首帧优先）
	var allImages []string
	if req.ImageURL != "" {
		allImages = append(allImages, req.ImageURL)
	}
	for _, u := range req.ImageURLs {
		if u != "" && u != req.ImageURL {
			allImages = append(allImages, u)
		}
	}

	var reqKey, prefix string
	switch {
	case len(allImages) >= 2:
		// 首尾帧模式：取前两张
		allImages = allImages[:2]
		reqKey = jimengReqKeyI2VTail
		prefix = jimengPrefixI2VTail
	case len(allImages) == 1:
		// 首帧模式
		reqKey = jimengReqKeyI2V
		prefix = jimengPrefixI2V
	default:
		// 文生视频
		reqKey = jimengReqKeyT2V
		prefix = jimengPrefixT2V
	}

	frames := 121 // 默认5秒
	if req.Duration > 7 {
		frames = 241 // 10秒
	}

	params := map[string]interface{}{
		"req_key": reqKey,
		"prompt":  req.Prompt,
		"seed":    -1,
		"frames":  frames,
	}

	if len(allImages) > 0 {
		// I2V 系列：图片用 image_urls 数组，无 aspect_ratio
		params["image_urls"] = allImages
	} else {
		// T2V：支持 aspect_ratio
		ar := req.AspectRatio
		if ar == "" {
			ar = "16:9"
		}
		params["aspect_ratio"] = ar
	}

	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("jimeng-video: 序列化请求失败: %w", err)
	}

	respBody, err := p.doRequest(ctx, volcengineActionSubmit, body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("jimeng-video: 解析提交响应失败: %w", err)
	}
	if resp.Code != 10000 {
		return nil, fmt.Errorf("jimeng-video: 提交任务失败 code=%d: %s", resp.Code, resp.Message)
	}
	if resp.Data.TaskID == "" {
		return nil, fmt.Errorf("jimeng-video: 未返回 task_id")
	}

	return &VideoTask{
		TaskID:   prefix + resp.Data.TaskID,
		Status:   "pending",
		Provider: p.GetName(),
	}, nil
}

// GetVideoStatus 查询任务状态
func (p *JimengVideoProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	reqKey, rawID := jimengParseTaskID(taskID)

	body, _ := json.Marshal(map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	})

	respBody, err := p.doRequest(ctx, volcengineActionGetResult, body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Status   string `json:"status"`
			VideoURL string `json:"video_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("jimeng-video: 解析状态响应失败: %w", err)
	}
	if resp.Code != 10000 {
		return &VideoTaskStatus{
			TaskID: rawID,
			Status: "failed",
			Error:  fmt.Sprintf("code=%d: %s", resp.Code, resp.Message),
		}, nil
	}
	if resp.Data == nil {
		return &VideoTaskStatus{TaskID: rawID, Status: "failed", Error: "data is null"}, nil
	}

	status := jimengMapStatus(resp.Data.Status)
	ts := &VideoTaskStatus{
		TaskID: rawID,
		Status: status,
	}
	switch resp.Data.Status {
	case "in_queue":
		ts.Progress = 5
	case "generating":
		ts.Progress = 50
	case "done":
		ts.Progress = 100
	case "not_found", "expired":
		ts.Error = resp.Data.Status
	}
	return ts, nil
}

// GetVideoURL 获取已完成任务的视频URL
func (p *JimengVideoProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	reqKey, rawID := jimengParseTaskID(taskID)

	body, _ := json.Marshal(map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	})

	respBody, err := p.doRequest(ctx, volcengineActionGetResult, body)
	if err != nil {
		return "", err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Status   string `json:"status"`
			VideoURL string `json:"video_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("jimeng-video: 解析视频URL响应失败: %w", err)
	}
	if resp.Code != 10000 {
		return "", fmt.Errorf("jimeng-video: 获取结果失败 code=%d: %s", resp.Code, resp.Message)
	}
	if resp.Data == nil {
		return "", fmt.Errorf("jimeng-video: data is null")
	}
	if resp.Data.Status != "done" {
		return "", fmt.Errorf("jimeng-video: 任务未完成，status=%s", resp.Data.Status)
	}
	if resp.Data.VideoURL == "" {
		return "", fmt.Errorf("jimeng-video: 任务完成但未返回 video_url")
	}
	return resp.Data.VideoURL, nil
}

// doRequest 发送带 Volcengine HMAC-SHA256 签名的请求（与 volcengine_visual.go 完全相同）
func (p *JimengVideoProvider) doRequest(ctx context.Context, action string, body []byte) ([]byte, error) {
	now := time.Now().UTC()
	queryString := fmt.Sprintf("Action=%s&Version=%s", action, volcengineVisualVersion)

	longDate := now.Format("20060102T150405Z")
	shortDate := now.Format("20060102")

	bodyHash := volcSHA256Hex(body)

	signedHeaders := "content-type;host;x-content-sha256;x-date"
	canonicalHeaders := fmt.Sprintf(
		"content-type:application/json\nhost:%s\nx-content-sha256:%s\nx-date:%s\n",
		volcengineVisualHost, bodyHash, longDate,
	)

	canonicalRequest := strings.Join([]string{
		"POST",
		"/",
		queryString,
		canonicalHeaders,
		signedHeaders,
		bodyHash,
	}, "\n")

	scope := shortDate + "/" + volcengineVisualRegion + "/" + volcengineVisualService + "/request"
	stringToSign := "HMAC-SHA256\n" + longDate + "\n" + scope + "\n" + volcSHA256Hex([]byte(canonicalRequest))

	kDate := volcHMACSHA256([]byte(p.secretKey), []byte(shortDate))
	kRegion := volcHMACSHA256(kDate, []byte(volcengineVisualRegion))
	kService := volcHMACSHA256(kRegion, []byte(volcengineVisualService))
	kSigning := volcHMACSHA256(kService, []byte("request"))

	signature := fmt.Sprintf("%x", volcHMACSHA256(kSigning, []byte(stringToSign)))
	authorization := fmt.Sprintf(
		"HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.accessKey, scope, signedHeaders, signature,
	)

	url := fmt.Sprintf("%s/?%s", volcengineVisualEndpoint, queryString)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jimeng-video: 创建请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Host", volcengineVisualHost)
	httpReq.Header.Set("X-Content-Sha256", bodyHash)
	httpReq.Header.Set("X-Date", longDate)
	httpReq.Header.Set("Authorization", authorization)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jimeng-video: HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("jimeng-video: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jimeng-video: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// jimengParseTaskID 解析带前缀的 taskID，返回 req_key 和原始 task_id
func jimengParseTaskID(taskID string) (reqKey, rawID string) {
	switch {
	case strings.HasPrefix(taskID, jimengPrefixI2VTail):
		return jimengReqKeyI2VTail, taskID[len(jimengPrefixI2VTail):]
	case strings.HasPrefix(taskID, jimengPrefixI2V):
		return jimengReqKeyI2V, taskID[len(jimengPrefixI2V):]
	default:
		// T2V（含 t2v: 前缀和裸 ID）
		return jimengReqKeyT2V, strings.TrimPrefix(taskID, jimengPrefixT2V)
	}
}

// jimengMapStatus 将即梦状态映射为通用状态
func jimengMapStatus(s string) string {
	switch s {
	case "in_queue":
		return "pending"
	case "generating":
		return "processing"
	case "done":
		return "completed"
	case "not_found", "expired":
		return "failed"
	default:
		return s
	}
}
