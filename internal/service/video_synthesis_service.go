package service

// video_synthesis_service.go
//
// Video stitching, polling, and final synthesis methods
// extracted from video_service.go. All methods remain on *VideoService.

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
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
		tenantID = s.videoTenantID(v)
	}
	provider, _, provErr := s.resolveVideoProvider(tenantID, shot.ShotProviderName)
	if provErr != nil {
		logger.Errorf("PollShotStatus: shot %d 找不到提供商 provider=%s: %v", shot.ShotNo, shot.ShotProviderName, provErr)
		return fmt.Errorf("provider %s not found", shot.ShotProviderName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := provider.GetVideoStatus(ctx, shot.ShotTaskID)
	if err != nil {
		logger.Errorf("PollShotStatus: shot %d GetVideoStatus失败 taskID=%s: %v", shot.ShotNo, shot.ShotTaskID, err)
		return fmt.Errorf("poll task %s: %w", shot.ShotTaskID, err)
	}

	logger.Printf("PollShotStatus: shot %d taskID=%s provider=%s status=%s progress=%d",
		shot.ShotNo, shot.ShotTaskID, shot.ShotProviderName, status.Status, status.Progress)

	switch status.Status {
	case "completed", "succeed":
		videoURL, urlErr := provider.GetVideoURL(ctx, shot.ShotTaskID)
		if urlErr != nil {
			logger.Errorf("PollShotStatus: shot %d GetVideoURL失败 taskID=%s: %v", shot.ShotNo, shot.ShotTaskID, urlErr)
		}
		if videoURL == "" {
			logger.Errorf("PollShotStatus: shot %d 任务完成但未返回video_url taskID=%s", shot.ShotNo, shot.ShotTaskID)
			shot.Status = "failed"
			shot.ErrorMessage = "task completed but no video URL returned"
			if dbErr := s.storyboardRepo.Update(shot); dbErr != nil {
				logger.Errorf("PollShotStatus: shot %d DB更新失败(failed): %v", shot.ShotNo, dbErr)
			}
			return s.storyboardRepo.Update(shot)
		}
		logger.Printf("PollShotStatus: shot %d 视频生成完成 url=%s", shot.ShotNo, videoURL)
		// 下载视频到本地临时文件（供 StitchVideo 使用）
		localPath := fmt.Sprintf("%s/inkframe-shot-%d-%d.mp4", inkframeTempDir(), shot.ID, time.Now().UnixNano())
		dlCtx, dlCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		dlErr := downloadFileCtx(dlCtx, videoURL, localPath)
		dlCancel()
		if dlErr != nil {
			// 下载失败不致命：保留远程 URL，StitchVideo 会重试
			logger.Errorf("PollShotStatus: shot %d 下载视频失败，保留远程URL: %v", shot.ShotNo, dlErr)
			shot.VideoURL = videoURL
			shot.ClipPath = ""
		} else {
			logger.Printf("PollShotStatus: shot %d 视频下载完成 path=%s", shot.ShotNo, localPath)
			shot.ClipPath = "file://" + localPath
		}
		shot.Status = "completed"
		shot.Progress = 100
	case "failed", "error":
		logger.Errorf("PollShotStatus: shot %d 生成失败 taskID=%s error=%s", shot.ShotNo, shot.ShotTaskID, status.Error)
		shot.Status = "failed"
		shot.ErrorMessage = status.Error
		if shot.RetryCount < 3 {
			shot.RetryCount++
			shot.Status = "pending"
			shot.ShotTaskID = ""
			logger.Printf("PollShotStatus: shot %d 进入重试队列 retry=%d", shot.ShotNo, shot.RetryCount)
		} else {
			logger.Errorf("PollShotStatus: shot %d 超过最大重试次数(%d)，永久失败", shot.ShotNo, shot.RetryCount)
		}
	case "processing", "running", "submitted":
		shot.Status = "processing"
		if status.Progress > 0 {
			shot.Progress = status.Progress
		}
	default:
		logger.Printf("PollShotStatus: shot %d 未知状态 %q，继续等待", shot.ShotNo, status.Status)
		shot.Status = "processing"
	}

	if dbErr := s.storyboardRepo.Update(shot); dbErr != nil {
		logger.Errorf("PollShotStatus: shot %d DB更新失败 status=%s: %v", shot.ShotNo, shot.Status, dbErr)
		return dbErr
	}
	return nil
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
		videoTenantID = s.videoTenantID(video)
	}
	logger.Printf("[StitchVideo] videoID=%d: aspectRatio=%s", videoID, aspectRatio)

	// P2-2: include UUID to prevent directory collision when the same videoID is synthesized concurrently
	tmpDir := fmt.Sprintf("%s/inkframe-%d-%s", inkframeTempDir(), videoID, uuid.New().String()[:8])
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("video synthesis: panic in cleanup: %v", r)
		}
		os.RemoveAll(tmpDir)
	}()

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
					if e := s.videoRepo.UpdateFields(videoID, map[string]interface{}{"progress": pct}); e != nil { logger.Errorf("[VideoService] videoRepo.UpdateFields progress videoID=%d: %v", videoID, e) }
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
				dlCtx, dlCancel := context.WithTimeout(ctx, 10*time.Minute)
				dlErr := downloadFileCtx(dlCtx, url, dest)
				dlCancel()
				if dlErr != nil {
					// URL 可能已过期，尝试从 provider 重新获取
					if sh.ShotTaskID != "" && sh.ShotProviderName != "" {
						if p, _, rErr := s.resolveVideoProvider(videoTenantID, sh.ShotProviderName); rErr == nil {
							rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
							freshURL, fErr := p.GetVideoURL(rCtx, sh.ShotTaskID)
							rCancel()
							if fErr == nil {
								logger.Printf("[StitchVideo] shot %d: got fresh URL, retrying download", sh.ShotNo)
								retryCtx, retryCancel := context.WithTimeout(ctx, 10*time.Minute)
								results[idx].downloadErr = downloadFileCtx(retryCtx, freshURL, dest)
								retryCancel()
							} else {
								results[idx].downloadErr = fmt.Errorf("download failed and refresh URL failed: %w", dlErr)
							}
						} else {
							results[idx].downloadErr = dlErr
						}
					} else {
						results[idx].downloadErr = dlErr
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
					if e := s.videoRepo.UpdateFields(videoID, map[string]interface{}{"progress": pct}); e != nil { logger.Errorf("[VideoService] videoRepo.UpdateFields progress videoID=%d: %v", videoID, e) }
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
		logger.Errorf("[StitchVideo] shot %d: processing (clipFile=%q imgFile=%q err=%v)",
			shot.ShotNo, res.clipFile, res.imgFile, res.downloadErr)

		if res.downloadErr != nil {
			logger.Errorf("[StitchVideo] shot %d: download error — skipping: %v", shot.ShotNo, res.downloadErr)
			continue
		}

		var videoClipPath string

		if res.imgFile != "" {
			// P1-2: image-only 镜头优先用纯 Go Ken Burns（可 context 取消，~10-20s/5s clip）。
			// 失败时降级到 still frame（-loop 1，x264 全 P 帧）。
			duration := shot.Duration
			if duration <= 0 {
				duration = defaultShotDurationSecs
			}
			var clipErr error
			videoClipPath, clipErr = s.generateKenBurnsPureGo(ctx, shot, res.imgFile, duration, aspectRatio)
			if clipErr != nil {
				logger.Errorf("[StitchVideo] shot %d: Ken Burns failed: %v — falling back to still frame", shot.ShotNo, clipErr)
				videoClipPath, clipErr = s.generateStillFrameClip(res.imgFile, duration, aspectRatio)
			}
			os.Remove(res.imgFile)
			if clipErr != nil {
				logger.Errorf("[StitchVideo] shot %d: still frame failed: %v — skipping", shot.ShotNo, clipErr)
				if e := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"status": "failed", "error_message": fmt.Sprintf("still frame generation failed: %v", clipErr)}); e != nil {
					logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d: %v", shot.ID, e)
				}
				continue
			}
			logger.Printf("[StitchVideo] shot %d: image clip ready: %s", shot.ShotNo, videoClipPath)
		} else if res.clipFile != "" {
			if res.isLocal {
				defer os.Remove(res.clipFile) //nolint:errcheck
			}
			videoClipPath = res.clipFile
		} else {
			logger.Printf("[StitchVideo] shot %d: no clip or image — skipping", shot.ShotNo)
			continue
		}

		// P0-3: ffprobe 探测实际时长，更新 DB 保证字幕时序准确
		if actualDur := probeClipDuration(ctx, videoClipPath); actualDur > 0 && math.Abs(actualDur-shot.Duration) > 0.15 {
			if updErr := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"duration": actualDur}); updErr == nil {
				logger.Printf("[StitchVideo] shot %d: duration corrected %.2f→%.2f", shot.ShotNo, shot.Duration, actualDur)
				shot.Duration = actualDur
			}
		}

		finalClip := videoClipPath

		// P0-2: 构建音频（优先 VoiceSegments+SFX，降级 AudioPath，再降级静音轨）
		// P1-1: buildShotAudio 同时返回音频时长，避免后续 ffprobe 重复探测
		audioPath, audioDurSecs := s.buildShotAudio(ctx, shot, tmpDir, i)
		if audioPath != "" {
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			logger.Printf("[StitchVideo] shot %d: merging audio: %s", shot.ShotNo, audioPath)
			mergeCtx, mergeCancel := context.WithTimeout(ctx, 90*time.Second)
			// P1-4: 根据音频与视频时长决策：音频明显更长时用 tpad 延伸视频末帧
			mergeArgs := buildAudioMergeArgs(audioPath, shot.Duration, audioDurSecs)
			allMergeArgs := append([]string{"-y", "-i", videoClipPath, "-i", audioPath}, append(mergeArgs, mergedFile)...)
			_, mergeErr := runFFmpegCtx(mergeCtx, allMergeArgs...)
			mergeCancel()
			if mergeErr != nil {
				logger.Errorf("[StitchVideo] shot %d: audio merge failed: %v — using clip without audio", shot.ShotNo, mergeErr)
			} else {
				logger.Printf("[StitchVideo] shot %d: audio merged OK", shot.ShotNo)
				finalClip = mergedFile
			}
		}

		escapedClip := strings.ReplaceAll(finalClip, "'", "\\'") // P1-1: escape single quotes (same fix as concatAudioFiles)
		concatLines = append(concatLines, fmt.Sprintf("file '%s'", escapedClip))
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
	// P0-1: 统一编解码/分辨率/帧率，消除 Kling(H.264) vs Seedance(H.265) 等格式不兼容导致的 concat 失败。
	// 每个 clip 编码留 60s 余量 + 基础 5 分钟，最多 45 分钟。
	concatTimeout := time.Duration(len(concatLines))*60*time.Second + 5*time.Minute
	if concatTimeout > 45*time.Minute {
		concatTimeout = 45 * time.Minute
	}
	// 根据宽高比生成规范化 vf
	normW, normH := "1920", "1080"
	switch aspectRatio {
	case "9:16":
		normW, normH = "1080", "1920"
	case "1:1":
		normW, normH = "1080", "1080"
	case "4:3":
		normW, normH = "1440", "1080"
	}
	normVF := fmt.Sprintf("scale=%s:%s:force_original_aspect_ratio=decrease,pad=%s:%s:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=24", normW, normH, normW, normH)
	if concatOut, concatErr := runFFmpegWithGoroutineTimeout(concatTimeout, "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-vf", normVF,
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "23",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-ar", "44100", "-ac", "2",
		"-movflags", "+faststart",
		stitchedPath,
	); concatErr != nil {
		logger.Errorf("[StitchVideo] videoID=%d: ffmpeg concat failed: %v\noutput: %s", videoID, concatErr, string(concatOut))
		if vid, ferr := s.videoRepo.GetByID(videoID); ferr == nil && vid != nil {
			vid.Status = "failed"
			vid.ErrorMessage = fmt.Sprintf("ffmpeg stitch failed: %v", concatErr)
			if updErr := s.videoRepo.Update(vid); updErr != nil {
				logger.Errorf("[StitchVideo] videoID=%d: failed to update status to failed: %v", videoID, updErr)
			}
		}
		return "", fmt.Errorf("ffmpeg stitch failed: %w", concatErr)
	}
	logger.Printf("[StitchVideo] videoID=%d: ffmpeg concat done", videoID)

	// P2-5: 端到端时长验证（仅警告，不阻断输出）
	{
		var expectedDur float64
		for _, sh := range shots {
			if sh.Duration > 0 {
				expectedDur += sh.Duration
			}
		}
		if expectedDur > 0 {
			if actualDur := probeClipDuration(ctx, stitchedPath); actualDur > 0 {
				diff := math.Abs(actualDur-expectedDur) / expectedDur
				if diff > 0.05 {
					logger.Printf("[StitchVideo] videoID=%d: duration mismatch: expected=%.2fs actual=%.2fs (%.1f%% diff)", videoID, expectedDur, actualDur, diff*100)
				}
			}
		}
	}

	// BGM 混音（非致命：失败时使用无BGM版本）
	outputPath := fmt.Sprintf("%s/inkframe-%d-output.mp4", inkframeTempDir(), videoID)
	if s.bgmService != nil {
		// P1-1: 根据分镜情感基调选择 BGM，而非固定传空字符串
		emotion := dominantEmotion(shots)
		bgmURL := s.bgmService.SelectBGM(emotion)
		if bgmURL != "" {
			logger.Printf("[StitchVideo] videoID=%d: mixing BGM: %s", videoID, bgmURL)
			if mixErr := s.bgmService.MixBGM(ctx, stitchedPath, bgmURL, outputPath); mixErr != nil {
				logger.Errorf("[StitchVideo] videoID=%d: BGM mix failed: %v — using stitched without BGM", videoID, mixErr)
				outputPath = stitchedPath
			} else {
				logger.Printf("[StitchVideo] videoID=%d: BGM mix done", videoID)
				// P2-2: stitchedPath is the pre-BGM intermediate; outputPath is now the final.
				// Remove the intermediate to avoid disk accumulation on long-running servers.
				os.Remove(stitchedPath) //nolint:errcheck
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
		logger.Errorf("[StitchVideo] videoID=%d: video record not found after stitch, status NOT updated: %v", videoID, err)
		return outputPath, fmt.Errorf("stitch succeeded but video record not found: %w", err)
	}
	video.VideoPath = outputPath
	video.Status = "completed"
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[StitchVideo] videoID=%d: failed to update status to completed: %v", videoID, err)
	}

	logger.Printf("[StitchVideo] videoID=%d: done → %s", videoID, outputPath)
	return outputPath, nil
}

