package ai

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	volcvisual "github.com/volcengine/volc-sdk-golang/service/visual"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// ErrVideoConcurrentLimit is returned by video providers when the API rejects
// the request due to concurrent task limits (e.g. jimeng-video code=50430).
// Callers should treat this as a transient error and retry after a delay.
var ErrVideoConcurrentLimit = fmt.Errorf("video API concurrent limit reached")

// ProviderNameJimengVideo 即梦视频3.0提供者名称
const ProviderNameJimengVideo = "jimeng-video"

// 即梦视频3.0 req_key 常量
const (
	jimengReqKeyT2V      = "jimeng_t2v_v30"            // 文生视频
	jimengReqKeyT2V1080p = "jimeng_t2v_v30_1080p"      // 文生视频 1080P
	jimengReqKeyI2V      = "jimeng_i2v_first_v30"      // 图生视频（首帧）
	jimengReqKeyI2V1080p = "jimeng_i2v_first_v30_1080" // 图生视频（首帧）1080P
	jimengReqKeyI2VTail      = "jimeng_i2v_first_tail_v30"      // 图生视频（首尾帧）
	jimengReqKeyI2VTail1080p = "jimeng_i2v_first_tail_v30_1080" // 图生视频（首尾帧）1080P
	jimengReqKeyPro      = "jimeng_ti2v_v30_pro"        // 视频3.0Pro（文生视频+图生视频首帧，1080P）
	jimengReqKeyRecamera = "jimeng_i2v_recamera_v30"    // 图生视频-运镜（template_id+camera_strength）
)

// taskID 前缀，用于在 GetVideoStatus/GetVideoURL 中恢复 req_key
const (
	jimengPrefixT2V      = "t2v:"
	jimengPrefixT2V1080p = "t2v-1080p:"
	jimengPrefixI2V      = "i2v:"
	jimengPrefixI2V1080p = "i2v-1080p:"
	jimengPrefixI2VTail      = "i2v-tail:"
	jimengPrefixI2VTail1080p = "i2v-tail-1080p:"
	jimengPrefixPro      = "pro:"
	jimengPrefixRecamera = "recamera:"
)

// JimengVideoProvider 即梦视频3.0提供者
//
// 鉴权：通过 volc-sdk-golang 自动完成 HMAC-SHA256 AK/SK 签名
// API：两步异步接口
//   - 提交：CVSync2AsyncSubmitTask（req_key=jimeng_t2v_v30 或 jimeng_i2v_v30）
//   - 查询：CVSync2AsyncGetResult
//
// 文档：https://www.volcengine.com/docs/85621/1792704（T2V）
//
//	https://www.volcengine.com/docs/85621/1785204（I2V 首帧）
//	https://www.volcengine.com/docs/85621/1791184（I2V 首尾帧）
type JimengVideoProvider struct {
	svc *volcvisual.Visual
}

// NewJimengVideoProvider 创建即梦视频3.0提供者
func NewJimengVideoProvider(accessKey, secretKey string) *JimengVideoProvider {
	svc := volcvisual.NewInstance()
	svc.Client.SetAccessKey(accessKey)
	svc.Client.SetSecretKey(secretKey)
	return &JimengVideoProvider{svc: svc}
}

func (p *JimengVideoProvider) GetName() string { return ProviderNameJimengVideo }

