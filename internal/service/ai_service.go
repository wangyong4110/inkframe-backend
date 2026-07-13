package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
)

type AIService struct {
	modelRepo     *repository.AIModelRepository
	aiManager     *ai.ModelManager
	providerRepo  *repository.ModelProviderRepository
	novelRepo     *repository.NovelRepository
	storageSvc    storage.Service
	dbMediaReader storage.Service // DB-backed reader for legacy /api/v1/media/* paths
	taskRouting   TaskRouting
	serverBaseURL string         // base URL for resolving relative media paths (fallback, prefer dbMediaReader)
	providerCache sync.Map       // key: "tenantID:providerName" → providerCacheEntry
	modelLimiters sync.Map       // key: "tenantID:modelName" → *modelCallLimiter (shared per-model concurrency+rate)
	stopCh        chan struct{}  // closed by Shutdown() to stop background goroutines
	encKey        string         // AES-256-GCM key for decrypting stored API credentials
	cache         redisPublisher // optional: for cross-instance provider cache invalidation
	promptFilter  *PromptFilter  // optional: proactive sensitive-word filtering for image prompts
	// ImageQueue 是按模型隔离的图片生成任务队列。
	// Worker 数量 = AIModel.Concurrency（DB 配置），确保不超出 API 并发限额。
	// 替代"goroutine+信号量"模式：调用方提交任务后立即返回 TaskFuture，
	// 只有 concurrency 个 Worker goroutine 真正执行 API 调用。
	ImageQueue *ModelTaskQueue
}

// redisPublisher is the subset of redis.Client used by AIService (allows nil-safe injection).
type redisPublisher interface {
	Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	aiManager *ai.ModelManager,
	providerRepo ...*repository.ModelProviderRepository,
) *AIService {
	svc := &AIService{
		modelRepo:  modelRepo,
		aiManager:  aiManager,
		stopCh:     make(chan struct{}),
		ImageQueue: newModelTaskQueue(),
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
			} else {
				// Health check passed: reset any open circuit breaker on the cached provider
				// so requests are unblocked immediately without waiting for the CB timeout.
				if rp, ok := provider.(*ai.RetryProvider); ok {
					rp.ResetCircuit()
				}
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

// WithDBMediaReader 注入专用于读取 DB 存储（/api/v1/media/*）媒体数据的 storage.Service。
func (s *AIService) WithDBMediaReader(svc storage.Service) *AIService {
	s.dbMediaReader = svc
	return s
}

// WithTaskRouting 设置各任务类型优先使用的 provider（来自 config.yaml ai.tasks）
func (s *AIService) WithTaskRouting(tr TaskRouting) *AIService {
	s.taskRouting = tr
	return s
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

// ResetProviderCircuit resets the circuit breaker on all cached provider instances whose
// key ends with providerName, then evicts them from the cache so the next request creates
// a fresh instance. Use this via the sysadmin API to recover from a stuck-open circuit.
func (s *AIService) ResetProviderCircuit(providerName string) {
	s.providerCache.Range(func(k, v any) bool {
		key, _ := k.(string)
		if !strings.HasSuffix(key, ":"+providerName) && providerName != "" {
			return true
		}
		if entry, ok := v.(providerCacheEntry); ok {
			if rp, ok := entry.provider.(*ai.RetryProvider); ok {
				rp.ResetCircuit()
			}
		}
		s.providerCache.Delete(k)
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

// WithServerBaseURL 设置本地服务器基础 URL（如 "http://127.0.0.1:8080"），用于将相对媒体路径
// 转换为可下载的绝对 URL（DB 存储返回 /api/v1/media/xxx 时需要此配置）。
func (s *AIService) WithServerBaseURL(baseURL string) {
	s.serverBaseURL = strings.TrimRight(baseURL, "/")
}
