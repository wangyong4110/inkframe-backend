package kling

import (
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	// "kling" 按 ModelType 分发：
	//   video → KlingProvider（视频生成）
	ai.Register("kling", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		if cfg.ModelType == "video" {
			return NewKlingProvider(cfg.APIKey, cfg.APISecretKey, cfg.APIEndpoint), nil
		}
		return nil, fmt.Errorf("provider %q only supports video model type, got %q", "kling", cfg.ModelType)
	})
	ai.Register("kling-sfx", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewKlingSFXProvider(cfg.APIKey, cfg.APISecretKey, cfg.APIEndpoint), nil
	})
	ai.Register("kling-tts", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewKlingTTSProvider(cfg.APIKey, cfg.APISecretKey, cfg.APIEndpoint), nil
	})
	ai.Register("kling-image", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewKlingImageProvider(cfg.APIKey, cfg.APISecretKey, cfg.APIEndpoint), nil
	})
}
