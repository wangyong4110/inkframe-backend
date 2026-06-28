package service

// ProviderStaticModelsByType 按提供商名称 → 模型类型 → 模型 ID 列表的静态映射。
// 这是 static_models 字段数据的唯一来源；DB 列已废弃并将被删除。
var ProviderStaticModelsByType = map[string]map[string][]string{
	"openai": {
		"llm":       {"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "o3", "o3-mini", "o1", "o1-mini"},
		"image":     {"dall-e-3", "dall-e-2", "gpt-image-1"},
		"embedding": {"text-embedding-3-large", "text-embedding-3-small", "text-embedding-ada-002"},
		"voice":     {"tts-1", "tts-1-hd"},
	},
	"anthropic": {
		"llm": {"claude-opus-4-7", "claude-opus-4-5", "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-haiku-4-5-20251001", "claude-3-7-sonnet-20250219", "claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022", "claude-3-opus-20240229"},
	},
	"google": {
		"llm": {"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.0-flash", "gemini-2.0-flash-lite", "gemini-1.5-pro", "gemini-1.5-flash"},
	},
	"xai": {
		"llm": {"grok-3", "grok-3-mini", "grok-3-fast", "grok-2", "grok-2-vision"},
	},
	"mistral": {
		"llm": {"mistral-large-latest", "mistral-small-latest", "codestral-latest", "open-mistral-nemo"},
	},
	"meta": {
		"llm": {"Llama-4-Scout-17B-16E-Instruct", "Llama-4-Maverick-17B-128E-Instruct", "Llama-3.3-70B-Instruct"},
	},
	"doubao": {
		"llm": {
			"doubao-pro-256k", "doubao-pro-128k", "doubao-pro-32k", "doubao-pro-4k",
			"doubao-lite-128k", "doubao-lite-32k",
			"doubao-seed-1-6", "doubao-seed-1-5",
		},
		"video": {
			"doubao-seedance-2-0-260128",
			"doubao-seedance-2-0-fast-260128",
			"doubao-seedance-2-0-mini-260615",
			"doubao-seedance-1-5-pro-251215",
			"doubao-seedance-1-0-pro-250528",
			"doubao-seedance-1-0-pro-fast-251015",
		},
	},
	"deepseek": {
		"llm": {"deepseek-chat", "deepseek-reasoner"},
	},
	"qianwen": {
		"llm": {
			"qwen-max", "qwen-plus", "qwen-turbo", "qwen-long",
			"qwen3-235b-a22b", "qwen3-32b", "qwen3-14b", "qwen3-8b",
			"qwen2.5-72b-instruct", "qwen2.5-32b-instruct",
		},
		"image": {"wanx2.1-t2i-plus", "wanx2.1-t2i-turbo", "wanx-x-v1"},
		"video": {"wanx2.1-i2v-plus", "wanx2.1-i2v-turbo"},
		"voice": {"cosyvoice-v2-0.5b", "cosyvoice-v1-5b"},
	},
	"zhipu": {
		"llm": {"glm-4-plus", "glm-4-air", "glm-4-flash", "glm-z1-plus", "glm-z1-air"},
	},
	"moonshot": {
		"llm": {"kimi-k2-0711-preview", "moonshot-v1-128k", "moonshot-v1-32k", "moonshot-v1-8k"},
	},
	"baidu": {
		"llm": {"ernie-4.5-turbo-128k", "ernie-4.5-8k", "ernie-3.5-128k", "ernie-speed-128k"},
	},
	"tencent": {
		"llm": {"hunyuan-turbos-latest", "hunyuan-large", "hunyuan-standard-256k"},
	},
	"hunyuan": {
		"llm":   {"hy3-preview"},
		"image": {"hy-image-lite", "hy-image-v3.0"},
	},
	"yi": {
		"llm": {"yi-lightning", "yi-large", "yi-medium"},
	},
	"volcengine-visual": {
		"image": {"general_v3.0", "general_v2.1", "general_v1.4"},
		"video": {"general_v3.0-I2V"},
	},
	"kling": {
		"video": {"kling-v1-6", "kling-v1-5", "kling-v1"},
		"image": {"kling-v1-6", "kling-v1-5", "kling-v1"},
		"sfx":   {"kling-v1"},
	},
	"doubao-speech": {
		"voice": {"seed-tts-2.0", "seed-tts-1.0"},
	},
	"doubao-speech-v1": {
		"voice": {"BV001_streaming", "BV002_streaming"},
	},
	"baidu-tts": {
		"voice": {"0", "1", "3", "4", "5", "103", "106", "110", "111"},
	},
	"minimax-tts": {
		"voice": {"female-shaonv", "female-yujie", "male-qn-qingse", "male-qn-jingying"},
	},
	"tencent-tts": {
		"voice": {"101001", "101002", "101011", "101012"},
	},
	"elevenlabs-sfx": {
		"sfx": {"sound-generation"},
	},
}

// FlattenStaticModels returns a deduplicated flat list of all models for the given provider.
func FlattenStaticModels(providerName string) []string {
	byType, ok := ProviderStaticModelsByType[providerName]
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	var result []string
	for _, models := range byType {
		for _, m := range models {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
				result = append(result, m)
			}
		}
	}
	return result
}

