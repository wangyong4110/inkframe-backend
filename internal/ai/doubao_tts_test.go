package ai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ── DoubaoSpeechProvider (V3) ──

func TestNewDoubaoSpeechProvider(t *testing.T) {
	t.Run("default resourceID when empty", func(t *testing.T) {
		p := NewDoubaoSpeechProvider("key", "")
		if p.apiKey != "key" {
			t.Errorf("apiKey = %q, want key", p.apiKey)
		}
		if p.resourceID != "seed-tts-2.0" {
			t.Errorf("resourceID = %q, want seed-tts-2.0", p.resourceID)
		}
	})

	t.Run("custom resourceID preserved", func(t *testing.T) {
		p := NewDoubaoSpeechProvider("key", "seed-tts-1.0")
		if p.resourceID != "seed-tts-1.0" {
			t.Errorf("resourceID = %q, want seed-tts-1.0", p.resourceID)
		}
	})
}

func TestDoubaoSpeechProvider_GetName(t *testing.T) {
	p := NewDoubaoSpeechProvider("key", "")
	if got := p.GetName(); got != "doubao-speech" {
		t.Errorf("GetName() = %q, want doubao-speech", got)
	}
}

func TestDoubaoSpeechProvider_GetModels(t *testing.T) {
	p := NewDoubaoSpeechProvider("key", "")
	models := p.GetModels()
	want := []string{"seed-tts-2.0", "seed-icl-2.0", "seed-tts-1.0"}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d", len(models), len(want))
	}
	for i, w := range want {
		if models[i] != w {
			t.Errorf("models[%d] = %q, want %q", i, models[i], w)
		}
	}
}

func TestDoubaoSpeechProvider_HealthCheck(t *testing.T) {
	if err := NewDoubaoSpeechProvider("", "").HealthCheck(context.Background()); err == nil {
		t.Error("expected error when apiKey empty")
	}
	if err := NewDoubaoSpeechProvider("key", "").HealthCheck(context.Background()); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDoubaoSpeechProvider_UnsupportedMethods(t *testing.T) {
	p := NewDoubaoSpeechProvider("key", "")
	ctx := context.Background()
	if _, err := p.Generate(ctx, &GenerateRequest{}); err == nil {
		t.Error("Generate should error")
	}
	if _, err := p.GenerateStream(ctx, &GenerateRequest{}); err == nil {
		t.Error("GenerateStream should error")
	}
	if _, err := p.Embed(ctx, "x"); err == nil {
		t.Error("Embed should error")
	}
	if _, err := p.ImageGenerate(ctx, &ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate should error")
	}
}

func TestDoubaoSpeechProvider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewDoubaoSpeechProvider("key", "")
	_, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error = %q, want mention of 未指定音色", err.Error())
	}
}

