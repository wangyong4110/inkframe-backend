package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// EnqueueImageTask 将一次图片生成函数提交到按模型隔离的 Worker 池，返回 TaskFuture。
//
// Worker 数量由 AIModel.Concurrency（DB 配置）决定，默认 1（串行）。
// 若 DB 中未配置该模型，或 Concurrency=0，则退回为单 Worker（不丢任务，仅降速）。
//
// 用法示例：
//
//	future := svc.EnqueueImageTask(ctx, tenantID, "seededit_v3.0", func(ctx context.Context) (string, error) {
//	    return svc.GenerateCharacterThreeViewMulti(ctx, ...)
//	})
//	url, err := future.Await(ctx)
func (s *AIService) EnqueueImageTask(ctx context.Context, tenantID uint, modelName string, fn func(ctx context.Context) (string, error)) *TaskFuture {
	concurrency := 1
	if s.modelRepo != nil {
		if m, err := s.modelRepo.GetByName(modelName); err == nil && m.Concurrency > 0 {
			concurrency = m.Concurrency
		}
	}
	key := fmt.Sprintf("%d:%s", tenantID, modelName)
	return s.ImageQueue.Submit(key, concurrency, ctx, fn)
}

// EnqueueImageTaskByProvider 与 EnqueueImageTask 类似，但以 providerName 为队列 key。
// 适用于调用方知道提供者但不知道具体模型名称的场景（如 BatchGenerateShotImages）。
// concurrency 直接指定（调用方负责从 DB 或配置读取）。
func (s *AIService) EnqueueImageTaskByProvider(ctx context.Context, tenantID uint, providerName string, concurrency int, fn func(ctx context.Context) (string, error)) *TaskFuture {
	if concurrency <= 0 {
		concurrency = 1
	}
	key := fmt.Sprintf("%d:provider:%s", tenantID, providerName)
	return s.ImageQueue.Submit(key, concurrency, ctx, fn)
}

// GetProviderConcurrency 从 DB 中查找指定类型（"image"/"video"/"voice"/"sfx"）的第一个活跃提供商，
// 返回其关联 AIModel 的 Concurrency 配置值（默认 1，表示串行执行）。
// 调用方无需关心具体模型名称，统一由此方法从 DB 配置中解析。
func (s *AIService) GetProviderConcurrency(tenantID uint, providerType string) int {
	if s.providerRepo == nil || s.modelRepo == nil {
		return 1
	}
	providers, err := s.eligibleProviders(tenantID, providerType)
	if err != nil || len(providers) == 0 {
		return 1
	}
	for _, p := range providers {
		modelName := s.activeModelNameFor(p, tenantID, providerType)
		if modelName == "" {
			continue
		}
		if m, err := s.modelRepo.GetByName(modelName); err == nil && m.Concurrency > 0 {
			return m.Concurrency
		}
		return 1 // 找到提供商但未配置并发度 → 保守默认值
	}
	return 1
}

// modelCallLimiter 为 (tenantID, modelName) 提供共享的并发控制和速率限制。
// 与 ConcurrentProvider / RateLimitProvider 不同，这里的状态是跨调用共享的：
//   - sem：容量 = AIModel.Concurrency，所有调用同一模型的 goroutine 共用同一个 channel
//   - token bucket：基于时间滑动，全局共享，不在每次调用时重置
//
// defaultMaxQueueWait 是并发槽排队等待的默认上限。
// 排队等待使用独立计时器，不受调用方执行超时（ctx）影响，确保后续请求能正常入队
// 而不是在执行超时耗尽时直接报错。30 分钟足以覆盖大多数批量生成场景。
const defaultMaxQueueWait = 30 * time.Minute

type modelCallLimiter struct {
	// concurrency semaphore (nil = unlimited)
	sem          chan struct{}
	maxQueueWait time.Duration // 并发槽最大排队等待时间（独立于执行 ctx）

	// token-bucket for rate limiting (refill=0 means disabled)
	rlMu    sync.Mutex
	rlTok   float64
	rlMax   float64
	rlRefNs float64 // tokens per nanosecond
	rlLast  time.Time
}

func newModelCallLimiter(concurrency, ratePerMin int) *modelCallLimiter {
	l := &modelCallLimiter{maxQueueWait: defaultMaxQueueWait}
	if concurrency > 0 {
		l.sem = make(chan struct{}, concurrency)
	}
	if ratePerMin > 0 {
		l.rlMax = float64(ratePerMin)
		l.rlTok = float64(ratePerMin)
		l.rlRefNs = float64(ratePerMin) / float64(time.Minute)
		l.rlLast = time.Now()
	}
	return l
}

