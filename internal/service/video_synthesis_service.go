package service

// video_synthesis_service.go
//
// Video stitching, polling, and final synthesis methods
// extracted from video_service.go. All methods remain on *VideoService.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// ─── Shot Polling ─────────────────────────────────────────────────────────────

// PollShotStatus 轮询单个分镜的视频生成状态
func (s *VideoService) PollShotStatus(shot *model.StoryboardShot) error {
	if shot.ShotTaskID == "" {
		return nil
	}
	var tenantID uint
	if v, vErr := s.videoRepo.GetByID(shot.VideoID); vErr == nil {
		tenantID = v.TenantID
	}
	provider, _, provErr := s.resolveVideoProvider(tenantID, shot.ShotProviderName)
	if provErr != nil {
		return fmt.Errorf("provider %s not found", shot.ShotProviderName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := provider.GetVideoStatus(ctx, shot.ShotTaskID)
	if err != nil {
		return fmt.Errorf("poll task %s: %w", shot.ShotTaskID, err)
	}

	switch status.Status {
	case "completed", "succeed":
		videoURL, _ := provider.GetVideoURL(ctx, shot.ShotTaskID)
		if videoURL == "" {
			shot.Status = "failed"
			shot.ErrorMessage = "task completed but no video URL returned"
			return s.storyboardRepo.Update(shot)
		}
		// 下载视频到本地临时文件（供 StitchVideo 使用）
		localPath := fmt.Sprintf("%s/inkframe-shot-%d-%d.mp4", inkframeTempDir(), shot.ID, time.Now().UnixNano())
		if err := downloadFile(videoURL, localPath); err != nil {
			// 下载失败不致命：保留远程 URL，StitchVideo 会重试
			logger.Printf("PollShotStatus: download shot %d video failed (keeping URL): %v", shot.ShotNo, err)
			shot.VideoURL = videoURL
			shot.ClipPath = ""
		} else {
			shot.ClipPath = "file://" + localPath
		}
		shot.Status = "completed"
		shot.Progress = 100
	case "failed", "error":
		shot.Status = "failed"
		shot.ErrorMessage = status.Error
		if shot.RetryCount < 3 {
			shot.RetryCount++
			shot.Status = "pending"
			shot.ShotTaskID = ""
		}
	case "processing", "running", "submitted":
		shot.Status = "processing"
		if status.Progress > 0 {
			shot.Progress = status.Progress
		}
	default:
		logger.Printf("PollShotStatus: shot %d unknown status %q", shot.ShotNo, status.Status)
		shot.Status = "processing"
	}

	return s.storyboardRepo.Update(shot)
}

// ─── Video Stitching ──────────────────────────────────────────────────────────

// StitchVideo 将所有 completed 分镜拼接为最终视频
func (s *VideoService) StitchVideo(videoID uint) (string, error) {
	return s.StitchVideoCtx(context.Background(), videoID)
}

// StitchVideoCtx 同 StitchVideo，但支持外部 context（超时 / 取消）。
func (s *VideoService) StitchVideoCtx(ctx context.Context, videoID uint) (string, error) {
	logger.Printf("[StitchVideo] videoID=%d: start", videoID)

	shots, err := s.storyboardRepo.ListByVideoAndStatus(videoID, "completed")
	if err != nil {
		return "", err
	}
	if len(shots) == 0 {
		logger.Printf("[StitchVideo] videoID=%d: no completed shots found", videoID)
		return "", fmt.Errorf("no completed shots to stitch")
	}
	logger.Printf("[StitchVideo] videoID=%d: found %d completed shots", videoID, len(shots))

	// 获取宽高比和租户 ID（Ken Burns 降级时使用）
	aspectRatio := "16:9"
	var videoTenantID uint
	if video, verr := s.videoRepo.GetByID(videoID); verr == nil && video != nil {
		if video.AspectRatio != "" {
			aspectRatio = video.AspectRatio
		}
		videoTenantID = video.TenantID
	}
	logger.Printf("[StitchVideo] videoID=%d: aspectRatio=%s", videoID, aspectRatio)

	tmpDir := fmt.Sprintf("%s/inkframe-%d", inkframeTempDir(), videoID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

// shotDownloadResult 记录一个分镜的下载结果（保序）
	type shotDownloadResult struct {
		index     int
		shot      *model.StoryboardShot
		clipFile  string // 下载后的本地路径（空 = 跳过 / 仅图片）
		imgFile   string // image-only 镜头的本地图片路径
		isLocal   bool   // clipFile 是 file:// 已存在的本地文件
		downloadErr error
	}

	results := make([]shotDownloadResult, len(shots))
	for i, shot := range shots {
		results[i] = shotDownloadResult{index: i, shot: shot}
	}

	// ── Phase 1: 并发下载远端素材（HTTP I/O，最多 4 并发） ────────────────
	const maxDownloadConc = 4
	sem := make(chan struct{}, maxDownloadConc)
	var wg sync.WaitGroup
	totalShots := len(shots)
	var doneDownloads int32

	for i, shot := range shots {
		switch {
		case shot.ClipPath == "" && shot.VideoURL == "":
			// image-only：并发下载图片
			if shot.ImageURL == "" {
				continue // 无任何素材，跳过
			}
			wg.Add(1)
			go func(idx int, sh *model.StoryboardShot) {
				defer wg.Done()
				sem <- struct{}{}; defer func() { <-sem }()
				tmp, dlErr := downloadToTemp(sh.ImageURL, fmt.Sprintf("inkframe-img-%d-", sh.ID), ".jpg")
				results[idx].imgFile = tmp
				results[idx].downloadErr = dlErr
				if totalShots > 0 {
					n := int(atomic.AddInt32(&doneDownloads, 1))
					pct := 10 + n*60/totalShots
					_ = s.videoRepo.UpdateFields(videoID, map[string]interface{}{"progress": pct})
				}
			}(i, shot)

		case strings.HasPrefix(shot.ClipPath, "file://"):
			// 已是本地文件，无需下载
			results[i].clipFile = strings.TrimPrefix(shot.ClipPath, "file://")
			results[i].isLocal = true

		default:
			// 远端视频：并发下载
			remoteURL := shot.ClipPath
			if remoteURL == "" {
				remoteURL = shot.VideoURL
			}
			clipFile := fmt.Sprintf("%s/clip_%d.mp4", tmpDir, i)
			wg.Add(1)
			go func(idx int, sh *model.StoryboardShot, url, dest string) {
				defer wg.Done()
				sem <- struct{}{}; defer func() { <-sem }()
				dlStart := time.Now()
				logger.Printf("[StitchVideo] shot %d: downloading from %s", sh.ShotNo, url)
				if err := downloadFile(url, dest); err != nil {
					// URL 可能已过期，尝试从 provider 重新获取
					if sh.ShotTaskID != "" && sh.ShotProviderName != "" {
						if p, _, rErr := s.resolveVideoProvider(videoTenantID, sh.ShotProviderName); rErr == nil {
							rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
							freshURL, fErr := p.GetVideoURL(rCtx, sh.ShotTaskID)
							rCancel()
							if fErr == nil {
								logger.Printf("[StitchVideo] shot %d: got fresh URL, retrying download", sh.ShotNo)
								results[idx].downloadErr = downloadFile(freshURL, dest)
							} else {
								results[idx].downloadErr = fmt.Errorf("download failed and refresh URL failed: %w", err)
							}
						} else {
							results[idx].downloadErr = err
						}
					} else {
						results[idx].downloadErr = err
					}
					return
				}
				if fi, statErr := os.Stat(dest); statErr == nil {
					logger.Printf("[StitchVideo] shot %d: download complete in %.1fs size=%.1fMB", sh.ShotNo, time.Since(dlStart).Seconds(), float64(fi.Size())/1e6)
				} else {
					logger.Printf("[StitchVideo] shot %d: download complete in %.1fs", sh.ShotNo, time.Since(dlStart).Seconds())
				}
				results[idx].clipFile = dest
				if totalShots > 0 {
					n := int(atomic.AddInt32(&doneDownloads, 1))
					pct := 10 + n*60/totalShots
					_ = s.videoRepo.UpdateFields(videoID, map[string]interface{}{"progress": pct})
				}
			}(i, shot, remoteURL, clipFile)
		}
	}
	wg.Wait()
	logger.Printf("[StitchVideo] videoID=%d: all downloads complete", videoID)

	// ── Phase 2: 按序处理（FFmpeg 串行，WASM 单线程限制） ────────────────
	var concatLines []string
	for i, res := range results {
		shot := res.shot
		logger.Printf("[StitchVideo] shot %d: processing (clipFile=%q imgFile=%q err=%v)",
			shot.ShotNo, res.clipFile, res.imgFile, res.downloadErr)

		if res.downloadErr != nil {
			logger.Printf("[StitchVideo] shot %d: download error — skipping: %v", shot.ShotNo, res.downloadErr)
			continue
		}

		// image-only 镜头：生成 still frame（FFmpeg 串行）
		if res.imgFile != "" {
			duration := shot.Duration
			if duration <= 0 {
				duration = defaultShotDurationSecs
			}
			// image-only 镜头：直接用 still frame（-loop 1，x264 全 P 帧，WASM 几秒完成）。
			// 注意：zoompan / JPEG序列编码在 WASM 单线程下耗时数分钟且 context 无法取消，禁止在合成路径使用。
			clipPath, clipErr := s.generateStillFrameClip(res.imgFile, duration, aspectRatio)
			os.Remove(res.imgFile)
			if clipErr != nil {
				logger.Printf("[StitchVideo] shot %d: still frame failed: %v — skipping", shot.ShotNo, clipErr)
				continue
			}
			logger.Printf("[StitchVideo] shot %d: still frame ready: %s", shot.ShotNo, clipPath)
			concatLines = append(concatLines, fmt.Sprintf("file '%s'", clipPath))
			continue
		}

		if res.clipFile == "" {
			logger.Printf("[StitchVideo] shot %d: no clip or image — skipping", shot.ShotNo)
			continue
		}

		// 本地文件：加入清理列表（file:// 本地缓存）
		if res.isLocal {
			defer os.Remove(res.clipFile) //nolint:errcheck
		}

		finalClip := res.clipFile

		// Merge audio — use real audio if present, otherwise generate silent track
		audioPath := ""
		if shot.AudioPath != "" {
			audioPath = strings.TrimPrefix(shot.AudioPath, "file://")
		} else {
			// Generate a silent audio file so every clip has an audio track.
			// This prevents FFmpeg concat from failing or producing audio dropouts
			// when clips with and without audio are mixed.
			silentPath := generateSilentAudio(tmpDir, shot.ShotNo, shot.Duration)
			if silentPath != "" {
				audioPath = silentPath
				logger.Printf("[StitchVideo] shot %d: no audio — using generated silent track", shot.ShotNo)
			}
		}
		if audioPath != "" {
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			logger.Printf("[StitchVideo] shot %d: merging audio: %s", shot.ShotNo, audioPath)
			mergeCtx, mergeCancel := context.WithTimeout(ctx, 60*time.Second)
			_, mergeErr := runFFmpegCtx(mergeCtx, "-y",
				"-i", res.clipFile,
				"-i", audioPath,
				"-c:v", "copy",
				"-c:a", "aac",
				"-shortest",
				mergedFile,
			)
			mergeCancel()
			if mergeErr != nil {
				logger.Printf("[StitchVideo] shot %d: audio merge failed: %v — using clip without audio", shot.ShotNo, mergeErr)
			} else {
				logger.Printf("[StitchVideo] shot %d: audio merged OK", shot.ShotNo)
				finalClip = mergedFile
			}
		}

		concatLines = append(concatLines, fmt.Sprintf("file '%s'", finalClip))
	}

	if len(concatLines) == 0 {
		logger.Printf("[StitchVideo] videoID=%d: no clips collected after processing all shots", videoID)
		return "", fmt.Errorf("no clips available to stitch (all shots missing video)")
	}
	logger.Printf("[StitchVideo] videoID=%d: %d clips ready for concat", videoID, len(concatLines))

	listFile := fmt.Sprintf("%s/list.txt", tmpDir)
	if err := os.WriteFile(listFile, []byte(strings.Join(concatLines, "\n")), 0644); err != nil {
		return "", err
	}

	stitchedPath := fmt.Sprintf("%s/inkframe-%d-stitched.mp4", inkframeTempDir(), videoID)
	logger.Printf("[StitchVideo] videoID=%d: running ffmpeg concat → %s", videoID, stitchedPath)
	// concat 使用 goroutine 超时（wazero 在 WASM 内无法通过 ctx 中断）
	// -c copy 只是复制流，通常很快；给每个 clip 留 30s 余量，最少 3 分钟
	concatTimeout := time.Duration(len(concatLines))*30*time.Second + 3*time.Minute
	if concatTimeout > 30*time.Minute {
		concatTimeout = 30 * time.Minute
	}
	if concatOut, concatErr := runFFmpegWithGoroutineTimeout(concatTimeout, "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		stitchedPath,
	); concatErr != nil {
		logger.Printf("[StitchVideo] videoID=%d: ffmpeg concat failed: %v\noutput: %s", videoID, concatErr, string(concatOut))
		if vid, ferr := s.videoRepo.GetByID(videoID); ferr == nil && vid != nil {
			vid.Status = "failed"
			vid.ErrorMessage = fmt.Sprintf("ffmpeg stitch failed: %v", concatErr)
			if updErr := s.videoRepo.Update(vid); updErr != nil {
				logger.Printf("[StitchVideo] videoID=%d: failed to update status to failed: %v", videoID, updErr)
			}
		}
		return "", fmt.Errorf("ffmpeg stitch failed: %w", concatErr)
	}
	logger.Printf("[StitchVideo] videoID=%d: ffmpeg concat done", videoID)

	// BGM 混音（非致命：失败时使用无BGM版本）
	outputPath := fmt.Sprintf("%s/inkframe-%d-output.mp4", inkframeTempDir(), videoID)
	if s.bgmService != nil {
		bgmURL := s.bgmService.SelectBGM("")
		if bgmURL != "" {
			logger.Printf("[StitchVideo] videoID=%d: mixing BGM: %s", videoID, bgmURL)
			if mixErr := s.bgmService.MixBGM(stitchedPath, bgmURL, outputPath); mixErr != nil {
				logger.Printf("[StitchVideo] videoID=%d: BGM mix failed: %v — using stitched without BGM", videoID, mixErr)
				outputPath = stitchedPath
			} else {
				logger.Printf("[StitchVideo] videoID=%d: BGM mix done", videoID)
			}
		} else {
			logger.Printf("[StitchVideo] videoID=%d: no BGM selected", videoID)
			outputPath = stitchedPath
		}
	} else {
		outputPath = stitchedPath
	}

	// Update video record — must succeed for status to be reflected in DB
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Printf("[StitchVideo] videoID=%d: video record not found after stitch, status NOT updated: %v", videoID, err)
		return outputPath, fmt.Errorf("stitch succeeded but video record not found: %w", err)
	}
	video.VideoPath = outputPath
	video.Status = "completed"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Printf("[StitchVideo] videoID=%d: failed to update status to completed: %v", videoID, err)
	}

	logger.Printf("[StitchVideo] videoID=%d: done → %s", videoID, outputPath)
	return outputPath, nil
}

// ─── Poll & Stitch Pipeline ───────────────────────────────────────────────────

// PollAndStitchVideo 后台轮询所有分镜状态，完成后拼接
func (s *VideoService) PollAndStitchVideo(videoID uint) {
	if _, loaded := s.activePoll.LoadOrStore(videoID, struct{}{}); loaded {
		logger.Printf("PollAndStitchVideo: videoID %d already polling, skip", videoID)
		return
	}
	defer s.activePoll.Delete(videoID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	noProgressCount := 0

	for {
		select {
		case <-ctx.Done():
			logger.Printf("PollAndStitchVideo: videoID %d timed out after 2h", videoID)
			video, _ := s.videoRepo.GetByID(videoID)
			if video != nil && video.Status != "completed" {
				video.Status = "failed"
				video.ErrorMessage = "stitch pipeline timed out (>2h)"
				if err := s.videoRepo.Update(video); err != nil {
					logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (timeout): %v", videoID, err)
				}
			}
			return
		case <-s.stopCh:
			logger.Printf("PollAndStitchVideo: videoID %d shutdown", videoID)
			return
		case <-ticker.C:
		}

		// Retry pending shots (from consistency/failed retry)
		pending, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		for _, shot := range pending {
			if shot.ShotTaskID == "" {
				video, _ := s.videoRepo.GetByID(videoID)
				aspectRatio := "16:9"
				if video != nil {
					aspectRatio = video.AspectRatio
				}
				if err := s.GenerateShotVideo(shot, aspectRatio); err != nil {
					logger.Printf("[PollAndStitch] GenerateShotVideo shot %d failed: %v", shot.ID, err)
					_ = s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
						"status":        "failed",
						"error_message": fmt.Sprintf("generation failed: %v", err),
					})
				}
			}
		}

		// Poll processing shots
		processing, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		for _, shot := range processing {
			if err := s.PollShotStatus(shot); err != nil {
				logger.Printf("[PollAndStitch] PollShotStatus shot %d failed: %v", shot.ID, err)
			}
		}

		// Check if all completed
		allShots, _ := s.storyboardRepo.ListByVideo(videoID)
		if len(allShots) == 0 {
			continue
		}
		completedCount := 0
		failedCount := 0
		for _, shot := range allShots {
			switch shot.Status {
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			}
		}

		if completedCount+failedCount == len(allShots) {
			if completedCount > 0 {
				if _, err := s.StitchVideoCtx(ctx, videoID); err != nil {
					logger.Printf("PollAndStitchVideo: stitch failed: %v", err)
					video, _ := s.videoRepo.GetByID(videoID)
					if video != nil {
						video.Status = "failed"
						video.ErrorMessage = err.Error()
						if updErr := s.videoRepo.Update(video); updErr != nil {
							logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (stitch error): %v", videoID, updErr)
						}
					}
				}
			} else {
				video, _ := s.videoRepo.GetByID(videoID)
				if video != nil {
					video.Status = "failed"
					video.ErrorMessage = "all shots failed"
					if updErr := s.videoRepo.Update(video); updErr != nil {
						logger.Printf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (all shots failed): %v", videoID, updErr)
					}
				}
			}
			return
		}

		// Stall detection (no progress after 5 ticks): re-query to get fresh counts
		processingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		pendingNow, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		if len(processingNow) == 0 && len(pendingNow) == 0 {
			noProgressCount++
			if noProgressCount >= 5 {
				logger.Printf("PollAndStitchVideo: videoID %d stalled, stopping", videoID)
				if vid, err := s.videoRepo.GetByID(videoID); err == nil && vid.Status == "generating" {
					vid.Status = "failed"
					vid.ErrorMessage = "generation stalled (no progress)"
					_ = s.videoRepo.Update(vid)
				}
				return
			}
		} else {
			noProgressCount = 0
		}
	}
}

