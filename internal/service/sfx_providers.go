package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

// normalizeTag 将标签统一为小写下划线格式（兼容空格和连字符）
func normalizeTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	tag = strings.ReplaceAll(tag, " ", "_")
	tag = strings.ReplaceAll(tag, "-", "_")
	return tag
}

// sfxCategoryVolume 根据音效类型返回建议混音音量（0.1–0.6）。
// 冲击音效音量较高，环境音效较低，避免掩盖人声。
func sfxCategoryVolume(tag string) float64 {
	lower := strings.ToLower(tag)
	// 冲击类：爆炸、打击、碰撞 → 较高音量
	for _, kw := range []string{"explosion", "blast", "impact", "clash", "punch", "crash", "bang", "boom", "thunder"} {
		if strings.Contains(lower, kw) {
			return 0.55
		}
	}
	// 动作类：脚步、门、武器 → 中等音量
	for _, kw := range []string{"footstep", "door", "sword", "arrow", "whoosh", "gallop", "swing", "click"} {
		if strings.Contains(lower, kw) {
			return 0.45
		}
	}
	// 环境类：自然音、人群 → 较低音量（避免掩盖旁白）
	for _, kw := range []string{"rain", "wind", "forest", "ambient", "crowd", "city", "river", "fire", "room"} {
		if strings.Contains(lower, kw) {
			return 0.3
		}
	}
	// 情绪/转场类：心跳、时钟 → 低音量
	for _, kw := range []string{"heartbeat", "clock", "tick", "breath"} {
		if strings.Contains(lower, kw) {
			return 0.25
		}
	}
	return 0.4 // 默认
}

// downloadURLAndUploadToOSS 从远端 URL 下载音频并上传到 OSS，返回 OSS 永久 URL。
// 用于将 Kling SFX / ElevenLabs 等 CDN 临时链接转为永久可访问地址。
func downloadURLAndUploadToOSS(ctx context.Context, svc storage.Service, srcURL, ossKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", srcURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", srcURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024)) // 最大 32MB
	if err != nil {
		return "", fmt.Errorf("read %s: %w", srcURL, err)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "audio/") {
		ct = "audio/mpeg"
	}
	return svc.Upload(ctx, ossKey, bytes.NewReader(data), int64(len(data)), ct)
}

// uploadLocalFileToOSS 读取本地文件并上传到 OSS，上传后删除临时文件。
func uploadLocalFileToOSS(ctx context.Context, svc storage.Service, localPath, ossKey string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return "", err
	}
	u, err := svc.Upload(ctx, ossKey, f, fi.Size(), "audio/mpeg")
	f.Close()
	os.Remove(localPath) //nolint:errcheck
	return u, err
}

// ensureOSSHit 确保 sfxHit.url 是可持久访问的 OSS 链接。
// file:// → 上传本地文件；http(s):// → 下载后上传。
// 存储服务未配置时原样返回；上传失败时清空 URL（不保存不可访问的地址），并标记 noCache。
func (s *SFXService) ensureOSSHit(ctx context.Context, hit sfxHit) sfxHit {
	if hit.url == "" {
		return hit
	}
	if s.storageSvc == nil {
		logger.Printf("[SFXService] ensureOSSHit: storageSvc not configured, returning raw URL %s", hit.url)
		return hit
	}
	ossKey := fmt.Sprintf("sfx/%s.mp3", uuid.New().String())
	if strings.HasPrefix(hit.url, "file://") {
		localPath := strings.TrimPrefix(hit.url, "file://")
		if u, err := uploadLocalFileToOSS(ctx, s.storageSvc, localPath, ossKey); err == nil {
			hit.url = u
		} else {
			logger.Errorf("[SFXService] ensureOSSHit: local upload failed (%s): %v", localPath, err)
			hit.url = ""
			hit.noCache = true
		}
		return hit
	}
	if strings.HasPrefix(hit.url, "http://") || strings.HasPrefix(hit.url, "https://") {
		if u, err := downloadURLAndUploadToOSS(ctx, s.storageSvc, hit.url, ossKey); err == nil {
			hit.url = u
		} else {
			logger.Errorf("[SFXService] ensureOSSHit: CDN download/upload failed (%s): %v", hit.url, err)
			hit.noCache = true // 临时 CDN URL 仍可用，但不缓存
		}
	}
	return hit
}
