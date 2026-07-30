package service

// video_asset_service.go
//
// Voice-segment CRUD and per-shot audio generation methods
// extracted from video_service.go. All methods remain on *VideoService.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ─── Voice Segment Types ──────────────────────────────────────────────────────

// VoiceSegmentInput 创建/更新语音段落时的输入
type VoiceSegmentInput struct {
	Text     string `json:"text"`
	Speaker  string `json:"speaker"`  // 空串=旁白，非空=角色名（对白）
	Emotion  string `json:"emotion"`  // 情绪标签（平静/温馨/激动等）
	Language string `json:"language"` // 方言/语言（空串=普通话；zh-yue=粤语；zh-scu=四川话；en=英语等）
	VoiceID  string `json:"voice_id"` // TTS 声音 ID，空串=自动
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

// AppendVoiceSegment 追加段落到分镜末尾（seq_no 由事务内 FOR UPDATE 原子分配，防止并发重复）
func (s *VideoService) AppendVoiceSegment(shotID uint, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	seg := &model.ShotVoiceSegment{
		ShotID:   shotID,
		Text:     input.Text,
		Speaker:  input.Speaker,
		Emotion:  input.Emotion,
		Language: input.Language,
		VoiceID:  input.VoiceID,
	}
	return seg, s.segmentRepo.AppendAtomic(seg)
}

// InsertVoiceSegment 在 afterSeqNo 之后插入新段落（afterSeqNo=0 表示插入到最前）
func (s *VideoService) InsertVoiceSegment(shotID uint, afterSeqNo int, input VoiceSegmentInput) (*model.ShotVoiceSegment, error) {
	if s.segmentRepo == nil {
		return nil, fmt.Errorf("segment repository not initialized")
	}
	newSeqNo := afterSeqNo + 1
	seg := &model.ShotVoiceSegment{
		ShotID:   shotID,
		SeqNo:    newSeqNo,
		Text:     input.Text,
		Speaker:  input.Speaker,
		Emotion:  input.Emotion,
		Language: input.Language,
		VoiceID:  input.VoiceID,
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
	seg.Emotion = input.Emotion
	seg.Language = input.Language
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
// 返回调整后的时长（秒）；无法读取音频时返回 currentDuration。
// 注意：此函数仅用于当次生成，不持久化回数据库。
func alignShotDurationToTTS(currentDuration float64, audioURL string) float64 {
	if audioURL == "" {
		return currentDuration
	}
	data, err := readLocalOrRemoteFile(audioURL)
	if err != nil || len(data) == 0 {
		return currentDuration
	}
	ext := audioExtension(audioURL)
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
		return currentDuration
	}
	const buffer = 0.3
	needed := audioDur + buffer
	if needed > currentDuration {
		return needed
	}
	return currentDuration
}

// GenerateSegmentAudio 为单条语音段落生成 TTS 音频
func (s *VideoService) GenerateSegmentAudio(ctx context.Context, segID uint, tenantID uint, defaultVoice string) error {
	logger.Printf("[TTS] GenerateSegmentAudio: start segID=%d tenantID=%d defaultVoice=%q", segID, tenantID, defaultVoice)
	if s.segmentRepo == nil {
		logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d ERROR segment repository not initialized", segID)
		return fmt.Errorf("segment repository not initialized")
	}
	seg, err := s.segmentRepo.GetByID(segID)
	if err != nil {
		logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d ERROR get segment failed: %v", segID, err)
		return fmt.Errorf("segment %d not found: %w", segID, err)
	}
	text := stripDialogueSpeakerPrefix(seg.Text)
	if text == "" {
		logger.Printf("[TTS] GenerateSegmentAudio: segID=%d text is empty after stripping speaker prefix, skipping", segID)
		metrics.TTSGenerationTotal.WithLabelValues("skipped").Inc()
		return nil
	}
	logger.Printf("[TTS] GenerateSegmentAudio: segID=%d shotID=%d speaker=%q emotion=%q language=%q existingAudio=%q textLen=%d text=%q",
		segID, seg.ShotID, seg.Speaker, seg.Emotion, seg.Language, seg.AudioPath, len([]rune(text)), truncate(text, 60))

	ttsStart := time.Now()

	// 预加载 shot + video 一次，用于角色声音查找
	var novelID uint
	if s.storyboardRepo != nil && s.videoRepo != nil {
		if shot, e := s.storyboardRepo.GetByID(seg.ShotID); e == nil {
			if video, e := s.videoRepo.GetByID(shot.VideoID); e == nil {
				novelID = video.NovelID
			}
		}
	}

	// 确定 TTS 声音与基准风格：段落级 voice > 角色配置 > 默认
	// style 查找与 voice 查找解耦：即使段落已有 VoiceID，也要读角色风格作为基准情感
	voice := seg.VoiceID
	voiceModel := ""
	speed := 1.0
	style := ""
	if seg.Speaker != "" && s.characterRepo != nil && novelID > 0 {
		if chars, e := s.listCharsByNovelCached(novelID); e == nil {
			for _, c := range chars {
				if strings.EqualFold(c.Name, seg.Speaker) {
					if voice == "" { // 只在段落未指定 VoiceID 时才从角色取音色
						if c.VoiceConfig.VoiceID != "" {
							voice = c.VoiceConfig.VoiceID
						}
						// 角色无 VoiceID 时不分配 OpenAI 专属音色名（alloy/echo 等），
						// 直接 fallthrough 到 defaultVoice，确保所有分镜走同一 TTS provider。
						if c.VoiceConfig.VoiceSpeed > 0 {
							speed = c.VoiceConfig.VoiceSpeed
						}
					}
					if voiceModel == "" && c.VoiceConfig.VoiceModel != "" {
						voiceModel = c.VoiceConfig.VoiceModel
					}
					style = c.VoiceConfig.VoiceStyle // 角色静态风格始终作为基准情感
					logger.Printf("[TTS] GenerateSegmentAudio: segID=%d matched character %q voiceID=%q voiceModel=%q speed=%.2f style=%q",
						segID, c.Name, voice, voiceModel, speed, style)
					break
				}
			}
		} else {
			logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d ERROR list characters for novelID=%d: %v", segID, novelID, e)
		}
	}
	// 情绪优先级：段落显式情绪（最高）> 角色静态风格
	if seg.Emotion != "" {
		style = seg.Emotion
	}
	if voice == "" {
		voice = defaultVoice
		logger.Printf("[TTS] GenerateSegmentAudio: segID=%d no character/segment voice, falling back to defaultVoice=%q", segID, defaultVoice)
	}
	logger.Printf("[TTS] GenerateSegmentAudio: segID=%d calling TTS voice=%q speed=%.2f style=%q language=%q", segID, voice, speed, style, seg.Language)

	audioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, GenerateAudioOptions{
		Text:       text,
		Voice:      voice,
		VoiceModel: voiceModel,
		Speed:      speed,
		Emotion:    style,
		Language:   seg.Language,
	})
	if err != nil {
		metrics.TTSGenerationTotal.WithLabelValues("error").Inc()
		metrics.TTSGenerationDuration.Observe(time.Since(ttsStart).Seconds())
		logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d ERROR TTS FAILED voice=%q textLen=%d elapsed=%s error: %v",
			segID, voice, len([]rune(text)), time.Since(ttsStart).Round(time.Millisecond), err)
		// Clear stale audio_path so the UI shows generation failed rather than showing an old path.
		if seg.AudioPath != "" {
			if clearErr := s.segmentRepo.UpdateFields(segID, map[string]interface{}{"audio_path": ""}); clearErr != nil {
				logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d clear audio_path failed: %v", segID, clearErr)
			}
		}
		return fmt.Errorf("TTS failed for segment %d: %w", segID, err)
	}
	if audioURL == "" {
		metrics.TTSGenerationTotal.WithLabelValues("error").Inc()
		metrics.TTSGenerationDuration.Observe(time.Since(ttsStart).Seconds())
		logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d ERROR TTS returned EMPTY URL voice=%q elapsed=%s",
			segID, voice, time.Since(ttsStart).Round(time.Millisecond))
		if seg.AudioPath != "" {
			if clearErr := s.segmentRepo.UpdateFields(segID, map[string]interface{}{"audio_path": ""}); clearErr != nil {
				logger.Errorf("[TTS] GenerateSegmentAudio: segID=%d clear audio_path failed: %v", segID, clearErr)
			}
		}
		return fmt.Errorf("TTS returned empty URL for segment %d", segID)
	}
	logger.Printf("[TTS] GenerateSegmentAudio: segID=%d TTS success elapsed=%s audioURL=%q",
		segID, time.Since(ttsStart).Round(time.Millisecond), audioURL)
	metrics.TTSGenerationTotal.WithLabelValues("success").Inc()
	metrics.TTSGenerationDuration.Observe(time.Since(ttsStart).Seconds())

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
		logger.Errorf("warn: could not read audio for segment %d: %v", segID, err)
	}

	// 检测音频格式（通过 magic bytes），用于时长解析和 OSS 上传
	audioFmt, audioContentType, audioExt := detectAudioFormat(audioData)

	// 上传到持久存储（如果配置了 storageSvc）
	if s.storageSvc != nil && len(audioData) > 0 {
		key := fmt.Sprintf("audio/%s%s", uuid.New().String(), audioExt)
		ossURL, e := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(audioData), int64(len(audioData)), audioContentType)
		if e != nil {
			logger.Errorf("GenerateSegmentAudio: OSS upload failed for segment %d: %v", segID, e)
			return e
		}
		if strings.HasPrefix(audioURL, "file://") {
			os.Remove(strings.TrimPrefix(audioURL, "file://")) //nolint:errcheck
		}
		audioURL = ossURL
	}

	// Persist audio path + measured duration
	fields := map[string]interface{}{"audio_path": audioURL}
	if d := calcAudioDuration(audioData, audioFmt); d > 0 {
		fields["duration_secs"] = d
	}
	if err := s.segmentRepo.UpdateFields(segID, fields); err != nil {
		logger.Errorf("[VideoService] GenerateSegmentAudio: failed to update segment %d fields: %v", segID, err)
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
				logger.Errorf("[VideoService] syncShotDurationAfterVoice: failed to set default duration for shot %d: %v", shotID, err)
			}
		}
		return
	}
	if totalVoice <= shot.Duration {
		return // 配音比当前时长短，不需要更新
	}
	if err := s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{"duration": totalVoice}); err != nil {
		logger.Errorf("[VideoService] syncShotDurationAfterVoice: failed to update shot %d duration: %v", shotID, err)
	}
}

