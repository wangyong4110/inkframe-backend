package deepseek

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDeepSeekProvider_Defaults(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	if p.endpoint != "https://api.deepseek.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", p.model)
	}
	if p.client.Timeout != ai.DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewDeepSeekProvider_CustomValues(t *testing.T) {
	p := NewDeepSeekProvider("key", "https://custom.example.com", "deepseek-reasoner", 10*time.Second)
	if p.endpoint != "https://custom.example.com" {
		t.Errorf("endpoint = %q, want custom", p.endpoint)
	}
	if p.model != "deepseek-reasoner" {
		t.Errorf("model = %q, want deepseek-reasoner", p.model)
	}
	if p.client.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", p.client.Timeout)
	}
}

func TestDeepSeekProvider_GetName(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	if p.GetName() != "deepseek" {
		t.Errorf("GetName() = %q, want deepseek", p.GetName())
	}
}

func TestDeepSeekProvider_GetModels(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	models := p.GetModels()
	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(models))
	}
	want := map[string]bool{"deepseek-chat": true, "deepseek-reasoner": true}
	for _, m := range models {
		if !want[m] {
			t.Errorf("unexpected model %q", m)
		}
	}
}

func TestDeepSeekProvider_HealthCheck(t *testing.T) {
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
				if r.URL.Path != "/models" {
					t.Errorf("path = %q, want /models", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer key" {
					t.Errorf("Authorization = %q, want Bearer key", got)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			p := NewDeepSeekProvider("key", server.URL, "", 0)
			err := p.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeepSeekProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("Authorization = %q, want Bearer key", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model": "deepseek-chat",
			"choices": [{"message": {"role":"assistant","content":"hello there"}, "finish_reason":"stop"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer server.Close()

	p := NewDeepSeekProvider("key", server.URL, "", 0)
	req := &ai.GenerateRequest{
		SystemPrompt: "you are helpful",
		Messages:     []ai.ChatMessage{{Role: "user", Content: "hi"}},
		Temperature:  0.5,
		MaxTokens:    100,
		TopP:         0.9,
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hello there" {
		t.Errorf("Content = %q, want %q", resp.Content, "hello there")
	}
	if resp.InputTokens != 10 || resp.Tokens != 5 {
		t.Errorf("tokens = (%d,%d), want (10,5)", resp.InputTokens, resp.Tokens)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", resp.StopReason)
	}

	msgs, _ := gotBody["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user)", len(msgs))
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "you are helpful" {
		t.Errorf("first message = %v, want system prompt", first)
	}
}

func TestDeepSeekProvider_Generate_ReasoningContentFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model": "deepseek-reasoner",
			"choices": [{"message": {"role":"assistant","content":"","reasoning_content":"thinking..."}, "finish_reason":"stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer server.Close()

	p := NewDeepSeekProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "thinking..." {
		t.Errorf("Content = %q, want fallback to reasoning_content", resp.Content)
	}
}

func TestDeepSeekProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer server.Close()

	p := NewDeepSeekProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if resp.Error == "" || !strings.Contains(resp.Error, "DeepSeek API") {
		t.Errorf("Error = %q, want DeepSeek API error message", resp.Error)
	}
}

func TestDeepSeekProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"deepseek-chat","choices":[]}`))
	}))
	defer server.Close()

	p := NewDeepSeekProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want %q", resp.Error, "no choices returned")
	}
}

func TestDeepSeekProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream flag = %v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewDeepSeekProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}

	var got strings.Builder
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				if got.String() != "hello" {
					t.Errorf("streamed content = %q, want hello", got.String())
				}
				return
			}
			if chunk.Error != "" {
				t.Fatalf("unexpected stream error: %s", chunk.Error)
			}
			got.WriteString(chunk.Content)
		case <-timeout:
			t.Fatal("timed out waiting for stream to close")
		}
	}
}

func TestDeepSeekProvider_Embed_NotSupported(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	_, err := p.Embed(context.Background(), "text")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDeepSeekProvider_ImageGenerate_NotSupported(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDeepSeekProvider_AudioGenerate_NotSupported(t *testing.T) {
	p := NewDeepSeekProvider("key", "", "", 0)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

var _ ai.AIProvider = (*DeepSeekProvider)(nil)
