package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/crypto"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
	"golang.org/x/image/draw"
)

type AIService struct {
	modelRepo     *repository.AIModelRepository
	taskRepo      *repository.TaskModelConfigRepository
	aiManager     *ai.ModelManager
	providerRepo  *repository.ModelProviderRepository
	novelRepo     *repository.NovelRepository
	storageSvc    storage.Service
	taskRouting   TaskRouting
	serverBaseURL string       // base URL for resolving relative media paths, e.g. "http://127.0.0.1:8080"
	providerCache sync.Map      // key: "tenantID:providerName" → providerCacheEntry
	imageSem      chan struct{} // nil = unlimited; set via WithImageConcurrency
	semMu         sync.RWMutex  // protects imageSem replacement
	stopCh        chan struct{} // closed by Shutdown() to stop background goroutines
	encKey        string       // AES-256-GCM key for decrypting stored API credentials
	cache         redisPublisher // optional: for cross-instance provider cache invalidation
	promptFilter  *PromptFilter  // optional: proactive sensitive-word filtering for image prompts
}

// redisPublisher is the subset of redis.Client used by AIService (allows nil-safe injection).
type redisPublisher interface {
	Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	taskRepo *repository.TaskModelConfigRepository,
	aiManager *ai.ModelManager,
	providerRepo ...*repository.ModelProviderRepository,
) *AIService {
	svc := &AIService{
		modelRepo: modelRepo,
		taskRepo:  taskRepo,
		aiManager: aiManager,
		stopCh:    make(chan struct{}),
	}
	if len(providerRepo) > 0 {
		svc.providerRepo = providerRepo[0]
	}
	svc.startProviderCacheCleanup()
	svc.startProviderHealthCheck()
	return svc
}

// WithEncryptionKey sets the AES-256-GCM key used to decrypt API credentials stored in the DB.
func (s *AIService) WithEncryptionKey(key string) *AIService {
	s.encKey = key
	return s
}

// WithPromptFilter injects a PromptFilter used to sanitize LLM-generated image prompts.
func (s *AIService) WithPromptFilter(f *PromptFilter) *AIService {
	s.promptFilter = f
	return s
}

// FilterPrompt applies the sensitive-word filter to a prompt.
// Called by other services (CharacterService, ItemService, NovelAnalysisService) right after
// the LLM generates a visual prompt, before it is persisted to the database.
func (s *AIService) FilterPrompt(prompt string) string {
	if s.promptFilter == nil {
		return prompt
	}
	return s.promptFilter.Apply(prompt)
}

// Shutdown stops background goroutines (call on server exit).
func (s *AIService) Shutdown() {
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
}

// startProviderCacheCleanup 启动 providerCache 的后台定期清理（每 10 分钟扫描一次，删除已过期条目）。
func (s *AIService) startProviderCacheCleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				s.providerCache.Range(func(k, v interface{}) bool {
					if entry, ok := v.(providerCacheEntry); ok && now.After(entry.expiresAt) {
						s.providerCache.Delete(k)
					}
					return true
				})
			case <-s.stopCh:
				return
			}
		}
	}()
}

// startProviderHealthCheck 每 5 分钟对所有已激活 provider 做一次健康探测，更新 health_check 字段。
// Fix 3: 启动时立即执行一次，不等待首个 ticker 信号。
func (s *AIService) startProviderHealthCheck() {
	// 立即执行一次，确保启动后 health 状态立刻有效
	go s.runProviderHealthChecks()

	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.runProviderHealthChecks()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// runProviderHealthChecks iterates active providers and updates their health status.
func (s *AIService) runProviderHealthChecks() {
	if s.providerRepo == nil {
		return
	}
	providers, err := s.providerRepo.List()
	if err != nil {
		return
	}
	sem := make(chan struct{}, 10)
	for _, p := range providers {
		if !p.IsActive || !providerHasCredentials(p) {
			continue
		}
		p := p
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			provider, err := s.getTenantProvider(p.TenantID, p.Name)
			status := "ok"
			if err != nil {
				status = "down"
			} else if provider == nil {
				status = "down"
			} else if hErr := provider.HealthCheck(ctx); hErr != nil {
				status = "degraded"
				logger.Errorf("[health] provider=%s err=%v", p.Name, hErr)
			}
			if upErr := s.providerRepo.UpdateHealthStatus(p.ID, status); upErr != nil {
				logger.Errorf("[health] UpdateHealthStatus provider=%s: %v", p.Name, upErr)
			}
		}()
	}
}

// GetOverallHealthStatus returns a single status string summarising all active
// AI providers based on the health_check field stored in the DB by the
// background health-check goroutine. Possible return values:
//   - "ok"       — all active providers are healthy (or no providers configured)
//   - "degraded" — at least one provider is degraded but none are down
//   - "down"     — at least one active provider is reported as down
//
// This is intentionally non-blocking: it reads from the already-populated DB
// column rather than performing live network checks, so it is safe to call on
// every HTTP health-check request.
func (s *AIService) GetOverallHealthStatus() string {
	if s.providerRepo == nil {
		return "ok"
	}
	providers, err := s.providerRepo.List()
	if err != nil {
		return "ok" // fail-open: don't report degraded when we can't read the DB
	}
	anyDegraded := false
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		switch strings.ToLower(p.HealthCheck) {
		case "down":
			return "down"
		case "degraded":
			anyDegraded = true
		}
	}
	if anyDegraded {
		return "degraded"
	}
	return "ok"
}

// WithNovelRepo 注入小说仓库，用于在生成时读取小说级 AI 配置
func (s *AIService) WithNovelRepo(repo *repository.NovelRepository) *AIService {
	s.novelRepo = repo
	return s
}

// WithStorage 注入媒体存储服务，供图片生成后持久化使用
func (s *AIService) WithStorage(svc storage.Service) *AIService {
	s.storageSvc = svc
	return s
}

// WithTaskRouting 设置各任务类型优先使用的 provider（来自 config.yaml ai.tasks）
func (s *AIService) WithTaskRouting(tr TaskRouting) *AIService {
	s.taskRouting = tr
	return s
}

// WithImageConcurrency 设置图像生成的最大并发数。
// n ≤ 0 时不限制并发（仅受 AI 提供商限速约束）。
func (s *AIService) WithImageConcurrency(n int) *AIService {
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	}
	return s
}

// SetImageConcurrency 运行时动态调整图像并发度（线程安全）。
// 替换 channel 前先排干旧 channel 中的令牌，避免新旧 channel 并发竞争导致
// goroutine 持有旧 channel 令牌但向新 channel 归还的情况。
func (s *AIService) SetImageConcurrency(n int) {
	s.semMu.Lock()
	defer s.semMu.Unlock()
	// 排干旧 channel（非阻塞）
	if s.imageSem != nil {
	drainLoop:
		for {
			select {
			case <-s.imageSem:
			default:
				break drainLoop
			}
		}
	}
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	} else {
		s.imageSem = nil
	}
}

// ImageConcurrency 返回当前图像并发度上限（0 = 不限制）。
func (s *AIService) ImageConcurrency() int {
	s.semMu.RLock()
	defer s.semMu.RUnlock()
	if s.imageSem == nil {
		return 0
	}
	return cap(s.imageSem)
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, novelID, taskType, prompt, "")
}

// GetDefaultProviderName 返回当前默认 provider 名称
func (s *AIService) GetDefaultProviderName() string {
	if s.aiManager == nil {
		return "unknown"
	}
	p, err := s.aiManager.GetProvider("")
	if err != nil {
		return "unknown"
	}
	return p.GetName()
}

