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
// 完整音色列表：https://cloud.tencent.com/document/product/1073/92668
//
// 超自然大模型音色（50xxxx / 60xxxx）：
//
//	502001 = 智小柔（聊天女声）  502003 = 智小敏（聊天女声）  502004 = 智小满（营销女声）
//	502005 = 智小解（解说男声）  502006 = 智小悟（聊天男声）  502007 = 智小虎（聊天童声）
//	602003 = 爱小悠（聊天女声）  602004 = 暖心阿灿（聊天男声）  602005 = 专业梓欣（聊天女声）
//	603000 = 懂事少年（特色男声）  603001 = 潇湘妹妹（特色女声）  603002 = 软萌心心（特色男童声）
//	603003 = 随和老李（聊天男声）  603004 = 温柔小柠（聊天女声）  603005 = 知心大林（聊天男声）
//	603006 = 沉稳青叔（聊天男声）  603007 = 邻家女孩（聊天女声）
//
// 大模型音色（50xxxx / 60xxxx）：
//
//	501000 = 智斌（阅读男声）  501001 = 智兰（资讯女声）  501002 = 智菊（阅读女声）
//	501003 = 智宇（阅读男声）  501004 = 月华（聊天女声）  501005 = 飞镜（聊天男声）
//	501006 = 千嶂（聊天男声）  501007 = 浅草（聊天男声）
//	501008 = WeJames（英文男声）  501009 = WeWinny（英文女声）
//	601008 = 爱小豪（聊天男声，多情感）  601009 = 爱小芊（聊天女声，多情感）  601010 = 爱小娇（聊天女声，多情感）
//	601011 = 爱小川（聊天男声）  601012 = 爱小璟（特色女声）  601013 = 爱小伊（阅读女声）  601014 = 爱小简（聊天男声）
//
// 精品音色（10xxxx）：
//
//	101001 = 智瑜（情感女声）  101004 = 智云（通用男声）  101011 = 智燕（新闻女声）
//	101013 = 智辉（新闻男声）  101015 = 智萌（男童声）    101016 = 智甜（女童声）
//	101019 = 智彤（粤语女声）  101021 = 智瑞（新闻男声）  101026 = 智希（通用女声）
//	101027 = 智梅（通用女声）  101030 = 智柯（通用男声）  101050 = WeJack（英文男声）
//	101054 = 智友（通用男声）  101055 = 智付（通用女声）
//
// 一句话复刻：200000000（配合 FastVoiceType 使用）
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
	Text            string  `json:"Text"`
	SessionId       string  `json:"SessionId"`
	Volume          float64 `json:"Volume"`          // [-10,10]，默认 0
	Speed           float64 `json:"Speed"`           // [-2,6]，默认 0
	ProjectId       int     `json:"ProjectId"`       // 默认 0
	ModelType       int     `json:"ModelType"`       // 1=默认
	VoiceType       int     `json:"VoiceType"`       // 音色 ID；一句话复刻固定填 200000000
	FastVoiceType   string  `json:"FastVoiceType,omitempty"` // 一句话复刻音色 ID（VoiceType=200000000 时填入）
	PrimaryLanguage int     `json:"PrimaryLanguage"` // 1=中文（默认），2=英文
	SampleRate      int     `json:"SampleRate"`      // 8000/16000（默认）/24000
	Codec           string  `json:"Codec"`           // mp3/wav/pcm，默认 mp3
	EnableSubtitle  bool    `json:"EnableSubtitle,omitempty"`  // 是否返回时间戳
	SegmentRate     int     `json:"SegmentRate,omitempty"`     // 断句敏感阈值 [0,1,2]
	EmotionCategory string  `json:"EmotionCategory,omitempty"` // 情感分类
	EmotionIntensity int    `json:"EmotionIntensity,omitempty"` // 情感强度 50~200
}

// tencentSubtitle 腾讯云 TTS 时间戳单元
type tencentSubtitle struct {
	BeginIndex int    `json:"BeginIndex"`
	EndIndex   int    `json:"EndIndex"`
	BeginTime  int    `json:"BeginTime"`
	EndTime    int    `json:"EndTime"`
	Phoneme    string `json:"Phoneme"`
	Text       string `json:"Text"`
}

