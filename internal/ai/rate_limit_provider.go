package ai

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RateLimitProvider wraps an AIProvider and enforces a per-minute request cap.
// Uses a simple token-bucket: tokens refill at requestsPerMin/min.
type RateLimitProvider struct {
	inner   AIProvider
	mu      sync.Mutex
	tokens  float64
	maxTok  float64
	refill  float64 // tokens per nanosecond
	lastRef time.Time
}

// NewRateLimitProvider creates a provider wrapper that limits to requestsPerMin calls/minute.
func NewRateLimitProvider(inner AIProvider, requestsPerMin int) *RateLimitProvider {
	if requestsPerMin <= 0 {
		requestsPerMin = 60
	}
	perNs := float64(requestsPerMin) / float64(time.Minute)
	max := float64(requestsPerMin)
	return &RateLimitProvider{
		inner:   inner,
		tokens:  max,
		maxTok:  max,
		refill:  perNs,
		lastRef: time.Now(),
	}
}

func (p *RateLimitProvider) acquire(ctx context.Context) error {
	for {
		p.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(p.lastRef)
		p.tokens += float64(elapsed) * p.refill
		if p.tokens > p.maxTok {
			p.tokens = p.maxTok
		}
		p.lastRef = now
		if p.tokens >= 1 {
			p.tokens--
			p.mu.Unlock()
			return nil
		}
		wait := time.Duration((1-p.tokens)/p.refill) + time.Millisecond
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return fmt.Errorf("rate limit wait cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

func (p *RateLimitProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	return p.inner.Generate(ctx, req)
}

func (p *RateLimitProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	return p.inner.GenerateStream(ctx, req)
}

func (p *RateLimitProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	return p.inner.Embed(ctx, text)
}

func (p *RateLimitProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	return p.inner.ImageGenerate(ctx, req)
}

func (p *RateLimitProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	return p.inner.AudioGenerate(ctx, req)
}

func (p *RateLimitProvider) GetName() string    { return p.inner.GetName() }
func (p *RateLimitProvider) GetModels() []string { return p.inner.GetModels() }
func (p *RateLimitProvider) HealthCheck(ctx context.Context) error { return p.inner.HealthCheck(ctx) }
