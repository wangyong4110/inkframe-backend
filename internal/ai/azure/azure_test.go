package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func TestNewAzureProvider_Defaults(t *testing.T) {
	p := NewAzureProvider(" key ", " https://foo.openai.azure.com/openai ", "gpt-4.1", "", 0)
	if p.apiKey != "key" {
		t.Errorf("apiKey = %q, want trimmed key", p.apiKey)
	}
	if p.endpoint != "https://foo.openai.azure.com/openai" {
		t.Errorf("endpoint = %q, want trimmed endpoint", p.endpoint)
	}
	if p.apiVersion != "2025-01-01-preview" {
		t.Errorf("apiVersion = %q, want default", p.apiVersion)
	}
	if p.client.Timeout != ai.DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewAzureProvider_EmptyEndpointFallback(t *testing.T) {
	p := NewAzureProvider("key", "", "dep", "", 0)
	if p.endpoint != "https://YOUR-RESOURCE.openai.azure.com/openai" {
		t.Errorf("endpoint = %q, want placeholder default", p.endpoint)
	}
}

func TestNewAzureProvider_ParsesFullChatCompletionsURL(t *testing.T) {
	p := NewAzureProvider("key", "https://foo.openai.azure.com/openai/deployments/gpt-4.1/chat/completions", "", "2024-05-01", 30*time.Second)
	if p.endpoint != "https://foo.openai.azure.com/openai" {
		t.Errorf("endpoint = %q, want base endpoint stripped of /deployments/...", p.endpoint)
	}
	if p.defaultDeployment != "gpt-4.1" {
		t.Errorf("defaultDeployment = %q, want gpt-4.1 (parsed from URL)", p.defaultDeployment)
	}
	if p.apiVersion != "2024-05-01" {
		t.Errorf("apiVersion = %q, want 2024-05-01", p.apiVersion)
	}
	if p.client.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", p.client.Timeout)
	}
}

func TestNewAzureProvider_ExplicitDeploymentTakesPriorityOverURL(t *testing.T) {
	p := NewAzureProvider("key", "https://foo.openai.azure.com/openai/deployments/from-url/chat/completions", "explicit-dep", "", 0)
	if p.defaultDeployment != "explicit-dep" {
		t.Errorf("defaultDeployment = %q, want explicit-dep (explicit arg wins)", p.defaultDeployment)
	}
}

func TestAzureProvider_GetName(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	if p.GetName() != "azure" {
		t.Errorf("GetName() = %q, want azure", p.GetName())
	}
}

func TestAzureProvider_GetModels(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "gpt-4.1", "", 0)
	models := p.GetModels()
	if len(models) != 1 || models[0] != "gpt-4.1" {
		t.Errorf("GetModels() = %v, want [gpt-4.1]", models)
	}

	p2 := NewAzureProvider("key", "https://foo", "", "", 0)
	if models2 := p2.GetModels(); models2 != nil {
		t.Errorf("GetModels() = %v, want nil when no default deployment", models2)
	}
}

func TestAzureProvider_DeploymentOf(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "default-dep", "", 0)

	dep, err := p.deploymentOf("")
	if err != nil || dep != "default-dep" {
		t.Errorf("deploymentOf(\"\") = (%q, %v), want (default-dep, nil)", dep, err)
	}

	dep2, err := p.deploymentOf("override-dep")
	if err != nil || dep2 != "override-dep" {
		t.Errorf("deploymentOf(override) = (%q, %v), want (override-dep, nil)", dep2, err)
	}

	p2 := NewAzureProvider("key", "https://foo", "", "", 0)
	if _, err := p2.deploymentOf(""); err == nil {
		t.Error("expected error when no deployment available")
	}
}

func TestAzureProvider_ChatURL(t *testing.T) {
	p := NewAzureProvider("key", "https://foo.openai.azure.com/openai", "dep", "2025-01-01-preview", 0)
	url := p.chatURL("mydeployment", "")
	want := "https://foo.openai.azure.com/openai/deployments/mydeployment/chat/completions?api-version=2025-01-01-preview"
	if url != want {
		t.Errorf("chatURL() = %q, want %q", url, want)
	}

	url2 := p.chatURL("mydeployment", "2024-02-01")
	want2 := "https://foo.openai.azure.com/openai/deployments/mydeployment/chat/completions?api-version=2024-02-01"
	if url2 != want2 {
		t.Errorf("chatURL() with override = %q, want %q", url2, want2)
	}
}

