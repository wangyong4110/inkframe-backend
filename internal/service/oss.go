package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
)

// uploadImageToStorage 下载 AI 模型返回的临时图片 URL 并上传到持久存储（OSS/本地/DB）。
// storageSvc 为 nil 或上传失败时降级返回原 imgURL（非致命）。
// OSS key 格式：
//   - 有小说+章节信息：novels/{title}/chapters/{no}/images/{uuid}.ext
//   - 有小说信息：     novels/{title}/images/{uuid}.ext
//   - 无信息（降级）：  images/{tenantID}/{uuid}.ext
func (s *AIService) uploadImageToStorage(ctx context.Context, imgURL string) string {
	if s.storageSvc == nil || imgURL == "" {
		return imgURL
	}
	// 下载→上传使用独立 background context，避免被 HTTP 请求 context 取消
	// （客户端断开连接会取消请求 context，但转存操作应当完成以防临时 URL 过期）
	dlCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, imgURL, nil)
	if err != nil {
		logger.Errorf("uploadImageToStorage: build request: %v", err)
		return imgURL
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Errorf("uploadImageToStorage: download %s: %v", imgURL, err)
		return imgURL
	}
	defer resp.Body.Close()
	const maxImageSize = 50 << 20 // 50 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		logger.Errorf("uploadImageToStorage: read body: %v", err)
		return imgURL
	}
	if len(data) > maxImageSize {
		logger.Printf("uploadImageToStorage: image too large (>50MB) from %s", imgURL)
		return imgURL
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = "image/jpeg"
	}
	ext := imageExtFromContentType(ct)
	filename := uuid.New().String() + ext
	logger.Printf("uploadImageToStorage: generated filename=%q from imgURL=%q", filename, imgURL)

	key := fmt.Sprintf("images/%s", filename)

	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer uploadCancel()
	persistURL, uploadErr := s.storageSvc.Upload(uploadCtx, key, bytes.NewReader(data), int64(len(data)), ct)
	if uploadErr != nil {
		logger.Errorf("uploadImageToStorage: upload failed (falling back to original URL): %v", uploadErr)
		return imgURL
	}
	logger.Printf("uploadImageToStorage: persisted %s → %s", imgURL, persistURL)
	return persistURL
}

// PersistExternalImage 下载外部图片 URL 并上传到持久存储（OSS），返回永久 URL。
// 用于将 AI 服务商返回的临时签名 URL（如 Volcengine TOS 24h 过期 URL）转存为永久可访问 URL。
// storageSvc 为 nil 或上传失败时降级返回原 URL（非致命）。
func (s *AIService) PersistExternalImage(ctx context.Context, imgURL string) string {
	return s.uploadImageToStorage(ctx, imgURL)
}
