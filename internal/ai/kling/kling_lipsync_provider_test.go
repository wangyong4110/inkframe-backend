package kling

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"testing"
	"time"
)

func TestNewKlingLipSyncProvider(t *testing.T) {
	p := NewKlingLipSyncProvider("ak", "sk", "")
	if p.accessKey != "ak" || p.secretKey != "sk" {
		t.Errorf("unexpected keys: accessKey=%q secretKey=%q", p.accessKey, p.secretKey)
	}
	if p.endpoint != klingLipSyncDefaultEndpoint {
		t.Errorf("endpoint = %q, want default %q", p.endpoint, klingLipSyncDefaultEndpoint)
	}
	if p.client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestNewKlingLipSyncProvider_CustomEndpoint(t *testing.T) {
	p := NewKlingLipSyncProvider("ak", "sk", "https://custom.host/v1")
	if p.endpoint != "https://custom.host" {
		t.Errorf("endpoint = %q, want https://custom.host", p.endpoint)
	}
}

func TestKlingLipSyncProvider_GetName(t *testing.T) {
	p := NewKlingLipSyncProvider("ak", "sk", "")
	if got := p.GetName(); got != "kling-lipsync" {
		t.Errorf("GetName() = %q, want kling-lipsync", got)
	}
}

func TestKlingLipSyncStatusToInternal(t *testing.T) {
	cases := map[string]string{
		"submitted":  "pending",
		"processing": "processing",
		"succeed":    "completed",
		"success":    "completed",
		"failed":     "failed",
		"weird":      "weird",
	}
	for in, want := range cases {
		if got := klingLipSyncStatusToInternal(in); got != want {
			t.Errorf("klingLipSyncStatusToInternal(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKlingLipSyncProvider_GenerateLipSync_ValidatesRequiredFields verifies
// GenerateLipSync rejects requests missing ImageURL/AudioURL before making
// any network call — no credentials needed for this path.
func TestKlingLipSyncProvider_GenerateLipSync_ValidatesRequiredFields(t *testing.T) {
	p := NewKlingLipSyncProvider("ak", "sk", "")
	ctx := context.Background()

	if _, err := p.GenerateLipSync(ctx, &ai.LipSyncRequest{AudioURL: "https://x/a.mp3"}); err == nil {
		t.Error("expected error when ImageURL is empty")
	}
	if _, err := p.GenerateLipSync(ctx, &ai.LipSyncRequest{ImageURL: "https://x/a.png"}); err == nil {
		t.Error("expected error when AudioURL is empty")
	}
}

// ---- Real-call tests (network-gated) ----

func klingLipSyncTestCredentials(t *testing.T) (accessKey, secretKey string) {
	t.Helper()
	accessKey = os.Getenv("KLING_ACCESS_KEY")
	secretKey = os.Getenv("KLING_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("KLING_ACCESS_KEY / KLING_SECRET_KEY not set; skipping real Kling lipsync API call")
	}
	return accessKey, secretKey
}

// TestKlingLipSyncProvider_GenerateLipSync_RealCall submits a real digital
// human lip-sync job when credentials and sample media URLs are configured.
func TestKlingLipSyncProvider_GenerateLipSync_RealCall(t *testing.T) {
	accessKey, secretKey := klingLipSyncTestCredentials(t)
	imageURL := os.Getenv("KLING_LIPSYNC_TEST_IMAGE_URL")
	audioURL := os.Getenv("KLING_LIPSYNC_TEST_AUDIO_URL")
	if imageURL == "" || audioURL == "" {
		t.Skip("KLING_LIPSYNC_TEST_IMAGE_URL / KLING_LIPSYNC_TEST_AUDIO_URL not set; skipping real Kling lipsync API call")
	}
	endpoint := os.Getenv("KLING_ENDPOINT")

	p := NewKlingLipSyncProvider(accessKey, secretKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateLipSync(ctx, &ai.LipSyncRequest{
		ImageURL: imageURL,
		AudioURL: audioURL,
	})
	if err != nil {
		t.Fatalf("GenerateLipSync: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	if task.Provider != "kling-lipsync" {
		t.Errorf("Provider = %q, want kling-lipsync", task.Provider)
	}
	t.Logf("submitted kling-lipsync task: %s status=%s", task.TaskID, task.Status)

	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer statusCancel()
	status, err := p.GetLipSyncStatus(statusCtx, task.TaskID)
	if err != nil {
		t.Fatalf("GetLipSyncStatus: %v", err)
	}
	t.Logf("kling-lipsync task status: %+v", status)
}
