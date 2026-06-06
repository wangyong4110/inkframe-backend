package ai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// MinimaxTTSProvider MiniMax 语音合成提供者
// 官方文档：https://platform.minimaxi.com/document/T2A%20V2
//
// 音色列表（voice_id）：
//
//	female-shaonv         少女音（年轻女性，活泼）
//	female-yujie          御姐音（成熟女性，知性）
//	female-tianmei        甜美音（女性，甜蜜）
//	female-qingxin        清新音（女性，清新）
//	male-qn-qingse        青涩青年音（男性，年轻）
//	male-qn-jingying      精英青年音（男性，知性）
//	male-qn-badao         霸道青年音（男性，有力）
//	male-qn-daxuesheng    大学生音（男性，活力）
//	presenter_male        男主持（专业）
//	presenter_female      女主持（专业）
//	audiobook_male_1      有声书男声1
//	audiobook_male_2      有声书男声2
//	audiobook_female_1    有声书女声1
//	audiobook_female_2    有声书女声2
//	male-story            故事男声（儿童）
//	female-story          故事女声（儿童）
type MinimaxTTSProvider struct {
	apiKey  string // MiniMax API Key
	groupID string // MiniMax Group ID
	client  *http.Client
}

const minimaxTTSEndpoint = "https://api.minimax.chat/v1/t2a_v2"
const minimaxTTSDefaultModel = "speech-01-turbo"

// minimaxTTSRequest T2A V2 请求体
type minimaxTTSRequest struct {
	Model        string              `json:"model"`
	Text         string              `json:"text"`
	Stream       bool                `json:"stream"`
	VoiceSetting minimaxVoiceSetting `json:"voice_setting"`
	AudioSetting minimaxAudioSetting `json:"audio_setting"`
}

type minimaxVoiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float64 `json:"speed"`            // 0.5~2.0
	Pitch   int     `json:"pitch"`            // -12~12 半音
	Vol     float64 `json:"vol"`              // 0.1~10.0
	Emotion string  `json:"emotion,omitempty"` // happy/sad/angry/fearful/surprised/neutral
}

type minimaxAudioSetting struct {
	SampleRate int    `json:"sample_rate"` // 32000
	Bitrate    int    `json:"bitrate"`     // 128000
	Format     string `json:"format"`      // "mp3"
}