// getTenantProvider 按租户加载提供商（带缓存，TTL 5 分钟）
func (s *AIService) getTenantProvider(tenantID uint, providerName string) (ai.AIProvider, error) {
	if s.providerRepo == nil {
		return s.aiManager.GetProvider(providerName)
	}

	cacheKey := fmt.Sprintf("%d:%s", tenantID, providerName)

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
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return s.aiManager.GetProvider(providerName)
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
		// 当未指定具体提供商时，跳过图像/视频/语音/嵌入/多能力类型（这些不做文本生成）
		if providerName == "" {
			t := strings.ToLower(p.Type)
			if t == "image" || t == "video" || t == "voice" || t == "tts" || t == "embedding" || t == "sfx" || t == "music" {
				continue
			}
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
		// DB 中无配置，降级到内存 aiManager
		p, err := s.aiManager.GetProvider(providerName)
		if err != nil {
			// 区分租户无配置与系统无配置，给出有指导意义的错误信息
			if tenantID > 0 {
				return nil, fmt.Errorf("tenant %d has no AI providers configured for task type %q; please add one in Model Management", tenantID, providerName)
			}
			return nil, fmt.Errorf("no AI provider available for %q: %w", providerName, err)
		}
		return p, nil
	}

	// Validate credentials before constructing the provider.
	if !providerHasCredentials(matched) {
		logger.Printf("getTenantProvider: DB provider %q missing credentials, falling back to in-memory manager", matched.Name)
		return s.aiManager.GetProvider(providerName)
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

	// Instantiate the provider.
	// 名称优先匹配已知 key；对自定义名称（如"豆包图片"）则根据 endpoint 推断构造器。
	timeout := ai.ResolveTimeout(matched.Timeout)
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
	case "openai":
		provider = ai.NewOpenAIProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "anthropic":
		provider = ai.NewAnthropicProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "google":
		provider = ai.NewGoogleProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao":
		provider = ai.NewDoubaoProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao-speech":
		// APIKey = X-Api-Key, APIVersion = resourceID（如 "seed-tts-2.0"）
		provider = ai.NewDoubaoSpeechProvider(apiKey, matched.APIVersion)
	case "doubao-speech-v1":
		// APIKey = appID, APISecretKey = access_token, APIVersion = cluster（默认 volcano_tts）
		provider = ai.NewDoubaoSpeechV1Provider(apiKey, apiSecretKey, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "qianwen":
		provider = ai.NewQianwenProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "azure":
		// APIEndpoint = Azure resource endpoint; APIVersion = REST API version ("2025-01-01-preview")
		// Deployment name is resolved at call time from req.Model (AIModel.Name).
		provider = ai.NewAzureProvider(apiKey, matched.APIEndpoint, "", matched.APIVersion, timeout)
	default:
		// 自定义名称：按 endpoint + type 推断底层 API 格式
		ep := strings.ToLower(matched.APIEndpoint)
		provType := strings.ToLower(matched.Type)
		switch {
		case strings.Contains(ep, "klingai.com"):
			// 可灵系列：按 provider type 选择正确的构造器，避免误用 OpenAI 兼容构造器
			switch provType {
			case "sfx":
				logger.Printf("getTenantProvider: provider %q mapped to KlingSFXProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingSFXProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "voice", "tts":
				logger.Printf("getTenantProvider: provider %q mapped to KlingTTSProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingTTSProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "image":
				logger.Printf("getTenantProvider: provider %q mapped to KlingImageProvider via endpoint+type", matched.Name)
				provider = ai.NewKlingImageProvider(apiKey, apiSecretKey, matched.APIEndpoint)
			case "video":
				// video 类型由 GetTenantVideoProvider 处理，AIProvider 路径不支持
				logger.Printf("getTenantProvider: provider %q type=video — use GetTenantVideoProvider instead", matched.Name)
				return nil, fmt.Errorf("provider %q is a video provider; use GetTenantVideoProvider", matched.Name)
			default:
				logger.Printf("getTenantProvider: provider %q (klingai endpoint, type=%q) — falling back to static aiManager", matched.Name, matched.Type)
				return s.aiManager.GetProvider(providerName)
			}
		case strings.Contains(ep, "volces.com") || strings.Contains(ep, "volcengine"):
			// 火山方舟 / 豆包系列（OpenAI 兼容格式）
			logger.Printf("getTenantProvider: provider %q mapped to doubao constructor via endpoint", matched.Name)
			provider = ai.NewDoubaoProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case strings.Contains(ep, "azure.com") || strings.Contains(ep, "openai.azure"):
			logger.Printf("getTenantProvider: provider %q mapped to azure constructor via endpoint", matched.Name)
			// APIEndpoint = Azure resource endpoint; APIVersion = REST API version ("2025-01-01-preview")
		// Deployment name is resolved at call time from req.Model (AIModel.Name).
		provider = ai.NewAzureProvider(apiKey, matched.APIEndpoint, "", matched.APIVersion, timeout)
		case strings.Contains(ep, "anthropic.com"):
			logger.Printf("getTenantProvider: provider %q mapped to anthropic constructor via endpoint", matched.Name)
			provider = ai.NewAnthropicProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case matched.APIEndpoint != "":
			// 有自定义 endpoint → 按 OpenAI 兼容格式通用处理
			logger.Printf("getTenantProvider: provider %q using OpenAI-compatible constructor for endpoint %s", matched.Name, matched.APIEndpoint)
			provider = ai.NewOpenAIProvider(apiKey, matched.APIEndpoint, matched.APIVersion, timeout)
		default:
			logger.Printf("getTenantProvider: unrecognized provider %q with no endpoint — falling back to static aiManager", matched.Name)
			return s.aiManager.GetProvider(providerName)
		}
	}

	provider = ai.NewRetryProvider(provider, 3, 500*time.Millisecond)
	if matched.Concurrency > 0 {
		provider = ai.NewConcurrentProvider(provider, matched.Concurrency)
	}
	if matched.RateLimit > 0 {
		provider = ai.NewRateLimitProvider(provider, matched.RateLimit)
	}

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

// WithRedis 注入 Redis 客户端，用于跨实例 provider 缓存失效广播。
// 可选：不注入时退化为单实例行为（仅清本实例内存缓存）。
func (s *AIService) WithRedis(c *redis.Client) *AIService {
	// 仅在 c 非 nil 时赋值：避免将 (*redis.Client)(nil) 存入 interface 后
	// interface != nil 判断为 true，但方法调用仍会 panic（Go interface nil 陷阱）
	if c != nil {
		s.cache = c
		go s.startProviderInvalidateSubscriber()
	}
	return s
}

const redisChanProviderInvalidate = "inkframe:provider:invalidate"

// startProviderInvalidateSubscriber 订阅 Redis 频道，收到消息后清除本实例的 provider 缓存。
func (s *AIService) startProviderInvalidateSubscriber() {
	sub := s.cache.Subscribe(context.Background(), redisChanProviderInvalidate)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.invalidateLocalProviderCache(msg.Payload)
		case <-s.stopCh:
			return
		}
	}
}

// invalidateLocalProviderCache 仅清除本实例内存缓存（不发布 Pub/Sub，防止循环）。
func (s *AIService) invalidateLocalProviderCache(providerName string) {
	s.providerCache.Range(func(k, _ any) bool {
		key, ok := k.(string)
		if ok && strings.HasSuffix(key, ":"+providerName) {
			s.providerCache.Delete(k)
		}
		return true
	})
}

// InvalidateProviderCache 清除本实例缓存并向其它实例广播失效通知。
// 供 DeleteProvider/UpdateProvider 调用。
func (s *AIService) InvalidateProviderCache(providerName string) {
	s.invalidateLocalProviderCache(providerName)
	// 广播给其它实例（Redis Pub/Sub）
	if s.cache != nil {
		_ = s.cache.Publish(context.Background(), redisChanProviderInvalidate, providerName).Err()
	}
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
// 参数优先级：小说项目配置 > 任务配置 > 内置默认值
func (s *AIService) GenerateWithProvider(tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	// 获取任务配置（基础层）
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{
			Temperature: 0.7,
			MaxTokens:   0, // 0 = 不限制，由 AI provider 使用其模型默认值
		}
	}
	// 复制一份避免污染缓存
	config := *base

	// 应用小说项目级 AI 配置（覆盖任务默认值）
	var resolvedModel string
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			// 若小说配置了特定 AI 模型且调用方未指定 provider，
			// 通过模型名反查对应 provider 并将模型名透传给 API
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	// JSON 结构化输出任务降温：高温度会产生格式漂移（多余 markdown / 说明文字），触发 retry 更慢。
	// MaxTokens 不设强制下限，由任务配置 / 小说配置 / 调用方覆盖自行决定。
	switch taskType {
	case "storyboard", "character", "worldview", "character_state", "scene_anchor_extract", "storyboard_review", "sfx_analyze":
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	}

	// 应用调用方传入的覆盖参数（优先级最高，覆盖任务配置和小说配置）
	if len(overrides) > 0 {
		o := overrides[0]
		if o.MaxTokens > 0 {
			config.MaxTokens = o.MaxTokens
		}
		if o.Temperature > 0 {
			config.Temperature = o.Temperature
		}
		if o.TimeoutSeconds > 0 {
			config.TimeoutSeconds = o.TimeoutSeconds
		}
	}

	// Task#6: 若 TaskModelConfig 显式绑定了 provider，优先使用它（消除同名模型歧义）
	if providerName == "" && config.PrimaryProviderID > 0 && s.providerRepo != nil {
		if p, err := s.providerRepo.GetByID(config.PrimaryProviderID); err == nil && p != nil {
			providerName = p.Name
		}
	}

	// Task#4: 若仍未确定 model/provider，按 strategy 从候选模型中自动选择
	if resolvedModel == "" && providerName == "" && config.Strategy != "" && s.modelRepo != nil {
		if candidates, err := s.modelRepo.GetAvailableByTaskType(taskType, tenantID); err == nil && len(candidates) > 0 {
			var selected *model.AIModel
			switch config.Strategy {
			case "quality_first":
				selected = selectByQuality(candidates)
			case "cost_first":
				selected = selectByCost(candidates)
			default: // "balanced" or unrecognised
				selected = selectBalanced(candidates)
			}
			// Fix 4: Guard against nil selected — degrade to balanced selection before giving up.
			if selected == nil {
				selected = selectBalanced(candidates)
				if selected == nil {
					return "", fmt.Errorf("no suitable model available for strategy=%q taskType=%q", config.Strategy, taskType)
				}
				logger.Printf("[AI] strategy %q found no suitable model, falling back to balanced selection", config.Strategy)
			}
			if selected.Provider == nil {
				return "", fmt.Errorf("model %d has no provider configured", selected.ID)
			}
			resolvedModel = selected.Name
			providerName = selected.Provider.Name
			// 策略选择路径：直接从已加载的 AIModel 取 MaxTokens，无需额外 DB 查询
			if config.MaxTokens == 0 && selected.MaxTokens > 0 {
				config.MaxTokens = selected.MaxTokens
			}
		}
	}

	// AIModel 级别 MaxTokens（novel.AIModel 路径：仅有 model name，需通过 DB 查找）
	if config.MaxTokens == 0 && resolvedModel != "" && s.modelRepo != nil {
		if m, err := s.modelRepo.GetByName(resolvedModel); err == nil && m != nil && m.MaxTokens > 0 {
			config.MaxTokens = m.MaxTokens
		}
	}

	// provider 级别 MaxTokens（兜底，优先级低于 AIModel 配置）
	if config.MaxTokens == 0 && providerName != "" && s.providerRepo != nil {
		if providers, err := s.providerRepo.ListByTenant(tenantID); err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.MaxTokens > 0 {
					config.MaxTokens = p.MaxTokens
					break
				}
			}
		}
	}

	// 任务类型兜底 MaxTokens：仅在配置链全部为 0 时生效，不覆盖任何已配置值。
	// 生产环境请在「模型管理 → 编辑提供商 / 模型 → 最大 Tokens」处配置（优先级高于此处）。
	if config.MaxTokens == 0 {
		switch taskType {
		case "storyboard":
			config.MaxTokens = 16384 // 每段 ~12 镜 × 700 tokens = 8400 tokens
		case "outline":
			config.MaxTokens = 16384 // 100 章大纲 JSON 约 20K+ 字符
		case "chapter_review", "storyboard_review":
			config.MaxTokens = 8192 // 段落级反馈 JSON，单章不超过 8K tokens
		case "storyboard_arc":
			config.MaxTokens = 2048 // 短弧线骨架 JSON
		}
	}

	// 注入 system prompt：章节写作类阻止元注释；JSON 输出类抑制推理模型的思考过程
	sysPmt := ""
	if chapterTaskTypes[taskType] {
		sysPmt = novelWritingSystemPrompt
	} else if jsonOnlyTaskTypes[taskType] {
		sysPmt = jsonOnlySystemPrompt
	}

	// 调用真实AI API
	effectiveProvider := providerName
	if effectiveProvider == "" {
		effectiveProvider = "default"
	}
	metrics.AIRequestsInFlight.WithLabelValues(taskType, effectiveProvider).Inc()
	callStart := time.Now()
	result, resp, err := s.callAIWithProviderSys(context.Background(), tenantID, prompt, sysPmt, &config, providerName, resolvedModel)
	elapsed := time.Since(callStart).Seconds()
	metrics.AIRequestsInFlight.WithLabelValues(taskType, effectiveProvider).Dec()

	// Prometheus 指标：AI 调用结果
	if err != nil {
		metrics.AIRequestsTotal.WithLabelValues(taskType, effectiveProvider, "error").Inc()
		return "", fmt.Errorf("AI generation failed: %w", err)
	}
	metrics.AIRequestsTotal.WithLabelValues(taskType, effectiveProvider, "success").Inc()
	metrics.AIRequestDuration.WithLabelValues(taskType, effectiveProvider).Observe(elapsed)
	if resp.InputTokens > 0 {
		metrics.AITokensTotal.WithLabelValues(taskType, effectiveProvider, "prompt").Add(float64(resp.InputTokens))
	}
	if resp.Tokens > 0 {
		metrics.AITokensTotal.WithLabelValues(taskType, effectiveProvider, "completion").Add(float64(resp.Tokens))
	}

	s.logUsage(tenantID, &config, taskType, resp, time.Since(callStart).Milliseconds())

	return result, nil
}

