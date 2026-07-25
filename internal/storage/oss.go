package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ─── OSS (Aliyun Object Storage, V1 signature) ──────────────────────────────

type ossService struct {
	cfg    Config
	client *http.Client
}

func newOSSService(cfg Config) *ossService {
	return &ossService{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}
}

const maxUploadSize = 2 * 1024 * 1024 * 1024 // 2 GB

func (s *ossService) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("storage: upload size must be > 0")
	}
	// Reject oversized uploads before reading the body.
	if size > maxUploadSize {
		return "", fmt.Errorf("upload size %d exceeds maximum %d bytes", size, maxUploadSize)
	}

	// Buffer the body so we can set Content-Length reliably.
	limited := io.LimitReader(r, maxUploadSize+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("storage: read body: %w", err)
	}
	if int64(len(buf)) > maxUploadSize {
		return "", fmt.Errorf("upload exceeds maximum size of 2GB")
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	canonicalResource := fmt.Sprintf("/%s/%s", s.cfg.Bucket, key)
	// x-oss-object-acl must appear in canonical headers (sorted alphabetically).
	canonicalOSSHeaders := "x-oss-object-acl:public-read\n"
	stringToSign := strings.Join([]string{
		http.MethodPut,
		"", // Content-MD5 (omitted)
		contentType,
		date,
		canonicalOSSHeaders + canonicalResource,
	}, "\n")

	sig := s.sign(stringToSign)

	uploadURL := fmt.Sprintf("https://%s.%s/%s",
		s.cfg.Bucket, strings.TrimPrefix(s.cfg.Endpoint, "https://"), key)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("storage: build request: %w", err)
	}
	req.ContentLength = int64(len(buf))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Date", date)
	req.Header.Set("x-oss-object-acl", "public-read")
	req.Header.Set("Authorization", fmt.Sprintf("OSS %s:%s", s.cfg.AccessKey, sig))

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("storage: OSS PUT: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("storage: OSS upload failed (%s): %s", resp.Status, string(body))
	}

	base := strings.TrimRight(s.cfg.BaseURL, "/")
	if base == "" {
		base = fmt.Sprintf("https://%s.%s", s.cfg.Bucket, strings.TrimPrefix(s.cfg.Endpoint, "https://"))
	}
	// 补全协议头：防止 base_url 配置时遗漏 https://
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return base + "/" + key, nil
}

func (s *ossService) Delete(ctx context.Context, key string) error {
	deleteURL := fmt.Sprintf("https://%s.%s/%s",
		s.cfg.Bucket, strings.TrimPrefix(s.cfg.Endpoint, "https://"), key)

	date := time.Now().UTC().Format(http.TimeFormat)
	canonicalResource := fmt.Sprintf("/%s/%s", s.cfg.Bucket, key)
	stringToSign := strings.Join([]string{http.MethodDelete, "", "", date, canonicalResource}, "\n")
	sig := s.sign(stringToSign)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("storage: build delete request: %w", err)
	}
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", fmt.Sprintf("OSS %s:%s", s.cfg.AccessKey, sig))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("storage: OSS DELETE: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("storage: OSS delete failed (%s): %s", resp.Status, string(body))
	}
	return nil
}

func (s *ossService) Get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: build get request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage: OSS GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage: OSS GET %s: HTTP %d", url, resp.StatusCode)
	}
	const maxSize = 500 << 20
	return io.ReadAll(io.LimitReader(resp.Body, maxSize))
}

func (s *ossService) sign(stringToSign string) string {
	mac := hmac.New(sha1.New, []byte(s.cfg.SecretKey))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
