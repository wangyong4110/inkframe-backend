package elevenlabs

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// decodeJSONBody decodes an HTTP request's JSON body into dst, failing the test on error.
// Shared by AudioGenerate tests across the SFX/music provider test files in this package.
func decodeJSONBody(t *testing.T, r *http.Request, dst interface{}) {
	t.Helper()
	if r.Body == nil {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		t.Fatalf("failed to decode request body: %v", err)
	}
}

func TestNewElevenLabsSFXProvider(t *testing.T) {
	t.Run("empty endpoint uses default", func(t *testing.T) {
		p := NewElevenLabsSFXProvider("key", "")
		if p.endpoint != elevenLabsSFXDefaultEndpoint {
			t.Errorf("endpoint = %q, want default %q", p.endpoint, elevenLabsSFXDefaultEndpoint)
		}
		if p.apiKey != "key" {
			t.Errorf("apiKey = %q, want %q", p.apiKey, "key")
		}
		if p.client == nil {
			t.Fatal("client should not be nil")
		}
	})

	t.Run("custom endpoint preserved", func(t *testing.T) {
		p := NewElevenLabsSFXProvider("key", "https://custom.example.com")
		if p.endpoint != "https://custom.example.com" {
			t.Errorf("endpoint = %q, want custom endpoint", p.endpoint)
		}
	})
}

func TestElevenLabsSFXProvider_GetName(t *testing.T) {
	p := NewElevenLabsSFXProvider("key", "")
	if got := p.GetName(); got != "elevenlabs-sfx" {
		t.Errorf("GetName() = %q, want %q", got, "elevenlabs-sfx")
	}
}

func TestElevenLabsSFXProvider_GetModels(t *testing.T) {
	p := NewElevenLabsSFXProvider("key", "")
	models := p.GetModels()
	if len(models) != 1 || models[0] != "sound-generation" {
		t.Errorf("GetModels() = %v, want [sound-generation]", models)
	}
}

func TestElevenLabsSFXProvider_HealthCheck(t *testing.T) {
	t.Run("missing api key errors", func(t *testing.T) {
		p := NewElevenLabsSFXProvider("", "")
		if err := p.HealthCheck(context.Background()); err == nil {
			t.Error("expected error when API key is empty")
		}
	})

	t.Run("api key present succeeds", func(t *testing.T) {
		p := NewElevenLabsSFXProvider("key", "")
		if err := p.HealthCheck(context.Background()); err != nil {
			t.Errorf("HealthCheck failed: %v", err)
		}
	})
}

// Ensure interface compliance at the test level too (compile-time check already exists in
// the source file, but this documents intent).
func TestElevenLabsSFXProvider_ImplementsAIProvider(t *testing.T) {
	var _ ai.AudioProvider = NewElevenLabsSFXProvider("key", "")
}

func TestElevenLabsSFXProvider_AudioGenerate_EmptyTextErrors(t *testing.T) {
	p := NewElevenLabsSFXProvider("key", "")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: ""})
	if err == nil {
		t.Fatal("expected error for empty Text")
	}
	if !strings.Contains(err.Error(), "prompt (Text) is required") {
		t.Errorf("error = %v, want mention of required prompt", err)
	}
}