// tencentTTSResponse 响应
type tencentTTSResponse struct {
	Response struct {
		Audio     string            `json:"Audio"`     // base64 编码的音频
		SessionId string            `json:"SessionId"`
		Subtitles []tencentSubtitle `json:"Subtitles,omitempty"`
		RequestId string            `json:"RequestId"`
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
		// 超自然大模型音色
		"502001", // 智小柔（聊天女声）
		"502003", // 智小敏（聊天女声）
		"502004", // 智小满（营销女声）
		"502005", // 智小解（解说男声）
		"502006", // 智小悟（聊天男声）
		"502007", // 智小虎（聊天童声）
		"602003", // 爱小悠（聊天女声）
		"602004", // 暖心阿灿（聊天男声）
		"602005", // 专业梓欣（聊天女声）
		"603000", // 懂事少年（特色男声）
		"603001", // 潇湘妹妹（特色女声）
		"603002", // 软萌心心（特色男童声）
		"603003", // 随和老李（聊天男声）
		"603004", // 温柔小柠（聊天女声）
		"603005", // 知心大林（聊天男声）
		"603006", // 沉稳青叔（聊天男声）
		"603007", // 邻家女孩（聊天女声）
		// 大模型音色
		"501000", // 智斌（阅读男声）
		"501001", // 智兰（资讯女声）
		"501002", // 智菊（阅读女声）
		"501003", // 智宇（阅读男声）
		"501004", // 月华（聊天女声）
		"501005", // 飞镜（聊天男声）
		"501006", // 千嶂（聊天男声）
		"501007", // 浅草（聊天男声）
		"501008", // WeJames（英文男声）
		"501009", // WeWinny（英文女声）
		"601008", // 爱小豪（聊天男声，多情感）
		"601009", // 爱小芊（聊天女声，多情感）
		"601010", // 爱小娇（聊天女声，多情感）
		"601011", // 爱小川（聊天男声）
		"601012", // 爱小璟（特色女声）
		"601013", // 爱小伊（阅读女声）
		"601014", // 爱小简（聊天男声）
		// 精品音色
		"101001", // 智瑜（情感女声）
		"101004", // 智云（通用男声）
		"101011", // 智燕（新闻女声）
		"101013", // 智辉（新闻男声）
		"101015", // 智萌（男童声）
		"101016", // 智甜（女童声）
		"101019", // 智彤（粤语女声）
		"101021", // 智瑞（新闻男声）
		"101026", // 智希（通用女声）
		"101027", // 智梅（通用女声）
		"101030", // 智柯（通用男声）
		"101050", // WeJack（英文男声）
		"101054", // 智友（通用男声）
		"101055", // 智付（通用女声）
		// 一句话复刻（需配合 FastVoiceType 使用）
		"200000000",
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

// AudioGenerate 调用腾讯云语音合成，返回音频临时文件路径。
//
// req.Voice:         音色 ID（如 "101002"）；一句话复刻填 "200000000"，同时设置 req.FastVoiceType
// req.FastVoiceType: 一句话复刻音色 ID（如 "WCHN-766926cXXX"）；VoiceType=200000000 时生效
// req.Speed:         语速（0.5~2.0，1.0=正常；映射到腾讯 -2~6）
// req.Loudness:      音量（[-10,10]，直接传给腾讯 Volume；0=默认正常音量）
// req.Codec:         音频格式（mp3/wav/pcm；默认 mp3）
// req.SampleRate:    采样率（8000/16000/24000；默认 16000）
// req.EnableSubtitle: 是否返回字级时间戳
// req.SegmentRate:   断句阈值（[0,1,2]；默认 0）
// req.Emotion:       情感分类（neutral/sad/happy/angry/fear/news/story/radio/poetry/call/sajiao 等）
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

	// Speed: req.Speed 0.5~2.0 → [-2,6]（1.0=正常=0）
	speed := 0.0
	if req.Speed > 0 && req.Speed != 1.0 {
		speed = (req.Speed - 1.0) * 4.0
		if speed < -2 {
			speed = -2
		} else if speed > 6 {
			speed = 6
		}
	}

	// Loudness → Volume [-10,10]
	volume := req.Loudness
	if volume < -10 {
		volume = -10
	} else if volume > 10 {
		volume = 10
	}

	codec := req.Codec
	if codec == "" {
		codec = "mp3"
	}
	sampleRate := req.SampleRate
	if sampleRate == 0 {
		sampleRate = 16000
	}

	idBytes := make([]byte, 8)
	rand.Read(idBytes) //nolint:errcheck
	sessionID := hex.EncodeToString(idBytes)

	ttsReq := tencentTTSRequest{
		Text:            req.Text,
		SessionId:       sessionID,
		Volume:          volume,
		Speed:           speed,
		ProjectId:       0,
		ModelType:       1,
		VoiceType:       voiceType,
		PrimaryLanguage: 1,
		SampleRate:      sampleRate,
		Codec:           codec,
	}
	if req.FastVoiceType != "" {
		ttsReq.FastVoiceType = req.FastVoiceType
	}
	if req.EnableSubtitle {
		ttsReq.EnableSubtitle = true
	}
	if req.SegmentRate > 0 {
		ttsReq.SegmentRate = req.SegmentRate
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

	tmpPath := fmt.Sprintf("/tmp/inkframe-tts-%s.%s", sessionID, codec)
	if err := os.WriteFile(tmpPath, audioData, 0644); err != nil {
		return nil, fmt.Errorf("tencent-tts: write temp file: %w", err)
	}

	// 转换时间戳
	var subtitles []TTSSubtitle
	for _, s := range result.Response.Subtitles {
		subtitles = append(subtitles, TTSSubtitle{
			BeginIndex: s.BeginIndex,
			EndIndex:   s.EndIndex,
			BeginTime:  s.BeginTime,
			EndTime:    s.EndTime,
			Phoneme:    s.Phoneme,
			Text:       s.Text,
		})
	}

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    codec,
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
		Subtitles: subtitles,
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
