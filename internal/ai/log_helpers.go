package ai

import (
	"fmt"
	"strings"
)

// imgRefTypeLabel classifies a reference-image string for logging purposes without
// ever printing the raw base64 payload.
func imgRefTypeLabel(img string) string {
	switch {
	case strings.HasPrefix(img, "data:"), !strings.HasPrefix(img, "http"):
		return "base64"
	default:
		return "url"
	}
}

// redactBase64Fields returns a shallow copy of params with the named fields replaced by a
// size summary, so request-parameter logs never contain raw base64 image/video/audio payloads.
func redactBase64Fields(params map[string]interface{}, fields ...string) map[string]interface{} {
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
