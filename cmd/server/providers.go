package main

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/vector"
)

// initAIModule 初始化AI模块（兜底层）
// 生产环境：租户通过模型管理页面配置各自的 AK/SK，env var 不需要设置。
// 开发/测试：设置 OPENAI_API_KEY 等 env var 可跳过 DB 配置直接使用。
// 仅注册 key 非空的 provider，避免用空 key 发起 API 请求返回 401。
func initAIModule(cfg *config.Config) *ai.ModelManager {
	manager := ai.NewModelManager()
	firstRegistered := ""

	type providerDef struct {
		name     string
		key      string
		endpoint string
		model    string
		factory  func(key, endpoint, model string) ai.AIProvider
	}
	// imageProviderModels 记录各提供者用于图像生成的模型和尺寸
	type imageProviderMeta struct{ model, size string }
	imageProviders := map[string]imageProviderMeta{
		"openai":  {"dall-e-3", "1024x1024"},
		"doubao":  {"seedream-3-0-t2i-250415", "1024x1024"},
		"qianwen": {"wanx2.1-t2i-turbo", "1024x1024"},
	}

	// env var 优先，config.yaml 作为备选（两者均可配置 API key）
	defs := []providerDef{
		// 国际
		{"openai", getEnv("OPENAI_API_KEY", cfg.AI.OpenAI.APIKey), cfg.AI.OpenAI.Endpoint, cfg.AI.OpenAI.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewOpenAIProvider(k, e, m, 0) }},
		{"anthropic", getEnv("ANTHROPIC_API_KEY", cfg.AI.Anthropic.APIKey), cfg.AI.Anthropic.Endpoint, cfg.AI.Anthropic.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewAnthropicProvider(k, e, m, 0) }},
		{"google", getEnv("GOOGLE_API_KEY", cfg.AI.Google.APIKey), cfg.AI.Google.Endpoint, cfg.AI.Google.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewGoogleProvider(k, e, m, 0) }},
		{"xai", getEnv("XAI_API_KEY", cfg.AI.XAI.APIKey), cfg.AI.XAI.Endpoint, cfg.AI.XAI.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewXAIProvider(k, e, m, 0) }},
		{"mistral", getEnv("MISTRAL_API_KEY", cfg.AI.Mistral.APIKey), cfg.AI.Mistral.Endpoint, cfg.AI.Mistral.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewMistralProvider(k, e, m, 0) }},
		{"meta", getEnv("META_API_KEY", cfg.AI.Meta.APIKey), cfg.AI.Meta.Endpoint, cfg.AI.Meta.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewMetaProvider(k, e, m, 0) }},
		// 国内
		{"doubao", getEnv("DOUBAO_API_KEY", cfg.AI.Doubao.APIKey), cfg.AI.Doubao.Endpoint, cfg.AI.Doubao.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewDoubaoProvider(k, e, m, 0) }},
		{"deepseek", getEnv("DEEPSEEK_API_KEY", cfg.AI.DeepSeek.APIKey), cfg.AI.DeepSeek.Endpoint, cfg.AI.DeepSeek.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewDeepSeekProvider(k, e, m, 0) }},
		{"qianwen", getEnv("QIANWEN_API_KEY", cfg.AI.Qianwen.APIKey), cfg.AI.Qianwen.Endpoint, cfg.AI.Qianwen.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewQianwenProvider(k, e, m, 0) }},
		{"zhipu", getEnv("ZHIPU_API_KEY", cfg.AI.Zhipu.APIKey), cfg.AI.Zhipu.Endpoint, cfg.AI.Zhipu.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewZhipuProvider(k, e, m, 0) }},
		{"moonshot", getEnv("MOONSHOT_API_KEY", cfg.AI.Moonshot.APIKey), cfg.AI.Moonshot.Endpoint, cfg.AI.Moonshot.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewMoonshotProvider(k, e, m, 0) }},
		{"baidu", getEnv("BAIDU_API_KEY", cfg.AI.Baidu.APIKey), cfg.AI.Baidu.Endpoint, cfg.AI.Baidu.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewBaiduProvider(k, e, m, 0) }},
		{"tencent", getEnv("TENCENT_API_KEY", cfg.AI.Tencent.APIKey), cfg.AI.Tencent.Endpoint, cfg.AI.Tencent.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewTencentProvider(k, e, m, 0) }},
		{"yi", getEnv("YI_API_KEY", cfg.AI.Yi.APIKey), cfg.AI.Yi.Endpoint, cfg.AI.Yi.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewYiProvider(k, e, m, 0) }},
	}
	for _, d := range defs {
		if d.key == "" {
			continue
		}
		manager.RegisterProvider(d.name, d.factory(d.key, d.endpoint, d.model))
		if firstRegistered == "" {
			firstRegistered = d.name
		}
		// 注册图像生成能力（仅当该 provider 实际可用时）
		if meta, ok := imageProviders[d.name]; ok {
			manager.RegisterImageProvider(d.name, meta.model, meta.size)
		}
	}
	if firstRegistered != "" {
		manager.SetDefault(firstRegistered)
	}
	if len(manager.ListProviders()) == 0 {
		logger.Println("initAIModule: no AI API keys in env — providers will be loaded from DB per-tenant")
	}

	// Ollama 本地 LLM（无需 API Key，endpoint 非空即注册）
	// 设置 OLLAMA_ENDPOINT 或在 config.yaml 中配置 ai.ollama.endpoint
	ollamaEndpoint := getEnv("OLLAMA_ENDPOINT", cfg.AI.Ollama.Endpoint)
	if ollamaEndpoint != "" {
		ollamaModel := getEnv("OLLAMA_MODEL", cfg.AI.Ollama.Model)
		manager.RegisterProvider("ollama", ai.NewOllamaProvider(ollamaEndpoint, ollamaModel, 0))
		if firstRegistered == "" {
			firstRegistered = "ollama"
		}
		logger.Printf("initAIModule: registered ollama at %s (model=%s)", ollamaEndpoint, ollamaModel)
	}

	// 即梦AI Visual API（AK/SK 鉴权图像生成）
	if vvp := initVolcengineVisual(cfg); vvp != nil {
		manager.RegisterProvider("volcengine-visual", vvp)
		manager.RegisterImageProvider("volcengine-visual", ai.VolcModelText2ImgV3, "1328x1328")
	}

	// 豆包语音合成 V3（openspeech.bytedance.com，X-Api-Key 鉴权，支持 seed-tts-2.0）
	if speechKey := getEnv("DOUBAO_SPEECH_API_KEY", ""); speechKey != "" {
		resourceID := getEnv("DOUBAO_SPEECH_RESOURCE_ID", "")
		manager.RegisterProvider("doubao-speech", ai.NewDoubaoSpeechProvider(speechKey, resourceID))
	}

	// 豆包语音合成 V1（HTTP 一次性合成，appid+access_token 鉴权，火山引擎老版控制台）
	if v1AppID := getEnv("DOUBAO_SPEECH_V1_APP_ID", ""); v1AppID != "" {
		v1Token := getEnv("DOUBAO_SPEECH_V1_TOKEN", "")
		v1Cluster := getEnv("DOUBAO_SPEECH_V1_CLUSTER", "")
		manager.RegisterProvider("doubao-speech-v1", ai.NewDoubaoSpeechV1Provider(v1AppID, v1Token, v1Cluster))
	}

	// 百度智能云语音合成（API Key + Secret Key OAuth 鉴权）
	if baiduAPIKey := getEnv("BAIDU_TTS_API_KEY", ""); baiduAPIKey != "" {
		baiduSecretKey := getEnv("BAIDU_TTS_SECRET_KEY", "")
		manager.RegisterProvider("baidu-tts", ai.NewBaiduTTSProvider(baiduAPIKey, baiduSecretKey))
	}

	// MiniMax 语音合成（Bearer Token + GroupID）
	if minimaxKey := getEnv("MINIMAX_TTS_API_KEY", ""); minimaxKey != "" {
		minimaxGroupID := getEnv("MINIMAX_TTS_GROUP_ID", "")
		manager.RegisterProvider("minimax-tts", ai.NewMinimaxTTSProvider(minimaxKey, minimaxGroupID))
	}

	// 阿里云 CosyVoice 语音合成（DashScope API Key）
	if aliyunKey := getEnv("ALIYUN_TTS_API_KEY", ""); aliyunKey != "" {
		manager.RegisterProvider("aliyun-tts", ai.NewAliyunTTSProvider(aliyunKey))
	}

	// 腾讯云语音合成（SecretId + SecretKey TC3 鉴权）
	if tencentSecretID := getEnv("TENCENT_TTS_SECRET_ID", ""); tencentSecretID != "" {
		tencentSecretKey := getEnv("TENCENT_TTS_SECRET_KEY", "")
		tencentRegion := getEnv("TENCENT_TTS_REGION", "")
		manager.RegisterProvider("tencent-tts", ai.NewTencentTTSProvider(tencentSecretID, tencentSecretKey, tencentRegion))
	}

	// 可灵文生音效（AK/SK JWT 鉴权）
	klingAK := getEnv("KLING_ACCESS_KEY", cfg.AI.Kling.APIKey)
	klingSK := getEnv("KLING_SECRET_KEY", cfg.AI.Kling.SecretKey)
	if klingAK != "" && klingSK != "" {
		manager.RegisterProvider("kling-sfx", ai.NewKlingSFXProvider(klingAK, klingSK, cfg.AI.Kling.Endpoint))
		manager.RegisterProvider("kling-tts", ai.NewKlingTTSProvider(klingAK, klingSK, cfg.AI.Kling.Endpoint))
		manager.RegisterProvider("kling-image", ai.NewKlingImageProvider(klingAK, klingSK, cfg.AI.Kling.Endpoint))
	}

	// 为所有 Provider 包装指数退避重试（最多 3 次，基础延迟 500ms）
	for _, name := range manager.ListProviders() {
		if err := manager.WrapWithRetry(name, 3, 500*time.Millisecond); err != nil {
			logger.Printf("Warning: failed to wrap provider %s with retry: %v", name, err)
		}
	}

	return manager
}

