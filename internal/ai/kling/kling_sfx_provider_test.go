package kling

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// decodeJSONBody decodes an HTTP request's JSON body into dst, failing the test on error.
func decodeJSONBody(t *testing.T, r *http.Request, dst interface{}) {
	t.Helper()
	if r.Body == nil {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		t.Fatalf("failed to decode request body: %v", err)
	}
}

func TestNewKlingSFXProvider(t *testing.T) {
	t.Run("normalizes empty endpoint to default", func(t *testing.T) {
		p := NewKlingSFXProvider("ak", "sk", "")
		if p.endpoint != klingSFXDefaultEndpoint {
			t.Errorf("endpoint = %q, want default %q", p.endpoint, klingSFXDefaultEndpoint)
		}
		if p.accessKey != "ak" || p.secretKey != "sk" {
			t.Errorf("accessKey/secretKey = %q/%q, want ak/sk", p.accessKey, p.secretKey)
		}
	})

	t.Run("normalizes custom endpoint trailing slash and v1", func(t *testing.T) {
		p := NewKlingSFXProvider("ak", "sk", "https://custom.example.com/v1/")
		if p.endpoint != "https://custom.example.com" {
			t.Errorf("endpoint = %q, want trimmed custom endpoint", p.endpoint)
		}
	})
}

func TestKlingSFXProvider_GetName(t *testing.T) {
	p := NewKlingSFXProvider("ak", "sk", "")
	if got := p.GetName(); got != "kling-sfx" {
		t.Errorf("GetName() = %q, want %q", got, "kling-sfx")
	}
}

func TestKlingSFXProvider_GetModels(t *testing.T) {
	p := NewKlingSFXProvider("ak", "sk", "")
	want := []string{"3s", "5s", "7s", "10s"}
	got := p.GetModels()
	if len(got) != len(want) {
		t.Fatalf("GetModels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GetModels()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestKlingSFXProvider_HealthCheck(t *testing.T) {
	cases := []struct {
		name      string
		accessKey string
		secretKey string
		wantErr   bool
	}{
		{"both missing", "", "", true},
		{"access key missing", "", "sk", true},
		{"secret key missing", "ak", "", true},
		{"both present", "ak", "sk", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewKlingSFXProvider(tc.accessKey, tc.secretKey, "")
			err := p.HealthCheck(context.Background())
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestKlingSFXProvider_ImplementsAIProvider(t *testing.T) {
	var _ ai.AudioProvider = NewKlingSFXProvider("ak", "sk", "")
}

func TestKlingSFXProvider_AudioGenerate_EmptyTextErrors(t *testing.T) {
	p := NewKlingSFXProvider("ak", "sk", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: ""})
	if err == nil {
		t.Fatal("expected error for empty Text")
	}
	if !strings.Contains(err.Error(), "prompt (Text) is required") {
		t.Errorf("error = %v, want mention of required prompt", err)
	}
}

// klingSFXMockServer builds an httptest server simulating the submit+poll task flow.
// succeedAfterPolls controls how many GET queries return "processing" before "succeed".
func klingSFXMockServer(t *testing.T, succeedAfterPolls int, mp3URL, durationMp3 string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	pollCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/text-to-audio":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    0,
				"message": "ok",
				"data":    map[string]interface{}{"task_id": "task-123"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/audio/text-to-audio/"):
			mu.Lock()
			pollCount++
			n := pollCount
			mu.Unlock()
			if n <= succeedAfterPolls {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code": 0,
					"data": map[string]interface{}{
						"task_id":     "task-123",
						"task_status": "processing",
					},
				})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"task_id":     "task-123",
					"task_status": "succeed",
					"task_result": map[string]interface{}{
						"audios": []map[string]interface{}{
							{"id": "a1", "url_mp3": mp3URL, "url_wav": "", "duration_mp3": durationMp3},
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server, &paths
}

// Speed up polling for tests by temporarily... we cannot change the const, so tests that
// rely on polling use short succeedAfterPolls counts; klingSFXPollInterval (2s) means each
// poll cycle costs real wall-clock time. Keep succeedAfterPolls small (0 or 1) to keep the
// test suite fast.

func TestKlingSFXProvider_AudioGenerate_SucceedsImmediately(t *testing.T) {
	server, paths := klingSFXMockServer(t, 0, "https://cdn.example.com/sfx.mp3", "4.5")
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:     "春节烟花声",
		Duration: 5.0,
	})
	if err != nil {
		t.Fatalf("AudioGenerate failed: %v", err)
	}
	if resp.URL != "https://cdn.example.com/sfx.mp3" {
		t.Errorf("URL = %q, want mock mp3 URL", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	if resp.Duration != 4.5 {
		t.Errorf("Duration = %v, want 4.5", resp.Duration)
	}
	if resp.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", resp.LatencyMs)
	}

	gotPaths := *paths
	if len(gotPaths) < 2 {
		t.Fatalf("expected at least a submit + one poll call, got %v", gotPaths)
	}
	if gotPaths[0] != "POST /v1/audio/text-to-audio" {
		t.Errorf("first call = %q, want POST /v1/audio/text-to-audio", gotPaths[0])
	}
}

func TestKlingSFXProvider_AudioGenerate_PollsUntilSucceed(t *testing.T) {
	server, paths := klingSFXMockServer(t, 1, "https://cdn.example.com/sfx2.mp3", "3.0")
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "雨打树叶声"})
	if err != nil {
		t.Fatalf("AudioGenerate failed: %v", err)
	}
	if resp.URL != "https://cdn.example.com/sfx2.mp3" {
		t.Errorf("URL = %q, want mock mp3 URL", resp.URL)
	}

	gotPaths := *paths
	getCalls := 0
	for _, p := range gotPaths {
		if strings.HasPrefix(p, "GET ") {
			getCalls++
		}
	}
	if getCalls < 2 {
		t.Errorf("expected at least 2 poll (GET) calls, got %d: %v", getCalls, gotPaths)
	}
}

func TestKlingSFXProvider_AudioGenerate_DurationFromModelField(t *testing.T) {
	var gotDuration interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]interface{}
			decodeJSONBody(t, r, &body)
			gotDuration = body["duration"]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{"task_id": "t1"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"task_id":     "t1",
				"task_status": "succeed",
				"task_result": map[string]interface{}{
					"audios": []map[string]interface{}{
						{"url_mp3": "https://cdn.example.com/x.mp3", "duration_mp3": "7"},
					},
				},
			},
		})
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "test",
		Model: "7s", // no explicit Duration -> parsed from Model
	})
	if err != nil {
		t.Fatalf("AudioGenerate failed: %v", err)
	}
	if gotDuration != 7.0 {
		t.Errorf("duration sent = %v, want 7.0 (parsed from Model=7s)", gotDuration)
	}
}

