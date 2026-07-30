package minimax

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	ai.Register("minimax-music", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewMinimaxMusicProvider(cfg.APIKey), nil
	})
	ai.Register("minimax-video", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewMinimaxVideoProvider(cfg.APIKey), nil
	})
}
