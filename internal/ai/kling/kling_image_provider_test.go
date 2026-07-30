package kling

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"testing"
	"time"
)

// ─── Pure function tests: parseKlingAspectRatio ────────────────────────────

func TestParseKlingAspectRatio(t *testing.T) {
	cases := []struct {
		name string
		size string
		want string
	}{
		{"already valid 16:9", "16:9", "16:9"},
		{"already valid 1:1", "1:1", "1:1"},
		{"already valid 9:16", "9:16", "9:16"},
		{"already valid 21:9", "21:9", "21:9"},
		{"WxH square", "1024x1024", "1:1"},
		{"WxH 16:9-ish", "1920x1080", "16:9"},
		{"WxH 9:16-ish", "1080x1920", "9:16"},
		{"WxH 4:3-ish", "1024x768", "4:3"},
		{"WxH 3:2-ish", "1500x1000", "3:2"},
		{"invalid string falls back to default", "not-a-size", klingImageDefaultAspect},
		{"empty string falls back to default", "", klingImageDefaultAspect},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseKlingAspectRatio(c.size); got != c.want {
				t.Errorf("parseKlingAspectRatio(%q) = %q, want %q", c.size, got, c.want)
			}
		})
	}
}

// ─── Constructor / GetName / GetModels / HealthCheck ───────────────────────

func TestNewKlingImageProvider(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "")
	if p.endpoint != klingImageDefaultEndpoint {
		t.Errorf("endpoint = %q, want default %q", p.endpoint, klingImageDefaultEndpoint)
	}
	if p.accessKey != "ak" || p.secretKey != "sk" {
		t.Errorf("accessKey/secretKey not set correctly: %q/%q", p.accessKey, p.secretKey)
	}
	if p.client == nil {
		t.Error("expected non-nil http client")
	}
}

func TestNewKlingImageProvider_CustomEndpoint(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "https://custom.example.com/v1/")
	want := "https://custom.example.com"
	if p.endpoint != want {
		t.Errorf("endpoint = %q, want %q", p.endpoint, want)
	}
}

func TestKlingImageProvider_GetName(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "")
	if got := p.GetName(); got != ProviderNameKlingImage {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameKlingImage)
	}
}

func TestKlingImageProvider_GetModels(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "")
	models := p.GetModels()
	want := []string{"kling-v1", "kling-v1-5", "kling-v2", "kling-v2-new", "kling-v2-1", "kling-v3"}
	if len(models) != len(want) {
		t.Fatalf("GetModels() len = %d, want %d (%v)", len(models), len(want), models)
	}
	for i, m := range want {
		if models[i] != m {
			t.Errorf("GetModels()[%d] = %q, want %q", i, models[i], m)
		}
	}
}

func TestKlingImageProvider_HealthCheck(t *testing.T) {
	t.Run("missing credentials errors", func(t *testing.T) {
		p := NewKlingImageProvider("", "", "")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Error("expected error when access/secret key are empty")
		}
	})
	t.Run("credentials present succeeds without network", func(t *testing.T) {
		p := NewKlingImageProvider("ak", "sk", "")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Errorf("expected no error with credentials set, got %v", err)
		}
	})
}

// ─── Unsupported AIProvider methods ────────────────────────────────────────

func TestKlingImageProvider_UnsupportedMethods(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "")
	ctx := context.Background()

	if _, err := p.Generate(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("Generate() expected error, got nil")
	}
	if _, err := p.GenerateStream(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("GenerateStream() expected error, got nil")
	}
	if _, err := p.Embed(ctx, "text"); err == nil {
		t.Error("Embed() expected error, got nil")
	}
	if _, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{}); err == nil {
		t.Error("AudioGenerate() expected error, got nil")
	}
}

// ─── ImageGenerate validation (no network required) ────────────────────────

func TestKlingImageProvider_ImageGenerate_RequiresPrompt(t *testing.T) {
	p := NewKlingImageProvider("ak", "sk", "")
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{})
	if err == nil {
		t.Fatal("expected error when prompt is empty")
	}
}

// ─── ImageEngineTraits registration ────────────────────────────────────────

func TestKlingImage_ImageEngineTraitsRegistered(t *testing.T) {
	traits := ai.ImageEngineTraitsFor(ProviderNameKlingImage)
	if !traits.Supports2KResolution {
		t.Error("expected Supports2KResolution=true for kling-image")
	}
	if !traits.SupportsReferenceImage {
		t.Error("expected SupportsReferenceImage=true for kling-image")
	}
}

// ─── Interface compliance ──────────────────────────────────────────────────

func TestKlingImageProvider_ImplementsAIProvider(t *testing.T) {
	var _ ai.AIProvider = (*KlingImageProvider)(nil)
}

// ─── Real network call (env-gated) ─────────────────────────────────────────

// TestKlingImageProvider_RealCall exercises ImageGenerate end-to-end against
// the live Kling API. Requires KLING_ACCESS_KEY and KLING_SECRET_KEY to be
// set; skips otherwise.
func TestKlingImageProvider_RealCall(t *testing.T) {
	ak := os.Getenv("KLING_ACCESS_KEY")
	sk := os.Getenv("KLING_SECRET_KEY")
	if ak == "" || sk == "" {
		t.Skip("KLING_ACCESS_KEY/KLING_SECRET_KEY not set; skipping real API call")
	}

	endpoint := os.Getenv("KLING_ENDPOINT")
	p := NewKlingImageProvider(ak, sk, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute+30*time.Second)
	defer cancel()

	resp, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{
		Model:  "kling-v1",
		Prompt: "a small red apple on a white table, studio lighting",
		Size:   "1:1",
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty image URL in response")
	}
}
