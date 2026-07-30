package doubao

import (
"github.com/inkframe/inkframe-backend/internal/ai"
	"context"
	"os"
	"strings"
	"testing"
)

// TestDoubaoProvider_EmbedMultimodal_UnsupportedInputType verifies the input-type
// validation in EmbedMultimodal fails fast (before any network call / SDK client
// construction) for a type outside {text, image_url, video_url}. This is the only
// part of EmbedMultimodal that's deterministically testable without real Volcengine
// Ark credentials, since the method builds its own SDK HTTP client internally rather
// than accepting an injectable one.
func TestDoubaoProvider_EmbedMultimodal_UnsupportedInputType(t *testing.T) {
	p := NewDoubaoProvider("key", "https://ark.volces.com/api/v3", "", 0)
	_, err := p.EmbedMultimodal(context.Background(), &ai.MultimodalEmbedRequest{
		Input: []ai.MultimodalEmbedItem{{Type: "audio_url"}},
	})
	if err == nil {
		t.Fatal("expected error for unsupported input type")
	}
	if !strings.Contains(err.Error(), "audio_url") {
		t.Errorf("error = %v, want mention of unsupported type", err)
	}
}

// TestDoubaoProvider_ImplementsMultimodalEmbedder confirms DoubaoProvider satisfies the
// optional MultimodalEmbedder interface, as documented in doubao_multimodal_embed.go's
// type-assertion usage example.
func TestDoubaoProvider_ImplementsMultimodalEmbedder(t *testing.T) {
	var p ai.AIProvider = NewDoubaoProvider("key", "", "", 0)
	if _, ok := p.(ai.MultimodalEmbedder); !ok {
		t.Fatal("DoubaoProvider does not implement ai.MultimodalEmbedder")
	}
}

// TestDoubaoProvider_EmbedMultimodal_RealCall exercises a real Volcengine Ark multimodal
// embedding call end-to-end. Requires DOUBAO_API_KEY (and optionally DOUBAO_ENDPOINT /
// DOUBAO_EMBED_MODEL) to be set; skipped otherwise since no credentials are available
// in this environment.
func TestDoubaoProvider_EmbedMultimodal_RealCall(t *testing.T) {
	apiKey := os.Getenv("DOUBAO_API_KEY")
	if apiKey == "" {
		t.Skip("DOUBAO_API_KEY not configured")
	}
	endpoint := os.Getenv("DOUBAO_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://ark.volces.com/api/v3"
	}
	model := os.Getenv("DOUBAO_EMBED_MODEL") // e.g. doubao-embedding-vision-250328

	p := NewDoubaoProvider(apiKey, endpoint, "", 0)
	resp, err := p.EmbedMultimodal(context.Background(), &ai.MultimodalEmbedRequest{
		Model: model,
		Input: []ai.MultimodalEmbedItem{{Type: "text", Text: "hello world"}},
	})
	if err != nil {
		t.Fatalf("EmbedMultimodal() error: %v", err)
	}
	if len(resp.Embedding) == 0 {
		t.Error("expected non-empty embedding vector")
	}
}

var _ ai.MultimodalEmbedder = (*DoubaoProvider)(nil)