func TestKlingSFXProvider_AudioGenerate_DurationClamped(t *testing.T) {
	cases := []struct {
		name     string
		duration float64
		want     float64
	}{
		{"below min clamps up", 1.0, klingSFXMinDuration},
		{"above max clamps down", 50.0, klingSFXMaxDuration},
		{"zero uses default", 0, klingSFXDefaultDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotDuration interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var body map[string]interface{}
					decodeJSONBody(t, r, &body)
					gotDuration = body["duration"]
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"code": 0,
						"data": map[string]interface{}{"task_id": "t1"},
					})
					return
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code": 0,
					"data": map[string]interface{}{
						"task_id":     "t1",
						"task_status": "succeed",
						"task_result": map[string]interface{}{
							"audios": []map[string]interface{}{
								{"url_mp3": "https://cdn.example.com/x.mp3", "duration_mp3": "1"},
							},
						},
					},
				})
			}))
			defer server.Close()

			p := NewKlingSFXProvider("ak", "sk", server.URL)
			_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
				Text:     "test",
				Duration: tc.duration,
			})
			if err != nil {
				t.Fatalf("AudioGenerate failed: %v", err)
			}
			if gotDuration != tc.want {
				t.Errorf("duration sent = %v, want %v", gotDuration, tc.want)
			}
		})
	}
}

func TestKlingSFXProvider_AudioGenerate_TaskFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{"task_id": "t1"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"task_id":         "t1",
				"task_status":     "failed",
				"task_status_msg": "content violates policy",
			},
		})
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for failed task")
	}
	if !strings.Contains(err.Error(), "content violates policy") {
		t.Errorf("error = %v, want mention of failure reason", err)
	}
}

func TestKlingSFXProvider_AudioGenerate_SubmitAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code":    1234,
			"message": "invalid prompt",
		})
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for non-zero API code")
	}
	if !strings.Contains(err.Error(), "invalid prompt") {
		t.Errorf("error = %v, want mention of API error message", err)
	}
}

func TestKlingSFXProvider_AudioGenerate_SubmitEmptyTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{"task_id": ""},
		})
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for empty task_id")
	}
	if !strings.Contains(err.Error(), "empty task_id") {
		t.Errorf("error = %v, want mention of empty task_id", err)
	}
}

func TestKlingSFXProvider_AudioGenerate_SubmitHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error for HTTP 500 on submit")
	}
}

func TestKlingSFXProvider_AudioGenerate_SucceedWithNoAudios(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{"task_id": "t1"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"task_id":     "t1",
				"task_status": "succeed",
				"task_result": map[string]interface{}{"audios": []map[string]interface{}{}},
			},
		})
	}))
	defer server.Close()

	p := NewKlingSFXProvider("ak", "sk", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "test"})
	if err == nil {
		t.Fatal("expected error when succeeded task has no audio in result")
	}
	if !strings.Contains(err.Error(), "no audio in result") {
		t.Errorf("error = %v, want mention of no audio in result", err)
	}
}

// TestKlingSFXProvider_AudioGenerate_RealCall hits the live Kling text-to-audio API.
// Requires KLING_ACCESS_KEY and KLING_SECRET_KEY to be set; otherwise skipped. Note this
// test can take up to klingSFXMaxWait (5 minutes) if the real API is slow.
func TestKlingSFXProvider_AudioGenerate_RealCall(t *testing.T) {
	accessKey := os.Getenv("KLING_ACCESS_KEY")
	secretKey := os.Getenv("KLING_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("KLING_ACCESS_KEY / KLING_SECRET_KEY not set; skipping real Kling SFX API call")
	}

	p := NewKlingSFXProvider(accessKey, secretKey, os.Getenv("KLING_ENDPOINT"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	resp, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     "轻快的鸟鸣声",
		Duration: 3.0,
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
