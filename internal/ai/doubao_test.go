package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewDoubaoProvider_Defaults(t *testing.T) {
	p := NewDoubaoProvider("key", "", "", 0)
	if p.endpoint != "https://ark.volces.com/api/v3" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "doubao-pro-32k" {
		t.Errorf("model = %q, want doubao-pro-32k", p.model)
	}
	if p.client.Timeout != DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewDoubaoProvider_CustomValues(t *testing.T) {
	p := NewDoubaoProvider("key", "https://custom", "doubao-lite-4k", 8*time.Second)
	if p.endpoint != "https://custom" || p.model != "doubao-lite-4k" {
		t.Errorf("provider = %+v", p)
	}
	if p.client.Timeout != 8*time.Second {
		t.Errorf("timeout = %v, want 8s", p.client.Timeout)
	}
}

func TestDoubaoProvider_GetName(t *testing.T) {
	p := NewDoubaoProvider("key", "", "", 0)
	if p.GetName() != ProviderNameDoubao {
		t.Errorf("GetName() = %q, want %q", p.GetName(), ProviderNameDoubao)
	}
	if p.GetName() != "doubao" {
		t.Errorf("GetName() = %q, want doubao", p.GetName())
	}
}

func TestDoubaoProvider_GetModels(t *testing.T) {
	p := NewDoubaoProvider("key", "", "", 0)
	if len(p.GetModels()) == 0 {
		t.Error("expected non-empty models list")
	}
}

func TestDoubaoProvider_HealthCheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"ok", http.StatusOK, false},
		{"unauthorized", http.StatusUnauthorized, true},
		{"server error", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			p := NewDoubaoProvider("key", server.URL, "", 0)
			err := p.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDoubaoProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"doubao-pro-32k",
			"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":2}
		}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ChatMessage{{Role: "user", Content: "hi"}},
		Temperature:  0.6,
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.InputTokens != 4 || resp.Tokens != 2 {
		t.Errorf("tokens = (%d,%d), want (4,2)", resp.InputTokens, resp.Tokens)
	}
	if gotBody["temperature"] != 0.6 {
		t.Errorf("temperature = %v, want 0.6", gotBody["temperature"])
	}
}

func TestDoubaoProvider_Generate_ReasoningContentFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"thinking"}}]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "thinking" {
		t.Errorf("Content = %q, want fallback to reasoning_content", resp.Content)
	}
}

func TestDoubaoProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "豆包 API 错误") {
		t.Errorf("Error = %q, want 豆包 API 错误 prefix", resp.Error)
	}
}

func TestDoubaoProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestDoubaoProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"yo\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "yo" {
		t.Errorf("stream content = %q, want yo", got.String())
	}
}

func TestDoubaoProvider_Embed(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5,0.6]}]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "my-endpoint-id", 0)
	vec, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vec = %v, want 2 elements", vec)
	}
	if gotBody["model"] != "my-endpoint-id" {
		t.Errorf("model = %v, want configured endpoint id", gotBody["model"])
	}
}

func TestDoubaoProvider_Embed_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error")
	}
}

func TestDoubaoProvider_Embed_NoData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error for empty embedding data")
	}
}

func TestDoubaoProvider_ImageGenerate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %q, want /images/generations", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/seed.png","size":"1024x1024"}]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{Model: "doubao-seedream-4-0-250828", Prompt: "a cat", Size: "1024x1024"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.URL != "https://example.com/seed.png" {
		t.Errorf("URL = %q", resp.URL)
	}
	if gotBody["watermark"] != false {
		t.Errorf("watermark = %v, want false by default", gotBody["watermark"])
	}
}

func TestDoubaoProvider_ImageGenerate_SensitiveContentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"InputTextSensitiveContentDetected","message":"blocked"}}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected transport error: %v", err)
	}
	if !strings.HasPrefix(resp.Error, "Seedream 错误: "+ErrPrefixSensitiveContent) {
		t.Errorf("Error = %q, want ErrPrefixSensitiveContent marker", resp.Error)
	}
}

func TestDoubaoProvider_ImageGenerate_PartialFailureInGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"error":{"code":"x","message":"failed"}},
			{"url":"https://example.com/ok.png"}
		]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{Prompt: "x", SequentialImageGeneration: "auto"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.URL != "https://example.com/ok.png" {
		t.Errorf("URL = %q, want the one successful image", resp.URL)
	}
}

func TestDoubaoProvider_ImageGenerate_NoImageReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.Error != "no image returned" {
		t.Errorf("Error = %q, want no image returned", resp.Error)
	}
}

func TestDoubaoProvider_AudioGenerate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("path = %q, want /audio/speech", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-audio"))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "tts-endpoint", 0)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{Text: "hello", Voice: "zh_female_1"})
	if err != nil {
		// AudioGenerate writes to a hardcoded /tmp path (os.WriteFile), which some sandboxed
		// CI/dev environments deny even though /tmp is otherwise writable. The HTTP
		// request/response handling above this write is what we're actually testing here.
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("sandbox denies writes to hardcoded /tmp path: %v", err)
		}
		t.Fatalf("AudioGenerate() error: %v", err)
	}
	if !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("URL = %q, want file:// prefix", resp.URL)
	}
	_ = os.Remove(strings.TrimPrefix(resp.URL, "file://"))
}

