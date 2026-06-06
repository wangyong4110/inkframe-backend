package service

// video_asset_service.go
//
// Voice-segment CRUD and per-shot audio generation methods
// extracted from video_service.go. All methods remain on *VideoService.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"gorm.io/gorm"
)

// ─── Voice Segment Types ──────────────────────────────────────────────────────

// VoiceSegmentInput 创建/更新语音段落时的输入
type VoiceSegmentInput struct {
	Text    string `json:"text"`
	Speaker string `json:"speaker"`  // 空串=旁白，非空=角色名（对白）
	VoiceID string `json:"voice_id"` // TTS 声音 ID，空串=自动
}

// ─── Voice Segment CRUD ───────────────────────────────────────────────────────

// GetVoiceSegment 按 ID 获取单个语音段落
func (s *VideoService) GetVoiceSegment(segID uint) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	return s.segmentRepo.GetByID(segID)
}

// ListVoiceSegments 获取分镜的所有语音段落
func (s *VideoService) ListVoiceSegments(shotID uint) ([]*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	return s.segmentRepo.ListByShotID(shotID)
}

// AppendVoiceSegment 追加段落到分镜末尾
func (s *VideoService) AppendVoiceSegment(shotID uint, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	maxSeq, err := s.segmentRepo.MaxSeqNo(shotID)
	if err != nil {
		return nil, err
	}
	seg := &model.ShotVoiceSegment{
		ShotID:  shotID,
		SeqNo:   maxSeq + 1,
		Text:    input.Text,
		Speaker: input.Speaker,
		VoiceID: input.VoiceID,
	}
	return seg, s.segmentRepo.Create(seg)
}

// InsertVoiceSegment 在 afterSeqNo 之后插入新段落（afterSeqNo=0 表示插入到最前）
func (s *VideoService) InsertVoiceSegment(shotID uint, afterSeqNo int, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	newSeqNo := afterSeqNo + 1
	seg := &model.ShotVoiceSegment{
		ShotID:  shotID,
		SeqNo:   newSeqNo,
		Text:    input.Text,
		Speaker: input.Speaker,
		VoiceID: input.VoiceID,
	}
	// Shift + create must be atomic to avoid a corrupt seq_no sequence on partial failure.
	// The shift runs first (before the insert) so the unique constraint on (shot_id, seq_no)
	// is never violated within the transaction.
	err := s.segmentRepo.DB().Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec(
			"UPDATE ink_shot_voice_segment SET seq_no = seq_no + 1 WHERE shot_id = ? AND seq_no >= ? AND deleted_at IS NULL",
			shotID, newSeqNo,
		).Error; e != nil {
			return e
		}
		// Verify no duplicate seqno exists after shifting (defensive check)
		var existing model.ShotVoiceSegment
		if e := tx.Where("shot_id = ? AND seq_no = ? AND deleted_at IS NULL", shotID, newSeqNo).
			First(&existing).Error; e == nil {
			return fmt.Errorf("voice segment with seq_no %d already exists for shot %d after shift", newSeqNo, shotID)
		}
		return tx.Create(seg).Error
	})
	if err != nil {
		return nil, err
	}
	return seg, nil
}

// UpdateVoiceSegment 更新段落文本/说话人/声音
func (s *VideoService) UpdateVoiceSegment(segID uint, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return nil, err
	}
	seg.Text = input.Text
	seg.Speaker = input.Speaker
	seg.VoiceID = input.VoiceID
	return seg, s.segmentRepo.Update(seg)
}

// DeleteVoiceSegment 删除段落并将后续段落 seq_no 前移（保持连续）
func (s *VideoService) DeleteVoiceSegment(segID uint) error {
	if s.segmentRepo == nil {
		return fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return err
	}
	if err := s.segmentRepo.Delete(segID); err != nil {
		return err
	}
	return s.segmentRepo.CompactSeqNosAfter(seg.ShotID, seg.SeqNo)
}

// ─── Audio Helpers ────────────────────────────────────────────────────────────

