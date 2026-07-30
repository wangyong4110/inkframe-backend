package volcengine

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"testing"
	"time"
)

func TestNewVolcengineProvider(t *testing.T) {
	p := NewJimengVideoProvider("ak", "sk")
	if p == nil {
		t.Fatal("NewVolcengineProvider returned nil")
	}
	if p.svc == nil {
		t.Fatal("expected non-nil underlying volcengine visual service")
	}
}

func TestVolcengineProvider_GetName(t *testing.T) {
	p := NewJimengVideoProvider("ak", "sk")
	if got := p.GetName(); got != ProviderNameJimengVideo {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameJimengVideo)
	}
	if ProviderNameJimengVideo != "jimeng-video" {
		t.Errorf("ProviderNameJimengVideo = %q", ProviderNameJimengVideo)
	}
}

func TestErrVideoConcurrentLimit(t *testing.T) {
	if ErrVideoConcurrentLimit == nil {
		t.Fatal("ErrVideoConcurrentLimit must not be nil")
	}
	if ErrVideoConcurrentLimit.Error() == "" {
		t.Error("ErrVideoConcurrentLimit must have a non-empty message")
	}
}

func TestJimengParseTaskID(t *testing.T) {
	cases := []struct {
		name       string
		taskID     string
		wantReqKey string
		wantRawID  string
	}{
		{"recamera prefix", "recamera:abc", jimengReqKeyRecamera, "abc"},
		{"t2v 1080p prefix", "t2v-1080p:abc", jimengReqKeyT2V1080p, "abc"},
		{"i2v 1080p prefix", "i2v-1080p:abc", jimengReqKeyI2V1080p, "abc"},
		{"pro prefix", "pro:abc", jimengReqKeyPro, "abc"},
		{"i2v tail 1080p prefix", "i2v-tail-1080p:abc", jimengReqKeyI2VTail1080p, "abc"},
		{"i2v tail prefix", "i2v-tail:abc", jimengReqKeyI2VTail, "abc"},
		{"i2v prefix", "i2v:abc", jimengReqKeyI2V, "abc"},
		{"t2v prefix", "t2v:abc", jimengReqKeyT2V, "abc"},
		{"no prefix defaults to t2v", "abc", jimengReqKeyT2V, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqKey, rawID := jimengParseTaskID(tc.taskID)
			if reqKey != tc.wantReqKey {
				t.Errorf("reqKey = %q, want %q", reqKey, tc.wantReqKey)
			}
			if rawID != tc.wantRawID {
				t.Errorf("rawID = %q, want %q", rawID, tc.wantRawID)
			}
		})
	}
}

// TestJimengParseTaskID_PrefixOrderMatters verifies that longer/more-specific
// prefixes (e.g. "i2v-tail-1080p:") are matched before shorter overlapping
// ones (e.g. "i2v:"), since the switch in jimengParseTaskID is order-sensitive.
func TestJimengParseTaskID_PrefixOrderMatters(t *testing.T) {
	reqKey, rawID := jimengParseTaskID("i2v-tail-1080p:xyz")
	if reqKey != jimengReqKeyI2VTail1080p {
		t.Errorf("expected i2v-tail-1080p prefix to win, got reqKey=%q", reqKey)
	}
	if rawID != "xyz" {
		t.Errorf("rawID = %q, want xyz", rawID)
	}

	reqKey2, rawID2 := jimengParseTaskID("i2v-1080p:xyz")
	if reqKey2 != jimengReqKeyI2V1080p {
		t.Errorf("expected i2v-1080p prefix to win over plain i2v, got reqKey=%q", reqKey2)
	}
	if rawID2 != "xyz" {
		t.Errorf("rawID = %q, want xyz", rawID2)
	}
}

func TestJimengMapStatus(t *testing.T) {
	cases := map[string]string{
		"in_queue":   "pending",
		"generating": "processing",
		"done":       "completed",
		"not_found":  "failed",
		"expired":    "failed",
		"weird":      "weird",
	}
	for in, want := range cases {
		if got := jimengMapStatus(in); got != want {
			t.Errorf("jimengMapStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- Real-call tests (network-gated) ----

func jimengTestCredentials(t *testing.T) (accessKey, secretKey string) {
	t.Helper()
	accessKey = os.Getenv("VOLC_ACCESS_KEY")
	secretKey = os.Getenv("VOLC_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("VOLC_ACCESS_KEY / VOLC_SECRET_KEY not set; skipping real Jimeng video API call")
	}
	return accessKey, secretKey
}

// TestVolcengineProvider_GenerateVideo_RealCall submits a real
// text-to-video (T2V) task against the Volcengine visual (即梦) API.
func TestVolcengineProvider_GenerateVideo_RealCall(t *testing.T) {
	accessKey, secretKey := jimengTestCredentials(t)
	p := NewJimengVideoProvider(accessKey, secretKey)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
		Prompt:      "a river flowing through mountains, cinematic lighting",
		AspectRatio: "16:9",
		Duration:    5,
	})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	t.Logf("submitted jimeng-video task: %s status=%s", task.TaskID, task.Status)

	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer statusCancel()
	status, err := p.GetVideoStatus(statusCtx, task.TaskID)
	if err != nil {
		t.Fatalf("GetVideoStatus: %v", err)
	}
	t.Logf("jimeng-video task status: %+v", status)
}
