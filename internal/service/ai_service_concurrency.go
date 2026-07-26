package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

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
func (l *modelCallLimiter) acquire(ctx context.Context) error {
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
	m, err := s.modelRepo.GetByName(tenantID, modelName)
	if err != nil || (m.Concurrency <= 0 && m.RateLimit <= 0) {
		return func() {}, nil
	}
	lim := s.getModelLimiter(tenantID, modelName, m.Concurrency, m.RateLimit)
	if lim == nil {
		return func() {}, nil
	}
	if err := lim.acquire(ctx); err != nil {
		return func() {}, fmt.Errorf("model %s: %w", modelName, err)
	}
	return lim.Release, nil
}
