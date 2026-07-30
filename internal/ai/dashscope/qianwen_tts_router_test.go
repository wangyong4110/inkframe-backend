package dashscope

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

func Test_ttsBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"empty returns empty", "", ""},
		{"strips path", "https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com"},
		{"bare host preserved", "https://dashscope.aliyuncs.com", "https://dashscope.aliyuncs.com"},
		{"workspace subdomain with path", "https://ws123.cn-beijing.maas.aliyuncs.com/v1/chat", "https://ws123.cn-beijing.maas.aliyuncs.com"},
		{"invalid URL returned as-is", "not a url:::", "not a url:::"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ttsBaseURL(tt.endpoint); got != tt.want {
				t.Errorf("ttsBaseURL(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestNewQianwenTTSRouter(t *testing.T) {
	r := NewQianwenTTSRouter("key", "https://dashscope.aliyuncs.com/compatible-mode/v1")
	if r.aliyun == nil {
		t.Fatal("aliyun sub-provider should not be nil")
	}
	if r.qwen == nil {
		t.Fatal("qwen sub-provider should not be nil")
	}
	if r.aliyun.apiKey != "key" {
		t.Errorf("aliyun apiKey = %q, want key", r.aliyun.apiKey)
	}
	if r.aliyun.endpoint != "https://dashscope.aliyuncs.com" {
		t.Errorf("aliyun endpoint = %q, want base URL without path", r.aliyun.endpoint)
	}
	if r.qwen.endpoint != "https://dashscope.aliyuncs.com" {
		t.Errorf("qwen endpoint = %q, want base URL without path", r.qwen.endpoint)
	}
}

func TestQianwenTTSRouter_GetName(t *testing.T) {
	r := NewQianwenTTSRouter("key", "")
	if got := r.GetName(); got != "qianwen-tts-router" {
		t.Errorf("GetName() = %q, want qianwen-tts-router", got)
	}
}

func TestQianwenTTSRouter_GetModels(t *testing.T) {
	r := NewQianwenTTSRouter("key", "")
	if got := r.GetModels(); got != nil {
		t.Errorf("GetModels() = %v, want nil", got)
	}
}

func TestQianwenTTSRouter_HealthCheck(t *testing.T) {
	// HealthCheck delegates to the qwen sub-provider, which only checks apiKey presence.
	if err := NewQianwenTTSRouter("", "").HealthCheck(context.Background()); err == nil {
		t.Error("expected error when apiKey empty")
	}
	if err := NewQianwenTTSRouter("key", "").HealthCheck(context.Background()); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestQianwenTTSRouter_AudioGenerate_Routing verifies voice-ID-prefix-based dispatch
// without making any real network call: both sub-providers reject an empty voice with
// their "未指定音色" validation error before ever reaching HTTP, so we instead confirm
// routing indirectly through a missing-text validation path is not available (both
// providers require Voice to be non-empty first). Since neither sub-provider has an
// early network-free success path, this test validates dispatch by checking a
// prefix-driven case that fails identically in both branches (missing voice), and a
// second case using a distinguishable error path: an invalid HTTP endpoint that fails
// fast with a connection error, letting us distinguish which sub-provider's endpoint
// was used by checking which provider's error prefix appears.
func TestQianwenTTSRouter_AudioGenerate_Routing(t *testing.T) {
	tests := []struct {
		name  string
		voice string
	}{
		{"long prefix routes to aliyun", "longxiaochun"},
		{"loong prefix routes to aliyun", "loongbella"},
		{"LONG prefix (case-insensitive) routes to aliyun", "LongXiaoChun"},
		{"other voice routes to qwen", "Cherry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewQianwenTTSRouter("key", "http://127.0.0.1:0") // unreachable port, fails fast
			_, err := r.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
				Text:  "hello",
				Voice: tt.voice,
			})
			if err == nil {
				t.Fatal("expected error from unreachable endpoint")
			}
			wantPrefix := "qwen-tts:"
			if strings.HasPrefix(strings.ToLower(tt.voice), "long") || strings.HasPrefix(strings.ToLower(tt.voice), "loong") {
				wantPrefix = "aliyun-tts:"
			}
			if !strings.Contains(err.Error(), wantPrefix) {
				t.Errorf("error = %q, want it to come from provider prefixed %q (routing check)", err.Error(), wantPrefix)
			}
		})
	}
}

// TestQianwenTTSRouter_AudioGenerate_RealCall makes a real call to DashScope, routed to
// either the Aliyun CosyVoice or Qwen TTS sub-provider depending on voice ID. Skipped
// unless QIANWEN_TTS_API_KEY is set.
func TestQianwenTTSRouter_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("QIANWEN_TTS_API_KEY")
	if apiKey == "" {
		t.Skip("QIANWEN_TTS_API_KEY not configured")
	}
	endpoint := os.Getenv("QIANWEN_TTS_ENDPOINT")

	r := NewQianwenTTSRouter(apiKey, endpoint)
	resp, err := r.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "longxiaochun",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
}