// initVideoProviders 初始化视频生成提供者
// 返回可用的 VideoProvider 列表，供视频服务按需选用
func initVideoProviders(cfg *config.Config) map[string]ai.VideoProvider {
	providers := make(map[string]ai.VideoProvider)

	klingAK := getEnv("KLING_ACCESS_KEY", cfg.AI.Kling.APIKey)
	klingSK := getEnv("KLING_SECRET_KEY", cfg.AI.Kling.SecretKey)
	if klingAK != "" && klingSK != "" {
		providers["kling"] = ai.NewKlingProvider(klingAK, klingSK, cfg.AI.Kling.Endpoint)
	}

	// Seedance 字节跳动火山引擎
	seedanceKey := getEnv("SEEDANCE_API_KEY", cfg.AI.Seedance.APIKey)
	if seedanceKey != "" {
		providers["seedance"] = ai.NewSeedanceProvider(seedanceKey, cfg.AI.Seedance.Endpoint)
	}

	logger.Printf("Initialized video providers: %d registered", len(providers))
	return providers
}

// initVolcengineVisual 初始化火山引擎即梦AI图像提供者（AK/SK 鉴权）
// env var 优先，config.yaml ai.volcengine_visual 作为备选
func initVolcengineVisual(cfg *config.Config) *ai.VolcengineVisualProvider {
	ak := getEnv("VOLCENGINE_ACCESS_KEY", cfg.AI.VolcengineVisual.AccessKey)
	sk := getEnv("VOLCENGINE_SECRET_KEY", cfg.AI.VolcengineVisual.SecretKey)
	if ak == "" || sk == "" {
		return nil
	}
	logger.Println("VolcengineVisual (即梦AI) provider initialized")
	return ai.NewVolcengineVisualProvider(ak, sk)
}

