package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// UpscaleImage 使用 AI 图像生成模型放大图片。scale 为整数倍放大系数（建议 2 或 4，最大 8）。
func (s *AIService) UpscaleImage(ctx context.Context, tenantID, novelID uint, imageURL string, scale int) (string, error) {
	if scale <= 1 {
		scale = 2
	}
	if scale > 8 {
		scale = 8
	}

	data, err := s.downloadImageBytes(ctx, imageURL)
	if err != nil {
		return "", fmt.Errorf("upscale: %w", err)
	}

	// 解码获取原始尺寸，用于按 scale 计算目标分辨率。
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("upscale: decode image: %w", err)
	}
	srcB := src.Bounds()
	dstW := srcB.Dx() * scale
	dstH := srcB.Dy() * scale

	return s.upscaleImageAI(ctx, tenantID, novelID, data, imageURL, dstW, dstH)
}

// downloadImageBytes 下载图片到内存。
// 支持绝对 URL 和相对路径（/api/v1/media/xxx）；相对路径优先用 dbMediaReader 直接读 DB。
func (s *AIService) downloadImageBytes(ctx context.Context, imageURL string) ([]byte, error) {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		if s.dbMediaReader != nil && strings.HasPrefix(imageURL, "/api/v1/media/") {
			data, err := s.dbMediaReader.Get(ctx, imageURL)
			if err == nil && len(data) > 0 {
				return data, nil
			}
			logger.Errorf("downloadImageBytes: dbMediaReader.Get(%q) failed: %v", imageURL, err)
		}
		if s.serverBaseURL == "" {
			return nil, fmt.Errorf("relative URL %q but no dbMediaReader or serverBaseURL configured", imageURL)
		}
		imageURL = s.serverBaseURL + "/" + strings.TrimLeft(imageURL, "/")
	}
	fetchURL := imageURL
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	const maxSize = 50 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("image too large (>50MB)")
	}
	return data, nil
}

// upscaleImageAI 使用 AI 图像生成模型（DreamO）在目标尺寸重新生成图片，保留原图视觉特征。
// 将原图转为 base64 作为参考图，CFGScale=8 强化特征保持，dstW/dstH 指定输出分辨率。
func (s *AIService) upscaleImageAI(ctx context.Context, tenantID, novelID uint, data []byte, origURL string, dstW, dstH int) (string, error) {
	// 转 base64 传给 AI（绕过 OSS 访问限制）
	b64 := base64.StdEncoding.EncodeToString(data)
	if b64 == "" {
		return "", fmt.Errorf("upscale ai: encode base64 failed")
	}

	const upscalePrompt = "masterpiece, best quality, ultra high resolution, sharp focus, fine details, perfect clarity, photorealistic"
	sizeStr := fmt.Sprintf("%dx%d", dstW, dstH)

	// CFGScale=8：高特征保持强度，让输出尽量忠于原图内容
	newURL, err := s.GenerateCharacterThreeView(ctx, tenantID, "", upscalePrompt, b64, "", "", sizeStr, 8.0)
	if err != nil {
		return "", fmt.Errorf("upscale ai: generate: %w", err)
	}
	if newURL == "" {
		return "", fmt.Errorf("upscale ai: empty URL returned")
	}

	// 持久化到 OSS
	persistURL := s.uploadImageToStorage(ctx, tenantID, newURL)
	logger.Printf("[AIService] upscaleImageAI: → %dx%d, saved to %s", dstW, dstH, persistURL)
	return persistURL, nil
}
