package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

// Note: FunMusicProvider.AudioGenerate posts to the hardcoded package-level constant
// funMusicEndpoint (https://dashscope.aliyuncs.com/api/v1/services/audio/music/generation)
// — the endpoint is not injectable via NewFunMusicProvider, so the HTTP round trip itself
// cannot be exercised against a local httptest server without modifying production code.
// Everything reachable without a live network call is covered below; the request-building
// (model/gender defaulting) and response-parsing behavior is covered by the env-gated
// real-call test at the bottom of this file.

func TestNewFunMusicProvider(t *testing.T) {
	p := NewFunMusicProvider("api-key-123")
	if p.apiKey != "api-key-123" {
		t.Errorf("apiKey = %q, want %q", p.apiKey, "api-key-123")
	}
	if p.client == nil {
		t.Fatal("client should not be nil")
	}
	if p.client.Timeout != 300*time.Second {
		t.Errorf("client timeout = %v, want 300s", p.client.Timeout)
	}
}

func TestFunMusicProvider_GetName(t *testing.T) {
	p := NewFunMusicProvider("key")
	if got := p.GetName(); got != "fun-music" {
		t.Errorf("GetName() = %q, want %q", got, "fun-music")
	}
}

func TestFunMusicProvider_GetModels(t *testing.T) {
	p := NewFunMusicProvider("key")
	models := p.GetModels()
	if len(models) != 1 || models[0] != funMusicDefaultModel {
		t.Errorf("GetModels() = %v, want [%s]", models, funMusicDefaultModel)
	}
}

func TestFunMusicProvider_HealthCheck(t *testing.T) {
	t.Run("missing api key errors", func(t *testing.T) {
		p := NewFunMusicProvider("")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Error("expected error when api_key is empty")
		}
	})

	t.Run("api key present succeeds", func(t *testing.T) {
		p := NewFunMusicProvider("key")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Errorf("HealthCheck failed: %v", err)
		}
	})
}

func TestFunMusicProvider_UnsupportedMethods(t *testing.T) {
	p := NewFunMusicProvider("key")
	ctx := context.Background()

	if _, err := p.Generate(ctx, &GenerateRequest{}); err == nil {
		t.Error("Generate: expected unsupported error, got nil")
	}
	if _, err := p.GenerateStream(ctx, &GenerateRequest{}); err == nil {
		t.Error("GenerateStream: expected unsupported error, got nil")
	}
	if _, err := p.Embed(ctx, "hello"); err == nil {
		t.Error("Embed: expected unsupported error, got nil")
	}
	if _, err := p.ImageGenerate(ctx, &ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate: expected unsupported error, got nil")
	}
}

func TestFunMusicProvider_ImplementsAIProvider(t *testing.T) {
	var _ AIProvider = NewFunMusicProvider("key")
}

// TestFunMusicProvider_AudioGenerate_RealCall hits the live Aliyun DashScope Fun-Music API.
// Requires DASHSCOPE_API_KEY to be set; otherwise skipped. Generation is slow (client
// timeout is 5 minutes), so this test may take a while when it actually runs.
func TestFunMusicProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set; skipping real Fun-Music API call")
	}

	p := NewFunMusicProvider(apiKey)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{
		Text:  "夏日清新民谣，木吉他与口琴伴奏",
		Voice: "female",
	})
	if err != nil {
		t.Fatalf("AudioGenerate real call failed: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
}
