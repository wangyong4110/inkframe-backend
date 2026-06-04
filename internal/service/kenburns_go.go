package service

// Pure-Go Ken Burns implementation.
//
// Strategy:
//   1. Decode source JPEG into *image.RGBA for fast direct pixel access.
//   2. For each frame, compute the crop rectangle (zoom/pan) in Go — interruptible
//      between frames via context cancellation.
//   3. Bilinear-scale the crop to output size using integer fixed-point arithmetic
//      (~30× faster than float64 for this inner loop).
//   4. Write each frame as JPEG to a temp directory.
//   5. Call WASM FFmpeg once with a simple "-i frame%05d.jpg -c:v libx264" command.
//      This is fast (just entropy coding, no per-frame pixel computation in WASM).
//
// Typical wall-clock time: ~10-20s for a 5-second 1920×1080 clip (vs 30-90s with
// WASM zoompan). The WASM zoompan runs pure CPU code that cannot be interrupted;
// pure Go frame computation is fully interruptible via context cancellation.

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // P0-1: register PNG decoder so image.Decode handles PNG inputs
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// generateKenBurnsPureGo renders Ken Burns animation frames in pure Go and encodes
// the resulting JPEG sequence to MP4 with WASM FFmpeg.
func (s *VideoService) generateKenBurnsPureGo(ctx context.Context, shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = defaultShotDurationSecs
	}
	const fps = 24 // P1-4: match synthesis output fps to eliminate concat stuttering
	totalFrames := int(duration*float64(fps) + 0.5)
	if totalFrames < 1 {
		totalFrames = 1
	}

	// Output dimensions.
	outW, outH := 1920, 1080
	switch aspectRatio {
	case "9:16":
		outW, outH = 1080, 1920
	case "1:1":
		outW, outH = 1080, 1080
	case "4:3":
		outW, outH = 1440, 1080
	}

	// Decode source JPEG.
	f, err := os.Open(imagePath)
	if err != nil {
		return "", fmt.Errorf("ken burns go: open: %w", err)
	}
	srcImg, _, err := image.Decode(f) // P0-1: image.Decode handles JPEG and PNG (via blank imports)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("ken burns go: image decode: %w", err)
	}

	// Convert to *image.RGBA for direct byte-slice access.
	rgba := kbToRGBA(srcImg)
	srcW := rgba.Bounds().Dx()
	srcH := rgba.Bounds().Dy()
	if srcW == 0 || srcH == 0 {
		return "", fmt.Errorf("ken burns go: empty source image")
	}

	// Temp directory for JPEG frames.
	// Must be under inkframeTempDir() so the WASM ffmpeg (WASI) can access it
	// via the absolute path mounted at startup. Using os.MkdirTemp("", ...) can
	// produce /tmp/... which on macOS is a symlink to /private/tmp — ffmpeg WASM
	// follows the literal path and may fail to open the frame files.
	frameDir, err := os.MkdirTemp(inkframeTempDir(), "kbgo-")
	if err != nil {
		return "", fmt.Errorf("ken burns go: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(frameDir)

	renderStart := time.Now()
	// P2-3: warn when frame disk usage is large (JPEG 85 ≈ 0.1 bytes/pixel)
	estFrameMB := int64(totalFrames) * int64(outW) * int64(outH) / (10 * 1024 * 1024)
	if estFrameMB > 200 {
		logger.Printf("generateKenBurnsPureGo: shot %d WARNING estimated frame disk usage ~%dMB (%d frames %dx%d)", shot.ShotNo, estFrameMB, totalFrames, outW, outH)
	}
	logger.Printf("generateKenBurnsPureGo: shot %d rendering %d frames (%dx%d) src=%dx%d camera=%s frameDir=%s",
		shot.ShotNo, totalFrames, outW, outH, srcW, srcH, shot.CameraType, frameDir)

	// Render one JPEG per frame.
	for i := 0; i < totalFrames; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		// 每 10% 打一条进度日志（totalFrames/10 帧间隔，最少每 60 帧）
		logInterval := totalFrames / 10
		if logInterval < 60 {
			logInterval = 60
		}
		if i > 0 && i%logInterval == 0 {
			logger.Printf("generateKenBurnsPureGo: shot %d frame %d/%d (%.0f%%)",
				shot.ShotNo, i, totalFrames, float64(i)*100/float64(totalFrames))
		}

		t := float64(i) / float64(fps)
		cropX, cropY, cropW, cropH := kbCrop(shot.CameraType, srcW, srcH, t, duration)

		frame := kbCropScale(rgba, cropX, cropY, cropW, cropH, outW, outH)

		framePath := filepath.Join(frameDir, fmt.Sprintf("frame%05d.jpg", i))
		if err := kbWriteJPEG(frame, framePath); err != nil {
			return "", fmt.Errorf("ken burns go: write frame %d: %w", i, err)
		}
	}
	logger.Printf("generateKenBurnsPureGo: shot %d frame rendering done in %.1fs", shot.ShotNo, time.Since(renderStart).Seconds())

	// 查询当前视频的调色配置（可选）
	colorGrade := ""
	var colorContrast, colorSaturation float64
	colorSaturation = 1.0
	var filmGrain, vignette, chromaticAberration bool
	if shot.VideoID > 0 && s.videoRepo != nil && s.novelRepo != nil {
		if video, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil && video.NovelID > 0 {
			if novel, nErr := s.novelRepo.GetByID(video.NovelID); nErr == nil {
				vc := novel.VideoConf()
				if vc.ColorGrade != "" && vc.ColorGrade != "none" {
					colorGrade = vc.ColorGrade
					colorContrast = vc.ContrastLevel
					colorSaturation = vc.Saturation
					if colorSaturation == 0 {
						colorSaturation = 1.0
					}
				}
				filmGrain = vc.FilmGrain
				vignette = vc.Vignette
				chromaticAberration = vc.ChromaticAberration
			}
		}
	}

	// Encode JPEG sequence → MP4.  This is the fast step: no per-frame pixel math
	// in WASM, just JPEG decode + x264 entropy coding.
	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%s.mp4", inkframeTempDir(), shot.ID, uuid.New().String()[:8])
	inputPattern := filepath.Join(frameDir, "frame%05d.jpg")

	// 构建 vf 滤镜链（基础 + 可选调色 + 镜头特效）
	var vfFilters []string
	if gradeFilter := buildColorGradeFilter(colorGrade, colorContrast, colorSaturation); gradeFilter != "" {
		vfFilters = append(vfFilters, gradeFilter)
	}

	// 胶片颗粒（Film Grain）
	if filmGrain {
		vfFilters = append(vfFilters, "noise=alls=8:allf=t+u")
	}

	// 镜头暗角（Vignette）
	if vignette {
		vfFilters = append(vfFilters, "vignette=PI/4.5:eval=frame")
	}

	// 色差（Chromatic Aberration）— 通过 colorbalance 模拟 RGB 偏移
	if chromaticAberration {
		vfFilters = append(vfFilters, "colorbalance=rs=0.03:gs=0:bs=-0.03:rm=0:gm=0:bm=0:rh=0:gh=0:bh=0")
	}

	vfFilters = append(vfFilters, "format=yuv420p")
	vfFilter := strings.Join(vfFilters, ",")

	encStart := time.Now()
	logger.Printf("generateKenBurnsPureGo: shot %d starting ffmpeg encode: %d frames → %s vf=%q", shot.ShotNo, totalFrames, outPath, vfFilter)
	// Use goroutine timeout: wazero cannot interrupt WASM x264 mid-loop via context cancellation.
	// -preset ultrafast dramatically reduces WASM libx264 encoding time (same reason as generateStillFrameClip).
	encOut, encErr := runFFmpegWithGoroutineTimeout(10*time.Minute,
		"-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", inputPattern,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-vf", vfFilter,
		"-r", fmt.Sprintf("%d", fps),
		outPath,
	)
	if encErr != nil {
		logger.Printf("generateKenBurnsPureGo: shot %d ffmpeg encode failed after %.1fs: %v\noutput: %s",
			shot.ShotNo, time.Since(encStart).Seconds(), encErr, string(encOut))
		return "", fmt.Errorf("ken burns go: ffmpeg encode: %w", encErr)
	}
	logger.Printf("generateKenBurnsPureGo: shot %d ffmpeg encode done in %.1fs → %s", shot.ShotNo, time.Since(encStart).Seconds(), outPath)
	return outPath, nil
}