// mp3Duration estimates the duration in seconds of MP3 audio data by counting frames.
// Supports MPEG1 Layer3 (44.1/48/32 kHz) and MPEG2 Layer3 (22.05/24/16 kHz, used by
// doubao-speech and other TTS providers). Returns 0 if the data cannot be parsed.
func mp3Duration(data []byte) float64 {
	if len(data) < 4 {
		return 0
	}
	// Skip ID3v2 tag if present
	offset := 0
	if len(data) >= 10 && data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		sz := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
		offset = 10 + sz
	}
	// MPEG1 Layer3 bitrate table (kbps)
	bitratesMPEG1 := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	// MPEG2/2.5 Layer3 bitrate table (kbps)
	bitratesMPEG2 := [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	// Sample rates indexed by srIdx (0-2); MPEG2.5 = MPEG2/2
	sampleRatesMPEG1 := [4]int{44100, 48000, 32000, 0}
	sampleRatesMPEG2 := [4]int{22050, 24000, 16000, 0}

	var frames, sampleRate, samplesPerFrame int
	for i := offset; i+3 < len(data); {
		if data[i] != 0xFF || (data[i+1]&0xE0) != 0xE0 {
			i++
			continue
		}
		// Layer must be Layer3 (bits 2-1 = 01)
		if (data[i+1] & 0x06) != 0x02 {
			i++
			continue
		}
		mpegVer := (data[i+1] >> 3) & 0x03 // 11=MPEG1, 10=MPEG2, 00=MPEG2.5
		bitrateIdx := int(data[i+2]>>4) & 0x0F
		srIdx := int(data[i+2]>>2) & 0x03
		padding := int(data[i+2]>>1) & 0x01

		var bitrate, sr, spf int
		switch mpegVer {
		case 0x03: // MPEG1: 1152 samples/frame
			bitrate = bitratesMPEG1[bitrateIdx] * 1000
			sr = sampleRatesMPEG1[srIdx]
			spf = 1152
		case 0x02: // MPEG2: 576 samples/frame
			bitrate = bitratesMPEG2[bitrateIdx] * 1000
			sr = sampleRatesMPEG2[srIdx]
			spf = 576
		case 0x00: // MPEG2.5: 576 samples/frame, half the MPEG2 sample rates
			bitrate = bitratesMPEG2[bitrateIdx] * 1000
			sr = sampleRatesMPEG2[srIdx] / 2
			spf = 576
		default:
			i++
			continue
		}
		if bitrate == 0 || sr == 0 {
			i++
			continue
		}
		frameLen := spf/8*bitrate/sr + padding
		if frameLen <= 4 || i+frameLen > len(data) {
			break
		}
		frames++
		if sampleRate == 0 {
			sampleRate = sr
			samplesPerFrame = spf
		}
		i += frameLen
	}
	if frames == 0 || sampleRate == 0 {
		return 0
	}
	return float64(frames) * float64(samplesPerFrame) / float64(sampleRate)
}

// alignShotDurationToTTS 检查分镜的 TTS 音频时长，若音频更长则延伸分镜时长以确保配音完整。
// 返回调整后的时长（秒）；无法读取音频时返回原 shot.Duration。
// 注意：此函数仅用于当次生成，不持久化回数据库。
func alignShotDurationToTTS(shot *model.StoryboardShot) float64 {
	if shot.AudioPath == "" {
		return shot.Duration
	}
	data, err := readLocalOrRemoteFile(shot.AudioPath)
	if err != nil || len(data) == 0 {
		return shot.Duration
	}
	ext := audioExtension(shot.AudioPath)
	var audioDur float64
	if ext == ".mp3" {
		audioDur = mp3Duration(data)
	} else {
		micros := parseAudioDurationMicros(data, ext)
		if micros > 0 {
			audioDur = float64(micros) / 1_000_000.0
		}
	}
	if audioDur <= 0 {
		return shot.Duration
	}
	const buffer = 0.3
	needed := audioDur + buffer
	if needed > shot.Duration {
		logger.Printf("[VideoService] alignShotDurationToTTS: shot %d duration %.1fs → %.1fs (TTS=%.1fs)",
			shot.ShotNo, shot.Duration, needed, audioDur)
		return needed
	}
	return shot.Duration
}

