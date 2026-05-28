package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// KlingSFXProvider 可灵文生音效提供者
// 官方文档：https://klingai.com/document-api/apiReference/model/textToAudio
//
// 该接口为异步任务：
//  1. POST /v1/audio/text-to-audio  — 提交任务，返回 task_id
//  2. GET  /v1/audio/text-to-audio/{task_id} — 轮询，直到 task_status=succeed
//
// 与 TTS 的区别：
//   - 输入为音效描述（如"春节烟花声""雨打树叶声"），而非需要朗读的文字
//   - 输出为生成的音效文件 URL（mp3/wav），不本地化存储
//   - req.Duration 控制音效时长（3.0~10.0 秒），默认 5.0 秒
type KlingSFXProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

const (
	klingSFXDefaultEndpoint = "https://api-beijing.klingai.com"
	klingSFXPollInterval    = 2 * time.Second
	klingSFXMaxWait         = 5 * time.Minute
	klingSFXMinDuration     = 3.0
	klingSFXMaxDuration     = 10.0
	klingSFXDefaultDuration = 5.0
)

// klingSFXTaskResult 查询结果中的 task_result
type klingSFXTaskResult struct {
	Audios []struct {
		ID          string `json:"id"`
		URLMp3      string `json:"url_mp3"`
		URLWav      string `json:"url_wav"`
		DurationMp3 string `json:"duration_mp3"`
	} `json:"audios"`
}

// klingSFXQueryResponse 查询任务接口响应
type klingSFXQueryResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		TaskID        string             `json:"task_id"`
		TaskStatus    string             `json:"task_status"`
		TaskStatusMsg string             `json:"task_status_msg"`
		TaskResult    klingSFXTaskResult `json:"task_result"`
	} `json:"data"`
}

// NewKlingSFXProvider 创建可灵文生音效提供者
// accessKey / secretKey: 可灵 AK/SK（与视频生成共用同一对密钥）
// endpoint: API 端点，留空使用默认 https://api-beijing.klingai.com
func NewKlingSFXProvider(accessKey, secretKey, endpoint string) *KlingSFXProvider {
	return &KlingSFXProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, klingSFXDefaultEndpoint),
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *KlingSFXProvider) GetName() string { return "kling-sfx" }

func (p *KlingSFXProvider) GetModels() []string {
	// Kling 文生音效不区分模型，以时长作为"配置"
	return []string{"3s", "5s", "7s", "10s"}
}

func (p *KlingSFXProvider) HealthCheck(ctx context.Context) error {
	if p.accessKey == "" || p.secretKey == "" {
		return fmt.Errorf("kling-sfx: Access Key and Secret Key not configured")
	}
	return nil
}

func (p *KlingSFXProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("kling-sfx: text generation not supported")
}

func (p *KlingSFXProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("kling-sfx: streaming not supported")
}

func (p *KlingSFXProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("kling-sfx: embeddings not supported")
}

func (p *KlingSFXProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("kling-sfx: image generation not supported")
}

// AudioGenerate 提交文生音效任务并同步等待完成，返回 MP3 URL。
//
// req.Text:     音效描述提示词（最多 200 字符），如 "春节烟花声"
// req.Duration: 音效时长（3.0~10.0 秒），留空/0 默认 5.0 秒
// req.Model:    可选，格式 "5s"/"5.0" 等表示时长，优先级低于 req.Duration
func (p *KlingSFXProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	if req.Text == "" {
		return nil, fmt.Errorf("kling-sfx: prompt (Text) is required")
	}

	// 解析时长：优先 req.Duration，其次从 req.Model 解析（"5s" / "5.0"）
	duration := req.Duration
	if duration <= 0 && req.Model != "" {
		s := req.Model
		if len(s) > 0 && s[len(s)-1] == 's' {
			s = s[:len(s)-1]
		}
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			duration = v
		}
	}
	if duration <= 0 {
		duration = klingSFXDefaultDuration
	}
	if duration < klingSFXMinDuration {
		duration = klingSFXMinDuration
	} else if duration > klingSFXMaxDuration {
		duration = klingSFXMaxDuration
	}

	// Step 1: 提交任务
	taskID, err := p.submitTask(ctx, req.Text, duration)
	if err != nil {
		return nil, fmt.Errorf("kling-sfx: submit task: %w", err)
	}

	// Step 2: 轮询直到完成（最多等待 klingSFXMaxWait）
	pollCtx, cancel := context.WithTimeout(ctx, klingSFXMaxWait)
	defer cancel()

	mp3URL, actualDuration, err := p.pollUntilDone(pollCtx, taskID)
	if err != nil {
		return nil, fmt.Errorf("kling-sfx: poll task %s: %w", taskID, err)
	}

	return &AudioResponse{
		URL:       mp3URL,
		Format:    "mp3",
		Duration:  actualDuration,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// submitTask 提交文生音效任务，返回 task_id
func (p *KlingSFXProvider) submitTask(ctx context.Context, prompt string, duration float64) (string, error) {
	body := map[string]interface{}{
		"prompt":   prompt,
		"duration": duration,
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/v1/audio/text-to-audio", body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != 202 {
		return "", fmt.Errorf("HTTP %d: %s", status, string(respBody[:min(200, len(respBody))]))
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("API error code=%d: %s", result.Code, result.Message)
	}
	if result.Data.TaskID == "" {
		return "", fmt.Errorf("empty task_id in response")
	}
	return result.Data.TaskID, nil
}

// pollUntilDone 轮询任务直到 succeed，返回 mp3 URL 和时长
func (p *KlingSFXProvider) pollUntilDone(ctx context.Context, taskID string) (string, float64, error) {
	ticker := time.NewTicker(klingSFXPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-ticker.C:
			status, err := p.queryTask(ctx, taskID)
			if err != nil {
				return "", 0, err
			}
			switch status.Data.TaskStatus {
			case "succeed", "success":
				audios := status.Data.TaskResult.Audios
				if len(audios) == 0 {
					return "", 0, fmt.Errorf("task succeeded but no audio in result")
				}
				audio := audios[0]
				if audio.URLMp3 == "" {
					return "", 0, fmt.Errorf("task succeeded but url_mp3 is empty")
				}
				dur := 0.0
				if audio.DurationMp3 != "" {
					dur, _ = strconv.ParseFloat(audio.DurationMp3, 64)
				}
				return audio.URLMp3, dur, nil
			case "failed":
				msg := status.Data.TaskStatusMsg
				if msg == "" {
					msg = "unknown error"
				}
				return "", 0, fmt.Errorf("task failed: %s", msg)
			case "submitted", "processing":
				// 继续轮询
			default:
				// 未知状态，继续等待
			}
		}
	}
}

// queryTask 查询任务状态
func (p *KlingSFXProvider) queryTask(ctx context.Context, taskID string) (*klingSFXQueryResponse, error) {
	path := fmt.Sprintf("/v1/audio/text-to-audio/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, string(respBody[:min(200, len(respBody))]))
	}

	var result klingSFXQueryResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse query response: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("API error code=%d: %s", result.Code, result.Message)
	}
	return &result, nil
}

// doRequest 发送 HTTP 请求，返回响应体、状态码
func (p *KlingSFXProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	kp := &KlingProvider{
		accessKey: p.accessKey,
		secretKey: p.secretKey,
		endpoint:  p.endpoint,
		client:    p.client,
	}
	return kp.doRequest(ctx, method, path, body)
}

// Ensure interface compliance.
var _ AIProvider = (*KlingSFXProvider)(nil)