// GenerateShotAudio 为单个分镜生成 TTS 音频（同步），生成后写入 ShotVoiceSegment 并更新 shot.Duration
func (s *VideoService) GenerateShotAudio(ctx context.Context, shot *model.StoryboardShot, tenantID uint, narrationVoice string) error {
	logger.Printf("[TTS] GenerateShotAudio: start shotID=%d shotNo=%d videoID=%d tenantID=%d narrationVoice=%q",
		shot.ID, shot.ShotNo, shot.VideoID, tenantID, narrationVoice)

	// Check idempotency + delegate to segment-aware stitching if segments exist.
	if s.segmentRepo != nil {
		segs, err := s.segmentRepo.ListByShotID(shot.ID)
		if err != nil {
			logger.Errorf("[TTS] GenerateShotAudio: shotID=%d list segments failed: %v", shot.ID, err)
		} else if len(segs) > 0 {
			for _, seg := range segs {
				if seg.AudioPath != "" {
					logger.Printf("[TTS] GenerateShotAudio: shotID=%d already has audio (segID=%d), skipping", shot.ID, seg.ID)
					return nil
				}
			}
			logger.Printf("[TTS] GenerateShotAudio: shotID=%d has %d segments without audio, delegating to generateShotAudioFromSegments", shot.ID, len(segs))
			return s.generateShotAudioFromSegments(ctx, shot, segs, tenantID, narrationVoice)
		}
	}

	// Determine the text to synthesize: narration > dialogue.
	// description is for image/video generation only — never read it aloud.
	text := shot.Narration()
	textSource := "narration"
	if text == "" {
		text = stripDialogueSpeakerPrefix(shot.Dialogue())
		textSource = "dialogue"
	}
	if text == "" {
		logger.Printf("[TTS] GenerateShotAudio: shotID=%d has no narration or dialogue text, shot is silent", shot.ID)
		return nil
	}
	logger.Printf("[TTS] GenerateShotAudio: shotID=%d textSource=%s textLen=%d text=%q",
		shot.ID, textSource, len([]rune(text)), truncate(text, 60))

	// 需要 novelID 以便角色声音查询
	var novelID uint
	if s.videoRepo != nil {
		if video, err := s.videoRepo.GetByID(shot.VideoID); err == nil {
			novelID = video.NovelID
		} else {
			logger.Errorf("[TTS] GenerateShotAudio: shotID=%d get video failed: %v", shot.ID, err)
		}
	}

	voice, voiceModel, speed, style := s.resolveVoiceForShot(shot, narrationVoice, novelID)
	logger.Printf("[TTS] GenerateShotAudio: shotID=%d resolved voice=%q voiceModel=%q speed=%.2f style=%q", shot.ID, voice, voiceModel, speed, style)

	localAudioURL, err := s.aiService.AudioGenerateWithOptions(ctx, tenantID, GenerateAudioOptions{
		Text:       text,
		Voice:      voice,
		VoiceModel: voiceModel,
		Speed:      speed,
		Emotion:    style,
		Language:   "",
	})
	if err != nil {
		logger.Errorf("[TTS] GenerateShotAudio: shotID=%d TTS FAILED voice=%q textLen=%d error: %v",
			shot.ID, voice, len([]rune(text)), err)
		return err
	}
	if localAudioURL == "" {
		logger.Errorf("[TTS] GenerateShotAudio: shotID=%d TTS returned EMPTY URL voice=%q", shot.ID, voice)
		return fmt.Errorf("TTS returned empty audio for shot %d", shot.ShotNo)
	}
	logger.Printf("[TTS] GenerateShotAudio: shotID=%d TTS success url=%q", shot.ID, localAudioURL)

	audioURL := localAudioURL

	shot.Duration = alignShotDurationToTTS(shot.Duration, localAudioURL)

	// 上传到持久存储（持久化音频避免本地 /tmp 文件重启后消失）
	if s.storageSvc != nil {
		persistURL, uploadErr := s.uploadAudioToStorage(ctx, shot, audioURL)
		if uploadErr != nil {
			logger.Errorf("GenerateShotAudio: storage upload failed (falling back to local): %v", uploadErr)
		} else {
			audioURL = persistURL
			logger.Printf("GenerateShotAudio: shot %d audio stored at %s", shot.ShotNo, audioURL)
			if strings.HasPrefix(localAudioURL, "file://") {
				os.Remove(strings.TrimPrefix(localAudioURL, "file://")) //nolint:errcheck
			}
		}
	}

	// Persist audio as a voice segment (replaces the removed shot.TaskMeta.AudioPath field).
	if s.segmentRepo != nil {
		seg := &model.ShotVoiceSegment{
			ShotID:    shot.ID,
			SeqNo:     1,
			Text:      text,
			VoiceID:   voice, // 存储已解析的音色ID，确保重新生成时走相同的 TTS provider
			AudioPath: audioURL,
		}
		if err := s.segmentRepo.Create(seg); err != nil {
			logger.Errorf("[VideoService] GenerateShotAudio: failed to create voice segment for shot %d: %v", shot.ShotNo, err)
		}
	}
	if err := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": shot.Duration}); err != nil {
		logger.Errorf("[VideoService] GenerateShotAudio: failed to update shot %d duration: %v", shot.ShotNo, err)
	}
	return nil
}