// buildColorGradeFilter 根据调色方案返回 FFmpeg 滤镜字符串（追加到 vf chain）。
// 空字符串表示无需调色（透传）。
func buildColorGradeFilter(grade string, contrast, saturation float64) string {
	var parts []string

	// 预设调色方案
	switch grade {
	case "cinematic":
		// 模拟 ARRI LogC→Rec709 的专业影调：蓝色高光 + 橙色阴影 + 降低整体饱和度
		if contrast == 0 {
			contrast = 0.15
		}
		if saturation == 1.0 {
			saturation = 0.85
		}
		parts = append(parts,
			"curves=r='0/0 0.3/0.28 0.7/0.72 1/0.97':g='0/0 0.3/0.30 0.7/0.70 1/0.97':b='0/0 0.3/0.32 0.7/0.74 1/1.02'",
			fmt.Sprintf("eq=contrast=%.2f:saturation=%.2f", 1.0+contrast, saturation),
		)
		// 已处理 eq，跳过后续 eq 逻辑
		return strings.Join(parts, ",")
	case "warm":
		parts = append(parts, "colorchannelmixer=rr=1.05:gg=1.0:bb=0.92")
	case "cool":
		parts = append(parts, "colorchannelmixer=rr=0.92:gg=0.98:bb=1.08")
	case "teal_orange":
		// 好莱坞商业片经典：青色阴影 + 橙色肤色
		if saturation == 1.0 {
			saturation = 1.15
		}
		parts = append(parts,
			"curves=r='0/0.05 0.3/0.35 0.6/0.65 1/1.0':g='0/0 0.3/0.27 0.7/0.70 1/0.95':b='0/0.1 0.3/0.35 0.6/0.55 1/0.85'",
			fmt.Sprintf("eq=saturation=%.2f", saturation),
		)
		// 已处理 eq，跳过后续 eq 逻辑
		return strings.Join(parts, ",")
	case "vintage":
		parts = append(parts, "colorchannelmixer=rr=1.02:gg=0.95:bb=0.88")
		if saturation == 1.0 {
			saturation = 0.8
		}
	case "noir":
		// 黑白
		parts = append(parts, "hue=s=0")
		if contrast == 0 {
			contrast = 0.2
		}
	default:
		// "none" 或未知方案：无调色
		return ""
	}

	// 对比度/饱和度调整（eq 滤镜）
	var eqParts []string
	if contrast != 0 {
		c := 1.0 + contrast // 0.15 → 1.15
		eqParts = append(eqParts, fmt.Sprintf("contrast=%.2f", c))
	}
	if saturation != 1.0 && grade != "noir" {
		eqParts = append(eqParts, fmt.Sprintf("saturation=%.2f", saturation))
	}
	if len(eqParts) > 0 {
		parts = append(parts, "eq="+strings.Join(eqParts, ":"))
	}

	return strings.Join(parts, ",")
}

