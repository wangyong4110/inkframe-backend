package anthropic

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("anthropic", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewAnthropicProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
	ai.Register("google", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewGoogleProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
}
