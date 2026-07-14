package ai

import (
	"context"
	"sync"
)

// fakeProvider is a hand-written, in-package stand-in for AIProvider used to drive
// RetryProvider / ConcurrentProvider / RateLimitProvider tests deterministically,
// without any network calls.
//
// Each call-queue field holds canned (response, error) pairs consumed in order, one per
// call. When the queue is exhausted, the last entry (if any) is reused, so a single-entry
// queue behaves like a fixed stub. Call counters let tests assert retry counts.
type fakeProvider struct {
	mu sync.Mutex

	name   string
	models []string

	generateQueue []genCall
	generateCalls int

	generateStreamQueue []genStreamCall
	generateStreamCalls int

	embedQueue []embedCall
	embedCalls int

	imageQueue []imageCall
	imageCalls int

	audioQueue []audioCall
	audioCalls int

	healthErr   error
	healthCalls int

	// onCall, if set, is invoked (under lock) before each Generate call — useful for
	// recording timestamps to check backoff timing.
	onGenerateCall func(attempt int)
}

type genCall struct {
	resp *GenerateResponse
	err  error
}

type genStreamCall struct {
	ch  <-chan *GenerateResponse
	err error
}

type embedCall struct {
	vec []float32
	err error
}

type imageCall struct {
	resp *ImageResponse
	err  error
}

type audioCall struct {
	resp *AudioResponse
	err  error
}

func newFakeProvider(name string) *fakeProvider {
	return &fakeProvider{name: name, models: []string{"fake-model"}}
}

// popOrLast returns queue[idx] if in range, otherwise the last element (sticky), so tests
// don't need to pad queues to the exact expected call count.
func popOrLast[T any](queue []T, idx int) (T, bool) {
	var zero T
	if len(queue) == 0 {
		return zero, false
	}
	if idx < len(queue) {
		return queue[idx], true
	}
	return queue[len(queue)-1], true
}

func (f *fakeProvider) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error) {
	f.mu.Lock()
	idx := f.generateCalls
	f.generateCalls++
	cb := f.onGenerateCall
	f.mu.Unlock()
	if cb != nil {
		cb(idx)
	}
	call, ok := popOrLast(f.generateQueue, idx)
	if !ok {
		return &GenerateResponse{Content: "ok"}, nil
	}
	return call.resp, call.err
}

func (f *fakeProvider) GenerateStream(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	f.mu.Lock()
	idx := f.generateStreamCalls
	f.generateStreamCalls++
	f.mu.Unlock()
	call, ok := popOrLast(f.generateStreamQueue, idx)
	if !ok {
		ch := make(chan *GenerateResponse)
		close(ch)
		return ch, nil
	}
	return call.ch, call.err
}

func (f *fakeProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	idx := f.embedCalls
	f.embedCalls++
	f.mu.Unlock()
	call, ok := popOrLast(f.embedQueue, idx)
	if !ok {
		return []float32{0.1, 0.2}, nil
	}
	return call.vec, call.err
}

func (f *fakeProvider) ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error) {
	f.mu.Lock()
	idx := f.imageCalls
	f.imageCalls++
	f.mu.Unlock()
	call, ok := popOrLast(f.imageQueue, idx)
	if !ok {
		return &ImageResponse{URL: "http://example.com/image.png"}, nil
	}
	return call.resp, call.err
}

func (f *fakeProvider) AudioGenerate(ctx context.Context, req *AudioGenerateRequest) (*AudioResponse, error) {
	f.mu.Lock()
	idx := f.audioCalls
	f.audioCalls++
	f.mu.Unlock()
	call, ok := popOrLast(f.audioQueue, idx)
	if !ok {
		return &AudioResponse{URL: "http://example.com/audio.mp3"}, nil
	}
	return call.resp, call.err
}

func (f *fakeProvider) GetName() string { return f.name }

func (f *fakeProvider) GetModels() []string { return f.models }

func (f *fakeProvider) HealthCheck(ctx context.Context) error {
	f.mu.Lock()
	f.healthCalls++
	f.mu.Unlock()
	return f.healthErr
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generateCalls
}

var _ AIProvider = (*fakeProvider)(nil)