// GenerateWithProviderCtx is like GenerateWithProvider but respects an external context.
// Use this when the caller holds a cancellable context (e.g. async task with cancel support).
func (s *AIService) GenerateWithProviderCtx(ctx context.Context, tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{Temperature: 0.7, MaxTokens: 0}
	}
	config := *base

	var resolvedModel string
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	switch taskType {
	case "storyboard", "character", "worldview", "character_state", "scene_anchor_extract", "storyboard_review", "sfx_analyze":
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	}

	if len(overrides) > 0 {
		o := overrides[0]
		if o.MaxTokens > 0 {
			config.MaxTokens = o.MaxTokens
		}
		if o.Temperature > 0 {
			config.Temperature = o.Temperature
		}
		if o.TimeoutSeconds > 0 {
			config.TimeoutSeconds = o.TimeoutSeconds
		}
	}

	// Task#6: 显式绑定的 provider 优先于自动选择
	if providerName == "" && config.PrimaryProviderID > 0 && s.providerRepo != nil {
		if p, err := s.providerRepo.GetByID(config.PrimaryProviderID); err == nil && p != nil {
			providerName = p.Name
		}
	}

	// Task#4: strategy-based 自动选模
	if resolvedModel == "" && providerName == "" && config.Strategy != "" && s.modelRepo != nil {
		if candidates, err := s.modelRepo.GetAvailableByTaskType(taskType, tenantID); err == nil && len(candidates) > 0 {
			var selected *model.AIModel
			switch config.Strategy {
			case "quality_first":
				selected = selectByQuality(candidates)
			case "cost_first":
				selected = selectByCost(candidates)
			default:
				selected = selectBalanced(candidates)
			}
			// Fix 4: Guard against nil selected — degrade to balanced selection before giving up.
			if selected == nil {
				selected = selectBalanced(candidates)
				if selected == nil {
					return "", fmt.Errorf("no suitable model available for strategy=%q taskType=%q", config.Strategy, taskType)
				}
				logger.Printf("[AI] strategy %q found no suitable model, falling back to balanced selection", config.Strategy)
			}
			if selected.Provider == nil {
				return "", fmt.Errorf("model %d has no provider configured", selected.ID)
			}
			resolvedModel = selected.Name
			providerName = selected.Provider.Name
			if config.MaxTokens == 0 && selected.MaxTokens > 0 {
				config.MaxTokens = selected.MaxTokens
			}
		}
	}

	// AIModel 级别 MaxTokens（novel.AIModel 路径）
	if config.MaxTokens == 0 && resolvedModel != "" && s.modelRepo != nil {
		if m, err := s.modelRepo.GetByName(resolvedModel); err == nil && m != nil && m.MaxTokens > 0 {
			config.MaxTokens = m.MaxTokens
		}
	}

	// provider 级别 MaxTokens（兜底，优先级低于 AIModel 配置）
	if config.MaxTokens == 0 && providerName != "" && s.providerRepo != nil {
		if providers, err := s.providerRepo.ListByTenant(tenantID); err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.MaxTokens > 0 {
					config.MaxTokens = p.MaxTokens
					break
				}
			}
		}
	}

	// 任务类型兜底 MaxTokens（同 GenerateWithProvider，仅在配置链全为 0 时生效）。
	if config.MaxTokens == 0 {
		switch taskType {
		case "storyboard":
			config.MaxTokens = 16384
		case "outline":
			config.MaxTokens = 16384
		case "chapter_review", "storyboard_review":
			config.MaxTokens = 8192
		case "storyboard_arc":
			config.MaxTokens = 2048
		}
	}

	sysPmt := ""
	if chapterTaskTypes[taskType] {
		sysPmt = novelWritingSystemPrompt
	} else if jsonOnlyTaskTypes[taskType] {
		sysPmt = jsonOnlySystemPrompt
	}

	ctxStart := time.Now()
	result, resp, err := s.callAIWithProviderSys(ctx, tenantID, prompt, sysPmt, &config, providerName, resolvedModel)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}
	s.logUsage(tenantID, &config, taskType, resp, time.Since(ctxStart).Milliseconds())
	return result, nil
}

// resolveTaskConfig 提取 GenerateWithProviderCtx 中的配置解析逻辑，供多轮/流式方法复用。
// 返回已填充好参数的 config、最终 providerName、resolvedModel。
func (s *AIService) resolveTaskConfig(tenantID uint, novelID uint, taskType string, providerName string, overrides []StoryboardOverrides) (model.TaskModelConfig, string, string) {
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{Temperature: 0.7, MaxTokens: 0}
	}
	config := *base
	resolvedModel := ""

	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	if len(overrides) > 0 {
		o := overrides[0]
		if o.MaxTokens > 0 {
			config.MaxTokens = o.MaxTokens
		}
		if o.Temperature > 0 {
			config.Temperature = o.Temperature
		}
		if o.TimeoutSeconds > 0 {
			config.TimeoutSeconds = o.TimeoutSeconds
		}
	}

	if providerName == "" && config.PrimaryProviderID > 0 && s.providerRepo != nil {
		if p, err := s.providerRepo.GetByID(config.PrimaryProviderID); err == nil && p != nil {
			providerName = p.Name
		}
	}

	if resolvedModel == "" && providerName == "" && config.Strategy != "" && s.modelRepo != nil {
		if candidates, err := s.modelRepo.GetAvailableByTaskType(taskType, tenantID); err == nil && len(candidates) > 0 {
			var selected *model.AIModel
			switch config.Strategy {
			case "quality_first":
				selected = selectByQuality(candidates)
			case "cost_first":
				selected = selectByCost(candidates)
			default:
				selected = selectBalanced(candidates)
			}
			if selected == nil {
				selected = selectBalanced(candidates)
			}
			if selected != nil && selected.Provider != nil {
				resolvedModel = selected.Name
				providerName = selected.Provider.Name
				if config.MaxTokens == 0 && selected.MaxTokens > 0 {
					config.MaxTokens = selected.MaxTokens
				}
			}
		}
	}

	if config.MaxTokens == 0 && resolvedModel != "" && s.modelRepo != nil {
		if m, err := s.modelRepo.GetByName(resolvedModel); err == nil && m != nil && m.MaxTokens > 0 {
			config.MaxTokens = m.MaxTokens
		}
	}

	if config.MaxTokens == 0 && providerName != "" && s.providerRepo != nil {
		if providers, err := s.providerRepo.ListByTenant(tenantID); err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.MaxTokens > 0 {
					config.MaxTokens = p.MaxTokens
					break
				}
			}
		}
	}

	return config, providerName, resolvedModel
}

// GenerateWithMessagesCtx calls the AI with a full conversation history (messages array).
// Unlike GenerateWithProviderCtx which takes a single prompt string, this method passes
// the complete message thread natively so the model sees proper role-based multi-turn context.
func (s *AIService) GenerateWithMessagesCtx(ctx context.Context, tenantID uint, taskType string, messages []ai.ChatMessage, systemPrompt string, overrides ...StoryboardOverrides) (string, error) {
	config, providerName, resolvedModel := s.resolveTaskConfig(tenantID, 0, taskType, "", overrides)

	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return "", fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
	}
	if resolvedModel != "" {
		req.Model = resolvedModel
	}
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}
	if req.MaxTokens == 0 && s.providerRepo != nil {
		if dbProviders, dbErr := s.providerRepo.ListByTenant(tenantID); dbErr == nil {
			resolvedName := provider.GetName()
			for _, p := range dbProviders {
				if p.Name == resolvedName && p.MaxTokens > 0 && p.TenantID == tenantID {
					req.MaxTokens = p.MaxTokens
					break
				}
			}
		}
	}

	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeoutDur)
	defer cancel()

	logger.Printf("[AI/chat] provider=%s maxTokens=%d temperature=%.2f msgs=%d calling...",
		provider.GetName(), req.MaxTokens, req.Temperature, len(messages))

	callStart := time.Now()
	resp, err := provider.Generate(tctx, req)
	elapsed := time.Since(callStart)
	if err != nil {
		logger.Errorf("[AI/chat] provider=%s elapsed=%s err=%v", provider.GetName(), elapsed.Round(time.Millisecond), err)
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	resp.FinishTime = elapsed.Milliseconds()
	logger.Printf("[AI/chat] provider=%s elapsed=%s respLen=%d in=%d out=%d",
		provider.GetName(), elapsed.Round(time.Millisecond), len(resp.Content), resp.InputTokens, resp.Tokens)
	if resp.Content == "" {
		return "", fmt.Errorf("provider returned empty content (stop_reason=%s)", resp.StopReason)
	}
	s.logUsage(tenantID, &config, taskType, resp, elapsed.Milliseconds())
	return resp.Content, nil
}

