package volcengine

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
)

func init() {
	// "volcengine-visual" 同时服务图像和视频模型：
	//   image → VolcengineVisualProvider（Seedream 图像生成）
	//   video → JimengVideoProvider（即梦视频生成）
	ai.Register("volcengine-visual", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		if cfg.ModelType == "video" {
			return NewJimengVideoProvider(cfg.APIKey, cfg.APISecretKey), nil
		}
		return NewVolcengineVisualProvider(cfg.APIKey, cfg.APISecretKey), nil
	})
	ai.Register("jimeng-video", func(cfg ai.ProviderConfig) (ai.ProviderMeta, error) {
		return NewJimengVideoProvider(cfg.APIKey, cfg.APISecretKey), nil
	})
}
