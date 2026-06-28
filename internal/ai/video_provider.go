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
	ImageURL       string   `json:"image_url"`        // 主参考图（首帧）；Kling 等单图提供商使用
	ImageURLs      []string `json:"image_urls"`       // 多参考图；ImageURL 若非空会自动插入首位
	VideoURLs      []string `json:"video_urls"`       // 参考视频 URL 列表（Seedance 2.0 多模态）
	AudioURLs      []string `json:"audio_urls"`       // 参考音频 URL 列表（Seedance 2.0 多模态）
	Prompt         string   `json:"prompt"`           // 视频描述 Prompt
	NegativePrompt string   `json:"negative_prompt"`  // 负向 Prompt
	Duration       float64  `json:"duration"`         // 时长（秒），如 5.0；-1 表示由模型自动选择（Seedance 2.0/1.5）
	Frames         int      `json:"frames,omitempty"` // 帧数（Seedance 1.0 系列，与 duration 二选一）
	AspectRatio    string   `json:"aspect_ratio"`     // 16:9, 4:3, 1:1, 3:4, 9:16, 21:9, adaptive
	Resolution     string   `json:"resolution"`       // 480p, 720p, 1080p, 4k（Doubao Seedance 系列）
	CameraMovement string   `json:"camera_movement"`  // pan_left, zoom_in, zoom_out, static 等（Kling）
	Model          string   `json:"model,omitempty"`  // 可选指定模型 / Endpoint ID
	CFGScale       float64  `json:"cfg_scale"`        // 提示词引导强度 (0.0-1.0)，默认 0.5
	Mode           string   `json:"mode,omitempty"`   // kling: std/pro
	GenerateAudio  *bool    `json:"generate_audio"`   // Seedance 2.0/1.5：true=有声视频，false=无声；nil=使用默认值(true)
	Watermark      bool     `json:"watermark"`        // 是否添加水印，默认 false
	Seed           int      `json:"seed"`             // 随机种子，-1 或 0 表示随机（Seedance 1.x）

	// Seedance 扩展参数
	ReturnLastFrame       bool   `json:"return_last_frame"`        // 是否在响应中返回最后一帧 URL（用于视频续接）
	Draft                 bool   `json:"draft"`                    // 草稿/预览模式（仅 Seedance 1.5 Pro，快速低质预览）
	DraftTaskID           string `json:"draft_task_id,omitempty"`  // 续接草稿任务 ID（二步式：草稿→正式）
	ServiceTier           string `json:"service_tier,omitempty"`   // "default"（在线）/"flex"（离线，价格减半，小时级延迟）
	ExecutionExpiresAfter int64  `json:"execution_expires_after"`  // flex 离线推理超时（秒）；0 表示不限
	CallbackURL           string `json:"callback_url,omitempty"`   // Webhook 回调地址；任务完成/失败时推送
}

// VideoTask 已提交的视频任务
type VideoTask struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"` // pending, processing, completed, failed
	Provider string `json:"provider"`
}

// VideoTaskStatus 视频任务状态
type VideoTaskStatus struct {
	TaskID       string  `json:"task_id"`
	Status       string  `json:"status"`                  // pending, processing, completed, failed
	Progress     float64 `json:"progress"`                // 0-100
	Error        string  `json:"error,omitempty"`
	LastFrameURL string  `json:"last_frame_url,omitempty"` // 最后一帧 URL（需请求时设置 ReturnLastFrame=true）
}
