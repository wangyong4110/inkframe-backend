package ai

import (
	"context"
	"errors"
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

// ── minimal fakeProvider (for ModelHealthChecker tests) ─────────────────────

type fakeProvider struct {
	name      string
	healthErr error
}

func newFakeProvider(name string) *fakeProvider { return &fakeProvider{name: name} }

func (f *fakeProvider) Generate(_ context.Context, _ *GenerateRequest) (*GenerateResponse, error) {
	return nil, nil
}
func (f *fakeProvider) GenerateStream(_ context.Context, _ *GenerateRequest) (<-chan *GenerateResponse, error) {
	return nil, nil
}
func (f *fakeProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (f *fakeProvider) ImageGenerate(_ context.Context, _ *ImageGenerateRequest) (*ImageResponse, error) {
	return nil, nil
}
func (f *fakeProvider) AudioGenerate(_ context.Context, _ *AudioGenerateRequest) (*AudioResponse, error) {
	return nil, nil
}
func (f *fakeProvider) GetName() string                       { return f.name }
func (f *fakeProvider) HealthCheck(_ context.Context) error   { return f.healthErr }

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