// StreamWithMessagesCtx streams AI response tokens for a multi-turn conversation.
// It returns a channel that emits content chunks; the caller must drain the channel fully.
// The last item may carry an empty Content with a non-empty Error field.
func (s *AIService) StreamWithMessagesCtx(ctx context.Context, tenantID uint, taskType string, messages []ai.ChatMessage, systemPrompt string, overrides ...StoryboardOverrides) (<-chan *ai.GenerateResponse, error) {
	config, providerName, resolvedModel := s.resolveTaskConfig(tenantID, 0, taskType, "", overrides)

	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		return nil, fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return nil, fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
		Stream:       true,
	}
	if resolvedModel != "" {
		req.Model = resolvedModel
	}
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}
	if req.MaxTokens == 0 && s.providerRepo != nil {
		if dbProviders, dbErr := s.providerRepo.ListByTenant(tenantID); dbErr == nil {
			resolvedName := provider.GetName()
			for _, p := range dbProviders {
				if p.Name == resolvedName && p.MaxTokens > 0 && p.TenantID == tenantID {
					req.MaxTokens = p.MaxTokens
					break
				}
			}
		}
	}

	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeoutDur)

	ch, err := provider.GenerateStream(streamCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Wrap the provider channel to ensure cancel is called when the stream ends.
	wrapped := make(chan *ai.GenerateResponse, 64)
	go func() {
		defer cancel()
		defer close(wrapped)
		for chunk := range ch {
			wrapped <- chunk
		}
	}()

	return wrapped, nil
}

// resolveProviderFromModel 通过模型名（如 "deepseek-chat"）在 DB 中查找对应的 provider name（如 "deepseek"）
// 若查找失败则静默返回空字符串（由 getTenantProvider 兜底选择第一个可用 provider）
func (s *AIService) resolveProviderFromModel(tenantID uint, modelName string) string {
	if s.modelRepo == nil {
		return ""
	}
	m, err := s.modelRepo.GetByName(modelName)
	if err != nil || m == nil || m.Provider == nil {
		return ""
	}
	providerName := m.Provider.Name
	// 确认该 provider 对当前租户可用（有凭证）
	if s.providerRepo != nil {
		providers, err := s.providerRepo.ListByTenant(tenantID)
		if err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.IsActive && providerHasCredentials(p) {
					return providerName
				}
			}
		}
		return "" // provider 无凭证，让 getTenantProvider 自动选择
	}
	return providerName
}

// callAI 调用AI接口（使用系统级 provider）
func (s *AIService) callAI(prompt string, config *model.TaskModelConfig) (string, error) {
	return s.callAIWithProvider(context.Background(), 0, prompt, config, "")
}

// GenerateWithVision 使用 Vision AI 分析图像内容
// 优先使用 anthropic（claude-3），其次 openai（gpt-4o），都失败则用默认 provider
func (s *AIService) GenerateWithVision(prompt string, imageURLs []string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider
	var err error
	for _, name := range []string{"anthropic", "openai"} {
		provider, err = s.aiManager.GetProvider(name)
		if err == nil {
			break
		}
	}
	if err != nil {
		provider, err = s.aiManager.GetProvider("")
		if err != nil {
			return "", fmt.Errorf("failed to get AI provider for vision: %w", err)
		}
	}

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{
			{
				Role:      "user",
				Content:   prompt,
				ImageURLs: imageURLs,
			},
		},
		Temperature: 0.1,
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	return resp.Content, nil
}

// callAIWithProvider 调用指定 Provider 的 AI 接口
// parentCtx 作为父 context；timeout 会在其上叠加（不会超出父 context 的 deadline）。
// modelOverride 可选，非空时会覆盖 provider 的默认模型（用于小说项目级 ai_model 配置）
// novelWritingSystemPrompt is injected as the system role for all chapter/scene writing tasks.
// It prevents the model from adding preambles, outlines, or meta-commentary.
const novelWritingSystemPrompt = `你是一个小说正文生成引擎，只负责按指令输出小说正文内容。

严格规则（任何情况下不得违反）：
- 输出内容只能是小说正文，从章节标题行开始，到正文自然结束为止
- 禁止任何开场白，如"好的""当然可以""非常抱歉""由于篇幅限制"等
- 禁止在正文外输出大纲、章节摘要、写作建议或元注释
- 禁止声明字数/篇幅限制，禁止请求用户确认续写
- 禁止在正文结束后追加任何说明文字
- 字数不足时直接写到章末钩子，不得截断并附注"待续"类说明`

// jsonOnlySystemPrompt is injected for structured JSON output tasks.
// It suppresses chain-of-thought reasoning that reasoning models (e.g. DeepSeek-R1) emit by default.
const jsonOnlySystemPrompt = `你是一个严格的JSON生成引擎。

规则（任何情况下不得违反）：
- 只输出纯JSON，不输出任何分析、推理、思考过程或说明文字
- 禁止输出"我们被要求""根据分析""综上所述"等任何前缀或后缀
- 禁止markdown代码块（不要用` + "```" + `包裹）
- 直接以 [ 或 { 开始，以 ] 或 } 结束
- 每个键值对必须用英文冒号 : 分隔（"key": value），不得省略冒号
- 禁止添加 schema 示例中未定义的额外字段`

// chapterTaskTypes is the set of task types that generate novel prose.
var chapterTaskTypes = map[string]bool{
	"chapter": true, "chapter_outline": true, "scene_outline": true,
}

// jsonOnlyTaskTypes is the set of task types that must output pure JSON.
// These tasks get jsonOnlySystemPrompt injected to suppress reasoning model chain-of-thought.
var jsonOnlyTaskTypes = map[string]bool{
	"storyboard": true, "character": true, "worldview": true,
	"character_state": true, "scene_anchor_extract": true,
	"storyboard_review": true, "sfx_analyze": true,
	"chapter_review": true, "extract_items": true,
	"outline": true, // 大纲生成：强制纯 JSON，防止 DeepSeek 输出思考过程或缺失冒号
	// 角色/物品/世界观提取——均输出 JSON，需抑制推理模型的思维链输出
	"extract_characters":       true,
	"extract_character_names":  true,
	"consolidate_character_names": true,
	"generate_character_profile": true,
	"extract_minor_characters": true,
	"extract_chapter_items":    true,
	"extract_worldview":        true,
	"extract_foreshadows":      true,
}

func (s *AIService) callAIWithProvider(parentCtx context.Context, tenantID uint, prompt string, config *model.TaskModelConfig, providerName string, modelOverride ...string) (string, error) {
	content, _, err := s.callAIWithProviderSys(parentCtx, tenantID, prompt, "", config, providerName, modelOverride...)
	return content, err
}

func (s *AIService) callAIWithProviderSys(parentCtx context.Context, tenantID uint, prompt string, systemPrompt string, config *model.TaskModelConfig, providerName string, modelOverride ...string) (string, *ai.GenerateResponse, error) {
	if s.aiManager == nil {
		return "", nil, fmt.Errorf("AI manager not initialized")
	}

	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		logger.Errorf("callAIWithProvider: getTenantProvider failed (tenant=%d, provider=%q): %v", tenantID, providerName, err)
		return "", nil, fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return "", nil, fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}

	req := &ai.GenerateRequest{
		Messages:     []ai.ChatMessage{{Role: "user", Content: prompt}},
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
	}
	if len(modelOverride) > 0 && modelOverride[0] != "" {
		req.Model = modelOverride[0]
	}
	// 兜底：req.MaxTokens == 0 时从 DB provider 记录补充。
	// 上层 GenerateWithProvider/Ctx 中 provider 级 MaxTokens 查找有 providerName!="" 前置条件，
	// 当调用方传 providerName="" 时会被跳过。此处用已解析 provider 的名称精确匹配，覆盖所有路径。
	if req.MaxTokens == 0 && s.providerRepo != nil {
		if dbProviders, dbErr := s.providerRepo.ListByTenant(tenantID); dbErr == nil {
			resolvedName := provider.GetName()
			var sysMT int
			for _, p := range dbProviders {
				if p.Name != resolvedName || p.MaxTokens <= 0 {
					continue
				}
				if p.TenantID == tenantID {
					req.MaxTokens = p.MaxTokens
					break
				}
				if sysMT == 0 {
					sysMT = p.MaxTokens // 系统级兜底
				}
			}
			if req.MaxTokens == 0 {
				req.MaxTokens = sysMT
			}
		}
	}
	// Claude 不支持 top_k，仅在非 Anthropic provider 时传入
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}

	// 超时：优先使用 config.TimeoutSeconds（由调用方注入），否则默认 5 分钟。
	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeoutDur)
	defer cancel()

	logger.Printf("[AI] provider=%s maxTokens=%d temperature=%.2f calling...", provider.GetName(), req.MaxTokens, req.Temperature)
	callStart := time.Now()
	resp, err := provider.Generate(ctx, req)
	elapsed := time.Since(callStart)
	if err != nil {
		logger.Errorf("[AI] provider=%s elapsed=%s err=%v", provider.GetName(), elapsed.Round(time.Millisecond), err)
		// Fallback chain: try models listed in config.FallbackModelIDs
		if fbContent, fbResp, fbErr := s.tryFallbackModels(ctx, tenantID, req, config, elapsed); fbErr == nil {
			return fbContent, fbResp, nil
		}
		return "", nil, err
	}
	if resp.Error != "" {
		logger.Errorf("[AI] provider=%s elapsed=%s providerErr=%s", provider.GetName(), elapsed.Round(time.Millisecond), resp.Error)
		return "", nil, fmt.Errorf("provider error: %s", resp.Error)
	}
	resp.FinishTime = elapsed.Milliseconds()
	logger.Printf("[AI] provider=%s elapsed=%s maxTokens=%d respLen=%d in=%d out=%d stopReason=%q",
		provider.GetName(), elapsed.Round(time.Millisecond), req.MaxTokens, len(resp.Content),
		resp.InputTokens, resp.Tokens, resp.StopReason)

	if resp.Content == "" {
		return "", nil, fmt.Errorf("provider returned empty content (stop_reason=%s)", resp.StopReason)
	}
	return resp.Content, resp, nil
}