// kbCrop returns the crop rectangle in source-image coordinates for the given
// camera type and time t ∈ [0, duration] (seconds).
//
// Zoom factors mirror the FFmpeg zoompan expressions in generateKenBurnsClip:
//   - "zoom":    z increments by 0.002/frame from 1.0 → 1.5, centred
//   - "pan":     fixed z=1.3, horizontal pan left→right, centred vertically
//   - "tilt":    fixed z=1.3, vertical pan top→bottom, centred horizontally
//   - default:   z increments by 0.0008/frame from 1.0 → 1.2, centred (Ken Burns classic)
func kbCrop(cameraType string, srcW, srcH int, t, duration float64) (x, y, w, h int) {
	const fps = 30

	// Compute linear progress [0, 1] then apply smoothstep easing (cubic 3t²-2t³).
	// This eliminates the mechanical constant-speed feel of linear motion.
	var progress float64
	if duration > 0 {
		progress = t / duration
		if progress < 0 {
			progress = 0
		}
		if progress > 1 {
			progress = 1
		}
	}
	// Smoothstep easing: 消除线性运动的机械感
	progress = progress * progress * (3 - 2*progress)

	var zoom float64
	switch cameraType {
	case "zoom":
		// z starts at 1.0, increments 0.002 per frame, caps at 1.5.
		frames := t * fps
		zoom = 1.0 + frames*0.002
		if zoom > 1.5 {
			zoom = 1.5
		}
		w = int(float64(srcW) / zoom)
		h = int(float64(srcH) / zoom)
		x = (srcW - w) / 2
		y = (srcH - h) / 2

	case "pan":
		// Fixed zoom=1.3, pan from left to right with smoothstep easing.
		const panZoom = 1.3
		w = int(float64(srcW) / panZoom)
		h = int(float64(srcH) / panZoom)
		xStart := (srcW - w) / 2
		xEnd := xStart + (srcW - w) // matches FFmpeg: iw/2-(iw/zoom/2) + (iw - iw/zoom)
		x = xStart + int(float64(xEnd-xStart)*progress)
		y = (srcH - h) / 2

	case "tilt":
		// 垂直平移（从上到下）with smoothstep easing.
		const tiltZoom = 1.3
		w = int(float64(srcW) / tiltZoom)
		h = int(float64(srcH) / tiltZoom)
		x = (srcW - w) / 2
		yStart := 0
		yEnd := srcH - h
		y = yStart + int(float64(yEnd-yStart)*progress)

	default:
		// Gentle Ken Burns: z increments 0.0008/frame, caps at 1.2.
		frames := t * fps
		zoom = 1.0 + frames*0.0008
		if zoom > 1.2 {
			zoom = 1.2
		}
		w = int(float64(srcW) / zoom)
		h = int(float64(srcH) / zoom)
		x = (srcW - w) / 2
		y = (srcH - h) / 2
	}

	// Clamp to source bounds.
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if x+w > srcW {
		w = srcW - x
	}
	if y+h > srcH {
		h = srcH - y
	}
	return
}

