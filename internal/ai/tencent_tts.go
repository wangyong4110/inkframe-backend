package ai

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// TencentTTSProvider 腾讯云语音合成提供者
// 官方文档：https://cloud.tencent.com/document/product/1073/94308
// 鉴权：TC3-HMAC-SHA256（腾讯云 API 3.0 签名方式）
//
// 音色列表（VoiceType）：
//
//	101001 = 智言（男，标准）
//	101002 = 智雅（女，标准）
//	101003 = 智燕（女，温暖）
//	101004 = 智晶（女，标准）
//	101005 = 智嘉（男，专业）
//	101006 = 智开（男，播音）
//	101007 = 智凯（男，专业）
//	101008 = 智浩（男，播音）
//	101009 = 智莉（女，温暖）
//	101010 = 智华（男，年轻）
//	101011 = 智燃（男，活力）
//	101012 = 智雪（女，温柔）
//	101013 = 智希（女，活泼）
//	101014 = 智宁（男，成熟）
//	101015 = 智萌（童，活泼）
//	101016 = 智甜（女，甜美）
//	101017 = 智蓉（女，四川话）
//	101050 = WeJack（英文男声）
//	101051 = WeRose（英文女声）
type TencentTTSProvider struct {
	secretID  string // 腾讯云 SecretId
	secretKey string // 腾讯云 SecretKey
	region    string // 地域（默认 ap-guangzhou）
	client    *http.Client
}

const (
	tencentTTSHost    = "tts.tencentcloudapi.com"
	tencentTTSURL     = "https://" + tencentTTSHost
	tencentTTSVersion = "2019-08-23"
	tencentTTSAction  = "TextToVoice"
	tencentTTSService = "tts"
)

// tencentTTSRequest 请求参数
type tencentTTSRequest struct {
	Text       string  `json:"Text"`
	SessionId  string  `json:"SessionId"`
	Volume     float64 `json:"Volume"`     // 0~10，默认 0
	Speed      float64 `json:"Speed"`      // -2~6，默认 0
	ProjectId  int     `json:"ProjectId"`  // 默认 0
	ModelType  int     `json:"ModelType"`  // 1=基础版，2=精品版（默认 1）
	VoiceType  int     `json:"VoiceType"`  // 音色 ID
	PrimaryLanguage int `json:"PrimaryLanguage"` // 1=中文（默认）
	SampleRate int     `json:"SampleRate"` // 采样率：8000/16000（默认 16000）
	Codec      string  `json:"Codec"`      // mp3/pcm/ogg，默认 mp3
	EmotionCategory string `json:"EmotionCategory,omitempty"` // 情感分类
	EmotionIntensity int  `json:"EmotionIntensity,omitempty"` // 情感强度 50~200
}

// tencentTTSResponse 响应
type tencentTTSResponse struct {
	Response struct {
		Audio     string `json:"Audio"`     // base64 编码的 MP3
		SessionId string `json:"SessionId"`
		RequestId string `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error,omitempty"`
	} `json:"Response"`
}

