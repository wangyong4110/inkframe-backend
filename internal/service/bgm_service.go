package service

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BGMService BGM背景音乐服务
type BGMService struct {
	bgmDir    string            // 本地 BGM 目录（优先）
	bgmURLMap map[string]string // emotion → URL（CDN fallback，占位符）
}

// NewBGMService 创建 BGM 服务
// bgmDir: 本地 BGM 文件目录，文件名格式 <emotion>.mp3 或 <emotion>.wav
// customURLs: 可选的 emotion→URL 映射覆盖
func NewBGMService(bgmDir string, customURLs ...map[string]string) *BGMService {
	urlMap := map[string]string{
		"tension":  "",
		"joy":      "",
		"sadness":  "",
		"romance":  "",
		"action":   "",
		"mystery":  "",
		"peaceful": "",
		"default":  "",
	}
	if len(customURLs) > 0 {
		for k, v := range customURLs[0] {
			urlMap[k] = v
		}
	}
	return &BGMService{
		bgmDir:    bgmDir,
		bgmURLMap: urlMap,
	}
}

// SelectBGM 根据情感选择 BGM，返回本地路径或 URL
// 返回空字符串表示没有可用的 BGM
func (s *BGMService) SelectBGM(emotion string) string {
	if emotion == "" {
		emotion = "default"
	}
	emotion = strings.ToLower(strings.TrimSpace(emotion))

	// 优先查找本地文件
	if s.bgmDir != "" {
		for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
			candidate := filepath.Join(s.bgmDir, emotion+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		// 尝试 default
		for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg"} {
			candidate := filepath.Join(s.bgmDir, "default"+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	// 回退到 URL map
	if url, ok := s.bgmURLMap[emotion]; ok && url != "" {
		return url
	}
	if url, ok := s.bgmURLMap["default"]; ok && url != "" {
		return url
	}

	return ""
}

// MixBGM 将 BGM 混入视频（BGM 音量 30%，对话优先）
// videoPath: 视频文件路径或 file:// URL
// bgmSource: BGM 本地路径或 HTTP URL
// outputPath: 输出文件路径
func (s *BGMService) MixBGM(videoPath, bgmSource, outputPath string) error {
	videoPath = strings.TrimPrefix(videoPath, "file://")
	bgmSource = strings.TrimPrefix(bgmSource, "file://")

	if videoPath == "" || bgmSource == "" || outputPath == "" {
		return fmt.Errorf("MixBGM: invalid arguments")
	}

	// BGM 需要下载的情况
	bgmLocalPath := bgmSource
	if strings.HasPrefix(bgmSource, "http://") || strings.HasPrefix(bgmSource, "https://") {
		tmpBGM := fmt.Sprintf("/tmp/inkframe-bgm-%d.mp3", os.Getpid())
		if err := downloadFile(bgmSource, tmpBGM); err != nil {
			return fmt.Errorf("MixBGM: download BGM failed: %w", err)
		}
		defer os.Remove(tmpBGM)
		bgmLocalPath = tmpBGM
	}

	// FFmpeg: 混合 BGM（30% 音量），原视频音频优先，BGM 循环填充
	// -stream_loop -1 让 BGM 循环
	// amix: 混合两路音频，duration=first 以视频时长为准
	cmd := exec.Command("ffmpeg", "-y",
		"-i", videoPath,
		"-stream_loop", "-1",
		"-i", bgmLocalPath,
		"-filter_complex", "[1:a]volume=0.3[bgm];[0:a][bgm]amix=inputs=2:duration=first[out]",
		"-map", "0:v",
		"-map", "[out]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-shortest",
		outputPath,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("MixBGM: ffmpeg failed: %v\n%s", err, string(out))
		return fmt.Errorf("ffmpeg BGM mix failed: %w", err)
	}
	return nil
}
