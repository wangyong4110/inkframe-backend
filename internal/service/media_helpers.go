package service

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// readLocalOrRemoteFile 根据路径前缀读取文件内容：
//   - file:// → 本地文件
//   - http(s):// → HTTP 下载
//   - / 开头 → 先尝试服务器工作目录下的本地文件
//   - 其余 → 当作本地路径
func readLocalOrRemoteFile(path string) ([]byte, error) {
	if strings.HasPrefix(path, "file://") {
		return os.ReadFile(strings.TrimPrefix(path, "file://"))
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return downloadMediaFile(path)
	}
	// 相对 HTTP 路径（/uploads/...）→ 先尝试作为服务器工作目录下的本地文件
	if strings.HasPrefix(path, "/") {
		if data, err := os.ReadFile("." + path); err == nil {
			return data, nil
		}
		// 其余 /api/... 等路径无法在此处解析（需要 serverBaseURL），直接失败
		return nil, fmt.Errorf("cannot resolve relative URL %q without server base URL", path)
	}
	// 裸本地路径（相对或绝对路径）
	return os.ReadFile(path)
}

// audioExtension 从路径/URL 猜测音频后缀，默认 .mp3
func audioExtension(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range []string{".wav", ".aac", ".ogg", ".m4a", ".flac", ".mp3"} {
		if strings.Contains(lower, ext) {
			return ext
		}
	}
	return ".mp3"
}

// downloadMediaFile 从 URL 下载文件，超时 2min，单文件最大 200MB。
// 加 io.LimitReader 防止异常 CDN 大文件耗尽内存。
func downloadMediaFile(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBytes = 200 << 20 // 200 MB per file
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("file exceeds max size %d bytes", maxBytes)
	}
	return data, nil
}