// TestElevenLabsSFXProvider_AudioGenerate_HTTPMocked exercises the full AudioGenerate path
// (request building, duration clamping, temp file write, response fields) against a local
// httptest server — no real network / credentials required.
func TestElevenLabsSFXProvider_AudioGenerate_HTTPMocked(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotContentType, gotAccept string
	var gotBody map[string]interface{}

	audioBytes := []byte("fake-mp3-bytes")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("xi-api-key")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		decodeJSONBody(t, r, &gotBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audioBytes)
	}))
	defer server.Close()

	p := NewElevenLabsSFXProvider("test-api-key", server.URL)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:     "fireworks on new year's eve",
		Duration: 3.0,
	})
	if err != nil {
		t.Fatalf("AudioGenerate failed: %v", err)
	}
	defer removeFileURL(t, resp.URL)

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/sound-generation" {
		t.Errorf("path = %q, want /v1/sound-generation", gotPath)
	}
	if gotAPIKey != "test-api-key" {
		t.Errorf("xi-api-key header = %q, want test-api-key", gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAccept != "audio/mpeg" {
		t.Errorf("Accept = %q, want audio/mpeg", gotAccept)
	}
	if gotBody["text"] != "fireworks on new year's eve" {
		t.Errorf("request body text = %v, want prompt text", gotBody["text"])
	}
	if gotBody["duration_seconds"] != 3.0 {
		t.Errorf("request body duration_seconds = %v, want 3.0", gotBody["duration_seconds"])
	}
	if gotBody["prompt_influence"] != elevenLabsSFXDefaultInfluence {
		t.Errorf("request body prompt_influence = %v, want %v", gotBody["prompt_influence"], elevenLabsSFXDefaultInfluence)
	}

	if !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("response URL = %q, want file:// prefix", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	if resp.Duration != 3.0 {
		t.Errorf("Duration = %v, want 3.0", resp.Duration)
	}
	if resp.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", resp.LatencyMs)
	}

	// Verify the file actually contains what the server sent.
	path := strings.TrimPrefix(resp.URL, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved audio file: %v", err)
	}
	if string(data) != string(audioBytes) {
		t.Errorf("saved file content = %q, want %q", data, audioBytes)
	}
}

func TestElevenLabsSFXProvider_AudioGenerate_DurationDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		name     string
		duration float64
		want     float64
	}{
		{"zero uses default", 0, elevenLabsSFXDefaultDuration},
		{"negative uses default", -5, elevenLabsSFXDefaultDuration},
		{"below min clamps to min", 0.1, elevenLabsSFXMinDuration},
		{"above max clamps to max", 100, elevenLabsSFXMaxDuration},
		{"within range unchanged", 8.5, 8.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotDuration interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				decodeJSONBody(t, r, &body)
				gotDuration = body["duration_seconds"]
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("audio-bytes"))
			}))
			defer server.Close()

			p := NewElevenLabsSFXProvider("key", server.URL)
			resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
				Text:     "test sound",
				Duration: tc.duration,
			})
			if err != nil {
				t.Fatalf("AudioGenerate failed: %v", err)
			}
			defer removeFileURL(t, resp.URL)

			if gotDuration != tc.want {
				t.Errorf("duration_seconds sent = %v, want %v", gotDuration, tc.want)
			}
			if resp.Duration != tc.want {
				t.Errorf("resp.Duration = %v, want %v", resp.Duration, tc.want)
			}
		})
	}
}

func TestElevenLabsSFXProvider_AudioGenerate_TruncatesLongPrompt(t *testing.T) {
	longText := strings.Repeat("a", 500)

	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		decodeJSONBody(t, r, &body)
		gotText, _ = body["text"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("audio-bytes"))
	}))
	defer server.Close()

	p := NewElevenLabsSFXProvider("key", server.URL)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: longText})
	if err != nil {
		t.Fatalf("AudioGenerate failed: %v", err)
	}
	defer removeFileURL(t, resp.URL)

	if len([]rune(gotText)) != 450 {
		t.Errorf("truncated text length = %d, want 450", len([]rune(gotText)))
	}
}

func TestElevenLabsSFXProvider_AudioGenerate_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
	}))
	defer server.Close()

	p := NewElevenLabsSFXProvider("bad-key", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "boom"})
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want mention of HTTP 401", err)
	}
}

func TestElevenLabsSFXProvider_AudioGenerate_EmptyAudioResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// no body written -> empty audio
	}))
	defer server.Close()

	p := NewElevenLabsSFXProvider("key", server.URL)
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "silence"})
	if err == nil {
		t.Fatal("expected error for empty audio response")
	}
	if !strings.Contains(err.Error(), "empty audio response") {
		t.Errorf("error = %v, want mention of empty audio response", err)
	}
}

func TestElevenLabsSFXProvider_AudioGenerate_NetworkError(t *testing.T) {
	p := NewElevenLabsSFXProvider("key", "http://127.0.0.1:1")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "unreachable"})
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
}

// TestElevenLabsSFXProvider_AudioGenerate_RealCall hits the live ElevenLabs API.
// Requires ELEVENLABS_API_KEY to be set; otherwise skipped.
func TestElevenLabsSFXProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		t.Skip("ELEVENLABS_API_KEY not set; skipping real ElevenLabs SFX API call")
	}

	p := NewElevenLabsSFXProvider(apiKey, os.Getenv("ELEVENLABS_ENDPOINT"))
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:     "a short burst of fireworks",
		Duration: 1.0,
	})
	if err != nil {
		t.Fatalf("AudioGenerate real call failed: %v", err)
	}
	defer removeFileURL(t, resp.URL)

	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
}

// removeFileURL cleans up a temp audio file created by AudioGenerate (file:// URL).
func removeFileURL(t *testing.T, fileURL string) {
	t.Helper()
	if !strings.HasPrefix(fileURL, "file://") {
		return
	}
	_ = os.Remove(strings.TrimPrefix(fileURL, "file://"))
}