// providerModelDef 内置的提供商模型定义，用于租户创建供应商时自动初始化模型列表。
// 这是单一数据来源 — seed.go 不再为 tenant_id=0 的系统供应商写入 ink_ai_model 记录。
type providerModelDef struct {
	Name        string
	DisplayName string
	Type        string
	Quality     float64
	MaxTokens   int
}

// defaultProviderModels 按供应商名称索引的默认模型列表。
var defaultProviderModels = map[string][]providerModelDef{
	"openai": {
		{"gpt-4o", "GPT-4o", "llm", 0.95, 16384},
		{"gpt-4o-mini", "GPT-4o Mini", "llm", 0.85, 16384},
		{"dall-e-3", "DALL-E 3", "image", 0.95, 0},
	},
	// Azure uses deployment-based naming — model names are account-specific deployment names
	// configured in Azure Portal; no generic defaults can be pre-seeded here.
	// Users must add models manually via the "添加模型" button.
	"azure": {},
	"anthropic": {
		{"claude-opus-4-5", "Claude Opus 4.5", "llm", 0.98, 8192},
		{"claude-sonnet-4-5", "Claude Sonnet 4.5", "llm", 0.96, 8192},
		{"claude-haiku-4-5-20251001", "Claude Haiku 4.5", "llm", 0.90, 4096},
	},
	"google": {
		{"gemini-2.5-pro", "Gemini 2.5 Pro", "llm", 0.95, 8192},
		{"gemini-2.5-flash", "Gemini 2.5 Flash", "llm", 0.91, 8192},
		{"gemini-2.0-flash", "Gemini 2.0 Flash", "llm", 0.90, 8192},
	},
	"xai": {
		{"grok-4", "Grok 4", "llm", 0.96, 8192},
		{"grok-4-0709", "Grok 4 0709", "llm", 0.95, 8192},
		{"grok-3-mini", "Grok 3 Mini", "llm", 0.87, 4096},
		{"grok-3-mini-fast", "Grok 3 Mini Fast", "llm", 0.85, 4096},
	},
	"mistral": {
		{"mistral-large-latest", "Mistral Large", "llm", 0.93, 8192},
		{"mistral-medium-latest", "Mistral Medium", "llm", 0.88, 4096},
		{"mistral-small-latest", "Mistral Small", "llm", 0.82, 4096},
	},
	"meta": {
		{"Llama-4-Scout-17B-16E-Instruct-FP8", "Llama 4 Scout", "llm", 0.88, 8192},
		{"Llama-4-Maverick-17B-128E-Instruct-FP8", "Llama 4 Maverick", "llm", 0.92, 8192},
		{"Llama-3.3-70B-Instruct", "Llama 3.3 70B", "llm", 0.87, 8192},
	},
	"doubao": {
		{"doubao-pro-32k", "豆包 Pro 32K", "llm", 0.88, 16384},
		{"doubao-lite-32k", "豆包 Lite 32K", "llm", 0.75, 16384},
		{"seedream-3-0-t2i-250415", "Seedream 3.0 文生图", "image", 0.9, 0},
		// 视频模型（Seedance 系列）：model 字段填写 Model ID 或火山引擎控制台的推理接入点 Endpoint ID
		{"doubao-seedance-2-0-260128", "Seedance 2.0（多模态 T2V/I2V）", "video", 0.95, 0},
		{"doubao-seedance-2-0-fast-260128", "Seedance 2.0 Fast", "video", 0.90, 0},
		{"doubao-seedance-2-0-mini-260615", "Seedance 2.0 Mini", "video", 0.85, 0},
		{"doubao-seedance-1-5-pro-251215", "Seedance 1.5 Pro（支持草稿模式）", "video", 0.92, 0},
		{"doubao-seedance-1-0-pro-250528", "Seedance 1.0 Pro", "video", 0.88, 0},
		{"doubao-seedance-1-0-pro-fast-251015", "Seedance 1.0 Pro Fast", "video", 0.85, 0},
		// 多模态 Embedding 模型（EmbedMultimodal，支持文本+图片+视频混合输入）
		{"doubao-embedding-vision-250328", "豆包多模态 Embedding", "embedding", 0.90, 0},
		{"doubao-embedding-vision-250615", "豆包多模态 Embedding v2（支持维度选择）", "embedding", 0.92, 0},
	},
	"deepseek": {
		{"deepseek-v4-pro", "DeepSeek V4 Pro", "llm", 0.96, 32768},
		{"deepseek-v4-flash", "DeepSeek V4 Flash", "llm", 0.90, 32768},
		{"deepseek-chat", "DeepSeek V3", "llm", 0.90, 16384},
		{"deepseek-reasoner", "DeepSeek R1", "llm", 0.94, 8192},
	},
	"qianwen": {
		{"qwen3-max", "Qwen3 Max", "llm", 0.93, 8192},
		{"qwen3-plus", "Qwen3 Plus", "llm", 0.88, 4096},
		{"qwen-max", "通义千问 Max", "llm", 0.92, 4096},
		{"wanx2.1-t2i-turbo", "万象 2.1 文生图 Turbo", "image", 0.85, 0},
		{"happyhorse-1.1-r2v", "HappyHorse 1.1 参考生视频（多图）", "video", 0.93, 0},
		{"happyhorse-1.0-r2v", "HappyHorse 1.0 参考生视频（多图）", "video", 0.88, 0},
		{"happyhorse-1.1-i2v", "HappyHorse 1.1 图生视频（首帧）", "video", 0.92, 0},
		{"happyhorse-1.0-i2v", "HappyHorse 1.0 图生视频（首帧）", "video", 0.87, 0},
		{"happyhorse-1.1-t2v", "HappyHorse 1.1 文生视频", "video", 0.9, 0},
		{"happyhorse-1.0-t2v", "HappyHorse 1.0 文生视频", "video", 0.85, 0},
	},
	"zhipu": {
		{"glm-4-plus", "GLM-4 Plus", "llm", 0.90, 8192},
		{"glm-4-flash", "GLM-4 Flash", "llm", 0.82, 4096},
		{"glm-4-air", "GLM-4 Air", "llm", 0.84, 4096},
		{"glm-z1-flash", "GLM-Z1 Flash", "llm", 0.85, 4096},
	},
	"moonshot": {
		{"kimi-k2-0711-preview", "Kimi K2", "llm", 0.93, 8192},
		{"moonshot-v1-128k", "Kimi 128K", "llm", 0.88, 8192},
		{"moonshot-v1-32k", "Kimi 32K", "llm", 0.86, 4096},
	},
	"baidu": {
		{"ernie-4.5-8k", "ERNIE 4.5", "llm", 0.89, 4096},
		{"ernie-4.5-128k", "ERNIE 4.5 128K", "llm", 0.89, 8192},
		{"ernie-3.5-8k", "ERNIE 3.5", "llm", 0.84, 4096},
		{"ernie-speed-128k", "ERNIE Speed 128K", "llm", 0.78, 4096},
	},
	"tencent": {
		{"hunyuan-turbo", "混元 Turbo", "llm", 0.91, 8192},
		{"hunyuan-pro", "混元 Pro", "llm", 0.89, 4096},
		{"hunyuan-lite", "混元 Lite", "llm", 0.80, 4096},
	},
	"hunyuan": {
		// Hy3 Preview：MoE 架构，256k 上下文，支持深度思考（thinking/reasoning_effort）
		{"hy3-preview", "混元 Hy3 Preview", "llm", 0.95, 32768},
		// 混元生图极速版：同步，快速出图
		{"hy-image-lite", "混元生图（极速版）", "image", 0.88, 0},
		// 混元生图 3.0：异步高质量，支持参考图，中文理解强
		{"hy-image-v3.0", "混元生图（3.0）", "image", 0.93, 0},
	},
	"yi": {
		{"yi-lightning", "Yi Lightning", "llm", 0.88, 4096},
		{"yi-large", "Yi Large", "llm", 0.87, 4096},
		{"yi-large-turbo", "Yi Large Turbo", "llm", 0.85, 4096},
	},
	"volcengine-visual": {
		{"jimeng_seedream46_cvtob", "即梦4.6 图像生成（人像写真/平面设计/风格化，支持多图输入输出）", "image", 0.97, 0},
		{"jimeng_i2i_v30", "即梦图生图3.0智能参考（编辑指令/真实图像/海报设计）", "image", 0.94, 0},
		{"jimeng_t2i_v31", "即梦文生图3.1（画面美感/风格精准/细节丰富）", "image", 0.94, 0},
		{"jimeng_t2i_v30", "即梦文生图3.0（文字排版/人像质感/艺术字体）", "image", 0.93, 0},
		{"jimeng_t2i_v40", "即梦4.0 图像生成（文生图/图像编辑，支持多图输入输出）", "image", 0.96, 0},
		{"general_v3.0", "即梦AI 文生图 V3", "image", 0.9, 0},
		{"jimeng_ti2v_v30_pro", "即梦视频3.0 Pro（文生视频+图生视频首帧，1080P高清）", "video", 0.95, 0},
		{"jimeng_t2v_v30", "即梦视频3.0 文生视频", "video", 0.92, 0},
		{"jimeng_t2v_v30_1080p", "即梦视频3.0 文生视频 1080P", "video", 0.93, 0},
		{"jimeng_i2v_first_v30", "即梦视频3.0 图生视频（首帧）", "video", 0.92, 0},
		{"jimeng_i2v_first_v30_1080", "即梦视频3.0 图生视频（首帧）1080P", "video", 0.93, 0},
		{"jimeng_i2v_first_tail_v30", "即梦视频3.0 图生视频（首尾帧）", "video", 0.92, 0},
		{"jimeng_i2v_first_tail_v30_1080", "即梦视频3.0 图生视频（首尾帧）1080P", "video", 0.93, 0},
		{"jimeng_i2v_recamera_v30", "即梦视频3.0 图生视频-运镜（镜头运动控制）", "video", 0.93, 0},
	},
	"kling": {
		{"kling-3.0-turbo", "可灵 3.0 Turbo", "video", 0.97, 0},
		{"kling-v1-6", "可灵 v1.6", "video", 0.9, 0},
		{"3s", "可灵音效 3s", "sfx", 0.88, 0},
		{"5s", "可灵音效 5s", "sfx", 0.90, 0},
		{"7s", "可灵音效 7s", "sfx", 0.89, 0},
		{"10s", "可灵音效 10s", "sfx", 0.88, 0},
		{"kling-v1", "可灵图像 v1", "image", 0.88, 0},
		{"kling-v1-5", "可灵图像 v1.5", "image", 0.90, 0},
		{"kling-v2", "可灵图像 v2", "image", 0.92, 0},
		{"kling-v2-1", "可灵图像 v2.1", "image", 0.93, 0},
		{"kling-v3", "可灵图像 v3", "image", 0.95, 0},
	},
	"doubao-speech": {
		{"seed-tts-2.0", "豆包 Seed-TTS 2.0", "voice", 0.92, 0},
		{"seed-tts-1.0", "豆包 Seed-TTS 1.0", "voice", 0.88, 0},
	},
	"doubao-speech-v1": {
		{"zh_female_vv_uranus_bigtts", "Vivi 2.0", "voice", 0.92, 0},
		{"zh_female_xiaohe_uranus_bigtts", "小何 2.0", "voice", 0.92, 0},
		{"zh_male_m191_uranus_bigtts", "云舟 2.0", "voice", 0.91, 0},
		{"zh_male_taocheng_uranus_bigtts", "小天 2.0", "voice", 0.91, 0},
	},
	"baidu-tts": {
		{"0", "度小美", "voice", 0.85, 0},
		{"1", "度小宇", "voice", 0.85, 0},
		{"3", "度逍遥", "voice", 0.85, 0},
		{"4", "度丫丫", "voice", 0.84, 0},
	},
	"minimax-tts": {
		{"female-shaonv", "少女音色", "voice", 0.88, 0},
		{"female-yujie", "御姐音色", "voice", 0.88, 0},
		{"male-qn-qingse", "青涩青年音色", "voice", 0.88, 0},
		{"male-qn-jingying", "精英青年音色", "voice", 0.88, 0},
	},
	"tencent-tts": {
		{"101001", "智言（男）", "voice", 0.87, 0},
		{"101002", "智雅（女）", "voice", 0.87, 0},
		{"101011", "智燕（女）", "voice", 0.86, 0},
		{"101012", "智丹（女）", "voice", 0.86, 0},
	},
	"elevenlabs-sfx": {
		{"sound-generation", "ElevenLabs 音效生成", "sfx", 0.90, 0},
	},
}
