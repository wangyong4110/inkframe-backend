package minimax

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

func TestNewMinimaxTTSProvider(t *testing.T) {
	p := NewMinimaxTTSProvider("key", "group1")
	if p.apiKey != "key" {
		t.Errorf("apiKey = %q, want key", p.apiKey)
	}
	if p.groupID != "group1" {
		t.Errorf("groupID = %q, want group1", p.groupID)
	}
	if p.client == nil {
		t.Fatal("client should not be nil")
	}
}

func TestMinimaxTTSProvider_GetName(t *testing.T) {
	p := NewMinimaxTTSProvider("key", "group1")
	if got := p.GetName(); got != "minimax-tts" {
		t.Errorf("GetName() = %q, want minimax-tts", got)
	}
}

func TestMinimaxTTSProvider_GetModels(t *testing.T) {
	p := NewMinimaxTTSProvider("key", "group1")
	models := p.GetModels()
	want := []string{
		"female-shaonv", "female-yujie", "female-tianmei", "female-qingxin",
		"male-qn-qingse", "male-qn-jingying", "male-qn-badao", "male-qn-daxuesheng",
		"presenter_male", "presenter_female", "audiobook_male_1", "audiobook_male_2",
		"audiobook_female_1", "audiobook_female_2", "male-story", "female-story",
	}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d", len(models), len(want))
	}
	for i, w := range want {
		if models[i] != w {
			t.Errorf("models[%d] = %q, want %q", i, models[i], w)
		}
	}
}

func TestMinimaxTTSProvider_HealthCheck(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		groupID string
		wantErr bool
	}{
		{"both empty", "", "", true},
		{"missing group", "key", "", true},
		{"missing key", "", "group1", true},
		{"both present", "key", "group1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewMinimaxTTSProvider(tt.apiKey, tt.groupID)
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

func TestMinimaxTTSProvider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewMinimaxTTSProvider("key", "group1")
	_, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error = %q, want mention of 未指定音色", err.Error())
	}
}

func Test_normalizeMinimaxEmotion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"happy", "happy"},
		{"cheerful", "happy"},
		{"excited", "happy"},
		{"sad", "sad"},
		{"angry", "angry"},
		{"fear", "fearful"},
		{"fearful", "fearful"},
		{"surprised", "surprised"},
		{"calm", "neutral"},
		{"neutral", "neutral"},
		{"serious", "neutral"},
		{"HAPPY", "happy"},
		{"unknown", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeMinimaxEmotion(tt.in); got != tt.want {
			t.Errorf("normalizeMinimaxEmotion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func Test_parseMinimaxSSE(t *testing.T) {
	t.Run("accumulates audio chunks until status 2", func(t *testing.T) {
		sse := "data: {\"data\":{\"audio\":\"aabb\",\"status\":1}}\n" +
			"data: {\"data\":{\"audio\":\"ccdd\",\"status\":2}}\n"
		got, err := parseMinimaxSSE(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "aabbccdd" {
			t.Errorf("got %q, want aabbccdd", got)
		}
	})

	t.Run("skips DONE and blank lines", func(t *testing.T) {
		sse := "data: {\"data\":{\"audio\":\"aa\",\"status\":1}}\n" +
			"\n" +
			"data: [DONE]\n" +
			"data: {\"data\":{\"audio\":\"bb\",\"status\":2}}\n"
		got, err := parseMinimaxSSE(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "aabb" {
			t.Errorf("got %q, want aabb", got)
		}
	})

	t.Run("ignores non-data lines", func(t *testing.T) {
		sse := "event: message\n" +
			"data: {\"data\":{\"audio\":\"ff\",\"status\":2}}\n"
		got, err := parseMinimaxSSE(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "ff" {
			t.Errorf("got %q, want ff", got)
		}
	})

	t.Run("returns error on non-zero base_resp status_code", func(t *testing.T) {
		sse := "data: {\"base_resp\":{\"status_code\":1004,\"status_msg\":\"auth failed\"}}\n"
		_, err := parseMinimaxSSE(strings.NewReader(sse))
		if err == nil {
			t.Fatal("expected error for non-zero status_code")
		}
		if !strings.Contains(err.Error(), "1004") || !strings.Contains(err.Error(), "auth failed") {
			t.Errorf("error = %q, want it to mention code and message", err.Error())
		}
	})

	t.Run("malformed JSON lines are skipped, not fatal", func(t *testing.T) {
		sse := "data: not-json\n" +
			"data: {\"data\":{\"audio\":\"aa\",\"status\":2}}\n"
		got, err := parseMinimaxSSE(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "aa" {
			t.Errorf("got %q, want aa", got)
		}
	})

	t.Run("empty stream returns empty string", func(t *testing.T) {
		got, err := parseMinimaxSSE(strings.NewReader(""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestMinimaxTTSProvider_AudioGenerate_RealCall makes a real call to the MiniMax T2A V2
// API. Skipped unless MINIMAX_TTS_API_KEY and MINIMAX_TTS_GROUP_ID are set.
func TestMinimaxTTSProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("MINIMAX_TTS_API_KEY")
	groupID := os.Getenv("MINIMAX_TTS_GROUP_ID")
	if apiKey == "" || groupID == "" {
		t.Skip("MINIMAX_TTS_API_KEY / MINIMAX_TTS_GROUP_ID not configured")
	}

	p := NewMinimaxTTSProvider(apiKey, groupID)
	resp, err := p.AudioGenerate(context.Background(), &ai.AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "female-shaonv",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" || !strings.HasPrefix(resp.URL, "file://") {
		t.Errorf("expected file:// URL, got %q", resp.URL)
	}
	if resp.Format != "mp3" {
		t.Errorf("Format = %q, want mp3", resp.Format)
	}
	if strings.HasPrefix(resp.URL, "file://") {
		os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	}
}
