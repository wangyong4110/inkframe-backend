package dashscope

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("aliyun-tts", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewAliyunTTSProvider(cfg.APIKey, cfg.APIEndpoint), nil
	})
	ai.Register("qwen-tts", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewQwenTTSProvider(cfg.APIKey, cfg.APIEndpoint), nil
	})
	ai.Register("fun-music", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewFunMusicProvider(cfg.APIKey), nil
	})
	// "qianwen" 根据 ModelType 分发到不同的子 provider：
	//   voice → QianwenTTSRouter（路由到 AliyunTTS / QwenTTS）
	//   video → HappyHorseProvider（DashScope 视频生成）
	//   其他  → QianwenProvider（LLM）
	ai.Register("qianwen", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		switch cfg.ModelType {
		case "voice":
			return NewQianwenTTSRouter(cfg.APIKey, cfg.APIEndpoint), nil
		case "video":
			return NewHappyHorseProvider(cfg.APIKey, cfg.APIEndpoint), nil
		default:
			return NewQianwenProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
		}
	})
	ai.Register("happyhorse", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewHappyHorseProvider(cfg.APIKey, cfg.APIEndpoint), nil
	})
}
