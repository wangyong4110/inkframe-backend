package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// ErrQueueStopped 在 pool 已停止时提交任务会立即收到此错误（而非阻塞）。
var ErrQueueStopped = errors.New("task queue stopped")

// ErrQueueFull 在 pool 的缓冲 channel 已满时提交任务会立即收到此错误。
var ErrQueueFull = errors.New("task queue full")

// workerPoolQueueCap 是每个 workerPool 的任务缓冲容量。
// 达到上限时 submit() 立即返回 ErrQueueFull（不阻塞调用方）。
const workerPoolQueueCap = 100_000

// TaskResult 是 TaskFuture 携带的结果。
type TaskResult struct {
	Value string
	Err   error
}

// TaskFuture 由 ModelTaskQueue.Submit 返回，调用方通过 Await 等待结果。
// 内部是容量为 1 的 channel，Worker 写入后即可释放，调用方随时可取。
type TaskFuture struct {
	ch <-chan TaskResult
}

// Await 阻塞直到任务完成或 ctx 取消。
// 若 ctx 先超时，Worker 仍会继续执行并将结果写入 channel（不泄漏 worker）。
func (f *TaskFuture) Await(ctx context.Context) (string, error) {
	select {
	case r := <-f.ch:
		return r.Value, r.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// workerPool 是 ModelTaskQueue 内部每个 key 对应的有界 Worker 池。
//
// 设计要点：
//   - submitMu 使"stopped 检查 + ch 写入/关闭"成为不可分割的原子对，
//     消除 stop() 与 submit() 之间的竞态（send-on-closed-channel panic）。
//   - Worker 通过 "for range ch" 消费任务：close(ch) 后 Worker 自动排空剩余任务再退出，
//     实现"先排空，再停止"的 graceful drain 语义（而非立即丢弃）。
//   - pending 仅在任务成功入队后才递增，确保监控数值准确。
//   - Worker 内置 panic recovery，防止单次任务崩溃导致 goroutine 泄漏。
type workerPool struct {
	key         string
	concurrency int
	ch          chan poolTask
	submitMu    sync.Mutex // 保护 stopped-check + ch-send/close 的原子性
	stopped     bool       // 由 submitMu 保护
	stopOnce    sync.Once
	wg          sync.WaitGroup
	pending     atomic.Int64 // 队列中 + 正在执行的任务数（成功入队后才计）
}

type poolTask struct {
	ctx context.Context
	fn  func(ctx context.Context) (string, error)
	out chan<- TaskResult
}

func newWorkerPool(key string, concurrency int) *workerPool {
	if concurrency <= 0 {
		concurrency = 1
	}
	p := &workerPool{
		key:         key,
		concurrency: concurrency,
		ch:          make(chan poolTask, workerPoolQueueCap),
	}
	for i := 0; i < concurrency; i++ {
		p.wg.Add(1)
		go p.run()
	}
	logger.Infof("[ModelTaskQueue] pool created key=%q concurrency=%d", key, concurrency)
	return p
}

// run 是 Worker goroutine 的主循环。
// "for range ch" 确保 close(ch) 后 Worker 排空所有已排队任务再退出，
// 而非立即中止（"drain then stop" 语义）。
func (p *workerPool) run() {
	defer p.wg.Done()
	for t := range p.ch {
		p.execTask(t)
	}
}

// execTask 执行单个任务，含 panic recovery，防止任务崩溃导致 Worker 退出和 Await 永久阻塞。
func (p *workerPool) execTask(t poolTask) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[ModelTaskQueue] pool %q: task panic: %v", p.key, r)
			t.out <- TaskResult{Err: fmt.Errorf("task panicked: %v", r)}
			p.pending.Add(-1)
		}
	}()
	url, err := t.fn(t.ctx)
	t.out <- TaskResult{Value: url, Err: err}
	p.pending.Add(-1)
}

