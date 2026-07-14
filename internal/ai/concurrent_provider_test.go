package ai

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentProvider_DelegatesNameAndModelsAndHealthCheck(t *testing.T) {
	inner := newFakeProvider("inner-provider")
	p := NewConcurrentProvider(inner, 2)

	if p.GetName() != "inner-provider" {
		t.Errorf("GetName() = %q, want inner-provider", p.GetName())
	}
	if len(p.GetModels()) != 1 || p.GetModels()[0] != "fake-model" {
		t.Errorf("GetModels() = %v, want [fake-model]", p.GetModels())
	}
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v, want nil", err)
	}
}

func TestConcurrentProvider_GenerateDelegatesToInner(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{{resp: &GenerateResponse{Content: "hello"}}}
	p := NewConcurrentProvider(inner, 1)

	resp, err := p.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("resp.Content = %q, want hello", resp.Content)
	}
}

func TestConcurrentProvider_EmbedDelegatesToInner(t *testing.T) {
	inner := newFakeProvider("p")
	inner.embedQueue = []embedCall{{vec: []float32{1, 2, 3}}}
	p := NewConcurrentProvider(inner, 1)

	vec, err := p.Embed(context.Background(), "text")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("vec = %v, want length 3", vec)
	}
}

func TestConcurrentProvider_ImageGenerateDelegatesToInner(t *testing.T) {
	inner := newFakeProvider("p")
	inner.imageQueue = []imageCall{{resp: &ImageResponse{URL: "http://x/1.png"}}}
	p := NewConcurrentProvider(inner, 1)

	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("ImageGenerate: %v", err)
	}
	if resp.URL != "http://x/1.png" {
		t.Errorf("resp.URL = %q", resp.URL)
	}
}

func TestConcurrentProvider_AudioGenerateDelegatesToInner(t *testing.T) {
	inner := newFakeProvider("p")
	inner.audioQueue = []audioCall{{resp: &AudioResponse{URL: "http://x/1.mp3"}}}
	p := NewConcurrentProvider(inner, 1)

	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL != "http://x/1.mp3" {
		t.Errorf("resp.URL = %q", resp.URL)
	}
}

// blockingProvider is a minimal AIProvider whose Generate call blocks until released,
// used to verify ConcurrentProvider actually caps in-flight calls.
type blockingProvider struct {
	fakeProvider
	release chan struct{}
	inFlight int32
	maxSeen  int32
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{
		fakeProvider: fakeProvider{name: "blocking", models: []string{"m"}},
		release:      make(chan struct{}),
	}
}

func (b *blockingProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	n := atomic.AddInt32(&b.inFlight, 1)
	for {
		old := atomic.LoadInt32(&b.maxSeen)
		if n <= old || atomic.CompareAndSwapInt32(&b.maxSeen, old, n) {
			break
		}
	}
	<-b.release
	atomic.AddInt32(&b.inFlight, -1)
	return &GenerateResponse{Content: "done"}, nil
}

func TestConcurrentProvider_CapsSimultaneousCalls(t *testing.T) {
	inner := newBlockingProvider()
	p := NewConcurrentProvider(inner, 2)

	const totalCalls = 5
	var wg sync.WaitGroup
	for i := 0; i < totalCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Generate(context.Background(), &GenerateRequest{})
		}()
	}

	// Give goroutines time to pile up against the semaphore.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&inner.maxSeen); got > 2 {
		t.Errorf("max concurrent Generate calls = %d, want <= 2", got)
	}

	close(inner.release)
	wg.Wait()
}

func TestConcurrentProvider_AcquireRespectsContextCancellation(t *testing.T) {
	inner := newBlockingProvider()
	p := NewConcurrentProvider(inner, 1)

	// Occupy the single slot.
	done := make(chan struct{})
	go func() {
		_, _ = p.Generate(context.Background(), &GenerateRequest{})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // ensure the first call has acquired the slot

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error when context deadline exceeded while waiting for semaphore")
	}

	close(inner.release)
	<-done
}

func TestConcurrentProvider_GenerateStreamDelegatesAndReleasesOnCompletion(t *testing.T) {
	inner := newFakeProvider("p")
	srcCh := make(chan *GenerateResponse, 2)
	srcCh <- &GenerateResponse{Content: "chunk1"}
	srcCh <- &GenerateResponse{Content: "chunk2"}
	close(srcCh)
	inner.generateStreamQueue = []genStreamCall{{ch: srcCh}}

	p := NewConcurrentProvider(inner, 1)
	out, err := p.GenerateStream(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}

	var got []string
	for r := range out {
		got = append(got, r.Content)
	}
	if len(got) != 2 || got[0] != "chunk1" || got[1] != "chunk2" {
		t.Errorf("got %v, want [chunk1 chunk2]", got)
	}

	// Slot must have been released; a subsequent call should succeed without blocking.
	done := make(chan struct{})
	go func() {
		_, _ = p.Generate(context.Background(), &GenerateRequest{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected semaphore slot to be released after stream drained")
	}
}

func TestConcurrentProvider_GenerateStreamReleasesOnInnerError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateStreamQueue = []genStreamCall{{err: context.DeadlineExceeded}}
	p := NewConcurrentProvider(inner, 1)

	_, err := p.GenerateStream(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error from inner GenerateStream to propagate")
	}

	// Slot must have been released despite the error.
	done := make(chan struct{})
	go func() {
		_, _ = p.Generate(context.Background(), &GenerateRequest{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected semaphore slot to be released after stream error")
	}
}

var _ AIProvider = (*ConcurrentProvider)(nil)
