package service

// video_synthesis_service.go
//
// Video stitching, polling, and final synthesis methods
// extracted from video_service.go. All methods remain on *VideoService.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
	provider, ok := s.videoProviders[shot.ShotProviderName]
	if !ok {
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

	// 获取宽高比（Ken Burns 降级时使用）
	aspectRatio := "16:9"
	if video, verr := s.videoRepo.GetByID(videoID); verr == nil && video != nil && video.AspectRatio != "" {
		aspectRatio = video.AspectRatio
	}
	logger.Printf("[StitchVideo] videoID=%d: aspectRatio=%s", videoID, aspectRatio)

	tmpDir := fmt.Sprintf("%s/inkframe-%d", inkframeTempDir(), videoID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var localShotFiles []string // 记录 PollShotStatus 下载的本地文件，拼接后清理
	defer func() {
		for _, f := range localShotFiles {
			os.Remove(f) //nolint:errcheck
		}
	}()
	var concatLines []string
	for i, shot := range shots {
		logger.Printf("[StitchVideo] shot %d: clipPath=%q videoURL=%q imageURL=%q audioPath=%q duration=%.1fs",
			shot.ShotNo, shot.ClipPath, shot.VideoURL, shot.ImageURL, shot.AudioPath, shot.Duration)

		// 镜头无视频 clip/URL，但有图片 → 实时生成 Ken Burns 片段
		if shot.ClipPath == "" && shot.VideoURL == "" {
			if shot.ImageURL == "" {
				logger.Printf("[StitchVideo] shot %d: no clip, video URL, or image — skipping", shot.ShotNo)
				continue
			}
			duration := shot.Duration
			if duration <= 0 {
				duration = defaultShotDurationSecs
			}
			logger.Printf("[StitchVideo] shot %d: image-only, generating Ken Burns (duration=%.1fs)", shot.ShotNo, duration)
			localImage, dlErr := downloadToTemp(shot.ImageURL, fmt.Sprintf("inkframe-img-%d-", shot.ID), ".jpg")
			if dlErr != nil {
				logger.Printf("[StitchVideo] shot %d: image download failed: %v — skipping", shot.ShotNo, dlErr)
				continue
			}
			// Ken Burns 每镜头超时 = max(120s, duration*20) 留出足够余量
			kbTimeout := time.Duration(duration*20)*time.Second + 120*time.Second
			kbCtx, kbCancel := context.WithTimeout(ctx, kbTimeout)
			kbPath, kbErr := s.generateKenBurnsPureGo(kbCtx, shot, localImage, duration, aspectRatio)
			kbCancel()
			if kbErr != nil {
				logger.Printf("[StitchVideo] shot %d: Ken Burns failed (%v), falling back to still frame", shot.ShotNo, kbErr)
				kbPath, kbErr = s.generateStillFrameClip(localImage, duration, aspectRatio)
			}
			os.Remove(localImage)
			if kbErr != nil {
				logger.Printf("[StitchVideo] shot %d: Ken Burns + still frame both failed: %v — skipping", shot.ShotNo, kbErr)
				continue
			}
			logger.Printf("[StitchVideo] shot %d: Ken Burns clip ready: %s", shot.ShotNo, kbPath)
			concatLines = append(concatLines, fmt.Sprintf("file '%s'", kbPath))
			continue
		}

		clipFile := fmt.Sprintf("%s/clip_%d.mp4", tmpDir, i)
		finalClip := clipFile

		// 如果已是本地文件（PollShotStatus 立即下载过），直接使用，无需再下载
		if strings.HasPrefix(shot.ClipPath, "file://") {
			clipFile = strings.TrimPrefix(shot.ClipPath, "file://")
			finalClip = clipFile
			localShotFiles = append(localShotFiles, clipFile)
			logger.Printf("[StitchVideo] shot %d: using local clip: %s", shot.ShotNo, clipFile)
		} else {
			// 远端 URL：优先用 ClipPath，fallback 到 VideoURL
			remoteURL := shot.ClipPath
			if remoteURL == "" {
				remoteURL = shot.VideoURL
			}
			logger.Printf("[StitchVideo] shot %d: downloading from %s", shot.ShotNo, remoteURL)
			if err := downloadFile(remoteURL, clipFile); err != nil {
				logger.Printf("[StitchVideo] shot %d: download failed (%v), trying fresh URL from provider", shot.ShotNo, err)
				// URL 可能已过期，尝试从 provider 重新获取
				if shot.ShotTaskID != "" && shot.ShotProviderName != "" {
					if p, ok := s.videoProviders[shot.ShotProviderName]; ok {
						rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
						freshURL, fErr := p.GetVideoURL(rCtx, shot.ShotTaskID)
						rCancel()
						if fErr == nil {
							logger.Printf("[StitchVideo] shot %d: got fresh URL, retrying download", shot.ShotNo)
							if err2 := downloadFile(freshURL, clipFile); err2 != nil {
								return "", fmt.Errorf("download shot %d clip failed (fresh URL also failed): %w", shot.ShotNo, err2)
							}
						} else {
							return "", fmt.Errorf("download shot %d clip failed and refresh URL failed: %w", shot.ShotNo, err)
						}
					} else {
						return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
					}
				} else {
					return "", fmt.Errorf("download shot %d clip failed: %w", shot.ShotNo, err)
				}
			}
			logger.Printf("[StitchVideo] shot %d: download complete: %s", shot.ShotNo, clipFile)
		}

		// Merge audio if present
		if shot.AudioPath != "" {
			audioPath := strings.TrimPrefix(shot.AudioPath, "file://")
			mergedFile := fmt.Sprintf("%s/clip_audio_%d.mp4", tmpDir, i)
			logger.Printf("[StitchVideo] shot %d: merging audio: %s", shot.ShotNo, audioPath)
			mergeCtx, mergeCancel := context.WithTimeout(ctx, 60*time.Second)
			_, mergeErr := runFFmpegCtx(mergeCtx, "-y",
				"-i", clipFile,
				"-i", audioPath,
				"-c:v", "copy",
				"-c:a", "aac",
				"-shortest",
				mergedFile,
			)
			mergeCancel()
			if mergeErr != nil {
				logger.Printf("[StitchVideo] shot %d: audio merge failed: %v — using clip without audio", shot.ShotNo, err)
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
	// concat 超时 = 每个 clip 2 分钟，最少 5 分钟
	concatTimeout := time.Duration(len(concatLines))*2*time.Minute + 5*time.Minute
	concatCtx, concatCancel := context.WithTimeout(ctx, concatTimeout)
	defer concatCancel()
	if _, err := runFFmpegCtx(concatCtx, "-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		stitchedPath,
	); err != nil {
		return "", fmt.Errorf("ffmpeg stitch failed: %w", err)
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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Hour)
	noProgressCount := 0

	for {
		if time.Now().After(deadline) {
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
		}

		<-ticker.C

		// Retry pending shots (from consistency/failed retry)
		pending, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "pending")
		for _, shot := range pending {
			if shot.ShotTaskID == "" {
				video, _ := s.videoRepo.GetByID(videoID)
				aspectRatio := "16:9"
				if video != nil {
					aspectRatio = video.AspectRatio
				}
				s.GenerateShotVideo(shot, aspectRatio) //nolint:errcheck
			}
		}

		// Poll processing shots
		processing, _ := s.storyboardRepo.ListByVideoAndStatus(videoID, "processing")
		for _, shot := range processing {
			s.PollShotStatus(shot) //nolint:errcheck
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
				if _, err := s.StitchVideo(videoID); err != nil {
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
		if _, err := runFFmpegCtx(synthCtx, "-y", "-ss", "2", "-i", finalPath,
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
	_, err = io.Copy(f, resp.Body)
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
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
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
	data, err := os.ReadFile(clipPath)
	if err != nil {
		logger.Printf("uploadClipToStorage: read %s: %v", clipPath, err)
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

	ossURL, err := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), "video/mp4")
	if err != nil {
		logger.Printf("uploadClipToStorage: upload failed for shot %d: %v", shot.ShotNo, err)
		return ""
	}
	return ossURL
}
