package service

// video_sync_service.go
//
// 声画同步时间轴：计算每个分镜在最终视频中的绝对时刻，
// 并生成带有精确 adelay 的 FFmpeg 混音脚本，保证视频/配音/音效三轴对齐。

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// ─── 数据结构 ──────────────────────────────────────────────────────────────────

// SFXTimeEvent 一个音效在最终视频时间轴上的绝对时间信息。
type SFXTimeEvent struct {
	Tag           string  `json:"tag"`
	SFXType       string  `json:"sfx_type"`    // action / ambient / emotion
	AbsoluteStart float64 `json:"absolute_start"` // 在最终视频中的绝对开始时刻（秒）
	Duration      float64 `json:"duration"`
	FadeInSecs    float64 `json:"fade_in_secs"`
	FadeOutSecs   float64 `json:"fade_out_secs"`
	URL           string  `json:"url"`
	Volume        float64 `json:"volume"`
	LoopEnabled   bool    `json:"loop_enabled"`
}

// ShotTimeEntry 一个分镜的完整同步时间清单。
type ShotTimeEntry struct {
	ShotID      uint    `json:"shot_id"`
	ShotNo      int     `json:"shot_no"`
	TimelineStart float64 `json:"timeline_start"` // 视频片段开始时刻
	VideoDuration float64 `json:"video_duration"` // 实测（或 target）时长
	VoiceDelay  float64 `json:"voice_delay"`    // 配音偏移（负=J-cut提前）
	VoiceStart  float64 `json:"voice_start"`    // 配音绝对开始时刻
	VoiceDuration float64 `json:"voice_duration"` // 所有配音段落累计时长
	VoiceEnd    float64 `json:"voice_end"`      // 配音结束时刻
	// ok / voice_longer / voice_shorter
	AlignmentMode string         `json:"alignment_mode"`
	SFXEvents     []SFXTimeEvent `json:"sfx_events"`
}

// SyncManifest 整段视频的同步时间清单。
type SyncManifest struct {
	VideoID       uint            `json:"video_id"`
	TotalDuration float64         `json:"total_duration"`  // 最终视频总时长
	ShotCount     int             `json:"shot_count"`
	Shots         []ShotTimeEntry `json:"shots"`
	FFmpegScript  string          `json:"ffmpeg_script"` // 可直接执行的混音命令
}

// ─── ComputeTimeManifest ───────────────────────────────────────────────────────

