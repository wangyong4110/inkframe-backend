package doubao

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	// "doubao" 和 "volcengine-ark-img" 共用同一个工厂，按 ModelType 分发：
	//   video → DoubaoVideoProvider（Seedance 视频生成）
	//   其他  → DoubaoProvider（LLM）
	factory := func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		if cfg.ModelType == "video" {
			return NewDoubaoVideoProvider(cfg.APIKey, cfg.APIEndpoint), nil
		}
		return NewDoubaoProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	}
	ai.Register("doubao", factory)
	ai.Register("volcengine-ark-img", factory)

	// "seedance" 是 doubao 视频生成的别名
	ai.Register("seedance", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewDoubaoVideoProvider(cfg.APIKey, cfg.APIEndpoint), nil
	})

	ai.Register("doubao-speech", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewDoubaoSpeechProvider(cfg.APIKey, cfg.APIVersion), nil
	})
	ai.Register("doubao-speech-v1", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewDoubaoSpeechV1Provider(cfg.APIKey, cfg.APISecretKey, cfg.APIVersion), nil
	})
}
