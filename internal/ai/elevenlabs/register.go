package elevenlabs

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("elevenlabs-sfx", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewElevenLabsSFXProvider(cfg.APIKey, cfg.APIEndpoint), nil
	})
}
