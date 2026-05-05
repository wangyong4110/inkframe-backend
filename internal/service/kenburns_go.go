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
	"os"
	"path/filepath"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// generateKenBurnsPureGo renders Ken Burns animation frames in pure Go and encodes
// the resulting JPEG sequence to MP4 with WASM FFmpeg.
func (s *VideoService) generateKenBurnsPureGo(ctx context.Context, shot *model.StoryboardShot, imagePath string, duration float64, aspectRatio string) (string, error) {
	if duration <= 0 {
		duration = 5.0
	}
	const fps = 30
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
	srcImg, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("ken burns go: jpeg decode: %w", err)
	}

	// Convert to *image.RGBA for direct byte-slice access.
	rgba := kbToRGBA(srcImg)
	srcW := rgba.Bounds().Dx()
	srcH := rgba.Bounds().Dy()
	if srcW == 0 || srcH == 0 {
		return "", fmt.Errorf("ken burns go: empty source image")
	}

	// Temp directory for JPEG frames.
	frameDir, err := os.MkdirTemp("", "inkframe-kbgo-")
	if err != nil {
		return "", fmt.Errorf("ken burns go: mkdirtemp: %w", err)
	}
	defer os.RemoveAll(frameDir)

	logger.Printf("generateKenBurnsPureGo: shot %d rendering %d frames (%dx%d) src=%dx%d camera=%s",
		shot.ShotNo, totalFrames, outW, outH, srcW, srcH, shot.CameraType)

	// Render one JPEG per frame.
	for i := 0; i < totalFrames; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		t := float64(i) / float64(fps)
		cropX, cropY, cropW, cropH := kbCrop(shot.CameraType, srcW, srcH, t, duration)

		frame := kbCropScale(rgba, cropX, cropY, cropW, cropH, outW, outH)

		framePath := filepath.Join(frameDir, fmt.Sprintf("frame%05d.jpg", i))
		if err := kbWriteJPEG(frame, framePath); err != nil {
			return "", fmt.Errorf("ken burns go: write frame %d: %w", i, err)
		}
	}

	// Encode JPEG sequence → MP4.  This is the fast step: no per-frame pixel math
	// in WASM, just JPEG decode + x264 entropy coding.
	outPath := fmt.Sprintf("%s/inkframe-slideshow-%d-%d.mp4", inkframeTempDir(), shot.ID, time.Now().UnixNano())
	inputPattern := filepath.Join(frameDir, "frame%05d.jpg")

	_, encErr := runFFmpegCtx(ctx,
		"-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", inputPattern,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-r", fmt.Sprintf("%d", fps),
		"-threads", "1",
		outPath,
	)
	if encErr != nil {
		return "", fmt.Errorf("ken burns go: ffmpeg encode: %w", encErr)
	}
	return outPath, nil
}

// kbCrop returns the crop rectangle in source-image coordinates for the given
// camera type and time t ∈ [0, duration] (seconds).
//
// Zoom factors mirror the FFmpeg zoompan expressions in generateKenBurnsClip:
//   - "zoom":    z increments by 0.002/frame from 1.0 → 1.5, centred
//   - "pan":     fixed z=1.3, horizontal pan left→right, centred vertically
//   - default:   z increments by 0.0008/frame from 1.0 → 1.2, centred (Ken Burns classic)
func kbCrop(cameraType string, srcW, srcH int, t, duration float64) (x, y, w, h int) {
	const fps = 30

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
		// Fixed zoom=1.3, pan from left to right.
		const panZoom = 1.3
		w = int(float64(srcW) / panZoom)
		h = int(float64(srcH) / panZoom)
		xStart := (srcW - w) / 2
		xEnd := xStart + (srcW - w) // matches FFmpeg: iw/2-(iw/zoom/2) + (iw - iw/zoom)
		if duration > 0 {
			x = xStart + int(float64(xEnd-xStart)*t/duration)
		} else {
			x = xStart
		}
		y = (srcH - h) / 2

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
