package ai

import "context"

// VideoProvider 视频生成提供者接口
type VideoProvider interface {
	// GenerateVideo 提交视频生成任务（图生视频）
	GenerateVideo(ctx context.Context, req *VideoGenerateRequest) (*VideoTask, error)

	// GetVideoStatus 查询任务状态
	GetVideoStatus(ctx context.Context, taskID string) (*VideoTaskStatus, error)

	// GetVideoURL 获取视频下载地址（任务完成后调用）
	GetVideoURL(ctx context.Context, taskID string) (string, error)

	// GetName 返回提供者名称
	GetName() string
}

// VideoGenerateRequest 视频生成请求
type VideoGenerateRequest struct {
	ImageURL       string  `json:"image_url"`        // 参考图（图生视频）
	Prompt         string  `json:"prompt"`           // 视频描述 Prompt
	NegativePrompt string  `json:"negative_prompt"`  // 负向 Prompt
	Duration       float64 `json:"duration"`         // 时长（秒），如 5.0
	AspectRatio    string  `json:"aspect_ratio"`     // 16:9, 9:16, 1:1
	CameraMovement string  `json:"camera_movement"`  // pan_left, zoom_in, zoom_out, static 等
	Model          string  `json:"model,omitempty"`  // 可选指定模型
	CFGScale       float64 `json:"cfg_scale"`        // 提示词引导强度 (0.0-1.0)，默认 0.5
	Mode           string  `json:"mode,omitempty"`   // kling: std/pro
}

// VideoTask 已提交的视频任务
type VideoTask struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"` // pending, processing, completed, failed
	Provider string `json:"provider"`
}

// VideoTaskStatus 视频任务状态
type VideoTaskStatus struct {
	TaskID   string  `json:"task_id"`
	Status   string  `json:"status"`   // pending, processing, completed, failed
	Progress float64 `json:"progress"` // 0-100
	Error    string  `json:"error,omitempty"`
}