// minimaxSSEEvent SSE 事件中的数据
type minimaxSSEEvent struct {
	Data struct {
		Audio  string `json:"audio"`  // hex 编码音频块
		Status int    `json:"status"` // 1=进行中, 2=完成
	} `json:"data"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// NewMinimaxTTSProvider 创建 MiniMax 语音合成提供者
// apiKey: 平台 API Key; groupID: 平台 Group ID（可在控制台获取）
func NewMinimaxTTSProvider(apiKey, groupID string) *MinimaxTTSProvider {
	return &MinimaxTTSProvider{
		apiKey:  apiKey,
		groupID: groupID,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *MinimaxTTSProvider) GetName() string { return "minimax-tts" }

func (p *MinimaxTTSProvider) GetModels() []string {
	return []string{
		"female-shaonv",
		"female-yujie",
		"female-tianmei",
		"female-qingxin",
		"male-qn-qingse",
		"male-qn-jingying",
		"male-qn-badao",
		"male-qn-daxuesheng",
		"presenter_male",
		"presenter_female",
		"audiobook_male_1",
		"audiobook_male_2",
		"audiobook_female_1",
		"audiobook_female_2",
		"male-story",
		"female-story",
	}
}

func (p *MinimaxTTSProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" {
		return fmt.Errorf("minimax-tts: api_key not configured")
	}
	if p.groupID == "" {
		return fmt.Errorf("minimax-tts: group_id not configured")
	}
	return nil
}

func (p *MinimaxTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("minimax-tts: text generation not supported")
}

func (p *MinimaxTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("minimax-tts: streaming not supported")
}

func (p *MinimaxTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("minimax-tts: embeddings not supported")
}

func (p *MinimaxTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("minimax-tts: image generation not supported")
}

// AudioGenerate 调用 MiniMax T2A V2 语音合成，返回 MP3 文件路径。
//
// req.Voice:   音色 ID（见音色列表），留空默认 "female-shaonv"
// req.Speed:   语速（0.5~2.0，1.0 正常）
// req.Pitch:   音调（-12~12 半音，0 正常）；req.Pitch 字段为 0~2 浮点，映射到 -12~12
// req.Emotion: 情感标签（happy/sad/angry/fearful/surprised/neutral），留空不设置
func (p *MinimaxTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voiceID := req.Voice
	if voiceID == "" {
		voiceID = "female-shaonv"
	}

	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}
	if speed < 0.5 {
		speed = 0.5
	} else if speed > 2.0 {
		speed = 2.0
	}

	// req.Pitch: 0~2 → -12~12 半音（1.0=0 半音）
	pitch := 0
	if req.Pitch != 0 {
		pitch = int((req.Pitch - 1.0) * 12.0)
		if pitch < -12 {
			pitch = -12
		} else if pitch > 12 {
			pitch = 12
		}
	}

	ttsReq := minimaxTTSRequest{
		Model:  minimaxTTSDefaultModel,
		Text:   req.Text,
		Stream: false,
		VoiceSetting: minimaxVoiceSetting{
			VoiceID: voiceID,
			Speed:   speed,
			Pitch:   pitch,
			Vol:     1.0,
			Emotion: req.Emotion,
		},
		AudioSetting: minimaxAudioSetting{
			SampleRate: 32000,
			Bitrate:    128000,
			Format:     "mp3",
		},
	}

	bodyBytes, err := json.Marshal(ttsReq)
	if err != nil {
		return nil, fmt.Errorf("minimax-tts: marshal request: %w", err)
	}

	endpoint := minimaxTTSEndpoint
	if p.groupID != "" {
		endpoint = fmt.Sprintf("%s?GroupId=%s", minimaxTTSEndpoint, p.groupID)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("minimax-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("minimax-tts: HTTP %d: %s", resp.StatusCode, string(errBody[:min(200, len(errBody))]))
	}

	ct := resp.Header.Get("Content-Type")
	var audioHex string

	if strings.Contains(ct, "text/event-stream") {
		// SSE 响应：逐行解析，累积 audio hex 数据
		audioHex, err = parseMinimaxSSE(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("minimax-tts: parse SSE: %w", err)
		}
	} else {
		// 标准 JSON 响应
		var result minimaxSSEEvent
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("minimax-tts: decode response: %w", err)
		}
		if result.BaseResp.StatusCode != 0 {
			return nil, fmt.Errorf("minimax-tts: API error %d: %s",
				result.BaseResp.StatusCode, result.BaseResp.StatusMsg)
		}
		audioHex = result.Data.Audio
	}

	if audioHex == "" {
		return nil, fmt.Errorf("minimax-tts: no audio data received")
	}

	audioData, err := hex.DecodeString(audioHex)
	if err != nil {
		return nil, fmt.Errorf("minimax-tts: decode hex audio: %w", err)
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("minimax-tts: write temp file: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// parseMinimaxSSE 从 SSE 响应流中解析并累积音频 hex 数据
func parseMinimaxSSE(body io.Reader) (string, error) {
	var hexBuf strings.Builder
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

		var event minimaxSSEEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.BaseResp.StatusCode != 0 {
			return "", fmt.Errorf("error %d: %s", event.BaseResp.StatusCode, event.BaseResp.StatusMsg)
		}
		if event.Data.Audio != "" {
			hexBuf.WriteString(event.Data.Audio)
		}
		if event.Data.Status == 2 {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return hexBuf.String(), nil
}

// Ensure interface compliance.
var _ AIProvider = (*MinimaxTTSProvider)(nil)
