package ai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AliyunTTSProvider 阿里云 CosyVoice 语音合成提供者（DashScope）
// 官方文档：https://help.aliyun.com/zh/dashscope/developer-reference/cosyvoice-api
//
// 音色列表（voice）：
//
//	longxiaochun      龙小淳（女声，知性温暖）
//	longxiaoxia       龙晓夏（女声，活泼朝气）
//	longxiaobai       龙小白（男声，年轻开朗）
//	longfei           龙飞（男声，沉稳自信）
//	longjielidou      龙姐励豆（女声，温暖励志）
//	longmiaomiao      龙淼淼（女声，儿童活泼）
//	longshu           龙叔（男声，叙述沉稳）
//	longwan           龙婉（女声，甜美温柔）
//	longcheng         龙橙（男声，清晰专业）
//	longhua           龙华（男声，成熟稳重）
//	longxiang         龙祥（男声，磁性低沉）
//	loongbella        贝拉（英文女声）
//	loongbobby        鲍比（英文男声）
type AliyunTTSProvider struct {
	apiKey string // DashScope API Key
	client *http.Client
}

const (
	aliyunTTSEndpoint    = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text2audio/generation"
	aliyunTTSDefaultModel = "cosyvoice-v1"
)

// aliyunTTSRequest DashScope CosyVoice 请求体
type aliyunTTSRequest struct {
	Model string             `json:"model"`
	Input aliyunTTSInput     `json:"input"`
	Param aliyunTTSParam     `json:"parameters"`
}

type aliyunTTSInput struct {
	Text  string `json:"text"`
	Voice string `json:"voice"`
}

type aliyunTTSParam struct {
	Volume     int    `json:"volume,omitempty"`      // 0~100，默认 50
	SpeechRate int    `json:"speech_rate,omitempty"` // -500~500，0 为正常
	PitchRate  int    `json:"pitch_rate,omitempty"`  // -500~500，0 为正常
	Emotion    string `json:"emotion,omitempty"`     // happy/sad/angry/fear/neutral（cosyvoice-v2 支持）
}

// aliyunSSEChunk SSE 事件数据
type aliyunSSEChunk struct {
	Output struct {
		Audio        string `json:"audio"`         // base64 编码的 PCM/WAV/MP3 分块
		FinishReason string `json:"finish_reason"` // "" / "stop"
	} `json:"output"`
	Usage struct {
		Characters int `json:"characters"`
		Duration   int `json:"duration_ms"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// NewAliyunTTSProvider 创建阿里云 CosyVoice 语音合成提供者
// apiKey: DashScope API Key（控制台获取）
func NewAliyunTTSProvider(apiKey string) *AliyunTTSProvider {
	return &AliyunTTSProvider{
		apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *AliyunTTSProvider) GetName() string { return "aliyun-tts" }

func (p *AliyunTTSProvider) GetModels() []string {
	return []string{
		"longxiaochun",
		"longxiaoxia",
		"longxiaobai",
		"longfei",
		"longjielidou",
		"longmiaomiao",
		"longshu",
		"longwan",
		"longcheng",
		"longhua",
		"longxiang",
		"loongbella",
		"loongbobby",
	}
}

func (p *AliyunTTSProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("aliyun-tts: api_key not configured")
	}
	return nil
}

func (p *AliyunTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: text generation not supported")
}

func (p *AliyunTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: streaming not supported")
}

func (p *AliyunTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("aliyun-tts: embeddings not supported")
}

func (p *AliyunTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("aliyun-tts: image generation not supported")
}

// AudioGenerate 调用阿里云 CosyVoice 语音合成，返回 MP3 文件路径。
//
// req.Voice:   音色名称（见音色列表），留空默认 "longxiaochun"
// req.Speed:   语速（0.5~2.0，1.0=正常，映射到 speech_rate -500~500）
// req.Pitch:   音调（0.5~1.5，1.0=正常，映射到 pitch_rate -500~500）
// req.Model:   覆盖模型名（如 "cosyvoice-v2"），留空使用 cosyvoice-v1
// req.Emotion: 情感标签（happy/sad/angry/fear/neutral），cosyvoice-v2 支持，v1 静默忽略
func (p *AliyunTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voice := req.Voice
	if voice == "" {
		voice = "longxiaochun"
	}

	model := req.Model
	if model == "" {
		model = aliyunTTSDefaultModel
	}

	speechRate := 0
	if req.Speed > 0 && req.Speed != 1.0 {
		// 线性映射：Speed 1.0 → 0; 2.0 → 500; 0.5 → -500
		speechRate = int((req.Speed - 1.0) * 500.0)
		if speechRate < -500 {
			speechRate = -500
		} else if speechRate > 500 {
			speechRate = 500
		}
	}

	pitchRate := 0
	if req.Pitch > 0 && req.Pitch != 1.0 {
		pitchRate = int((req.Pitch - 1.0) * 500.0)
		if pitchRate < -500 {
			pitchRate = -500
		} else if pitchRate > 500 {
			pitchRate = 500
		}
	}

	ttsReq := aliyunTTSRequest{
		Model: model,
		Input: aliyunTTSInput{
			Text:  req.Text,
			Voice: voice,
		},
		Param: aliyunTTSParam{
			Volume:     50,
			SpeechRate: speechRate,
			PitchRate:  pitchRate,
			Emotion:    req.Emotion,
		},
	}

	bodyBytes, err := json.Marshal(ttsReq)
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", aliyunTTSEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	// 请求 SSE 流式返回以获取音频数据
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-DashScope-SSE", "enable")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("aliyun-tts: HTTP %d: %s", resp.StatusCode, string(errBody[:min(200, len(errBody))]))
	}

	audioData, err := parseAliyunSSE(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aliyun-tts: parse response: %w", err)
	}
	if len(audioData) == 0 {
		return nil, fmt.Errorf("aliyun-tts: no audio data received")
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("aliyun-tts: write temp file: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// parseAliyunSSE 解析 DashScope SSE 流，拼接 base64 音频数据
func parseAliyunSSE(body io.Reader) ([]byte, error) {
	var audioData []byte
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk aliyunSSEChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Code != "" && chunk.Code != "200" {
			return nil, fmt.Errorf("error %s: %s", chunk.Code, chunk.Message)
		}
		if chunk.Output.Audio != "" {
			decoded, err := base64.StdEncoding.DecodeString(chunk.Output.Audio)
			if err != nil {
				// 可能是 hex 或 URL，尝试 URL-safe base64
				decoded, err = base64.RawURLEncoding.DecodeString(chunk.Output.Audio)
				if err != nil {
					continue
				}
			}
			audioData = append(audioData, decoded...)
		}
		if chunk.Output.FinishReason == "stop" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return audioData, nil
}

// Ensure interface compliance.
var _ AIProvider = (*AliyunTTSProvider)(nil)
