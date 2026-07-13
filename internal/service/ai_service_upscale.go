package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"golang.org/x/image/draw"
)

// UpscaleImage 放大图片。method 为 "ai" 时调用 AI 增强，否则使用 CatmullRom 双三次插值。
// scale 为整数倍放大系数（建议 2 或 4，最大 8）。
func (s *AIService) UpscaleImage(ctx context.Context, tenantID, novelID uint, imageURL string, scale int, method string) (string, error) {
	if scale <= 1 {
		scale = 2
	}
	if scale > 8 {
		scale = 8
	}

	// 下载原图（两种模式共用）
	data, contentType, err := s.downloadImageBytes(ctx, imageURL)
	if err != nil {
		return "", fmt.Errorf("upscale: %w", err)
	}

	// 解码获取尺寸（两种模式均需要）
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("upscale: decode image: %w", err)
	}
	srcB := src.Bounds()
	dstW := srcB.Dx() * scale
	dstH := srcB.Dy() * scale

	if method == "ai" {
		return s.upscaleImageAI(ctx, tenantID, novelID, data, imageURL, dstW, dstH)
	}
	return s.upscaleImageBicubic(ctx, src, srcB, format, contentType, dstW, dstH)
}

// downloadImageBytes 下载图片到内存，返回 (data, contentType, error)。
// 支持绝对 URL 和相对路径（/api/v1/media/xxx）；相对路径优先用 dbMediaReader 直接读 DB。
func (s *AIService) downloadImageBytes(ctx context.Context, imageURL string) ([]byte, string, error) {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		if s.dbMediaReader != nil && strings.HasPrefix(imageURL, "/api/v1/media/") {
			data, err := s.dbMediaReader.Get(ctx, imageURL)
			if err == nil && len(data) > 0 {
				return data, "image/jpeg", nil
			}
			logger.Errorf("downloadImageBytes: dbMediaReader.Get(%q) failed: %v", imageURL, err)
		}
		if s.serverBaseURL == "" {
			return nil, "", fmt.Errorf("relative URL %q but no dbMediaReader or serverBaseURL configured", imageURL)
		}
		imageURL = s.serverBaseURL + "/" + strings.TrimLeft(imageURL, "/")
	}
	fetchURL := imageURL
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	const maxSize = 50 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxSize {
		return nil, "", fmt.Errorf("image too large (>50MB)")
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return data, ct, nil
}

// applySharpen 对放大后的 RGBA 图像应用 3×3 锐化卷积核，使边缘更清晰。
// 核：中心 5，上下左右各 -1，角不参与（等价于 USM 的快速近似）。
func applySharpen(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if x == b.Min.X || x == b.Max.X-1 || y == b.Min.Y || y == b.Max.Y-1 {
				dst.Set(x, y, src.At(x, y))
				continue
			}
			c := src.RGBAAt(x, y)
			t := src.RGBAAt(x, y-1)
			bm := src.RGBAAt(x, y+1)
			l := src.RGBAAt(x-1, y)
			r := src.RGBAAt(x+1, y)
			dst.SetRGBA(x, y, color.RGBA{
				R: clamp(5*int(c.R) - int(t.R) - int(bm.R) - int(l.R) - int(r.R)),
				G: clamp(5*int(c.G) - int(t.G) - int(bm.G) - int(l.G) - int(r.G)),
				B: clamp(5*int(c.B) - int(t.B) - int(bm.B) - int(l.B) - int(r.B)),
				A: c.A,
			})
		}
	}
	return dst
}

// upscaleImageBicubic CatmullRom 双三次插值放大 + 锐化，不依赖任何 AI 接口。
func (s *AIService) upscaleImageBicubic(ctx context.Context, src image.Image, srcB image.Rectangle, format, _ string, dstW, dstH int) (string, error) {
	scaled := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), src, srcB, draw.Over, nil)
	dst := applySharpen(scaled)

	var buf bytes.Buffer
	var outCT string
	switch format {
	case "png":
		outCT = "image/png"
		if err := png.Encode(&buf, dst); err != nil {
			return "", fmt.Errorf("upscale bicubic: encode png: %w", err)
		}
	default:
		outCT = "image/jpeg"
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 95}); err != nil {
			return "", fmt.Errorf("upscale bicubic: encode jpeg: %w", err)
		}
	}

	if s.storageSvc == nil {
		return "", fmt.Errorf("upscale bicubic: storage service not configured")
	}
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	key := fmt.Sprintf("images/upscaled/%s%s", uuid.New().String(), ext)
	outData := buf.Bytes()
	newURL, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(outData), int64(len(outData)), outCT)
	if err != nil {
		return "", fmt.Errorf("upscale bicubic: upload: %w", err)
	}
	logger.Printf("[AIService] upscaleImageBicubic: → %dx%d, saved to %s", dstW, dstH, newURL)
	return newURL, nil
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