func TestAzureProvider_HealthCheck_NoDefaultDeployment(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"ok", http.StatusOK, false},
		{"not found tolerated", http.StatusNotFound, false},
		{"unauthorized", http.StatusUnauthorized, true},
		{"server error", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/deployments" {
					t.Errorf("path = %q, want /deployments", r.URL.Path)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			p := NewAzureProvider("key", server.URL, "", "", 0)
			err := p.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAzureProvider_HealthCheck_WithDefaultDeployment(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"ok", http.StatusOK, false},
		{"not found", http.StatusNotFound, true},
		{"unauthorized", http.StatusUnauthorized, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/deployments/mydep") {
					t.Errorf("path = %q, want deployment-specific path", r.URL.Path)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			p := NewAzureProvider("key", server.URL, "mydep", "", 0)
			err := p.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAzureProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotKey = r.Header.Get("api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":2}
		}`))
	}))
	defer server.Close()

	p := NewAzureProvider("secretkey", server.URL, "mydep", "", 0)
	req := &ai.GenerateRequest{
		Messages:    []ai.ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens:   200,
		Temperature: 0.4,
		TopP:        0.8,
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("Content = %q, want hello", resp.Content)
	}
	if resp.Model != "mydep" {
		t.Errorf("Model = %q, want deployment name mydep", resp.Model)
	}
	if resp.InputTokens != 4 || resp.Tokens != 2 {
		t.Errorf("tokens = (%d,%d), want (4,2)", resp.InputTokens, resp.Tokens)
	}
	if gotKey != "secretkey" {
		t.Errorf("api-key header = %q, want secretkey", gotKey)
	}
	if !strings.Contains(gotPath, "/deployments/mydep/chat/completions") {
		t.Errorf("path = %q, want deployment path", gotPath)
	}
	if gotBody["max_tokens"] != float64(200) {
		t.Errorf("max_tokens = %v, want 200", gotBody["max_tokens"])
	}
}

func TestAzureProvider_Generate_NoDeploymentError(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "", "", 0)
	_, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error when no deployment configured")
	}
}

func TestAzureProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`rate limited`))
	}))
	defer server.Close()

	p := NewAzureProvider("key", server.URL, "dep", "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "429") {
		t.Errorf("Error = %q, want mention of 429", resp.Error)
	}
}

func TestAzureProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewAzureProvider("key", server.URL, "dep", "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestAzureProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"xy\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewAzureProvider("key", server.URL, "dep", "", 0)
	ch, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "xy" {
		t.Errorf("stream content = %q, want xy", got.String())
	}
}

func TestAzureProvider_GenerateStream_NoDeploymentError(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "", "", 0)
	_, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error when no deployment configured")
	}
}

func TestAzureProvider_Embed_NotImplemented(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	if _, err := p.Embed(context.Background(), "text"); err == nil {
		t.Error("expected not-implemented error")
	}
}

func TestAzureProvider_ImageGenerate_NotImplemented(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	if _, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{}); err == nil {
		t.Error("expected not-implemented error")
	}
}

func TestAzureProvider_AudioGenerate_NotImplemented(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	if _, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{}); err == nil {
		t.Error("expected not-implemented error")
	}
}

func TestAzureProvider_BuildChatRequest(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	req := &ai.GenerateRequest{
		Messages:    []ai.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
		MaxTokens:   10,
		Temperature: 0.6,
		TopP:        0.7,
	}
	payload := p.buildChatRequest(req)
	msgs, ok := payload["messages"].([]map[string]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 entries", payload["messages"])
	}
	if payload["max_tokens"] != 10 {
		t.Errorf("max_tokens = %v, want 10", payload["max_tokens"])
	}
	if payload["temperature"] != 0.6 {
		t.Errorf("temperature = %v, want 0.6", payload["temperature"])
	}
	if payload["top_p"] != 0.7 {
		t.Errorf("top_p = %v, want 0.7", payload["top_p"])
	}
}

func TestAzureProvider_BuildChatRequest_OmitsZeroValues(t *testing.T) {
	p := NewAzureProvider("key", "https://foo", "dep", "", 0)
	req := &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}}
	payload := p.buildChatRequest(req)
	if _, ok := payload["max_tokens"]; ok {
		t.Error("max_tokens should be omitted when zero")
	}
	if _, ok := payload["temperature"]; ok {
		t.Error("temperature should be omitted when zero")
	}
	if _, ok := payload["top_p"]; ok {
		t.Error("top_p should be omitted when zero")
	}
}

var _ ai.AIProvider = (*AzureProvider)(nil)