// GenerateSegmentAudio 为单条语音段落生成 TTS 音频
func (s *VideoService) GenerateSegmentAudio(segID uint, tenantID uint, defaultVoice string) error {
	if s.segmentRepo == nil {
		return fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		return fmt.Errorf("segment %d not found: %w", segID, err)
	}
	text := stripDialogueSpeakerPrefix(seg.Text)
	if text == "" {
		return nil
	}

	// 预加载 shot + video 一次，同时用于：① 角色声音查找 ② EmotionalTone ③ OSS 存储 key
	var novelID, chapterID uint
	var shotEmotionalTone string
	if s.storyboardRepo != nil && s.videoRepo != nil {
		if shot, e := s.storyboardRepo.GetByID(seg.ShotID); e == nil {
			shotEmotionalTone = shot.EmotionalTone
			if video, e := s.videoRepo.GetByID(shot.VideoID); e == nil {
				novelID = video.NovelID
				if video.ChapterID != nil {
					chapterID = *video.ChapterID
				}
			}
		}
	}

	// 确定 TTS 声音：段落级 > 角色声音查找（带缓存）> 默认
	voice := seg.VoiceID
	speed := 1.0
	style := ""
	if voice == "" && seg.Speaker != "" && s.characterRepo != nil && novelID > 0 {
		if chars, e := s.listCharsByNovelCached(novelID); e == nil {
			autoVoices := []string{"alloy", "echo", "fable", "nova", "onyx", "shimmer"}
			for _, c := range chars {
				if strings.EqualFold(c.Name, seg.Speaker) {
					if c.VoiceID != "" {
						voice = c.VoiceID
					} else {
						voice = autoVoices[c.ID%uint(len(autoVoices))]
					}
					style = c.VoiceStyle // 角色静态风格作为基准情感
					break
				}
			}
		}
	}
	// 分镜情绪基调覆盖角色静态风格（动态优先级更高）
	if shotEmotionalTone != "" {
		if mapped := mapEmotionalToneToTTS(shotEmotionalTone); mapped != "" {
			style = mapped
		}
	}
	if voice == "" {
		voice = defaultVoice
	}
	if voice == "" {
		voice = "alloy"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	audioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, text, voice, speed, style)
	if err != nil {
		// Clear stale audio_path so the UI shows generation failed rather than showing an old path.
		if seg.AudioPath != "" {
			if clearErr := s.segmentRepo.UpdateFields(segID, map[string]interface{}{"audio_path": ""}); clearErr != nil {
				logger.Errorf("[VideoService] GenerateSegmentAudio: clear audio_path for segment %d: %v", segID, clearErr)
			}
		}
		return fmt.Errorf("TTS failed for segment %d: %w", segID, err)
	}
	if audioURL == "" {
		if seg.AudioPath != "" {
			if clearErr := s.segmentRepo.UpdateFields(segID, map[string]interface{}{"audio_path": ""}); clearErr != nil {
				logger.Errorf("[VideoService] GenerateSegmentAudio: clear audio_path for segment %d: %v", segID, clearErr)
			}
		}
		return fmt.Errorf("TTS returned empty URL for segment %d", segID)
	}

	// Download audio bytes (needed for OSS upload and duration calculation)
	var audioData []byte
	if strings.HasPrefix(audioURL, "file://") {
		audioData, err = os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
	} else {
		if resp, e := http.Get(audioURL); e == nil { //nolint:gosec
			audioData, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		} else {
			err = e
		}
	}
	if err != nil {
		logger.Printf("warn: could not read audio for segment %d: %v", segID, err)
	}

	// 上传到持久存储（如果配置了 storageSvc）
	// key 格式：novels/{novelID}/chapters/{chapterID}/audio/seg-{segID}.mp3
	if s.storageSvc != nil && len(audioData) > 0 {
		filename := fmt.Sprintf("seg-%d.mp3", segID)
		key := storage.BuildKey(novelID, chapterID, "audio", filename)
		ossURL, e := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(audioData), int64(len(audioData)), "audio/mpeg")
		if e != nil {
			logger.Printf("GenerateSegmentAudio: OSS upload failed for segment %d: %v", segID, e)
			return e
		}
		if strings.HasPrefix(audioURL, "file://") {
			os.Remove(strings.TrimPrefix(audioURL, "file://")) //nolint:errcheck
		}
		audioURL = ossURL
	}

	// Persist audio path + measured duration
	fields := map[string]interface{}{"audio_path": audioURL}
	if d := mp3Duration(audioData); d > 0 {
		fields["duration_secs"] = d
	}
	if err := s.segmentRepo.UpdateFields(segID, fields); err != nil {
		logger.Printf("[VideoService] GenerateSegmentAudio: failed to update segment %d fields: %v", segID, err)
	}

	// 配音生成完成后，同步更新分镜时长：取视频时长与所有配音段落累计时长中的较大值
	s.syncShotDurationAfterVoice(seg.ShotID)

	return nil
}

