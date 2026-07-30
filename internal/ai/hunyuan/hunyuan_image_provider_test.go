package hunyuan

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// ─── Constructor ────────────────────────────────────────────────────────────

func TestNewHunyuanImageProvider(t *testing.T) {
	t.Run("empty baseURL defaults", func(t *testing.T) {
		p := NewHunyuanImageProvider("key", "")
		if p.baseURL != hunyuanImageBaseURL {
			t.Errorf("baseURL = %q, want default %q", p.baseURL, hunyuanImageBaseURL)
		}
		if p.apiKey != "key" {
			t.Errorf("apiKey = %q, want key", p.apiKey)
		}
		if p.client == nil {
			t.Error("expected non-nil http client")
		}
	})

	t.Run("custom baseURL preserved", func(t *testing.T) {
		p := NewHunyuanImageProvider("key", "https://custom.example.com/v1")
		if p.baseURL != "https://custom.example.com/v1" {
			t.Errorf("baseURL = %q, want custom", p.baseURL)
		}
	})
}

// ─── GetName / GetModels / HealthCheck ─────────────────────────────────────

func TestHunyuanImageProvider_GetName(t *testing.T) {
	p := NewHunyuanImageProvider("key", "")
	if got := p.GetName(); got != "hunyuan-image" {
		t.Errorf("GetName() = %q, want hunyuan-image", got)
	}
}

func TestHunyuanImageProvider_GetModels(t *testing.T) {
	p := NewHunyuanImageProvider("key", "")
	models := p.GetModels()
	want := []string{hunyuanImageModelLite, hunyuanImageModelV3}
	if len(models) != len(want) {
		t.Fatalf("GetModels() = %v, want %v", models, want)
	}
	for i, m := range want {
		if models[i] != m {
			t.Errorf("GetModels()[%d] = %q, want %q", i, models[i], m)
		}
	}
}

func TestHunyuanImageProvider_HealthCheck(t *testing.T) {
	t.Run("missing api key errors", func(t *testing.T) {
		p := NewHunyuanImageProvider("", "")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Error("expected error when apiKey is empty")
		}
	})
	t.Run("api key present succeeds without network", func(t *testing.T) {
		p := NewHunyuanImageProvider("key", "")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Errorf("expected no error with apiKey set, got %v", err)
		}
	})
}

// ─── Interface compliance ──────────────────────────────────────────────────

func TestHunyuanImageProvider_ImplementsAIProvider(t *testing.T) {
	var _ ai.ImageProvider = (*HunyuanImageProvider)(nil)
}

// ─── ImageGenerate model dispatch + request/response handling via mock server ─

func TestHunyuanImageProvider_ImageGenerate_LiteDefaultModel(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"request_id": "req-1",
			"data":       []map[string]string{{"url": "https://cdn.example.com/lite.png"}},
		})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("test-api-key", srv.URL)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Prompt: "a cat", // Model empty -> defaults to lite
		Size:   "1024x1024",
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if resp.URL != "https://cdn.example.com/lite.png" {
		t.Errorf("URL = %q, want https://cdn.example.com/lite.png", resp.URL)
	}
	if resp.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", resp.LatencyMs)
	}
	if gotPath != hunyuanImageLitePath {
		t.Errorf("request path = %q, want %q", gotPath, hunyuanImageLitePath)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("Authorization header = %q, want Bearer test-api-key", gotAuth)
	}
	if gotBody["model"] != hunyuanImageModelLite {
		t.Errorf("body model = %v, want %v", gotBody["model"], hunyuanImageModelLite)
	}
	if gotBody["resolution"] != "1024x1024" {
		t.Errorf("body resolution = %v, want 1024x1024", gotBody["resolution"])
	}
	if gotBody["rsp_img_type"] != "url" {
		t.Errorf("body rsp_img_type = %v, want url", gotBody["rsp_img_type"])
	}
}

func TestHunyuanImageProvider_ImageGenerate_LiteExtraPassthrough(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{{"url": "https://cdn.example.com/x.png"}},
		})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Model:  hunyuanImageModelLite,
		Prompt: "a dog",
		Extra:  map[string]interface{}{"logo_add": true, "style": "vivid"},
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if gotBody["logo_add"] != true {
		t.Errorf("logo_add = %v, want true", gotBody["logo_add"])
	}
	if gotBody["style"] != "vivid" {
		t.Errorf("style = %v, want vivid", gotBody["style"])
	}
}

