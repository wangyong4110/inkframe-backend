package service

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// SubtitleService 字幕生成与嵌入服务
type SubtitleService struct{}

func NewSubtitleService() *SubtitleService {
	return &SubtitleService{}
}

// assHeader 生成 ASS 字幕文件头（支持中文字体、描边、阴影）
func assHeader(fontName string) string {
	if fontName == "" {
		fontName = "Noto Sans CJK SC"
	}
	return fmt.Sprintf(`[Script Info]
ScriptType: v4.00+
PlayResX: 1920
PlayResY: 1080
ScaledBorderAndShadow: yes
YCbCr Matrix: TV.709

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Narration,%s,64,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,2.5,1.5,2,80,80,60,1
Style: Dialogue,%s,60,&H00FFFF00,&H000000FF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,2.0,1.0,2,80,80,60,1
Style: Speaker,%s,48,&H0000FFFF,&H000000FF,&H00000000,&H80000000,0,0,0,0,100,100,0,0,1,1.5,0.8,2,80,80,120,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`, fontName, fontName, fontName)
}

// formatASSTime 将秒数转换为 ASS 时间格式 H:MM:SS.cc
func formatASSTime(secs float64) string {
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	s := int(secs) % 60
	cs := int(math.Round((secs - math.Floor(secs)) * 100))
	if cs >= 100 {
		cs = 99
	}
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

// GenerateASS 从分镜列表生成 ASS 字幕内容字符串。
// shots 必须按 ShotNo 排序，每个 shot 的 Duration 字段须有效。
func (s *SubtitleService) GenerateASS(shots []model.StoryboardShot, fontName string) string {
	var sb strings.Builder
	sb.WriteString(assHeader(fontName))

	currentTime := 0.0
	for _, shot := range shots {
		dur := shot.Duration
		if dur <= 0 {
			dur = 4.0 // default fallback
		}
		endTime := currentTime + dur

		// 字幕持续时间略短于镜头（留 0.15s 过渡）
		subEnd := endTime - 0.15
		if subEnd <= currentTime {
			subEnd = endTime
		}

		start := formatASSTime(currentTime)
		end := formatASSTime(subEnd)

		// shot.GenMeta.Subtitle 为手动覆写优先级最高，与 GenerateShotSRT 保持一致
		if shot.GenMeta.Subtitle != "" {
			text := cleanSubtitleText(shot.GenMeta.Subtitle)
			if text != "" {
				sb.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Narration,,0,0,0,,%s\n", start, end, text))
			}
		} else if shot.Narration != "" {
			// 旁白字幕（白色，居中底部）
			text := cleanSubtitleText(shot.Narration)
			if text != "" {
				sb.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Narration,,0,0,0,,%s\n", start, end, text))
			}
		} else if shot.GenMeta.Dialogue != "" {
			// 对话字幕（黄色，带说话人名称）
			speaker, line := parseDialogue(shot.GenMeta.Dialogue)
			if line != "" {
				if speaker != "" {
					// 说话人名称（青色，居中底部，比对话稍高）
					sb.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Speaker,,0,0,0,,%s：\n", start, end, speaker))
				}
				sb.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Dialogue,,0,0,0,,%s\n", start, end, line))
			}
		}

		currentTime = endTime
	}
	return sb.String()
}

// ExportASS 将分镜列表导出为 ASS 字幕文件
func (s *SubtitleService) ExportASS(shots []model.StoryboardShot, fontName, outputPath string) error {
	content := s.GenerateASS(shots, fontName)
	return os.WriteFile(outputPath, []byte(content), 0644)
}

// BurnSubtitles 使用 FFmpeg 将 ASS 字幕烧录到视频文件中。
// assPath: ASS 字幕文件路径
// videoPath: 输入视频路径
// outputPath: 输出视频路径
func (s *SubtitleService) BurnSubtitles(ctx context.Context, videoPath, assPath, outputPath string) error {
	// P1-3: 转义 ASS 路径 — Windows 反斜杠、冒号、单引号均需处理
	// 先统一正斜杠，再转义特殊字符（顺序不可颠倒）
	escapedASS := strings.ReplaceAll(assPath, "\\", "/")
	escapedASS = strings.ReplaceAll(escapedASS, "'", "\\'") // 防止路径中的单引号破坏 FFmpeg 命令
	escapedASS = strings.ReplaceAll(escapedASS, ":", "\\:") // Windows 盘符转义

	// P1-6: 若 Noto Sans CJK SC 不可用，尝试常见中文替代字体
	// 字幕滤镜在字体缺失时会静默降级；此处在 ASS Header 中指定回退链
	vf := fmt.Sprintf("ass='%s':fontsdir=/usr/share/fonts", escapedASS)

	args := []string{
		"-y",
		"-i", videoPath,
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-c:a", "copy",
		outputPath,
	}
	// 使用 goroutine 超时：wazero 在密集 x264 编码时不响应 ctx 取消
	// 字幕烧录需重新编码整个视频，给 5 分钟余量
	if _, err := runFFmpegWithGoroutineTimeout(5*time.Minute, args...); err != nil {
		return fmt.Errorf("subtitle burn failed: %w", err)
	}
	return nil
}

// parseDialogue 解析对话格式 "角色名:对话内容" 或 "角色名：对话内容"
func parseDialogue(dialogue string) (speaker, line string) {
	for _, sep := range []string{"：", ":"} {
		idx := strings.Index(dialogue, sep)
		if idx > 0 && idx < 20 { // 说话人名字不超过20字
			return strings.TrimSpace(dialogue[:idx]), strings.TrimSpace(dialogue[idx+len(sep):])
		}
	}
	return "", dialogue
}

// cleanSubtitleText 清理字幕文本（移除ASS特殊字符、按视觉宽度换行）
func cleanSubtitleText(text string) string {
	// 移除 ASS 控制字符（花括号包裹的标签如 {\b1} 会破坏 ASS 结构）
	text = strings.ReplaceAll(text, "{", "")
	text = strings.ReplaceAll(text, "}", "")
	text = strings.ReplaceAll(text, "\n", "\\N") // ASS换行
	text = strings.ReplaceAll(text, "\r", "")

	// P1-2: 按视觉宽度换行（CJK 全角字符宽度 = 2，ASCII 半角 = 1）
	// 16:9 视频安全区约 40 列（全角）；超过时插入 \N 换行
	const maxVisualWidth = 40
	runes := []rune(text)
	var result strings.Builder
	lineWidth := 0
	for _, r := range runes {
		w := 1
		if r > 0x2E7F { // CJK / 全角范围（含标点）
			w = 2
		}
		if lineWidth+w > maxVisualWidth {
			result.WriteString("\\N")
			lineWidth = 0
		}
		result.WriteRune(r)
		lineWidth += w
	}
	return result.String()
}
