package openai

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

func TestNewOpenAICompatProvider_Defaults(t *testing.T) {
	p := NewOpenAICompatProvider("myprovider", "key", "https://example.com", "model-a", []string{"model-a", "model-b"}, 0)
	if p.GetName() != "myprovider" {
		t.Errorf("GetName() = %q, want myprovider", p.GetName())
	}
	if p.client.Timeout != ai.DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
	models := p.GetModels()
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Errorf("GetModels() = %v, want [model-a model-b]", models)
	}
}

func TestNewOpenAICompatProvider_CustomTimeout(t *testing.T) {
	p := NewOpenAICompatProvider("p", "key", "https://example.com", "m", nil, 5*time.Second)
	if p.client.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", p.client.Timeout)
	}
}

func TestOpenAICompatProvider_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "m", nil, 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestOpenAICompatProvider_HealthCheck_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "m", nil, 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestOpenAICompatProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "default-model", nil, 0)
	req := &ai.GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ai.ChatMessage{{Role: "user", Content: "hello"}},
		Temperature:  0.3,
		MaxTokens:    50,
		TopP:         0.8,
		Extra:        map[string]interface{}{"thinking": map[string]string{"type": "enabled"}},
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	if resp.InputTokens != 3 || resp.Tokens != 2 {
		t.Errorf("tokens = (%d,%d), want (3,2)", resp.InputTokens, resp.Tokens)
	}

	if gotBody["model"] != "default-model" {
		t.Errorf("model = %v, want default-model (req.Model empty)", gotBody["model"])
	}
	if gotBody["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want 0.3", gotBody["temperature"])
	}
	if gotBody["thinking"] == nil {
		t.Error("expected Extra field 'thinking' to be passed through")
	}
	msgs := gotBody["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
}

func TestOpenAICompatProvider_Generate_UsesRequestModel(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "default-model", nil, 0)
	_, err := p.Generate(context.Background(), &ai.GenerateRequest{Model: "override-model", Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotBody["model"] != "override-model" {
		t.Errorf("model = %v, want override-model", gotBody["model"])
	}
}

func TestOpenAICompatProvider_Generate_ReasoningContentFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"thinking"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "m", nil, 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "thinking" {
		t.Errorf("Content = %q, want fallback to reasoning_content", resp.Content)
	}
}

func TestOpenAICompatProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad request`))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("myname", "key", server.URL, "m", nil, 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport err: %v", err)
	}
	if !strings.Contains(resp.Error, "myname") {
		t.Errorf("Error = %q, want to contain provider name", resp.Error)
	}
}

func TestOpenAICompatProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "m", nil, 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestOpenAICompatProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ab\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAICompatProvider("p", "key", server.URL, "m", nil, 0)
	ch, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "ab" {
		t.Errorf("stream content = %q, want ab", got.String())
	}
}

// ─── Concrete constructors ──────────────────────────────────────────────

func TestNewXAIProvider(t *testing.T) {
	p := NewXAIProvider("key", "", "", 0)
	if p.GetName() != "xai" {
		t.Errorf("GetName() = %q, want xai", p.GetName())
	}
	if p.endpoint != "https://api.x.ai/v1" {
		t.Errorf("endpoint = %q, want default xai endpoint", p.endpoint)
	}
	if p.model != "grok-3-mini" {
		t.Errorf("model = %q, want grok-3-mini", p.model)
	}
}

func TestNewXAIProvider_CustomEndpointModel(t *testing.T) {
	p := NewXAIProvider("key", "https://custom", "grok-4", 0)
	if p.endpoint != "https://custom" {
		t.Errorf("endpoint = %q, want custom", p.endpoint)
	}
	if p.model != "grok-4" {
		t.Errorf("model = %q, want grok-4", p.model)
	}
}

func TestNewMistralProvider(t *testing.T) {
	p := NewMistralProvider("key", "", "", 0)
	if p.GetName() != "mistral" {
		t.Errorf("GetName() = %q, want mistral", p.GetName())
	}
	if p.endpoint != "https://api.mistral.ai/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "mistral-large-latest" {
		t.Errorf("model = %q, want mistral-large-latest", p.model)
	}
}

func TestNewMetaProvider(t *testing.T) {
	p := NewMetaProvider("key", "", "", 0)
	if p.GetName() != "meta" {
		t.Errorf("GetName() = %q, want meta", p.GetName())
	}
	if p.endpoint != "https://api.llama.com/compat/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
}

func TestNewZhipuProvider(t *testing.T) {
	p := NewZhipuProvider("key", "", "", 0)
	if p.GetName() != "zhipu" {
		t.Errorf("GetName() = %q, want zhipu", p.GetName())
	}
	if p.endpoint != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "glm-4-plus" {
		t.Errorf("model = %q, want glm-4-plus", p.model)
	}
}

func TestNewMoonshotProvider(t *testing.T) {
	p := NewMoonshotProvider("key", "", "", 0)
	if p.GetName() != "moonshot" {
		t.Errorf("GetName() = %q, want moonshot", p.GetName())
	}
	if p.endpoint != "https://api.moonshot.cn/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
}

func TestNewBaiduProvider(t *testing.T) {
	p := NewBaiduProvider("key", "", "", 0)
	if p.GetName() != "baidu" {
		t.Errorf("GetName() = %q, want baidu", p.GetName())
	}
	if p.endpoint != "https://qianfan.baidubce.com/v2" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
}

func TestNewTencentProvider(t *testing.T) {
	p := NewTencentProvider("key", "", "", 0)
	if p.GetName() != "tencent" {
		t.Errorf("GetName() = %q, want tencent", p.GetName())
	}
	if p.endpoint != "https://api.hunyuan.cloud.tencent.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
}

func TestNewYiProvider(t *testing.T) {
	p := NewYiProvider("key", "", "", 0)
	if p.GetName() != "yi" {
		t.Errorf("GetName() = %q, want yi", p.GetName())
	}
	if p.endpoint != "https://api.lingyiwanwu.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
}

func TestNewHunyuanProvider(t *testing.T) {
	p := NewHunyuanProvider("key", "", "", 0)
	if p.GetName() != "hunyuan" {
		t.Errorf("GetName() = %q, want hunyuan", p.GetName())
	}
	if p.endpoint != "https://tokenhub.tencentmaas.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "hy3-preview" {
		t.Errorf("model = %q, want hy3-preview", p.model)
	}
}

var _ ai.TextProvider = (*OpenAICompatProvider)(nil)
