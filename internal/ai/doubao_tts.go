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
	"time"
)

// DoubaoSpeechProvider 豆包语音合成大模型提供者（Volcengine 原生 TTS API）
// 区别于 DoubaoProvider 使用的 OpenAI 兼容 /audio/speech 接口，
// 本提供者直接调用 openspeech.bytedance.com 接口，支持更多发音人和情感参数。
// 官方文档：https://www.volcengine.com/docs/6561/1257544
type DoubaoSpeechProvider struct {
	apiKey     string
	resourceID string // X-Api-Resource-Id，推理接入点 ID（如 "seed-tts-2.0"）
	client     *http.Client
}

const doubaoSpeechTTSEndpoint = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
const doubaoSpeechDefaultResourceID = "seed-tts-2.0"

// doubaoSpeechFinalCode 最终分块标识码（合成完成）
const doubaoSpeechFinalCode = 20000000

// doubaoSpeechChunk TTS 分块响应结构
type doubaoSpeechChunk struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"` // base64 编码的 MP3 音频分块
}

// NewDoubaoSpeechProvider 创建豆包语音合成提供者
// apiKey: 新版控制台 API Key（作为 X-Api-Key 请求头）
// resourceID: 推理接入点，为空时默认 "seed-tts-2.0"（也支持 "seed-tts-1.0"）
func NewDoubaoSpeechProvider(apiKey, resourceID string) *DoubaoSpeechProvider {
	if resourceID == "" {
		resourceID = doubaoSpeechDefaultResourceID
	}
	return &DoubaoSpeechProvider{
		apiKey:     apiKey,
		resourceID: resourceID,
		client:     &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *DoubaoSpeechProvider) GetName() string { return "doubao-speech" }

func (p *DoubaoSpeechProvider) GetModels() []string {
	return []string{"seed-tts-2.0", "seed-tts-1.0"}
}

func (p *DoubaoSpeechProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("doubao-speech: API key not configured")
	}
	return nil
}

func (p *DoubaoSpeechProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("doubao-speech: text generation not supported")
}

func (p *DoubaoSpeechProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("doubao-speech: streaming not supported")
}

func (p *DoubaoSpeechProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("doubao-speech: embeddings not supported")
}

func (p *DoubaoSpeechProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("doubao-speech: image generation not supported")
}

// AudioGenerate 调用豆包语音合成 API，返回 MP3 音频文件路径。
//
// req.Voice:   发音人 ID（如 "zh_female_shuangkuaisisi_moon_bigtts"），留空使用默认发音人
// req.Speed:   语速倍率（0.5~2.0，映射到 speech_rate [-50,100]，1.0 为正常）
// req.Emotion: 情感标签（如 happy/sad/angry 等）
// req.Model:   覆盖推理接入点（如 "seed-tts-1.0"），留空使用 resourceID
func (p *DoubaoSpeechProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	speaker := req.Voice
	if speaker == "" {
		speaker = "zh_female_shuangkuaisisi_moon_bigtts"
	}

	reqParams := map[string]interface{}{
		"text":    req.Text,
		"speaker": speaker,
		"audio_params": map[string]interface{}{
			"format":      "mp3",
			"sample_rate": 24000,
		},
	}

	// speech_rate: [-50, 100]，从 Speed 字段线性映射（1.0=正常=0，2.0=+100，0.5=-50）
	if req.Speed > 0 && req.Speed != 1.0 {
		speechRate := int((req.Speed - 1.0) * 100)
		if speechRate < -50 {
			speechRate = -50
		} else if speechRate > 100 {
			speechRate = 100
		}
		reqParams["speech_rate"] = speechRate
	}
	if req.Emotion != "" {
		reqParams["emotion"] = req.Emotion
	}

	ttsBody := map[string]interface{}{
		"user":       map[string]string{"uid": "inkframe"},
		"req_params": reqParams,
	}

	body, err := json.Marshal(ttsBody)
	if err != nil {
		return nil, fmt.Errorf("doubao-speech: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", doubaoSpeechTTSEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", p.apiKey)
	resourceID := p.resourceID
	if req.Model != "" {
		resourceID = req.Model
	}
	httpReq.Header.Set("X-Api-Resource-Id", resourceID)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("doubao-speech: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("doubao-speech: API error %d: %s", resp.StatusCode, string(errBody))
	}

	// 读取分块 JSON 响应：每行一个 JSON 对象，data 字段为 base64 编码的 MP3 分块
	// 最终分块的 code 为 20000000
	var audioData []byte
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB 缓冲，避免大分块截断
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk doubaoSpeechChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Code != 0 && chunk.Code != doubaoSpeechFinalCode {
			return nil, fmt.Errorf("doubao-speech: error in chunk (code=%d): %s", chunk.Code, chunk.Message)
		}
		if chunk.Data != "" {
			decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return nil, fmt.Errorf("doubao-speech: base64 decode: %w", err)
			}
			audioData = append(audioData, decoded...)
		}
		if chunk.Code == doubaoSpeechFinalCode {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("doubao-speech: read response: %w", err)
	}
	if len(audioData) == 0 {
		return nil, fmt.Errorf("doubao-speech: no audio data received")
	}

	// 写入临时文件（后续可由调用方通过 storage 上传至 OSS）
	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("doubao-speech: write temp file: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0, // 粗略估算：约 8 字/秒
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}