// GenerateVideo 提交即梦视频生成任务（非阻塞，返回 task_id）
//
// 模式选择（标准 3.0）：
//   - 无图片                              → T2V 文生视频（req_key=jimeng_t2v_v30）
//   - ImageURL 非空，ImageURLs 为空       → I2V 首帧（req_key=jimeng_i2v_first_v30）
//   - ImageURL + ImageURLs[0] 均非空      → I2V 首尾帧（req_key=jimeng_i2v_first_tail_v30）
//   - ImageURL 为空，len(ImageURLs) >= 2  → I2V 首尾帧（ImageURLs[0]=首帧，ImageURLs[1]=尾帧）
//
// 模式选择（Pro 3.0，req.Model == "jimeng_ti2v_v30_pro"）：
//   - 无图片  → T2V 文生视频（req_key=jimeng_ti2v_v30_pro）
//   - 有图片  → I2V 首帧（req_key=jimeng_ti2v_v30_pro，仅取第1张，不支持尾帧）
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
	// 运镜模式：必须有图片，参数与其他模式差异大，优先匹配
	case req.Model == jimengReqKeyRecamera:
		reqKey = jimengReqKeyRecamera
		prefix = jimengPrefixRecamera
		if len(allImages) > 1 {
			allImages = allImages[:1] // 运镜只支持单张图片
		}

	// 1080P 文生视频：强制纯 T2V，忽略图片输入
	case req.Model == jimengReqKeyT2V1080p:
		reqKey = jimengReqKeyT2V1080p
		prefix = jimengPrefixT2V1080p
		allImages = nil // 1080P 文生视频不接受图片

	// 1080P 图生视频（首帧）：仅取第1张图片
	case req.Model == jimengReqKeyI2V1080p:
		reqKey = jimengReqKeyI2V1080p
		prefix = jimengPrefixI2V1080p
		if len(allImages) > 1 {
			allImages = allImages[:1]
		}

	// 1080P 图生视频（首尾帧）：取前2张图片
	case req.Model == jimengReqKeyI2VTail1080p:
		reqKey = jimengReqKeyI2VTail1080p
		prefix = jimengPrefixI2VTail1080p
		if len(allImages) > 2 {
			allImages = allImages[:2]
		}

	// Pro 3.0：单一 req_key 处理 T2V 和 I2V 首帧，不支持尾帧
	case req.Model == jimengReqKeyPro:
		reqKey = jimengReqKeyPro
		prefix = jimengPrefixPro
		if len(allImages) > 1 {
			allImages = allImages[:1] // Pro 只支持首帧，多余图片截断
			logger.Printf("[jimeng-video] Pro 模式：忽略多余图片，仅保留首帧")
		}

	default:
		switch {
		case len(allImages) >= 2:
			allImages = allImages[:2]
			reqKey = jimengReqKeyI2VTail
			prefix = jimengPrefixI2VTail
		case len(allImages) == 1:
			reqKey = jimengReqKeyI2V
			prefix = jimengPrefixI2V
		default:
			reqKey = jimengReqKeyT2V
			prefix = jimengPrefixT2V
		}
	}

	// 运镜模式走独立参数构建路径
	if reqKey == jimengReqKeyRecamera {
		return p.submitRecameraTask(ctx, req, allImages, prefix)
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
		ar := req.AspectRatio
		if ar == "" {
			ar = "16:9"
		}
		params["aspect_ratio"] = ar
		logger.Printf("[jimeng-video] GenerateVideo: T2V aspect_ratio=%s", ar)
	}

	resp, _, err := p.svc.CVSync2AsyncSubmitTask(params)
	if err != nil {
		logger.Errorf("[jimeng-video] GenerateVideo: HTTP请求失败: %v", err)
		return nil, fmt.Errorf("jimeng-video: 提交任务失败: %w", err)
	}

	code, _ := resp["code"].(float64)
	msg, _ := resp["message"].(string)
	if int(code) != 10000 {
		logger.Errorf("[jimeng-video] GenerateVideo: API返回错误 code=%d message=%s", int(code), msg)
		apiErr := fmt.Errorf("jimeng-video: 提交任务失败 code=%d: %s", int(code), msg)
		if int(code) == 50430 {
			return nil, fmt.Errorf("%w: %w", ErrVideoConcurrentLimit, apiErr)
		}
		return nil, apiErr
	}

	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("jimeng-video: 响应缺少 data 字段")
	}
	rawTaskID, _ := data["task_id"].(string)
	if rawTaskID == "" {
		logger.Errorf("[jimeng-video] GenerateVideo: API成功但未返回task_id")
		return nil, fmt.Errorf("jimeng-video: 未返回 task_id")
	}

	taskID := prefix + rawTaskID
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

	params := map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	}

	resp, _, err := p.svc.CVSync2AsyncGetResult(params)
	if err != nil {
		logger.Errorf("[jimeng-video] GetVideoStatus: HTTP请求失败 taskID=%s: %v", rawID, err)
		return nil, fmt.Errorf("jimeng-video: 查询状态失败: %w", err)
	}

	code, _ := resp["code"].(float64)
	msg, _ := resp["message"].(string)
	if int(code) != 10000 {
		logger.Errorf("[jimeng-video] GetVideoStatus: API错误 taskID=%s code=%d message=%s", rawID, int(code), msg)
		return &VideoTaskStatus{
			TaskID: rawID,
			Status: "failed",
			Error:  fmt.Sprintf("code=%d: %s", int(code), msg),
		}, nil
	}

	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		logger.Errorf("[jimeng-video] GetVideoStatus: data为null taskID=%s", rawID)
		return &VideoTaskStatus{TaskID: rawID, Status: "failed", Error: "data is null"}, nil
	}

	rawStatus, _ := data["status"].(string)
	status := jimengMapStatus(rawStatus)
	logger.Printf("[jimeng-video] GetVideoStatus: taskID=%s rawStatus=%s mappedStatus=%s", rawID, rawStatus, status)

	ts := &VideoTaskStatus{TaskID: rawID, Status: status}
	switch rawStatus {
	case "in_queue":
		ts.Progress = 5
	case "generating":
		ts.Progress = 50
	case "done":
		ts.Progress = 100
	case "not_found", "expired":
		ts.Error = rawStatus
		logger.Errorf("[jimeng-video] GetVideoStatus: 任务异常 taskID=%s status=%s", rawID, rawStatus)
	}
	return ts, nil
}

