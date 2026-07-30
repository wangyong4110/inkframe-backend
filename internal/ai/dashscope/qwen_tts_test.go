package dashscope

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

func TestNewQwenTTSProvider(t *testing.T) {
	t.Run("default endpoint when empty", func(t *testing.T) {
		p := NewQwenTTSProvider("key", "")
		if p.apiKey != "key" {
			t.Errorf("apiKey = %q, want key", p.apiKey)
		}
		if p.endpoint != "https://dashscope.aliyuncs.com" {
			t.Errorf("endpoint = %q, want default", p.endpoint)
		}
	})

	t.Run("custom endpoint preserved", func(t *testing.T) {
		p := NewQwenTTSProvider("key", "https://custom.example.com")
		if p.endpoint != "https://custom.example.com" {
			t.Errorf("endpoint = %q, want custom", p.endpoint)
		}
	})
}

func TestQwenTTSProvider_GetName(t *testing.T) {
	p := NewQwenTTSProvider("key", "")
	if got := p.GetName(); got != "qwen-tts" {
		t.Errorf("GetName() = %q, want qwen-tts", got)
	}
}

func TestQwenTTSProvider_GetModels(t *testing.T) {
	p := NewQwenTTSProvider("key", "")
	models := p.GetModels()
	want := []string{"qwen3-tts-flash", "qwen3-tts-instruct-flash", "qwen-tts"}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d", len(models), len(want))
	}
	for i, w := range want {
		if models[i] != w {
			t.Errorf("models[%d] = %q, want %q", i, models[i], w)
		}
	}
}

func TestQwenTTSProvider_HealthCheck(t *testing.T) {
	if err := NewQwenTTSProvider("", "").HealthCheck(context.Background()); err == nil {
		t.Error("expected error when apiKey empty")
	}
	if err := NewQwenTTSProvider("key", "").HealthCheck(context.Background()); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestQwenTTSProvider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewQwenTTSProvider("key", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error = %q, want mention of 未指定音色", err.Error())
	}
}

// TestQwenTTSProvider_AudioGenerate_RealCall makes a real call to the Qwen TTS
// (DashScope) API. Skipped unless QWEN_TTS_API_KEY is set.
func TestQwenTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("QWEN_TTS_API_KEY")
	if apiKey == "" {
		t.Skip("QWEN_TTS_API_KEY not configured")
	}
	endpoint := os.Getenv("QWEN_TTS_ENDPOINT")

	p := NewQwenTTSProvider(apiKey, endpoint)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "Hello, world",
		Voice: "Cherry",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if resp.Format != "wav" {
		t.Errorf("Format = %q, want wav", resp.Format)
	}
}
