package service

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
	"azure": {
		{"gpt-4o", "GPT-4o（Azure）", "llm", 0.95, 16384},
		{"gpt-4o-mini", "GPT-4o Mini（Azure）", "llm", 0.85, 16384},
		{"gpt-4.1", "GPT-4.1（Azure）", "llm", 0.96, 32768},
		{"gpt-4.1-mini", "GPT-4.1 Mini（Azure）", "llm", 0.88, 16384},
		{"o3-mini", "o3-mini（Azure）", "llm", 0.94, 65536},
	},
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
		{"seedance-01-lite", "Seedance 01 Lite", "video", 0.88, 0},
	},
	"deepseek": {
		{"deepseek-v4", "DeepSeek V4", "llm", 0.96, 32768},
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
	"yi": {
		{"yi-lightning", "Yi Lightning", "llm", 0.88, 4096},
		{"yi-large", "Yi Large", "llm", 0.87, 4096},
		{"yi-large-turbo", "Yi Large Turbo", "llm", 0.85, 4096},
	},
	"volcengine-visual": {
		{"general_v3.0", "即梦AI 文生图 V3", "image", 0.9, 0},
		{"jimeng_t2v_v30", "即梦视频3.0 文生视频", "video", 0.92, 0},
		{"jimeng_i2v_first_v30", "即梦视频3.0 图生视频（首帧）", "video", 0.92, 0},
		{"jimeng_i2v_first_tail_v30", "即梦视频3.0 图生视频（首尾帧）", "video", 0.92, 0},
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