// syncShotDurationAfterVoice 累加该分镜所有配音段落的 duration_secs，
// 若合计时长超过当前分镜时长，则更新分镜 duration 为二者中较大值。
// 若所有配音均失败（totalVoice==0），将分镜时长设置为默认最小值（defaultShotDurationSecs）
// 以避免 shot.Duration 为零导致后续 FFmpeg 处理出错。
func (s *VideoService) syncShotDurationAfterVoice(shotID uint) {
	if s.segmentRepo == nil {
		return
	}
	segs, err := s.segmentRepo.ListByShotID(shotID)
	if err != nil || len(segs) == 0 {
		return
	}
	var totalVoice float64
	for _, sg := range segs {
		if sg.DurationSecs > 0 {
			totalVoice += sg.DurationSecs
		}
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil || shot == nil {
		return
	}
	if totalVoice <= 0 {
		// 所有配音段落均失败：确保分镜时长有合理默认值，不跳过更新
		if shot.Duration <= 0 {
			if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"duration": defaultShotDurationSecs}); err != nil {
				logger.Printf("[VideoService] syncShotDurationAfterVoice: failed to set default duration for shot %d: %v", shotID, err)
			}
		}
		return
	}
	if totalVoice <= shot.Duration {
		return // 配音比当前时长短，不需要更新
	}
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"duration": totalVoice}); err != nil {
		logger.Printf("[VideoService] syncShotDurationAfterVoice: failed to update shot %d duration: %v", shotID, err)
	}
}

// GenerateShotAudio 为单个分镜生成 TTS 音频（同步），生成后更新 shot.AudioPath
func (s *VideoService) GenerateShotAudio(shot *model.StoryboardShot, tenantID uint, narrationVoice string) error {
	// 阻塞等待信号量槽位：audioSem 用于限速（防 429），不应跳过。
	// 注意：此函数始终在后台 goroutine 中调用，阻塞等待是正确行为。
	if s.audioSem != nil {
		s.audioSem <- struct{}{}
		defer func() { <-s.audioSem }()
	}

	// Skip if audio already generated (idempotency — prevents re-billing TTS on retry)
	if shot.AudioPath != "" {
		return nil
	}

	// If the shot has voice segments, delegate to segment-aware stitching logic.
	if s.segmentRepo != nil {
		segs, err := s.segmentRepo.ListByShotID(shot.ID)
		if err == nil && len(segs) > 0 {
			return s.generateShotAudioFromSegments(shot, segs, tenantID, narrationVoice)
		}
	}

	// Determine the text to synthesize
	text := shot.Narration
	if text == "" {
		text = stripDialogueSpeakerPrefix(shot.Dialogue)
	}
	if text == "" {
		text = shot.Description
	}
	if text == "" {
		return nil
	}

	// 需要 novelID 以便角色声音查询和存储 key
	var novelID, chapterID uint
	if s.videoRepo != nil {
		if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
			novelID = video.NovelID
			if video.ChapterID != nil {
				chapterID = *video.ChapterID
			}
		}
	}

	voice, speed, style := s.resolveVoiceForShot(shot, narrationVoice, novelID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	localAudioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, text, voice, speed, style)
	if err != nil {
		logger.Printf("GenerateShotAudio: TTS failed for shot %d: %v", shot.ShotNo, err)
		return err
	}
	if localAudioURL == "" {
		logger.Printf("GenerateShotAudio: TTS returned empty URL for shot %d", shot.ShotNo)
		return nil
	}

	audioURL := localAudioURL

	// 在上传/删除本地文件之前先测量时长，保证能读到本地音频数据
	shot.AudioPath = localAudioURL
	shot.Duration = alignShotDurationToTTS(shot)

	// 上传到持久存储（持久化音频避免本地 /tmp 文件重启后消失）
	if s.storageSvc != nil {
		persistURL, uploadErr := s.uploadAudioToStorage(ctx, shot, audioURL, novelID, chapterID)
		if uploadErr != nil {
			logger.Printf("GenerateShotAudio: storage upload failed (falling back to local): %v", uploadErr)
		} else {
			audioURL = persistURL
			logger.Printf("GenerateShotAudio: shot %d audio stored at %s", shot.ShotNo, audioURL)
			// 删除本次新建的 /tmp 临时文件（时长已测量完毕，可以安全删除）
			if strings.HasPrefix(localAudioURL, "file://") {
				os.Remove(strings.TrimPrefix(localAudioURL, "file://")) //nolint:errcheck
			}
		}
	}

	shot.AudioPath = audioURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("[VideoService] GenerateShotAudio: failed to update shot %d audio path: %v", shot.ShotNo, err)
	}
	return nil
}

