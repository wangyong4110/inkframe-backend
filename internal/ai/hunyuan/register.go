package hunyuan

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/ai/openai"
)

func init() {
	// "hunyuan" 根据 ModelType 分发：
	//   image → HunyuanImageProvider（专属图像生成接口）
	//   其他  → OpenAI 兼容接口（TokenHub 新一代混元 LLM）
	ai.Register("hunyuan", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		if cfg.ModelType == "image" {
			return NewHunyuanImageProvider(cfg.APIKey, cfg.APIEndpoint), nil
		}
		return openai.NewHunyuanProvider(cfg.APIKey, cfg.APIEndpoint, cfg.ModelName, cfg.Timeout), nil
	})
}
