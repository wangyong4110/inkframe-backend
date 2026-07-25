package storage

import (
	"context"
	"io"

	"gorm.io/gorm"
)

// Service is the abstraction for file storage.
type Service interface {
	// Upload stores r under the given object key and returns the public URL.
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (url string, err error)
	// Delete removes the object identified by the given key (best-effort, non-fatal on missing object).
	Delete(ctx context.Context, key string) error
	// Get retrieves the raw bytes for a stored URL previously returned by Upload.
	// For DB-backed storage the url is "/api/v1/media/{id}".
	// For local storage the url is a "/uploads/..." relative path.
	// For OSS storage the url is a full https:// URL — data is downloaded.
	Get(ctx context.Context, url string) ([]byte, error)
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
