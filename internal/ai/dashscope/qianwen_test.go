package dashscope

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

func TestNewQianwenProvider_Defaults(t *testing.T) {
	p := NewQianwenProvider("key", "", "", 0)
	if p.endpoint != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.model != "qwen-plus" {
		t.Errorf("model = %q, want qwen-plus", p.model)
	}
	if p.client.Timeout != ai.DefaultProviderTimeout {
		t.Errorf("timeout = %v, want default", p.client.Timeout)
	}
}

func TestNewQianwenProvider_CustomValues(t *testing.T) {
	p := NewQianwenProvider("key", "https://custom", "qwen-max", 5*time.Second)
	if p.endpoint != "https://custom" || p.model != "qwen-max" {
		t.Errorf("provider = %+v", p)
	}
	if p.client.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", p.client.Timeout)
	}
}

func TestQianwenProvider_GetName(t *testing.T) {
	p := NewQianwenProvider("key", "", "", 0)
	if p.GetName() != "qianwen" {
		t.Errorf("GetName() = %q, want qianwen", p.GetName())
	}
}

func TestQianwenProvider_GetModels(t *testing.T) {
	p := NewQianwenProvider("key", "", "", 0)
	if len(p.GetModels()) == 0 {
		t.Error("expected non-empty models list")
	}
}

func TestQianwenProvider_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestQianwenProvider_HealthCheck_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("expected error")
	}
}

func TestQianwenProvider_Generate(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"qwen-plus",
			"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2}
		}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{
		SystemPrompt: "sys",
		Messages:     []ai.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q", resp.Content)
	}
	if _, ok := gotBody["enable_thinking"]; ok {
		t.Error("enable_thinking should not be set for non-qwen3 models")
	}
}

func TestQianwenProvider_Generate_Qwen3DisablesThinking(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	_, err := p.Generate(context.Background(), &ai.GenerateRequest{Model: "qwen3-32b", Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if gotBody["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false for qwen3 models", gotBody["enable_thinking"])
	}
}

func TestQianwenProvider_Generate_ReasoningContentFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"thinking"}}]}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Content != "thinking" {
		t.Errorf("Content = %q, want fallback to reasoning_content", resp.Content)
	}
}

func TestQianwenProvider_Generate_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() unexpected transport error: %v", err)
	}
	if !strings.Contains(resp.Error, "千问 API 错误") {
		t.Errorf("Error = %q, want 千问 API 错误 prefix", resp.Error)
	}
}

func TestQianwenProvider_Generate_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.Generate(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Error != "no choices returned" {
		t.Errorf("Error = %q, want no choices returned", resp.Error)
	}
}

func TestQianwenProvider_GenerateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	ch, err := p.GenerateStream(context.Background(), &ai.GenerateRequest{Messages: []ai.ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	if got.String() != "hi" {
		t.Errorf("stream content = %q, want hi", got.String())
	}
}

func TestQianwenProvider_Embed(t *testing.T) {
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

	p := NewQianwenProvider("key", server.URL, "text-embedding-v3", 0)
	vec, err := p.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vec = %v, want 2 elements", vec)
	}
	if gotBody["model"] != "text-embedding-v3" {
		t.Errorf("model = %v, want text-embedding-v3", gotBody["model"])
	}
}

func TestQianwenProvider_Embed_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error")
	}
}

func TestQianwenProvider_Embed_NoData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	if _, err := p.Embed(context.Background(), "hi"); err == nil {
		t.Error("expected error for empty embedding data")
	}
}

func TestQianwenProvider_DashscopeBaseURL(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
	}{
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com"},
		{"https://dashscope.aliyuncs.com/compatible-mode", "https://dashscope.aliyuncs.com"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1/", "https://dashscope.aliyuncs.com"},
		{"https://custom.example.com", "https://custom.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			p := NewQianwenProvider("key", tt.endpoint, "", 0)
			if got := p.dashscopeBaseURL(); got != tt.want {
				t.Errorf("dashscopeBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQianwenProvider_ImageGenerate_CompatSyncPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %q, want /images/generations (wanx-v1 compat path)", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/img.png"}]}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "wanx-v1", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.URL != "https://example.com/img.png" {
		t.Errorf("URL = %q", resp.URL)
	}
}

func TestQianwenProvider_ImageGenerate_CompatError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad request`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "wanx-v1", Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() unexpected transport error: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestQianwenProvider_ImageGenerate_AsyncSubmitAndPoll(t *testing.T) {
	var pollCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v1/services/aigc/text2image/image-synthesis"):
			if got := r.Header.Get("X-DashScope-Async"); got != "enable" {
				t.Errorf("X-DashScope-Async = %q, want enable", got)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"output":{"task_id":"task-123","task_status":"PENDING"}}`))
		case strings.Contains(r.URL.Path, "/api/v1/tasks/task-123"):
			pollCount++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"output":{"task_status":"SUCCEEDED","results":[{"url":"https://example.com/wanx.png"}]}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL+"/compatible-mode/v1", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "wanx2.1-t2i-turbo", Prompt: "a dog", Size: "1024x1024"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if resp.URL != "https://example.com/wanx.png" {
		t.Errorf("URL = %q, want wanx result URL", resp.URL)
	}
	if pollCount == 0 {
		t.Error("expected at least one poll request")
	}
}

