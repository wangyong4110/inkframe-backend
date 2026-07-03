package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

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
// 若 ctx 先超时，任务仍会继续执行并将结果写入 channel（不泄漏 worker）。
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
//   - ch 是大容量 buffered channel，Submit 在 channel 未满时即时返回（非阻塞）。
//   - 固定 concurrency 个 Worker goroutine 从 ch 消费任务，严格限制并行度。
//   - 与信号量方案不同：调用方 goroutine 不会因等待 slot 而阻塞，只有 Worker 数量的
//     goroutine 真正在做 I/O，其余任务安静地排在 channel 里。
type workerPool struct {
	key         string
	concurrency int
	ch          chan poolTask
	stopOnce    sync.Once
	stopCh      chan struct{}
	wg          sync.WaitGroup
	pending     atomic.Int64 // 队列中 + 正在执行的任务数
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
		// 10 万容量：每个任务仅占 ~80 字节（3 个指针），约 8 MB；实际队列远小于此上限。
		ch:     make(chan poolTask, 100_000),
		stopCh: make(chan struct{}),
	}
	for i := 0; i < concurrency; i++ {
		p.wg.Add(1)
		go p.run()
	}
	logger.Infof("[ModelTaskQueue] pool created key=%q concurrency=%d", key, concurrency)
	return p
}

func (p *workerPool) run() {
	defer p.wg.Done()
	for {
		select {
		case t, ok := <-p.ch:
			if !ok {
				return
			}
			url, err := t.fn(t.ctx)
			t.out <- TaskResult{Value: url, Err: err}
			p.pending.Add(-1)
		case <-p.stopCh:
			// 排空剩余任务，通知调用方
			for {
				select {
				case t := <-p.ch:
					t.out <- TaskResult{Err: fmt.Errorf("model task queue stopped")}
					p.pending.Add(-1)
				default:
					return
				}
			}
		}
	}
}

// submit 向 pool 提交一个任务，返回 TaskFuture。
// 若 ch 已满（超过 100_000 条待处理任务），此调用会短暂阻塞直到有空间。
func (p *workerPool) submit(ctx context.Context, fn func(ctx context.Context) (string, error)) *TaskFuture {
	out := make(chan TaskResult, 1)
	p.pending.Add(1)
	p.ch <- poolTask{ctx: ctx, fn: fn, out: out}
	return &TaskFuture{ch: out}
}

func (p *workerPool) stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
	p.wg.Wait()
}

// ModelTaskQueue 管理多个 workerPool，每个 key 独立。
//
// key 通常为 "{tenantID}:{modelName}" 或 "{tenantID}:{providerName}"。
// concurrency 仅在 key 首次创建时生效；后续 Submit 使用已有 pool，忽略传入的 concurrency。
// 若需更新 concurrency，重启服务即可（pool 随进程生命周期存在）。
type ModelTaskQueue struct {
	mu    sync.Mutex
	pools map[string]*workerPool
}

func newModelTaskQueue() *ModelTaskQueue {
	return &ModelTaskQueue{pools: make(map[string]*workerPool)}
}

func (q *ModelTaskQueue) getOrCreate(key string, concurrency int) *workerPool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p, ok := q.pools[key]; ok {
		return p
	}
	p := newWorkerPool(key, concurrency)
	q.pools[key] = p
	return p
}

// Submit 提交一个任务到 key 对应的 worker pool（不存在则以 concurrency 创建）。
// fn 接收 ctx，应将超时控制交给 fn 内部。
// 返回的 TaskFuture 在 fn 完成后写入结果。
func (q *ModelTaskQueue) Submit(key string, concurrency int, ctx context.Context, fn func(ctx context.Context) (string, error)) *TaskFuture {
	pool := q.getOrCreate(key, concurrency)
	return pool.submit(ctx, fn)
}

// PendingCount 返回指定 key 的 pool 中待处理（队列中 + 正在执行）的任务数，不存在时返回 0。
func (q *ModelTaskQueue) PendingCount(key string) int64 {
	q.mu.Lock()
	p, ok := q.pools[key]
	q.mu.Unlock()
	if !ok {
		return 0
	}
	return p.pending.Load()
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

// Stop 关闭所有 pool 并等待 Worker 退出（服务器 graceful shutdown 时调用）。
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
