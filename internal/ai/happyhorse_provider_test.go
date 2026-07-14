package ai

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNewHappyHorseProvider(t *testing.T) {
	p := NewHappyHorseProvider("key", "")
	if p.apiKey != "key" {
		t.Errorf("apiKey = %q, want key", p.apiKey)
	}
	if p.endpoint != "https://dashscope.aliyuncs.com" {
		t.Errorf("endpoint = %q, want default dashscope endpoint", p.endpoint)
	}
	if p.client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestNewHappyHorseProvider_CustomEndpoint(t *testing.T) {
	p := NewHappyHorseProvider("key", "https://ws123.cn-beijing.maas.aliyuncs.com")
	if p.endpoint != "https://ws123.cn-beijing.maas.aliyuncs.com" {
		t.Errorf("endpoint = %q, want custom endpoint", p.endpoint)
	}
}

func TestHappyHorseProvider_GetName(t *testing.T) {
	p := NewHappyHorseProvider("key", "")
	if got := p.GetName(); got != ProviderNameHappyHorse {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameHappyHorse)
	}
	if ProviderNameHappyHorse != "happyhorse" {
		t.Errorf("ProviderNameHappyHorse = %q", ProviderNameHappyHorse)
	}
}

func TestHappyHorseProvider_RegisteredTraits(t *testing.T) {
	traits := VideoEngineTraitsFor(ProviderNameHappyHorse)
	if !traits.SupportsMultiImageReference {
		t.Error("expected SupportsMultiImageReference=true")
	}
	if !traits.NeedsPerImageAnnotation {
		t.Error("expected NeedsPerImageAnnotation=true")
	}
	if traits.DefaultResolution == nil {
		t.Fatal("expected DefaultResolution func to be set")
	}
	if got := traits.DefaultResolution(true, ""); got != "1080p" {
		t.Errorf("DefaultResolution(true, \"\") = %q, want 1080p", got)
	}
	if got := traits.DefaultResolution(false, ""); got != "720p" {
		t.Errorf("DefaultResolution(false, \"\") = %q, want 720p", got)
	}
}

func TestMapHappyHorseStatus(t *testing.T) {
	cases := map[string]string{
		"PENDING":   "pending",
		"RUNNING":   "processing",
		"SUCCEEDED": "completed",
		"FAILED":    "failed",
		"CANCELED":  "failed",
		"WEIRD":     "pending", // default case
	}
	for in, want := range cases {
		if got := mapHappyHorseStatus(in); got != want {
			t.Errorf("mapHappyHorseStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- Real-call tests (network-gated) ----

func happyHorseTestCredentials(t *testing.T) (apiKey string) {
	t.Helper()
	apiKey = os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set; skipping real HappyHorse API call")
	}
	return apiKey
}

// TestHappyHorseProvider_GenerateVideo_T2V_RealCall submits a real
// text-to-video task (no reference images) against the DashScope API.
func TestHappyHorseProvider_GenerateVideo_T2V_RealCall(t *testing.T) {
	apiKey := happyHorseTestCredentials(t)
	endpoint := os.Getenv("DASHSCOPE_ENDPOINT")

	p := NewHappyHorseProvider(apiKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &VideoGenerateRequest{
		Prompt:      "a cat playing in a sunny garden",
		AspectRatio: "16:9",
		Duration:    5,
	})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	if task.Provider != ProviderNameHappyHorse {
		t.Errorf("Provider = %q, want %q", task.Provider, ProviderNameHappyHorse)
	}
	t.Logf("submitted happyhorse t2v task: %s status=%s", task.TaskID, task.Status)

	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer statusCancel()
	status, err := p.GetVideoStatus(statusCtx, task.TaskID)
	if err != nil {
		t.Fatalf("GetVideoStatus: %v", err)
	}
	t.Logf("happyhorse task status: %+v", status)
}

// TestHappyHorseProvider_GenerateVideo_I2V_RealCall submits a real
// image-to-video (single reference image, first_frame) task.
func TestHappyHorseProvider_GenerateVideo_I2V_RealCall(t *testing.T) {
	apiKey := happyHorseTestCredentials(t)
	imageURL := os.Getenv("HAPPYHORSE_TEST_IMAGE_URL")
	if imageURL == "" {
		t.Skip("HAPPYHORSE_TEST_IMAGE_URL not set; skipping real HappyHorse I2V API call")
	}
	endpoint := os.Getenv("DASHSCOPE_ENDPOINT")

	p := NewHappyHorseProvider(apiKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &VideoGenerateRequest{
		Prompt:   "the scene comes to life",
		ImageURL: imageURL,
		Duration: 5,
	})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	t.Logf("submitted happyhorse i2v task: %s status=%s", task.TaskID, task.Status)
}
