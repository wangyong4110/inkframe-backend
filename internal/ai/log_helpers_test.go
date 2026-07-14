package ai

import (
	"strings"
	"testing"
)

func TestImgRefTypeLabel(t *testing.T) {
	tests := []struct {
		name string
		img  string
		want string
	}{
		{"data URI", "data:image/png;base64,iVBORw0KGgo=", "base64"},
		{"raw base64 no scheme", "iVBORw0KGgoAAAANSUhEUgAA", "base64"},
		{"empty string", "", "base64"},
		{"http URL", "http://example.com/image.png", "url"},
		{"https URL", "https://example.com/image.png", "url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imgRefTypeLabel(tt.img); got != tt.want {
				t.Errorf("imgRefTypeLabel(%q) = %q, want %q", tt.img, got, tt.want)
			}
		})
	}
}

func TestRedactBase64Fields_RedactsNamedFieldsOnly(t *testing.T) {
	params := map[string]interface{}{
		"reference_image": "aGVsbG8gd29ybGQ=", // 16 bytes
		"prompt":          "a cat on a rooftop",
		"seed":            int64(42),
	}
	redacted := redactBase64Fields(params, "reference_image")

	if redacted["prompt"] != "a cat on a rooftop" {
		t.Errorf("expected untouched field 'prompt' to be unchanged, got %v", redacted["prompt"])
	}
	if redacted["seed"] != int64(42) {
		t.Errorf("expected untouched field 'seed' to be unchanged, got %v", redacted["seed"])
	}
	got, ok := redacted["reference_image"].(string)
	if !ok || !strings.Contains(got, "base64 redacted") {
		t.Errorf("expected reference_image to be redacted, got %v", redacted["reference_image"])
	}
	if strings.Contains(got, "aGVsbG8gd29ybGQ=") {
		t.Errorf("redacted value must not contain raw payload, got %v", got)
	}
}

func TestRedactBase64Fields_DoesNotMutateOriginalMap(t *testing.T) {
	params := map[string]interface{}{
		"reference_image": "original-payload",
	}
	_ = redactBase64Fields(params, "reference_image")
	if params["reference_image"] != "original-payload" {
		t.Errorf("expected original map to be unmodified, got %v", params["reference_image"])
	}
}

func TestRedactBase64Fields_MissingFieldIsNoop(t *testing.T) {
	params := map[string]interface{}{
		"prompt": "hello",
	}
	redacted := redactBase64Fields(params, "reference_image")
	if _, ok := redacted["reference_image"]; ok {
		t.Error("expected no reference_image key to be added when absent from input")
	}
	if len(redacted) != 1 {
		t.Errorf("expected redacted map to have same size as input, got %d entries", len(redacted))
	}
}

func TestRedactBase64Fields_StringSliceField(t *testing.T) {
	params := map[string]interface{}{
		"reference_images": []string{"img1base64", "img2base64", "img3base64"},
	}
	redacted := redactBase64Fields(params, "reference_images")
	got, ok := redacted["reference_images"].(string)
	if !ok {
		t.Fatalf("expected redacted reference_images to be a string summary, got %T", redacted["reference_images"])
	}
	if !strings.Contains(got, "3 items") {
		t.Errorf("expected summary to mention item count, got %q", got)
	}
}

func TestRedactBase64Fields_OtherTypeField(t *testing.T) {
	params := map[string]interface{}{
		"weird_field": 12345,
	}
	redacted := redactBase64Fields(params, "weird_field")
	got, ok := redacted["weird_field"].(string)
	if !ok || !strings.Contains(got, "base64 redacted") {
		t.Errorf("expected generic redaction message for non-string/non-[]string type, got %v", redacted["weird_field"])
	}
}

func TestRedactBase64Fields_MultipleFieldsAtOnce(t *testing.T) {
	params := map[string]interface{}{
		"reference_image": "payload-a",
		"reference_video":  "payload-b",
		"prompt":           "untouched",
	}
	redacted := redactBase64Fields(params, "reference_image", "reference_video")
	for _, f := range []string{"reference_image", "reference_video"} {
		got, ok := redacted[f].(string)
		if !ok || !strings.Contains(got, "redacted") {
			t.Errorf("expected field %q to be redacted, got %v", f, redacted[f])
		}
	}
	if redacted["prompt"] != "untouched" {
		t.Errorf("expected unrelated field to be unchanged, got %v", redacted["prompt"])
	}
}