// kbToRGBA converts any image.Image to *image.RGBA for direct pixel access.
func kbToRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)
	return dst
}

// kbCropScale crops the region [cropX, cropY, cropW, cropH] from src and scales
// it to dstW×dstH using bilinear interpolation with integer fixed-point arithmetic
// (1024-scale fractions, shift-20 at the end).
func kbCropScale(src *image.RGBA, cropX, cropY, cropW, cropH, dstW, dstH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	srcPix := src.Pix
	srcStride := src.Stride
	dstPix := dst.Pix

	// Precompute per-output-column: source x0, x1, and fraction (0..1023).
	type xc struct{ x0, x1 int; frac int32 }
	xCoords := make([]xc, dstW)
	for dx := 0; dx < dstW; dx++ {
		var fx float64
		if dstW > 1 {
			fx = float64(dx) * float64(cropW-1) / float64(dstW-1)
		}
		xi := int(fx)
		frac := int32((fx - float64(xi)) * 1024)
		xi0 := cropX + xi
		xi1 := xi0 + 1
		if xi >= cropW-1 || cropW <= 1 {
			xi0 = cropX + min2(xi, cropW-1)
			xi1 = xi0
			frac = 0
		}
		xCoords[dx] = xc{xi0, xi1, frac}
	}

	for dy := 0; dy < dstH; dy++ {
		var fy float64
		if dstH > 1 {
			fy = float64(dy) * float64(cropH-1) / float64(dstH-1)
		}
		yi := int(fy)
		yFrac := int32((fy - float64(yi)) * 1024)
		if yi >= cropH-1 || cropH <= 1 {
			yi = min2(yi, cropH-1)
			yFrac = 0
		}

		row0 := (cropY+yi)*srcStride
		row1 := (cropY+yi+1)*srcStride
		if yi >= cropH-1 || cropH <= 1 {
			row1 = row0
		}
		dstRow := dy * dstW * 4

		for dx := 0; dx < dstW; dx++ {
			c := xCoords[dx]
			o00 := row0 + c.x0*4
			o10 := row0 + c.x1*4
			o01 := row1 + c.x0*4
			o11 := row1 + c.x1*4
			xf := c.frac

			base := dstRow + dx*4
			for ch := 0; ch < 4; ch++ {
				v00 := int32(srcPix[o00+ch])
				v10 := int32(srcPix[o10+ch])
				v01 := int32(srcPix[o01+ch])
				v11 := int32(srcPix[o11+ch])
				top := v00*(1024-xf) + v10*xf
				bot := v01*(1024-xf) + v11*xf
				val := (top*(1024-yFrac) + bot*yFrac) >> 20
				dstPix[base+ch] = uint8(val)
			}
		}
	}
	return dst
}

// min2 is a simple int min helper (Go 1.21 has min() built-in but 1.23 is safe).
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// kbWriteJPEG encodes an RGBA image as JPEG quality 85 to the given path.
func kbWriteJPEG(img image.Image, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 85})
}