// Acquire 先等速率令牌，再获取并发槽。
//
// 速率限制阶段：受 ctx 约束（ctx 取消即放弃）。
// 并发槽等待阶段：使用独立的 maxQueueWait 计时器，不受调用方执行超时影响。
// 这样可以确保"队满时排队等待"而不是"执行超时到期时直接报错"，
// 调用方应在获取槽后再创建执行超时 ctx（参见 callAIWithProviderSys）。
func (l *modelCallLimiter) Acquire(ctx context.Context) error {
	// 1. 速率限制（token bucket）：受 ctx 控制
	if l.rlRefNs > 0 {
		for {
			l.rlMu.Lock()
			now := time.Now()
			l.rlTok += float64(now.Sub(l.rlLast)) * l.rlRefNs
			if l.rlTok > l.rlMax {
				l.rlTok = l.rlMax
			}
			l.rlLast = now
			if l.rlTok >= 1 {
				l.rlTok--
				l.rlMu.Unlock()
				break
			}
			wait := time.Duration((1-l.rlTok)/l.rlRefNs) + time.Millisecond
			l.rlMu.Unlock()
			select {
			case <-ctx.Done():
				return fmt.Errorf("rate limit wait cancelled: %w", ctx.Err())
			case <-time.After(wait):
			}
		}
	}
	// 2. 并发槽：用独立 timer，不依赖调用方 ctx 的执行超时
	if l.sem != nil {
		// 快速路径：无需等待直接获取
		select {
		case l.sem <- struct{}{}:
			return nil
		default:
		}
		// 慢速路径：入队等待
		var queueDeadline <-chan time.Time
		if l.maxQueueWait > 0 {
			t := time.NewTimer(l.maxQueueWait)
			defer t.Stop()
			queueDeadline = t.C
		}
		logger.Printf("[ModelLimiter] concurrency full (cap=%d), queuing request (maxWait=%v)", cap(l.sem), l.maxQueueWait)
		select {
		case l.sem <- struct{}{}:
			logger.Printf("[ModelLimiter] concurrency slot acquired from queue (cap=%d)", cap(l.sem))
		case <-ctx.Done():
			return fmt.Errorf("concurrency slot wait cancelled: %w", ctx.Err())
		case <-queueDeadline:
			return fmt.Errorf("concurrency slot queue wait exceeded %v", l.maxQueueWait)
		}
	}
	return nil
}

// Release 释放并发槽（速率令牌无需归还）。
func (l *modelCallLimiter) Release() {
	if l.sem != nil {
		<-l.sem
	}
}

// getModelLimiter 返回 (tenantID, modelName) 对应的共享限流器；concurrency 和 ratePerMin 均为 0 时返回 nil。
// 限流器在首次调用时创建并缓存，进程生命周期内复用（修改 AI 模型配置后需重启生效）。
func (s *AIService) getModelLimiter(tenantID uint, modelName string, concurrency, ratePerMin int) *modelCallLimiter {
	if concurrency <= 0 && ratePerMin <= 0 {
		return nil
	}
	key := fmt.Sprintf("%d:%s", tenantID, modelName)
	if v, ok := s.modelLimiters.Load(key); ok {
		return v.(*modelCallLimiter)
	}
	l := newModelCallLimiter(concurrency, ratePerMin)
	actual, _ := s.modelLimiters.LoadOrStore(key, l)
	return actual.(*modelCallLimiter)
}

// acquireModelSlotByName 按模型名从 DB 查找并发/限速配置，获取共享限制器令牌。
// 返回 release 函数（必须调用以释放令牌）和错误。未配置限制时返回 no-op release。
func (s *AIService) acquireModelSlotByName(ctx context.Context, tenantID uint, modelName string) (func(), error) {
	if s.modelRepo == nil || modelName == "" {
		return func() {}, nil
	}
	m, err := s.modelRepo.GetByName(modelName)
	if err != nil || (m.Concurrency <= 0 && m.RateLimit <= 0) {
		return func() {}, nil
	}
	lim := s.getModelLimiter(tenantID, modelName, m.Concurrency, m.RateLimit)
	if lim == nil {
		return func() {}, nil
	}
	if err := lim.Acquire(ctx); err != nil {
		return func() {}, fmt.Errorf("model %s: %w", modelName, err)
	}
	return lim.Release, nil
}

// acquireProviderSlot 按提供商名查找并发/限速配置（适用于 TTS/SFX 等按 provider 而非 model 调度的场景）。
// 从 DB 中找出该 provider 下第一个配置了并发/限速的 AIModel 记录；未配置时返回 no-op release。
func (s *AIService) acquireProviderSlot(ctx context.Context, tenantID uint, providerName string) (func(), error) {
	if s.modelRepo == nil || s.providerRepo == nil || providerName == "" {
		return func() {}, nil
	}
	// 找到 provider 的 DB ID
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return func() {}, nil
	}
	var providerID uint
	for _, p := range providers {
		if p.IsActive && strings.EqualFold(p.Name, providerName) {
			providerID = p.ID
			break
		}
	}
	if providerID == 0 {
		return func() {}, nil
	}
	// 找到该 provider 下有限流配置的 AIModel
	pid := providerID
	models, err := s.modelRepo.List(&pid, tenantID)
	if err != nil || len(models) == 0 {
		return func() {}, nil
	}
	var concurrency, rateLimit int
	for _, mm := range models {
		if mm.Concurrency > 0 || mm.RateLimit > 0 {
			concurrency = mm.Concurrency
			rateLimit = mm.RateLimit
			break
		}
	}
	if concurrency <= 0 && rateLimit <= 0 {
		return func() {}, nil
	}
	lim := s.getModelLimiter(tenantID, "provider:"+providerName, concurrency, rateLimit)
	if lim == nil {
		return func() {}, nil
	}
	if err := lim.Acquire(ctx); err != nil {
		return func() {}, fmt.Errorf("provider %s: %w", providerName, err)
	}
	return lim.Release, nil
}