// tryFallbackModels iterates config.FallbackModelIDs and attempts generation on each.
// Fix 3: Added circular-reference detection (visitedIDs) and aggregated error reporting.
func (s *AIService) tryFallbackModels(ctx context.Context, tenantID uint, origReq *ai.GenerateRequest, config *model.TaskModelConfig, _ time.Duration) (string, *ai.GenerateResponse, error) {
	if config.FallbackModelIDs == "" || s.modelRepo == nil {
		return "", nil, fmt.Errorf("no fallback")
	}
	var ids []uint
	if err := json.Unmarshal([]byte(config.FallbackModelIDs), &ids); err != nil || len(ids) == 0 {
		return "", nil, fmt.Errorf("no fallback")
	}
	const maxFallbackDepth = 5
	if len(ids) > maxFallbackDepth {
		ids = ids[:maxFallbackDepth]
		logger.Printf("[AI] fallback chain truncated to %d levels", maxFallbackDepth)
	}
	visitedIDs := make(map[uint]bool)
	var errs []string
	for _, id := range ids {
		if visitedIDs[id] {
			return "", nil, fmt.Errorf("circular fallback chain detected at model %d", id)
		}
		visitedIDs[id] = true
		m, err := s.modelRepo.GetByID(id)
		if err != nil || m == nil || m.Provider == nil {
			errs = append(errs, fmt.Sprintf("model_%d: not found or no provider", id))
			continue
		}
		fbProvider, err := s.getTenantProvider(tenantID, m.Provider.Name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("model_%d: provider load error: %v", id, err))
			continue
		}
		fbReq := *origReq
		fbReq.Model = m.Name
		start := time.Now()
		fbResp, err := fbProvider.Generate(ctx, &fbReq)
		elapsed := time.Since(start)
		if err == nil && fbResp.Error == "" {
			fbResp.FinishTime = elapsed.Milliseconds()
			// Fix 4: 记录实际执行的模型 DB ID，供 logUsage 精确写入 usage log
			fbResp.ActualModelID = m.ID
			logger.Printf("[AI fallback] provider=%s model=%s elapsed=%s", m.Provider.Name, m.Name, elapsed.Round(time.Millisecond))
			return fbResp.Content, fbResp, nil
		}
		fbErr := err
		if fbErr == nil && fbResp.Error != "" {
			fbErr = fmt.Errorf("%s", fbResp.Error)
		}
		// 认证失败（401/403）不值得继续尝试其他 fallback，立即返回
		if fbErr != nil && isAuthError(fbErr) {
			return "", nil, fmt.Errorf("fallback provider %q authentication failed (not retrying): %w", m.Provider.Name, fbErr)
		}
		errs = append(errs, fmt.Sprintf("model_%d: %v", id, fbErr))
		logger.Errorf("[AI fallback] provider=%s model=%s err=%v", m.Provider.Name, m.Name, fbErr)
	}
	return "", nil, fmt.Errorf("all fallbacks exhausted: %s", strings.Join(errs, "; "))
}

// isAuthError returns true when the error clearly indicates an authentication
// or authorisation failure (HTTP 401/403, invalid API key, etc.).
// These errors are non-retryable and should short-circuit any fallback chain.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401") ||
		strings.Contains(s, "403") ||
		strings.Contains(s, "authentication") ||
		strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "Unauthorized") ||
		strings.Contains(s, "invalid_api_key") ||
		strings.Contains(s, "invalid api key") ||
		strings.Contains(s, "Forbidden")
}

// generateJSONForTenant 带 tenantID 的 JSON 生成重试（最多重试 maxRetries 次）
func (s *AIService) generateJSONForTenant(tenantID, novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	return s.generateJSONForTenantCtx(context.Background(), tenantID, novelID, taskType, prompt, maxRetries)
}

// generateJSONForTenantCtx 与 generateJSONForTenant 相同，但支持 context 取消/超时。
func (s *AIService) generateJSONForTenantCtx(ctx context.Context, tenantID, novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("generateJSONForTenantCtx: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.GenerateWithProviderCtx(ctx, tenantID, novelID, taskType, p, "")
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = err
			// 4xx provider errors (e.g. "max_tokens too large") are not retryable
			if strings.Contains(err.Error(), "provider error:") {
				logger.Errorf("generateJSONForTenantCtx: non-retryable provider error on attempt %d taskType=%s: %v", attempt+1, taskType, err)
				break
			}
			continue
		}
		cleaned := extractJSONAuto(result)
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		logger.Errorf("generateJSONForTenantCtx: %v", lastErr)
	}
	return "", fmt.Errorf("generateJSONForTenantCtx failed after %d attempts: %w", maxRetries+1, lastErr)
}

// generateWithRetry 带容错重试的 JSON 生成（最多重试 2 次）
func (s *AIService) generateWithRetry(novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("generateWithRetry: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.Generate(novelID, taskType, p)
		if err != nil {
			lastErr = err
			continue
		}
		// 尝试提取 JSON
		cleaned := extractJSON(result)
		// 验证是否为有效 JSON
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		logger.Errorf("generateWithRetry: %v", lastErr)
	}
	return "", fmt.Errorf("generateWithRetry failed after %d attempts: %w", maxRetries+1, lastErr)
}

// logUsage records a ModelUsageLog entry using token counts and latency from the response.
// Fix 1: accepts tenantID and uses resp.ActualModelID when available (Fix 4).
func (s *AIService) logUsage(tenantID uint, config *model.TaskModelConfig, taskType string, resp *ai.GenerateResponse, latencyMs int64) {
	if s.modelRepo == nil || resp == nil {
		return
	}
	modelID := config.PrimaryModelID
	if resp.ActualModelID > 0 {
		modelID = resp.ActualModelID
	}
	entry := &model.ModelUsageLog{
		TenantID:     tenantID,
		ModelID:      modelID,
		TaskType:     taskType,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.Tokens,
		TotalTokens:  resp.InputTokens + resp.Tokens,
		Cost:         float64(resp.InputTokens+resp.Tokens) / 1000 * config.MaxCostPerTask,
		Latency:      float64(latencyMs) / 1000,
		Success:      true,
	}
	if err := s.modelRepo.LogUsage(entry); err != nil {
		logger.Errorf("[AI] logUsage failed: %v", err)
	}
}

// GenerateImage 调用AI生成图像
func (s *AIService) GenerateImage(prompt string, options *ImageGenerationOptions) (*GeneratedImage, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.aiManager.GetProvider("")
	if err != nil {
		return nil, fmt.Errorf("get AI provider failed: %w", err)
	}
	req := &ai.ImageGenerateRequest{
		Prompt:         prompt,
		NegativePrompt: options.NegativePrompt,
		Size:           options.Size,
		Steps:          options.Steps,
		CFGScale:       options.CFGScale,
	}
	resp, err := provider.ImageGenerate(context.Background(), req)
	if err != nil {
		return nil, err
	}
	persistURL := s.uploadImageToStorage(context.Background(), 0, resp.URL)
	return &GeneratedImage{
		URL:    persistURL,
		Width:  resp.Width,
		Height: resp.Height,
	}, nil
}

// knownImageCapableProviders 已知支持图像生成的提供者及其默认模型，用于 DB 动态加载的回退路径。
var knownImageCapableProviders = []ai.ImageProviderEntry{
	{ProviderName: "doubao", Model: "seedream-3-0-t2i-250415", Size: "2048x2048"},
	{ProviderName: "qianwen", Model: "wanx2.1-t2i-turbo", Size: "1024x1024"},
	{ProviderName: "openai", Model: "dall-e-3", Size: "1792x1024"},
	{ProviderName: "volcengine-visual", Model: ai.VolcModelText2ImgV3, Size: "2048x2048"},
}

// loadDBImageProviderEntries 从 DB 加载 IMAGE 类型的提供者列表，使用实际配置的模型名称（APIVersion）。
// 避免 knownImageCapableProviders 硬编码模型与用户实际配置不匹配的问题。
// volcengine-visual 排在末尾：它需要服务端下载参考图，私有 OSS URL 会导致 403 失败。
func (s *AIService) loadDBImageProviderEntries(tenantID uint) []ai.ImageProviderEntry {
	if s.providerRepo == nil {
		return nil
	}
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil
	}
	defaultSizeMap := map[string]string{
		"doubao":                        "2048x2048",
		"qianwen":                       "1024x1024",
		"openai":                        "1792x1024",
		ai.ProviderNameVolcengineVisual: "2048x2048",
	}
	var primary, volcengine []ai.ImageProviderEntry
	seen := map[string]bool{}
	for _, p := range providers {
		if !p.IsActive {
			logger.Printf("loadDBImageProviderEntries: skip provider %q (inactive)", p.Name)
			continue
		}
		pt := strings.ToLower(p.Type)
		if pt != "image" {
			continue // non-image providers are expected, no need to log
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBImageProviderEntries: skip IMAGE provider %q (missing credentials)", p.Name)
			continue
		}
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		size := defaultSizeMap[p.Name]
		if size == "" {
			size = "1024x1024"
		}
		entry := ai.ImageProviderEntry{ProviderName: p.Name, Model: p.APIVersion, Size: size}
		logger.Printf("loadDBImageProviderEntries: adding IMAGE provider %q model=%q size=%s (tenantID=%d)", p.Name, p.APIVersion, size, tenantID)
		// volcengine-visual 依赖服务端下载参考图，排到最后作为兜底
		if p.Name == ai.ProviderNameVolcengineVisual {
			volcengine = append(volcengine, entry)
		} else {
			primary = append(primary, entry)
		}
	}
	result := append(primary, volcengine...)
	if len(result) == 0 {
		logger.Printf("loadDBImageProviderEntries: no IMAGE providers found for tenantID=%d (total providers checked: %d)", tenantID, len(providers))
	}
	return result
}

// isRealisticStyle 判断给定风格字符串是否属于写实/摄影类风格。
// 支持中英文：realistic / photorealistic / photography / 写实 / 真实 / 摄影
func isRealisticStyle(style string) bool {
	s := strings.ToLower(style)
	return s == "realistic" || s == "real_person" ||
		strings.Contains(s, "realistic") ||
		strings.Contains(s, "photorealistic") || strings.Contains(s, "photography") ||
		strings.Contains(s, "写实") || strings.Contains(s, "真实") || strings.Contains(s, "摄影") ||
		strings.Contains(s, "真人")
}

