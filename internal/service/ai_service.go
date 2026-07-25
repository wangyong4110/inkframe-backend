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
)

type AIService struct {
	modelRepo     *repository.AIModelRepository
	aiManager     *ai.ModelManager
	providerRepo  *repository.ModelProviderRepository
	storageSvc    storage.Service
	modelLimiters sync.Map      // key: "tenantID:modelName" → *modelCallLimiter (shared per-model concurrency+rate)
	stopCh        chan struct{} // closed by Shutdown() to stop background goroutines
	encKey        string        // AES-256-GCM key for decrypting stored API credentials
	// ImageQueue 是按模型隔离的图片生成任务队列。
	// Worker 数量 = AIModel.Concurrency（DB 配置），确保不超出 API 并发限额。
	// 替代"goroutine+信号量"模式：调用方提交任务后立即返回 TaskFuture，
	// 只有 concurrency 个 Worker goroutine 真正执行 API 调用。
	ImageQueue *ModelTaskQueue
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
	svc.startProviderHealthCheck()
	return svc
}

// WithEncryptionKey sets the AES-256-GCM key used to decrypt API credentials stored in the DB.
func (s *AIService) WithEncryptionKey(key string) *AIService {
	s.encKey = key
	return s
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
			provider, _, err := s.getTenantProvider(p.TenantID, p.Name)
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

// WithStorage 注入媒体存储服务，供图片生成后持久化使用
func (s *AIService) WithStorage(svc storage.Service) *AIService {
	s.storageSvc = svc
	return s
}
