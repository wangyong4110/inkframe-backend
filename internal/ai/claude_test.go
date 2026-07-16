package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── AnthropicProvider ──────────────────────────────────────────────────

func TestNewAnthropicProvider_Defaults(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	if p.endpoint != "https://api.anthropic.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "claude-3-opus-20240229" {
		t.Errorf("model = %q, want claude-3-opus-20240229", p.model)
	}
	if p.client.Timeout != DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewAnthropicProvider_CustomValues(t *testing.T) {
	p := NewAnthropicProvider("key", "https://custom", "claude-3-haiku-20240307", 20*time.Second)
	if p.endpoint != "https://custom" {
		t.Errorf("endpoint = %q, want custom", p.endpoint)
	}
	if p.model != "claude-3-haiku-20240307" {
		t.Errorf("model = %q", p.model)
	}
	if p.client.Timeout != 20*time.Second {
		t.Errorf("timeout = %v, want 20s", p.client.Timeout)
	}
}

func TestAnthropicProvider_GetName(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	if p.GetName() != "anthropic" {
		t.Errorf("GetName() = %q, want anthropic", p.GetName())
	}
}

func TestAnthropicProvider_GetModels(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	models := p.GetModels()
	if len(models) != 3 {
		t.Errorf("len(models) = %d, want 3", len(models))
	}
}

func TestAnthropicProvider_HealthCheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"ok", http.StatusOK, false},
		{"bad request tolerated", http.StatusBadRequest, false},
		{"server error", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("x-api-key"); got != "key" {
					t.Errorf("x-api-key = %q, want key", got)
				}
				if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
					t.Errorf("anthropic-version = %q", got)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			p := NewAnthropicProvider("key", server.URL, "", 0)
			err := p.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAnthropicProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model": "claude-3-opus-20240229",
			"content": [{"type":"text","text":"hi from claude"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 6}
		}`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("key", server.URL, "", 0)
	req := &GenerateRequest{
		SystemPrompt: "be nice",
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature:  0.5,
		MaxTokens:    500,
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if resp.Content != "hi from claude" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.InputTokens != 12 || resp.Tokens != 6 {
		t.Errorf("tokens = (%d,%d), want (12,6)", resp.InputTokens, resp.Tokens)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if gotBody["system"] != "be nice" {
		t.Errorf("system = %v, want be nice", gotBody["system"])
	}
	if gotBody["max_tokens"] != float64(500) {
		t.Errorf("max_tokens = %v, want 500", gotBody["max_tokens"])
	}
}

func TestAnthropicProvider_Generate_DefaultMaxTokensWhenZero(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("key", server.URL, "", 0)
	_, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotBody["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want default 1024", gotBody["max_tokens"])
	}
}

func TestAnthropicProvider_Generate_VisionMessage(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("key", server.URL, "", 0)
	req := &GenerateRequest{
		Messages: []ChatMessage{{Role: "user", Content: "describe", ImageURLs: []string{"https://example.com/a.png"}}},
	}
	_, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	msgs := gotBody["messages"].([]interface{})
	first := msgs[0].(map[string]interface{})
	blocks, ok := first["content"].([]interface{})
	if !ok || len(blocks) != 2 {
		t.Fatalf("content = %v, want 2 content blocks (image + text)", first["content"])
	}
}

func TestAnthropicProvider_GenerateStream_UsesConfiguredModel(t *testing.T) {
	var gotBody map[string]interface{}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message_stop"}`))
		close(done)
	}))
	defer server.Close()

	// p.model is the provider's fallback default; req.Model is what the caller explicitly
	// configured for this request and must win — GenerateStream must not ignore it like
	// Generate correctly doesn't.
	p := NewAnthropicProvider("key", server.URL, "claude-3-haiku-20240307", 0)
	req := &GenerateRequest{
		Model:    "claude-3-opus-20240229",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	}
	ch, err := p.GenerateStream(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	for range ch {
	}
	<-done
	if gotBody["model"] != "claude-3-opus-20240229" {
		t.Errorf("model = %v, want configured request model claude-3-opus-20240229", gotBody["model"])
	}
}

func TestAnthropicProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad request`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "Claude API error") {
		t.Errorf("Error = %q, want Claude API error prefix", resp.Error)
	}
}

func TestAnthropicProvider_Generate_NoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no content in response" {
		t.Errorf("Error = %q, want no content in response", resp.Error)
	}
}

func TestAnthropicProvider_Embed_NotSupported(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	if _, err := p.Embed(context.Background(), "text"); err == nil {
		t.Error("expected error, Anthropic has no embedding API")
	}
}

func TestAnthropicProvider_ImageGenerate_NotSupported(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestAnthropicProvider_AudioGenerate_NotSupported(t *testing.T) {
	p := NewAnthropicProvider("key", "", "", 0)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{})
	if err != nil {
		t.Fatalf("AudioGenerate() unexpected error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

var _ AIProvider = (*AnthropicProvider)(nil)

// ─── GoogleProvider ─────────────────────────────────────────────────────

func TestNewGoogleProvider_Defaults(t *testing.T) {
	p := NewGoogleProvider("key", "", "", 0)
	if p.endpoint != "https://generativelanguage.googleapis.com/v1beta" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "gemini-pro" {
		t.Errorf("model = %q, want gemini-pro", p.model)
	}
	if p.client.Timeout != DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewGoogleProvider_CustomValues(t *testing.T) {
	p := NewGoogleProvider("key", "https://custom", "gemini-ultra", 25*time.Second)
	if p.endpoint != "https://custom" || p.model != "gemini-ultra" {
		t.Errorf("provider = %+v, want custom endpoint/model", p)
	}
	if p.client.Timeout != 25*time.Second {
		t.Errorf("timeout = %v, want 25s", p.client.Timeout)
	}
}

func TestGoogleProvider_GetName(t *testing.T) {
	p := NewGoogleProvider("key", "", "", 0)
	if p.GetName() != "google" {
		t.Errorf("GetName() = %q, want google", p.GetName())
	}
}

func TestGoogleProvider_GetModels(t *testing.T) {
	p := NewGoogleProvider("key", "", "", 0)
	if len(p.GetModels()) != 3 {
		t.Errorf("len(models) = %d, want 3", len(p.GetModels()))
	}
}

func TestGoogleProvider_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "key=key") {
			t.Errorf("query = %q, want key param", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestGoogleProvider_HealthCheck_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error")
	}
}

func TestGoogleProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{"content": {"parts":[{"text":"hi from gemini"}], "role":"model"}, "finishReason":"STOP"}]
		}`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "gemini-pro", 0)
	req := &GenerateRequest{
		SystemPrompt: "sys",
		Messages: []ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
		Temperature: 0.5,
		MaxTokens:   100,
		TopP:        0.9,
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if !strings.Contains(gotPath, "/models/gemini-pro:generateContent") {
		t.Errorf("path = %q, want generateContent endpoint", gotPath)
	}
	if resp.Content != "hi from gemini" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.StopReason != "STOP" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}

	contents := gotBody["contents"].([]interface{})
	if len(contents) != 2 {
		t.Fatalf("contents len = %d, want 2", len(contents))
	}
	second := contents[1].(map[string]interface{})
	if second["role"] != "model" {
		t.Errorf("assistant role mapped = %v, want model", second["role"])
	}
	if gotBody["systemInstruction"] == nil {
		t.Error("expected systemInstruction to be set")
	}
}

func TestGoogleProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "Gemini API error") {
		t.Errorf("Error = %q, want Gemini API error prefix", resp.Error)
	}
}

func TestGoogleProvider_Generate_NoCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no content returned" {
		t.Errorf("Error = %q, want no content returned", resp.Error)
	}
}

func TestGoogleProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			t.Errorf("path = %q, want streamGenerateContent", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ab\"}]}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"cd\"}]}}]}\n\n"))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "abcd" {
		t.Errorf("stream content = %q, want abcd", got.String())
	}
}

func TestGoogleProvider_GenerateStream_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`server error`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	chunk, ok := <-ch
	if !ok || chunk.Error == "" {
		t.Fatal("expected an error chunk on the stream")
	}
}

func TestGoogleProvider_Embed(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2]}}`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "embedding-001", 0)
	vec, err := p.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vec = %v, want 2 elements", vec)
	}
	if !strings.Contains(gotPath, "models/embedding-001:embedContent") {
		t.Errorf("path = %q, want embedContent endpoint", gotPath)
	}
	if gotBody["model"] != "models/embedding-001" {
		t.Errorf("model = %v, want models/ prefix", gotBody["model"])
	}
}

func TestGoogleProvider_Embed_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewGoogleProvider("key", server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error")
	}
}

func TestGoogleProvider_ImageGenerate(t *testing.T) {
	p := NewGoogleProvider("key", "", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestGoogleProvider_AudioGenerate(t *testing.T) {
	p := NewGoogleProvider("key", "", "", 0)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{})
	if err != nil {
		t.Fatalf("AudioGenerate() unexpected error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

var _ AIProvider = (*GoogleProvider)(nil)