// ─── Synthesis Pipeline ───────────────────────────────────────────────────────

// SynthesizeVideo 完整合成流水线（拼接→字幕→封面→上传OSS），异步执行，返回 task_id。
func (s *VideoService) SynthesizeVideo(ctx context.Context, videoID uint, tenantID uint) (string, error) {
	logger.Printf("[SynthesizeVideo] videoID=%d tenantID=%d: start", videoID, tenantID)

	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return "", fmt.Errorf("video not found: %w", err)
	}

	// 租户隔离校验
	if tenantID != 0 && video.TenantID != 0 && video.TenantID != tenantID {
		return "", fmt.Errorf("access denied: video %d does not belong to tenant %d", videoID, tenantID)
	}

	// 创建异步任务
	var taskID string
	if s.taskSvc != nil {
		task, err := s.taskSvc.Create(tenantID, "video_synthesis", "视频合成", "video", videoID)
		if err != nil {
			return "", fmt.Errorf("create task: %w", err)
		}
		taskID = task.TaskID
	} else {
		taskID = fmt.Sprintf("synth-%d", videoID)
	}
	logger.Printf("[SynthesizeVideo] videoID=%d: taskID=%s", videoID, taskID)

	synthCtx, synthCancel := context.WithTimeout(context.Background(), 2*time.Hour)
	go func() {
		defer synthCancel()
		if s.taskSvc != nil {
			_ = s.taskSvc.SetRunning(taskID)
		}

		// 1. 拼接视频
		logger.Printf("[SynthesizeVideo] videoID=%d step=1/4: stitching video", videoID)
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 10)
		}
		stitchedPath, err := s.StitchVideoCtx(synthCtx, videoID)
		if err != nil {
			logger.Printf("[SynthesizeVideo] videoID=%d step=1/4: stitch failed: %v", videoID, err)
			if s.taskSvc != nil {
				_ = s.taskSvc.Fail(taskID, "stitch failed: "+err.Error())
			}
			return
		}
		logger.Printf("[SynthesizeVideo] videoID=%d step=1/4: stitch OK → %s", videoID, stitchedPath)

		finalPath := stitchedPath

		// 2. 字幕烧录（可选）
		logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: subtitle burn", videoID)
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 40)
		}
		novelCfg := s.GetNovelVideoConfig(video.NovelID)
		if novelCfg != nil && novelCfg.SubtitleStyle != "" && novelCfg.SubtitleStyle != "none" {
			shots, err := s.storyboardRepo.ListByVideo(videoID)
			if err == nil && len(shots) > 0 {
				subtitleSvc := NewSubtitleService()
				fontName := "Noto Sans CJK SC"
				if novelCfg.SubtitleFont != "" {
					fontName = novelCfg.SubtitleFont
				}
				logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: burning subtitles (style=%s font=%s shots=%d)",
					videoID, novelCfg.SubtitleStyle, fontName, len(shots))
				shotSlice := make([]model.StoryboardShot, len(shots))
				for i, sh := range shots {
					if sh != nil {
						shotSlice[i] = *sh
					}
				}
				assContent := subtitleSvc.GenerateASS(shotSlice, fontName)
				assPath := fmt.Sprintf("%s/inkframe-%d-subtitles.ass", inkframeTempDir(), videoID)
				if writeErr := os.WriteFile(assPath, []byte(assContent), 0644); writeErr == nil {
					burnedPath := fmt.Sprintf("%s/inkframe-%d-burned.mp4", inkframeTempDir(), videoID)
					if burnErr := subtitleSvc.BurnSubtitles(synthCtx, stitchedPath, assPath, burnedPath); burnErr == nil {
						logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: subtitle burn OK → %s", videoID, burnedPath)
						finalPath = burnedPath
					} else {
						logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: subtitle burn failed: %v — skipping", videoID, burnErr)
					}
					os.Remove(assPath)
				}
			}
		} else {
			logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: subtitles disabled or not configured — skipping", videoID)
		}

		// 3. 提取封面
		logger.Printf("[SynthesizeVideo] videoID=%d step=3/4: extracting cover", videoID)
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 60)
		}
		coverPath := fmt.Sprintf("%s/inkframe-%d-cover.jpg", inkframeTempDir(), videoID)
		coverURL := ""
		if _, err := runFFmpegWithGoroutineTimeout(30*time.Second, "-y", "-ss", "2", "-i", finalPath,
			"-frames:v", "1", "-vf", "scale=640:-1", coverPath); err == nil {
			logger.Printf("[SynthesizeVideo] videoID=%d step=3/4: cover extracted → %s", videoID, coverPath)
			defer os.Remove(coverPath)
		} else {
			logger.Printf("[SynthesizeVideo] videoID=%d step=3/4: cover extraction failed: %v — continuing", videoID, err)
		}

		// 4. 上传视频和封面到 OSS
		logger.Printf("[SynthesizeVideo] videoID=%d step=4/4: uploading to OSS", videoID)
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 70)
		}
		finalVideoURL := ""
		novel, _ := s.novelRepo.GetByID(video.NovelID)
		novelTitle := ""
		if novel != nil {
			novelTitle = sanitizeStorageName(novel.Title)
		}

		if s.storageSvc != nil {
			// 上传视频
			videoUUID := uuid.New().String()
			var videoKey string
			if novelTitle != "" {
				videoKey = fmt.Sprintf("novels/%s/videos/%s.mp4", novelTitle, videoUUID)
			} else {
				videoKey = fmt.Sprintf("videos/%s.mp4", videoUUID)
			}
			logger.Printf("[SynthesizeVideo] videoID=%d: uploading video to key=%s", videoID, videoKey)
			if vf, err := os.Open(finalPath); err == nil {
				defer vf.Close()
				if fi, err := vf.Stat(); err == nil {
					logger.Printf("[SynthesizeVideo] videoID=%d: video file size=%.1fMB", videoID, float64(fi.Size())/1e6)
					if ossURL, err := s.storageSvc.Upload(synthCtx, videoKey, vf, fi.Size(), "video/mp4"); err == nil {
						logger.Printf("[SynthesizeVideo] videoID=%d: video upload OK → %s", videoID, ossURL)
						finalVideoURL = ossURL
					} else {
						logger.Printf("[SynthesizeVideo] videoID=%d: video upload failed: %v", videoID, err)
					}
				}
			}

			// 上传封面
			if cf, err := os.Open(coverPath); err == nil {
				defer cf.Close()
				if fi, err := cf.Stat(); err == nil {
					coverKey := videoKey[:len(videoKey)-4] + "_cover.jpg"
					logger.Printf("[SynthesizeVideo] videoID=%d: uploading cover key=%s", videoID, coverKey)
					if ossURL, err := s.storageSvc.Upload(synthCtx, coverKey, cf, fi.Size(), "image/jpeg"); err == nil {
						logger.Printf("[SynthesizeVideo] videoID=%d: cover upload OK → %s", videoID, ossURL)
						coverURL = ossURL
					} else {
						logger.Printf("[SynthesizeVideo] videoID=%d: cover upload failed: %v — continuing", videoID, err)
					}
				}
			}
		} else {
			logger.Printf("[SynthesizeVideo] videoID=%d step=4/4: storageSvc not configured — skipping upload", videoID)
		}

		// 5. 更新数据库
		logger.Printf("[SynthesizeVideo] videoID=%d step=5/5: updating DB (finalVideoURL=%q)", videoID, finalVideoURL)
		if s.taskSvc != nil {
			_ = s.taskSvc.UpdateProgress(taskID, 90)
		}
		if finalVideoURL != "" {
			video.FinalVideoURL = finalVideoURL
		}
		if coverURL != "" {
			video.CoverURL = coverURL
		}
		// 仅当视频成功上传（有 URL）才标记 completed；否则标记 failed 以告知用户
		if finalVideoURL != "" {
			video.Status = "completed"
			logger.Printf("[SynthesizeVideo] videoID=%d: pipeline complete", videoID)
		} else {
			video.Status = "failed"
			logger.Printf("[SynthesizeVideo] videoID=%d: upload failed — marking video as failed", videoID)
		}
		if err := s.videoRepo.Update(video); err != nil {
			logger.Printf("[SynthesizeVideo] videoID=%d: DB update failed: %v", videoID, err)
		}

		if s.taskSvc != nil {
			if finalVideoURL != "" {
				result := map[string]string{"final_video_url": finalVideoURL, "cover_url": coverURL}
				_ = s.taskSvc.Complete(taskID, result)
			} else {
				_ = s.taskSvc.Fail(taskID, "video upload failed, no URL generated")
			}
		}

		// 清理临时文件
		os.Remove(finalPath)
		if finalPath != stitchedPath {
			os.Remove(stitchedPath)
		}
		logger.Printf("[SynthesizeVideo] videoID=%d: goroutine done", videoID)
	}()

	return taskID, nil
}