func TestHunyuanImageProvider_ImageGenerate_LiteNoImageURLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]string{}})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when response has no image URL")
	}
}

func TestHunyuanImageProvider_ImageGenerate_LiteHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error on HTTP 400")
	}
}

func TestHunyuanImageProvider_ImageGenerate_V3SubmitAndPollSucceeds(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case hunyuanImageSubmitPath:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != hunyuanImageModelV3 {
				t.Errorf("submit body model = %v, want %v", body["model"], hunyuanImageModelV3)
			}
			imgs, _ := body["images"].([]interface{})
			if len(imgs) != 2 {
				t.Errorf("submit body images len = %d, want 2", len(imgs))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "job-123",
				"status": "queued",
			})
		case hunyuanImageQueryPath:
			callCount++
			if callCount < 2 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "processing"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "completed",
				"data":   []map[string]string{{"url": "https://cdn.example.com/v3.png"}},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	resp, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Model:           hunyuanImageModelV3,
		Prompt:          "edit",
		ReferenceImages: []string{"https://a.com/1.png", "https://a.com/2.png"},
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if resp.URL != "https://cdn.example.com/v3.png" {
		t.Errorf("URL = %q, want https://cdn.example.com/v3.png", resp.URL)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 poll calls, got %d", callCount)
	}
}

func TestHunyuanImageProvider_ImageGenerate_V3SingleReferenceImageFallback(t *testing.T) {
	var gotImages []interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == hunyuanImageSubmitPath {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotImages, _ = body["images"].([]interface{})
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "job-1", "status": "queued"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "completed",
			"data":   []map[string]string{{"url": "https://cdn.example.com/one.png"}},
		})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Model:          hunyuanImageModelV3,
		Prompt:         "edit",
		ReferenceImage: "https://a.com/single.png",
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if len(gotImages) != 1 {
		t.Errorf("expected 1 image in submit body, got %d", len(gotImages))
	}
}

func TestHunyuanImageProvider_ImageGenerate_V3JobFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == hunyuanImageSubmitPath {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "job-fail", "status": "queued"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "failed"})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Model:  hunyuanImageModelV3,
		Prompt: "edit",
	})
	if err == nil {
		t.Fatal("expected error when job status is failed")
	}
}

func TestHunyuanImageProvider_ImageGenerate_V3NoJobIDErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "queued"}) // no id
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	_, err := p.ImageGenerate(context.Background(), &ai.ImageGenerateRequest{
		Model:  hunyuanImageModelV3,
		Prompt: "edit",
	})
	if err == nil {
		t.Fatal("expected error when submit response has no job id")
	}
}

func TestHunyuanImageProvider_ImageGenerate_V3ContextCancelDuringPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == hunyuanImageSubmitPath {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "job-cancel", "status": "queued"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "processing"})
	}))
	defer srv.Close()

	p := NewHunyuanImageProvider("key", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{
		Model:  hunyuanImageModelV3,
		Prompt: "edit",
	})
	if err == nil {
		t.Fatal("expected error when context is canceled during poll")
	}
}

// ─── Real network call (env-gated) ─────────────────────────────────────────

// TestHunyuanImageProvider_RealCall exercises ImageGenerate end-to-end
// against the live TokenHub Hunyuan API. Requires HUNYUAN_API_KEY (or
// TOKENHUB_API_KEY) to be set; skips otherwise.
func TestHunyuanImageProvider_RealCall(t *testing.T) {
	apiKey := os.Getenv("HUNYUAN_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("TOKENHUB_API_KEY")
	}
	if apiKey == "" {
		t.Skip("HUNYUAN_API_KEY/TOKENHUB_API_KEY not set; skipping real API call")
	}

	baseURL := os.Getenv("HUNYUAN_BASE_URL")
	p := NewHunyuanImageProvider(apiKey, baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{
		Model:  hunyuanImageModelLite,
		Prompt: "a small red apple on a white table, studio lighting",
	})
	if err != nil {
		t.Fatalf("ImageGenerate returned error: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty image URL in response")
	}
}
