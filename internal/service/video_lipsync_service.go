package service

import (
	"context"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

const (
	lipSyncTaskPrefix    = "lipsync:"
	lipSyncPollInterval  = 3 * time.Second
	lipSyncPollTimeout   = 10 * time.Minute
)

// LipSyncRequest 口型对齐请求（来自 handler）
type LipSyncRequest struct {
	AudioURL  string `json:"audio_url"`  // 音频 URL（可选，优先使用分镜已生成的音频）
	ImageURL  string `json:"image_url"`  // 角色图 URL（可选，优先使用分镜已生成的图片）
	Model     string `json:"model"`      // 可选，覆盖默认模型
	UseFirstSegment bool `json:"use_first_segment"` // true=使用第一条语音段落的音频（默认行为）
}

// LipSyncResult 口型对齐结果
type LipSyncResult struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	VideoURL string `json:"video_url,omitempty"`
}

// GenerateLipSyncVideo 为指定分镜生成口型对齐视频。
//
// 音频来源（优先级从高到低）：
//  1. req.AudioURL（请求体显式指定）
//  2. 分镜第一条语音段落的 AudioPath
//  3. 分镜 TaskMeta.AudioPath（历史兼容）
//
// 角色图来源（优先级从高到低）：
//  1. req.ImageURL（请求体显式指定）
//  2. 分镜 ImageURL
//
// 结果视频 URL 写回 shot.VideoURL；任务 ID 写到 shot.TaskMeta.ShotTaskID（"lipsync:" 前缀）。
func (s *VideoService) GenerateLipSyncVideo(videoID, shotID uint) (*LipSyncResult, error) {
	return s.GenerateLipSyncVideoWithReq(videoID, shotID, LipSyncRequest{})
}

func (s *VideoService) GenerateLipSyncVideoWithReq(videoID, shotID uint, req LipSyncRequest) (*LipSyncResult, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, fmt.Errorf("lipsync: get shot %d: %w", shotID, err)
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("lipsync: shot %d does not belong to video %d", shotID, videoID)
	}

	// 解析租户 ID
	var tenantID uint
	if video, vErr := s.videoRepo.GetByID(videoID); vErr == nil {
		tenantID = s.videoTenantID(video)
	}

	// 获取 lip sync provider
	provider, err := s.resolveLipSyncProvider(tenantID)
	if err != nil {
		return nil, fmt.Errorf("lipsync: %w", err)
	}

	// 确定图片 URL
	imageURL := req.ImageURL
	if imageURL == "" {
		imageURL = shot.ImageURL
	}
	if imageURL == "" {
		return nil, fmt.Errorf("lipsync: shot %d has no image; generate image first", shotID)
	}
	imageURL = s.resolveAbsURL(imageURL)

	// 确定音频 URL
	audioURL := req.AudioURL
	if audioURL == "" {
		audioURL = s.resolveShotAudioURL(shot)
	}
	if audioURL == "" {
		return nil, fmt.Errorf("lipsync: shot %d has no audio; generate voice first", shotID)
	}
	audioURL = s.resolveAbsURL(audioURL)

	logger.Printf("GenerateLipSyncVideo: shot %d image=%s audio=%s", shotID, imageURL, audioURL)

	// 提交任务
	task, err := provider.GenerateLipSync(context.Background(), &ai.LipSyncRequest{
		ImageURL: imageURL,
		AudioURL: audioURL,
		Model:    req.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("lipsync: submit task: %w", err)
	}

	// 更新分镜状态 — 记录 task ID，标记为 processing
	meta := shot.TaskMeta
	meta.ShotTaskID = lipSyncTaskPrefix + task.TaskID
	meta.ShotProviderName = provider.GetName()
	meta.ErrorMessage = ""
	if err := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
		"status":         "processing",
		"shot_task_meta": meta,
	}); err != nil {
		logger.Errorf("GenerateLipSyncVideo: update shot %d meta: %v", shotID, err)
	}

	return &LipSyncResult{
		TaskID: task.TaskID,
		Status: "processing",
	}, nil
}

