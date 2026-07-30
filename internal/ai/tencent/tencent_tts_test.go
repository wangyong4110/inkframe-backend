package tencent

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

func TestNewTencentTTSProvider(t *testing.T) {
	t.Run("default region when empty", func(t *testing.T) {
		p := NewTencentTTSProvider("id", "key", "")
		if p.secretID != "id" || p.secretKey != "key" {
			t.Errorf("secretID/secretKey not set: %q %q", p.secretID, p.secretKey)
		}
		if p.region != "ap-guangzhou" {
			t.Errorf("region = %q, want ap-guangzhou", p.region)
		}
	})

	t.Run("custom region preserved", func(t *testing.T) {
		p := NewTencentTTSProvider("id", "key", "ap-shanghai")
		if p.region != "ap-shanghai" {
			t.Errorf("region = %q, want ap-shanghai", p.region)
		}
	})
}

func TestTencentTTSProvider_GetName(t *testing.T) {
	p := NewTencentTTSProvider("id", "key", "")
	if got := p.GetName(); got != "tencent-tts" {
		t.Errorf("GetName() = %q, want tencent-tts", got)
	}
}

func TestTencentTTSProvider_GetModels(t *testing.T) {
	p := NewTencentTTSProvider("id", "key", "")
	models := p.GetModels()
	if len(models) == 0 {
		t.Fatal("expected non-empty model list")
	}
	want := map[string]bool{"101001": false, "501000": false, "200000000": false}
	for _, m := range models {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for m, found := range want {
		if !found {
			t.Errorf("expected voice %q in GetModels()", m)
		}
	}
}

func TestTencentTTSProvider_HealthCheck(t *testing.T) {
	tests := []struct {
		name      string
		secretID  string
		secretKey string
		wantErr   bool
	}{
		{"both empty", "", "", true},
		{"missing key", "id", "", true},
		{"missing id", "", "key", true},
		{"both present", "id", "key", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewTencentTTSProvider(tt.secretID, tt.secretKey, "")
			err := p.HealthCheck(context.Background())
			if tt.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestTencentTTSProvider_UnsupportedMethods(t *testing.T) {
	p := NewTencentTTSProvider("id", "key", "")
	ctx := context.Background()
	if _, err := p.Generate(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("Generate should error")
	}
	if _, err := p.GenerateStream(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("GenerateStream should error")
	}
	if _, err := p.Embed(ctx, "x"); err == nil {
		t.Error("Embed should error")
	}
	if _, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate should error")
	}
}

func TestTencentTTSProvider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewTencentTTSProvider("id", "key", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error = %q, want mention of 未指定音色", err.Error())
	}
}

func TestTencentTTSProvider_AudioGenerate_InvalidVoiceID(t *testing.T) {
	p := NewTencentTTSProvider("id", "key", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "hello",
		Voice: "not-a-number",
	})
	if err == nil {
		t.Fatal("expected error for non-numeric voice ID")
	}
	if !strings.Contains(err.Error(), "无效的音色 ID") {
		t.Errorf("error = %q, want mention of 无效的音色 ID", err.Error())
	}
}

// TestTencentTTSProvider_buildAuthHeader verifies the TC3-HMAC-SHA256 signature
// construction is deterministic and well-formed for a fixed timestamp/payload/credentials,
// and that changing any input changes the signature (sanity check against a fixed
// wrong-answer / non-varying implementation).
func TestTencentTTSProvider_buildAuthHeader(t *testing.T) {
	p := NewTencentTTSProvider("AKIDtest", "secrettest", "ap-guangzhou")
	payload := []byte(`{"Text":"hello"}`)
	timestamp := int64(1700000000)

	auth, err := p.buildAuthHeader(timestamp, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(auth, "TC3-HMAC-SHA256 Credential=AKIDtest/") {
		t.Errorf("auth header missing expected prefix: %q", auth)
	}
	if !strings.Contains(auth, "/tts/tc3_request, SignedHeaders=content-type;host, Signature=") {
		t.Errorf("auth header missing expected structure: %q", auth)
	}

	// Determinism: same inputs -> same signature.
	auth2, err := p.buildAuthHeader(timestamp, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != auth2 {
		t.Errorf("buildAuthHeader is not deterministic: %q != %q", auth, auth2)
	}

	// Different payload -> different signature.
	auth3, err := p.buildAuthHeader(timestamp, []byte(`{"Text":"different"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == auth3 {
		t.Error("expected different signature for different payload")
	}

	// Different secretKey -> different signature.
	p2 := NewTencentTTSProvider("AKIDtest", "othersecret", "ap-guangzhou")
	auth4, err := p2.buildAuthHeader(timestamp, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == auth4 {
		t.Error("expected different signature for different secretKey")
	}
}

func Test_tc3SHA256Hex(t *testing.T) {
	// Known SHA-256 of empty string.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := tc3SHA256Hex([]byte("")); got != want {
		t.Errorf("tc3SHA256Hex(\"\") = %q, want %q", got, want)
	}
	// Deterministic for same input.
	if got1, got2 := tc3SHA256Hex([]byte("abc")), tc3SHA256Hex([]byte("abc")); got1 != got2 {
		t.Errorf("tc3SHA256Hex not deterministic: %q != %q", got1, got2)
	}
}

func Test_tc3HMACSHA256(t *testing.T) {
	mac1 := tc3HMACSHA256([]byte("key"), []byte("data"))
	mac2 := tc3HMACSHA256([]byte("key"), []byte("data"))
	if string(mac1) != string(mac2) {
		t.Error("tc3HMACSHA256 not deterministic for same inputs")
	}
	mac3 := tc3HMACSHA256([]byte("otherkey"), []byte("data"))
	if string(mac1) == string(mac3) {
		t.Error("expected different MAC for different key")
	}
}

func Test_normalizeTencentEmotion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"happy", "happy"},
		{"cheerful", "happy"},
		{"excited", "happy"},
		{"sad", "sad"},
		{"angry", "angry"},
		{"fear", "fear"},
		{"fearful", "fear"},
		{"calm", "neutral"},
		{"neutral", "neutral"},
		{"serious", "neutral"},
		{"HAPPY", "happy"},
		{"surprised", ""}, // not supported
		{"", ""},
		{"unknown", ""},
	}
	for _, tt := range tests {
		if got := normalizeTencentEmotion(tt.in); got != tt.want {
			t.Errorf("normalizeTencentEmotion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestTencentTTSProvider_AudioGenerate_RealCall makes a real call to the Tencent Cloud
// TTS API. Skipped unless TENCENT_TTS_SECRET_ID / TENCENT_TTS_SECRET_KEY are set.
func TestTencentTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	secretID := os.Getenv("TENCENT_TTS_SECRET_ID")
	secretKey := os.Getenv("TENCENT_TTS_SECRET_KEY")
	if secretID == "" || secretKey == "" {
		t.Skip("TENCENT_TTS_SECRET_ID / TENCENT_TTS_SECRET_KEY not configured")
	}
	region := os.Getenv("TENCENT_TTS_REGION")

	p := NewTencentTTSProvider(secretID, secretKey, region)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "101001",
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
	if strings.HasPrefix(resp.URL, "file://") {
		os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	}
}

func TestTencentTTSProvider_HealthCheck_RealCall(t *testing.T) {
	secretID := os.Getenv("TENCENT_TTS_SECRET_ID")
	secretKey := os.Getenv("TENCENT_TTS_SECRET_KEY")
	if secretID == "" || secretKey == "" {
		t.Skip("TENCENT_TTS_SECRET_ID / TENCENT_TTS_SECRET_KEY not configured")
	}
	p := NewTencentTTSProvider(secretID, secretKey, "")
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}