// selectImageModel returns the model to use for the given entry.
// For volcengine-visual: referenceImage → DreamO; style == realistic → PortraitPhoto.
// selectImageModel 根据提供者、参考图、风格和一致性权重选择合适的图像生成模型。
// consistencyWeight: 0-1，≥0.7 使用 DreamO（角色特征保持），<0.7 使用 SeedEditV3（指令编辑）
// klingResolutionExtra 当 provider 是 kling-image 且目标尺寸 ≥ 2K（较长边 ≥ 1536px）时，
// 自动返回 Extra{"resolution": "2k"} 以启用 Kling 2K 高清生成模式。
// 对其他 provider 返回 nil（Volcengine 等直接通过 width/height 控制分辨率）。
func klingResolutionExtra(providerName, size string) map[string]interface{} {
	if providerName != "kling-image" {
		return nil
	}
	var w, h int
	fmt.Sscanf(size, "%dx%d", &w, &h)
	maxSide := w
	if h > maxSide {
		maxSide = h
	}
	if maxSide >= 1536 {
		return map[string]interface{}{"resolution": "2k"}
	}
	return nil
}

func selectImageModel(entry ai.ImageProviderEntry, referenceImage, style string, consistencyWeight ...float64) string {
	if entry.ProviderName == ai.ProviderNameVolcengineVisual {
		// volcengine-visual 始终用内置 req_key，不依赖用户填写的 APIVersion
		if referenceImage != "" {
			// 写实风格：即使有参考图也使用 PortraitPhoto，保证生成真实感肖像
			if isRealisticStyle(style) {
				return ai.VolcModelPortraitPhoto
			}
			weight := 1.0
			if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
				weight = consistencyWeight[0]
			}
			if weight >= 0.7 {
				return ai.VolcModelDreamO
			}
			return ai.VolcModelSeedEditV3
		}
		// 无参考图时：PortraitPhoto 是 I2I 模型，必须有 image_input 才能正常工作；
		// 无论什么风格都使用 Text2ImgV3（文生图），这样 prompt/negative_prompt 才能完整生效。
		return ai.VolcModelText2ImgV3
	}
	return entry.Model
}

// sanitizeImagePrompt 移除中文角色描述中常见的内容审核触发词，用于
// Seedream 等对中文敏感词检查严格的模型，在首次被拦截后重试时使用。
func sanitizeImagePrompt(prompt string) string {
	// 暴力/伤亡类：替换为中性词
	replacer := strings.NewReplacer(
		"杀人", "战斗",
		"杀死", "击败",
		"杀掉", "击败",
		"血腥", "激烈",
		"血流", "战斗",
		"鲜血", "红色",
		"伤口", "痕迹",
		"伤痕", "痕迹",
		"伤疤", "痕迹",
		"尸体", "倒地",
		"死亡", "消逝",
		"屠杀", "激战",
		"残忍", "严肃",
		"暴力", "力量",
		"凶器", "武器",
		"刺穿", "刺击",
		"砍断", "劈砍",
		"爆炸", "冲击",
		"炸弹", "装置",
		"毒药", "液体",
	)
	sanitized := replacer.Replace(prompt)
	// 截断超长 prompt，减少审核误判概率（超过 300 字时只保留前 300 字）
	runes := []rune(sanitized)
	if len(runes) > 300 {
		sanitized = string(runes[:300])
	}
	return sanitized
}

// isSensitiveContentError 判断图像生成错误是否为内容审核拦截。
func isSensitiveContentError(errMsg string) bool {
	return strings.Contains(errMsg, ai.ErrPrefixSensitiveContent) ||
		strings.Contains(errMsg, "InputTextSensitiveContentDetected")
}

// GenerateCharacterThreeView 使用图像生成 API 生成角色/场景视图图像。
// style: 图片风格（"realistic"/"anime"/"ink_painting" 等），影响 Volcengine 模型选择。
// 空字符串表示使用提供者默认模型。
// consistencyWeight（可选）: 0-1，角色一致性强度；默认 1.0（严格）。
//
//	≥0.7 → DreamO（角色特征保持），<0.7 → SeedEditV3（指令编辑，scale 线性映射 1-10）
func (s *AIService) GenerateCharacterThreeView(ctx context.Context, tenantID uint, providerName, prompt, referenceImage, style, negativePrompt, sizeOverride string, consistencyWeight ...float64) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	// 并发限流：若配置了 image_concurrency，则在此处等待令牌
	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	weight := 1.0
	if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
		weight = consistencyWeight[0]
	}
	// SeedEditV3 的 scale 参数范围 1-10；以 weight 线性映射
	cfgScale := 1.0 + weight*9.0

	// 指定提供者时：直接加载并调用，不走遍历逻辑
	if providerName != "" {
		// 找到对应的 entry（model/size）
		var entry *ai.ImageProviderEntry
		for _, e := range knownImageCapableProviders {
			if e.ProviderName == providerName {
				entry = &e
				break
			}
		}
		// 也在静态注册列表里找
		if entry == nil {
			for _, e := range s.aiManager.GetImageProviders() {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			return "", fmt.Errorf("unknown image provider: %s", providerName)
		}
		provider, err := s.aiManager.GetProvider(providerName)
		if err != nil {
			// 静态 manager 无此 provider，尝试 DB
			provider, err = s.getTenantProvider(tenantID, providerName)
			if err != nil {
				return "", fmt.Errorf("image provider %q not available: %w", providerName, err)
			}
		}
		sz := sizeOverride
		if sz == "" {
			sz = entry.Size
		}
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             selectImageModel(*entry, referenceImage, style, weight),
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              sz,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
			Extra:             klingResolutionExtra(entry.ProviderName, sz),
		})
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", fmt.Errorf("image generation failed: %s", resp.Error)
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}

	// DB 模式（providerRepo 存在）时：DB 是唯一权威来源，完全忽略静态 aiManager 的图像提供者。
	// 这样删除/更改 DB 中的提供者可以立即生效，不受 env/config.yaml 中通用 AI key 的干扰。
	// 纯静态模式（无 DB）：读 aiManager 静态列表，为空时回退硬编码表。
	var entries []ai.ImageProviderEntry
	useDB := s.providerRepo != nil
	if useDB {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("no image providers configured")
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if useDB {
			// 从 DB 动态加载提供者（带租户感知和缓存）
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		model := selectImageModel(e, referenceImage, style, weight)
		logger.Printf("GenerateCharacterThreeView: trying provider=%s model=%s refImage=%v", e.ProviderName, model, referenceImage != "")
		eSz := sizeOverride
		if eSz == "" {
			eSz = e.Size
		}
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             model,
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              eSz,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
			Extra:             klingResolutionExtra(e.ProviderName, eSz),
		})
		if err != nil {
			logger.Errorf("GenerateCharacterThreeView: provider=%s failed: %v", e.ProviderName, err)
			lastErr = err
			continue
		}
		if resp.Error != "" {
			if isSensitiveContentError(resp.Error) {
				sanitized := sanitizeImagePrompt(prompt)
				if sanitized != prompt {
					logger.Printf("GenerateCharacterThreeView: provider=%s sensitive content, retrying with sanitized prompt", e.ProviderName)
					resp2, err2 := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
						Model:             model,
						Prompt:            sanitized,
						NegativePrompt:    negativePrompt,
						Size:              eSz,
						ReferenceImage:    referenceImage,
						CFGScale:          cfgScale,
						ConsistencyWeight: weight,
						Extra:             klingResolutionExtra(e.ProviderName, eSz),
					})
					if err2 == nil && resp2.Error == "" {
						return s.uploadImageToStorage(ctx, tenantID, resp2.URL), nil
					}
				}
			}
			logger.Errorf("GenerateCharacterThreeView: provider=%s error: %s", e.ProviderName, resp.Error)
			lastErr = fmt.Errorf("image generation failed: %s", resp.Error)
			continue
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}
	return "", fmt.Errorf("no image provider available: %w", lastErr)
}

