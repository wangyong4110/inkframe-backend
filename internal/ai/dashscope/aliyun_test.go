package dashscope

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

func TestNewAliyunTTSProvider(t *testing.T) {
	t.Run("default endpoint when empty", func(t *testing.T) {
		p := NewAliyunTTSProvider("key123", "")
		if p.apiKey != "key123" {
			t.Fatalf("apiKey = %q, want %q", p.apiKey, "key123")
		}
		if p.endpoint != "https://dashscope.aliyuncs.com" {
			t.Fatalf("endpoint = %q, want default", p.endpoint)
		}
		if p.client == nil {
			t.Fatal("client should not be nil")
		}
	})

	t.Run("custom endpoint preserved", func(t *testing.T) {
		p := NewAliyunTTSProvider("key123", "https://custom.example.com")
		if p.endpoint != "https://custom.example.com" {
			t.Fatalf("endpoint = %q, want custom", p.endpoint)
		}
	})
}

func TestAliyunTTSProvider_GetName(t *testing.T) {
	p := NewAliyunTTSProvider("key", "")
	if got := p.GetName(); got != "aliyun-tts" {
		t.Fatalf("GetName() = %q, want %q", got, "aliyun-tts")
	}
}

func TestAliyunTTSProvider_HealthCheck(t *testing.T) {
	t.Run("missing api key errors", func(t *testing.T) {
		p := NewAliyunTTSProvider("", "")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Fatal("expected error for missing api_key")
		}
	})

	t.Run("api key present passes", func(t *testing.T) {
		p := NewAliyunTTSProvider("key", "")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestAliyunTTSProvider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewAliyunTTSProvider("key", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error message = %q, want it to mention 未指定音色", err.Error())
	}
}

func Test_aliyunTTSModelForVoice(t *testing.T) {
	tests := []struct {
		voice string
		want  string
	}{
		{"longxiaochun_v3", "cosyvoice-v3-flash"},
		{"loongbella_v3", "cosyvoice-v3-flash"},
		{"longxiaochun", "cosyvoice-v2"},
		{"longgaoseng", "cosyvoice-v2"},
		{"", "cosyvoice-v2"},
	}
	for _, tt := range tests {
		if got := aliyunTTSModelForVoice(tt.voice); got != tt.want {
			t.Errorf("aliyunTTSModelForVoice(%q) = %q, want %q", tt.voice, got, tt.want)
		}
	}
}

func Test_clampFloat(t *testing.T) {
	tests := []struct {
		v, lo, hi, want float64
	}{
		{1.5, 0.5, 2.0, 1.5},
		{0.1, 0.5, 2.0, 0.5},
		{3.0, 0.5, 2.0, 2.0},
		{0.5, 0.5, 2.0, 0.5},
		{2.0, 0.5, 2.0, 2.0},
	}
	for _, tt := range tests {
		if got := clampFloat(tt.v, tt.lo, tt.hi); got != tt.want {
			t.Errorf("clampFloat(%v, %v, %v) = %v, want %v", tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

func Test_truncate(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello"},
		{"", 5, ""},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		if got := ai.Truncate(tt.s, tt.n); got != tt.want {
			t.Errorf("ai.Truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}

func Test_emotionToAliyunInstruction(t *testing.T) {
	tests := []struct {
		emotion string
		want    string
	}{
		{"happy", "请用开心愉快的语气朗读"},
		{"HAPPY", "请用开心愉快的语气朗读"},
		{"sad", "请用悲伤低沉的语气朗读"},
		{"angry", "请用愤怒激动的语气朗读"},
		{"surprised", "请用惊讶的语气朗读"},
		{"已经是中文指令", "已经是中文指令"},
		{"unknown_emotion", "unknown_emotion"},
	}
	for _, tt := range tests {
		if got := emotionToAliyunInstruction(tt.emotion); got != tt.want {
			t.Errorf("emotionToAliyunInstruction(%q) = %q, want %q", tt.emotion, got, tt.want)
		}
	}
}

// TestAliyunTTSProvider_AudioGenerate_RealCall makes a real call to the Aliyun DashScope
// CosyVoice TTS API. It is skipped unless ALIYUN_TTS_API_KEY is set in the environment.
func TestAliyunTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("ALIYUN_TTS_API_KEY")
	if apiKey == "" {
		t.Skip("ALIYUN_TTS_API_KEY not configured")
	}
	endpoint := os.Getenv("ALIYUN_TTS_ENDPOINT")

	p := NewAliyunTTSProvider(apiKey, endpoint)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "longxiaochun",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
}
