package docs

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("azure", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewAzureProvider(cfg.APIKey, cfg.APIEndpoint, "", cfg.APIVersion, cfg.Timeout), nil
	})
}
