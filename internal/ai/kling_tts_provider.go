package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// KlingTTSProvider 可灵语音合成提供者
// 官方文档：https://klingai.com/document-api/apiReference/model/TTS
//
// 该接口为异步任务：
//  1. POST /v1/audio/tts        — 提交任务，返回 task_id
//  2. GET  /v1/audio/tts/{task_id} — 轮询，直到 task_status=succeed
//
// 与 SFX 的区别：
//   - 输入为需要朗读的文字（最多 1000 字符），而非音效描述
//   - 需指定 voice_id（音色）、voice_language（zh/en）、voice_speed（0.8~2.0）
//   - 返回音频 URL 字段为 url（而非 url_mp3）
type KlingTTSProvider struct {
	accessKey string
	secretKey string
	endpoint  string
	client    *http.Client
}

const (
	klingTTSDefaultEndpoint  = "https://api-beijing.klingai.com"
	klingTTSPollInterval     = 2 * time.Second
	klingTTSMaxWait          = 5 * time.Minute
	klingTTSDefaultVoiceID   = "zh_female_story"
	klingTTSDefaultLanguage  = "zh"
	klingTTSMinSpeed         = 0.8
	klingTTSMaxSpeed         = 2.0
	klingTTSDefaultSpeed     = 1.0
)

// klingTTSTaskResult 查询结果中的 task_result
type klingTTSTaskResult struct {
	Audios []struct {
		ID       string `json:"id"`
		URL      string `json:"url"`
		Duration string `json:"duration"` // 秒（字符串）
	} `json:"audios"`
}

// klingTTSQueryResponse 查询任务接口响应
type klingTTSQueryResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		TaskID        string             `json:"task_id"`
		TaskStatus    string             `json:"task_status"`
		TaskStatusMsg string             `json:"task_status_msg"`
		TaskResult    klingTTSTaskResult `json:"task_result"`
	} `json:"data"`
}

// NewKlingTTSProvider 创建可灵语音合成提供者
// accessKey / secretKey: 可灵 AK/SK（与视频生成共用同一对密钥）
// endpoint: API 端点，留空使用默认 https://api-beijing.klingai.com
func NewKlingTTSProvider(accessKey, secretKey, endpoint string) *KlingTTSProvider {
	return &KlingTTSProvider{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  normalizeKlingEndpoint(endpoint, klingTTSDefaultEndpoint),
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *KlingTTSProvider) GetName() string { return "kling-tts" }

// GetModels 返回可用的音色 ID 列表
func (p *KlingTTSProvider) GetModels() []string {
	return []string{
		// 中文女声
		"zh_female_story", "zh_female_qingxin", "zh_female_tianmei",
		"zh_female_wenrou", "zh_female_zhishixing",
		// 中文男声
		"zh_male_story", "zh_male_zhengpai", "zh_male_xinwen",
		"zh_male_shuhu", "zh_male_qingnian",
		// 英文
		"oversea_male1", "oversea_female1",
	}
}

func (p *KlingTTSProvider) HealthCheck(ctx context.Context) error {
	if p.accessKey == "" || p.secretKey == "" {
		return fmt.Errorf("kling-tts: Access Key and Secret Key not configured")
	}
	return nil
}

func (p *KlingTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("kling-tts: text generation not supported")
}

func (p *KlingTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("kling-tts: streaming not supported")
}

func (p *KlingTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("kling-tts: embeddings not supported")
}

func (p *KlingTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("kling-tts: image generation not supported")
}

// AudioGenerate 提交语音合成任务并同步等待完成，返回音频 URL。
//
// req.Text:     待合成文本（最多 1000 字符）
// req.Voice:    音色 ID（如 "zh_female_story"），留空使用默认
// req.Language: 语言（"zh" 或 "en"），留空默认 "zh"
// req.Speed:    语速（0.8~2.0），留空/0 默认 1.0
func (p *KlingTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	if req.Text == "" {
		return nil, fmt.Errorf("kling-tts: text is required")
	}

	// 音色
	voiceID := req.Voice
	if voiceID == "" {
		return nil, fmt.Errorf("kling-tts: 未指定音色，请先在小说设置或角色配置中选择音色")
	}

	// 语言
	lang := req.Language
	if lang == "" {
		lang = klingTTSDefaultLanguage
	}

	// 语速
	speed := req.Speed
	if speed <= 0 {
		speed = klingTTSDefaultSpeed
	}
	if speed < klingTTSMinSpeed {
		speed = klingTTSMinSpeed
	} else if speed > klingTTSMaxSpeed {
		speed = klingTTSMaxSpeed
	}

	// Step 1: 提交任务
	taskID, err := p.submitTask(ctx, req.Text, voiceID, lang, speed)
	if err != nil {
		return nil, fmt.Errorf("kling-tts: submit task: %w", err)
	}

	// Step 2: 轮询直到完成（最多等待 klingTTSMaxWait）
	pollCtx, cancel := context.WithTimeout(ctx, klingTTSMaxWait)
	defer cancel()

	audioURL, duration, err := p.pollUntilDone(pollCtx, taskID)
	if err != nil {
		return nil, fmt.Errorf("kling-tts: poll task %s: %w", taskID, err)
	}

	return &AudioResponse{
		URL:       audioURL,
		Format:    "mp3",
		Duration:  duration,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// submitTask 提交语音合成任务，返回 task_id
func (p *KlingTTSProvider) submitTask(ctx context.Context, text, voiceID, lang string, speed float64) (string, error) {
	body := map[string]interface{}{
		"text":           text,
		"voice_id":       voiceID,
		"voice_language": lang,
		"voice_speed":    speed,
	}

	respBody, status, err := p.doRequest(ctx, "POST", "/v1/audio/tts", body)
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

// pollUntilDone 轮询任务直到 succeed，返回音频 URL 和时长
func (p *KlingTTSProvider) pollUntilDone(ctx context.Context, taskID string) (string, float64, error) {
	ticker := time.NewTicker(klingTTSPollInterval)
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
				if audio.URL == "" {
					return "", 0, fmt.Errorf("task succeeded but url is empty")
				}
				dur := 0.0
				if audio.Duration != "" {
					dur, _ = strconv.ParseFloat(audio.Duration, 64)
				}
				return audio.URL, dur, nil
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
func (p *KlingTTSProvider) queryTask(ctx context.Context, taskID string) (*klingTTSQueryResponse, error) {
	path := fmt.Sprintf("/v1/audio/tts/%s", taskID)
	respBody, status, err := p.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, string(respBody[:min(200, len(respBody))]))
	}

	var result klingTTSQueryResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse query response: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("API error code=%d: %s", result.Code, result.Message)
	}
	return &result, nil
}

func (p *KlingTTSProvider) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	return klingDoRequest(ctx, p.accessKey, p.secretKey, p.endpoint, p.client, method, path, body)
}

// Ensure interface compliance.
var _ AIProvider = (*KlingTTSProvider)(nil)