// initVectorStore 初始化向量存储
// 优先使用 config.yaml 的 vector_db 配置；API Key 敏感字段走环境变量。
func initVectorStore(cfg *config.Config) *vector.StoreManager {
	manager := vector.NewStoreManager(nil)

	switch cfg.VectorDB.Type {
	case "dashvector":
		apiKey := getEnv("DASHVECTOR_API_KEY", cfg.VectorDB.APIKey)
		dashStore := vector.NewDashVectorStore(cfg.VectorDB.Endpoint, apiKey)
		manager.RegisterStore("dashvector", dashStore)
		logger.Printf("VectorStore: DashVector @ %s", cfg.VectorDB.Endpoint)

	case "chroma":
		chromaStore := vector.NewChromaStore(cfg.VectorDB.Endpoint)
		manager.RegisterStore("chroma", chromaStore)
		logger.Printf("VectorStore: Chroma @ %s", cfg.VectorDB.Endpoint)

	default: // "qdrant" 或未填，向后兼容
		endpoint := getEnv("QDRANT_ENDPOINT", cfg.VectorDB.Endpoint)
		if endpoint == "" {
			endpoint = "localhost:6333"
		}
		apiKey := getEnv("QDRANT_API_KEY", cfg.VectorDB.APIKey)
		qdrantStore := vector.NewQdrantStore(endpoint, apiKey)
		manager.RegisterStore("qdrant", qdrantStore)
		logger.Printf("VectorStore: Qdrant @ %s", endpoint)
	}

	return manager
}