// ComputeTimeManifest 计算 videoID 下所有分镜的绝对时间轴，更新 timeline_start 到 DB，
// 并返回完整的 SyncManifest（含 FFmpeg 脚本）。
func (s *VideoService) ComputeTimeManifest(videoID uint) (*SyncManifest, error) {
	shots, err := s.storyboardRepo.ListByVideo(videoID)
	if err != nil {
		return nil, fmt.Errorf("list shots: %w", err)
	}
	sort.Slice(shots, func(i, j int) bool { return shots[i].ShotNo < shots[j].ShotNo })

	// 获取 BGM 配置（用于 FFmpeg 脚本）
	var bgmURL string
	var bgmVolume float64 = 0.3
	if s.bgmRepo != nil {
		if segs, e := s.bgmRepo.ListByVideoID(videoID); e == nil && len(segs) > 0 {
			for _, seg := range segs {
				if !seg.Disabled && seg.URL != "" {
					bgmURL = seg.URL
					bgmVolume = seg.Volume
					break
				}
			}
		}
	}

	entries := make([]ShotTimeEntry, 0, len(shots))
	cursor := 0.0

	for _, shot := range shots {
		dur := shot.TaskMeta.ActualVideoDuration
		if dur <= 0 {
			dur = shot.Duration
		}
		if dur <= 0 {
			dur = 5.0 // 兜底
		}

		entry := ShotTimeEntry{
			ShotID:        shot.ID,
			ShotNo:        shot.ShotNo,
			TimelineStart: cursor,
			VideoDuration: dur,
			VoiceDelay:    shot.TaskMeta.VoiceDelay,
			VoiceStart:    cursor + shot.TaskMeta.VoiceDelay,
		}

		// 累加所有配音段落时长
		if s.segmentRepo != nil {
			if segs, e := s.segmentRepo.ListByShotID(shot.ID); e == nil {
				for _, seg := range segs {
					if seg.DurationSecs > 0 {
						entry.VoiceDuration += seg.DurationSecs
					}
				}
			}
		}
		entry.VoiceEnd = entry.VoiceStart + entry.VoiceDuration

		const epsilon = 0.5 // 0.5s 容忍误差
		switch {
		case entry.VoiceDuration <= 0:
			entry.AlignmentMode = "no_voice"
		case math.Abs(entry.VoiceDuration-dur) <= epsilon:
			entry.AlignmentMode = "ok"
		case entry.VoiceDuration > dur+epsilon:
			entry.AlignmentMode = "voice_longer"
		default:
			entry.AlignmentMode = "voice_shorter"
		}

		// 音效绝对时刻
		if s.sfxService != nil {
			if items, e := s.sfxService.ListSFXItems(shot.ID); e == nil {
				for _, item := range items {
					if item.Disabled || item.URL == "" {
						continue
					}
					sfxDur := item.DurationSecs
					if item.LoopEnabled && sfxDur <= 0 {
						sfxDur = dur // 环境音持续整个镜头
					}
					entry.SFXEvents = append(entry.SFXEvents, SFXTimeEvent{
						Tag:           item.Tag,
						SFXType:       item.SFXType,
						AbsoluteStart: cursor + item.StartOffset,
						Duration:      sfxDur,
						FadeInSecs:    float64(item.FadeInMs) / 1000.0,
						FadeOutSecs:   float64(item.FadeOutMs) / 1000.0,
						URL:           item.URL,
						Volume:        item.Volume,
						LoopEnabled:   item.LoopEnabled,
					})
				}
			}
		}

		entries = append(entries, entry)

		// 更新 DB 中的 timeline_start（幂等）
		if e := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
			"timeline_start": cursor,
		}); e != nil {
			logger.Errorf("[SyncManifest] update timeline_start shot %d: %v", shot.ShotNo, e)
		}

		cursor += dur
	}

	manifest := &SyncManifest{
		VideoID:       videoID,
		TotalDuration: cursor,
		ShotCount:     len(entries),
		Shots:         entries,
	}
	manifest.FFmpegScript = buildFFmpegSyncScript(entries, bgmURL, bgmVolume, "output.mp4")
	return manifest, nil
}

// ─── FFmpeg 脚本生成 ────────────────────────────────────────────────────────────

