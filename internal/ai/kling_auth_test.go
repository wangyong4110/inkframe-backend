package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestNormalizeKlingEndpoint(t *testing.T) {
	cases := []struct {
		name            string
		endpoint        string
		defaultEndpoint string
		want            string
	}{
		{"empty uses default", "", "https://api-beijing.klingai.com", "https://api-beijing.klingai.com"},
		{"trims trailing slash", "https://api-beijing.klingai.com/", "https://default", "https://api-beijing.klingai.com"},
		{"trims trailing v1", "https://api-beijing.klingai.com/v1", "https://default", "https://api-beijing.klingai.com"},
		{"trims trailing v1 and slash", "https://api-beijing.klingai.com/v1/", "https://default", "https://api-beijing.klingai.com"},
		{"no change needed", "https://custom.endpoint.com", "https://default", "https://custom.endpoint.com"},
		{"only trims one v1 suffix", "https://host/v1/v1", "https://default", "https://host/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeKlingEndpoint(tc.endpoint, tc.defaultEndpoint)
			if got != tc.want {
				t.Errorf("normalizeKlingEndpoint(%q, %q) = %q, want %q", tc.endpoint, tc.defaultEndpoint, got, tc.want)
			}
		})
	}
}

func TestKlingJWT(t *testing.T) {
	accessKey := "test-access-key"
	secretKey := "test-secret-key"

	tokenStr, err := klingJWT(accessKey, secretKey)
	if err != nil {
		t.Fatalf("klingJWT returned error: %v", err)
	}
	if tokenStr == "" {
		t.Fatal("klingJWT returned empty token")
	}
	// A JWT has three dot-separated segments.
	if parts := strings.Split(tokenStr, "."); len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d (%q)", len(parts), tokenStr)
	}

	// Parse and validate claims/signature using the same secret.
	parsed, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			t.Fatalf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secretKey), nil
	})
	if err != nil {
		t.Fatalf("failed to parse/verify generated JWT: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("generated JWT is not valid")
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("unexpected claims type: %T", parsed.Claims)
	}

	iss, _ := claims["iss"].(string)
	if iss != accessKey {
		t.Errorf("iss claim = %q, want %q", iss, accessKey)
	}

	expFloat, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("exp claim missing or wrong type: %v", claims["exp"])
	}
	nbfFloat, ok := claims["nbf"].(float64)
	if !ok {
		t.Fatalf("nbf claim missing or wrong type: %v", claims["nbf"])
	}

	now := time.Now()
	exp := time.Unix(int64(expFloat), 0)
	nbf := time.Unix(int64(nbfFloat), 0)

	// exp should be ~30 minutes from now (allow some slack for test execution time).
	wantExp := now.Add(30 * time.Minute)
	if diff := exp.Sub(wantExp); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("exp = %v, want approx %v (diff %v)", exp, wantExp, diff)
	}
	// nbf should be ~5 seconds in the past.
	wantNbf := now.Add(-5 * time.Second)
	if diff := nbf.Sub(wantNbf); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("nbf = %v, want approx %v (diff %v)", nbf, wantNbf, diff)
	}
}

func TestKlingJWT_DifferentSecretsProduceDifferentSignatures(t *testing.T) {
	t1, err := klingJWT("ak", "secret-a")
	if err != nil {
		t.Fatalf("klingJWT: %v", err)
	}
	t2, err := klingJWT("ak", "secret-b")
	if err != nil {
		t.Fatalf("klingJWT: %v", err)
	}
	if t1 == t2 {
		t.Error("expected different tokens for different secret keys")
	}

	// Verify cross-validation fails: token signed with secret-a should not
	// validate against secret-b.
	_, err = jwt.Parse(t1, func(token *jwt.Token) (interface{}, error) {
		return []byte("secret-b"), nil
	})
	if err == nil {
		t.Error("expected verification failure when using the wrong secret")
	}
}

// TestKlingDoRequest_SendsAuthHeaderAndBody verifies klingDoRequest builds the
// HTTP request correctly (method, path, JSON body, Bearer JWT header) against
// a local httptest server — no real network / credentials required.
func TestKlingDoRequest_SendsAuthHeaderAndBody(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	reqBody := map[string]interface{}{"prompt": "hello world"}

	respBody, status, err := klingDoRequest(context.Background(), "my-access-key", "my-secret-key", server.URL, client, "POST", "/v1/videos/image2video", reqBody)
	if err != nil {
		t.Fatalf("klingDoRequest error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if len(respBody) == 0 {
		t.Error("expected non-empty response body")
	}

	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/videos/image2video" {
		t.Errorf("path = %q, want /v1/videos/image2video", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("Authorization header = %q, want Bearer prefix", gotAuth)
	}

	// Validate the embedded JWT was signed with the secret we passed in.
	tokenStr := strings.TrimPrefix(gotAuth, "Bearer ")
	parsed, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		return []byte("my-secret-key"), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("failed to verify JWT sent in Authorization header: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != "my-access-key" {
		t.Errorf("iss claim = %v, want my-access-key", claims["iss"])
	}

	if gotBody["prompt"] != "hello world" {
		t.Errorf("request body prompt = %v, want %q", gotBody["prompt"], "hello world")
	}
}

// TestKlingDoRequest_NilBody verifies a nil body is sent as an empty request
// without error (GET-style requests).
func TestKlingDoRequest_NilBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, status, err := klingDoRequest(context.Background(), "ak", "sk", server.URL, client, "GET", "/v1/videos/image2video/abc", nil)
	if err != nil {
		t.Fatalf("klingDoRequest error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}

// TestKlingDoRequest_NetworkError verifies transport-level errors (e.g.
// unreachable host) are propagated.
func TestKlingDoRequest_NetworkError(t *testing.T) {
	client := &http.Client{Timeout: 1 * time.Second}
	_, _, err := klingDoRequest(context.Background(), "ak", "sk", "http://127.0.0.1:1", client, "GET", "/x", nil)
	if err == nil {
		t.Fatal("expected error for unreachable endpoint, got nil")
	}
}