// GetVideoURL 获取已完成任务的视频URL
func (p *JimengVideoProvider) GetVideoURL(ctx context.Context, taskID string) (string, error) {
	reqKey, rawID := jimengParseTaskID(taskID)

	logger.Printf("[jimeng-video] GetVideoURL: taskID=%s reqKey=%s", rawID, reqKey)

	params := map[string]interface{}{
		"req_key": reqKey,
		"task_id": rawID,
	}

	resp, _, err := p.svc.CVSync2AsyncGetResult(params)
	if err != nil {
		logger.Errorf("[jimeng-video] GetVideoURL: HTTP请求失败 taskID=%s: %v", rawID, err)
		return "", fmt.Errorf("jimeng-video: 获取视频URL失败: %w", err)
	}

	code, _ := resp["code"].(float64)
	msg, _ := resp["message"].(string)
	if int(code) != 10000 {
		logger.Errorf("[jimeng-video] GetVideoURL: API错误 taskID=%s code=%d message=%s", rawID, int(code), msg)
		return "", fmt.Errorf("jimeng-video: 获取结果失败 code=%d: %s", int(code), msg)
	}

	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		logger.Errorf("[jimeng-video] GetVideoURL: data为null taskID=%s", rawID)
		return "", fmt.Errorf("jimeng-video: data is null")
	}

	rawStatus, _ := data["status"].(string)
	if rawStatus != "done" {
		logger.Errorf("[jimeng-video] GetVideoURL: 任务未完成 taskID=%s status=%s", rawID, rawStatus)
		return "", fmt.Errorf("jimeng-video: 任务未完成，status=%s", rawStatus)
	}

	videoURL, _ := data["video_url"].(string)
	if videoURL == "" {
		logger.Errorf("[jimeng-video] GetVideoURL: done但video_url为空 taskID=%s", rawID)
		return "", fmt.Errorf("jimeng-video: 任务完成但未返回 video_url")
	}
	logger.Printf("[jimeng-video] GetVideoURL: 成功获取URL taskID=%s url=%s", rawID, videoURL)
	return videoURL, nil
}

