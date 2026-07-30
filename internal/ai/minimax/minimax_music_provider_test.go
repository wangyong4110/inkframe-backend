package minimax

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"testing"
	"time"
)

// Note: MinimaxMusicProvider.AudioGenerate posts to the hardcoded package-level constant
// minimaxMusicEndpoint (https://api.minimaxi.com/v1/music_generation) — the endpoint is not
// injectable via NewMinimaxMusicProvider, so the HTTP round trip itself cannot be exercised
// against a local httptest server without modifying production code. Everything reachable
// without a live network call is covered below; request-building and response-parsing
// behavior is covered by the env-gated real-call test at the bottom of this file.

func TestNewMinimaxMusicProvider(t *testing.T) {
	p := NewMinimaxMusicProvider("api-key-123")
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

func TestMinimaxMusicProvider_GetName(t *testing.T) {
	p := NewMinimaxMusicProvider("key")
	if got := p.GetName(); got != "minimax-music" {
		t.Errorf("GetName() = %q, want %q", got, "minimax-music")
	}
}

func TestMinimaxMusicProvider_GetModels(t *testing.T) {
	p := NewMinimaxMusicProvider("key")
	models := p.GetModels()
	if len(models) != 4 {
		t.Errorf("GetModels() = %v, want 4 entries", models)
	}
}

func TestMinimaxMusicProvider_HealthCheck(t *testing.T) {
	t.Run("missing api key errors", func(t *testing.T) {
		p := NewMinimaxMusicProvider("")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Error("expected error when api_key is empty")
		}
	})

	t.Run("api key present succeeds", func(t *testing.T) {
		p := NewMinimaxMusicProvider("key")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Errorf("HealthCheck failed: %v", err)
		}
	})
}

func TestMinimaxMusicProvider_AudioGenerate_RejectsUnsupportedModel(t *testing.T) {
	p := NewMinimaxMusicProvider("key")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "some prompt",
		Model: "music-cover", // requires reference audio, not supported by this provider
	})
	if err == nil {
		t.Error("expected error for unsupported model music-cover, got nil")
	}
}

func TestMinimaxMusicProvider_ImplementsAIProvider(t *testing.T) {
	var _ ai.AudioProvider = NewMinimaxMusicProvider("key")
}

// TestMinimaxMusicProvider_AudioGenerate_RealCall hits the live MiniMax music generation API.
// Requires MINIMAX_API_KEY to be set; otherwise skipped. Generation is slow (client timeout
// is 5 minutes), so this test may take a while when it actually runs.
func TestMinimaxMusicProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("MINIMAX_API_KEY")
	if apiKey == "" {
		t.Skip("MINIMAX_API_KEY not set; skipping real MiniMax music API call")
	}

	p := NewMinimaxMusicProvider(apiKey)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:         "独立民谣,忧郁,内省,渴望,独自漫步,咖啡馆",
		Instrumental: true,
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