// submit 向 pool 提交一个任务，返回 TaskFuture。
// 若 pool 已停止或 channel 已满，future 立即携带错误（不阻塞调用方）。
func (p *workerPool) submit(ctx context.Context, fn func(ctx context.Context) (string, error)) *TaskFuture {
	out := make(chan TaskResult, 1)

	p.submitMu.Lock()
	if p.stopped {
		// pool 已停止：立即返回错误，不阻塞
		p.submitMu.Unlock()
		out <- TaskResult{Err: ErrQueueStopped}
		return &TaskFuture{ch: out}
	}
	select {
	case p.ch <- poolTask{ctx: ctx, fn: fn, out: out}:
		// 任务成功入队后再递增 pending，确保监控数值不多计
		p.pending.Add(1)
		p.submitMu.Unlock()
	default:
		// channel 满：立即返回错误，不阻塞
		p.submitMu.Unlock()
		logger.Errorf("[ModelTaskQueue] pool %q at capacity (%d), task rejected", p.key, workerPoolQueueCap)
		out <- TaskResult{Err: ErrQueueFull}
	}
	return &TaskFuture{ch: out}
}

// stop 标记 pool 为 stopped（后续 submit 立即返回 ErrQueueStopped），
// 然后等待所有已排队任务被处理完毕后 Worker 退出。
// 可并发调用、多次调用（幂等）。
func (p *workerPool) stop() {
	p.stopOnce.Do(func() {
		p.submitMu.Lock()
		p.stopped = true
		close(p.ch) // 触发 Worker 排空剩余任务后退出
		p.submitMu.Unlock()
	})
	p.wg.Wait()
}

// ModelTaskQueue 管理多个 workerPool，每个 key 独立。
//
// key 通常为 "{tenantID}:image-gen" 或 "{tenantID}:video-gen"。
// 当传入的 concurrency 与现有 pool 不同时，自动创建新 pool 并在后台排空旧 pool，
// 实现并发度的热更新（无需重启服务）。
type ModelTaskQueue struct {
	mu    sync.Mutex
	pools map[string]*workerPool
}

func newModelTaskQueue() *ModelTaskQueue {
	return &ModelTaskQueue{pools: make(map[string]*workerPool)}
}

// getOrCreate 返回 key 对应的 pool。
// 若 concurrency 与已有 pool 不同，则替换为新 pool 并在后台排空旧 pool。
func (q *ModelTaskQueue) getOrCreate(key string, concurrency int) *workerPool {
	q.mu.Lock()
	if p, ok := q.pools[key]; ok {
		if p.concurrency == concurrency {
			q.mu.Unlock()
			return p
		}
		// 并发度已更新：创建新 pool，异步排空旧 pool（旧 pool 中已排队的任务会继续执行完毕）
		newPool := newWorkerPool(key, concurrency)
		q.pools[key] = newPool
		q.mu.Unlock()
		logger.Infof("[ModelTaskQueue] pool resized key=%q %d→%d, draining old pool in background", key, p.concurrency, concurrency)
		go p.stop()
		return newPool
	}
	p := newWorkerPool(key, concurrency)
	q.pools[key] = p
	q.mu.Unlock()
	return p
}

// Submit 提交一个任务到 key 对应的 worker pool（不存在或并发度变更时自动重建）。
// fn 接收 ctx，应将超时控制交给 fn 内部（Submit 传入的 ctx 仅在 Await 超时时使用）。
// 返回的 TaskFuture 在 fn 完成后写入结果；若 pool 已停止或满载，future 立即携带错误。
func (q *ModelTaskQueue) Submit(key string, concurrency int, ctx context.Context, fn func(ctx context.Context) (string, error)) *TaskFuture {
	return q.getOrCreate(key, concurrency).submit(ctx, fn)
}

// Stats 返回所有 pool 的待处理任务数快照（用于监控/日志）。
func (q *ModelTaskQueue) Stats() map[string]int64 {
	q.mu.Lock()
	snapshot := make(map[string]int64, len(q.pools))
	for k, p := range q.pools {
		snapshot[k] = p.pending.Load()
	}
	q.mu.Unlock()
	return snapshot
}

// Stop 关闭所有 pool 并等待已排队任务全部处理完毕后 Worker 退出（服务器 graceful shutdown 时调用）。
func (q *ModelTaskQueue) Stop() {
	q.mu.Lock()
	pools := make([]*workerPool, 0, len(q.pools))
	for _, p := range q.pools {
		pools = append(pools, p)
	}
	q.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range pools {
		wg.Add(1)
		go func(wp *workerPool) {
			defer wg.Done()
			wp.stop()
		}(p)
	}
	wg.Wait()
	logger.Infof("[ModelTaskQueue] all pools stopped")
}