// submitRecameraTask 提交运镜（Recamera）视频任务
//
// 必填：单张图片（image_url 或 binary_data_base64[0]）、template_id
// 选填：camera_strength（weak/medium/strong，默认 medium）、prompt
func (p *JimengVideoProvider) submitRecameraTask(ctx context.Context, req *VideoGenerateRequest, images []string, prefix string) (*VideoTask, error) {
	templateID := req.CameraMovement
	if templateID == "" {
		return nil, fmt.Errorf("jimeng-video: 运镜模式需要指定 template_id（CameraMovement 字段）")
	}

	cameraStrength := req.Mode
	if cameraStrength == "" {
		cameraStrength = "medium"
	}

	params := map[string]interface{}{
		"req_key":         jimengReqKeyRecamera,
		"prompt":          req.Prompt,
		"seed":            -1,
		"template_id":     templateID,
		"camera_strength": cameraStrength,
	}

	if len(images) > 0 {
		img := images[0]
		switch {
		case strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://"):
			params["image_url"] = img
		case strings.HasPrefix(img, "file://"):
			filePath := strings.TrimPrefix(img, "file://")
			if data, err := os.ReadFile(filePath); err == nil && len(data) > 0 {
				params["binary_data_base64"] = []string{base64.StdEncoding.EncodeToString(data)}
			} else {
				logger.Errorf("[jimeng-video] recamera: cannot read local file %q: %v", filePath, err)
				return nil, fmt.Errorf("jimeng-video: 读取图片文件失败: %w", err)
			}
		default:
			logger.Errorf("[jimeng-video] recamera: skipping non-public image URL %q", img)
			return nil, fmt.Errorf("jimeng-video: 运镜模式需要公网可访问的图片URL")
		}
	} else {
		return nil, fmt.Errorf("jimeng-video: 运镜模式需要提供图片")
	}

	logger.Printf("[jimeng-video] submitRecameraTask: template_id=%s camera_strength=%s promptLen=%d",
		templateID, cameraStrength, len(req.Prompt))

	resp, _, err := p.svc.CVSync2AsyncSubmitTask(params)
	if err != nil {
		logger.Errorf("[jimeng-video] submitRecameraTask: HTTP请求失败: %v", err)
		return nil, fmt.Errorf("jimeng-video: 提交运镜任务失败: %w", err)
	}

	code, _ := resp["code"].(float64)
	msg, _ := resp["message"].(string)
	if int(code) != 10000 {
		logger.Errorf("[jimeng-video] submitRecameraTask: API错误 code=%d message=%s", int(code), msg)
		return nil, fmt.Errorf("jimeng-video: 提交运镜任务失败 code=%d: %s", int(code), msg)
	}

	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("jimeng-video: 响应缺少 data 字段")
	}
	rawTaskID, _ := data["task_id"].(string)
	if rawTaskID == "" {
		return nil, fmt.Errorf("jimeng-video: 未返回 task_id")
	}

	taskID := prefix + rawTaskID
	logger.Printf("[jimeng-video] submitRecameraTask: 任务提交成功 taskID=%s", taskID)
	return &VideoTask{
		TaskID:   taskID,
		Status:   "pending",
		Provider: p.GetName(),
	}, nil
}

// jimengParseTaskID 解析带前缀的 taskID，返回 req_key 和原始 task_id
func jimengParseTaskID(taskID string) (reqKey, rawID string) {
	switch {
	case strings.HasPrefix(taskID, jimengPrefixRecamera):
		return jimengReqKeyRecamera, taskID[len(jimengPrefixRecamera):]
	case strings.HasPrefix(taskID, jimengPrefixT2V1080p):
		return jimengReqKeyT2V1080p, taskID[len(jimengPrefixT2V1080p):]
	case strings.HasPrefix(taskID, jimengPrefixI2V1080p):
		return jimengReqKeyI2V1080p, taskID[len(jimengPrefixI2V1080p):]
	case strings.HasPrefix(taskID, jimengPrefixPro):
		return jimengReqKeyPro, taskID[len(jimengPrefixPro):]
	case strings.HasPrefix(taskID, jimengPrefixI2VTail1080p):
		return jimengReqKeyI2VTail1080p, taskID[len(jimengPrefixI2VTail1080p):]
	case strings.HasPrefix(taskID, jimengPrefixI2VTail):
		return jimengReqKeyI2VTail, taskID[len(jimengPrefixI2VTail):]
	case strings.HasPrefix(taskID, jimengPrefixI2V):
		return jimengReqKeyI2V, taskID[len(jimengPrefixI2V):]
	default:
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