// GetStatus 获取视频生成状态概览
func (s *VideoService) GetStatus(id uint) (interface{}, error) {
	video, err := s.videoRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	shots, err := s.storyboardRepo.ListByVideo(id)
	if err != nil {
		return nil, err
	}

	type ShotStatus struct {
		ShotNo   int     `json:"shot_no"`
		Status   string  `json:"status"`
		Progress int     `json:"progress"`
		ImageURL string  `json:"image_url,omitempty"`
		VideoURL string  `json:"video_url,omitempty"`
		ClipPath string  `json:"clip_path,omitempty"`
		Duration float64 `json:"duration"`
		Error    string  `json:"error,omitempty"`
	}

	shotStatuses := make([]ShotStatus, 0, len(shots))
	completedCount := 0
	for _, shot := range shots {
		ss := ShotStatus{
			ShotNo:   shot.ShotNo,
			Status:   shot.Status,
			Progress: int(shot.Progress),
			ImageURL: shot.ImageURL,
			VideoURL: shot.VideoURL,
			ClipPath: shot.ClipPath,
			Duration: shot.Duration,
			Error:    shot.ErrorMessage,
		}
		shotStatuses = append(shotStatuses, ss)
		if shot.Status == "completed" {
			completedCount++
		}
	}

	overallProgress := 0
	if len(shots) > 0 {
		overallProgress = completedCount * 100 / len(shots)
	}

	return map[string]interface{}{
		"video": map[string]interface{}{
			"id":              video.ID,
			"status":          video.Status,
			"title":           video.Title,
			"total_shots":     video.TotalShots,
			"completed_shots": completedCount,
			"progress":        overallProgress,
			"video_path":      video.VideoPath,
			"final_video_url": video.FinalVideoURL,
			"error_message":   video.ErrorMessage,
		},
		"shots": shotStatuses,
	}, nil
}