// uploadAudioToStorage 读取 TTS 输出（file:// 路径或 HTTP URL），上传并返回持久 URL。
// novelID/chapterID 由调用方提供，避免重复查询 video 记录。
func (s *VideoService) uploadAudioToStorage(ctx context.Context, shot *model.StoryboardShot, audioURL string, novelID, chapterID uint) (string, error) {
	var data []byte
	var readErr error

	if strings.HasPrefix(audioURL, "file://") {
		data, readErr = os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
	} else if strings.HasPrefix(audioURL, "http://") || strings.HasPrefix(audioURL, "https://") {
		resp, err := http.Get(audioURL) //nolint:gosec
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		data, readErr = io.ReadAll(resp.Body)
	} else {
		return "", fmt.Errorf("unsupported audio URL scheme: %s", audioURL)
	}
	if readErr != nil {
		return "", readErr
	}

	filename := fmt.Sprintf("shot-%d.mp3", shot.ID)
	key := storage.BuildKey(novelID, chapterID, "audio", filename)
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}

// GenerateShotSRT 根据分镜的台词/旁白和时长生成单条 SRT 字幕内容。
// 时间码从 00:00:00,000 开始，结束时间 = shot.Duration。
// 文本优先级：Dialogue > Narration > Description（兜底兼容旧数据）。
func GenerateShotSRT(shot *model.StoryboardShot) string {
	var text string
	if shot.Subtitle != "" {
		text = shot.Subtitle
	} else if shot.Dialogue != "" {
		// 去除"角色名："前缀，字幕只显示台词内容
		text = stripDialogueSpeakerPrefix(shot.Dialogue)
	} else if shot.Narration != "" {
		text = shot.Narration
	} else {
		text = shot.Description
	}
	if text == "" {
		return ""
	}
	dur := shot.Duration
	if dur <= 0 {
		dur = 5.0
	}
	end := formatSRTTimecode(dur)
	return fmt.Sprintf("1\n00:00:00,000 --> %s\n%s\n", end, text)
}

