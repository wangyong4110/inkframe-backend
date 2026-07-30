package openai

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewOpenAIProvider_Defaults(t *testing.T) {
	p := NewOpenAIProvider("key", "", "", 0)
	if p.endpoint != "https://api.openai.com/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "gpt-4" {
		t.Errorf("model = %q, want gpt-4", p.model)
	}
	if p.client.Timeout != ai.DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewOpenAIProvider_CustomValues(t *testing.T) {
	p := NewOpenAIProvider("key", "https://custom", "gpt-4-turbo", 15*time.Second)
	if p.endpoint != "https://custom" {
		t.Errorf("endpoint = %q, want custom", p.endpoint)
	}
	if p.model != "gpt-4-turbo" {
		t.Errorf("model = %q, want gpt-4-turbo", p.model)
	}
	if p.client.Timeout != 15*time.Second {
		t.Errorf("timeout = %v, want 15s", p.client.Timeout)
	}
}

func TestOpenAIProvider_GetName(t *testing.T) {
	p := NewOpenAIProvider("key", "", "", 0)
	if p.GetName() != "openai" {
		t.Errorf("GetName() = %q, want openai", p.GetName())
	}
}

func TestOpenAIProvider_GetModels(t *testing.T) {
	p := NewOpenAIProvider("key", "", "", 0)
	models := p.GetModels()
	if len(models) == 0 {
		t.Fatal("expected non-empty models list")
	}
	found := false
	for _, m := range models {
		if m == "gpt-4" {
			found = true
		}
	}
	if !found {
		t.Error("expected gpt-4 in models list")
	}
}

func TestOpenAIProvider_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestOpenAIProvider_HealthCheck_Failure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestOpenAIProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model": "gpt-4",
			"choices": [{"message": {"role":"assistant","content":"hi there"}, "finish_reason":"stop"}],
			"usage": {"prompt_tokens": 7, "completion_tokens": 3}
		}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	req := &ai.GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ai.ChatMessage{{Role: "user", Content: "hello"}},
		Temperature:  0.5,
		MaxTokens:    100,
		TopP:         0.9,
		TopK:         50,
		Stop:         []string{"\n"},
	}
	resp, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.InputTokens != 7 || resp.Tokens != 3 {
		t.Errorf("tokens = (%d,%d), want (7,3)", resp.InputTokens, resp.Tokens)
	}
	if gotBody["presence_penalty"] != 0.5 {
		t.Errorf("presence_penalty (from TopK/100) = %v, want 0.5", gotBody["presence_penalty"])
	}
	if stop, ok := gotBody["stop"].([]interface{}); !ok || len(stop) != 1 {
		t.Errorf("stop = %v, want [\\n]", gotBody["stop"])
	}
}

func TestOpenAIProvider_Generate_CompletionsEndpointForDavinci(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	_, err := p.Generate(context.Background(), &ai.GenerateRequest{Model: "text-davinci-003", Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotPath != "/completions" {
		t.Errorf("path = %q, want /completions for davinci model", gotPath)
	}
}

func TestOpenAIProvider_Generate_VisionMessageUsesArrayContent(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "gpt-3.5-turbo", 0)
	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{{Role: "user", Content: "describe", ImageURLs: []string{"https://example.com/img.png"}}},
	}
	_, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	// Vision messages auto-upgrade to a vision-capable model when the configured model
	// isn't already one of gpt-4o/gpt-4-vision-preview/gpt-4-turbo.
	if gotBody["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o (auto-upgraded for vision request)", gotBody["model"])
	}
	msgs := gotBody["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	parts, ok := last["content"].([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %v, want array of 2 parts (image_url + text)", last["content"])
	}
}

func TestOpenAIProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	_, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want mention of status 400", err)
	}
}

func TestOpenAIProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestOpenAIProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"foo\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "foo" {
		t.Errorf("stream content = %q, want foo", got.String())
	}
}

func TestOpenAIProvider_Embed(t *testing.T) {
	var gotBody map[string]interface{}
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "text-embedding-3-small", 0)
	vec, err := p.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if gotPath != "/embeddings" {
		t.Errorf("path = %q, want /embeddings", gotPath)
	}
	if gotBody["model"] != "text-embedding-3-small" {
		t.Errorf("model = %v, want configured embedding model", gotBody["model"])
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vec = %v, want [0.1 0.2 0.3]", vec)
	}
}

func TestOpenAIProvider_Embed_DefaultModelWhenEmpty(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5]}]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	p.model = "" // simulate empty configured model
	_, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if gotBody["model"] != "text-embedding-ada-002" {
		t.Errorf("model = %v, want fallback text-embedding-ada-002", gotBody["model"])
	}
}

func TestOpenAIProvider_Embed_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	_, err := p.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenAIProvider_Embed_NoData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	_, err := p.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error for empty embedding data")
	}
}

func TestOpenAIProvider_ImageGenerate_DallE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %q, want /images/generations", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/img.png"}]}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "dall-e-3", Prompt: "a cat", Size: "1024x1024"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.URL != "https://example.com/img.png" {
		t.Errorf("URL = %q", resp.URL)
	}
}

func TestOpenAIProvider_ImageGenerate_DallEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`content policy violation`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "dall-e-3", Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected transport error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestOpenAIProvider_ImageGenerate_NonDallEModel(t *testing.T) {
	p := NewOpenAIProvider("key", "http://unused.invalid", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "stable-diffusion-xl", Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected SD-not-implemented error message")
	}
}

func TestOpenAIProvider_AudioGenerate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("path = %q, want /audio/speech", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-mp3-bytes"))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello", Voice: "alloy", Speed: 1.0})
	if err != nil {
		t.Fatalf("AudioGenerate() error: %v", err)
	}
	if !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("URL = %q, want file:// prefix", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	// cleanup temp file
	_ = os.Remove(strings.TrimPrefix(resp.URL, "file://"))
}

func TestOpenAIProvider_AudioGenerate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "", 0)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hi", Voice: "alloy"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewSSEReader_ParsesDataLines(t *testing.T) {
	r := ai.NewSSEReader(strings.NewReader("data: {\"a\":1}\nignored: line\ndata: {\"b\":2}\n"))
	ev, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if ev.Data != `{"a":1}` {
		t.Errorf("Data = %q, want {\"a\":1}", ev.Data)
	}
	ev2, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if ev2.Data != `{"b":2}` {
		t.Errorf("Data = %q, want {\"b\":2}", ev2.Data)
	}
}

func TestNewSSEReader_DoneSentinel(t *testing.T) {
	r := ai.NewSSEReader(strings.NewReader("data: [DONE]\n"))
	_, err := r.Read()
	if err == nil {
		t.Fatal("expected io.EOF for [DONE] sentinel")
	}
}

var _ ai.TextProvider = (*OpenAIProvider)(nil)