// checkTenantAccess 检查租户对小说的访问权限
func (s *VideoService) checkTenantAccess(novelID uint) error {
	if s.tenantRepo == nil {
		return nil // no tenant enforcement
	}
	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return fmt.Errorf("novel not found: %w", err)
	}
	tenant, err := s.tenantRepo.GetByID(novel.TenantID)
	if err != nil {
		return fmt.Errorf("tenant not found: %w", err)
	}
	if tenant.Status != "active" {
		return fmt.Errorf("tenant account is not active (status: %s)", tenant.Status)
	}
	return nil
}

// ─── Shared utilities (stitch / clip helpers) ─────────────────────────────────

// downloadFile 下载 HTTP URL 到本地路径
func downloadFile(url, dest string) error {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	const maxVideoBytes = 500 << 20 // 500 MB
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxVideoBytes+1))
	if err == nil && n > maxVideoBytes {
		f.Close()
		os.Remove(dest)
		return fmt.Errorf("video file too large (>500MB)")
	}
	return err
}

// downloadToTemp 将 URL 下载到临时文件，返回本地路径
func downloadToTemp(url, prefix, ext string) (string, error) {
	resp, err := downloadHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmpFile, err := os.CreateTemp(inkframeTempDir(), prefix+"*"+ext)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()
	const maxVideoBytes = 500 << 20 // 500 MB
	n, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxVideoBytes+1))
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	if n > maxVideoBytes {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("video file too large (>500MB)")
	}
	return tmpFile.Name(), nil
}

