package deepseek

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("deepseek", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewDeepSeekProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
}
