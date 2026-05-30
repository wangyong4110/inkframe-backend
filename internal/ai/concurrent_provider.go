package ai

import "context"

// ConcurrentProvider wraps any AIProvider with a semaphore to cap simultaneous calls.
// It is constructed inside getTenantProvider when ModelProvider.Concurrency > 0 and
// stored in the 5-minute provider cache, so the same semaphore is shared across all
// callers within the cache window.
type ConcurrentProvider struct {
	inner AIProvider
	sem   chan struct{}
}

func NewConcurrentProvider(inner AIProvider, maxConcurrent int) *ConcurrentProvider {
	return &ConcurrentProvider{
		inner: inner,
		sem:   make(chan struct{}, maxConcurrent),
	}
}

func (p *ConcurrentProvider) acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ConcurrentProvider) release() { <-p.sem }

func (p *ConcurrentProvider) GetName() string      { return p.inner.GetName() }
func (p *ConcurrentProvider) GetModels() []string  { return p.inner.GetModels() }

func (p *ConcurrentProvider) HealthCheck(ctx context.Context) error {
	return p.inner.HealthCheck(ctx)
}

func (p *ConcurrentProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	return p.inner.Generate(ctx, req)
}

func (p *ConcurrentProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	// Release after the channel is drained or closed; wrap the inner channel.
	inner, err := p.inner.GenerateStream(ctx, req)
	if err != nil {
		p.release()
		return nil, err
	}
	out := make(chan *GenerateResponse, cap(inner)+1)
	go func() {
		defer p.release()
		defer close(out)
		for r := range inner {
			out <- r
		}
	}()
	return out, nil
}

func (p *ConcurrentProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	return p.inner.Embed(ctx, text)
}

func (p *ConcurrentProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	return p.inner.ImageGenerate(ctx, req)
}

func (p *ConcurrentProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	return p.inner.AudioGenerate(ctx, req)
}

var _ AIProvider = (*ConcurrentProvider)(nil)