// formatSRTTimecode 将秒数格式化为 SRT 时间码 HH:MM:SS,mmm
func formatSRTTimecode(secs float64) string {
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	s := int(secs) % 60
	ms := int((secs-float64(int(secs)))*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// generateShotAudioFromSegments generates TTS for each segment that lacks audio,
// then stitches all segment audio files into a single track using ffmpeg and
// uploads the result to storage, finally updating shot.AudioPath.
func (s *VideoService) generateShotAudioFromSegments(shot *model.StoryboardShot, segs []*model.ShotVoiceSegment, tenantID uint, defaultVoice string) error {
	// 1. For each segment without audio, call GenerateSegmentAudio
	for _, seg := range segs {
		if seg.AudioPath == "" && seg.Text != "" {
			if err := s.GenerateSegmentAudio(seg.ID, tenantID, defaultVoice); err != nil {
				logger.Printf("generateShotAudioFromSegments: segment %d TTS failed: %v", seg.ID, err)
			}
		}
	}

	// 2. Reload segments to get updated AudioPath values
	freshSegs, err := s.segmentRepo.ListByShotID(shot.ID)
	if err != nil || len(freshSegs) == 0 {
		return nil
	}

	// 3. Collect local audio paths (download http URLs to temp files)
	tmpDir, err := os.MkdirTemp("", "inkframe_seg_stitch_*")
	if err != nil {
		return fmt.Errorf("generateShotAudioFromSegments: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	var localPaths []string
	for _, seg := range freshSegs {
		if seg.AudioPath == "" {
			continue
		}
		localPath, err := fetchAudioToLocal(tmpDir, seg.AudioPath, int(seg.ID))
		if err != nil {
			logger.Printf("generateShotAudioFromSegments: fetch segment %d audio: %v", seg.ID, err)
			continue
		}
		localPaths = append(localPaths, localPath)
	}
	if len(localPaths) == 0 {
		return nil
	}
	if len(localPaths) == 1 {
		// Only one segment with audio — use it directly without ffmpeg
		shot.AudioPath = freshSegs[0].AudioPath
		shot.Duration = alignShotDurationToTTS(shot)
		return s.storyboardRepo.Update(shot)
	}

	// 4. Stitch with ffmpeg concat
	listFile := filepath.Join(tmpDir, "concat.txt")
	var lines []string
	for _, p := range localPaths {
		lines = append(lines, fmt.Sprintf("file '%s'", p))
	}
	if err := os.WriteFile(listFile, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return fmt.Errorf("generateShotAudioFromSegments: write list: %w", err)
	}
	stitchedPath := filepath.Join(tmpDir, fmt.Sprintf("shot_%d_stitched.mp3", shot.ID))
	out, ffmpegErr := runFFmpegCtx(context.Background(),
		"-y", "-f", "concat", "-safe", "0", "-i", listFile,
		"-c", "copy", stitchedPath,
	)
	if ffmpegErr != nil {
		logger.Printf("generateShotAudioFromSegments: ffmpeg failed: %v\n%s", ffmpegErr, string(out))
		// fallback: use first segment audio
		shot.AudioPath = freshSegs[0].AudioPath
		return s.storyboardRepo.Update(shot)
	}

	stitchedData, err := os.ReadFile(stitchedPath)
	if err != nil {
		return fmt.Errorf("generateShotAudioFromSegments: read stitched: %w", err)
	}

	// 5. Upload stitched audio to persistent storage
	audioURL := "file://" + stitchedPath
	if s.storageSvc != nil && len(stitchedData) > 0 {
		var novelID, chapterID uint
		if video, e := s.videoRepo.GetByID(shot.VideoID); e == nil {
			novelID = video.NovelID
			if video.ChapterID != nil {
				chapterID = *video.ChapterID
			}
		}
		key := storage.BuildKey(novelID, chapterID, "audio", fmt.Sprintf("shot-%d-stitched.mp3", shot.ID))
		if ossURL, e := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(stitchedData), int64(len(stitchedData)), "audio/mpeg"); e == nil {
			audioURL = ossURL
		} else {
			logger.Printf("generateShotAudioFromSegments: OSS upload failed for shot %d: %v", shot.ID, e)
		}
	}

	shot.AudioPath = audioURL
	shot.Duration = alignShotDurationToTTS(shot)
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Printf("generateShotAudioFromSegments: update shot %d: %v", shot.ID, err)
	}
	return nil
}

// MergeVoiceSegments stitches already-generated segment audio files for a shot into a single
// combined audio track and updates the shot's AudioPath. Only segments with a non-empty
// AudioPath are included. Returns the merged audio URL.
func (s *VideoService) MergeVoiceSegments(ctx context.Context, shotID, tenantID uint) (string, error) {
	if s.segmentRepo == nil || s.storyboardRepo == nil {
		return "", fmt.Errorf("segment or storyboard repository not configured")
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return "", fmt.Errorf("shot %d not found: %w", shotID, err)
	}
	segs, err := s.segmentRepo.ListByShotID(shotID)
	if err != nil {
		return "", fmt.Errorf("list segments for shot %d: %w", shotID, err)
	}
	// Filter to only segments that already have audio.
	var ready []*model.ShotVoiceSegment
	for _, seg := range segs {
		if seg.AudioPath != "" {
			ready = append(ready, seg)
		}
	}
	if len(ready) == 0 {
		return "", fmt.Errorf("no generated segment audio found for shot %d", shotID)
	}
	if len(ready) == 1 {
		shot.AudioPath = ready[0].AudioPath
		if err := s.storyboardRepo.Update(shot); err != nil {
			logger.Errorf("[VideoService] MergeVoiceSegments: storyboardRepo.Update shot %d: %v", shot.ID, err)
		}
		return shot.AudioPath, nil
	}

	tmpDir, err := os.MkdirTemp("", "inkframe_merge_*")
	if err != nil {
		return "", fmt.Errorf("MergeVoiceSegments: mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	var localPaths []string
	for _, seg := range ready {
		lp, err := fetchAudioToLocal(tmpDir, seg.AudioPath, int(seg.ID))
		if err != nil {
			logger.Printf("MergeVoiceSegments: fetch segment %d audio: %v", seg.ID, err)
			continue
		}
		localPaths = append(localPaths, lp)
	}
	if len(localPaths) == 0 {
		return "", fmt.Errorf("MergeVoiceSegments: all audio fetches failed for shot %d", shotID)
	}

	listFile := filepath.Join(tmpDir, "concat.txt")
	var buf strings.Builder
	for _, p := range localPaths {
		buf.WriteString(fmt.Sprintf("file '%s'\n", p))
	}
	if err := os.WriteFile(listFile, []byte(buf.String()), 0600); err != nil {
		return "", fmt.Errorf("MergeVoiceSegments: write list: %w", err)
	}

	outPath := filepath.Join(tmpDir, "merged.mp3")
	out, ffmpegErr := runFFmpegCtx(ctx, "-y", "-f", "concat", "-safe", "0", "-i", listFile, "-c", "copy", outPath)
	if ffmpegErr != nil {
		return "", fmt.Errorf("MergeVoiceSegments: ffmpeg failed: %v\n%s", ffmpegErr, string(out))
	}

	merged, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("MergeVoiceSegments: read merged: %w", err)
	}

	var audioURL string
	if s.storageSvc != nil && len(merged) > 0 {
		var novelID, chapterID uint
		if s.videoRepo != nil {
			if video, e := s.videoRepo.GetByID(shot.VideoID); e == nil {
				novelID = video.NovelID
				if video.ChapterID != nil {
					chapterID = *video.ChapterID
				}
			}
		}
		key := storage.BuildKey(novelID, chapterID, "audio", fmt.Sprintf("shot-%d-merged.mp3", shotID))
		if ossURL, e := s.storageSvc.Upload(ctx, key, bytes.NewReader(merged), int64(len(merged)), "audio/mpeg"); e == nil {
			audioURL = ossURL
		} else {
			logger.Printf("MergeVoiceSegments: OSS upload failed: %v", e)
			return "", fmt.Errorf("MergeVoiceSegments: upload failed: %w", e)
		}
	} else {
		audioURL = "file://" + outPath
	}

	shot.AudioPath = audioURL
	if err := s.storyboardRepo.Update(shot); err != nil {
		logger.Errorf("[VideoService] MergeVoiceSegments: storyboardRepo.Update shot %d: %v", shot.ID, err)
	}
	return audioURL, nil
}

// fetchAudioToLocal downloads or copies an audio file to a local temp path.
// Supports file:// paths and http/https URLs.
func fetchAudioToLocal(dir, audioURL string, id int) (string, error) {
	localPath := filepath.Join(dir, fmt.Sprintf("seg_%d.mp3", id))
	if strings.HasPrefix(audioURL, "file://") {
		data, err := os.ReadFile(strings.TrimPrefix(audioURL, "file://"))
		if err != nil {
			return "", err
		}
		return localPath, os.WriteFile(localPath, data, 0600)
	}
	if strings.HasPrefix(audioURL, "http://") || strings.HasPrefix(audioURL, "https://") {
		resp, err := http.Get(audioURL) //nolint:gosec
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		return localPath, os.WriteFile(localPath, data, 0600)
	}
	return "", fmt.Errorf("unsupported URL scheme: %s", audioURL)
}

// resolveVoiceForShot 解析分镜对应角色的配音设置（voice, speed, style）。
// 优先级：① 对话文本「角色名：」前缀 → ② shot.CharacterIDs 第一个角色 → ③ narrationVoice → ④ alloy。
// novelID 由调用方提供（避免此函数重复查询 video 记录）。
func (s *VideoService) resolveVoiceForShot(shot *model.StoryboardShot, narrationVoice string, novelID uint) (voice string, speed float64, style string) {
	voice = "alloy"
	if narrationVoice != "" {
		voice = narrationVoice
	}
	speed = 1.0

	if novelID == 0 || s.characterRepo == nil {
		return
	}

	// 步骤一：从对话中解析发言角色（格式：角色名：对话内容 或 角色名:对话内容）
	speakerName := ""
	for _, sep := range []string{"：", ":"} {
		if idx := strings.Index(shot.Dialogue, sep); idx > 0 && idx < 20 {
			speakerName = strings.TrimSpace(shot.Dialogue[:idx])
			break
		}
	}

	applyCharVoice := func(c *model.Character) {
		if c.VoiceID != "" {
			voice = c.VoiceID
		} else {
			// 角色无显式 voice_id 时，按角色 ID 取模自动分配内置音色。
			// 保证不同角色始终使用不同音色，无需手动配置。
			autoVoices := []string{"alloy", "echo", "fable", "nova", "onyx", "shimmer"}
			voice = autoVoices[c.ID%uint(len(autoVoices))]
		}
		if c.VoiceSpeed > 0 {
			speed = c.VoiceSpeed
		}
		style = c.VoiceStyle
	}

	if speakerName != "" {
		// 使用带 TTL 缓存的角色列表（批量配音时避免 N+1 查询）
		characters, err := s.listCharsByNovelCached(novelID)
		if err != nil {
			return
		}
		for _, c := range characters {
			if strings.EqualFold(c.Name, speakerName) {
				applyCharVoice(c)
				return
			}
		}
	}

	// 步骤二：仅当分镜有角色台词（Dialogue 非空）时，才使用第一个绑定角色的音色。
	// 旁白/描述文本不应被角色音色覆盖，应沿用 narrationVoice。
	if len(shot.CharacterIDs) > 0 && shot.Dialogue != "" {
		char, err := s.characterRepo.GetByID(shot.CharacterIDs[0])
		if err == nil && char != nil {
			applyCharVoice(char)
			return
		}
	}

	// 步骤三：CharacterIDs 为空时降级到 shot.Characters JSON（名称匹配）。
	// autoMatchShotCharacters 可能未命中，但 Characters JSON 由 LLM 直接写入，更可靠。
	if shot.Dialogue != "" && shot.Characters != "" {
		var shotChars []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(shot.Characters), &shotChars); err == nil && len(shotChars) > 0 {
			characters, err := s.listCharsByNovelCached(novelID)
			if err == nil {
				nameMap := make(map[string]*model.Character, len(characters))
				for _, c := range characters {
					nameMap[strings.ToLower(c.Name)] = c
				}
				for _, sc := range shotChars {
					if char, ok := nameMap[strings.ToLower(sc.Name)]; ok {
						applyCharVoice(char)
						return
					}
				}
			}
		}
	}

	// 步骤四：用分镜情绪基调覆盖角色静态风格（动态优先级更高）
	if shot.EmotionalTone != "" {
		if mapped := mapEmotionalToneToTTS(shot.EmotionalTone); mapped != "" {
			style = mapped
		}
	}

	return
}

// mapEmotionalToneToTTS 将分镜情绪基调（中文）映射为 TTS 通用情感标签。
// 返回空串表示无法映射，调用方应保持当前 style 不变。
func mapEmotionalToneToTTS(tone string) string {
	switch {
	case strings.ContainsAny(tone, "紧张") || strings.Contains(tone, "恐惧") || strings.Contains(tone, "害怕") || strings.Contains(tone, "惶恐"):
		return "fear"
	case strings.Contains(tone, "愤怒") || strings.Contains(tone, "愤") || strings.Contains(tone, "怒") || strings.Contains(tone, "激怒"):
		return "angry"
	case strings.Contains(tone, "悲伤") || strings.Contains(tone, "悲") || strings.Contains(tone, "哀") || strings.Contains(tone, "哭") || strings.Contains(tone, "伤心"):
		return "sad"
	case strings.Contains(tone, "快乐") || strings.Contains(tone, "开心") || strings.Contains(tone, "喜悦") || strings.Contains(tone, "兴奋") || strings.Contains(tone, "欢"):
		return "happy"
	case strings.Contains(tone, "平静") || strings.Contains(tone, "宁静") || strings.Contains(tone, "淡然") || strings.Contains(tone, "释怀"):
		return "calm"
	case strings.Contains(tone, "浪漫") || strings.Contains(tone, "温柔") || strings.Contains(tone, "温情"):
		return "happy"
	case strings.Contains(tone, "惊讶") || strings.Contains(tone, "惊") || strings.Contains(tone, "讶"):
		return "surprised"
	case strings.Contains(tone, "压抑") || strings.Contains(tone, "沉重") || strings.Contains(tone, "绝望"):
		return "sad"
	default:
		return ""
	}
}
