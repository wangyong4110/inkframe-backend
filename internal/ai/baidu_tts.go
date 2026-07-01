package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// BaiduTTSProvider 百度智能云语音合成提供者
// 官方文档：https://cloud.baidu.com/doc/SPEECH/s/mlbxh7xie
//
// 音色列表（per 参数）：
//
//	0  = 度小美（标准女声）
//	1  = 度小宇（标准男声）
//	3  = 度逍遥（情感男声，磁性）
//	4  = 度丫丫（情感童声，女）
//	5  = 度小娇（情感女声）
//	103 = 度米朵（精品童声，女）
//	106 = 度博文（精品情感男声）
//	110 = 度小童（精品童声，男）
//	111 = 度小萌（精品童声，女）
type BaiduTTSProvider struct {
	apiKey    string // 百度 AI 应用 API Key (client_id)
	secretKey string // 百度 AI 应用 Secret Key (client_secret)

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time

	client *http.Client
}

const (
	baiduTokenURL = "https://aip.baidubce.com/oauth/2.0/token"
	baiduTTSURL   = "https://tsn.baidu.com/text2audio"
)

// NewBaiduTTSProvider 创建百度语音合成提供者
// apiKey / secretKey 从百度 AI 开放平台控制台获取
func NewBaiduTTSProvider(apiKey, secretKey string) *BaiduTTSProvider {
	return &BaiduTTSProvider{
		apiKey:    apiKey,
		secretKey: secretKey,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *BaiduTTSProvider) GetName() string { return "baidu-tts" }

func (p *BaiduTTSProvider) GetModels() []string {
	return []string{
		"0",   // 度小美（标准女声）
		"1",   // 度小宇（标准男声）
		"3",   // 度逍遥（情感男声）
		"4",   // 度丫丫（情感童声）
		"5",   // 度小娇（情感女声）
		"103", // 度米朵（精品童声女）
		"106", // 度博文（精品情感男）
		"110", // 度小童（精品童声男）
		"111", // 度小萌（精品童声女）
	}
}

func (p *BaiduTTSProvider) HealthCheck(ctx context.Context) error {
	if p.apiKey == "" || p.secretKey == "" {
		return fmt.Errorf("baidu-tts: api_key or secret_key not configured")
	}
	_, err := p.getAccessToken(ctx)
	return err
}

func (p *BaiduTTSProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("baidu-tts: text generation not supported")
}

func (p *BaiduTTSProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, fmt.Errorf("baidu-tts: streaming not supported")
}

func (p *BaiduTTSProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("baidu-tts: embeddings not supported")
}

func (p *BaiduTTSProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, fmt.Errorf("baidu-tts: image generation not supported")
}

// AudioGenerate 调用百度短文本语音合成 API，返回 MP3 文件路径。
//
// req.Voice:   音色编号字符串（"0"~"111"，见音色列表），留空默认 "0"（度小美）
// req.Speed:   语速（0.5~2.0 映射到 0~9，1.0=正常=5）
// req.Pitch:   音调（0.5~1.5 映射到 0~9，1.0=正常=5）
// req.Model:   留空使用默认音色（等同于 req.Voice）
func (p *BaiduTTSProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	start := time.Now()

	token, err := p.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("baidu-tts: get access token: %w", err)
	}

	per := req.Voice
	if per == "" {
		per = req.Model
	}
	if per == "" {
		return nil, fmt.Errorf("baidu-tts: 未指定音色，请先在小说设置或角色配置中选择音色")
	}

	// spd: 语速 0-9，5 为正常速度。Speed 1.0 → 5
	spd := 5
	if req.Speed > 0 {
		spd = int((req.Speed - 0.5) * 9.0 / 1.5)
		if spd < 0 {
			spd = 0
		} else if spd > 9 {
			spd = 9
		}
	}

	// pit: 音调 0-9，5 为正常
	pit := 5
	if req.Pitch > 0 {
		pit = int((req.Pitch - 0.5) * 9.0 / 1.0)
		if pit < 0 {
			pit = 0
		} else if pit > 9 {
			pit = 9
		}
	}

	params := url.Values{}
	params.Set("tex", req.Text)
	params.Set("tok", token)
	params.Set("cuid", "inkframe")
	params.Set("ctp", "1")
	params.Set("lan", "zh")
	params.Set("per", per)
	params.Set("spd", fmt.Sprintf("%d", spd))
	params.Set("pit", fmt.Sprintf("%d", pit))
	params.Set("vol", "5")
	params.Set("aue", "3") // 3 = mp3

	httpReq, err := http.NewRequestWithContext(ctx, "POST", baiduTTSURL,
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("baidu-tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("baidu-tts: read response: %w", err)
	}

	// 若 Content-Type 为 application/json，则是错误响应
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") || resp.StatusCode != http.StatusOK {
		var errResp struct {
			ErrNo  int    `json:"err_no"`
			ErrMsg string `json:"err_msg"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.ErrNo != 0 {
			return nil, fmt.Errorf("baidu-tts: error %d: %s", errResp.ErrNo, errResp.ErrMsg)
		}
		return nil, fmt.Errorf("baidu-tts: HTTP %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("baidu-tts: no audio data received")
	}

	tmpFile, err := os.CreateTemp("", "inkframe-tts-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("baidu-tts: write temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(body); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return nil, fmt.Errorf("baidu-tts: write temp file: %w", err)
	}
	tmpFile.Close()

	return &AudioResponse{
		URL:       "file://" + tmpPath,
		Format:    "mp3",
		Duration:  float64(len(req.Text)) / 8.0,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// getAccessToken 获取（或从缓存返回）百度 API Access Token
func (p *BaiduTTSProvider) getAccessToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if p.accessToken != "" && time.Now().Before(p.tokenExpiry) {
		return p.accessToken, nil
	}

	params := url.Values{}
	params.Set("grant_type", "client_credentials")
	params.Set("client_id", p.apiKey)
	params.Set("client_secret", p.secretKey)

	req, err := http.NewRequestWithContext(ctx, "POST", baiduTokenURL,
		strings.NewReader(params.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch access_token: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode access_token response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("baidu-tts oauth error: %s — %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("baidu-tts: empty access_token")
	}

	p.accessToken = result.AccessToken
	// 提前 5 分钟刷新，避免边界竞争
	p.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second)
	return p.accessToken, nil
}

var _ AIProvider = (*BaiduTTSProvider)(nil)
