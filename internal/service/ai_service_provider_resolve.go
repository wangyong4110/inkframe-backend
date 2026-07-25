package service

import (
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/ai/aliyun"
	"github.com/inkframe/inkframe-backend/internal/ai/azure"
	"github.com/inkframe/inkframe-backend/internal/ai/claude"
	"github.com/inkframe/inkframe-backend/internal/ai/doubao"
	"github.com/inkframe/inkframe-backend/internal/ai/kling"
	"github.com/inkframe/inkframe-backend/internal/commons"
	"github.com/inkframe/inkframe-backend/internal/crypto"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

func (s *AIService) getTenantProvider(tenantId uint, targetType commons.ModelType, selectedModel string) (ai.AIProvider, *model.AIModel, error) {
	models, err := s.modelRepo.ListByTenantAndType(tenantId, targetType)
	if err != nil {
		return nil, nil, err
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("model not configured")
	}
	var m *model.AIModel
	for _, model := range models {
		if !model.IsActive {
			continue
		}
		if selectedModel == "" || selectedModel == model.Name {
			m = model
			break
		}
	}
	p, err := s.toAIProvider(m)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, fmt.Errorf("AI provider not found")
	}
	return p, m, err
}

func (s *AIService) toAIProvider(m *model.AIModel) (ai.AIProvider, error) {
	apiKey := m.Provider.APIKey
	apiSecretKey := m.Provider.APISecretKey
	endpoint := m.Provider.APIEndpoint
	modelName := m.Name
	timeout := time.Duration(m.Timeout) * time.Second
	matched := m.Provider

	var provider ai.AIProvider
	switch m.Provider.Name {
	case ai.ProviderNameVolcengineVisual:
		provider = ai.NewVolcengineVisualProvider(apiKey, apiSecretKey)
	case "kling-sfx":
		provider = kling.NewKlingSFXProvider(apiKey, apiSecretKey, endpoint)
	case "kling-tts":
		provider = kling.NewKlingTTSProvider(apiKey, apiSecretKey, endpoint)
	case "kling-image":
		provider = kling.NewKlingImageProvider(apiKey, apiSecretKey, endpoint)
	case "elevenlabs-sfx":
		provider = ai.NewElevenLabsSFXProvider(apiKey, endpoint)
	case "aliyun-tts":
		provider = aliyun.NewAliyunTTSProvider(apiKey, endpoint)
	case "qwen-tts":
		provider = ai.NewQwenTTSProvider(apiKey, endpoint)
	case "fun-music":
		provider = ai.NewFunMusicProvider(apiKey)
	case "minimax-music":
		provider = ai.NewMinimaxMusicProvider(apiKey)
	case "openai", "openai-image":
		provider = ai.NewOpenAIProvider(apiKey, endpoint, modelName, timeout)
	case "anthropic":
		provider = claude.NewAnthropicProvider(apiKey, endpoint, modelName, timeout)
	case "google":
		provider = claude.NewGoogleProvider(apiKey, endpoint, modelName, timeout)
	case "doubao", "volcengine-ark-img":
		// "volcengine-ark-img" 是 DB 中 Seedream 图片模型的自定义名称，使用相同的 DoubaoProvider
		logger.Printf("getTenantProvider: provider %q → DoubaoProvider endpoint=%s model=%s", matched.Name, matched.APIEndpoint, modelName)
		provider = doubao.NewDoubaoProvider(apiKey, endpoint, modelName, timeout)
	case "doubao-speech":
		// APIKey = X-Api-Key, APIVersion = resourceID（如 "seed-tts-2.0"）
		provider = doubao.NewDoubaoSpeechProvider(apiKey, matched.APIVersion)
	case "doubao-speech-v1":
		// APIKey = appID, APISecretKey = access_token, APIVersion = cluster（默认 volcano_tts）
		provider = doubao.NewDoubaoSpeechV1Provider(apiKey, apiSecretKey, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "qianwen":
		switch m.Type {
		case "voice":
			provider = ai.NewQianwenTTSRouter(apiKey, matched.APIEndpoint)
		case "video":
			return nil, fmt.Errorf("provider %q is a video provider; use GetTenantVideoProvider", matched.Name)
		default:
			provider = ai.NewQianwenProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		}
	case "hunyuan":
		// TokenHub 新一代混元 API：LLM 用 OpenAI 兼容接口，图像用专属接口
		if m.Type == "image" {
			provider = ai.NewHunyuanImageProvider(apiKey, matched.APIEndpoint)
		} else {
			provider = ai.NewHunyuanProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		}
	case "azure":
		// APIEndpoint = Azure resource endpoint; APIVersion = REST API version ("2025-01-01-preview")
		// Deployment name is resolved at call time from req.Model (AIModel.Name).
		provider = azure.NewAzureProvider(apiKey, matched.APIEndpoint, "", matched.APIVersion, timeout)
	default:

	}
	return provider, nil
}

// CheckAvailability 检查指定租户是否有可用的 LLM 提供商（用于 pipeline 预检）
func (s *AIService) CheckAvailability(tenantID uint) error {
	_, _, err := s.getTenantProvider(tenantID, commons.LLM, "")
	return err
}

// eligibleProviders 从 DB 加载指定类型（"image"/"video"/"voice"/"sfx"/...）的提供者，
// 过滤掉未激活或缺少凭据的。这是 loadDBImageProviderEntries/loadDBVoiceProvider/
// GetTenantVideoProvider/GetTenantLipSyncProvider/ListCapableProviders 共用的第一步——
// 过滤之后"怎么从候选里选一个"（按 voiceID 匹配、按 provider 名称偏好排序等）由各自的
// 调用方决定，不同类型的选择规则差异较大，不适合也塞进这个共享函数里。
// 注意：getTenantProvider 不复用这个函数——它对"没有凭据"的处理是降级容忍而非硬性跳过
// （租户级/系统级都找不到有凭据的才退而求其次接受无凭据的），语义上不是同一个过滤条件。
func (s *AIService) eligibleProviders(tenantID uint, modelType commons.ModelType) ([]*model.ModelProvider, error) {
	providers, err := s.providerRepo.ListByModelType(tenantID, string(modelType))
	if err != nil {
		return nil, err
	}
	var result []*model.ModelProvider
	for _, p := range providers {
		if !p.IsActive {
			logger.Printf("eligibleProviders: skip %s provider %q (inactive)", modelType, p.Name)
			continue
		}
		if !providerHasCredentials(p) {
			logger.Printf("eligibleProviders: skip %s provider %q (missing credentials)", modelType, p.Name)
			continue
		}
		result = append(result, p)
	}
	return result, nil
}

// loadDBVoiceProvider 按 voiceID 从内置音色表优先匹配，未命中则取第一个有效 provider。
// voiceID 为空时退化为 loadDBProviderByType 行为。
// 返回 provider、提供商名称和错误。
func (s *AIService) loadDBVoiceProvider(tenantID uint, modelType commons.ModelType, voiceID string) (ai.AIProvider, error) {
	logger.Printf("[TTS] loadDBVoiceProvider: tenantID=%d modelType=%q voiceID=%q", tenantID, modelType, voiceID)
	providers, err := s.eligibleProviders(tenantID, modelType)
	if err != nil {
		logger.Errorf("[TTS] loadDBVoiceProvider: ERROR ListByModelType tenantID=%d modelType=%q: %v", tenantID, modelType, err)
		return nil, err
	}
	logger.Printf("[TTS] loadDBVoiceProvider: %d eligible providers of type %q for tenantID=%d", len(providers), modelType, tenantID)

	// 按 voiceID 打优先级
	type candidate struct {
		p        *model.ModelProvider
		priority int // 0=voice匹配, 1=无匹配/voiceID为空
	}
	var candidates []candidate
	for _, p := range providers {
		pri := 1
		if voiceID != "" {
			for _, v := range model.BuiltinVoices(p.Name) {
				if v.ID == voiceID {
					pri = 0
					break
				}
			}
		}
		if voiceID != "" {
			if pri == 0 {
				logger.Printf("[TTS] loadDBVoiceProvider: provider %q has builtin voice %q (priority=0/voice-match)", p.Name, voiceID)
			} else {
				logger.Printf("[TTS] loadDBVoiceProvider: provider %q does NOT have builtin voice %q (priority=1/fallback)", p.Name, voiceID)
			}
		}
		candidates = append(candidates, candidate{p, pri})
	}

	if len(candidates) == 0 {
		logger.Errorf("[TTS] loadDBVoiceProvider: ERROR no active credentialed %s providers found for tenantID=%d", modelType, tenantID)
		return nil, fmt.Errorf("no %s providers configured in DB", modelType)
	}

	// 先取 priority=0（voice 匹配），再取 priority=1（兜底）
	for _, pass := range []int{0, 1} {
		for _, c := range candidates {
			if c.priority != pass {
				continue
			}
			provider, _, err := s.getTenantProvider(tenantID, modelType, c.p.Name)
			if err != nil {
				logger.Errorf("[TTS] loadDBVoiceProvider: ERROR instantiate %s provider %q: %v", modelType, c.p.Name, err)
				continue
			}
			logger.Printf("[TTS] loadDBVoiceProvider: selected %s provider %q (voice=%q priority=%d)", modelType, c.p.Name, voiceID, pass)
			return provider, nil
		}
	}
	logger.Errorf("[TTS] loadDBVoiceProvider: ERROR all %d candidates failed to instantiate for modelType=%q voiceID=%q", len(candidates), modelType, voiceID)
	return nil, fmt.Errorf("no %s providers configured in DB", modelType)
}

// GetTenantVideoProvider 从 DB 中查找指定租户已配置的视频生成提供商。
// name 为空时返回第一个可用的视频提供商（kling 优先）。
func (s *AIService) GetTenantVideoProvider(tenantID uint, modelName string) (ai.VideoProvider, error) {
	m, err := s.modelRepo.GetByName(modelName)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("video model %s not found", modelName)
	}
	p := m.Provider
	// Decrypt stored credentials before passing to provider constructors.
	apiKey, err := crypto.Decrypt(p.APIKey, s.encKey)
	if err != nil {
		logger.Errorf("GetTenantVideoProvider: decrypt APIKey for %q: %v", p.Name, err)
		return nil, err
	}
	apiSecretKey, err := crypto.Decrypt(p.APISecretKey, s.encKey)
	if err != nil {
		logger.Errorf("GetTenantVideoProvider: decrypt APISecretKey for %q: %v", p.Name, err)
		return nil, err
	}
	switch p.Name {
	case "volcengine-visual":
		// volcengine-visual 合并了 jimeng-video（即梦视频）
		return ai.NewJimengVideoProvider(apiKey, apiSecretKey), nil
	case "jimeng-video":
		return ai.NewJimengVideoProvider(apiKey, apiSecretKey), nil
	case "kling":
		return kling.NewKlingProvider(apiKey, apiSecretKey, p.APIEndpoint), nil
	case "seedance", "doubao":
		return doubao.NewDoubaoVideoProvider(apiKey, p.APIEndpoint), nil
	case "minimax-video":
		return ai.NewMinimaxVideoProvider(apiKey), nil
	case "happyhorse":
		return ai.NewHappyHorseProvider(apiKey, p.APIEndpoint), nil
	case "qianwen":
		// qianwen 合并了 happyhorse（DashScope 视频生成）
		return ai.NewHappyHorseProvider(apiKey, p.APIEndpoint), nil
	}
	return nil, fmt.Errorf("no video provider configured for tenant %d", tenantID)
}