// uploadAudioToStorage 读取 TTS 输出（file:// 路径或 HTTP URL），上传并返回持久 URL。
// novelID/chapterID 由调用方提供，避免重复查询 video 记录。
func (s *VideoService) uploadAudioToStorage(ctx context.Context, shot *model.StoryboardShot, audioURL string) (string, error) {
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

	key := fmt.Sprintf("audio/%s.mp3", uuid.New().String())
	return s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "audio/mpeg")
}

// GenerateShotSRT 根据分镜的台词/旁白和时长生成单条 SRT 字幕内容。
// 时间码从 00:00:00,000 开始，结束时间 = shot.Duration。
// 文本优先级：Dialogue > Narration > Description（兜底兼容旧数据）。
func GenerateShotSRT(shot *model.StoryboardShot) string {
	var text string
	if dial := shot.Dialogue(); dial != "" {
		// 去除"角色名："前缀，字幕只显示台词内容
		text = stripDialogueSpeakerPrefix(dial)
	} else if narr := shot.Narration(); narr != "" {
		text = narr
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
// uploads the result to storage, and updates shot.Duration.
func (s *VideoService) generateShotAudioFromSegments(ctx context.Context, shot *model.StoryboardShot, segs []*model.ShotVoiceSegment, tenantID uint, defaultVoice string) error {
	logger.Printf("[TTS] generateShotAudioFromSegments: start shotID=%d shotNo=%d segCount=%d tenantID=%d defaultVoice=%q",
		shot.ID, shot.ShotNo, len(segs), tenantID, defaultVoice)

	// 1. For each segment without audio, call GenerateSegmentAudio
	pending, skipped := 0, 0
	for _, seg := range segs {
		if seg.AudioPath == "" && seg.Text != "" {
			pending++
			logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d generating segID=%d seqNo=%d speaker=%q textLen=%d",
				shot.ID, seg.ID, seg.SeqNo, seg.Speaker, len([]rune(seg.Text)))
			if err := s.GenerateSegmentAudio(ctx, seg.ID, tenantID, defaultVoice); err != nil {
				logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d ERROR segID=%d TTS failed: %v", shot.ID, seg.ID, err)
			}
		} else {
			skipped++
			logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d skip segID=%d seqNo=%d (audioPath=%q textEmpty=%v)",
				shot.ID, seg.ID, seg.SeqNo, seg.AudioPath, seg.Text == "")
		}
	}
	logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d TTS phase done pending=%d skipped=%d", shot.ID, pending, skipped)

	// 2. Reload segments to get updated AudioPath values
	freshSegs, err := s.segmentRepo.ListByShotID(shot.ID)
	if err != nil || len(freshSegs) == 0 {
		logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d ERROR reload segments failed (err=%v freshCount=%d)", shot.ID, err, len(freshSegs))
		return nil
	}
	withAudio, withoutAudio := 0, 0
	for _, seg := range freshSegs {
		if seg.AudioPath != "" {
			withAudio++
		} else {
			withoutAudio++
			logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d segID=%d seqNo=%d still has NO audio after generation (speaker=%q text=%q)",
				shot.ID, seg.ID, seg.SeqNo, seg.Speaker, truncate(seg.Text, 40))
		}
	}
	logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d reload done withAudio=%d withoutAudio=%d", shot.ID, withAudio, withoutAudio)

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
			logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d ERROR fetch segID=%d audio: %v", shot.ID, seg.ID, err)
			continue
		}
		localPaths = append(localPaths, localPath)
	}
	if len(localPaths) == 0 {
		logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d ERROR no local audio paths available after fetch, aborting stitch", shot.ID)
		return nil
	}
	if len(localPaths) == 1 {
		logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d single segment, no stitch needed", shot.ID)
		shot.Duration = alignShotDurationToTTS(shot.Duration, freshSegs[0].AudioPath)
		return s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": shot.Duration})
	}

	// 4. Stitch with ffmpeg concat
	logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d stitching %d segments with ffmpeg", shot.ID, len(localPaths))
	listFile := filepath.Join(tmpDir, "concat.txt")
	var lines []string
	for _, p := range localPaths {
		lines = append(lines, fmt.Sprintf("file '%s'", p))
	}
	if err := os.WriteFile(listFile, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return fmt.Errorf("generateShotAudioFromSegments: write list: %w", err)
	}
	stitchedPath := filepath.Join(tmpDir, fmt.Sprintf("shot_%d_stitched.mp3", shot.ID))
	out, ffmpegErr := runFFmpegCtx(ctx,
		"-y", "-f", "concat", "-safe", "0", "-i", listFile,
		"-c", "copy", stitchedPath,
	)
	if ffmpegErr != nil {
		logger.Errorf("[TTS] generateShotAudioFromSegments: shotID=%d ERROR ffmpeg stitch failed: %v\n%s", shot.ID, ffmpegErr, string(out))
		shot.Duration = alignShotDurationToTTS(shot.Duration, freshSegs[0].AudioPath)
		return s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": shot.Duration})
	}
	logger.Printf("[TTS] generateShotAudioFromSegments: shotID=%d ffmpeg stitch success", shot.ID)

	stitchedData, err := os.ReadFile(stitchedPath)
	if err != nil {
		return fmt.Errorf("generateShotAudioFromSegments: read stitched: %w", err)
	}

	// 5. Upload stitched audio to persistent storage
	audioURL := "file://" + stitchedPath
	if s.storageSvc != nil && len(stitchedData) > 0 {
		key := fmt.Sprintf("audio/%s.mp3", uuid.New().String())
		if ossURL, e := s.storageSvc.Upload(ctx, key, bytes.NewReader(stitchedData), int64(len(stitchedData)), "audio/mpeg"); e == nil {
			audioURL = ossURL
		} else {
			logger.Errorf("generateShotAudioFromSegments: OSS upload failed for shot %d: %v", shot.ID, e)
		}
	}

	shot.Duration = alignShotDurationToTTS(shot.Duration, audioURL)
	if err := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": shot.Duration}); err != nil {
		logger.Errorf("generateShotAudioFromSegments: update shot %d duration: %v", shot.ID, err)
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
	if _, err := s.storyboardRepo.GetByID(shotID); err != nil {
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
		return ready[0].AudioPath, nil
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
			logger.Errorf("MergeVoiceSegments: fetch segment %d audio: %v", seg.ID, err)
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
		key := fmt.Sprintf("audio/%s.mp3", uuid.New().String())
		if ossURL, e := s.storageSvc.Upload(ctx, key, bytes.NewReader(merged), int64(len(merged)), "audio/mpeg"); e == nil {
			audioURL = ossURL
		} else {
			logger.Errorf("MergeVoiceSegments: OSS upload failed: %v", e)
			return "", fmt.Errorf("MergeVoiceSegments: upload failed: %w", e)
		}
	} else {
		audioURL = "file://" + outPath
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

// resolveVoiceForShot 解析分镜对应角色的配音设置（voice, voiceModel, speed, style）。
// 优先级：① 对话文本「角色名：」前缀精确匹配 → ② narrationVoice（全局旁白音色或空串）。
// 不按 CharacterIDs 兜底：画面在场角色 ≠ 说话角色，兜底会导致音色混乱。
// novelID 由调用方提供（避免此函数重复查询 video 记录）。
func (s *VideoService) resolveVoiceForShot(shot *model.StoryboardShot, narrationVoice string, novelID uint) (voice string, voiceModel string, speed float64, style string) {
	voice = narrationVoice // 空串 = 由 TTS Provider 自选默认音色
	speed = 1.0

	if novelID != 0 && s.characterRepo != nil && shot.Narration() == "" {
		// 对白镜头：尝试按发言角色查找专属音色和静态风格。
		// 旁白镜头（Narration 非空）直接使用 narrationVoice，不做角色音色覆盖。
		// 注意：autoMatchShotCharacters 会把旁白中出现的角色名写入 CharacterIDs，
		// 这些 CharacterIDs 仅用于图像生成，不应影响配音音色。

		applyCharVoice := func(c *model.Character) {
			if c.VoiceConfig.VoiceID != "" {
				voice = c.VoiceConfig.VoiceID
			}
			if c.VoiceConfig.VoiceModel != "" {
				voiceModel = c.VoiceConfig.VoiceModel
			}
			// 角色无显式 voice_id 时保持 narrationVoice（全局旁白音色），
			// 不使用 OpenAI 专用内置音色名（alloy/echo 等），避免 qianwen/doubao 等 provider 返回 InvalidVoice 错误。
			if c.VoiceConfig.VoiceSpeed > 0 {
				speed = c.VoiceConfig.VoiceSpeed
			}
			style = c.VoiceConfig.VoiceStyle // 角色静态风格作为基准，后续被情感覆盖
		}

		// 步骤一：台词行的 character 字段即精确发言角色名，无需再从文本中解析。
		speakerName := ""
		for _, l := range shot.VoiceLines() {
			if l.Character != "" {
				speakerName = l.Character
				break
			}
		}

		if speakerName != "" {
			characters, err := s.listCharsByNovelCached(novelID)
			if err == nil {
				// 精确名称匹配
				for _, c := range characters {
					if strings.EqualFold(c.Name, speakerName) {
						applyCharVoice(c)
						return
					}
				}
				// 发言角色名无法匹配：保持 narrationVoice，不按 CharacterIDs 兜底。
				// CharacterIDs[0] 是画面在场角色，不一定是说话角色，用其音色会导致音色错乱。
			}
		}
	}

	return
}

// detectAudioFormat 通过 magic bytes 检测音频格式，返回 (format, contentType, fileExt)。
// 支持 WAV（RIFF 头）和 MP3（默认）。
func detectAudioFormat(data []byte) (format, contentType, ext string) {
	if len(data) >= 4 && data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' {
		return "wav", "audio/wav", ".wav"
	}
	return "mp3", "audio/mpeg", ".mp3"
}

