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
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/storage"
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
	// 把 seq_no >= newSeqNo 的段落全部后移 1 位
	if err := s.segmentRepo.ShiftSeqNos(shotID, newSeqNo); err != nil {
		return nil, err
	}
	seg := &model.ShotVoiceSegment{
		ShotID:  shotID,
		SeqNo:   newSeqNo,
		Text:    input.Text,
		Speaker: input.Speaker,
		VoiceID: input.VoiceID,
	}
	return seg, s.segmentRepo.Create(seg)
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

// mp3Duration estimates the duration in seconds of MPEG1 Layer3 audio data by
// counting frames. Returns 0 if the data cannot be parsed.
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
	bitrateTable := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	sampleRates := [4]int{44100, 48000, 32000, 0}
	var frames, sampleRate int
	for i := offset; i+3 < len(data); {
		if data[i] != 0xFF || (data[i+1]&0xE0) != 0xE0 {
			i++
			continue
		}
		// MPEG1 (bits 4-3 = 11) + Layer3 (bits 2-1 = 01)
		if (data[i+1]&0x18) != 0x18 || (data[i+1]&0x06) != 0x02 {
			i++
			continue
		}
		bitrateIdx := int(data[i+2]>>4) & 0x0F
		srIdx := int(data[i+2]>>2) & 0x03
		padding := int(data[i+2]>>1) & 0x01
		bitrate := bitrateTable[bitrateIdx] * 1000
		sr := sampleRates[srIdx]
		if bitrate == 0 || sr == 0 {
			i++
			continue
		}
		frameLen := 144*bitrate/sr + padding
		if frameLen <= 4 || i+frameLen > len(data) {
			break
		}
		frames++
		if sampleRate == 0 {
			sampleRate = sr
		}
		i += frameLen
	}
	if frames == 0 || sampleRate == 0 {
		return 0
	}
	return float64(frames) * 1152.0 / float64(sampleRate)
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

	// 预加载 shot + video 一次，同时用于：① 角色声音查找 ② OSS 存储 key（避免重复查询）
	var novelID, chapterID uint
	if s.storyboardRepo != nil && s.videoRepo != nil {
		if shot, e := s.storyboardRepo.GetByID(seg.ShotID); e == nil {
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
			for _, c := range chars {
				if strings.EqualFold(c.Name, seg.Speaker) {
					voice = c.VoiceID
					break
				}
			}
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
		return fmt.Errorf("TTS failed for segment %d: %w", segID, err)
	}
	if audioURL == "" {
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
		if ossURL, e := s.storageSvc.Upload(context.Background(), key, bytes.NewReader(audioData), int64(len(audioData)), "audio/mpeg"); e == nil {
			if strings.HasPrefix(audioURL, "file://") {
				os.Remove(strings.TrimPrefix(audioURL, "file://")) //nolint:errcheck
			}
			audioURL = ossURL
		} else {
			logger.Printf("GenerateSegmentAudio: OSS upload failed for segment %d: %v", segID, e)
		}
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
	if totalVoice <= 0 {
		return
	}
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil || shot == nil {
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

	// 上传到持久存储（持久化音频避免本地 /tmp 文件重启后消失）
	if s.storageSvc != nil {
		persistURL, uploadErr := s.uploadAudioToStorage(ctx, shot, audioURL, novelID, chapterID)
		if uploadErr != nil {
			logger.Printf("GenerateShotAudio: storage upload failed (falling back to local): %v", uploadErr)
		} else {
			audioURL = persistURL
			logger.Printf("GenerateShotAudio: shot %d audio stored at %s", shot.ShotNo, audioURL)
			// 删除本次新建的 /tmp 临时文件（修复：之前错误地删除旧 shot.AudioPath）
			if strings.HasPrefix(localAudioURL, "file://") {
				os.Remove(strings.TrimPrefix(localAudioURL, "file://")) //nolint:errcheck
			}
		}
	}

	shot.AudioPath = audioURL
	// 更新分镜时长：取视频时长与配音时长的最大值（含 0.3s 缓冲），并持久化
	shot.Duration = alignShotDurationToTTS(shot)
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

	return
}
