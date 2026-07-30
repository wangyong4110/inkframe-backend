package ai

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Truncate shortens s to at most n bytes, suitable for including in error messages.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ImgRefTypeLabel classifies a reference-image string for logging purposes without
// ever printing the raw base64 payload.
func ImgRefTypeLabel(img string) string {
	switch {
	case strings.HasPrefix(img, "data:"), !strings.HasPrefix(img, "http"):
		return "base64"
	default:
		return "url"
	}
}

// RedactBase64Fields returns a shallow copy of params with the named fields replaced by a
// size summary, so request-parameter logs never contain raw base64 image/video/audio payloads.
func RedactBase64Fields(params map[string]interface{}, fields ...string) map[string]interface{} {
	redacted := make(map[string]interface{}, len(params))
	for k, v := range params {
		redacted[k] = v
	}
	for _, f := range fields {
		if v, ok := redacted[f]; ok {
			redacted[f] = base64FieldSummary(v)
		}
	}
	return redacted
}

func base64FieldSummary(v interface{}) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("<base64 redacted, %d bytes>", len(val))
	case []string:
		return fmt.Sprintf("<base64 redacted, %d items>", len(val))
	default:
		return "<base64 redacted>"
	}
}

// --- Shared OpenAI-compatible types used across multiple provider sub-packages ---

// OpenAIChatResponse is the standard OpenAI Chat Completions response shape.
type OpenAIChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // DeepSeek-R1 / Doubao thinking models
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// OpenAIStreamChunk is a single chunk from an OpenAI-compatible SSE stream.
type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // Hunyuan Hy3 / DeepSeek-R1 streaming thought
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// OpenAIEmbedResponse is the standard OpenAI Embeddings response shape.
type OpenAIEmbedResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// DALLEResponse is the DALL-E image generation response shape.
type DALLEResponse struct {
	Created int `json:"created"`
	Data    []struct {
		URL       string   `json:"url"`
		B64JSON   string   `json:"b64_json,omitempty"`
		Size      string   `json:"size,omitempty"`
		RevvedURL string   `json:"revised_prompt,omitempty"`
		Error     *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"data"`
}

// SSEReader reads Server-Sent Events from an io.Reader.
type SSEReader struct {
	scanner *bufio.Scanner
}

// SSEEvent is a single SSE event.
type SSEEvent struct {
	Event string
	Data  string
}

// NewSSEReader creates a new SSEReader.
func NewSSEReader(r io.Reader) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	return &SSEReader{scanner: scanner}
}

// Read reads the next SSE event.
func (r *SSEReader) Read() (*SSEEvent, error) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return nil, io.EOF
			}
			return &SSEEvent{Data: data}, nil
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
