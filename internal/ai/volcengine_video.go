package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
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

	logger.Printf("[jimeng-video] GenerateVideo: mode=%s images=%d frames=%d duration=%.1fs promptLen=%d",
		reqKey, len(allImages), frames, req.Duration, len(req.Prompt))

	params := map[string]interface{}{
		"req_key": reqKey,
		"prompt":  req.Prompt,
		"seed":    -1,
		"frames":  frames,
	}

	if len(allImages) > 0 {
		// I2V 系列：公网 URL → image_urls；file:// 本地路径 → binary_data_base64
		var urls, b64s []string
		for _, img := range allImages {
			switch {
			case strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://"):
				urls = append(urls, img)
			case strings.HasPrefix(img, "file://"):
				filePath := strings.TrimPrefix(img, "file://")
				if data, err := os.ReadFile(filePath); err == nil && len(data) > 0 {
					b64s = append(b64s, base64.StdEncoding.EncodeToString(data))
				} else {
					logger.Errorf("[jimeng-video] cannot read local file %q: %v", filePath, err)
				}
			default:
				logger.Errorf("[jimeng-video] skipping non-public image URL %q", img)
			}
		}
		if len(urls) > 0 {
			params["image_urls"] = urls
		}
		if len(b64s) > 0 {
			params["binary_data_base64"] = b64s
		}
		if len(urls) == 0 && len(b64s) == 0 {
			logger.Errorf("[jimeng-video] all images were filtered out, falling back to T2V")
		}
	} else {
		// T2V：支持 aspect_ratio
		ar := req.AspectRatio
		if ar == "" {
			ar = "16:9"
		}
		params["aspect_ratio"] = ar
		logger.Printf("[jimeng-video] GenerateVideo: T2V aspect_ratio=%s", ar)
	}

	body, err := json.Marshal(params)
	if err != nil {
		logger.Errorf("[jimeng-video] GenerateVideo: 序列化请求失败: %v", err)
		return nil, fmt.Errorf("jimeng-video: 序列化请求失败: %w", err)
	}

	respBody, err := p.doRequest(ctx, volcengineActionSubmit, body)
	if err != nil {
		logger.Errorf("[jimeng-video] GenerateVideo: HTTP请求失败: %v", err)
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
		logger.Errorf("[jimeng-video] GenerateVideo: 解析响应失败: %v, body=%s", err, string(respBody))
		return nil, fmt.Errorf("jimeng-video: 解析提交响应失败: %w", err)
	}
	if resp.Code != 10000 {
		logger.Errorf("[jimeng-video] GenerateVideo: API返回错误 code=%d message=%s", resp.Code, resp.Message)
		return nil, fmt.Errorf("jimeng-video: 提交任务失败 code=%d: %s", resp.Code, resp.Message)
	}
	if resp.Data.TaskID == "" {
		logger.Errorf("[jimeng-video] GenerateVideo: API成功但未返回task_id, body=%s", string(respBody))
		return nil, fmt.Errorf("jimeng-video: 未返回 task_id")
	}

	taskID := prefix + resp.Data.TaskID
	logger.Printf("[jimeng-video] GenerateVideo: 任务提交成功 taskID=%s mode=%s", taskID, reqKey)
	return &VideoTask{
		TaskID:   taskID,
		Status:   "pending",
		Provider: p.GetName(),
	}, nil
}

// GetVideoStatus 查询任务状态
func (p *JimengVideoProvider) GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error) {
	reqKey, rawID := jimengParseTaskID(taskID)

	logger.Printf("[jimeng-video] GetVideoStatus: taskID=%s reqKey=%s", rawID, reqKey)

	body, _ := json.Marshal(map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	})

	respBody, err := p.doRequest(ctx, volcengineActionGetResult, body)
	if err != nil {
		logger.Errorf("[jimeng-video] GetVideoStatus: HTTP请求失败 taskID=%s: %v", rawID, err)
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
		logger.Errorf("[jimeng-video] GetVideoStatus: 解析响应失败 taskID=%s: %v, body=%s", rawID, err, string(respBody))
		return nil, fmt.Errorf("jimeng-video: 解析状态响应失败: %w", err)
	}
	if resp.Code != 10000 {
		logger.Errorf("[jimeng-video] GetVideoStatus: API错误 taskID=%s code=%d message=%s", rawID, resp.Code, resp.Message)
		return &VideoTaskStatus{
			TaskID: rawID,
			Status: "failed",
			Error:  fmt.Sprintf("code=%d: %s", resp.Code, resp.Message),
		}, nil
	}
	if resp.Data == nil {
		logger.Errorf("[jimeng-video] GetVideoStatus: data为null taskID=%s", rawID)
		return &VideoTaskStatus{TaskID: rawID, Status: "failed", Error: "data is null"}, nil
	}

	status := jimengMapStatus(resp.Data.Status)
	logger.Printf("[jimeng-video] GetVideoStatus: taskID=%s rawStatus=%s mappedStatus=%s", rawID, resp.Data.Status, status)

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
		logger.Errorf("[jimeng-video] GetVideoStatus: 任务异常 taskID=%s status=%s", rawID, resp.Data.Status)
	}
	return ts, nil
}

// GetVideoURL 获取已完成任务的视频URL
func (p *JimengVideoProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	reqKey, rawID := jimengParseTaskID(taskID)

	logger.Printf("[jimeng-video] GetVideoURL: taskID=%s reqKey=%s", rawID, reqKey)

	body, _ := json.Marshal(map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	})

	respBody, err := p.doRequest(ctx, volcengineActionGetResult, body)
	if err != nil {
		logger.Errorf("[jimeng-video] GetVideoURL: HTTP请求失败 taskID=%s: %v", rawID, err)
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
		logger.Errorf("[jimeng-video] GetVideoURL: 解析响应失败 taskID=%s: %v", rawID, err)
		return "", fmt.Errorf("jimeng-video: 解析视频URL响应失败: %w", err)
	}
	if resp.Code != 10000 {
		logger.Errorf("[jimeng-video] GetVideoURL: API错误 taskID=%s code=%d message=%s", rawID, resp.Code, resp.Message)
		return "", fmt.Errorf("jimeng-video: 获取结果失败 code=%d: %s", resp.Code, resp.Message)
	}
	if resp.Data == nil {
		logger.Errorf("[jimeng-video] GetVideoURL: data为null taskID=%s", rawID)
		return "", fmt.Errorf("jimeng-video: data is null")
	}
	if resp.Data.Status != "done" {
		logger.Errorf("[jimeng-video] GetVideoURL: 任务未完成 taskID=%s status=%s", rawID, resp.Data.Status)
		return "", fmt.Errorf("jimeng-video: 任务未完成，status=%s", resp.Data.Status)
	}
	if resp.Data.VideoURL == "" {
		logger.Errorf("[jimeng-video] GetVideoURL: done但video_url为空 taskID=%s", rawID)
		return "", fmt.Errorf("jimeng-video: 任务完成但未返回 video_url")
	}
	logger.Printf("[jimeng-video] GetVideoURL: 成功获取URL taskID=%s url=%s", rawID, resp.Data.VideoURL)
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
		logger.Errorf("[jimeng-video] doRequest: action=%s 网络错误: %v", action, err)
		return nil, fmt.Errorf("jimeng-video: HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf("[jimeng-video] doRequest: action=%s 读取响应体失败: %v", action, err)
		return nil, fmt.Errorf("jimeng-video: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(respBody)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		logger.Errorf("[jimeng-video] doRequest: action=%s HTTP %d body=%s", action, resp.StatusCode, snippet)
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