func TestDoubaoSpeechProvider_buildDoubaoSpeechBody(t *testing.T) {
	p := NewDoubaoSpeechProvider("key", "")

	t.Run("basic fields", func(t *testing.T) {
		body, err := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi"}, "speaker1", "seed-tts-2.0")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		reqParams, ok := parsed["req_params"].(map[string]interface{})
		if !ok {
			t.Fatal("req_params missing or wrong type")
		}
		if reqParams["text"] != "hi" {
			t.Errorf("text = %v, want hi", reqParams["text"])
		}
		if reqParams["speaker"] != "speaker1" {
			t.Errorf("speaker = %v, want speaker1", reqParams["speaker"])
		}
		if reqParams["model"] != "seed-tts-2.0-standard" {
			t.Errorf("model = %v, want seed-tts-2.0-standard (no emotion set)", reqParams["model"])
		}
	})

	t.Run("emotion selects expressive submodel and context_texts", func(t *testing.T) {
		body, err := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi", Emotion: "happy"}, "speaker1", "seed-tts-2.0")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		if reqParams["model"] != "seed-tts-2.0-expressive" {
			t.Errorf("model = %v, want seed-tts-2.0-expressive", reqParams["model"])
		}
		contextTexts, ok := reqParams["context_texts"].([]interface{})
		if !ok || len(contextTexts) != 1 {
			t.Fatalf("context_texts = %v, want single-element array", reqParams["context_texts"])
		}
		if contextTexts[0] != "请用快乐的语气说话" {
			t.Errorf("context_texts[0] = %v, want 请用快乐的语气说话", contextTexts[0])
		}
	})

	t.Run("doubao-character-tts skips model field", func(t *testing.T) {
		body, err := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi", Emotion: "happy"}, "speaker1", "doubao-character-tts")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		if _, exists := reqParams["model"]; exists {
			t.Errorf("model field should be omitted for doubao-character-tts, got %v", reqParams["model"])
		}
	})

	t.Run("speed maps to speech_rate clamped range", func(t *testing.T) {
		body, _ := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi", Speed: 3.0}, "speaker1", "seed-tts-2.0")
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		audioParams := reqParams["audio_params"].(map[string]interface{})
		// Speed 3.0 -> (3.0-1.0)*100 = 200, clamped to 100
		if audioParams["speech_rate"] != float64(100) {
			t.Errorf("speech_rate = %v, want 100 (clamped)", audioParams["speech_rate"])
		}
	})

	t.Run("pitch clamped to -12..12", func(t *testing.T) {
		body, _ := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi", Pitch: 50}, "speaker1", "seed-tts-2.0")
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		postProcess := reqParams["post_process"].(map[string]interface{})
		if postProcess["pitch"] != float64(12) {
			t.Errorf("pitch = %v, want 12 (clamped)", postProcess["pitch"])
		}
	})

	t.Run("additions fields marshaled to JSON string", func(t *testing.T) {
		body, _ := p.buildDoubaoSpeechBody(&AudioGenerateRequest{
			Text: "hi", Language: "en", Dialect: "zh-yue", SilenceDuration: 500, DisableMarkdown: true,
		}, "speaker1", "seed-tts-2.0")
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		additionsStr, ok := reqParams["additions"].(string)
		if !ok {
			t.Fatalf("additions should be a JSON string, got %T", reqParams["additions"])
		}
		var additions map[string]interface{}
		if err := json.Unmarshal([]byte(additionsStr), &additions); err != nil {
			t.Fatalf("additions is not valid JSON: %v", err)
		}
		if additions["explicit_language"] != "en" {
			t.Errorf("explicit_language = %v, want en", additions["explicit_language"])
		}
		if additions["explicit_dialect"] != "zh-yue" {
			t.Errorf("explicit_dialect = %v, want zh-yue", additions["explicit_dialect"])
		}
		if additions["silence_duration"] != float64(500) {
			t.Errorf("silence_duration = %v, want 500", additions["silence_duration"])
		}
		if additions["disable_markdown_filter"] != true {
			t.Errorf("disable_markdown_filter = %v, want true", additions["disable_markdown_filter"])
		}
	})

	t.Run("section_id passthrough", func(t *testing.T) {
		body, _ := p.buildDoubaoSpeechBody(&AudioGenerateRequest{Text: "hi", SectionID: "sec-1"}, "speaker1", "seed-tts-2.0")
		var parsed map[string]interface{}
		json.Unmarshal(body, &parsed) //nolint:errcheck
		reqParams := parsed["req_params"].(map[string]interface{})
		if reqParams["section_id"] != "sec-1" {
			t.Errorf("section_id = %v, want sec-1", reqParams["section_id"])
		}
	})
}

func Test_doubaoResourceErr(t *testing.T) {
	err := &doubaoResourceErr{msg: "resource mismatch"}
	if err.Error() != "resource mismatch" {
		t.Errorf("Error() = %q, want %q", err.Error(), "resource mismatch")
	}
}

// ── DoubaoSpeechV1Provider ──

func TestNewDoubaoSpeechV1Provider(t *testing.T) {
	t.Run("default cluster when empty", func(t *testing.T) {
		p := NewDoubaoSpeechV1Provider("app1", "token1", "")
		if p.appID != "app1" || p.accessToken != "token1" {
			t.Errorf("appID/accessToken not set correctly: %q %q", p.appID, p.accessToken)
		}
		if p.cluster != "volcano_tts" {
			t.Errorf("cluster = %q, want volcano_tts", p.cluster)
		}
	})

	t.Run("custom cluster preserved", func(t *testing.T) {
		p := NewDoubaoSpeechV1Provider("app1", "token1", "volcano_mega")
		if p.cluster != "volcano_mega" {
			t.Errorf("cluster = %q, want volcano_mega", p.cluster)
		}
	})
}

func TestDoubaoSpeechV1Provider_GetName(t *testing.T) {
	p := NewDoubaoSpeechV1Provider("a", "b", "")
	if got := p.GetName(); got != "doubao-speech-v1" {
		t.Errorf("GetName() = %q, want doubao-speech-v1", got)
	}
}

func TestDoubaoSpeechV1Provider_GetModels(t *testing.T) {
	p := NewDoubaoSpeechV1Provider("a", "b", "")
	models := p.GetModels()
	if len(models) == 0 {
		t.Fatal("expected non-empty model list")
	}
	found := false
	for _, m := range models {
		if m == "BV001_streaming" {
			found = true
		}
	}
	if !found {
		t.Error("expected BV001_streaming in model list")
	}
}

