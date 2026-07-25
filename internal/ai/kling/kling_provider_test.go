package kling

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func TestNewKlingProvider(t *testing.T) {
	p := NewKlingProvider("ak", "sk", "")
	if p == nil {
		t.Fatal("NewKlingProvider returned nil")
	}
	if p.accessKey != "ak" || p.secretKey != "sk" {
		t.Errorf("unexpected keys: accessKey=%q secretKey=%q", p.accessKey, p.secretKey)
	}
	if p.endpoint != "https://api-beijing.klingai.com" {
		t.Errorf("endpoint = %q, want default", p.endpoint)
	}
	if p.client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestNewKlingProvider_CustomEndpoint(t *testing.T) {
	p := NewKlingProvider("ak", "sk", "https://custom.host/v1/")
	if p.endpoint != "https://custom.host" {
		t.Errorf("endpoint = %q, want https://custom.host", p.endpoint)
	}
}

func TestKlingProvider_GetName(t *testing.T) {
	p := NewKlingProvider("ak", "sk", "")
	if got := p.GetName(); got != ProviderNameKling {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameKling)
	}
	if ProviderNameKling != "kling" {
		t.Errorf("ProviderNameKling = %q, want kling", ProviderNameKling)
	}
}

func TestKlingProvider_RegisteredTraits(t *testing.T) {
	traits := ai.VideoEngineTraitsFor(ProviderNameKling)
	if !traits.SupportsMultiImageReference {
		t.Error("expected kling traits to support multi-image reference")
	}
}

func TestIs3xModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"kling-3.0-turbo", true},
		{"kling-3.1", true},
		{"kling-3.5-fast", true},
		{"kling-v1", false},
		{"kling-v1-6", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := is3xModel(tc.model); got != tc.want {
			t.Errorf("is3xModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestKling3StatusToInternal(t *testing.T) {
	cases := map[string]string{
		"submitted":  "pending",
		"processing": "processing",
		"succeeded":  "completed",
		"failed":     "failed",
		"unknown-xy": "unknown-xy",
	}
	for in, want := range cases {
		if got := kling3StatusToInternal(in); got != want {
			t.Errorf("kling3StatusToInternal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKlingVideoTaskPath(t *testing.T) {
	cases := []struct {
		name      string
		taskID    string
		wantPath  string
		wantRawID string
	}{
		{"t2v3 prefix", "t2v3:abc123", "/tasks?external_task_ids=abc123", "abc123"},
		{"multi prefix", "multi:xyz789", "/v1/videos/multi-image2video/xyz789", "xyz789"},
		{"no prefix (v1 single image / t2v)", "plain-id-1", "/v1/videos/image2video/plain-id-1", "plain-id-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, rawID := klingVideoTaskPath(tc.taskID)
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if rawID != tc.wantRawID {
				t.Errorf("rawID = %q, want %q", rawID, tc.wantRawID)
			}
		})
	}
}

// ---- Real-call tests (network-gated) ----

func klingTestCredentials(t *testing.T) (accessKey, secretKey string) {
	t.Helper()
	accessKey = os.Getenv("KLING_ACCESS_KEY")
	secretKey = os.Getenv("KLING_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("KLING_ACCESS_KEY / KLING_SECRET_KEY not set; skipping real Kling API call")
	}
	return accessKey, secretKey
}

// TestKlingProvider_GenerateVideo_RealCall submits a real text-to-video job
// against the Kling API when credentials are configured via env vars, and
// skips otherwise. It exercises GenerateVideo, GetVideoStatus and (best
// effort) GetVideoURL against the live service.
func TestKlingProvider_GenerateVideo_RealCall(t *testing.T) {
	accessKey, secretKey := klingTestCredentials(t)
	endpoint := os.Getenv("KLING_ENDPOINT")

	p := NewKlingProvider(accessKey, secretKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
		Prompt:      "a calm ocean at sunset, cinematic",
		AspectRatio: "16:9",
		Duration:    5,
		Model:       "kling-v1",
	})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	if task.Provider != ProviderNameKling {
		t.Errorf("Provider = %q, want %q", task.Provider, ProviderNameKling)
	}
	t.Logf("submitted kling task: %s status=%s", task.TaskID, task.Status)

	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer statusCancel()
	status, err := p.GetVideoStatus(statusCtx, task.TaskID)
	if err != nil {
		t.Fatalf("GetVideoStatus: %v", err)
	}
	t.Logf("kling task status: %+v", status)
}

// TestKlingProvider_GenerateVideo3x_RealCall exercises the Kling 3.x
// text-to-video path (generate3xTurbo) when credentials are configured.
func TestKlingProvider_GenerateVideo3x_RealCall(t *testing.T) {
	accessKey, secretKey := klingTestCredentials(t)
	endpoint := os.Getenv("KLING_ENDPOINT")

	p := NewKlingProvider(accessKey, secretKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
		Prompt:      "a spaceship flying through a nebula",
		AspectRatio: "16:9",
		Duration:    5,
		Model:       "kling-3.0-turbo",
	})
	if err != nil {
		t.Fatalf("GenerateVideo (3.x): %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	t.Logf("submitted kling 3.x task: %s status=%s", task.TaskID, task.Status)
}