// PollLipSyncUntilDone 轮询口型对齐任务直到完成，将结果 VideoURL 写入分镜。
// 通常在后台 goroutine 中调用。
func (s *VideoService) PollLipSyncUntilDone(videoID, shotID uint) error {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return fmt.Errorf("lipsync poll: get shot: %w", err)
	}

	rawTaskID := shot.TaskMeta.ShotTaskID
	if !isLipSyncTask(rawTaskID) {
		return fmt.Errorf("lipsync poll: shot %d has no pending lipsync task (taskID=%q)", shotID, rawTaskID)
	}
	taskID := rawTaskID[len(lipSyncTaskPrefix):]

	var tenantID uint
	if video, vErr := s.videoRepo.GetByID(videoID); vErr == nil {
		tenantID = s.videoTenantID(video)
	}

	provider, err := s.resolveLipSyncProvider(tenantID)
	if err != nil {
		return fmt.Errorf("lipsync poll: %w", err)
	}

	deadline := time.Now().Add(lipSyncPollTimeout)
	ticker := time.NewTicker(lipSyncPollInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		if time.Now().After(deadline) {
			s.markShotFailed(shot.ID, "lipsync: polling timeout")
			return fmt.Errorf("lipsync poll: timeout after %v", lipSyncPollTimeout)
		}

		ts, err := provider.GetLipSyncStatus(context.Background(), taskID)
		if err != nil {
			logger.Errorf("PollLipSyncUntilDone: shot %d status error: %v", shotID, err)
			continue
		}

		switch ts.Status {
		case "completed":
			videoURL, err := provider.GetLipSyncURL(context.Background(), taskID)
			if err != nil {
				s.markShotFailed(shot.ID, "lipsync: get URL: "+err.Error())
				return fmt.Errorf("lipsync poll: get URL: %w", err)
			}
			if err := s.storyboardRepo.UpdateFields(shot.ID, map[string]interface{}{
				"video_url": videoURL,
				"status":    "done",
			}); err != nil {
				logger.Errorf("PollLipSyncUntilDone: update shot %d video_url: %v", shotID, err)
			}
			logger.Printf("PollLipSyncUntilDone: shot %d done, video_url=%s", shotID, videoURL)
			return nil

		case "failed":
			msg := ts.Error
			if msg == "" {
				msg = "unknown error"
			}
			s.markShotFailed(shot.ID, "lipsync: "+msg)
			return fmt.Errorf("lipsync poll: task failed: %s", msg)

		default:
			// pending / processing — 继续等待
			logger.Printf("PollLipSyncUntilDone: shot %d status=%s", shotID, ts.Status)
		}
	}
}

// GetLipSyncStatus 查询指定分镜的口型对齐任务状态（用于前端轮询）。
func (s *VideoService) GetLipSyncStatus(videoID, shotID uint) (*LipSyncResult, error) {
	shot, err := s.storyboardRepo.GetByID(shotID)
	if err != nil {
		return nil, fmt.Errorf("lipsync status: get shot: %w", err)
	}
	if shot.VideoID != videoID {
		return nil, fmt.Errorf("lipsync status: shot %d not in video %d", shotID, videoID)
	}

	if shot.VideoURL != "" && shot.Status == "done" {
		return &LipSyncResult{Status: "completed", VideoURL: shot.VideoURL}, nil
	}
	if !isLipSyncTask(shot.TaskMeta.ShotTaskID) {
		return &LipSyncResult{Status: shot.Status}, nil
	}

	taskID := shot.TaskMeta.ShotTaskID[len(lipSyncTaskPrefix):]
	var tenantID uint
	if video, vErr := s.videoRepo.GetByID(videoID); vErr == nil {
		tenantID = s.videoTenantID(video)
	}

	provider, err := s.resolveLipSyncProvider(tenantID)
	if err != nil {
		return nil, err
	}

	ts, err := provider.GetLipSyncStatus(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	return &LipSyncResult{
		TaskID: taskID,
		Status: ts.Status,
	}, nil
}

// ---- helpers ----

func (s *VideoService) resolveLipSyncProvider(tenantID uint) (ai.LipSyncProvider, error) {
	if s.aiService != nil {
		if p, err := s.aiService.GetTenantLipSyncProvider(tenantID); err == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no lip sync provider configured (only kling-lipsync supported)")
}

// resolveShotAudioURL 返回分镜的可用音频 URL（段落优先，其次历史字段）。
func (s *VideoService) resolveShotAudioURL(shot *model.StoryboardShot) string {
	if s.segmentRepo != nil {
		segs, err := s.segmentRepo.ListByShotID(shot.ID)
		if err == nil {
			for _, seg := range segs {
				if seg.AudioPath != "" {
					return seg.AudioPath
				}
			}
		}
	}
	return shot.TaskMeta.AudioPath
}

func (s *VideoService) markShotFailed(shotID uint, msg string) {
	_ = s.storyboardRepo.UpdateFields(shotID, map[string]interface{}{
		"status":        "failed",
		"error_message": msg,
	})
}

func isLipSyncTask(taskID string) bool {
	return len(taskID) > len(lipSyncTaskPrefix) &&
		taskID[:len(lipSyncTaskPrefix)] == lipSyncTaskPrefix
}
