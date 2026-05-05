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

	"github.com/google/uuid"
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
	// 方言/语言：BigTTS 通过 language 字段控制发音方言（如 zh-yue 粤语、zh-scu 四川话）
	if req.Language != "" {
		reqParams["language"] = req.Language
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
	// 若未显式指定 resource，根据音色名称自动推断：
	//   _tob 后缀（角色扮演系列） → doubao-character-tts
	//   其余保持 seed-tts-2.0 / 用户配置值
	if resourceID == doubaoSpeechDefaultResourceID && strings.HasSuffix(speaker, "_tob") {
		resourceID = "doubao-character-tts"
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

// ─────────────────────────────────────────────────────────────
// DoubaoSpeechV1Provider — 豆包 HTTP 一次性合成（V1 非流式）
// 文档：https://www.volcengine.com/docs/6561/79823
//
// 与 V3 (DoubaoSpeechProvider) 的区别：
//   - 认证方式：Authorization: Bearer;{access_token}（分号分隔）
//   - 凭证：appid + access_token + cluster（火山引擎老版控制台）
//   - 请求体：app/user/audio/request 四层嵌套
//   - 成功响应码：3000（非 20000000）
//   - 不支持豆包语音合成 2.0 音色（_uranus_bigtts 后缀）
// ─────────────────────────────────────────────────────────────

// DoubaoSpeechV1Provider 豆包语音合成 V1 HTTP 接口提供者
type DoubaoSpeechV1Provider struct {
	appID       string
	accessToken string
	cluster     string // 通常为 "volcano_tts"
	client      *http.Client
}

const doubaoV1TTSEndpoint = "https://openspeech.bytedance.com/api/v1/tts"
// doubaoV1DefaultCluster 默认集群。
// volcano_tts  — 经典集群，支持 BV001_streaming 等老音色
// volcano_mega — 大模型集群，支持 _uranus_bigtts / _tob 等豆包2.0音色（推荐）
const doubaoV1DefaultCluster = "volcano_mega"
const doubaoV1SuccessCode = 3000

// doubaoV1Request V1 请求体
type doubaoV1Request struct {
	App     doubaoV1App     `json:"app"`
	User    doubaoV1User    `json:"user"`
	Audio   doubaoV1Audio   `json:"audio"`
	Request doubaoV1ReqBody `json:"request"`
}

type doubaoV1App struct {
	AppID   string `json:"appid"`
	Token   string `json:"token"`
	Cluster string `json:"cluster"`
}

type doubaoV1User struct {
	UID string `json:"uid"`
}

type doubaoV1Audio struct {
	VoiceType     string  `json:"voice_type"`
	Encoding      string  `json:"encoding"`
	SpeedRatio    float64 `json:"speed_ratio,omitempty"`
	Emotion       string  `json:"emotion,omitempty"`
	EnableEmotion bool    `json:"enable_emotion,omitempty"`
	Language      string  `json:"language,omitempty"` // 方言/语言（如 zh-yue 粤语）
}

type doubaoV1ReqBody struct {
	ReqID     string `json:"reqid"`
	Text      string `json:"text"`
	Operation string `json:"operation"` // 一次性合成固定为 "query"
}

// doubaoV1Response V1 响应体
type doubaoV1Response struct {
	ReqID    string              `json:"reqid"`
	Code     int                 `json:"code"`
	Message  string              `json:"message"`
	Sequence int                 `json:"sequence"`
	Data     string              `json:"data"` // base64 编码音频
	Addition doubaoV1Addition    `json:"addition"`
}

type doubaoV1Addition struct {
	Duration string `json:"duration"` // 毫秒字符串
}

// NewDoubaoSpeechV1Provider 创建 V1 提供者
// appID, accessToken: 火山引擎控制台获取
// cluster: 留空则默认 "volcano_tts"
func NewDoubaoSpeechV1Provider(appID, accessToken, cluster string) *DoubaoSpeechV1Provider {
	if cluster == "" {
		cluster = doubaoV1DefaultCluster
	}
	return &DoubaoSpeechV1Provider{
		appID:       appID,
		accessToken: accessToken,
		cluster:     cluster,
		client:      &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *DoubaoSpeechV1Provider) GetName() string { return "doubao-speech-v1" }

// GetModels 返回 V1 支持的主要音色列表（不包含 2.0 音色）
func (p *DoubaoSpeechV1Provider) GetModels() []string {
	return []string{
		// 通用音色
		"BV001_streaming", // 通用女声
		"BV002_streaming", // 通用男声
		"BV005_streaming", // 活泼女声
		"BV006_streaming", // 沉稳男声
		"BV007_streaming", // 新闻女声
		"BV033_streaming", // 温柔小哥
		"BV034_streaming", // 知性女声
		// 月亮系列（经典）
		"zh_female_shuangkuaisisi_moon_bigtts",    // 爽快思思
		"zh_male_jingqiangkanye_moon_bigtts",      // 精英男声
		"zh_female_linjingzhu_moon_bigtts",        // 甜美女声
		"zh_male_chunhou_moon_bigtts",             // 醇厚男声
		"zh_female_wanqingxiaochun_moon_bigtts",   // 温情晓春
		"zh_male_zhubo_moon_bigtts",               // 主播男声
		// 英文音色
		"en_female_sarah_stream",
		"en_male_adam_stream",
	}
}

func (p *DoubaoSpeechV1Provider) HealthCheck(ctx context.Context) error {
	if p.appID == "" || p.accessToken == "" {
		return fmt.Errorf("doubao-speech-v1: appid or access_token not configured")
	}
	return nil
}

func (p *DoubaoSpeechV1Provider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("doubao-speech-v1: text generation not supported")
}

func (p *DoubaoSpeechV1Provider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("doubao-speech-v1: streaming not supported")
}

func (p *DoubaoSpeechV1Provider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("doubao-speech-v1: embeddings not supported")
}

func (p *DoubaoSpeechV1Provider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("doubao-speech-v1: image generation not supported")
}

// AudioGenerate 调用豆包 TTS V1 HTTP 一次性合成接口，返回 MP3 音频文件路径。
//
// req.Voice:   音色 ID（如 "BV001_streaming" 或 "zh_female_shuangkuaisisi_moon_bigtts"），
//
//	留空使用默认音色 BV001_streaming
//
// req.Speed:   语速倍率（0.1~2.0，1.0 为正常速度）
// req.Emotion: 情感标签（如 happy/sad/angry），设置时自动开启 enable_emotion
func (p *DoubaoSpeechV1Provider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voiceType := req.Voice
	if voiceType == "" {
		voiceType = "BV001_streaming"
	}

	speedRatio := req.Speed
	if speedRatio <= 0 {
		speedRatio = 1.0
	}

	audio := doubaoV1Audio{
		VoiceType:  voiceType,
		Encoding:   "mp3",
		SpeedRatio: speedRatio,
		Language:   req.Language,
	}
	if req.Emotion != "" {
		audio.Emotion = req.Emotion
		audio.EnableEmotion = true
	}

	ttsReq := doubaoV1Request{
		App: doubaoV1App{
			AppID:   p.appID,
			Token:   p.accessToken,
			Cluster: p.cluster,
		},
		User: doubaoV1User{UID: "inkframe"},
		Audio: audio,
		Request: doubaoV1ReqBody{
			ReqID:     uuid.New().String(),
			Text:      req.Text,
			Operation: "query",
		},
	}

	body, err := json.Marshal(ttsReq)
	if err != nil {
		return nil, fmt.Errorf("doubao-speech-v1: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", doubaoV1TTSEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// V1 鉴权：Bearer;token（分号分隔，不是空格）
	httpReq.Header.Set("Authorization", "Bearer;"+p.accessToken)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("doubao-speech-v1: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		// 解析 JSON body 提供更明确的错误指引
		var errJSON struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(errBody, &errJSON) == nil {
			switch errJSON.Code {
			case 3001:
				return nil, fmt.Errorf("doubao-speech-v1: 应用未获授权访问 TTS 资源（%s）。" +
					"请前往火山引擎控制台 → 语音技术 → 应用管理，确认应用已开通「语音合成」服务并激活对应资源包", errJSON.Message)
			case 3000:
				// 正常成功码不会出现在 4xx 里，防御性处理
			default:
				return nil, fmt.Errorf("doubao-speech-v1: API 错误 (code=%d): %s", errJSON.Code, errJSON.Message)
			}
		}
		return nil, fmt.Errorf("doubao-speech-v1: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	var result doubaoV1Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("doubao-speech-v1: decode response: %w", err)
	}
	if result.Code != doubaoV1SuccessCode {
		return nil, fmt.Errorf("doubao-speech-v1: API error (code=%d): %s", result.Code, result.Message)
	}
	if result.Data == "" {
		return nil, fmt.Errorf("doubao-speech-v1: no audio data in response")
	}

	audioData, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return nil, fmt.Errorf("doubao-speech-v1: base64 decode: %w", err)
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", hex.EncodeToString(idBytes))
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("doubao-speech-v1: write temp file: %w", err)
	}

	duration := float64(len(req.Text)) / 8.0 // 粗略估算
	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  duration,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}