// ─── Poll & Stitch Pipeline ───────────────────────────────────────────────────

// RecoverActivePollTasks 服务重启后恢复状态为 "generating" 的视频轮询任务。
// 在 main.go 完成所有服务 wiring 后调用（goroutine 方式，非阻塞）。
func (s *VideoService) RecoverActivePollTasks() {
	var videos []model.Video
	if err := s.videoRepo.DB().
		Where("status = ?", "generating").
		Find(&videos).Error; err != nil {
		logger.Errorf("[VideoService] RecoverActivePollTasks: query failed: %v", err)
		return
	}
	for _, v := range videos {
		v := v
		if _, loaded := s.activePoll.LoadOrStore(v.ID, struct{}{}); !loaded {
			logger.Printf("[VideoService] RecoverActivePollTasks: resuming poll for video %d", v.ID)
			go func() {
				s.activePoll.Delete(v.ID) // PollAndStitchVideo will re-register
				s.PollAndStitchVideo(v.ID)
			}()
		}
	}
}

// PollAndStitchVideo 后台轮询所有分镜状态，完成后拼接
func (s *VideoService) PollAndStitchVideo(videoID uint) {
	// Cross-instance dedup via Redis SETNX; fallback to local sync.Map.
	if s.cache != nil {
		redisKey := fmt.Sprintf("lock:video:poll:%d", videoID)
		ok, err := s.cache.SetNX(context.Background(), redisKey, "1", 2*time.Hour).Result()
		if err == nil {
			if !ok {
				logger.Printf("PollAndStitchVideo: videoID %d already polling on another instance, skip", videoID)
				return
			}
			defer s.cache.Del(context.Background(), redisKey)
		}
		// err != nil: Redis unavailable, fall through to local check
	}
	if _, loaded := s.activePoll.LoadOrStore(videoID, struct{}{}); loaded {
		logger.Printf("PollAndStitchVideo: videoID %d already polling, skip", videoID)
		return
	}
	defer s.activePoll.Delete(videoID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// P2-2: 自适应轮询间隔（15s→30s→60s），降低空闲时的 DB 压力
	pollInterval := 15 * time.Second
	startedAt := time.Now()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	noProgressCount := 0

	const heartbeatStallDuration = 30 * time.Minute
	lastProgressAt := time.Now()
	lastCompletedCount := -1

	for {
		select {
		case <-ctx.Done():
			logger.Printf("PollAndStitchVideo: videoID %d timed out after 2h", videoID)
			video, _ := s.videoRepo.GetByID(videoID)
			if video != nil && video.Status != "completed" {
				video.Status = "failed"
				video.ErrorMessage = "stitch pipeline timed out (>2h)"
				if err := s.videoRepo.Update(video); err != nil {
					logger.Errorf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (timeout): %v", videoID, err)
				}
			}
			return
		case <-s.stopCh:
			logger.Printf("PollAndStitchVideo: videoID %d shutdown", videoID)
			return
		case <-ticker.C:
		}

		// P2-2: 根据已等待时长自适应调整轮询间隔
		elapsed := time.Since(startedAt)
		var newInterval time.Duration
		switch {
		case elapsed < 5*time.Minute:
			newInterval = 15 * time.Second
		case elapsed < 15*time.Minute:
			newInterval = 30 * time.Second
		default:
			newInterval = 60 * time.Second
		}
		if newInterval != pollInterval {
			ticker.Reset(newInterval)
			pollInterval = newInterval
			logger.Printf("PollAndStitchVideo: videoID %d poll interval → %v", videoID, pollInterval)
		}

		// P2-1: 单次查询获取所有分镜，内存分组，避免 N+1 DB 查询
		allShots, _ := s.storyboardRepo.ListByVideo(videoID)
		if len(allShots) == 0 {
			continue
		}
		var pending, processing []*model.StoryboardShot
		completedCount, failedCount := 0, 0
		for _, shot := range allShots {
			switch shot.Status {
			case "pending":
				pending = append(pending, shot)
			case "processing":
				processing = append(processing, shot)
			case "completed":
				completedCount++
			case "failed":
				failedCount++
			}
		}

		// Retry pending shots (from consistency/failed retry)
		if len(pending) > 0 {
			video, _ := s.videoRepo.GetByID(videoID)
			aspectRatio := "16:9"
			if video != nil {
				aspectRatio = video.AspectRatio
			}
			for _, shot := range pending {
				if shot.ShotTaskID == "" {
					if err := s.GenerateShotVideo(shot, aspectRatio); err != nil {
						logger.Errorf("[PollAndStitch] GenerateShotVideo shot %d failed: %v", shot.ID, err)
						if e := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{"status": "failed", "error_message": fmt.Sprintf("generation failed: %v", err)}); e != nil {
							logger.Errorf("[VideoService] storyboardRepo.UpdateFields shot %d: %v", shot.ID, e)
						}
					}
				}
			}
		}

		// Poll processing shots
		for _, shot := range processing {
			if err := s.PollShotStatus(shot); err != nil {
				logger.Errorf("[PollAndStitch] PollShotStatus shot %d failed: %v", shot.ID, err)
			}
		}

		// Heartbeat: update lastProgressAt whenever completedCount advances.
		if completedCount != lastCompletedCount {
			lastProgressAt = time.Now()
			lastCompletedCount = completedCount
		}

		// Heartbeat stall detection: no shot has completed in 30 minutes.
		if time.Since(lastProgressAt) > heartbeatStallDuration {
			logger.Printf("PollAndStitchVideo: videoID %d stalled — no progress for %.0f minutes",
				videoID, heartbeatStallDuration.Minutes())
			if vid, err := s.videoRepo.GetByID(videoID); err == nil && vid != nil && vid.Status == "generating" {
				vid.Status = "failed"
				vid.ErrorMessage = fmt.Sprintf("video generation stalled: no progress for %.0f minutes", heartbeatStallDuration.Minutes())
				if e := s.videoRepo.Update(vid); e != nil { logger.Errorf("[VideoService] videoRepo.Update videoID=%d: %v", vid.ID, e) }
			}
			return
		}

		if completedCount+failedCount == len(allShots) {
			if completedCount > 0 {
				if _, err := s.StitchVideoCtx(ctx, videoID); err != nil {
					logger.Errorf("PollAndStitchVideo: stitch failed: %v", err)
					video, _ := s.videoRepo.GetByID(videoID)
					if video != nil {
						video.Status = "failed"
						video.ErrorMessage = err.Error()
						if updErr := s.videoRepo.Update(video); updErr != nil {
							logger.Errorf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (stitch error): %v", videoID, updErr)
						}
					}
				}
			} else {
				video, _ := s.videoRepo.GetByID(videoID)
				if video != nil {
					video.Status = "failed"
					video.ErrorMessage = "all shots failed"
					if updErr := s.videoRepo.Update(video); updErr != nil {
						logger.Errorf("[VideoService] PollAndStitchVideo: failed to update video %d status to failed (all shots failed): %v", videoID, updErr)
					}
				}
			}
			return
		}

		// Stall detection: no active work after 5 ticks
		if len(processing) == 0 && len(pending) == 0 {
			noProgressCount++
			if noProgressCount >= 5 {
				logger.Printf("PollAndStitchVideo: videoID %d stalled (no active shots), stopping", videoID)
				if vid, err := s.videoRepo.GetByID(videoID); err == nil && vid.Status == "generating" {
					vid.Status = "failed"
					vid.ErrorMessage = "generation stalled (no progress)"
					if e := s.videoRepo.Update(vid); e != nil { logger.Errorf("[VideoService] videoRepo.Update videoID=%d: %v", vid.ID, e) }
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

	// 租户隔离校验（通过小说归属验证，而非直接比较 video.TenantID）
	if tenantID != 0 && !s.videoBelongsToTenant(video, tenantID) {
		return "", fmt.Errorf("access denied: video %d does not belong to tenant %d", videoID, tenantID)
	}

	// 创建异步任务
	var taskID string
	if s.taskSvc != nil {
		task, err := s.taskSvc.Create(tenantID, TaskTypeVideoSynthesis, "视频合成", "video", videoID)
		if err != nil {
			return "", fmt.Errorf("create task: %w", err)
		}
		taskID = task.TaskID
		_ = s.taskSvc.SetParams(taskID, map[string]interface{}{
			"video_id": videoID,
		})
	} else {
		taskID = fmt.Sprintf("synth-%d", videoID)
	}
	logger.Printf("[SynthesizeVideo] videoID=%d: taskID=%s", videoID, taskID)

	go s.RunSynthesisPipeline(taskID, videoID)
	return taskID, nil
}

// RunSynthesisPipeline 执行合成流水线核心逻辑（由 SynthesizeVideo 和 resume handler 共享）。
// 该方法阻塞直到流水线完成，应在独立 goroutine 中调用。
func (s *VideoService) RunSynthesisPipeline(taskID string, videoID uint) {
	synthCtx, synthCancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer synthCancel()

	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[SynthesizeVideo] panic recovered videoID=%d: %v", videoID, r)
			metrics.VideoSynthesisTotal.WithLabelValues("error").Inc()
			if s.taskSvc != nil {
				_ = s.taskSvc.Fail(taskID, "内部错误")
			}
		}
	}()

	metrics.VideoSynthesisInFlight.Inc()
	defer metrics.VideoSynthesisInFlight.Dec()
	synthStart := time.Now()

	// 统一管理所有临时文件：退出时一并清理（无论成功/失败）
	var tempFiles []string
	defer func() {
		for _, f := range tempFiles {
			if f != "" {
				os.Remove(f)
			}
		}
	}()

	if s.taskSvc != nil {
		_ = s.taskSvc.SetRunning(taskID)
	}

	// 重新从 DB 加载 video（避免使用调用方持有的过期对象）
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		logger.Errorf("[SynthesizeVideo] videoID=%d: reload video failed: %v", videoID, err)
		if s.taskSvc != nil {
			_ = s.taskSvc.Fail(taskID, "video not found on pipeline start")
		}
		return
	}

	// 0. BGM 覆盖率预检（可选；bgmService + bgmRepo 均配置时才检查）
	// 覆盖率不完整时：自动修复 gap（延伸前段 EndShotNo）并继续合成，不阻断流程。
	if s.bgmService != nil && s.bgmRepo != nil {
		allShots, shotsErr := s.storyboardRepo.ListByVideo(videoID)
		if shotsErr == nil && len(allShots) > 0 {
			if coverErr := s.bgmService.ValidateCoverageBeforeSynthesis(synthCtx, videoID, allShots, s.bgmRepo); coverErr != nil {
				logger.Errorf("[SynthesizeVideo] videoID=%d: BGM coverage gap detected, auto-repairing and continuing: %v", videoID, coverErr)
				if repairErr := s.bgmService.RepairCoverageGaps(synthCtx, videoID, allShots, s.bgmRepo); repairErr != nil {
					logger.Errorf("[SynthesizeVideo] videoID=%d: BGM gap repair failed, proceeding without full BGM coverage: %v", videoID, repairErr)
				}
			}
		}
	}

	// 1. 拼接视频
	logger.Printf("[SynthesizeVideo] videoID=%d step=1/4: stitching video", videoID)
	if s.taskSvc != nil {
		_ = s.taskSvc.UpdateProgress(taskID, 10)
	}
	stitchCtx, stitchCancel := context.WithTimeout(synthCtx, 30*time.Minute)
	stitchedPath, err := s.StitchVideoCtx(stitchCtx, videoID)
	stitchCancel()
	if err != nil {
		logger.Errorf("[SynthesizeVideo] videoID=%d step=1/4: stitch failed: %v", videoID, err)
		metrics.VideoSynthesisTotal.WithLabelValues("error").Inc()
		metrics.VideoSynthesisDuration.Observe(time.Since(synthStart).Seconds())
		if s.taskSvc != nil {
			_ = s.taskSvc.Fail(taskID, "stitch failed: "+err.Error())
		}
		return
	}
	logger.Printf("[SynthesizeVideo] videoID=%d step=1/4: stitch OK → %s", videoID, stitchedPath)
	tempFiles = append(tempFiles, stitchedPath)

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
			tempFiles = append(tempFiles, assPath)
			if writeErr := os.WriteFile(assPath, []byte(assContent), 0644); writeErr == nil {
				burnedPath := fmt.Sprintf("%s/inkframe-%d-burned.mp4", inkframeTempDir(), videoID)
				tempFiles = append(tempFiles, burnedPath)
				burnCtx, burnCancel := context.WithTimeout(synthCtx, 20*time.Minute)
				burnErr := subtitleSvc.BurnSubtitles(burnCtx, stitchedPath, assPath, burnedPath)
				burnCancel()
				if burnErr == nil {
					logger.Printf("[SynthesizeVideo] videoID=%d step=2/4: subtitle burn OK → %s", videoID, burnedPath)
					finalPath = burnedPath
				} else {
					logger.Errorf("[SynthesizeVideo] videoID=%d step=2/4: subtitle burn failed: %v — skipping", videoID, burnErr)
				}
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
	tempFiles = append(tempFiles, coverPath)
	coverURL := ""
	// P1-3: 用视频时长 15% 位置提取封面，比固定 t=2s 更具代表性；失败时回退到 2s
	coverOffset := 2.0
	if totalDur := probeClipDuration(synthCtx, finalPath); totalDur > 4 {
		coverOffset = totalDur * 0.15
		if coverOffset < 1 {
			coverOffset = 1
		}
	}
	if _, err := runFFmpegWithGoroutineTimeout(30*time.Second, "-y",
		"-ss", fmt.Sprintf("%.2f", coverOffset),
		"-i", finalPath,
		"-frames:v", "1", "-vf", "scale=640:-1", coverPath); err == nil {
		logger.Printf("[SynthesizeVideo] videoID=%d step=3/4: cover extracted at %.1fs → %s", videoID, coverOffset, coverPath)
	} else {
		logger.Errorf("[SynthesizeVideo] videoID=%d step=3/4: cover extraction failed: %v — continuing", videoID, err)
	}

	// 4. 上传视频和封面到 OSS
	logger.Printf("[SynthesizeVideo] videoID=%d step=4/4: uploading to OSS", videoID)
	if s.taskSvc != nil {
		_ = s.taskSvc.UpdateProgress(taskID, 70)
	}
	finalVideoURL := ""

	if s.storageSvc != nil {
		// P2-4: 将 upload 包在匿名函数中，确保 uploadCancel 在上传完成后立即释放资源
		videoKey := fmt.Sprintf("videos/%s.mp4", uuid.New().String())
		// 上传视频
		finalVideoURL = func() string {
			uploadCtx, uploadCancel := context.WithTimeout(synthCtx, 30*time.Minute)
			defer uploadCancel()
			logger.Printf("[SynthesizeVideo] videoID=%d: uploading video to key=%s", videoID, videoKey)
			vf, err := os.Open(finalPath)
			if err != nil {
				return ""
			}
			defer vf.Close()
			fi, err := vf.Stat()
			if err != nil {
				return ""
			}
			logger.Printf("[SynthesizeVideo] videoID=%d: video file size=%.1fMB", videoID, float64(fi.Size())/1e6)
			ossURL, err := s.storageSvc.Upload(uploadCtx, videoKey, vf, fi.Size(), "video/mp4")
			if err != nil {
				logger.Errorf("[SynthesizeVideo] videoID=%d: video upload failed: %v", videoID, err)
				return ""
			}
			logger.Printf("[SynthesizeVideo] videoID=%d: video upload OK → %s", videoID, ossURL)
			return ossURL
		}()

		// 上传封面
		if videoKey != "" {
			coverURL = func() string {
				uploadCtx, uploadCancel := context.WithTimeout(synthCtx, 10*time.Minute)
				defer uploadCancel()
				coverKey := videoKey[:len(videoKey)-4] + "_cover.jpg"
				logger.Printf("[SynthesizeVideo] videoID=%d: uploading cover key=%s", videoID, coverKey)
				cf, err := os.Open(coverPath)
				if err != nil {
					return ""
				}
				defer cf.Close()
				fi, err := cf.Stat()
				if err != nil {
					return ""
				}
				ossURL, err := s.storageSvc.Upload(uploadCtx, coverKey, cf, fi.Size(), "image/jpeg")
				if err != nil {
					logger.Errorf("[SynthesizeVideo] videoID=%d: cover upload failed: %v — continuing", videoID, err)
					return ""
				}
				logger.Printf("[SynthesizeVideo] videoID=%d: cover upload OK → %s", videoID, ossURL)
				return ossURL
			}()
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
		metrics.VideoSynthesisTotal.WithLabelValues("success").Inc()
		metrics.VideoSynthesisDuration.Observe(time.Since(synthStart).Seconds())
		logger.Printf("[SynthesizeVideo] videoID=%d: pipeline complete", videoID)
	} else {
		video.Status = "failed"
		metrics.VideoSynthesisTotal.WithLabelValues("error").Inc()
		metrics.VideoSynthesisDuration.Observe(time.Since(synthStart).Seconds())
		logger.Errorf("[SynthesizeVideo] videoID=%d: upload failed — marking video as failed", videoID)
	}
	if err := s.videoRepo.Update(video); err != nil {
		logger.Errorf("[SynthesizeVideo] videoID=%d: DB update failed: %v", videoID, err)
	}

	if s.taskSvc != nil {
		if finalVideoURL != "" {
			result := map[string]string{"final_video_url": finalVideoURL, "cover_url": coverURL}
			_ = s.taskSvc.Complete(taskID, result)
		} else {
			_ = s.taskSvc.Fail(taskID, "video upload failed, no URL generated")
		}
	}

	logger.Printf("[SynthesizeVideo] videoID=%d: pipeline done", videoID)
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

// downloadFileCtx P1-5: 带 context 的 HTTP 下载，支持超时取消。
func downloadFileCtx(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec
	if err != nil {
		return err
	}
	resp, err := downloadHTTPClient.Do(req)
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

// downloadFile 下载 HTTP URL 到本地路径（不带 context，供旧代码兼容）
func downloadFile(url, dest string) error {
	return downloadFileCtx(context.Background(), url, dest)
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

// generateSilentAudio 生成指定时长的静音 AAC 轨道，返回本地路径。
// P2-3: 直接输出 AAC（而非 MP3 再转码），与合成路径的 -c:a aac 保持一致，减少一次无谓的转码。
// Returns "" if FFmpeg fails, so callers must handle the empty case.
func generateSilentAudio(dir string, shotNo int, durationSecs float64) string {
	if durationSecs <= 0 {
		durationSecs = 5.0
	}
	outPath := filepath.Join(dir, fmt.Sprintf("silent_%d.aac", shotNo))
	out, err := runFFmpegCtx(context.Background(),
		"-y",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", fmt.Sprintf("%.3f", durationSecs),
		"-c:a", "aac", "-b:a", "128k",
		outPath,
	)
	if err != nil {
		logger.Errorf("generateSilentAudio: shot %d ffmpeg failed: %v\n%s", shotNo, err, string(out))
		return ""
	}
	return outPath
}

// ─── Synthesis helpers ────────────────────────────────────────────────────────

// probeClipDuration P0-3: 用 ffprobe 探测文件实际时长（秒）。失败时返回 0。
func probeClipDuration(ctx context.Context, path string) float64 {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := runFFprobeCtx(probeCtx,
		"-v", "quiet",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	if err != nil || len(out) == 0 {
		return 0
	}
	dur, parseErr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if parseErr != nil {
		return 0
	}
	return dur
}

// buildShotAudio P0-2: 构建分镜音频轨道。
// 优先级：VoiceSegments（多段拼接）→ SFX 叠加 → 旧版 AudioPath → 静音轨。
// 返回值：(audioPath, audioDurationSecs)。audioDurationSecs 从 VoiceSegment.DurationSecs 累加，
// 0 表示未知；调用方应以 shot.Duration 作为兜底值。
func (s *VideoService) buildShotAudio(ctx context.Context, shot *model.StoryboardShot, tmpDir string, idx int) (string, float64) {
	// 1. 收集 VoiceSegment 音频路径（按 SeqNo 排序）并累计已知时长
	var voicePaths []string
	var segsDurSecs float64
	if s.segmentRepo != nil {
		segs, err := s.segmentRepo.ListByShotID(shot.ID)
		if err == nil {
			for _, seg := range segs {
				if seg.AudioPath != "" {
					voicePaths = append(voicePaths, strings.TrimPrefix(seg.AudioPath, "file://"))
				}
				segsDurSecs += seg.DurationSecs
			}
		}
	}

	var speechAudioPath string
	var audioDur float64
	switch len(voicePaths) {
	case 0:
		// 无 VoiceSegment，使用旧版 AudioPath
		if shot.AudioPath != "" {
			speechAudioPath = strings.TrimPrefix(shot.AudioPath, "file://")
			// P1-1: probe duration once so tpad can trigger when audio exceeds video length
			if d := probeClipDuration(ctx, speechAudioPath); d > 0 {
				audioDur = d
			}
		}
	case 1:
		speechAudioPath = voicePaths[0]
		audioDur = segsDurSecs
	default:
		// 多段：用 FFmpeg concat 拼接
		if concatPath, err := concatAudioFiles(ctx, voicePaths, tmpDir, idx); err == nil {
			speechAudioPath = concatPath
			audioDur = segsDurSecs
		} else {
			logger.Errorf("[buildShotAudio] shot %d: audio concat failed: %v — using first segment", shot.ShotNo, err)
			speechAudioPath = voicePaths[0]
		}
	}

	// 2. 叠加 SFX（通过 SFXService）
	if speechAudioPath != "" && s.sfxService != nil {
		sfxItems, err := s.sfxService.ListSFXItems(shot.ID)
		if err == nil && len(sfxItems) > 0 {
			if mixedPath, mixErr := mixSFXLayers(ctx, speechAudioPath, sfxItems, shot.Duration, tmpDir, idx); mixErr == nil {
				return mixedPath, audioDur
			} else {
				logger.Errorf("[buildShotAudio] shot %d: SFX mix failed: %v — using speech only", shot.ShotNo, mixErr)
			}
		}
	}

	// 3. 如果有语音音轨直接返回
	if speechAudioPath != "" {
		return speechAudioPath, audioDur
	}

	// 4. 降级：生成静音轨
	silentPath := generateSilentAudio(tmpDir, shot.ShotNo, shot.Duration)
	if silentPath != "" {
		logger.Printf("[buildShotAudio] shot %d: no audio — using generated silent track", shot.ShotNo)
	}
	return silentPath, 0
}

// concatAudioFiles 将多个音频文件用 FFmpeg concat demuxer 拼接为一个 AAC 文件。
func concatAudioFiles(ctx context.Context, paths []string, tmpDir string, idx int) (string, error) {
	listPath := fmt.Sprintf("%s/audio_list_%d.txt", tmpDir, idx)
	var lines []string
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, "'", "\\'") // P0-3: escape single quotes for FFmpeg concat format
		lines = append(lines, fmt.Sprintf("file '%s'", escaped))
	}
	if err := os.WriteFile(listPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return "", err
	}
	outPath := fmt.Sprintf("%s/audio_concat_%d.aac", tmpDir, idx)
	concatCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := runFFmpegCtx(concatCtx,
		"-y", "-f", "concat", "-safe", "0", "-i", listPath,
		"-c:a", "aac", "-b:a", "128k",
		outPath,
	)
	if err != nil {
		return "", fmt.Errorf("concat audio: %w", err)
	}
	return outPath, nil
}

// mixSFXLayers 将 SFX 音效叠加到语音轨道上。
// 使用 FFmpeg amix 过滤器按音量混合各 SFX 条目。
func mixSFXLayers(ctx context.Context, speechPath string, sfxItems []*model.ShotSFXItem, shotDuration float64, tmpDir string, idx int) (string, error) {
	// 过滤有 URL 且未禁用的 SFX 条目
	var active []*model.ShotSFXItem
	for _, item := range sfxItems {
		if !item.Disabled && item.URL != "" {
			active = append(active, item)
		}
	}
	if len(active) == 0 {
		return "", fmt.Errorf("no active SFX items")
	}

	// 下载 SFX 文件到临时目录
	type sfxFile struct {
		path        string
		volume      float64
		startOffset float64
	}
	var sfxFiles []sfxFile
	for i, item := range active {
		if item.URL == "" {
			continue
		}
		sfxPath := fmt.Sprintf("%s/sfx_%d_%d", tmpDir, idx, i)
		dlCtx, dlCancel := context.WithTimeout(ctx, 2*time.Minute)
		dlErr := downloadFileCtx(dlCtx, item.URL, sfxPath)
		dlCancel()
		if dlErr != nil {
			logger.Errorf("[mixSFXLayers] sfx item %d: download failed: %v — skipping", item.ID, dlErr)
			continue
		}
		vol := item.Volume
		if vol <= 0 {
			vol = 0.3
		}
		startOff := item.StartOffset
		if startOff < 0 {
			startOff = 0 // P2-5: 负偏移在 adelay 中无效，重置为 0
		}
		sfxFiles = append(sfxFiles, sfxFile{path: sfxPath, volume: vol, startOffset: startOff})
	}
	if len(sfxFiles) == 0 {
		return "", fmt.Errorf("all SFX downloads failed")
	}

	// 构建 amix filter_complex：speech + SFX inputs，各 SFX 按 volume 缩放后 adelay 对齐
	outPath := fmt.Sprintf("%s/audio_sfx_%d.aac", tmpDir, idx)
	var args []string
	args = append(args, "-y", "-i", speechPath)
	for _, sf := range sfxFiles {
		args = append(args, "-i", sf.path)
	}

	// 构建 filter_complex
	n := len(sfxFiles) + 1 // 1 speech + N sfx
	var filterParts []string
	// speech input [0:a] → volume=1.0 (保持语音原音量)
	filterParts = append(filterParts, "[0:a]aformat=sample_rates=44100:channel_layouts=stereo[speech]")
	for i, sf := range sfxFiles {
		sfxLabel := fmt.Sprintf("sfx%d", i)
		delayMs := int(sf.startOffset * 1000)
		filterParts = append(filterParts, fmt.Sprintf("[%d:a]aformat=sample_rates=44100:channel_layouts=stereo,adelay=%d|%d,volume=%.2f[%s]",
			i+1, delayMs, delayMs, sf.volume, sfxLabel))
	}
	// amix 合并所有轨道
	var mixInputs []string
	mixInputs = append(mixInputs, "[speech]")
	for i := range sfxFiles {
		mixInputs = append(mixInputs, fmt.Sprintf("[sfx%d]", i))
	}
	filterParts = append(filterParts, fmt.Sprintf("%samix=inputs=%d:duration=longest:dropout_transition=3[out]", strings.Join(mixInputs, ""), n))
	filterComplex := strings.Join(filterParts, ";")

	args = append(args, "-filter_complex", filterComplex, "-map", "[out]",
		"-c:a", "aac", "-b:a", "128k")
	if shotDuration > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", shotDuration))
	}
	args = append(args, outPath)

	mixCtx, mixCancel := context.WithTimeout(ctx, 60*time.Second)
	defer mixCancel()
	_, err := runFFmpegCtx(mixCtx, args...)
	if err != nil {
		return "", fmt.Errorf("sfx mix: %w", err)
	}
	return outPath, nil
}

// buildAudioMergeArgs P1-4: 根据音频与视频时长关系选择合适的合并参数。
// 若音频明显长于视频（>0.3s），用 tpad 延伸视频末帧，避免 TTS 被 -shortest 截断。
// P1-1: 接受已知时长参数，避免对每个分镜重复 ffprobe（消除每镜额外 15s 探测延迟）。
// audioDur=0 表示时长未知，此时使用 clipDur 作为兜底。
func buildAudioMergeArgs(audioPath string, clipDur, audioDur float64) []string {
	if audioDur <= 0 {
		audioDur = clipDur // 时长未知时保守处理，不触发 tpad
	}
	if audioDur > 0 && clipDur > 0 && audioDur > clipDur+0.3 {
		// 音频明显更长：用 tpad 克隆最后一帧延伸视频，避免 TTS 末尾被截断
		extra := audioDur - clipDur
		return []string{
			"-filter_complex", fmt.Sprintf("[0:v]tpad=stop_mode=clone:stop_duration=%.3f[v]", extra),
			"-map", "[v]", "-map", "1:a",
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "23",
			"-c:a", "aac",
		}
	}
	// 正常情况：视频时长 ≥ 音频时长，用 -shortest 保证输出与视频等长
	return []string{
		"-c:v", "copy",
		"-c:a", "aac",
		"-shortest",
	}
}

// dominantEmotion 从分镜列表中计算主导情感基调，用于 BGM 选择。
// P2-1: lexicographic tiebreak makes result deterministic regardless of map iteration order.
func dominantEmotion(shots []*model.StoryboardShot) string {
	counts := make(map[string]int)
	for _, sh := range shots {
		if t := strings.TrimSpace(sh.EmotionalTone); t != "" {
			counts[t]++
		}
	}
	best, bestN := "", 0
	for tone, n := range counts {
		if n > bestN || (n == bestN && (best == "" || tone < best)) {
			best, bestN = tone, n
		}
	}
	return best
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
		logger.Errorf("uploadClipToStorage: open %s: %v", clipPath, err)
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		logger.Errorf("uploadClipToStorage: stat %s: %v", clipPath, err)
		return ""
	}

	filename := uuid.New().String() + ".mp4"
	key := fmt.Sprintf("videos/%s", filename)

	ossURL, err := s.storageSvc.Upload(ctx, key, f, fi.Size(), "video/mp4")
	if err != nil {
		logger.Errorf("uploadClipToStorage: upload failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}
	return ossURL
}