// GenerateCharacterThreeViewMulti 与 GenerateCharacterThreeView 相同，但支持传入多张参考图和自定义尺寸。
// referenceImages：多张参考图 URL，直接传给支持多图的 API（如 DreamO image_urls[]），无需调用方拼接合图。
// size：图片尺寸（"WxH" 格式，如 "1024x576"），覆盖提供者默认尺寸；为空时使用提供者默认值。
// 若 referenceImages 为空，退化为纯文本生成。
func (s *AIService) GenerateCharacterThreeViewMulti(ctx context.Context, tenantID uint, providerName, prompt string, referenceImages []string, style, negativePrompt, size string, consistencyWeight ...float64) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	weight := 1.0
	if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
		weight = consistencyWeight[0]
	}
	cfgScale := 1.0 + weight*9.0

	// 取第一张图作为 selectImageModel 的存在性判断
	firstRef := ""
	if len(referenceImages) > 0 {
		firstRef = referenceImages[0]
	}

	buildReq := func(model, entrySize string) *ai.ImageGenerateRequest {
		sz := size // 优先使用调用方传入的尺寸（基于 AspectRatio+QualityTier 计算）
		if sz == "" {
			sz = entrySize
		}
		return &ai.ImageGenerateRequest{
			Model:             model,
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              sz,
			ReferenceImage:    firstRef,
			ReferenceImages:   referenceImages,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		}
	}

	if providerName != "" {
		var entry *ai.ImageProviderEntry
		// DB 模式优先：使用 DB 中实际配置的模型名称
		if s.providerRepo != nil {
			for _, e := range s.loadDBImageProviderEntries(tenantID) {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			for _, e := range knownImageCapableProviders {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			for _, e := range s.aiManager.GetImageProviders() {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			return "", fmt.Errorf("unknown image provider: %s", providerName)
		}
		var provider ai.AIProvider
		var err error
		if s.providerRepo != nil {
			provider, err = s.getTenantProvider(tenantID, providerName)
		} else {
			provider, err = s.aiManager.GetProvider(providerName)
			if err != nil {
				provider, err = s.getTenantProvider(tenantID, providerName)
			}
		}
		if err != nil {
			return "", fmt.Errorf("image provider %q not available: %w", providerName, err)
		}
		resp, err := provider.ImageGenerate(ctx, buildReq(selectImageModel(*entry, firstRef, style, weight), entry.Size))
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", fmt.Errorf("image generation failed: %s", resp.Error)
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}

	var entries []ai.ImageProviderEntry
	useDB := s.providerRepo != nil
	if useDB {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no image providers configured")
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if useDB {
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		model := selectImageModel(e, firstRef, style, weight)
		logger.Printf("GenerateCharacterThreeViewMulti: trying provider=%s model=%s refs=%d", e.ProviderName, model, len(referenceImages))
		resp, err := provider.ImageGenerate(ctx, buildReq(model, e.Size))
		if err != nil {
			logger.Errorf("GenerateCharacterThreeViewMulti: provider=%s failed: %v", e.ProviderName, err)
			lastErr = err
			continue
		}
		if resp.Error != "" {
			// 内容审核拦截：净化 prompt 后用同一 provider 重试一次
			if isSensitiveContentError(resp.Error) {
				sanitized := sanitizeImagePrompt(prompt)
				if sanitized != prompt {
					logger.Printf("GenerateCharacterThreeViewMulti: provider=%s sensitive content, retrying with sanitized prompt", e.ProviderName)
					origBuildReq := buildReq
					buildReqSanitized := func(model, sz string) *ai.ImageGenerateRequest {
						r := origBuildReq(model, sz)
						r.Prompt = sanitized
						return r
					}
					resp2, err2 := provider.ImageGenerate(ctx, buildReqSanitized(model, e.Size))
					if err2 == nil && resp2.Error == "" {
						return s.uploadImageToStorage(ctx, tenantID, resp2.URL), nil
					}
				}
			}
			logger.Errorf("GenerateCharacterThreeViewMulti: provider=%s error: %s", e.ProviderName, resp.Error)
			lastErr = fmt.Errorf("image generation failed: %s", resp.Error)
			continue
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}
	return "", fmt.Errorf("no image provider available: %w", lastErr)
}

// imageStorageHintKey is the context key for ImageStorageHint.
type imageStorageHintKey struct{}

// ImageStorageHint carries novel/chapter metadata for OSS key building.
type ImageStorageHint struct {
	NovelTitle string
	ChapterNo  int // 0 = novel-level, non-zero = chapter-level
}

// WithImageStorageHint enriches a context with novel/chapter metadata for OSS key building.
func WithImageStorageHint(ctx context.Context, hint ImageStorageHint) context.Context {
	return context.WithValue(ctx, imageStorageHintKey{}, hint)
}

// uploadImageToStorage 下载 AI 模型返回的临时图片 URL 并上传到持久存储（OSS/本地/DB）。
// storageSvc 为 nil 或上传失败时降级返回原 imgURL（非致命）。
// OSS key 格式：
//   - 有小说+章节信息：novels/{title}/chapters/{no}/images/{uuid}.ext
//   - 有小说信息：     novels/{title}/images/{uuid}.ext
//   - 无信息（降级）：  images/{tenantID}/{uuid}.ext
func (s *AIService) uploadImageToStorage(ctx context.Context, tenantID uint, imgURL string) string {
	if s.storageSvc == nil || imgURL == "" {
		return imgURL
	}
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, imgURL, nil)
	if err != nil {
		logger.Errorf("uploadImageToStorage: build request: %v", err)
		return imgURL
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorf("uploadImageToStorage: download %s: %v", imgURL, err)
		return imgURL
	}
	defer resp.Body.Close()
	const maxImageSize = 50 << 20 // 50 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		logger.Errorf("uploadImageToStorage: read body: %v", err)
		return imgURL
	}
	if len(data) > maxImageSize {
		logger.Printf("uploadImageToStorage: image too large (>50MB) from %s", imgURL)
		return imgURL
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = "image/jpeg"
	}
	ext := imageExtFromContentType(ct)
	filename := uuid.New().String() + ext
	logger.Printf("uploadImageToStorage: generated filename=%q from imgURL=%q", filename, imgURL)

	key := fmt.Sprintf("images/%s", filename)

	persistURL, uploadErr := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), ct)
	if uploadErr != nil {
		logger.Errorf("uploadImageToStorage: upload failed (falling back to original URL): %v", uploadErr)
		return imgURL
	}
	logger.Printf("uploadImageToStorage: persisted %s → %s", imgURL, persistURL)
	return persistURL
}

// WithServerBaseURL 设置本地服务器基础 URL（如 "http://127.0.0.1:8080"），用于将相对媒体路径
// 转换为可下载的绝对 URL（DB 存储返回 /api/v1/media/xxx 时需要此配置）。
func (s *AIService) WithServerBaseURL(baseURL string) {
	s.serverBaseURL = strings.TrimRight(baseURL, "/")
}

// fetchImageAsBase64 下载图片并返回 base64 编码的原始数据（不含 data URI 前缀）。
// 支持绝对 URL（https://...）和相对路径（/api/v1/media/xxx，需已配置 serverBaseURL）。
// 下载失败时返回空字符串，由调用方决定是否降级。
func (s *AIService) fetchImageAsBase64(ctx context.Context, imageURL string) string {
	if imageURL == "" {
		return ""
	}
	fetchURL := imageURL
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		if s.serverBaseURL == "" {
			logger.Printf("fetchImageAsBase64: relative URL %q but serverBaseURL not set", imageURL)
			return ""
		}
		fetchURL = s.serverBaseURL + "/" + strings.TrimLeft(imageURL, "/")
	}
	dlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		logger.Errorf("fetchImageAsBase64: build request for %s: %v", fetchURL, err)
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorf("fetchImageAsBase64: download %s: %v", fetchURL, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Printf("fetchImageAsBase64: HTTP %d for %s", resp.StatusCode, fetchURL)
		return ""
	}
	const maxFetchSize = 20 << 20 // 20 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchSize+1))
	if err != nil {
		logger.Errorf("fetchImageAsBase64: read body: %v", err)
		return ""
	}
	if len(data) > maxFetchSize {
		logger.Printf("fetchImageAsBase64: image too large (>20MB) from %s", fetchURL)
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// EditImageWithInstruction 使用文生图模型重新生成图片，将原图作为参考图保持风格/角色一致性。
// instruction 作为完整提示词驱动新构图，原图作为 reference image 保持视觉一致性。
// 自动选用第一个可用的图片生成 provider，不限定具体厂商。
func (s *AIService) EditImageWithInstruction(ctx context.Context, tenantID uint, imageURL, instruction string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}
	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// 将图片转为 base64，确保图片提供商服务器能取到数据
	imgInput := s.fetchImageAsBase64(ctx, imageURL)
	if imgInput == "" {
		imgInput = imageURL
		logger.Errorf("EditImageWithInstruction: base64 fetch failed, falling back to URL: %s", imageURL)
	}

	req := &ai.ImageGenerateRequest{
		Prompt:         instruction,
		ReferenceImage: imgInput,
	}

	var entries []ai.ImageProviderEntry
	if s.providerRepo != nil {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if s.providerRepo != nil {
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		req.Model = selectImageModel(e, imgInput, "")
		resp, err := provider.ImageGenerate(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Error != "" {
			lastErr = fmt.Errorf("%s", resp.Error)
			continue
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no image provider available for image editing")
}

// imageExtFromContentType 根据 Content-Type 返回图片文件扩展名。
func imageExtFromContentType(ct string) string {
	switch {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return ".jpg"
	case strings.Contains(ct, "png"):
		return ".png"
	case strings.Contains(ct, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

// AudioGenerate 调用默认 AI provider 生成 TTS 音频，返回本地文件路径（file:// URL）
func (s *AIService) AudioGenerate(ctx context.Context, text, voice string) (string, error) {
	return s.AudioGenerateWithOptions(ctx, 0, text, voice, 1.0, "")
}

// AudioGenerateWithOptions 支持语速、风格和语言/方言的 TTS 生成。
// Provider 选取顺序（DB 模式优先，与图像生成保持一致）：
//  1. DB 中租户配置的 voice/tts 类型 provider
//  2. config.yaml ai.tasks.tts 指定的 provider（静态模式兜底）
//  3. env-var 注册的默认 provider（静态模式兜底）
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, text, voice string, speed float64, style string, language ...string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider

	// 1. DB 模式：扫描 voice/tts 类型的 provider（与图像生成逻辑对称）
	if s.providerRepo != nil {
		if p, err := s.loadDBVoiceProvider(tenantID); err == nil && p != nil {
			provider = p
		}
	}

	// 2. 静态模式：config.yaml task routing
	if provider == nil && s.taskRouting.TTS != "" {
		if p, err := s.aiManager.GetProvider(s.taskRouting.TTS); err == nil {
			provider = p
		}
	}
	// 注意：不再兜底到默认 LLM provider，LLM 提供商通常不支持 /audio/speech 接口，
	// 兜底只会产生 404 错误，不如直接给用户明确的配置提示。

	if provider == nil {
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key")
	}

	if voice == "" {
		voice = "alloy"
	}
	if speed <= 0 {
		speed = 1.0
	}
	lang := ""
	if len(language) > 0 {
		lang = language[0]
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     text,
		Voice:    voice,
		Speed:    speed,
		Emotion:  style,
		Language: lang,
	})
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

// GenerateSFX 使用 DB 中配置的 sfx 类型提供商生成音效，返回 CDN URL 和时长（秒）。
// prompt: 音效描述，如 "春节烟花声"；duration: 期望时长（秒，3.0~10.0）。
func (s *AIService) GenerateSFX(ctx context.Context, tenantID uint, prompt string, duration float64) (string, float64, error) {
	provider, err := s.loadDBSFXProvider(tenantID)
	if err != nil {
		return "", 0, err
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     prompt,
		Duration: duration,
	})
	if err != nil {
		return "", 0, err
	}
	return resp.URL, resp.Duration, nil
}

// HasSFXProvider 判断当前租户是否已配置可用的文生音效提供商。
func (s *AIService) HasSFXProvider(tenantID uint) bool {
	_, err := s.loadDBSFXProvider(tenantID)
	return err == nil
}

// GenerateSFXWithProvider 使用指定名称的 sfx 提供商生成音效（从 DB 加载密钥）。
// 用于前端明确选择某个提供商（如 "elevenlabs-sfx"）时的强制路由。
func (s *AIService) GenerateSFXWithProvider(ctx context.Context, tenantID uint, providerName string, prompt string, duration float64) (string, float64, error) {
	p, err := s.loadDBProviderByName(tenantID, providerName)
	if err != nil {
		return "", 0, err
	}
	resp, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{Text: prompt, Duration: duration})
	if err != nil {
		return "", 0, err
	}
	return resp.URL, resp.Duration, nil
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

// loadDBSFXProvider 从 DB 中取第一个有效的 sfx 类型提供商（文生音效）。
func (s *AIService) loadDBSFXProvider(tenantID uint) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if pt := strings.ToLower(p.Type); pt != "sfx" {
			continue
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBSFXProvider: skip sfx provider %q (missing credentials)", p.Name)
			continue
		}
		provider, err := s.getTenantProvider(tenantID, p.Name)
		if err != nil {
			logger.Errorf("loadDBSFXProvider: failed to instantiate provider %q: %v", p.Name, err)
			continue
		}
		logger.Printf("loadDBSFXProvider: using sfx provider %q", p.Name)
		return provider, nil
	}
	return nil, fmt.Errorf("no sfx providers configured in DB")
}

// loadDBVoiceProvider 从 DB 中取第一个有效的 voice/tts 类型提供商。
func (s *AIService) loadDBVoiceProvider(tenantID uint) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		t := strings.ToLower(p.Type)
		if t != "voice" && t != "tts" {
			continue
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBVoiceProvider: skip voice provider %q (missing credentials)", p.Name)
			continue
		}
		provider, err := s.getTenantProvider(tenantID, p.Name)
		if err != nil {
			logger.Errorf("loadDBVoiceProvider: failed to instantiate provider %q: %v", p.Name, err)
			continue
		}
		logger.Printf("loadDBVoiceProvider: using voice provider %q model=%q", p.Name, p.APIVersion)
		return provider, nil
	}
	return nil, fmt.Errorf("no voice providers configured in DB")
}

// GetBGMProviderCreds 从 DB 中取指定 music 类型提供商的凭据（apiKey, endpoint）。
// 找不到返回空字符串；调用方负责判断空值。
func (s *AIService) GetBGMProviderCreds(tenantID uint, name string) (apiKey, endpoint string) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return "", ""
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if strings.ToLower(p.Type) != "music" {
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
			logger.Errorf("GetBGMProviderCreds: decrypt APIKey for %q: %v", p.Name, decErr)
			return "", ""
		}
		return key, p.APIEndpoint
	}
	return "", ""
}

