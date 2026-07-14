package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── ResolveTimeout ──────────────────────────────────────────────────────────

func TestResolveTimeout(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"zero returns default", 0, DefaultProviderTimeout},
		{"negative returns default", -10, DefaultProviderTimeout},
		{"positive converts to seconds", 30, 30 * time.Second},
		{"large value", 600, 600 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveTimeout(tt.seconds); got != tt.want {
				t.Errorf("ResolveTimeout(%d) = %v, want %v", tt.seconds, got, tt.want)
			}
		})
	}
}

// ── isRetryable ─────────────────────────────────────────────────────────────

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"temporary", errors.New("temporary network error"), true},
		{"429 in message", errors.New("http 429 too many requests"), true},
		{"502", errors.New("bad gateway 502"), true},
		{"503", errors.New("service unavailable: 503"), true},
		{"rate limit", errors.New("rate limit exceeded"), true},
		{"overloaded", errors.New("model is overloaded"), true},
		{"concurrent limit", errors.New("concurrent limit reached"), true},
		{"50430 code", errors.New(`{"code":50430,"message":"concurrent"}`), true},
		{"too many", errors.New("too many requests"), true},
		{"case insensitive", errors.New("RATE LIMIT hit"), true},
		{"non-retryable 400", errors.New("400 bad request: invalid parameter"), false},
		{"generic error", errors.New("something went wrong"), false},
		{"context deadline exceeded is not retryable", errors.New("context deadline exceeded"), false},
		{"client.Timeout not retryable", errors.New("Get \"x\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)"), false},
		{"request timed out not retryable", errors.New("request timed out"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ── isConcurrentLimitError ──────────────────────────────────────────────────

func TestIsConcurrentLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"50430 code", errors.New("code=50430"), true},
		{"concurrent limit phrase", errors.New("concurrent limit exceeded"), true},
		{"case insensitive", errors.New("CONCURRENT LIMIT"), true},
		{"unrelated error", errors.New("connection refused"), false},
		{"rate limit but not concurrent", errors.New("rate limit exceeded"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConcurrentLimitError(tt.err); got != tt.want {
				t.Errorf("isConcurrentLimitError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ── IsTimeoutError ──────────────────────────────────────────────────────────

func TestIsTimeoutError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline exceeded", errors.New("context deadline exceeded"), true},
		{"client timeout", errors.New("net/http: Client.Timeout exceeded while awaiting headers"), true},
		{"request timed out", errors.New("request timed out"), true},
		{"case insensitive", errors.New("CONTEXT DEADLINE EXCEEDED"), true},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTimeoutError(tt.err); got != tt.want {
				t.Errorf("IsTimeoutError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ── isRetryableStatus ───────────────────────────────────────────────────────

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusInternalServerError, false},
		{http.StatusUnauthorized, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.status), func(t *testing.T) {
			if got := isRetryableStatus(tt.status); got != tt.want {
				t.Errorf("isRetryableStatus(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

// ── shouldPenalizeCB (exercised indirectly via retry behavior below, plus directly) ──

func TestShouldPenalizeCB(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"timeout not penalized", errors.New("context deadline exceeded"), false},
		{"429 not penalized", errors.New("429 too many requests"), false},
		{"rate limit not penalized", errors.New("rate limit exceeded"), false},
		{"context canceled not penalized", errors.New("context canceled"), false},
		{"concurrent limit not penalized", errors.New("concurrent limit"), false},
		{"connection refused is penalized", errors.New("connection refused"), true},
		{"generic 500 is penalized", errors.New("internal server error"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPenalizeCB(tt.err); got != tt.want {
				t.Errorf("shouldPenalizeCB(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ── RetryProvider ────────────────────────────────────────────────────────────

func TestNewRetryProvider_DefaultsAppliedForInvalidArgs(t *testing.T) {
	inner := newFakeProvider("p")
	rp := NewRetryProvider(inner, 0, 0)
	if rp.maxRetries != 3 {
		t.Errorf("maxRetries = %d, want default 3", rp.maxRetries)
	}
	if rp.baseDelay != 500*time.Millisecond {
		t.Errorf("baseDelay = %v, want default 500ms", rp.baseDelay)
	}

	rp2 := NewRetryProvider(inner, -1, -1)
	if rp2.maxRetries != 3 || rp2.baseDelay != 500*time.Millisecond {
		t.Errorf("expected defaults for negative args, got maxRetries=%d baseDelay=%v", rp2.maxRetries, rp2.baseDelay)
	}
}

func TestRetryProvider_Generate_SucceedsWithoutRetryOnFirstTry(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{{resp: &GenerateResponse{Content: "ok"}}}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("resp.Content = %q", resp.Content)
	}
	if inner.callCount() != 1 {
		t.Errorf("expected exactly 1 call, got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_RetriesConfiguredNumberOfTimesOnRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{err: errors.New("connection refused")},
		{err: errors.New("connection refused")},
		{err: errors.New("connection refused")},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	_, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "failed after 3 attempts") {
		t.Errorf("error message = %q, want mention of 3 attempts", err.Error())
	}
	if inner.callCount() != 3 {
		t.Errorf("expected exactly 3 calls (maxRetries), got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_StopsImmediatelyOnNonRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{err: errors.New("400 bad request: invalid model")},
	}
	rp := NewRetryProvider(inner, 5, time.Millisecond)

	_, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if inner.callCount() != 1 {
		t.Errorf("expected exactly 1 call for non-retryable error (no retry), got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_SucceedsAfterTransientRetryableFailures(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{err: errors.New("503 service unavailable")},
		{err: errors.New("503 service unavailable")},
		{resp: &GenerateResponse{Content: "recovered"}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("resp.Content = %q, want recovered", resp.Content)
	}
	if inner.callCount() != 3 {
		t.Errorf("expected 3 calls (2 failures + success), got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_RespondsWithNonRetryableResponseErrorFieldImmediately(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{resp: &GenerateResponse{Error: "400 invalid request body"}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate returned unexpected error (should return resp,nil for non-retryable resp.Error): %v", err)
	}
	if resp.Error != "400 invalid request body" {
		t.Errorf("resp.Error = %q", resp.Error)
	}
	if inner.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_RetriesOnRetryableResponseErrorField(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{resp: &GenerateResponse{Error: "rate limit exceeded"}},
		{resp: &GenerateResponse{Content: "ok now"}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Content != "ok now" {
		t.Errorf("resp.Content = %q", resp.Content)
	}
	if inner.callCount() != 2 {
		t.Errorf("expected 2 calls, got %d", inner.callCount())
	}
}

func TestRetryProvider_Generate_ContextCancelDuringBackoffReturnsCtxErr(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{err: errors.New("connection refused")},
		{err: errors.New("connection refused")},
	}
	rp := NewRetryProvider(inner, 5, 200*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := rp.Generate(ctx, &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error when context is canceled during backoff wait")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestRetryProvider_Generate_BackoffDelayRoughlyDoubles(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{
		{err: errors.New("connection refused")},
		{err: errors.New("connection refused")},
		{resp: &GenerateResponse{Content: "ok"}},
	}
	baseDelay := 20 * time.Millisecond
	rp := NewRetryProvider(inner, 5, baseDelay)

	var timestamps []time.Time
	inner.onGenerateCall = func(attempt int) {
		timestamps = append(timestamps, time.Now())
	}

	_, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(timestamps) != 3 {
		t.Fatalf("expected 3 timestamps, got %d", len(timestamps))
	}
	gap1 := timestamps[1].Sub(timestamps[0])
	gap2 := timestamps[2].Sub(timestamps[1])
	// gap1 ~ baseDelay*1, gap2 ~ baseDelay*2 (exponential backoff 1<<attempt-1).
	if gap1 < baseDelay {
		t.Errorf("gap1 = %v, want >= %v (first retry delay)", gap1, baseDelay)
	}
	if gap2 < gap1 {
		t.Errorf("gap2 = %v should be >= gap1 = %v (exponential backoff)", gap2, gap1)
	}
}

func TestRetryProvider_Generate_CircuitBreakerOpensAfterRepeatedPenalizedFailures(t *testing.T) {
	inner := newFakeProvider("p")
	// Each Generate call exhausts all retries with a penalizing (non-retryable, non-timeout,
	// non-ratelimit) error, so every call to rp.Generate contributes one RecordFailure().
	inner.generateQueue = []genCall{{err: errors.New("500 internal server error")}}
	rp := NewRetryProvider(inner, 1, time.Millisecond) // maxRetries=1 => no in-call retry loop waiting

	// Default circuit breaker threshold created in NewRetryProvider is 5.
	var lastErr error
	for i := 0; i < 5; i++ {
		_, lastErr = rp.Generate(context.Background(), &GenerateRequest{})
		if lastErr == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	// The 6th call should be short-circuited by the open breaker without calling inner again.
	callsBefore := inner.callCount()
	_, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected circuit breaker open error")
	}
	if !strings.Contains(err.Error(), "circuit breaker open") {
		t.Errorf("error = %q, want circuit breaker open message", err.Error())
	}
	if inner.callCount() != callsBefore {
		t.Errorf("expected no additional inner call once circuit is open, calls went from %d to %d", callsBefore, inner.callCount())
	}
}

func TestRetryProvider_Generate_NonPenalizingErrorDoesNotOpenCircuit(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{{err: errors.New("rate limit exceeded")}}
	rp := NewRetryProvider(inner, 1, time.Millisecond)

	for i := 0; i < 10; i++ {
		_, err := rp.Generate(context.Background(), &GenerateRequest{})
		if err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	// Circuit should still be closed (rate limit errors don't penalize CB).
	if err := rp.cb.Err(); err != nil {
		t.Errorf("expected circuit still closed after repeated rate-limit errors, got %v", err)
	}
}

func TestRetryProvider_ResetCircuitClosesOpenBreaker(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateQueue = []genCall{{err: errors.New("500 internal server error")}}
	rp := NewRetryProvider(inner, 1, time.Millisecond)

	for i := 0; i < 5; i++ {
		_, _ = rp.Generate(context.Background(), &GenerateRequest{})
	}
	if err := rp.cb.Err(); err == nil {
		t.Fatal("expected circuit breaker to be open before reset")
	}
	rp.ResetCircuit()
	if err := rp.cb.Err(); err != nil {
		t.Errorf("expected circuit breaker closed after ResetCircuit, got %v", err)
	}
}

func TestRetryProvider_Generate_FailsFastWhenCircuitAlreadyOpen(t *testing.T) {
	inner := newFakeProvider("p")
	rp := NewRetryProvider(inner, 3, time.Millisecond)
	rp.cb.open = true
	rp.cb.lastFailure = time.Now()
	rp.cb.threshold = 1
	rp.cb.resetTimeout = time.Hour

	_, err := rp.Generate(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected immediate error when circuit already open")
	}
	if inner.callCount() != 0 {
		t.Errorf("expected 0 inner calls when circuit pre-open, got %d", inner.callCount())
	}
}

func TestRetryProvider_GenerateStream_RetriesOnRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	okCh := make(chan *GenerateResponse)
	close(okCh)
	inner.generateStreamQueue = []genStreamCall{
		{err: errors.New("connection refused")},
		{ch: okCh},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	ch, err := rp.GenerateStream(context.Background(), &GenerateRequest{})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if inner.generateStreamCalls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.generateStreamCalls)
	}
}

func TestRetryProvider_GenerateStream_StopsOnNonRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateStreamQueue = []genStreamCall{{err: errors.New("400 bad request")}}
	rp := NewRetryProvider(inner, 5, time.Millisecond)

	_, err := rp.GenerateStream(context.Background(), &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if inner.generateStreamCalls != 1 {
		t.Errorf("expected exactly 1 call, got %d", inner.generateStreamCalls)
	}
}

func TestRetryProvider_GenerateStream_ExhaustsRetriesAndReturnsWrappedError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.generateStreamQueue = []genStreamCall{{err: errors.New("502 bad gateway")}}
	rp := NewRetryProvider(inner, 2, time.Millisecond)

	_, err := rp.GenerateStream(context.Background(), &GenerateRequest{})
	if err == nil || !strings.Contains(err.Error(), "GenerateStream failed after 2 attempts") {
		t.Errorf("err = %v, want wrapped failure message", err)
	}
	if inner.generateStreamCalls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.generateStreamCalls)
	}
}

func TestRetryProvider_Embed_RetriesAndSucceeds(t *testing.T) {
	inner := newFakeProvider("p")
	inner.embedQueue = []embedCall{
		{err: errors.New("connection refused")},
		{vec: []float32{1, 2}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	vec, err := rp.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vec = %v", vec)
	}
	if inner.embedCalls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.embedCalls)
	}
}

func TestRetryProvider_Embed_NonRetryableFailsImmediately(t *testing.T) {
	inner := newFakeProvider("p")
	inner.embedQueue = []embedCall{{err: errors.New("400 bad request")}}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	_, err := rp.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if inner.embedCalls != 1 {
		t.Errorf("expected 1 call, got %d", inner.embedCalls)
	}
}

func TestRetryProvider_ImageGenerate_RetriesOnRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.imageQueue = []imageCall{
		{err: errors.New("503 service unavailable")},
		{resp: &ImageResponse{URL: "http://x/img.png"}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("ImageGenerate: %v", err)
	}
	if resp.URL != "http://x/img.png" {
		t.Errorf("resp.URL = %q", resp.URL)
	}
	if inner.imageCalls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.imageCalls)
	}
}

func TestRetryProvider_ImageGenerate_ConcurrentLimitErrorUsesLongerDelay(t *testing.T) {
	inner := newFakeProvider("p")
	inner.imageQueue = []imageCall{
		{err: errors.New("code=50430 concurrent limit")},
		{resp: &ImageResponse{URL: "ok"}},
	}
	// Use a large baseDelay so the concurrent-limit-specific override (6s*attempt) would matter,
	// but cap the observation via a short-enough real wait: attempt=1 => concurrentLimitDelay = 6s*1 = 6s
	// which is too slow for a unit test. Instead verify the delay is at least the base exponential
	// delay, and that on attempt=1 the call still eventually succeeds (functional, not timing-strict).
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	start := time.Now()
	resp, err := rp.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ImageGenerate: %v", err)
	}
	if resp.URL != "ok" {
		t.Errorf("resp.URL = %q", resp.URL)
	}
	// concurrentLimitDelay for attempt=1 is 6s*1=6s, which the code applies since it exceeds
	// the tiny exponential delay. Confirm the wait was actually lengthened towards that order
	// of magnitude (allow slack, but it must be well above the 1ms base delay).
	if elapsed < time.Second {
		t.Errorf("expected concurrent-limit backoff to force a multi-second wait, got %v", elapsed)
	}
}

func TestRetryProvider_ImageGenerate_NonRetryableResponseErrorReturnsRespNoErr(t *testing.T) {
	inner := newFakeProvider("p")
	inner.imageQueue = []imageCall{{resp: &ImageResponse{Error: "400 invalid size"}}}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "400 invalid size" {
		t.Errorf("resp.Error = %q", resp.Error)
	}
	if inner.imageCalls != 1 {
		t.Errorf("expected 1 call, got %d", inner.imageCalls)
	}
}

func TestRetryProvider_AudioGenerate_RetriesOnRetryableError(t *testing.T) {
	inner := newFakeProvider("p")
	inner.audioQueue = []audioCall{
		{err: errors.New("connection refused")},
		{resp: &AudioResponse{URL: "http://x/audio.mp3"}},
	}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.AudioGenerate(context.Background(), &AudioGenerateRequest{})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL != "http://x/audio.mp3" {
		t.Errorf("resp.URL = %q", resp.URL)
	}
	if inner.audioCalls != 2 {
		t.Errorf("expected 2 calls, got %d", inner.audioCalls)
	}
}

func TestRetryProvider_AudioGenerate_NonRetryableResponseErrorReturnsRespNoErr(t *testing.T) {
	inner := newFakeProvider("p")
	inner.audioQueue = []audioCall{{resp: &AudioResponse{Error: "400 invalid voice"}}}
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	resp, err := rp.AudioGenerate(context.Background(), &AudioGenerateRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "400 invalid voice" {
		t.Errorf("resp.Error = %q", resp.Error)
	}
}

func TestRetryProvider_DelegatesNameModelsHealthCheck(t *testing.T) {
	inner := newFakeProvider("delegate-test")
	rp := NewRetryProvider(inner, 3, time.Millisecond)

	if rp.GetName() != "delegate-test" {
		t.Errorf("GetName() = %q", rp.GetName())
	}
	if len(rp.GetModels()) != 1 {
		t.Errorf("GetModels() = %v", rp.GetModels())
	}
	if err := rp.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v", err)
	}
}

var _ AIProvider = (*RetryProvider)(nil)

// ── ModelManager ──────────────────────────────────────────────────────────

func TestModelManager_RegisterAndGetProvider(t *testing.T) {
	m := NewModelManager()
	p := newFakeProvider("p1")
	m.RegisterProvider("p1", p)

	got, err := m.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.GetName() != "p1" {
		t.Errorf("got.GetName() = %q", got.GetName())
	}
}

func TestModelManager_GetProvider_NotFound(t *testing.T) {
	m := NewModelManager()
	_, err := m.GetProvider("missing")
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestModelManager_GetProvider_EmptyNameUsesDefault(t *testing.T) {
	m := NewModelManager()
	m.RegisterProvider("p1", newFakeProvider("p1"))
	if err := m.SetDefault("p1"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	got, err := m.GetProvider("")
	if err != nil {
		t.Fatalf("GetProvider(\"\"): %v", err)
	}
	if got.GetName() != "p1" {
		t.Errorf("got.GetName() = %q, want p1", got.GetName())
	}
}

func TestModelManager_SetDefault_NotFoundErrors(t *testing.T) {
	m := NewModelManager()
	if err := m.SetDefault("missing"); err == nil {
		t.Fatal("expected error setting default to unregistered provider")
	}
}

func TestModelManager_ListProviders(t *testing.T) {
	m := NewModelManager()
	m.RegisterProvider("a", newFakeProvider("a"))
	m.RegisterProvider("b", newFakeProvider("b"))

	names := m.ListProviders()
	if len(names) != 2 {
		t.Fatalf("ListProviders() = %v, want 2 entries", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("ListProviders() = %v, want a and b", names)
	}
}

func TestModelManager_RegisterAndGetImageProviders(t *testing.T) {
	m := NewModelManager()
	m.RegisterImageProvider("prov1", "model1", "1024x1024")
	m.RegisterImageProvider("prov2", "model2", "2048x2048")

	entries := m.GetImageProviders()
	if len(entries) != 2 {
		t.Fatalf("GetImageProviders() = %v, want 2 entries", entries)
	}
	if entries[0].ProviderName != "prov1" || entries[0].Model != "model1" || entries[0].Size != "1024x1024" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[1].ProviderName != "prov2" {
		t.Errorf("entries[1] = %+v", entries[1])
	}
}

func TestModelManager_SwitchProvider(t *testing.T) {
	m := NewModelManager()
	m.RegisterProvider("a", newFakeProvider("a"))
	m.RegisterProvider("b", newFakeProvider("b"))
	if err := m.SwitchProvider("b"); err != nil {
		t.Fatalf("SwitchProvider: %v", err)
	}
	got, err := m.GetProvider("")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.GetName() != "b" {
		t.Errorf("default provider = %q, want b", got.GetName())
	}
}

func TestModelManager_SwitchProvider_NotFoundErrors(t *testing.T) {
	m := NewModelManager()
	if err := m.SwitchProvider("missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestModelManager_WrapWithRetry(t *testing.T) {
	m := NewModelManager()
	m.RegisterProvider("p1", newFakeProvider("p1"))

	if err := m.WrapWithRetry("p1", 3, time.Millisecond); err != nil {
		t.Fatalf("WrapWithRetry: %v", err)
	}
	got, err := m.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if _, ok := got.(*RetryProvider); !ok {
		t.Errorf("expected provider to be wrapped as *RetryProvider, got %T", got)
	}
}

func TestModelManager_WrapWithRetry_NotFoundErrors(t *testing.T) {
	m := NewModelManager()
	if err := m.WrapWithRetry("missing", 3, time.Millisecond); err == nil {
		t.Fatal("expected error")
	}
}

func TestModelManager_WrapWithRetry_AvoidsDoubleWrapping(t *testing.T) {
	m := NewModelManager()
	m.RegisterProvider("p1", newFakeProvider("p1"))
	if err := m.WrapWithRetry("p1", 3, time.Millisecond); err != nil {
		t.Fatalf("first WrapWithRetry: %v", err)
	}
	first, _ := m.GetProvider("p1")

	if err := m.WrapWithRetry("p1", 5, 2*time.Millisecond); err != nil {
		t.Fatalf("second WrapWithRetry: %v", err)
	}
	second, _ := m.GetProvider("p1")
	if first != second {
		t.Error("expected WrapWithRetry to be a no-op when already wrapped (same instance)")
	}
}

// ── GenerateRequestBuilder ──────────────────────────────────────────────────

func TestGenerateRequestBuilder_Defaults(t *testing.T) {
	req := NewGenerateRequestBuilder().Build()
	if req.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", req.Temperature)
	}
	if req.TopP != 0.9 {
		t.Errorf("TopP = %v, want 0.9", req.TopP)
	}
	if req.TopK != 40 {
		t.Errorf("TopK = %v, want 40", req.TopK)
	}
}

func TestGenerateRequestBuilder_FluentChaining(t *testing.T) {
	req := NewGenerateRequestBuilder().
		Model("gpt-4").
		SystemPrompt("you are helpful").
		UserMessage("hello").
		AssistantMessage("hi there").
		Temperature(0.5).
		MaxTokens(100).
		TopP(0.8).
		TopK(20).
		Stop([]string{"\n"}).
		Extra("key1", "value1").
		Build()

	if req.Model != "gpt-4" {
		t.Errorf("Model = %q", req.Model)
	}
	if req.SystemPrompt != "you are helpful" {
		t.Errorf("SystemPrompt = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %v, want 2", req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content != "hello" {
		t.Errorf("Messages[0] = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "assistant" || req.Messages[1].Content != "hi there" {
		t.Errorf("Messages[1] = %+v", req.Messages[1])
	}
	if req.Temperature != 0.5 {
		t.Errorf("Temperature = %v", req.Temperature)
	}
	if req.MaxTokens != 100 {
		t.Errorf("MaxTokens = %v", req.MaxTokens)
	}
	if req.TopP != 0.8 {
		t.Errorf("TopP = %v", req.TopP)
	}
	if req.TopK != 20 {
		t.Errorf("TopK = %v", req.TopK)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "\n" {
		t.Errorf("Stop = %v", req.Stop)
	}
	if req.Extra["key1"] != "value1" {
		t.Errorf("Extra[key1] = %v", req.Extra["key1"])
	}
}

func TestGenerateRequestBuilder_ExtraInitializesMapLazily(t *testing.T) {
	req := NewGenerateRequestBuilder().Build()
	if req.Extra != nil {
		t.Fatal("expected Extra to be nil before first call")
	}
	req2 := NewGenerateRequestBuilder().Extra("a", 1).Extra("b", 2).Build()
	if len(req2.Extra) != 2 {
		t.Errorf("Extra = %v, want 2 entries", req2.Extra)
	}
}

// ── CostEstimator ────────────────────────────────────────────────────────────

func TestCostEstimator_EstimateCost_KnownProviderModel(t *testing.T) {
	e := NewCostEstimator()
	cost := e.EstimateCost("openai", "gpt-4", 1000, 1000)
	// input: 1000/1000*0.03 = 0.03 ; output: 1000/1000*0.03*2 = 0.06 ; total = 0.09
	want := 0.09
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("EstimateCost = %v, want %v", cost, want)
	}
}

func TestCostEstimator_EstimateCost_UnknownProviderReturnsZero(t *testing.T) {
	e := NewCostEstimator()
	if cost := e.EstimateCost("unknown-provider", "unknown-model", 100, 100); cost != 0 {
		t.Errorf("EstimateCost = %v, want 0 for unknown provider", cost)
	}
}

func TestCostEstimator_EstimateCost_UnknownModelReturnsZero(t *testing.T) {
	e := NewCostEstimator()
	if cost := e.EstimateCost("openai", "unknown-model", 100, 100); cost != 0 {
		t.Errorf("EstimateCost = %v, want 0 for unknown model", cost)
	}
}

func TestCostEstimator_EstimateCost_ZeroTokens(t *testing.T) {
	e := NewCostEstimator()
	if cost := e.EstimateCost("anthropic", "claude-3-sonnet", 0, 0); cost != 0 {
		t.Errorf("EstimateCost = %v, want 0", cost)
	}
}

// ── UsageLogger ──────────────────────────────────────────────────────────────

func TestUsageLogger_LogAndGetStats(t *testing.T) {
	l := &UsageLogger{}
	l.Log(UsageLogEntry{Provider: "openai", Model: "gpt-4", InputTokens: 100, OutputTokens: 50, LatencyMs: 200, Cost: 0.01, Success: true})
	l.Log(UsageLogEntry{Provider: "openai", Model: "gpt-4", InputTokens: 200, OutputTokens: 100, LatencyMs: 400, Cost: 0.02, Success: false, Error: "boom"})
	l.Log(UsageLogEntry{Provider: "anthropic", Model: "claude-3-opus", InputTokens: 50, OutputTokens: 25, LatencyMs: 100, Cost: 0.005, Success: true})

	stats := l.GetStats("openai", "gpt-4")
	if stats.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", stats.TotalRequests)
	}
	if stats.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", stats.SuccessCount)
	}
	if stats.TotalInputTokens != 300 {
		t.Errorf("TotalInputTokens = %d, want 300", stats.TotalInputTokens)
	}
	if stats.TotalOutputTokens != 150 {
		t.Errorf("TotalOutputTokens = %d, want 150", stats.TotalOutputTokens)
	}
	if diff := stats.TotalCost - 0.03; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("TotalCost = %v, want 0.03", stats.TotalCost)
	}
	if stats.TotalLatency != 600 {
		t.Errorf("TotalLatency = %d, want 600", stats.TotalLatency)
	}
	if stats.AverageLatency != 300 {
		t.Errorf("AverageLatency = %d, want 300", stats.AverageLatency)
	}
	if diff := stats.SuccessRate - 0.5; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("SuccessRate = %v, want 0.5", stats.SuccessRate)
	}
}

func TestUsageLogger_GetStats_NoFilterAggregatesAll(t *testing.T) {
	l := &UsageLogger{}
	l.Log(UsageLogEntry{Provider: "openai", Model: "gpt-4", Success: true})
	l.Log(UsageLogEntry{Provider: "anthropic", Model: "claude-3-opus", Success: true})

	stats := l.GetStats("", "")
	if stats.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", stats.TotalRequests)
	}
}

func TestUsageLogger_GetStats_NoMatchingEntriesReturnsZeroStats(t *testing.T) {
	l := &UsageLogger{}
	l.Log(UsageLogEntry{Provider: "openai", Model: "gpt-4", Success: true})

	stats := l.GetStats("nonexistent", "")
	if stats.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0", stats.TotalRequests)
	}
	if stats.AverageLatency != 0 || stats.SuccessRate != 0 {
		t.Errorf("expected zero averages when no requests, got %+v", stats)
	}
}

// ── ModelHealthChecker ───────────────────────────────────────────────────────

func TestModelHealthChecker_CheckHealthyProvider(t *testing.T) {
	h := NewModelHealthChecker()
	p := newFakeProvider("p1")
	h.Register("p1", p)

	status, err := h.Check("p1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !status.Healthy {
		t.Errorf("expected Healthy=true, got %+v", status)
	}
	if status.Message != "" {
		t.Errorf("expected empty Message on success, got %q", status.Message)
	}
}

func TestModelHealthChecker_CheckUnhealthyProvider(t *testing.T) {
	h := NewModelHealthChecker()
	p := newFakeProvider("p1")
	p.healthErr = errors.New("connection refused")
	h.Register("p1", p)

	status, err := h.Check("p1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status.Healthy {
		t.Error("expected Healthy=false")
	}
	if status.Message != "connection refused" {
		t.Errorf("Message = %q", status.Message)
	}
}

func TestModelHealthChecker_Check_NotFoundErrors(t *testing.T) {
	h := NewModelHealthChecker()
	_, err := h.Check("missing")
	if err == nil {
		t.Fatal("expected error for unregistered provider")
	}
}

func TestModelHealthChecker_CheckAll(t *testing.T) {
	h := NewModelHealthChecker()
	h.Register("good", newFakeProvider("good"))
	bad := newFakeProvider("bad")
	bad.healthErr = errors.New("down")
	h.Register("bad", bad)

	results := h.CheckAll()
	if len(results) != 2 {
		t.Fatalf("CheckAll() = %v, want 2 entries", results)
	}
	if !results["good"].Healthy {
		t.Errorf("good provider should be healthy: %+v", results["good"])
	}
	if results["bad"].Healthy {
		t.Errorf("bad provider should be unhealthy: %+v", results["bad"])
	}
}

// ── FallbackManager ──────────────────────────────────────────────────────────

func TestFallbackManager_GetPrimaryAndFallbacks(t *testing.T) {
	f := NewFallbackManager("primary", "fb1", "fb2", "fb3")
	if f.GetPrimary() != "primary" {
		t.Errorf("GetPrimary() = %q", f.GetPrimary())
	}
	fbs := f.GetFallbacks()
	if len(fbs) != 3 || fbs[0] != "fb1" || fbs[2] != "fb3" {
		t.Errorf("GetFallbacks() = %v", fbs)
	}
}

func TestFallbackManager_GetNext_FromPrimary(t *testing.T) {
	f := NewFallbackManager("primary", "fb1", "fb2")
	if next := f.GetNext("primary"); next != "fb1" {
		t.Errorf("GetNext(primary) = %q, want fb1", next)
	}
}

func TestFallbackManager_GetNext_ChainsThroughFallbacks(t *testing.T) {
	f := NewFallbackManager("primary", "fb1", "fb2", "fb3")
	if next := f.GetNext("fb1"); next != "fb2" {
		t.Errorf("GetNext(fb1) = %q, want fb2", next)
	}
	if next := f.GetNext("fb2"); next != "fb3" {
		t.Errorf("GetNext(fb2) = %q, want fb3", next)
	}
}

func TestFallbackManager_GetNext_LastFallbackReturnsEmpty(t *testing.T) {
	f := NewFallbackManager("primary", "fb1", "fb2")
	if next := f.GetNext("fb2"); next != "" {
		t.Errorf("GetNext(last fallback) = %q, want empty", next)
	}
}

func TestFallbackManager_GetNext_NoFallbacksConfigured(t *testing.T) {
	f := NewFallbackManager("primary")
	if next := f.GetNext("primary"); next != "" {
		t.Errorf("GetNext(primary) with no fallbacks = %q, want empty", next)
	}
}

func TestFallbackManager_GetNext_UnknownCurrentReturnsEmpty(t *testing.T) {
	f := NewFallbackManager("primary", "fb1")
	if next := f.GetNext("unknown"); next != "" {
		t.Errorf("GetNext(unknown) = %q, want empty", next)
	}
}

// ── FormatJSON ───────────────────────────────────────────────────────────────

func TestFormatJSON(t *testing.T) {
	type sample struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	got := FormatJSON(sample{Name: "alice", Age: 30})
	if !strings.Contains(got, `"name": "alice"`) {
		t.Errorf("FormatJSON output = %q, want indented JSON with name field", got)
	}
	if !strings.Contains(got, `"age": 30`) {
		t.Errorf("FormatJSON output = %q, want indented JSON with age field", got)
	}
}

func TestFormatJSON_Nil(t *testing.T) {
	got := FormatJSON(nil)
	if got != "null" {
		t.Errorf("FormatJSON(nil) = %q, want null", got)
	}
}
