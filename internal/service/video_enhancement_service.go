package service

import (
	"context"
	"strings"

	"github.com/inkframe/inkframe-backend/internal/ai"
)

// ============================================
// Intelligent Storyboard Generator - 智能分镜生成器
// ============================================

type IntelligentStoryboardService struct {
	aiService    *AIService
	imageService *ImageService
}

func NewIntelligentStoryboardService(aiService *AIService, imageService *ImageService) *IntelligentStoryboardService {
	return &IntelligentStoryboardService{
		aiService:    aiService,
		imageService: imageService,
	}
}

// ShotType 镜头类型
type ShotType string

const (
	ShotStatic ShotType = "static"   // 静态镜头
	ShotPan    ShotType = "pan"      // 平移
	ShotZoom   ShotType = "zoom"     // 缩放
	ShotTrack  ShotType = "tracking" // 跟拍
	ShotDolly  ShotType = "dolly"    // 推拉
	ShotCrane  ShotType = "crane"    // 升降
)

// ShotSize 镜头尺寸
type ShotSize string

const (
	SizeExtremeWide ShotSize = "extreme_wide"     // 大远景
	SizeWide        ShotSize = "wide"             // 远景
	SizeFull        ShotSize = "full"             // 全景
	SizeMedium      ShotSize = "medium"           // 中景
	SizeCloseUp     ShotSize = "close_up"         // 近景
	SizeExtreme     ShotSize = "extreme_close_up" // 特写
)

// ShotAngle 镜头角度
type ShotAngle string

const (
	AngleEyeLevel ShotAngle = "eye_level" // 平视
	AngleHigh     ShotAngle = "high"      // 俯视
	AngleLow      ShotAngle = "low"       // 仰视
	AngleDutch    ShotAngle = "dutch"     // 倾斜
	AngleOverhead ShotAngle = "overhead"  // 顶摄
	AnglePOV      ShotAngle = "POV"       // 主观视角
)

// ShotCharacter 分镜角色信息
type ShotCharacter struct {
	CharacterID uint   `json:"character_id"`
	Name        string `json:"name"`
	Expression  string `json:"expression"`
	Pose        string `json:"pose"`
	Position    string `json:"position"`
}

// StoryboardShot 智能分镜
type StoryboardShot struct {
	VideoID        uint            `json:"video_id,omitempty"`
	ShotNo         int             `json:"shot_no"`
	Description    string          `json:"description"`
	Emotion        string          `json:"emotion"` // 情感标签
	Beat           string          `json:"beat"`    // 节奏点
	ShotType       ShotType        `json:"shot_type"`
	ShotSize       ShotSize        `json:"shot_size"`
	ShotAngle      ShotAngle       `json:"shot_angle"`
	Duration       float64         `json:"duration"` // 秒
	Characters     []ShotCharacter `json:"characters"`
	Location       string          `json:"location"`
	Scene          string          `json:"scene,omitempty"`
	TimeOfDay      string          `json:"time_of_day"`
	Weather        string          `json:"weather"`
	Lighting       string          `json:"lighting"`
	Dialogue       string          `json:"dialogue,omitempty"`
	Action         string          `json:"action,omitempty"`
	CameraMovement string          `json:"camera_movement,omitempty"`
	Transition     string          `json:"transition"`   // 转场方式
	VisualNotes    string          `json:"visual_notes"` // 视觉备注
	Prompt         string          `json:"prompt,omitempty"`
	NegativePrompt string          `json:"negative_prompt,omitempty"`
}

// extractDialogues 提取对话
func (s *IntelligentStoryboardService) extractDialogues(content string) []string {
	// 简化实现:使用引号提取
	dialogues := make([]string, 0)

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "\u201c") || strings.HasPrefix(line, "\"") {
			// 移除引号
			if len(line) > 2 {
				dialogues = append(dialogues, line)
			}
		}
	}

	return dialogues
}

// LoRAConfig LoRA配置
type LoRAConfig struct {
	ModelID         string  `json:"model_id"`
	Weight          float64 `json:"weight"`           // 0.6-0.9
	InjectionMethod string  `json:"injection_method"` // Attention/LoRA/LyCORIS
}

// IPAdapterConfig IP-Adapter配置
type IPAdapterConfig struct {
	Weight        float64 `json:"weight"`         // 0.5-0.8
	StyleTemplate string  `json:"style_template"` // IP-Adapter/IP-Adapter Plus
}

// ControlNetConfig ControlNet配置
type ControlNetConfig struct {
	Pose  bool `json:"pose"`  // 姿态控制
	Face  bool `json:"face"`  // 人脸控制
	Depth bool `json:"depth"` // 深度控制
}

// HumanReviewConfig 人工审核配置
type HumanReviewConfig struct {
	AutoApproveThreshold float64 `json:"auto_approve_threshold"` // 超过阈值自动通过
	RequireApproval      bool    `json:"require_approval"`
}

// ============================================
// Image Generation Service - 图像生成服务
// ============================================

type ImageService struct {
	provider AIProvider
}

func NewImageService(provider AIProvider) *ImageService {
	return &ImageService{
		provider: provider,
	}
}

// ImageGenerationRequest 图像生成请求
type ImageGenerationRequest struct {
	Prompt         string             `json:"prompt"`
	NegativePrompt string             `json:"negative_prompt,omitempty"`
	Size           string             `json:"size"` // 512x512, 1024x1024
	Steps          int                `json:"steps"`
	CFGScale       float64            `json:"cfg_scale"`
	Seed           int64              `json:"seed"`
	Style          string             `json:"style"` // realistic, anime, cartoon
	ReferenceImage string             `json:"reference_image,omitempty"`
	ControlNet     *ControlNetRequest `json:"control_net,omitempty"`
}

// ControlNetRequest ControlNet请求
type ControlNetRequest struct {
	Type   string  `json:"type"`  // canny, depth, pose, etc.
	Image  string  `json:"image"` // 图像URL或base64
	Weight float64 `json:"weight"`
}

// AIProvider AI提供者接口
type AIProvider interface {
	Generate(ctx context.Context, req *ai.GenerateRequest) (*ai.GenerateResponse, error)
	ImageGenerate(ctx context.Context, req *ai.GenerateRequest) (*ai.ImageResponse, error)
}
