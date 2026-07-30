package service

import (
	"context"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/commons"
	"github.com/inkframe/inkframe-backend/internal/logger"
)

type GenerateAudioOptions struct {
	Text       string  `json:"text"`
	Voice      string  `json:"voice"`
	VoiceModel string  `json:"voice_model"` // provider 名称，如 "doubao-speech"；空=走扫描匹配
	Speed      float64 `json:"speed"`
	Emotion    string  `json:"emotion"`
	Language   string  `json:"language"`
}

// AudioGenerateWithOptions 支持语速、风格和语言/方言的 TTS 生成。
// DB 是唯一权威来源：loadDBVoiceProvider 失败就直接把错误返回给调用方，绝不静默退化到
// 别的 provider——否则用户以为自己在 DB 里配置的 provider 生效了，实际上请求偷偷换成了
// 另一个完全不同的 provider，故障也被日志吞掉。
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, opt GenerateAudioOptions) (string, error) {
	logger.Printf("[TTS] AudioGenerateWithOptions: tenantID=%d options=%+v", tenantID, opt)

	if s.providerRepo == nil {
		return "", fmt.Errorf("provider repository not configured")
	}

	providerMeta, _, err := s.getTenantProvider(tenantID, commons.Voice, opt.VoiceModel)
	if err != nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: getTenantProvider ERROR: %v", err)
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key: %w", err)
	}
	provider, ok := providerMeta.(ai.AudioProvider)
	if !ok {
		return "", fmt.Errorf("configured voice provider %q does not support audio generation", providerMeta.GetName())
	}
	logger.Printf("[TTS] AudioGenerateWithOptions: selected DB provider=%q for voice=%q", provider.GetName(), opt.Voice)

	if opt.Speed <= 0 {
		opt.Speed = 1.0
	}
	ttsStart := time.Now()
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     opt.Text,
		Voice:    opt.Voice,
		Speed:    opt.Speed,
		Emotion:  opt.Emotion,
		Language: opt.Language,
	})
	if err != nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR provider=%q AudioGenerate failed elapsed=%s: %v",
			provider.GetName(), time.Since(ttsStart).Round(time.Millisecond), err)
		return "", err
	}
	if resp.URL == "" {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR provider=%q returned empty URL elapsed=%s", provider.GetName(), time.Since(ttsStart).Round(time.Millisecond))
	} else {
		logger.Printf("[TTS] AudioGenerateWithOptions: provider=%q success elapsed=%s url=%q", provider.GetName(), time.Since(ttsStart).Round(time.Millisecond), resp.URL)
	}
	return resp.URL, nil
}

// GenerateSFX 使用 DB 中配置的 sfx 类型提供商生成音效，返回 CDN URL 和时长（秒）。
// prompt: 音效描述，如 "春节烟花声"；duration: 期望时长（秒，3.0~10.0）。
func (s *AIService) GenerateSFX(ctx context.Context, tenantID uint, prompt string, duration float64) (string, float64, error) {
	providerMeta, _, err := s.getTenantProvider(tenantID, commons.SFX, "")
	if err != nil {
		return "", 0, err
	}
	provider, ok := providerMeta.(ai.AudioProvider)
	if !ok {
		return "", 0, fmt.Errorf("configured SFX provider %q does not support audio generation", providerMeta.GetName())
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     prompt,
		Duration: duration,
	})
	if err != nil {
		return "", 0, err
	}
	return resp.URL, resp.Duration, nil
}

// HasSFXProvider 判断当前租户是否已配置可用的文生音效提供商。
func (s *AIService) HasSFXProvider(tenantID uint) bool {
	_, _, err := s.getTenantProvider(tenantID, commons.SFX, "")
	return err == nil
}
