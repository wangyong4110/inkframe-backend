package service

import (
	"context"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
)

// AudioGenerate 调用默认 AI provider 生成 TTS 音频，返回本地文件路径（file:// URL）
func (s *AIService) AudioGenerate(ctx context.Context, text, voice string) (string, error) {
	return s.AudioGenerateWithOptions(ctx, 0, text, voice, 1.0, "")
}

// AudioGenerateWithOptions 支持语速、风格和语言/方言的 TTS 生成。
// DB 是唯一权威来源：loadDBVoiceProvider 失败就直接把错误返回给调用方，绝不静默退化到
// 别的 provider——否则用户以为自己在 DB 里配置的 provider 生效了，实际上请求偷偷换成了
// 另一个完全不同的 provider，故障也被日志吞掉。
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, text, voice string, speed float64, style string, language ...string) (string, error) {
	lang := ""
	if len(language) > 0 {
		lang = language[0]
	}
	logger.Printf("[TTS] AudioGenerateWithOptions: tenantID=%d voice=%q speed=%.2f style=%q language=%q textLen=%d text=%q",
		tenantID, voice, speed, style, lang, len([]rune(text)), truncate(text, 60))

	if s.providerRepo == nil {
		return "", fmt.Errorf("provider repository not configured")
	}

	provider, provName, err := s.loadDBVoiceProvider(tenantID, "voice", voice)
	if err != nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: loadDBVoiceProvider ERROR: %v", err)
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key: %w", err)
	}
	logger.Printf("[TTS] AudioGenerateWithOptions: selected DB provider=%q for voice=%q", provName, voice)

	release, err := s.acquireProviderSlot(ctx, tenantID, provName)
	if err != nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: acquireProviderSlot ERROR provider=%q: %v", provName, err)
		return "", err
	}
	defer release()

	if speed <= 0 {
		speed = 1.0
	}
	ttsStart := time.Now()
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     text,
		Voice:    voice,
		Speed:    speed,
		Emotion:  style,
		Language: lang,
	})
	if err != nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR provider=%q AudioGenerate failed elapsed=%s: %v",
			provName, time.Since(ttsStart).Round(time.Millisecond), err)
		return "", err
	}
	if resp.URL == "" {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR provider=%q returned empty URL elapsed=%s", provName, time.Since(ttsStart).Round(time.Millisecond))
	} else {
		logger.Printf("[TTS] AudioGenerateWithOptions: provider=%q success elapsed=%s url=%q", provName, time.Since(ttsStart).Round(time.Millisecond), resp.URL)
	}
	return resp.URL, nil
}

// GenerateSFX 使用 DB 中配置的 sfx 类型提供商生成音效，返回 CDN URL 和时长（秒）。
// prompt: 音效描述，如 "春节烟花声"；duration: 期望时长（秒，3.0~10.0）。
func (s *AIService) GenerateSFX(ctx context.Context, tenantID uint, prompt string, duration float64) (string, float64, error) {
	provider, provName, err := s.loadDBProviderByType(tenantID, "sfx")
	if err != nil {
		return "", 0, err
	}
	release, err := s.acquireProviderSlot(ctx, tenantID, provName)
	if err != nil {
		return "", 0, err
	}
	defer release()
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
	_, _, err := s.loadDBProviderByType(tenantID, "sfx")
	return err == nil
}

// GenerateSFXWithProvider 使用指定名称的 sfx 提供商生成音效（从 DB 加载密钥）。
// 用于前端明确选择某个提供商（如 "elevenlabs-sfx"）时的强制路由。
func (s *AIService) GenerateSFXWithProvider(ctx context.Context, tenantID uint, providerName string, prompt string, duration float64) (string, float64, error) {
	p, err := s.loadDBProviderByName(tenantID, providerName)
	if err != nil {
		return "", 0, err
	}
	release, err := s.acquireProviderSlot(ctx, tenantID, providerName)
	if err != nil {
		return "", 0, err
	}
	defer release()
	resp, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{Text: prompt, Duration: duration})
	if err != nil {
		return "", 0, err
	}
	return resp.URL, resp.Duration, nil
}