func TestDoubaoProvider_AudioGenerate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewDoubaoProvider("key", server.URL, "", 0)
	if _, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{Text: "hi"}); err == nil {
		t.Error("expected error")
	}
}

// ─── Seedream helper functions ──────────────────────────────────────────

func TestSeedreamIsNewGen(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"doubao-seedream-4-0-250828", true},
		{"doubao-seedream-4-5-251128", true},
		{"doubao-seedream-5-0-260128", true},
		{"seededit-3-0-t2i-250428", false},
		{"doubao-pro-32k", false},
	}
	for _, tt := range tests {
		if got := seedreamIsNewGen(tt.model); got != tt.want {
			t.Errorf("seedreamIsNewGen(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestSeedreamIs5Lite(t *testing.T) {
	if !seedreamIs5Lite("doubao-seedream-5-0-260128") {
		t.Error("expected 5-0 model to be detected as 5 lite")
	}
	if seedreamIs5Lite("doubao-seedream-4-0-250828") {
		t.Error("4-0 model should not be detected as 5 lite")
	}
}

func TestSeedreamSize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "1024x1024"},
		{"512x768", "512x768"},
		{"2k", "2k"},
		{"1:1", "1024x1024"},
		{"16:9", "1820x1024"},
		{"9:16", "1024x1820"},
		{"garbage", "1024x1024"},
	}
	for _, tt := range tests {
		if got := seedreamSize(tt.in); got != tt.want {
			t.Errorf("seedreamSize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSeedreamModelMinPixels(t *testing.T) {
	if seedreamModelMinPixels("doubao-seedream-5-0-260128") != 3686400 {
		t.Error("5.0 min pixels mismatch")
	}
	if seedreamModelMinPixels("doubao-seedream-4-0-250828") != 921600 {
		t.Error("4.0 min pixels mismatch")
	}
	if seedreamModelMinPixels("seededit-3-0-t2i-250428") != 0 {
		t.Error("3.0 (old gen) should have no min pixels")
	}
}

func TestSeedreamModelMaxPixels(t *testing.T) {
	if seedreamModelMaxPixels("doubao-seedream-4-0-250828") != 0 {
		t.Error("new gen should have no max pixel cap")
	}
	if seedreamModelMaxPixels("seededit-3-0-t2i-250428") != 1048576 {
		t.Error("old gen should cap at 1048576")
	}
}

func TestSeedreamEnforceMinSize(t *testing.T) {
	// Old-gen model has no minimum, size unchanged.
	if got := seedreamEnforceMinSize("seededit-3-0-t2i-250428", "512x512"); got != "512x512" {
		t.Errorf("enforceMinSize (old gen) = %q, want unchanged", got)
	}
	// New-gen 4.0 requires >= 921600 px; 512x512 (262144) should be scaled up.
	got := seedreamEnforceMinSize("doubao-seedream-4-0-250828", "512x512")
	var w, h int
	if _, err := fmt.Sscanf(got, "%dx%d", &w, &h); err != nil {
		t.Fatalf("could not parse result %q", got)
	}
	if w*h < 921600 {
		t.Errorf("enforceMinSize result %q has %d px, want >= 921600", got, w*h)
	}
}

func TestSeedreamEnforceMaxSize(t *testing.T) {
	if got := seedreamEnforceMaxSize("512x512", 0); got != "512x512" {
		t.Errorf("enforceMaxSize with maxPx=0 should be no-op, got %q", got)
	}
	got := seedreamEnforceMaxSize("4096x4096", 1048576)
	var w, h int
	if _, err := fmt.Sscanf(got, "%dx%d", &w, &h); err != nil {
		t.Fatalf("could not parse result %q", got)
	}
	if w*h > 1048576 {
		t.Errorf("enforceMaxSize result %q has %d px, want <= 1048576", got, w*h)
	}
}

func TestSeedreamFormatImage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"http url", "http://example.com/a.png", "http://example.com/a.png"},
		{"https url", "https://example.com/a.png", "https://example.com/a.png"},
		{"already data uri", "data:image/png;base64,abc", "data:image/png;base64,abc"},
		{"too short bare string", "short", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seedreamFormatImage(tt.in); got != tt.want {
				t.Errorf("seedreamFormatImage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSeedreamFormatImage_BareBase64GetsDataURIPrefix(t *testing.T) {
	longBase64 := strings.Repeat("A", 100) // long enough to pass the len < 64 check
	got := seedreamFormatImage(longBase64)
	if !strings.HasPrefix(got, "data:image/") {
		t.Errorf("seedreamFormatImage(bare base64) = %q, want data: URI prefix", got)
	}
}

func TestSeedreamDetectMime(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{"/9j/4AAQ", "image/jpeg"},
		{"iVBORw0K", "image/png"},
		{"R0lGODlh", "image/gif"},
		{"UklGRgA", "image/webp"},
		{"unknownprefix", "image/jpeg"},
	}
	for _, tt := range tests {
		if got := seedreamDetectMime(tt.prefix); got != tt.want {
			t.Errorf("seedreamDetectMime(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

var _ AIProvider = (*DoubaoProvider)(nil)
