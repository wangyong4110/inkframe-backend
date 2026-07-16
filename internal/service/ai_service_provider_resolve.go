package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/crypto"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// GetDefaultProviderName 返回当前默认 provider 名称。
// aiManager 是一个空注册表——所有 provider 均按租户从 DB 加载（见 cmd/server/providers.go
// initAIModule 的说明"no static registration"），从未调用过 RegisterProvider，因此
// aiManager.GetProvider(...) 恒定失败，这里恒定返回 "unknown"。
func (s *AIService) GetDefaultProviderName() string {
	return "unknown"
}

// getTenantProvider 按租户加载提供商（带缓存，TTL 5 分钟）。
// targetType 为可选的模型类型提示（如 "voice"、"sfx"、"image"），用于合并型提供商（如 qianwen、kling）
// 根据类型选择正确的底层构造器。
func (s *AIService) getTenantProvider(tenantID uint, providerName string, targetType ...string) (ai.AIProvider, error) {
	if s.providerRepo == nil {
		return nil, fmt.Errorf("provider repository not configured")
	}

	tType := ""
	if len(targetType) > 0 {
		tType = strings.ToLower(targetType[0])
	}
	cacheKey := fmt.Sprintf("%d:%s:%s", tenantID, providerName, tType)

	// 检查缓存
	if v, ok := s.providerCache.Load(cacheKey); ok {
		entry, assertOK := v.(providerCacheEntry)
		if !assertOK {
			s.providerCache.Delete(cacheKey)
		} else if time.Now().Before(entry.expiresAt) {
			return entry.provider, nil
		} else {
			s.providerCache.Delete(cacheKey)
		}
	}

	// 从 DB 加载（租户私有 + 系统级）
	var providers []*model.ModelProvider
	var err error
	if providerName == "" {
		providers, err = s.providerRepo.ListByModelType(tenantID, "llm")
	} else {
		providers, err = s.providerRepo.ListByTenant(tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list providers: %w", err)
	}

	// 优先租户私有，其次系统级（优先选择有 credentials 的 provider）
	var tenantMatch, systemMatch *model.ModelProvider
	for _, p := range providers {
		// 跳过已禁用的提供商
		if !p.IsActive {
			continue
		}
		// Fix 2: 跳过健康检查明确标记为 down 的 provider（degraded 仍可使用）
		if strings.EqualFold(p.HealthCheck, "down") {
			logger.Printf("[AI] getTenantProvider: skipping provider %q (health=down)", p.Name)
			continue
		}
		if providerName == "" || p.Name == providerName {
			if p.TenantID == tenantID && tenantID != 0 {
				if tenantMatch == nil || (!providerHasCredentials(tenantMatch) && providerHasCredentials(p)) {
					tenantMatch = p
				}
				if providerHasCredentials(tenantMatch) {
					break
				}
			}
			if p.TenantID == 0 {
				// 优先选有凭证的系统级 provider，已有有凭证的则不覆盖
				if systemMatch == nil {
					systemMatch = p
				} else if !providerHasCredentials(systemMatch) && providerHasCredentials(p) {
					systemMatch = p
				}
			}
		}
	}
	matched := tenantMatch
	if matched == nil {
		matched = systemMatch
	}

	if matched == nil {
		// 区分租户无配置与系统无配置，给出有指导意义的错误信息
		if tenantID > 0 {
			return nil, fmt.Errorf("tenant %d has no AI providers configured for task type %q; please add one in Model Management", tenantID, providerName)
		}
		return nil, fmt.Errorf("no AI provider configured for %q; please add one in Model Management", providerName)
	}

	// Validate credentials before constructing the provider.
	if !providerHasCredentials(matched) {
		return nil, fmt.Errorf("provider %q has no credentials configured", matched.Name)
	}

	// Decrypt stored credentials (AES-GCM; plaintext values pass through unchanged).
	// Fix 7: 区分"未配置密钥"与"密钥解密失败"两种情况，提供更清晰的错误信息。
	apiKey, err := crypto.Decrypt(matched.APIKey, s.encKey)
	if err != nil {
		if matched.APIKey == "" {
			return nil, fmt.Errorf("provider %q has no API key configured", matched.Name)
		}
		logger.Errorf("getTenantProvider: decrypt APIKey for %q failed (check DB_ENCRYPTION_KEY): %v", matched.Name, err)
		return nil, fmt.Errorf("failed to decrypt API key for provider %q (verify encryption key configuration)", matched.Name)
	}
	apiSecretKey, err := crypto.Decrypt(matched.APISecretKey, s.encKey)
	if err != nil {
		logger.Errorf("getTenantProvider: decrypt APISecretKey for %q failed (check DB_ENCRYPTION_KEY): %v", matched.Name, err)
		return nil, fmt.Errorf("failed to decrypt API secret key for provider %q (verify encryption key configuration)", matched.Name)
	}

	// Trim whitespace from credentials/endpoint — users sometimes paste values with
	// leading/trailing spaces which cause URL-parse failures at health check time.
	apiKey = strings.TrimSpace(apiKey)
	apiSecretKey = strings.TrimSpace(apiSecretKey)
	matched.APIEndpoint = strings.TrimSpace(matched.APIEndpoint)

	// Instantiate the provider.
	// 名称优先匹配已知 key；对自定义名称（如"豆包图片"）则根据 endpoint 推断构造器。
	timeout := ai.ResolveTimeout(0) // timeout/concurrency/rateLimit 由 AIModel 级别控制，provider 使用默认值
	modelName := effectiveModelName(matched)
	var provider ai.AIProvider
	switch matched.Name {
	case ai.ProviderNameVolcengineVisual:
		provider = ai.NewVolcengineVisualProvider(apiKey, apiSecretKey)
	case "kling-sfx":
		provider = ai.NewKlingSFXProvider(apiKey, apiSecretKey, matched.APIEndpoint)
	case "kling-tts":
		provider = ai.NewKlingTTSProvider(apiKey, apiSecretKey, matched.APIEndpoint)
	case "kling-image":
		provider = ai.NewKlingImageProvider(apiKey, apiSecretKey, matched.APIEndpoint)
	case "elevenlabs-sfx":
		provider = ai.NewElevenLabsSFXProvider(apiKey, matched.APIEndpoint)
	case "aliyun-tts":
		provider = ai.NewAliyunTTSProvider(apiKey, matched.APIEndpoint)
	case "qwen-tts":
		provider = ai.NewQwenTTSProvider(apiKey, matched.APIEndpoint)
	case "fun-music":
		provider = ai.NewFunMusicProvider(apiKey)
	case "minimax-music":
		provider = ai.NewMinimaxMusicProvider(apiKey)
	case "openai", "openai-image":
		provider = ai.NewOpenAIProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "anthropic":
		provider = ai.NewAnthropicProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "google":
		provider = ai.NewGoogleProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "doubao", "volcengine-ark-img":
		// "volcengine-ark-img" 是 DB 中 Seedream 图片模型的自定义名称，使用相同的 DoubaoProvider
		logger.Printf("getTenantProvider: provider %q → DoubaoProvider endpoint=%s model=%s", matched.Name, matched.APIEndpoint, modelName)
		provider = ai.NewDoubaoProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "doubao-speech":
		// APIKey = X-Api-Key, APIVersion = resourceID（如 "seed-tts-2.0"）
		provider = ai.NewDoubaoSpeechProvider(apiKey, matched.APIVersion)
	case "doubao-speech-v1":
		// APIKey = appID, APISecretKey = access_token, APIVersion = cluster（默认 volcano_tts）
		provider = ai.NewDoubaoSpeechV1Provider(apiKey, apiSecretKey, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(apiKey, matched.APIEndpoint, modelName, timeout)
	case "qianwen":
		switch tType {
		case "voice":
			provider = ai.NewQianwenTTSRouter(apiKey, matched.APIEndpoint)
		case "video":
			return nil, fmt.Errorf("provider %q is a video provider; use GetTenantVideoProvider", matched.Name)
		default:
			provider = ai.NewQianwenProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		}
	case "hunyuan":
		// TokenHub 新一代混元 API：LLM 用 OpenAI 兼容接口，图像用专属接口
		if tType == "image" {
			provider = ai.NewHunyuanImageProvider(apiKey, matched.APIEndpoint)
		} else {
			provider = ai.NewHunyuanProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		}
	case "azure":
		// APIEndpoint = Azure resource endpoint; APIVersion = REST API version ("2025-01-01-preview")
		// Deployment name is resolved at call time from req.Model (AIModel.Name).
		provider = ai.NewAzureProvider(apiKey, matched.APIEndpoint, "", matched.APIVersion, timeout)
	default:
		// 自定义名称：按 endpoint + model type 推断底层 API 格式
		ep := strings.ToLower(matched.APIEndpoint)
		provType := ""
		if s.modelRepo != nil {
			if mods, _ := s.modelRepo.List(&matched.ID, tenantID); len(mods) > 0 {
				provType = strings.ToLower(mods[0].Type)
			}
		}
		switch {
		case strings.Contains(ep, "klingai.com"):
			// 可灵系列：按 model type 选择正确的构造器（tType 优先，其次 provType）
			klingType := tType
			if klingType == "" {
				klingType = provType
			}
			switch klingType {
			case "sfx":
				logger.Printf("getTenantProvider: provider %q mapped to KlingSFXProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingSFXProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "voice":
				logger.Printf("getTenantProvider: provider %q mapped to KlingTTSProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingTTSProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "image", "img2img":
				logger.Printf("getTenantProvider: provider %q mapped to KlingImageProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingImageProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "video":
				// video 类型由 GetTenantVideoProvider 处理，AIProvider 路径不支持
				logger.Printf("getTenantProvider: provider %q type=video — use GetTenantVideoProvider instead", matched.Name)
				return nil, fmt.Errorf("provider %q is a video provider; use GetTenantVideoProvider", matched.Name)
			default:
				return nil, fmt.Errorf("provider %q (klingai endpoint): cannot determine service type (sfx/voice/image/video) — check the associated AIModel's type", matched.Name)
			}
		case strings.Contains(ep, "volces.com") || strings.Contains(ep, "volcengine"):
			// 火山方舟 / 豆包系列（OpenAI 兼容格式）
			logger.Printf("getTenantProvider: provider %q mapped to doubao constructor via endpoint", matched.Name)
			provider = ai.NewDoubaoProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		case strings.Contains(ep, "azure.com") || strings.Contains(ep, "openai.azure"):
			logger.Printf("getTenantProvider: provider %q mapped to azure constructor via endpoint", matched.Name)
			// APIEndpoint = Azure resource endpoint; APIVersion = REST API version ("2025-01-01-preview")
			// Deployment name is resolved at call time from req.Model (AIModel.Name).
			provider = ai.NewAzureProvider(apiKey, matched.APIEndpoint, "", matched.APIVersion, timeout)
		case strings.Contains(ep, "anthropic.com"):
			logger.Printf("getTenantProvider: provider %q mapped to anthropic constructor via endpoint", matched.Name)
			provider = ai.NewAnthropicProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		case matched.APIEndpoint != "":
			// 有自定义 endpoint → 按 OpenAI 兼容格式通用处理
			logger.Printf("getTenantProvider: provider %q using OpenAI-compatible constructor for endpoint %s", matched.Name, matched.APIEndpoint)
			provider = ai.NewOpenAIProvider(apiKey, matched.APIEndpoint, modelName, timeout)
		default:
			return nil, fmt.Errorf("provider %q: cannot determine provider type (unrecognized name and no endpoint configured)", matched.Name)
		}
	}

	provider = ai.NewRetryProvider(provider, 3, 500*time.Millisecond)

	// 写入缓存
	s.providerCache.Store(cacheKey, providerCacheEntry{
		provider:  provider,
		expiresAt: time.Now().Add(5 * time.Minute),
	})

	return provider, nil
}

// CheckAvailability 检查指定租户是否有可用的 LLM 提供商（用于 pipeline 预检）
func (s *AIService) CheckAvailability(tenantID uint) error {
	_, err := s.getTenantProvider(tenantID, "")
	return err
}

// loadDBProviderByName 从 DB 中按名称精确查找提供商（不限类型）。
// 优先使用租户级别（tenant_id=N）有凭证的记录，其次使用系统级（tenant_id=0）有凭证的记录。
// 同名但无凭证的记录（种子占位符）会被跳过，不会阻断查找。
func (s *AIService) loadDBProviderByName(tenantID uint, name string) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	nameFound := false
	for _, p := range providers {
		if !p.IsActive || !strings.EqualFold(p.Name, name) {
			continue
		}
		nameFound = true
		if !providerHasCredentials(p) {
			continue // 跳过无凭证的种子占位符，继续查找有凭证的租户记录
		}
		return s.getTenantProvider(tenantID, p.Name)
	}
	if nameFound {
		return nil, fmt.Errorf("provider %q has no credentials configured", name)
	}
	return nil, fmt.Errorf("provider %q not found or not active in DB", name)
}

// eligibleProviders 从 DB 加载指定类型（"image"/"video"/"voice"/"sfx"/...）的提供者，
// 过滤掉未激活或缺少凭据的。这是 loadDBImageProviderEntries/loadDBVoiceProvider/
// GetTenantVideoProvider/GetTenantLipSyncProvider/ListCapableProviders 共用的第一步——
// 过滤之后"怎么从候选里选一个"（按 voiceID 匹配、按 provider 名称偏好排序等）由各自的
// 调用方决定，不同类型的选择规则差异较大，不适合也塞进这个共享函数里。
// 注意：getTenantProvider 不复用这个函数——它对"没有凭据"的处理是降级容忍而非硬性跳过
// （租户级/系统级都找不到有凭据的才退而求其次接受无凭据的），语义上不是同一个过滤条件。
func (s *AIService) eligibleProviders(tenantID uint, modelType string) ([]*model.ModelProvider, error) {
	providers, err := s.providerRepo.ListByModelType(tenantID, modelType)
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

// loadDBProviderByType 从 DB 中取第一个有效的指定类型提供商（如 "sfx"、"voice"）。
// 返回 provider、提供商名称和错误。
func (s *AIService) loadDBProviderByType(tenantID uint, modelType string) (ai.AIProvider, string, error) {
	return s.loadDBVoiceProvider(tenantID, modelType, "")
}

// loadDBVoiceProvider 按 voiceID 从内置音色表优先匹配，未命中则取第一个有效 provider。
// voiceID 为空时退化为 loadDBProviderByType 行为。
// 返回 provider、提供商名称和错误。
func (s *AIService) loadDBVoiceProvider(tenantID uint, modelType, voiceID string) (ai.AIProvider, string, error) {
	logger.Printf("[TTS] loadDBVoiceProvider: tenantID=%d modelType=%q voiceID=%q", tenantID, modelType, voiceID)
	providers, err := s.eligibleProviders(tenantID, modelType)
	if err != nil {
		logger.Errorf("[TTS] loadDBVoiceProvider: ERROR ListByModelType tenantID=%d modelType=%q: %v", tenantID, modelType, err)
		return nil, "", err
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
		return nil, "", fmt.Errorf("no %s providers configured in DB", modelType)
	}

	// 先取 priority=0（voice 匹配），再取 priority=1（兜底）
	for _, pass := range []int{0, 1} {
		for _, c := range candidates {
			if c.priority != pass {
				continue
			}
			provider, err := s.getTenantProvider(tenantID, c.p.Name, modelType)
			if err != nil {
				logger.Errorf("[TTS] loadDBVoiceProvider: ERROR instantiate %s provider %q: %v", modelType, c.p.Name, err)
				continue
			}
			logger.Printf("[TTS] loadDBVoiceProvider: selected %s provider %q (voice=%q priority=%d)", modelType, c.p.Name, voiceID, pass)
			return provider, c.p.Name, nil
		}
	}
	logger.Errorf("[TTS] loadDBVoiceProvider: ERROR all %d candidates failed to instantiate for modelType=%q voiceID=%q", len(candidates), modelType, voiceID)
	return nil, "", fmt.Errorf("no %s providers configured in DB", modelType)
}

// getProviderCreds 从 DB 中取指定 tenantID 下、名称匹配 name 的 provider 凭据（apiKey, endpoint）。
// err 非 nil 表示真实故障（ListByTenant 查询失败、APIKey 解密失败——比如加密密钥换了或密文损坏）；
// err 为 nil 但 apiKey 为空，才表示这个 provider 确实未配置/未激活/未填凭据。调用方必须能区分
// 这两种情况——不能像过去那样把"解密失败"和"没配置"统一返回成一样的空字符串，那样会让一个
// 真实的基础设施故障被永久掩盖成"用户没打开这个可选功能"。
func (s *AIService) getProviderCreds(tenantID uint, name string) (apiKey, endpoint string, err error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return "", "", err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if !strings.EqualFold(p.Name, name) {
			continue
		}
		if !providerHasCredentials(p) {
			continue
		}
		key, decErr := crypto.Decrypt(p.APIKey, s.encKey)
		if decErr != nil {
			return "", "", fmt.Errorf("decrypt APIKey for provider %q: %w", p.Name, decErr)
		}
		return key, p.APIEndpoint, nil
	}
	return "", "", nil
}

// GetBGMProviderCreds 从 DB 中取指定 music 类型提供商的凭据（apiKey, endpoint）。
// err 非 nil 表示真实故障（见 getProviderCreds），调用方不应把它当成"未配置"静默忽略。
func (s *AIService) GetBGMProviderCreds(tenantID uint, name string) (apiKey, endpoint string, err error) {
	apiKey, endpoint, err = s.getProviderCreds(tenantID, name)
	if err != nil {
		logger.Errorf("GetBGMProviderCreds: provider %q: %v", name, err)
	}
	return apiKey, endpoint, err
}

// GetSFXProviderCreds 从 DB 中取指定 sfx 类型提供商的凭据（apiKey, endpoint）。
// err 非 nil 表示真实故障（见 getProviderCreds），调用方不应把它当成"未配置"静默忽略。
func (s *AIService) GetSFXProviderCreds(tenantID uint, name string) (apiKey, endpoint string, err error) {
	apiKey, endpoint, err = s.getProviderCreds(tenantID, name)
	if err != nil {
		logger.Errorf("GetSFXProviderCreds: provider %q: %v", name, err)
	}
	return apiKey, endpoint, err
}

// GetTenantVideoProvider 从 DB 中查找指定租户已配置的视频生成提供商。
// name 为空时返回第一个可用的视频提供商（kling 优先）。
func (s *AIService) GetTenantVideoProvider(tenantID uint, name string) (ai.VideoProvider, error) {
	providers, err := s.eligibleProviders(tenantID, "video")
	if err != nil {
		return nil, err
	}
	// 按照 volcengine-visual/jimeng-video → kling → seedance/doubao → minimax-video → happyhorse/qianwen 顺序优先选择
	// volcengine-visual 合并了 jimeng-video；doubao 合并了 seedance；qianwen 合并了 happyhorse
	preferOrder := []string{"volcengine-visual", "jimeng-video", "kling", "seedance", "doubao", "minimax-video", "happyhorse", "qianwen"}
	if name != "" {
		preferOrder = []string{strings.ToLower(name)}
	}
	byName := make(map[string]*model.ModelProvider)
	for _, p := range providers {
		pname := strings.ToLower(p.Name)
		if _, exists := byName[pname]; !exists {
			byName[pname] = p
		}
	}
	for _, pname := range preferOrder {
		p, ok := byName[pname]
		if !ok {
			continue
		}
		// Decrypt stored credentials before passing to provider constructors.
		apiKey, err := crypto.Decrypt(p.APIKey, s.encKey)
		if err != nil {
			logger.Errorf("GetTenantVideoProvider: decrypt APIKey for %q: %v", p.Name, err)
			continue
		}
		apiSecretKey, err := crypto.Decrypt(p.APISecretKey, s.encKey)
		if err != nil {
			logger.Errorf("GetTenantVideoProvider: decrypt APISecretKey for %q: %v", p.Name, err)
			continue
		}
		switch pname {
		case "volcengine-visual":
			// volcengine-visual 合并了 jimeng-video（即梦视频）
			return ai.NewJimengVideoProvider(apiKey, apiSecretKey), nil
		case "jimeng-video":
			return ai.NewJimengVideoProvider(apiKey, apiSecretKey), nil
		case "kling":
			return ai.NewKlingProvider(apiKey, apiSecretKey, p.APIEndpoint), nil
		case "seedance", "doubao":
			return ai.NewDoubaoVideoProvider(apiKey, p.APIEndpoint), nil
		case "minimax-video":
			return ai.NewMinimaxVideoProvider(apiKey), nil
		case "happyhorse":
			return ai.NewHappyHorseProvider(apiKey, p.APIEndpoint), nil
		case "qianwen":
			// qianwen 合并了 happyhorse（DashScope 视频生成）
			return ai.NewHappyHorseProvider(apiKey, p.APIEndpoint), nil
		}
	}
	if name != "" {
		return nil, fmt.Errorf("video provider %q not configured for tenant %d", name, tenantID)
	}
	return nil, fmt.Errorf("no video provider configured for tenant %d", tenantID)
}

// GetTenantLipSyncProvider 查找租户已配置的口型对齐提供商。
// 目前仅支持 kling（使用 kling provider 的 AK/SK 构造 KlingLipSyncProvider）。
func (s *AIService) GetTenantLipSyncProvider(tenantID uint) (ai.LipSyncProvider, error) {
	providers, err := s.eligibleProviders(tenantID, "video")
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if strings.ToLower(p.Name) != "kling" {
			continue
		}
		apiKey, err := crypto.Decrypt(p.APIKey, s.encKey)
		if err != nil {
			continue
		}
		apiSecretKey, err := crypto.Decrypt(p.APISecretKey, s.encKey)
		if err != nil {
			continue
		}
		return ai.NewKlingLipSyncProvider(apiKey, apiSecretKey, p.APIEndpoint), nil
	}
	return nil, fmt.Errorf("no lip sync provider configured for tenant %d (kling required)", tenantID)
}

// GetActiveVideoModelName 从数据库查询指定 provider 的第一个激活视频模型名。
// 调用方在 VideoGenerateRequest.Model 为空时用此值，避免 provider 内部使用写死的默认模型。
func (s *AIService) GetActiveVideoModelName(tenantID uint, providerName string) (string, error) {
	if s.providerRepo == nil || s.modelRepo == nil {
		return "", fmt.Errorf("repos not available")
	}
	providers, err := s.providerRepo.ListByModelType(tenantID, "video")
	if err != nil {
		return "", err
	}
	pnameLower := strings.ToLower(providerName)
	for _, p := range providers {
		if strings.ToLower(p.Name) != pnameLower {
			continue
		}
		models, mErr := s.modelRepo.List(&p.ID, tenantID)
		if mErr != nil {
			return "", mErr
		}
		for _, m := range models {
			if m.Type == "video" && m.IsActive {
				return m.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no active video model for provider %q (tenant %d)", providerName, tenantID)
}