// calcAudioDuration 根据格式从原始字节计算音频时长（秒）。
func calcAudioDuration(data []byte, format string) float64 {
	if format == "wav" {
		if micros := wavDurationMicros(data); micros > 0 {
			return float64(micros) / 1_000_000.0
		}
		return 0
	}
	return mp3Duration(data)
}

// shotTotalAudioDuration 返回分镜的配音总时长（秒）。
// 优先累加 ShotVoiceSegment.DurationSecs；若无 segments，退化为 shot.Duration（已由
// syncShotDurationAfterVoice 对齐到配音时长）。返回 0 表示尚未生成配音。
func (s *VideoService) shotTotalAudioDuration(shot *model.StoryboardShot) float64 {
	if s.segmentRepo != nil {
		if segs, err := s.segmentRepo.ListByShotID(shot.ID); err == nil && len(segs) > 0 {
			var total float64
			for _, seg := range segs {
				total += seg.DurationSecs
			}
			if total > 0 {
				return total
			}
		}
	}
	return shot.Duration
}

// stripDialogueSpeakerPrefix 去除台词字段中的"角色名："前缀，仅保留台词内容。
// 例："妈妈：你好吗？" → "你好吗？"
// Dialogue 字段保留完整格式供 TTS 音色解析，字幕显示时才调用此函数。
func stripDialogueSpeakerPrefix(text string) string {
	for _, colon := range []string{"：", ":"} {
		idx := strings.Index(text, colon)
		if idx <= 0 || idx > len(colon)*12 {
			continue
		}
		prefix := []rune(text[:idx])
		if len(prefix) < 1 || len(prefix) > 8 {
			continue
		}
		rest := strings.TrimSpace(text[idx+len(colon):])
		if rest != "" {
			return rest
		}
	}
	return text
}