// NewTencentTTSProvider 创建腾讯云语音合成提供者
// secretID / secretKey: 腾讯云 API 密钥（CAM 控制台获取）
// region: 地域（留空默认 ap-guangzhou）
func NewTencentTTSProvider(secretID, secretKey, region string) *TencentTTSProvider {
	if region == "" {
		region = "ap-guangzhou"
	}
	return &TencentTTSProvider{
		secretID:  secretID,
		secretKey: secretKey,
		region:    region,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *TencentTTSProvider) GetName() string { return "tencent-tts" }

func (p *TencentTTSProvider) GetModels() []string {
	return []string{
		"101001", // 智言（男，标准）
		"101002", // 智雅（女，标准）
		"101003", // 智燕（女，温暖）
		"101004", // 智晶（女，标准）
		"101005", // 智嘉（男，专业）
		"101006", // 智开（男，播音）
		"101008", // 智浩（男，播音）
		"101009", // 智莉（女，温暖）
		"101010", // 智华（男，年轻）
		"101011", // 智燃（男，活力）
		"101012", // 智雪（女，温柔）
		"101013", // 智希（女，活泼）
		"101014", // 智宁（男，成熟）
		"101015", // 智萌（童，活泼）
		"101016", // 智甜（女，甜美）
		"101050", // WeJack（英文男声）
		"101051", // WeRose（英文女声）
	}
}

func (p *TencentTTSProvider) HealthCheck(ctx context.Context) error {
	if p.secretID == "" || p.secretKey == "" {
		return fmt.Errorf("tencent-tts: secret_id or secret_key not configured")
	}
	return nil
}

func (p *TencentTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("tencent-tts: text generation not supported")
}

func (p *TencentTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("tencent-tts: streaming not supported")
}

func (p *TencentTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("tencent-tts: embeddings not supported")
}

func (p *TencentTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("tencent-tts: image generation not supported")
}

// AudioGenerate 调用腾讯云语音合成，返回 MP3 文件路径。
//
// req.Voice:  音色 ID 字符串（如 "101001"），留空默认 "101002"（智雅，女）
// req.Speed:  语速（-2~6，0=正常；req.Speed 0.5~2.0 映射到 -2~6）
// req.Emotion: 情感分类（如 "arousal"=激动, "calm"=平静, "fear"=恐惧 etc.）
func (p *TencentTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	voiceStr := req.Voice
	if voiceStr == "" {
		return nil, fmt.Errorf("tencent-tts: 未指定音色，请先在小说设置或角色配置中选择音色")
	}
	voiceType, err := strconv.Atoi(voiceStr)
	if err != nil {
		return nil, fmt.Errorf("tencent-tts: 无效的音色 ID %q（应为整数，如 101002）", voiceStr)
	}

	// Speed: req.Speed 0.5~2.0 → -2~6（1.0=正常=0）
	speed := 0.0
	if req.Speed > 0 && req.Speed != 1.0 {
		speed = (req.Speed - 1.0) * 4.0 // 0.5→-2, 2.0→+4
		if speed < -2 {
			speed = -2
		} else if speed > 6 {
			speed = 6
		}
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	sessionID := hex.EncodeToString(idBytes)

	ttsReq := tencentTTSRequest{
		Text:            req.Text,
		SessionId:       sessionID,
		Volume:          0,
		Speed:           speed,
		ProjectId:       0,
		ModelType:       1,
		VoiceType:       voiceType,
		PrimaryLanguage: 1,
		SampleRate:      16000,
		Codec:           "mp3",
	}
	if req.Emotion != "" {
		ttsReq.EmotionCategory = req.Emotion
		ttsReq.EmotionIntensity = 100
	}

	bodyBytes, err := json.Marshal(ttsReq)
	if err != nil {
		return nil, fmt.Errorf("tencent-tts: marshal request: %w", err)
	}

	timestamp := time.Now().Unix()
	authHeader, err := p.buildAuthHeader(timestamp, bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("tencent-tts: build auth: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", tencentTTSURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Host", tencentTTSHost)
	httpReq.Header.Set("X-TC-Action", tencentTTSAction)
	httpReq.Header.Set("X-TC-Version", tencentTTSVersion)
	httpReq.Header.Set("X-TC-Timestamp", strconv.FormatInt(timestamp, 10))
	httpReq.Header.Set("X-TC-Region", p.region)
	httpReq.Header.Set("Authorization", authHeader)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tencent-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tencent-tts: HTTP %d: %s", resp.StatusCode, string(errBody[:min(200, len(errBody))]))
	}

	var result tencentTTSResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("tencent-tts: decode response: %w", err)
	}
	if result.Response.Error != nil {
		return nil, fmt.Errorf("tencent-tts: API error %s: %s",
			result.Response.Error.Code, result.Response.Error.Message)
	}
	if result.Response.Audio == "" {
		return nil, fmt.Errorf("tencent-tts: no audio data in response")
	}

	audioData, err := base64.StdEncoding.DecodeString(result.Response.Audio)
	if err != nil {
		return nil, fmt.Errorf("tencent-tts: base64 decode: %w", err)
	}

	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.mp3", sessionID)
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("tencent-tts: write temp file: %w", err)
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// buildAuthHeader 构建腾讯云 TC3-HMAC-SHA256 鉴权头
func (p *TencentTTSProvider) buildAuthHeader(timestamp int64, payload []byte) (string, error) {
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")

	// Step 1: 构建规范请求串
	canonicalHeaders := "content-type:application/json\nhost:" + tencentTTSHost + "\n"
	signedHeaders := "content-type;host"
	hashedPayload := tc3SHA256Hex(payload)
	canonicalReq := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + hashedPayload

	// Step 2: 构建待签名字符串
	credentialScope := date + "/" + tencentTTSService + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		credentialScope + "\n" +
		tc3SHA256Hex([]byte(canonicalReq))

	// Step 3: 计算签名
	secretDate := tc3HMACSHA256([]byte("TC3"+p.secretKey), []byte(date))
	secretService := tc3HMACSHA256(secretDate, []byte(tencentTTSService))
	secretSigning := tc3HMACSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(tc3HMACSHA256(secretSigning, []byte(stringToSign)))

	// Step 4: 组装 Authorization
	auth := fmt.Sprintf(
		"TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.secretID, credentialScope, signedHeaders, signature,
	)
	return auth, nil
}

func tc3SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func tc3HMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// Ensure interface compliance.
var _ AIProvider = (*TencentTTSProvider)(nil)