// GetSFXProviderCreds 从 DB 中取指定 sfx 类型提供商的凭据（apiKey, endpoint）。
// 找不到返回空字符串；调用方负责判断空值。
func (s *AIService) GetSFXProviderCreds(tenantID uint, name string) (apiKey, endpoint string) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return "", ""
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if strings.ToLower(p.Type) != "sfx" {
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
			logger.Errorf("GetSFXProviderCreds: decrypt APIKey for %q: %v", p.Name, decErr)
			return "", ""
		}
		return key, p.APIEndpoint
	}
	return "", ""
}

// GetTenantVideoProvider 从 DB 中查找指定租户已配置的视频生成提供商。
// name 为空时返回第一个可用的视频提供商（kling 优先）。
func (s *AIService) GetTenantVideoProvider(tenantID uint, name string) (ai.VideoProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	// 按照 kling → seedance 顺序优先选择
	preferOrder := []string{"kling", "seedance"}
	if name != "" {
		preferOrder = []string{strings.ToLower(name)}
	}
	byName := make(map[string]*model.ModelProvider)
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if strings.ToLower(p.Type) != "video" {
			continue
		}
		if !providerHasCredentials(p) {
			continue
		}
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
		case "kling":
			return ai.NewKlingProvider(apiKey, apiSecretKey, p.APIEndpoint), nil
		case "seedance":
			return ai.NewSeedanceProvider(apiKey, p.APIEndpoint), nil
		}
	}
	if name != "" {
		return nil, fmt.Errorf("video provider %q not configured for tenant %d", name, tenantID)
	}
	return nil, fmt.Errorf("no video provider configured for tenant %d", tenantID)
}

// UpscaleImage 放大图片。method 为 "ai" 时调用 AI 增强，否则使用 CatmullRom 双三次插值。
// scale 为整数倍放大系数（建议 2 或 4，最大 8）。
func (s *AIService) UpscaleImage(ctx context.Context, tenantID, novelID uint, imageURL string, scale int, method string) (string, error) {
	if scale <= 1 {
		scale = 2
	}
	if scale > 8 {
		scale = 8
	}

	// 下载原图（两种模式共用）
	data, contentType, err := s.downloadImageBytes(ctx, imageURL)
	if err != nil {
		return "", fmt.Errorf("upscale: %w", err)
	}

	// 解码获取尺寸（两种模式均需要）
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("upscale: decode image: %w", err)
	}
	srcB := src.Bounds()
	dstW := srcB.Dx() * scale
	dstH := srcB.Dy() * scale

	if method == "ai" {
		return s.upscaleImageAI(ctx, tenantID, novelID, data, imageURL, dstW, dstH)
	}
	return s.upscaleImageBicubic(ctx, src, srcB, format, contentType, dstW, dstH)
}

// downloadImageBytes 下载图片到内存，返回 (data, contentType, error)。
// 支持绝对 URL 和相对路径（/api/v1/media/xxx），相对路径需已配置 serverBaseURL。
func (s *AIService) downloadImageBytes(ctx context.Context, imageURL string) ([]byte, string, error) {
	fetchURL := imageURL
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		if s.serverBaseURL == "" {
			return nil, "", fmt.Errorf("relative URL %q but serverBaseURL not configured", imageURL)
		}
		fetchURL = s.serverBaseURL + "/" + strings.TrimLeft(imageURL, "/")
	}
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	const maxSize = 50 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxSize {
		return nil, "", fmt.Errorf("image too large (>50MB)")
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return data, ct, nil
}

// applySharpen 对放大后的 RGBA 图像应用 3×3 锐化卷积核，使边缘更清晰。
// 核：中心 5，上下左右各 -1，角不参与（等价于 USM 的快速近似）。
func applySharpen(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if x == b.Min.X || x == b.Max.X-1 || y == b.Min.Y || y == b.Max.Y-1 {
				dst.Set(x, y, src.At(x, y))
				continue
			}
			c := src.RGBAAt(x, y)
			t := src.RGBAAt(x, y-1)
			bm := src.RGBAAt(x, y+1)
			l := src.RGBAAt(x-1, y)
			r := src.RGBAAt(x+1, y)
			dst.SetRGBA(x, y, color.RGBA{
				R: clamp(5*int(c.R) - int(t.R) - int(bm.R) - int(l.R) - int(r.R)),
				G: clamp(5*int(c.G) - int(t.G) - int(bm.G) - int(l.G) - int(r.G)),
				B: clamp(5*int(c.B) - int(t.B) - int(bm.B) - int(l.B) - int(r.B)),
				A: c.A,
			})
		}
	}
	return dst
}

// upscaleImageBicubic CatmullRom 双三次插值放大 + 锐化，不依赖任何 AI 接口。
func (s *AIService) upscaleImageBicubic(ctx context.Context, src image.Image, srcB image.Rectangle, format, _ string, dstW, dstH int) (string, error) {
	scaled := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), src, srcB, draw.Over, nil)
	dst := applySharpen(scaled)

	var buf bytes.Buffer
	var outCT string
	switch format {
	case "png":
		outCT = "image/png"
		if err := png.Encode(&buf, dst); err != nil {
			return "", fmt.Errorf("upscale bicubic: encode png: %w", err)
		}
	default:
		outCT = "image/jpeg"
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 95}); err != nil {
			return "", fmt.Errorf("upscale bicubic: encode jpeg: %w", err)
		}
	}

	if s.storageSvc == nil {
		return "", fmt.Errorf("upscale bicubic: storage service not configured")
	}
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	key := fmt.Sprintf("images/upscaled/%s%s", uuid.New().String(), ext)
	outData := buf.Bytes()
	newURL, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(outData), int64(len(outData)), outCT)
	if err != nil {
		return "", fmt.Errorf("upscale bicubic: upload: %w", err)
	}
	logger.Printf("[AIService] upscaleImageBicubic: → %dx%d, saved to %s", dstW, dstH, newURL)
	return newURL, nil
}

// upscaleImageAI 使用 AI 图像生成模型（DreamO）在目标尺寸重新生成图片，保留原图视觉特征。
// 将原图转为 base64 作为参考图，CFGScale=8 强化特征保持，dstW/dstH 指定输出分辨率。
func (s *AIService) upscaleImageAI(ctx context.Context, tenantID, novelID uint, data []byte, origURL string, dstW, dstH int) (string, error) {
	// 转 base64 传给 AI（绕过 OSS 访问限制）
	b64 := base64.StdEncoding.EncodeToString(data)
	if b64 == "" {
		return "", fmt.Errorf("upscale ai: encode base64 failed")
	}

	const upscalePrompt = "masterpiece, best quality, ultra high resolution, sharp focus, fine details, perfect clarity, photorealistic"
	sizeStr := fmt.Sprintf("%dx%d", dstW, dstH)

	// CFGScale=8：高特征保持强度，让输出尽量忠于原图内容
	newURL, err := s.GenerateCharacterThreeView(ctx, tenantID, "", upscalePrompt, b64, "", "", sizeStr, 8.0)
	if err != nil {
		return "", fmt.Errorf("upscale ai: generate: %w", err)
	}
	if newURL == "" {
		return "", fmt.Errorf("upscale ai: empty URL returned")
	}

	// 持久化到 OSS
	persistURL := s.uploadImageToStorage(ctx, tenantID, newURL)
	logger.Printf("[AIService] upscaleImageAI: → %dx%d, saved to %s", dstW, dstH, persistURL)
	return persistURL, nil
}

// QualityService 质量服务