// buildFFmpegSyncScript 生成一条完整的 FFmpeg 命令，精确对齐视频/配音/音效。
//
// 结构：
//   输入流：视频片段（-i clip1.mp4 -i clip2.mp4 …）
//         + 配音音频（-i voice1.mp3 …）
//         + 音效（-i sfx1.mp3 …）
//         + BGM（可选）
//   filter_complex:
//     视频：[0:v][1:v]…concat=n=N:v=1:a=0[vout]
//     配音：各流 adelay=T*1000 → amix
//     音效：各流 adelay=T*1000, volume=V → amix
//     BGM：adelay=0, volume=V → amix（可与人声闪避）
//     总混：amix → [aout]
//   输出：-map [vout] -map [aout] -c:v copy -c:a aac output.mp4
func buildFFmpegSyncScript(shots []ShotTimeEntry, bgmURL string, bgmVolume float64, outputPath string) string {
	if len(shots) == 0 {
		return ""
	}

	var inputs []string  // -i 参数列表
	var filters []string // filter_complex 内容
	var audioMixParts []string

	// ── 视频输入 & concat ──────────────────────────────────────────────────────
	// （注：实际使用时用户需将占位符替换为真实路径）
	for i, sh := range shots {
		clipSource := fmt.Sprintf("shot_%d_clip.mp4", sh.ShotNo)
		inputs = append(inputs, fmt.Sprintf("-i %q", clipSource))
		_ = i
	}
	// 视频 concat filter
	concatInputs := ""
	for i := range shots {
		concatInputs += fmt.Sprintf("[%d:v]", i)
	}
	videoInputCount := len(shots)
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[vout]", concatInputs, videoInputCount))

	audioIdx := videoInputCount // 音频流从此下标开始

	// ── 配音输入（每个分镜的所有 voice segment） ──────────────────────────────
	type voiceInput struct {
		url        string
		delayMs    int64
		vol        float64
		filterLabel string
	}
	var voiceInputs []voiceInput

	for _, sh := range shots {
		// 占位符：实际路径由调用方填入
		voiceURL := fmt.Sprintf("shot_%d_voice.mp3", sh.ShotNo)
		if sh.VoiceDuration > 0 {
			delayMs := int64(math.Max(0, sh.VoiceStart) * 1000)
			voiceInputs = append(voiceInputs, voiceInput{
				url:     voiceURL,
				delayMs: delayMs,
				vol:     0.9,
			})
		}
	}

	for i, v := range voiceInputs {
		inputs = append(inputs, fmt.Sprintf("-i %q", v.url))
		label := fmt.Sprintf("[voice%d]", i)
		filters = append(filters, fmt.Sprintf("[%d:a]adelay=%d|%d,volume=%.2f%s",
			audioIdx, v.delayMs, v.delayMs, v.vol, label))
		audioMixParts = append(audioMixParts, label)
		audioIdx++
	}

	// ── 音效输入 ──────────────────────────────────────────────────────────────
	type sfxInput struct {
		url     string
		delayMs int64
		vol     float64
		loop    bool
		dur     float64
	}
	var sfxInputs []sfxInput
	for _, sh := range shots {
		for _, ev := range sh.SFXEvents {
			if ev.URL == "" {
				continue
			}
			sfxInputs = append(sfxInputs, sfxInput{
				url:     ev.URL,
				delayMs: int64(math.Max(0, ev.AbsoluteStart) * 1000),
				vol:     ev.Volume,
				loop:    ev.LoopEnabled,
				dur:     ev.Duration,
			})
		}
	}
	for i, sfx := range sfxInputs {
		inputs = append(inputs, fmt.Sprintf("-i %q", sfx.url))
		label := fmt.Sprintf("[sfx%d]", i)
		var loopFilter string
		if sfx.loop && sfx.dur > 0 {
			loopFilter = fmt.Sprintf("aloop=loop=-1:size=2e+09,atrim=duration=%.3f,", sfx.dur)
		}
		filters = append(filters, fmt.Sprintf("[%d:a]%sadelay=%d|%d,volume=%.2f%s",
			audioIdx, loopFilter, sfx.delayMs, sfx.delayMs, sfx.vol, label))
		audioMixParts = append(audioMixParts, label)
		audioIdx++
	}

	// ── BGM 输入 ──────────────────────────────────────────────────────────────
	if bgmURL != "" {
		inputs = append(inputs, fmt.Sprintf("-i %q", bgmURL))
		label := "[bgm]"
		filters = append(filters, fmt.Sprintf("[%d:a]aloop=loop=-1:size=2e+09,volume=%.2f%s",
			audioIdx, bgmVolume, label))
		audioMixParts = append(audioMixParts, label)
	}

	// ── amix 总混 ─────────────────────────────────────────────────────────────
	if len(audioMixParts) > 0 {
		mixInputs := strings.Join(audioMixParts, "")
		if len(audioMixParts) == 1 {
			filters = append(filters, fmt.Sprintf("%saresample=44100[aout]", mixInputs))
		} else {
			filters = append(filters, fmt.Sprintf("%samix=inputs=%d:duration=longest:dropout_transition=2,aresample=44100[aout]",
				mixInputs, len(audioMixParts)))
		}
	}

	// ── 组装命令 ──────────────────────────────────────────────────────────────
	var sb strings.Builder
	sb.WriteString("ffmpeg \\\n")
	for _, inp := range inputs {
		sb.WriteString("  " + inp + " \\\n")
	}
	sb.WriteString("  -filter_complex \"\n")
	for i, f := range filters {
		if i < len(filters)-1 {
			sb.WriteString("    " + f + ";\n")
		} else {
			sb.WriteString("    " + f + "\n")
		}
	}
	sb.WriteString("  \" \\\n")
	if len(audioMixParts) > 0 {
		sb.WriteString("  -map '[vout]' -map '[aout]' \\\n")
		sb.WriteString("  -c:v copy -c:a aac -b:a 192k \\\n")
	} else {
		sb.WriteString("  -map '[vout]' \\\n")
		sb.WriteString("  -c:v copy \\\n")
	}
	sb.WriteString(fmt.Sprintf("  %q\n", outputPath))
	return sb.String()
}