func TestQianwenProvider_ImageGenerate_AsyncFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "image-synthesis"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"output":{"task_id":"task-err","task_status":"PENDING"}}`))
		case strings.Contains(r.URL.Path, "/tasks/task-err"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"output":{"task_status":"FAILED","results":[{"code":"DataInspectionFailed","message":"content blocked"}]}}`))
		}
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL+"/compatible-mode/v1", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "wanx2.1-t2i-turbo", Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if !strings.Contains(resp.Error, "task-err") {
		t.Errorf("Error = %q, want mention of task ID", resp.Error)
	}
}

func TestQianwenProvider_ImageGenerate_AsyncSubmitError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"bad size"}`))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL+"/compatible-mode/v1", "", 0)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Model: "wanx2.1-t2i-turbo", Prompt: "x"})
	if err != nil {
		t.Fatalf("ImageGenerate() error: %v", err)
	}
	if !strings.Contains(resp.Error, "InvalidParameter") {
		t.Errorf("Error = %q, want mention of submit error code", resp.Error)
	}
}

func TestQianwenProvider_AudioGenerate_CosyVoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("path = %q, want /audio/speech", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-mp3-data"))
	}))
	defer server.Close()

	p := NewQianwenProvider("key", server.URL, "cosyvoice-v1", 0)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello", Voice: "longxiaochun"})
	if err != nil {
		t.Fatalf("AudioGenerate() error: %v", err)
	}
	if !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("URL = %q, want file:// prefix", resp.URL)
	}
	_ = os.Remove(strings.TrimPrefix(resp.URL, "file://"))
}

func TestQianwenProvider_AudioGenerate_ExplicitModelNeverOverridden(t *testing.T) {
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-mp3-data"))
	}))
	defer server.Close()

	// req.Model is explicitly configured by the user and doesn't match the tts/cosyvoice
	// naming heuristic — it must still be sent as-is, never silently swapped to cosyvoice-v1.
	p := NewQianwenProvider("key", server.URL, "qwen-plus", 0)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text: "hello", Voice: "longxiaochun", Model: "future-custom-voice-model",
	})
	if err != nil {
		t.Fatalf("AudioGenerate() error: %v", err)
	}
	_ = os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	if gotBody["model"] != "future-custom-voice-model" {
		t.Errorf("model = %v, want configured request model future-custom-voice-model", gotBody["model"])
	}
}

func TestQianwenProvider_AudioGenerate_NonTTSProviderModelErrors(t *testing.T) {
	// p.model defaults to "qwen-plus" (a text-generation model) when nothing TTS-specific is
	// configured. This must error, not silently substitute cosyvoice-v1 — never guess a model
	// the user didn't configure.
	p := NewQianwenProvider("key", "http://unused.invalid", "", 0)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello", Voice: "longxiaochun"})
	if err == nil {
		t.Fatal("expected error when provider's configured model is not a TTS model")
	}
	if !strings.Contains(err.Error(), "不是语音合成模型") {
		t.Errorf("error = %q, want mention of 不是语音合成模型", err.Error())
	}
}

func TestQianwenProvider_AudioGenerate_NoModelConfiguredErrors(t *testing.T) {
	p := NewQianwenProvider("key", "http://unused.invalid", "", 0)
	p.model = "" // simulate nothing configured at all
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello", Voice: "longxiaochun"})
	if err == nil {
		t.Fatal("expected error when no model is configured")
	}
	if !strings.Contains(err.Error(), "未配置语音模型") {
		t.Errorf("error = %q, want mention of 未配置语音模型", err.Error())
	}
}

// Note: generateQwenTTS (qwen-tts/qwen3-tts models) calls hardcoded DashScope endpoints
// rather than p.endpoint, so it cannot be exercised against an httptest server without a
// real DashScope credential. AudioGenerate's model-routing logic (tts vs cosyvoice) is
// covered indirectly by TestQianwenProvider_AudioGenerate_CosyVoice above and by reading
// the routing condition in AudioGenerate directly.

func TestQianwenProvider_SaveTTSToTemp(t *testing.T) {
	resp, err := saveTTSToTemp([]byte("data"), "some text", time.Now())
	if err != nil {
		t.Fatalf("saveTTSToTemp() error: %v", err)
	}
	if !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("URL = %q, want file:// prefix", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	_ = os.Remove(strings.TrimPrefix(resp.URL, "file://"))
}

var _ ai.AIProvider = (*QianwenProvider)(nil)
