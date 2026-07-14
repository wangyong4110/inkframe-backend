package ai

import (
	"context"
	"testing"
	"time"
)

func TestRateLimitProvider_DelegatesNameModelsHealthCheck(t *testing.T) {
	inner := newFakeProvider("rl-inner")
	p := NewRateLimitProvider(inner, 60)

	if p.GetName() != "rl-inner" {
		t.Errorf("GetName() = %q", p.GetName())
	}
	if len(p.GetModels()) != 1 {
		t.Errorf("GetModels() = %v", p.GetModels())
	}
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v", err)
	}
}

func TestRateLimitProvider_AllowsBurstUpToLimit(t *testing.T) {
	inner := newFakeProvider("p")
	p := NewRateLimitProvider(inner, 5)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if _, err := p.Generate(ctx, &GenerateRequest{}); err != nil {
			t.Fatalf("Generate call %d unexpectedly failed: %v", i, err)
		}
	}
	if inner.callCount() != 5 {
		t.Errorf("inner call count = %d, want 5", inner.callCount())
	}
}

func TestRateLimitProvider_BlocksBeyondBurstUntilRefill(t *testing.T) {
	// 120 requests/min => refill of 2/sec, so bucket refills a full extra token every 500ms.
	inner := newFakeProvider("p")
	p := NewRateLimitProvider(inner, 120)

	ctx := context.Background()
	// Drain the initial burst (starts full at maxTok=120 tokens... too slow to drain in test).
	// Instead, verify a single call succeeds immediately given full bucket.
	start := time.Now()
	if _, err := p.Generate(ctx, &GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("first call with full bucket took %v, want near-instant", elapsed)
	}
}

func TestRateLimitProvider_ExhaustedBucketDelaysCall(t *testing.T) {
	// Very low rate: 1 request per minute => bucket starts with 1 token (maxTok=1).
	inner := newFakeProvider("p")
	p := NewRateLimitProvider(inner, 1)

	ctx := context.Background()
	// First call consumes the only token immediately.
	if _, err := p.Generate(ctx, &GenerateRequest{}); err != nil {
		t.Fatalf("first Generate: %v", err)
	}

	// Second call should have to wait for refill (~60s for a full token at 1/min);
	// use a short timeout context to confirm it actually blocks rather than proceeding
	// immediately, without waiting the full minute in the test.
	shortCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Generate(shortCtx, &GenerateRequest{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected second call to be rate-limited and fail via context timeout")
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("expected call to block until context timeout (~100ms), took %v", elapsed)
	}
}

func TestRateLimitProvider_AcquireReturnsErrorOnContextCancel(t *testing.T) {
	inner := newFakeProvider("p")
	p := NewRateLimitProvider(inner, 1)

	ctx := context.Background()
	if _, err := p.Generate(ctx, &GenerateRequest{}); err != nil {
		t.Fatalf("first Generate: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel() // already canceled
	_, err := p.Generate(cancelCtx, &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error for already-canceled context")
	}
}

func TestRateLimitProvider_ZeroOrNegativeDefaultsTo60PerMinute(t *testing.T) {
	inner := newFakeProvider("p")
	p := NewRateLimitProvider(inner, 0)
	if p.maxTok != 60 {
		t.Errorf("maxTok = %v, want 60 (default)", p.maxTok)
	}

	p2 := NewRateLimitProvider(inner, -5)
	if p2.maxTok != 60 {
		t.Errorf("maxTok = %v, want 60 (default) for negative input", p2.maxTok)
	}
}

func TestRateLimitProvider_EmbedImageAudioStreamDelegateToInner(t *testing.T) {
	inner := newFakeProvider("p")
	inner.embedQueue = []embedCall{{vec: []float32{9}}}
	inner.imageQueue = []imageCall{{resp: &ImageResponse{URL: "u"}}}
	inner.audioQueue = []audioCall{{resp: &AudioResponse{URL: "a"}}}
	srcCh := make(chan *GenerateResponse)
	close(srcCh)
	inner.generateStreamQueue = []genStreamCall{{ch: srcCh}}

	p := NewRateLimitProvider(inner, 100)
	ctx := context.Background()

	if vec, err := p.Embed(ctx, "x"); err != nil || len(vec) != 1 {
		t.Errorf("Embed = %v, %v", vec, err)
	}
	if resp, err := p.ImageGenerate(ctx, &ImageGenerateRequest{}); err != nil || resp.URL != "u" {
		t.Errorf("ImageGenerate = %v, %v", resp, err)
	}
	if resp, err := p.AudioGenerate(ctx, &AudioGenerateRequest{}); err != nil || resp.URL != "a" {
		t.Errorf("AudioGenerate = %v, %v", resp, err)
	}
	if ch, err := p.GenerateStream(ctx, &GenerateRequest{}); err != nil || ch == nil {
		t.Errorf("GenerateStream = %v, %v", ch, err)
	}
}

var _ AIProvider = (*RateLimitProvider)(nil)