// parseAudioDurationMicros 解析音频文件的实际时长（微秒）。
// 支持 WAV（精确解析 RIFF 头）和 MP3（扫描首帧 bitrate 近似估算）。
// 其他格式或解析失败时返回 0（调用方应降级为 shot.Duration）。
func parseAudioDurationMicros(data []byte, ext string) int64 {
	switch strings.ToLower(ext) {
	case ".wav":
		return wavDurationMicros(data)
	case ".mp3":
		return mp3DurationMicros(data)
	default:
		return 0
	}
}

// wavDurationMicros 解析 WAV/RIFF 格式的精确时长（微秒）。
func wavDurationMicros(data []byte) int64 {
	if len(data) < 44 {
		return 0
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0
	}
	readU32LE := func(b []byte, off int) uint32 {
		return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
	}
	readU16LE := func(b []byte, off int) uint16 {
		return uint16(b[off]) | uint16(b[off+1])<<8
	}

	var byteRate uint32
	i := 12
	for i+8 <= len(data) {
		chunkID := string(data[i : i+4])
		chunkSize := int(readU32LE(data, i+4))
		// P0-3: 防止损坏的 WAV chunkSize 导致下一轮越界
		if chunkSize < 0 || i+8+chunkSize > len(data) {
			break
		}
		if chunkID == "fmt " && chunkSize >= 16 {
			// byteRate = sampleRate × numChannels × bitsPerSample/8
			sampleRate := readU32LE(data, i+8+4)
			numCh := readU16LE(data, i+8+2)
			bps := readU16LE(data, i+8+14)
			byteRate = sampleRate * uint32(numCh) * uint32(bps) / 8
		}
		if chunkID == "data" && byteRate > 0 {
			durationSec := float64(chunkSize) / float64(byteRate)
			return int64(durationSec * 1_000_000)
		}
		i += 8 + chunkSize
		if chunkSize%2 != 0 {
			i++ // RIFF chunks are word-aligned
		}
	}
	return 0
}

