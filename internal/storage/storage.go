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
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// Service is the abstraction for file storage.
type Service interface {
	// Upload stores r under the given object key and returns the public URL.
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (url string, err error)
}

// Config maps to config.StorageConfig.
type Config struct {
	Type      string // "oss" | "local" (default)
	Endpoint  string // e.g. "oss-cn-hangzhou.aliyuncs.com"
	AccessKey string
	SecretKey string
	Bucket    string
	BaseURL   string // public URL prefix, e.g. "https://my-bucket.oss-cn-hangzhou.aliyuncs.com"
	LocalDir  string // root dir for local storage (default "./uploads")
	LocalBase string // URL prefix for local storage (default "/uploads")
}

// New returns an OSS-backed Service when credentials are present,
// a DB-backed Service when a *gorm.DB is provided, otherwise falls back to local filesystem.
func New(cfg Config, db ...*gorm.DB) Service {
	if cfg.Type == "oss" && cfg.AccessKey != "" && cfg.SecretKey != "" && cfg.Bucket != "" {
		return newOSSService(cfg)
	}
	if len(db) > 0 && db[0] != nil {
		return &dbStorageService{db: db[0]}
	}
	dir := cfg.LocalDir
	if dir == "" {
		dir = "./uploads"
	}
	base := cfg.LocalBase
	if base == "" {
		base = "/uploads"
	}
	return &localService{dir: dir, base: base}
}

// BuildKey constructs the canonical storage key for a media asset.
// Format: novels/{novelID}/chapters/{chapterID}/{mediaType}/{filename}
func BuildKey(novelID, chapterID uint, mediaType, filename string) string {
	return fmt.Sprintf("novels/%d/chapters/%d/%s/%s", novelID, chapterID, mediaType, filename)
}

// ─── OSS (Aliyun Object Storage, V1 signature) ──────────────────────────────

type ossService struct {
	cfg    Config
	client *http.Client
}

func newOSSService(cfg Config) *ossService {
	return &ossService{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}
}

func (s *ossService) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	// Buffer the body so we can set Content-Length reliably.
	var buf []byte
	if size > 0 {
		buf = make([]byte, 0, size)
	}
	var err error
	buf, err = io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("storage: read body: %w", err)
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	canonicalResource := fmt.Sprintf("/%s/%s", s.cfg.Bucket, key)
	stringToSign := strings.Join([]string{
		http.MethodPut,
		"", // Content-MD5 (omitted)
		contentType,
		date,
		canonicalResource,
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
	return base + "/" + key, nil
}

func (s *ossService) sign(stringToSign string) string {
	mac := hmac.New(sha1.New, []byte(s.cfg.SecretKey))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ─── Local filesystem fallback ───────────────────────────────────────────────

type localService struct {
	dir  string
	base string
}

func (s *localService) Upload(_ context.Context, key string, r io.Reader, _ int64, _ string) (string, error) {
	dest := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "", fmt.Errorf("storage: mkdir: %w", err)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("storage: create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", fmt.Errorf("storage: write file: %w", err)
	}
	return strings.TrimRight(s.base, "/") + "/" + key, nil
}

// ─── DB storage backend ──────────────────────────────────────────────────────

type dbStorageService struct{ db *gorm.DB }

func (s *dbStorageService) Upload(_ context.Context, key string, r io.Reader, _ int64, contentType string) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("storage: read body: %w", err)
	}
	mediaType := parseKeySegmentStr(key, 4) // novels/{id}/chapters/{id}/{type}/...
	if mediaType == "" {
		mediaType = mediaTypeFromContentType(contentType)
	}
	asset := &model.MediaAsset{
		NovelID:     parseKeySegment(key, 1),
		ChapterID:   parseKeySegment(key, 3),
		MediaType:   mediaType,
		Filename:    path.Base(key),
		ContentType: contentType,
		Size:        int64(len(data)),
		Data:        data,
	}
	if err := s.db.Create(asset).Error; err != nil {
		return "", fmt.Errorf("storage: db create: %w", err)
	}
	return fmt.Sprintf("/api/v1/media/%d", asset.ID), nil
}

func mediaTypeFromContentType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	default:
		return "file"
	}
}

// parseKeySegment splits the storage key by "/" and parses the segment at idx as uint.
// Key format: novels/{1}/chapters/{3}/{4:type}/{5:filename}
func parseKeySegment(key string, idx int) uint {
	parts := strings.Split(key, "/")
	if idx >= len(parts) {
		return 0
	}
	v, _ := strconv.ParseUint(parts[idx], 10, 64)
	return uint(v)
}

func parseKeySegmentStr(key string, idx int) string {
	parts := strings.Split(key, "/")
	if idx >= len(parts) {
		return ""
	}
	return parts[idx]
}
