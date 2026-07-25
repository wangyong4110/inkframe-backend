package kling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

func TestNewKlingTTSProvider(t *testing.T) {
	t.Run("default endpoint when empty", func(t *testing.T) {
		p := NewKlingTTSProvider("ak", "sk", "")
		if p.accessKey != "ak" || p.secretKey != "sk" {
			t.Errorf("accessKey/secretKey not set: %q %q", p.accessKey, p.secretKey)
		}
		if p.endpoint != klingTTSDefaultEndpoint {
			t.Errorf("endpoint = %q, want %q", p.endpoint, klingTTSDefaultEndpoint)
		}
	})

	t.Run("custom endpoint normalized", func(t *testing.T) {
		p := NewKlingTTSProvider("ak", "sk", "https://custom.example.com/v1/")
		if p.endpoint != "https://custom.example.com" {
			t.Errorf("endpoint = %q, want normalized custom endpoint", p.endpoint)
		}
	})
}

func TestKlingTTSProvider_GetName(t *testing.T) {
	p := NewKlingTTSProvider("ak", "sk", "")
	if got := p.GetName(); got != "kling-tts" {
		t.Errorf("GetName() = %q, want kling-tts", got)
	}
}

func TestKlingTTSProvider_GetModels(t *testing.T) {
	p := NewKlingTTSProvider("ak", "sk", "")
	models := p.GetModels()
	if len(models) == 0 {
		t.Fatal("expected non-empty model list")
	}
	found := false
	for _, m := range models {
		if m == "zh_female_story" {
			found = true
		}
	}
	if !found {
		t.Error("expected zh_female_story in model list")
	}
}

func TestKlingTTSProvider_HealthCheck(t *testing.T) {
	tests := []struct {
		name      string
		accessKey string
		secretKey string
		wantErr   bool
	}{
		{"both empty", "", "", true},
		{"missing secret", "ak", "", true},
		{"missing access", "", "sk", true},
		{"both present", "ak", "sk", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewKlingTTSProvider(tt.accessKey, tt.secretKey, "")
			err := p.HealthCheck(context.Background())
			if tt.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestKlingTTSProvider_UnsupportedMethods(t *testing.T) {
	p := NewKlingTTSProvider("ak", "sk", "")
	ctx := context.Background()
	if _, err := p.Generate(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("Generate should error")
	}
	if _, err := p.GenerateStream(ctx, &ai.GenerateRequest{}); err == nil {
		t.Error("GenerateStream should error")
	}
	if _, err := p.Embed(ctx, "x"); err == nil {
		t.Error("Embed should error")
	}
	if _, err := p.ImageGenerate(ctx, &ai.ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate should error")
	}
}

func TestKlingTTSProvider_AudioGenerate_Validation(t *testing.T) {
	p := NewKlingTTSProvider("ak", "sk", "")

	t.Run("empty text errors", func(t *testing.T) {
		_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Voice: "zh_female_story"})
		if err == nil {
			t.Fatal("expected error for empty text")
		}
		if !strings.Contains(err.Error(), "text is required") {
			t.Errorf("error = %q, want mention of text is required", err.Error())
		}
	})

	t.Run("empty voice errors", func(t *testing.T) {
		_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
		if err == nil {
			t.Fatal("expected error for empty voice")
		}
		if !strings.Contains(err.Error(), "未指定音色") {
			t.Errorf("error = %q, want mention of 未指定音色", err.Error())
		}
	})
}

// TestKlingTTSProvider_AudioGenerate_EndToEndFake spins up a local httptest.Server that
// emulates the Kling submit + poll task flow, and points the provider at it via a custom
// endpoint. This exercises submitTask, pollUntilDone, queryTask, and AudioGenerate's happy
// path without any real network access or real credentials.
func TestKlingTTSProvider_AudioGenerate_EndToEndFake(t *testing.T) {
	var pollCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/tts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"code":    0,
			"message": "ok",
			"data":    map[string]string{"task_id": "task-123"},
		})
	})
	mux.HandleFunc("/v1/audio/tts/task-123", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		w.Header().Set("Content-Type", "application/json")
		status := "processing"
		if pollCount >= 2 {
			status = "succeed"
		}
		resp := map[string]interface{}{
			"code":    0,
			"message": "ok",
			"data": map[string]interface{}{
				"task_id":     "task-123",
				"task_status": status,
			},
		}
		if status == "succeed" {
			resp["data"].(map[string]interface{})["task_result"] = map[string]interface{}{
				"audios": []map[string]string{
					{"id": "a1", "url": "https://example.com/audio.mp3", "duration": "3.5"},
				},
			}
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewKlingTTSProvider("ak", "sk", server.URL)
	// klingTTSPollInterval (2s) is a const and cannot be overridden for the test;
	// the fake server returns "succeed" on the 2nd poll, so this takes ~2-4s.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:  "hello world",
		Voice: "zh_female_story",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL != "https://example.com/audio.mp3" {
		t.Errorf("URL = %q, want https://example.com/audio.mp3", resp.URL)
	}
	if resp.Duration != 3.5 {
		t.Errorf("Duration = %v, want 3.5", resp.Duration)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
}

// TestKlingTTSProvider_AudioGenerate_TaskFailed verifies that a "failed" task status
// surfaces the task_status_msg as the returned error.
func TestKlingTTSProvider_AudioGenerate_TaskFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"code": 0, "message": "ok",
			"data": map[string]string{"task_id": "task-fail"},
		})
	})
	mux.HandleFunc("/v1/audio/tts/task-fail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"code": 0, "message": "ok",
			"data": map[string]interface{}{
				"task_id": "task-fail", "task_status": "failed", "task_status_msg": "content violation",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewKlingTTSProvider("ak", "sk", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := p.AudioGenerate(ctx, &ai.AudioGenerateRequest{Text: "hello", Voice: "zh_female_story"})
	if err == nil {
		t.Fatal("expected error for failed task")
	}
	if !strings.Contains(err.Error(), "content violation") {
		t.Errorf("error = %q, want mention of content violation", err.Error())
	}
}

// TestKlingTTSProvider_submitTask_APIError verifies that a non-zero API error code from
// the submit endpoint is surfaced with its message.
func TestKlingTTSProvider_submitTask_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audio/tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"code": 1002, "message": "invalid parameter",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewKlingTTSProvider("ak", "sk", server.URL)
	_, err := p.submitTask(context.Background(), "hello", "zh_female_story", "zh", 1.0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid parameter") || !strings.Contains(err.Error(), "1002") {
		t.Errorf("error = %q, want mention of code 1002 and message", err.Error())
	}
}

// TestKlingTTSProvider_AudioGenerate_RealCall makes a real call to the Kling TTS API.
// Skipped unless KLING_ACCESS_KEY and KLING_SECRET_KEY are set. Note: this test can take
// up to klingTTSMaxWait (5 minutes) to complete since it polls a real async task.
func TestKlingTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	accessKey := os.Getenv("KLING_ACCESS_KEY")
	secretKey := os.Getenv("KLING_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("KLING_ACCESS_KEY / KLING_SECRET_KEY not configured")
	}
	endpoint := os.Getenv("KLING_ENDPOINT")

	p := NewKlingTTSProvider(accessKey, secretKey, endpoint)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "zh_female_story",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
}