// inkframeTempDir 返回可写的临时目录（绝对路径，优先用工作目录下的 tmp/，fallback 到系统临时目录）
// 必须返回绝对路径，否则嵌入式 WASM ffmpeg 无法通过 WASI 文件系统访问。
func inkframeTempDir() string {
	dir := "tmp"
	if abs, err := os.Getwd(); err == nil {
		dir = abs + "/tmp"
	}
	if err := os.MkdirAll(dir, 0755); err == nil {
		return dir
	}
	return os.TempDir()
}

// generateSilentAudio creates a silent MP3 of the given duration and returns its local path.
// Returns "" if FFmpeg fails, so callers must handle the empty case.
func generateSilentAudio(dir string, shotNo int, durationSecs float64) string {
	if durationSecs <= 0 {
		durationSecs = 5.0
	}
	outPath := filepath.Join(dir, fmt.Sprintf("silent_%d.mp3", shotNo))
	out, err := runFFmpegCtx(context.Background(),
		"-y",
		"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=44100:cl=stereo"),
		"-t", fmt.Sprintf("%.3f", durationSecs),
		"-c:a", "libmp3lame", "-q:a", "9",
		outPath,
	)
	if err != nil {
		logger.Printf("generateSilentAudio: shot %d ffmpeg failed: %v\n%s", shotNo, err, string(out))
		return ""
	}
	return outPath
}

