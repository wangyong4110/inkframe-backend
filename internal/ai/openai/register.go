package openai

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("openai", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewOpenAIProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
	ai.Register("openai-image", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewOpenAIProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
}
