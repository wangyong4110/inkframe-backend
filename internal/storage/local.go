package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ─── Local filesystem fallback ───────────────────────────────────────────────

type localService struct {
	dir  string
	base string
}

func (s *localService) Delete(_ context.Context, key string) error {
	dest := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: remove file: %w", err)
	}
	return nil
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

func (s *localService) Get(_ context.Context, url string) ([]byte, error) {
	// url is e.g. "/uploads/novels/1/chapters/2/image/foo.jpg"
	rel := strings.TrimPrefix(url, strings.TrimRight(s.base, "/"))
	dest := filepath.Join(s.dir, filepath.FromSlash(rel))
	return os.ReadFile(dest)
}
