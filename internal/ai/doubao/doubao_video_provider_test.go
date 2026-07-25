package doubao

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func TestNewDoubaoVideoProvider(t *testing.T) {
	p := NewDoubaoVideoProvider("key", "")
	if p.apiKey != "key" {
		t.Errorf("apiKey = %q, want key", p.apiKey)
	}
	if p.endpoint != doubaoVideoDefaultEndpoint {
		t.Errorf("endpoint = %q, want default %q", p.endpoint, doubaoVideoDefaultEndpoint)
	}
	if p.client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestNewDoubaoVideoProvider_CustomEndpoint(t *testing.T) {
	p := NewDoubaoVideoProvider("key", "https://custom.endpoint")
	if p.endpoint != "https://custom.endpoint" {
		t.Errorf("endpoint = %q, want https://custom.endpoint", p.endpoint)
	}
}

func TestDoubaoVideoProvider_GetName(t *testing.T) {
	p := NewDoubaoVideoProvider("key", "")
	if got := p.GetName(); got != ProviderNameDoubaoVideo {
		t.Errorf("GetName() = %q, want %q", got, ProviderNameDoubaoVideo)
	}
	if ProviderNameDoubaoVideo != "doubao-video" {
		t.Errorf("ProviderNameDoubaoVideo = %q", ProviderNameDoubaoVideo)
	}
}

func TestDoubaoVideoProvider_RegisteredTraits(t *testing.T) {
	for _, name := range []string{ProviderNameDoubao, ProviderNameSeedance} {
		traits := ai.VideoEngineTraitsFor(name)
		if !traits.SnapsFixedDuration {
			t.Errorf("%s: expected SnapsFixedDuration=true", name)
		}
		if !traits.ResolvesModelFromDB {
			t.Errorf("%s: expected ResolvesModelFromDB=true", name)
		}
		if !traits.SupportsMultiImageReference {
			t.Errorf("%s: expected SupportsMultiImageReference=true", name)
		}
		if !traits.SupportsTemporalLinking {
			t.Errorf("%s: expected SupportsTemporalLinking=true", name)
		}
		if !traits.SupportsExtendedVideoParams {
			t.Errorf("%s: expected SupportsExtendedVideoParams=true", name)
		}
		if traits.DefaultResolution == nil {
			t.Fatalf("%s: expected DefaultResolution func to be set", name)
		}
		if got := traits.DefaultResolution(false, ""); got != "1080p" {
			t.Errorf("%s: DefaultResolution(false, \"\") = %q, want 1080p", name, got)
		}
		if got := traits.DefaultResolution(true, "720p"); got != "720p" {
			t.Errorf("%s: DefaultResolution(true, \"720p\") = %q, want 720p (explicit override wins)", name, got)
		}
	}
}

func TestMapDoubaoVideoStatus(t *testing.T) {
	cases := map[string]string{
		"queued":    "pending",
		"running":   "processing",
		"succeeded": "completed",
		"failed":    "failed",
		"cancelled": "failed",
		"expired":   "failed",
		"weird":     "weird",
	}
	for in, want := range cases {
		if got := mapDoubaoVideoStatus(in); got != want {
			t.Errorf("mapDoubaoVideoStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDoubaoCollectImages(t *testing.T) {
	cases := []struct {
		name      string
		imageURL  string
		imageURLs []string
		want      []string
	}{
		{"empty", "", nil, []string{}},
		{"only imageURL", "a", nil, []string{"a"}},
		{"imageURL first then urls", "a", []string{"b", "c"}, []string{"a", "b", "c"}},
		{"dedupe imageURL from urls", "a", []string{"a", "b"}, []string{"a", "b"}},
		{"skip empty strings in urls", "a", []string{"", "b"}, []string{"a", "b"}},
		{"no imageURL, just urls", "", []string{"b", "c"}, []string{"b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := doubaoCollectImages(tc.imageURL, tc.imageURLs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("doubaoCollectImages(%q, %v) = %v, want %v", tc.imageURL, tc.imageURLs, got, tc.want)
			}
		})
	}
}

func TestDoubaoMakeContent_TextOnly(t *testing.T) {
	content := doubaoMakeContent(nil, nil, nil, "a story prompt")
	if len(content) != 1 {
		t.Fatalf("expected 1 content item (text only), got %d: %+v", len(content), content)
	}
	if content[0]["type"] != "text" || content[0]["text"] != "a story prompt" {
		t.Errorf("unexpected text item: %+v", content[0])
	}
}

func TestDoubaoMakeContent_SingleImage(t *testing.T) {
	content := doubaoMakeContent([]string{"img1"}, nil, nil, "")
	if len(content) != 1 {
		t.Fatalf("expected 1 content item, got %d: %+v", len(content), content)
	}
	if content[0]["role"] != "first_frame" {
		t.Errorf("expected role=first_frame for single image, got %v", content[0]["role"])
	}
	if content[0]["type"] != "image_url" {
		t.Errorf("expected type=image_url, got %v", content[0]["type"])
	}
}

func TestDoubaoMakeContent_TwoImages(t *testing.T) {
	content := doubaoMakeContent([]string{"img1", "img2"}, nil, nil, "")
	if len(content) != 2 {
		t.Fatalf("expected 2 content items, got %d", len(content))
	}
	if content[0]["role"] != "first_frame" {
		t.Errorf("image[0].role = %v, want first_frame", content[0]["role"])
	}
	if content[1]["role"] != "last_frame" {
		t.Errorf("image[1].role = %v, want last_frame", content[1]["role"])
	}
}

func TestDoubaoMakeContent_ThreePlusImagesAreReferenceImages(t *testing.T) {
	content := doubaoMakeContent([]string{"img1", "img2", "img3"}, nil, nil, "")
	if len(content) != 3 {
		t.Fatalf("expected 3 content items, got %d", len(content))
	}
	for i, item := range content {
		if item["role"] != "reference_image" {
			t.Errorf("image[%d].role = %v, want reference_image", i, item["role"])
		}
	}
}

func TestDoubaoMakeContent_VideosAndAudios(t *testing.T) {
	content := doubaoMakeContent(nil, []string{"vid1"}, []string{"aud1", ""}, "")
	// vid1 + aud1 (empty audio skipped)
	if len(content) != 2 {
		t.Fatalf("expected 2 content items, got %d: %+v", len(content), content)
	}
	if content[0]["type"] != "video_url" || content[0]["role"] != "reference_video" {
		t.Errorf("unexpected video item: %+v", content[0])
	}
	if content[1]["type"] != "audio_url" || content[1]["role"] != "reference_audio" {
		t.Errorf("unexpected audio item: %+v", content[1])
	}
}

func TestDoubaoMakeContent_FullMix(t *testing.T) {
	content := doubaoMakeContent([]string{"img1"}, []string{"vid1"}, []string{"aud1"}, "prompt text")
	if len(content) != 4 {
		t.Fatalf("expected 4 content items (image+video+audio+text), got %d: %+v", len(content), content)
	}
	if content[3]["type"] != "text" || content[3]["text"] != "prompt text" {
		t.Errorf("expected trailing text item, got %+v", content[3])
	}
}

func TestDoubaoMakeDraftContent(t *testing.T) {
	content := doubaoMakeDraftContent("draft-123", "")
	if len(content) != 1 {
		t.Fatalf("expected 1 item without prompt, got %d", len(content))
	}
	if content[0]["type"] != "draft_task" {
		t.Errorf("expected type=draft_task, got %v", content[0]["type"])
	}

	withPrompt := doubaoMakeDraftContent("draft-123", "refine this")
	if len(withPrompt) != 2 {
		t.Fatalf("expected 2 items with prompt, got %d", len(withPrompt))
	}
	if withPrompt[1]["type"] != "text" || withPrompt[1]["text"] != "refine this" {
		t.Errorf("expected trailing text item, got %+v", withPrompt[1])
	}
}

// ---- Real-call tests (network-gated) ----

func doubaoVideoTestCredentials(t *testing.T) (apiKey string) {
	t.Helper()
	apiKey = os.Getenv("DOUBAO_API_KEY")
	if apiKey == "" {
		t.Skip("DOUBAO_API_KEY not set; skipping real Doubao video API call")
	}
	return apiKey
}

// TestDoubaoVideoProvider_GenerateVideo_RealCall submits a real text-to-video
// task against the Volcengine Ark API. Requires DOUBAO_API_KEY and
// DOUBAO_VIDEO_MODEL (a valid model/endpoint ID) to be set; skips otherwise.
func TestDoubaoVideoProvider_GenerateVideo_RealCall(t *testing.T) {
	apiKey := doubaoVideoTestCredentials(t)
	model := os.Getenv("DOUBAO_VIDEO_MODEL")
	if model == "" {
		t.Skip("DOUBAO_VIDEO_MODEL not set; skipping real Doubao video API call")
	}
	endpoint := os.Getenv("DOUBAO_VIDEO_ENDPOINT")

	p := NewDoubaoVideoProvider(apiKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task, err := p.GenerateVideo(ctx, &ai.VideoGenerateRequest{
		Prompt:      "a quiet forest in the morning mist",
		Model:       model,
		AspectRatio: "16:9",
		Duration:    5,
	})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if task.TaskID == "" {
		t.Fatal("expected non-empty TaskID")
	}
	t.Logf("submitted doubao-video task: %s", task.TaskID)

	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer statusCancel()
	status, err := p.GetVideoStatus(statusCtx, task.TaskID)
	if err != nil {
		t.Fatalf("GetVideoStatus: %v", err)
	}
	t.Logf("doubao-video task status: %+v", status)

	// Best-effort cleanup: cancel/delete the task we just created.
	_ = p.DeleteVideoTask(context.Background(), task.TaskID)
}

// TestDoubaoVideoProvider_ListVideoTasks_RealCall exercises the
// Doubao-specific ListVideoTasks helper against the live API.
func TestDoubaoVideoProvider_ListVideoTasks_RealCall(t *testing.T) {
	apiKey := doubaoVideoTestCredentials(t)
	endpoint := os.Getenv("DOUBAO_VIDEO_ENDPOINT")

	p := NewDoubaoVideoProvider(apiKey, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := p.ListVideoTasks(ctx, &DoubaoVideoListRequest{PageNum: 1, PageSize: 5})
	if err != nil {
		t.Fatalf("ListVideoTasks: %v", err)
	}
	t.Logf("doubao-video task list: total=%d items=%d", resp.Total, len(resp.Items))
}
