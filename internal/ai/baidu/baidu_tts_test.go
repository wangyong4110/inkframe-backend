package baidu

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func TestNewBaiduTTSProvider(t *testing.T) {
	p := NewBaiduTTSProvider("ak", "sk")
	if p.apiKey != "ak" {
		t.Errorf("apiKey = %q, want %q", p.apiKey, "ak")
	}
	if p.secretKey != "sk" {
		t.Errorf("secretKey = %q, want %q", p.secretKey, "sk")
	}
	if p.client == nil {
		t.Fatal("client should not be nil")
	}
}

func TestBaiduTTSProvider_GetName(t *testing.T) {
	p := NewBaiduTTSProvider("ak", "sk")
	if got := p.GetName(); got != "baidu-tts" {
		t.Errorf("GetName() = %q, want %q", got, "baidu-tts")
	}
}

func TestBaiduTTSProvider_GetModels(t *testing.T) {
	p := NewBaiduTTSProvider("ak", "sk")
	models := p.GetModels()
	want := []string{"0", "1", "3", "4", "5", "103", "106", "110", "111"}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d", len(models), len(want))
	}
	for i, w := range want {
		if models[i] != w {
			t.Errorf("models[%d] = %q, want %q", i, models[i], w)
		}
	}
}

func TestBaiduTTSProvider_HealthCheck_MissingCredentials(t *testing.T) {
	tests := []struct {
		name      string
		apiKey    string
		secretKey string
	}{
		{"both empty", "", ""},
		{"missing secret", "ak", ""},
		{"missing api key", "", "sk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewBaiduTTSProvider(tt.apiKey, tt.secretKey)
			if err := p.HealthCheck(context.Background()); err == nil {
				t.Fatal("expected error when credentials are missing")
			}
		})
	}
}

func TestBaiduTTSProvider_UnsupportedMethods(t *testing.T) {
	p := NewBaiduTTSProvider("ak", "sk")
	ctx := context.Background()

	if _, err := p.Generate(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("Generate should return error")
	}
	if _, err := p.GenerateStream(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("GenerateStream should return error")
	}
	if _, err := p.Embed(ctx, "text"); err == nil {
		t.Error("Embed should return error")
	}
	if _, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate should return error")
	}
}

func TestBaiduTTSProvider_AudioGenerate_MissingVoice(t *testing.T) {
	// getAccessToken is called before voice validation, so without real credentials
	// this will fail at the token fetch stage (network call to baiduTokenURL) rather
	// than reaching the voice check. We can only assert the code path errors out.
	p := NewBaiduTTSProvider("", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error with no credentials configured")
	}
}

// TestBaiduTTSProvider_getAccessToken_Cache verifies that a cached, unexpired token is
// returned without making a network call (the underlying HTTP round trip would fail
// since no real credentials are used, so success here proves the cache path was taken).
func TestBaiduTTSProvider_getAccessToken_Cache(t *testing.T) {
	p := NewBaiduTTSProvider("ak", "sk")
	p.accessToken = "cached-token"
	p.tokenExpiry = time.Now().Add(10 * time.Minute)

	token, err := p.getAccessToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cached-token" {
		t.Errorf("token = %q, want cached-token", token)
	}
}

// TestBaiduTTSProvider_getAccessToken_ExpiredForcesRefetch verifies that an expired
// cached token is not returned as-is; since we don't have real credentials, the refetch
// attempt against the real Baidu endpoint should fail with a network/auth error rather
// than silently returning the stale cached value.
func TestBaiduTTSProvider_getAccessToken_ExpiredForcesRefetch(t *testing.T) {
	p := NewBaiduTTSProvider("invalid-ak", "invalid-sk")
	p.accessToken = "stale-token"
	p.tokenExpiry = time.Now().Add(-1 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := p.getAccessToken(ctx)
	if err == nil && token == "stale-token" {
		t.Fatal("expired token should not be returned from cache")
	}
	// Either an error (network/auth failure) or a different token is acceptable;
	// what we must not see is the stale token silently reused.
}

// TestBaiduTTSProvider_AudioGenerate_RealCall makes a real call to the Baidu TTS API.
// It is skipped unless BAIDU_TTS_API_KEY and BAIDU_TTS_SECRET_KEY are set.
func TestBaiduTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("BAIDU_TTS_API_KEY")
	secretKey := os.Getenv("BAIDU_TTS_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		t.Skip("BAIDU_TTS_API_KEY / BAIDU_TTS_SECRET_KEY not configured")
	}

	p := NewBaiduTTSProvider(apiKey, secretKey)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "0",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" || !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("expected file:// URL, got %q", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	// Cleanup temp file
	if strings.HasPrefix(resp.URL, "file://") {
		os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	}
}

func TestBaiduTTSProvider_HealthCheck_RealCall(t *testing.T) {
	apiKey := os.Getenv("BAIDU_TTS_API_KEY")
	secretKey := os.Getenv("BAIDU_TTS_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		t.Skip("BAIDU_TTS_API_KEY / BAIDU_TTS_SECRET_KEY not configured")
	}

	p := NewBaiduTTSProvider(apiKey, secretKey)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}
