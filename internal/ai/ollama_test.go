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

func TestNewOllamaProvider_Defaults(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	if p.endpoint != OllamaDefaultEndpoint {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "llama3.2" {
		t.Errorf("model = %q, want llama3.2", p.model)
	}
	if p.client.Timeout != OllamaDefaultTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewOllamaProvider_CustomValues(t *testing.T) {
	p := NewOllamaProvider("http://localhost:9999/v1/", "qwen2.5:7b", 10*time.Second)
	if p.endpoint != "http://localhost:9999/v1" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", p.endpoint)
	}
	if p.model != "qwen2.5:7b" {
		t.Errorf("model = %q", p.model)
	}
	if p.client.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", p.client.Timeout)
	}
}

func TestOllamaProvider_GetName(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	if p.GetName() != "ollama" {
		t.Errorf("GetName() = %q, want ollama", p.GetName())
	}
}

func TestOllamaProvider_GetModels_FromServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2"},{"name":"qwen2.5:7b"}]}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL+"/v1", "", 0)
	models := p.GetModels()
	if len(models) != 2 {
		t.Fatalf("models = %v, want 2 entries", models)
	}
}

func TestOllamaProvider_GetModels_FallbackOnError(t *testing.T) {
	p := NewOllamaProvider("http://127.0.0.1:1/v1", "fallback-model", 0)
	models := p.GetModels()
	if len(models) != 1 || models[0] != "fallback-model" {
		t.Errorf("models = %v, want [fallback-model] on unreachable server", models)
	}
}

func TestOllamaProvider_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL+"/v1", "", 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestOllamaProvider_HealthCheck_Unreachable(t *testing.T) {
	p := NewOllamaProvider("http://127.0.0.1:1/v1", "", 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestOllamaProvider_HealthCheck_BadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL+"/v1", "", 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error for 500 status")
	}
}

func TestOllamaProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"llama3.2",
			"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}
		}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	req := &GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature:  0.4,
		MaxTokens:    50,
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	if resp.InputTokens != 5 {
		t.Errorf("InputTokens = %d, want 5", resp.InputTokens)
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false", gotBody["stream"])
	}
	msgs := gotBody["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (system+user)", len(msgs))
	}
}

func TestOllamaProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad model`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "ollama error") {
		t.Errorf("Error = %q, want ollama error prefix", resp.Error)
	}
}

func TestOllamaProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestOllamaProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "ok" {
		t.Errorf("stream content = %q, want ok", got.String())
	}
}

func TestOllamaProvider_Embed(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "nomic-embed-text", 0)
	vec, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vec = %v, want 2 elements", vec)
	}
	if gotBody["model"] != "nomic-embed-text" {
		t.Errorf("model = %v, want nomic-embed-text", gotBody["model"])
	}
}

func TestOllamaProvider_Embed_UsesConfiguredModel(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	}))
	defer server.Close()

	// p.model is whatever the user explicitly configured in Model Management — must be used
	// as-is, even if it doesn't look like an embedding model name. Never silently swapped.
	p := NewOllamaProvider(server.URL, "llama3.2", 0)
	_, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if gotBody["model"] != "llama3.2" {
		t.Errorf("model = %v, want configured model llama3.2 (must not be silently swapped)", gotBody["model"])
	}
}

func TestOllamaProvider_Embed_DefaultModelWhenEmpty(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	p.model = "" // simulate nothing configured (constructor defaults empty to llama3.2)
	_, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if gotBody["model"] != "nomic-embed-text" {
		t.Errorf("model = %v, want fallback nomic-embed-text when nothing configured", gotBody["model"])
	}
}

func TestOllamaProvider_Embed_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error")
	}
}

func TestOllamaProvider_Embed_NoData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error for empty embedding data")
	}
}

func TestOllamaProvider_ImageGenerate_NotSupported(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ImageGenerateRequest{})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestOllamaProvider_AudioGenerate_NotSupported(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	if _, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{}); err == nil {
		t.Error("expected error")
	}
}

func TestOllamaProvider_BuildMessages(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	req := &GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
	}
	msgs := p.buildMessages(req)
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (system+user+assistant)", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "sys" {
		t.Errorf("first message = %v, want system prompt", msgs[0])
	}
}

func TestOllamaProvider_BuildMessages_NoSystemPrompt(t *testing.T) {
	p := NewOllamaProvider("", "", 0)
	req := &GenerateRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	msgs := p.buildMessages(req)
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1 (no system prompt)", len(msgs))
	}
}

var _ AIProvider = (*OllamaProvider)(nil)