// mp3DurationMicros 通过扫描首个有效 MPEG-1 Layer3 帧获取 bitrate，
// 再用文件大小估算 MP3 时长（对 CBR MP3 准确，VBR 有偏差）。
func mp3DurationMicros(data []byte) int64 {
	// MPEG-1 Layer3 bitrate 表（kbps），索引 0 和 15 无效
	bitrateKbps := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}

	// 跳过 ID3v2 标签（常见于 TTS 输出文件头）
	offset := 0
	if len(data) >= 10 && string(data[0:3]) == "ID3" {
		syncsafe := func(b []byte) int {
			return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
		}
		offset = 10 + syncsafe(data[6:10])
	}

	// 扫描首个有效帧头（0xFF 0xFB / 0xFF 0xFA 等 MPEG-1 Layer3 同步字）
	for i := offset; i < len(data)-3; i++ {
		if data[i] != 0xFF || (data[i+1]&0xE0 != 0xE0) {
			continue
		}
		ver := (data[i+1] >> 3) & 0x03   // 11=MPEG1
		layer := (data[i+1] >> 1) & 0x03 // 01=Layer3
		if ver != 3 || layer != 1 {
			continue
		}
		brIdx := (data[i+2] >> 4) & 0x0F
		if brIdx == 0 || brIdx == 15 {
			continue
		}
		bitsPerSec := int64(bitrateKbps[brIdx]) * 1000
		if bitsPerSec <= 0 {
			continue
		}
		// 有效数据长度（去掉 ID3 标签）
		audioBytes := int64(len(data) - offset)
		durationSec := float64(audioBytes*8) / float64(bitsPerSec)
		return int64(durationSec * 1_000_000)
	}
	return 0
}
