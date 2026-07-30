package minimax

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"testing"
	"time"
)

// Note: MinimaxVideoProvider talks to the hardcoded package-level constant
// minimaxVideoBaseURL (https://api.minimaxi.com) — the base URL is not injectable via
// NewMinimaxVideoProvider, so the HTTP round trip itself cannot be exercised against a
// local httptest server without modifying production code. Everything reachable without
// a live network call (model/mode validation) is covered below; the full submit→poll→
// retrieve flow is covered by the env-gated real-call test at the bottom of this file.

func TestNewMinimaxVideoProvider(t *testing.T) {
	p := NewMinimaxVideoProvider("api-key-123")
	if p.apiKey != "api-key-123" {
		t.Errorf("apiKey = %q, want %q", p.apiKey, "api-key-123")
	}
	if p.client == nil {
		t.Fatal("client should not be nil")
	}
	if p.client.Timeout != 60*time.Second {
		t.Errorf("client timeout = %v, want 60s", p.client.Timeout)
	}
}

func TestMinimaxVideoProvider_GetName(t *testing.T) {
	p := NewMinimaxVideoProvider("key")
	if got := p.GetName(); got != "minimax-video" {
		t.Errorf("GetName() = %q, want %q", got, "minimax-video")
	}
}

func TestMinimaxVideoProvider_ImplementsVideoProvider(t *testing.T) {
	var _ ai.VideoProvider = NewMinimaxVideoProvider("key")
}

func TestMinimaxVideoProvider_GenerateVideo_Validation(t *testing.T) {
	p := NewMinimaxVideoProvider("key")
	ctx := context.Background()

	t.Run("text-to-video requires prompt", func(t *testing.T) {
		_, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{})
		if err == nil {
			t.Error("expected error when prompt is empty and no ImageURL, got nil")
		}
	})

	t.Run("i2v-only model rejected without ImageURL", func(t *testing.T) {
		_, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
			Prompt: "a cat walking",
			Model:  "I2V-01",
		})
		if err == nil {
			t.Error("expected error for I2V-01 without ImageURL, got nil")
		}
	})

	t.Run("t2v-only model rejected with ImageURL", func(t *testing.T) {
		_, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
			Prompt:   "a cat walking",
			ImageURL: "https://example.com/cat.jpg",
			Model:    "T2V-01",
		})
		if err == nil {
			t.Error("expected error for T2V-01 with ImageURL, got nil")
		}
	})
}

func TestMapMinimaxVideoStatus(t *testing.T) {
	cases := map[string]string{
		"Preparing":  "pending",
		"Queueing":   "pending",
		"Processing": "processing",
		"Success":    "completed",
		"Fail":       "failed",
		"Unknown":    "Unknown",
	}
	for in, want := range cases {
		if got := mapMinimaxVideoStatus(in); got != want {
			t.Errorf("mapMinimaxVideoStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMinimaxVideoProvider_FullFlow_RealCall hits the live MiniMax video generation API
// end to end: submit (image-to-video) → poll status → retrieve download URL.
// Requires MINIMAX_API_KEY to be set; otherwise skipped. Video generation is slow, so
// this test polls for up to 5 minutes.
func TestMinimaxVideoProvider_FullFlow_RealCall(t *testing.T) {
	apiKey := os.Getenv("MINIMAX_API_KEY")
	if apiKey == "" {
		t.Skip("MINIMAX_API_KEY not set; skipping real MiniMax video API call")
	}

	p := NewMinimaxVideoProvider(apiKey)
	ctx := context.Background()

	task, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
		ImageURL: "https://cdn.hailuoai.com/prod/2024-09-18-16/user/multi_chat_file/9c0b5c14-ee88-4a5b-b503-4f626f018639.jpeg",
		Prompt:   "A mouse runs toward the camera, smiling and blinking.",
	})
	if err != nil {
		t.Fatalf("GenerateVideo failed: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty task_id")
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		status, err := p.GetVideoStatus(ctx, task.TaskID)
		if err != nil {
			t.Fatalf("GetVideoStatus failed: %v", err)
		}
		if status.Status == "completed" {
			break
		}
		if status.Status == "failed" {
			t.Fatalf("video generation failed: %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for video generation to complete")
		}
		time.Sleep(10 * time.Second)
	}

	url, err := p.GetVideoURL(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("GetVideoURL failed: %v", err)
	}
	if url == "" {
		t.Error("expected non-empty download URL")
	}
}