// uploadClipToStorage 将本地 MP4 文件上传到持久存储（OSS），返回持久 URL。
// storageSvc 为 nil 或上传失败时返回 ""（调用方保留 file:// 本地路径）。
func (s *VideoService) uploadClipToStorage(ctx context.Context, shot *model.StoryboardShot, clipPath string) string {
	if s.storageSvc == nil {
		return ""
	}
	// If the context has no deadline, add a 5-minute timeout to prevent hangs.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	f, err := os.Open(clipPath)
	if err != nil {
		logger.Printf("uploadClipToStorage: open %s: %v", clipPath, err)
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		logger.Printf("uploadClipToStorage: stat %s: %v", clipPath, err)
		return ""
	}

	filename := uuid.New().String() + ".mp4"
	key := fmt.Sprintf("videos/%s", filename) // fallback key

	if shot.ChapterID != nil {
		if ch, err := s.chapterRepo.GetByID(*shot.ChapterID); err == nil {
			if novel, err := s.novelRepo.GetByID(ch.NovelID); err == nil && novel.Title != "" {
				if sanitized := sanitizeStorageName(novel.Title); sanitized != "" {
					key = fmt.Sprintf("novels/%s/chapters/%d/videos/%s", sanitized, ch.ChapterNo, filename)
				}
			}
		}
	}

	ossURL, err := s.storageSvc.Upload(ctx, key, f, fi.Size(), "video/mp4")
	if err != nil {
		logger.Printf("uploadClipToStorage: upload failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}
	return ossURL
}
