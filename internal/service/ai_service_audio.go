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
// Provider 选取顺序（DB 模式优先，与图像生成保持一致）：
//  1. DB 中租户配置的 voice/tts 类型 provider
//  2. config.yaml ai.tasks.tts 指定的 provider（静态模式兜底）
//  3. env-var 注册的默认 provider（静态模式兜底）
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, text, voice string, speed float64, style string, language ...string) (string, error) {
	lang := ""
	if len(language) > 0 {
		lang = language[0]
	}
	logger.Printf("[TTS] AudioGenerateWithOptions: tenantID=%d voice=%q speed=%.2f style=%q language=%q textLen=%d text=%q",
		tenantID, voice, speed, style, lang, len([]rune(text)), truncate(text, 60))

	if s.aiManager == nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR AI manager not initialized")
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider
	var provName string

	// 1. DB 模式：优先选取内置音色表中包含请求 voice ID 的 provider
	if s.providerRepo != nil {
		if p, name, err := s.loadDBVoiceProvider(tenantID, "voice", voice); err == nil && p != nil {
			provider = p
			provName = name
			logger.Printf("[TTS] AudioGenerateWithOptions: selected DB provider=%q for voice=%q", name, voice)
		} else if err != nil {
			logger.Errorf("[TTS] AudioGenerateWithOptions: loadDBVoiceProvider ERROR: %v (will try static fallback)", err)
		}
	}

	// 2. 静态模式：config.yaml task routing
	if provider == nil && s.taskRouting.TTS != "" {
		if p, err := s.aiManager.GetProvider(s.taskRouting.TTS); err == nil {
			provider = p
			provName = s.taskRouting.TTS
			logger.Printf("[TTS] AudioGenerateWithOptions: selected static provider=%q (taskRouting.TTS)", s.taskRouting.TTS)
		} else {
			logger.Errorf("[TTS] AudioGenerateWithOptions: static provider %q not available: %v", s.taskRouting.TTS, err)
		}
	}
	// 注意：不再兜底到默认 LLM provider，LLM 提供商通常不支持 /audio/speech 接口，
	// 兜底只会产生 404 错误，不如直接给用户明确的配置提示。

	if provider == nil {
		logger.Errorf("[TTS] AudioGenerateWithOptions: ERROR no voice provider found (tenantID=%d voice=%q) — 请在「模型管理」中配置 voice/tts 类型提供商", tenantID, voice)
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key")
	}

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
