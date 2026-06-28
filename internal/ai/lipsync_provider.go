package ai

import "context"

// LipSyncProvider 口型对齐视频生成接口
// 输入：角色参考图 + 音频 → 输出：口型与音频对齐的视频
type LipSyncProvider interface {
	// GenerateLipSync 提交口型对齐任务，返回异步任务
	GenerateLipSync(ctx context.Context, req *LipSyncRequest) (*LipSyncTask, error)

	// GetLipSyncStatus 查询任务状态
	GetLipSyncStatus(ctx context.Context, taskID string) (*LipSyncTaskStatus, error)

	// GetLipSyncURL 获取已完成任务的视频 URL
	GetLipSyncURL(ctx context.Context, taskID string) (string, error)

	// GetName 返回提供者名称
	GetName() string
}

// LipSyncRequest 口型对齐请求
type LipSyncRequest struct {
	ImageURL string `json:"image_url"` // 角色参考图（头像/半身像）
	AudioURL string `json:"audio_url"` // TTS 音频 URL（mp3/wav）
	Model    string `json:"model"`     // 可选模型，空则用默认
	Mode     string `json:"mode"`      // std/pro，默认 std
}

// LipSyncTask 已提交的口型对齐任务
type LipSyncTask struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"` // pending, processing, completed, failed
	Provider string `json:"provider"`
}

// LipSyncTaskStatus 任务状态
type LipSyncTaskStatus struct {
	TaskID   string  `json:"task_id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
}
