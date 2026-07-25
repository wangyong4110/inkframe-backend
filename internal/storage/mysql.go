package storage

import (
	"context"
	"fmt"
	"io"

	"gorm.io/gorm"
)

// ─── DB storage backend ──────────────────────────────────────────────────────

type dbStorageService struct{ db *gorm.DB }

func (s *dbStorageService) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, fmt.Errorf("storage: DB media storage is disabled; configure OSS")
}

func (s *dbStorageService) Delete(_ context.Context, _ string) error {
	// DB media storage is disabled; ink_media_asset table has been dropped.
	return nil
}

func (s *dbStorageService) Upload(_ context.Context, _ string, _ io.Reader, _ int64, _ string) (string, error) {
	return "", fmt.Errorf("storage: DB media storage is disabled; configure OSS")
}