func TestDoubaoSpeechV1Provider_HealthCheck(t *testing.T) {
	tests := []struct {
		name        string
		appID       string
		accessToken string
		wantErr     bool
	}{
		{"both empty", "", "", true},
		{"missing token", "app", "", true},
		{"missing appid", "", "token", true},
		{"both present", "app", "token", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewDoubaoSpeechV1Provider(tt.appID, tt.accessToken, "")
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

func TestDoubaoSpeechV1Provider_UnsupportedMethods(t *testing.T) {
	p := NewDoubaoSpeechV1Provider("a", "b", "")
	ctx := context.Background()
	if _, err := p.Generate(ctx, &GenerateRequest{}); err == nil {
		t.Error("Generate should error")
	}
	if _, err := p.GenerateStream(ctx, &GenerateRequest{}); err == nil {
		t.Error("GenerateStream should error")
	}
	if _, err := p.Embed(ctx, "x"); err == nil {
		t.Error("Embed should error")
	}
	if _, err := p.ImageGenerate(ctx, &ImageGenerateRequest{}); err == nil {
		t.Error("ImageGenerate should error")
	}
}

func TestDoubaoSpeechV1Provider_AudioGenerate_MissingVoice(t *testing.T) {
	p := NewDoubaoSpeechV1Provider("a", "b", "")
	_, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error when voice is empty")
	}
	if !strings.Contains(err.Error(), "未指定音色") {
		t.Errorf("error = %q, want mention of 未指定音色", err.Error())
	}
}

// ── Pure helper functions ──

func Test_normalizeDoubaoV1Emotion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"happy", "happy"},
		{"cheerful", "happy"},
		{"excited", "happy"},
		{"sad", "sad"},
		{"angry", "angry"},
		{"fear", "fear"},
		{"fearful", "fear"},
		{"calm", "neutral"},
		{"neutral", "neutral"},
		{"serious", "neutral"},
		{"HAPPY", "happy"},
		{"surprised", ""}, // not supported by V1
		{"unknown", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeDoubaoV1Emotion(tt.in); got != tt.want {
			t.Errorf("normalizeDoubaoV1Emotion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func Test_emotionToChineseLabel(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"happy", "快乐"},
		{"HAPPY", "快乐"},
		{"cheerful", "欢快"},
		{"excited", "兴奋"},
		{"sad", "悲伤"},
		{"angry", "愤怒"},
		{"fear", "恐惧"},
		{"fearful", "恐惧"},
		{"calm", "平静"},
		{"neutral", "平静"},
		{"serious", "严肃"},
		{"surprised", "惊讶"},
		{"已经是中文", "已经是中文"},
	}
	for _, tt := range tests {
		if got := emotionToChineseLabel(tt.in); got != tt.want {
			t.Errorf("emotionToChineseLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── Real network calls (skipped without credentials) ──

func TestDoubaoSpeechProvider_AudioGenerate_RealCall(t *testing.T) {
	apiKey := os.Getenv("DOUBAO_TTS_API_KEY")
	if apiKey == "" {
		t.Skip("DOUBAO_TTS_API_KEY not configured")
	}
	resourceID := os.Getenv("DOUBAO_TTS_RESOURCE_ID")
	voice := os.Getenv("DOUBAO_TTS_VOICE")
	if voice == "" {
		t.Skip("DOUBAO_TTS_VOICE not configured (no safe default speaker known)")
	}

	p := NewDoubaoSpeechProvider(apiKey, resourceID)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: voice,
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if strings.HasPrefix(resp.URL, "file://") {
		os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	}
}

func TestDoubaoSpeechV1Provider_AudioGenerate_RealCall(t *testing.T) {
	appID := os.Getenv("DOUBAO_TTS_V1_APP_ID")
	accessToken := os.Getenv("DOUBAO_TTS_V1_ACCESS_TOKEN")
	if appID == "" || accessToken == "" {
		t.Skip("DOUBAO_TTS_V1_APP_ID / DOUBAO_TTS_V1_ACCESS_TOKEN not configured")
	}
	cluster := os.Getenv("DOUBAO_TTS_V1_CLUSTER")

	p := NewDoubaoSpeechV1Provider(appID, accessToken, cluster)
	resp, err := p.AudioGenerate(context.Background(), &AudioGenerateRequest{
		Text:  "你好，世界",
		Voice: "BV001_streaming",
	})
	if err != nil {
		t.Fatalf("AudioGenerate: %v", err)
	}
	if resp.URL == "" {
		t.Error("expected non-empty audio URL")
	}
	if strings.HasPrefix(resp.URL, "file://") {
		os.Remove(strings.TrimPrefix(resp.URL, "file://"))
	}
}
