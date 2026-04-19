package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ============================================
// Intelligent Storyboard Generator - 智能分镜生成器
// ============================================

type IntelligentStoryboardService struct {
	aiService   *AIService
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
	ShotStatic  ShotType = "static"  // 静态镜头
	ShotPan     ShotType = "pan"     // 平移
	ShotZoom    ShotType = "zoom"    // 缩放
	ShotTrack   ShotType = "tracking" // 跟拍
	ShotDolly  ShotType = "dolly"   // 推拉
	ShotCrane  ShotType = "crane"    // 升降
)

// ShotSize 镜头尺寸
type ShotSize string

const (
	SizeExtremeWide ShotSize = "extreme_wide" // 大远景
	SizeWide       ShotSize = "wide"        // 远景
	SizeFull      ShotSize = "full"        // 全景
	SizeMedium    ShotSize = "medium"      // 中景
	SizeCloseUp   ShotSize = "close_up"    // 近景
	SizeExtreme   ShotSize = "extreme_close_up" // 特写
)

// ShotAngle 镜头角度
type ShotAngle string

const (
	AngleEyeLevel  ShotAngle = "eye_level"  // 平视
	AngleHigh      ShotAngle = "high"      // 俯视
	AngleLow       ShotAngle = "low"       // 仰视
	AngleDutch     ShotAngle = "dutch"     // 倾斜
	AngleOverhead  ShotAngle = "overhead"  // 顶摄
	AnglePOV      ShotAngle = "POV"       // 主观视角
)

// StoryboardShot 智能分镜
type StoryboardShot struct {
	ShotNo        int       `json:"shot_no"`
	Description   string    `json:"description"`
	Emotion       string    `json:"emotion"`      // 情感标签
	Beat          string    `json:"beat"`         // 节奏点
	ShotType      ShotType  `json:"shot_type"`
	ShotSize      ShotSize  `json:"shot_size"`
	ShotAngle     ShotAngle `json:"shot_angle"`
	Duration      float64   `json:"duration"`     // 秒
	Characters    []string  `json:"characters"`
	Location      string    `json:"location"`
	TimeOfDay     string    `json:"time_of_day"`
	Weather       string    `json:"weather"`
	Lighting      string    `json:"lighting"`
	Dialogue      string    `json:"dialogue,omitempty"`
	Action        string    `json:"action,omitempty"`
	CameraMovement string   `json:"camera_movement,omitempty"`
	Transition    string    `json:"transition"`    // 转场方式
	VisualNotes   string    `json:"visual_notes"`   // 视觉备注
}

// EmotionBeat 情感节奏分析结果
type EmotionBeat struct {
	Position     int     `json:"position"`     // 在章节中的位置(0-1)
	Emotion      string  `json:"emotion"`      // 主导情感
	Intensity    float64 `json:"intensity"`    // 情感强度(0-1)
	RhythmChange string  `json:"rhythm_change"` // 节奏变化
}

// EmotionalAnalysis 情感分析结果
type EmotionalAnalysis struct {
	OverallEmotion string       `json:"overall_emotion"` // 整体情感
	EmotionCurve  []EmotionBeat `json:"emotion_curve"` // 情感曲线
	PeakMoments   []int        `json:"peak_moments"`   // 高潮点位置
	CalmMoments   []int        `json:"calm_moments"`   // 平静点位置
}

// AnalyzeEmotions 分析章节情感
func (s *IntelligentStoryboardService) AnalyzeEmotions(content string) (*EmotionalAnalysis, error) {
	prompt := fmt.Sprintf(`请分析以下小说章节的情感节奏，返回JSON格式：

分析要求：
1. 识别章节中的情感变化
2. 标记情感高潮和低谷点
3. 评估整体情感基调

章节内容（摘要）：
%s

请返回JSON格式：
{
  "overall_emotion": "整体情感基调（如：紧张、温馨、悬疑）",
  "emotion_curve": [
    {
      "position": 0.0-1.0之间的位置,
      "emotion": "此时的主导情感",
      "intensity": 0-1的情感强度,
      "rhythm_change": "此时节奏是加快/减慢/保持"
    }
  ],
  "peak_moments": [高潮点位置列表],
  "calm_moments": [平静点位置列表]
}`, content[:min(len(content), 3000)])

	result, err := s.aiService.Generate(0, "emotion_analysis", prompt)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(result), &analysis); err != nil {
		// 返回默认分析
		return &EmotionalAnalysis{
			OverallEmotion: "neutral",
			EmotionCurve:  []EmotionBeat{},
			PeakMoments:   []int{},
			CalmMoments:   []int{},
		}, nil
	}

	return &analysis, nil
}

// DetectActionBeats 检测动作节奏点
func (s *IntelligentStoryboardService) DetectActionBeats(content string) ([]struct {
	Position  int    `json:"position"`
	Type     string `json:"type"` // action/dialogue/description
	Intensity float64 `json:"intensity"`
}, error) {
	// 简化实现
	return []struct {
		Position   int     `json:"position"`
		Type      string  `json:"type"`
		Intensity float64 `json:"intensity"`
	}{
		{Position: 0, Type: "description", Intensity: 0.3},
		{Position: 25, Type: "action", Intensity: 0.7},
		{Position: 50, Type: "dialogue", Intensity: 0.5},
		{Position: 75, Type: "action", Intensity: 0.9},
		{Position: 100, Type: "description", Intensity: 0.2},
	}, nil
}

// GenerateIntelligentShots 智能生成分镜
func (s *IntelligentStoryboardService) GenerateIntelligentShots(
	content string,
	characters []string,
	scene string,
) ([]*StoryboardShot, error) {
	// 1. 情感分析
	emotionAnalysis, err := s.AiService.AnalyzeEmotions(content)
	if err != nil {
		return nil, err
	}

	// 2. 动作节奏检测
	beats, err := s.DetectActionBeats(content)
	if err != nil {
		return nil, err
	}

	// 3. 提取对话
	dialogues := s.extractDialogues(content)

	// 4. 生成镜头序列
	shots := s.optimizeShotSequence(emotionAnalysis, beats, dialogues, characters, scene)

	return shots, nil
}

// optimizeShotSequence 优化镜头序列
func (s *IntelligentStoryboardService) optimizeShotSequence(
	emotions *EmotionalAnalysis,
	beats []struct{ Position int; Type string; Intensity float64 },
	dialogues []string,
	characters []string,
	scene string,
) []*StoryboardShot {
	shots := make([]*StoryboardShot, 0)

	// 根据情感曲线确定镜头数量
	numShots := 5 + len(emotions.PeakMoments)*2

	for i := 0; i < numShots; i++ {
		position := float64(i) / float64(numShots)
		shot := &StoryboardShot{
			ShotNo: i + 1,
		}

		// 查找对应的情感点
		var currentEmotion string = "neutral"
		var intensity float64 = 0.5
		for _, eb := range emotions.EmotionCurve {
			if math.Abs(eb.Position-position) < 0.15 {
				currentEmotion = eb.Emotion
				intensity = eb.Intensity
				break
			}
		}
		shot.Emotion = currentEmotion

		// 根据情感和强度选择镜头参数
		shot.ShotType, shot.ShotSize, shot.Duration = s.selectShotParams(intensity, currentEmotion)

		// 根据位置和内容确定其他参数
		if i == 0 {
			shot.ShotSize = SizeWide // 开场通常是远景
			shot.Description = fmt.Sprintf("场景全景：%s", scene)
		} else if i == numShots-1 {
			shot.Description = fmt.Sprintf("场景收尾：%s", scene)
			shot.Transition = "fade_out"
		}

		// 添加对话
		if len(dialogues) > i {
			shot.Dialogue = dialogues[i]
		}

		// 添加转场
		if i > 0 {
			shot.Transition = s.selectTransition(shots[i-1].Emotion, currentEmotion)
		}

		shots = append(shots, shot)
	}

	return shots
}

// selectShotParams 根据情感选择镜头参数
func (s *IntelligentStoryboardService) selectShotParams(intensity float64, emotion string) (ShotType, ShotSize, float64) {
	// 情感高潮 → 特写/快速切换
	if intensity > 0.7 {
		if emotion == "紧张" || emotion == "恐惧" {
			return ShotZoom, SizeExtreme, 2.0
		}
		return ShotStatic, SizeCloseUp, 3.0
	}

	// 情感低谷 → 远景/缓慢平移
	if intensity < 0.3 {
		return ShotPan, SizeWide, 6.0
	}

	// 中等情感 → 中景/标准节奏
	return ShotStatic, SizeMedium, 4.0
}

// selectTransition 选择转场
func (s *IntelligentStoryboardService) selectTransition(fromEmotion, toEmotion string) string {
	// 紧张→平静：渐慢
	if (fromEmotion == "紧张" || fromEmotion == "恐惧") && toEmotion == "平静" {
		return "fade"
	}

	// 平静→紧张：硬切
	if fromEmotion == "平静" && (toEmotion == "紧张" || toEmotion == "震惊") {
		return "hard_cut"
	}

	// 默认
	return "dissolve"
}

// extractDialogues 提取对话
func (s *IntelligentStoryboardService) extractDialogues(content string) []string {
	// 简化实现：使用引号提取
	dialogues := make([]string, 0)

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "「") || strings.HasPrefix(line, "\"") {
			// 移除引号
			if len(line) > 2 {
				dialogues = append(dialogues, line)
			}
		}
	}

	return dialogues
}

// ============================================
// Character Consistency Service - 角色一致性控制
// ============================================

type CharacterConsistencyService struct {
	imageService *ImageService
	loraService *LoRAService
}

func NewCharacterConsistencyService(imageService *ImageService, loraService *LoRAService) *CharacterConsistencyService {
	return &CharacterConsistencyService{
		imageService: imageService,
		loraService:  loraService,
	}
}

// ConsistencyLevel 一致性控制层级
type ConsistencyLevel struct {
	// L0: 基础视觉一致性
	Lora *LoRAConfig `json:"lora,omitempty"`

	// L1: 特征层一致性
	IPAdapter *IPAdapterConfig `json:"ip_adapter,omitempty"`

	// L2: 内容层一致性
	ControlNet *ControlNetConfig `json:"control_net,omitempty"`

	// L3: 人工层
	HumanReview *HumanReviewConfig `json:"human_review,omitempty"`
}

// LoRAConfig LoRA配置
type LoRAConfig struct {
	ModelID          string  `json:"model_id"`
	Weight           float64 `json:"weight"`            // 0.6-0.9
	InjectionMethod string  `json:"injection_method"` // Attention/LoRA/LyCORIS
}

// IPAdapterConfig IP-Adapter配置
type IPAdapterConfig struct {
	Weight         float64 `json:"weight"`          // 0.5-0.8
	StyleTemplate string `json:"style_template"` // IP-Adapter/IP-Adapter Plus
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
	RequireApproval     bool    `json:"require_approval"`
}

// GetDefaultConsistencyLevel 获取默认一致性配置
func (s *CharacterConsistencyService) GetDefaultConsistencyLevel() *ConsistencyLevel {
	return &ConsistencyLevel{
		Lora: &LoRAConfig{
			Weight:          0.8,
			InjectionMethod: "LoRA",
		},
		IPAdapter: &IPAdapterConfig{
			Weight:         0.7,
			StyleTemplate: "IP-Adapter",
		},
		ControlNet: &ControlNetConfig{
			Pose:  true,
			Face:  true,
			Depth: false,
		},
		HumanReview: &HumanReviewConfig{
			AutoApproveThreshold: 0.9,
			RequireApproval:     false,
		},
	}
}

// ConsistencyScore 一致性评分
type ConsistencyScore struct {
	OverallScore    float64 `json:"overall_score"`
	VisualScore     float64 `json:"visual_score"`    // 视觉一致性
	FeatureScore    float64 `json:"feature_score"`   // 特征一致性
	ExpressionScore float64 `json:"expression_score"` // 表情一致性
}

// CalculateConsistencyScore 计算一致性评分
func (s *CharacterConsistencyService) CalculateConsistencyScore(
	referenceImage string,
	generatedImages []string,
) (*ConsistencyScore, error) {
	// 简化实现：返回模拟评分
	scores := &ConsistencyScore{
		OverallScore:     0.85,
		VisualScore:      0.88,
		FeatureScore:     0.82,
		ExpressionScore: 0.85,
	}

	return scores, nil
}

// ============================================
// Image Generation Service - 图像生成服务
// ============================================

type ImageService struct {
	sdClient *StableDiffusionClient
	provider AIProvider
}

func NewImageService(provider AIProvider) *ImageService {
	return &ImageService{
		provider: provider,
	}
}

// ImageGenerationRequest 图像生成请求
type ImageGenerationRequest struct {
	Prompt           string                 `json:"prompt"`
	NegativePrompt   string                 `json:"negative_prompt,omitempty"`
	Size             string                 `json:"size"`        // 512x512, 1024x1024
	Steps            int                    `json:"steps"`
	CFGScale         float64               `json:"cfg_scale"`
	Seed            int64                 `json:"seed"`
	Style            string                 `json:"style"`       // realistic, anime, cartoon
	ReferenceImage   string                 `json:"reference_image,omitempty"`
	ConsistencyLevel *ConsistencyLevel     `json:"consistency_level,omitempty"`
	ControlNet       *ControlNetRequest     `json:"control_net,omitempty"`
}

// ControlNetRequest ControlNet请求
type ControlNetRequest struct {
	Type    string `json:"type"`    // canny, depth, pose, etc.
	Image   string `json:"image"`   // 图像URL或base64
	Weight  float64 `json:"weight"`
}

// GenerateCharacterImage 生成角色图像
func (s *ImageService) GenerateCharacterImage(
	charName string,
	expression string,
	pose string,
	config *ConsistencyLevel,
) (string, error) {
	// 构建提示词
	prompt := s.buildCharacterPrompt(charName, expression, pose)

	req := &ImageGenerationRequest{
		Prompt:         prompt,
		NegativePrompt: "blurry, low quality, bad anatomy, distorted face",
		Size:          "1024x1024",
		Steps:         30,
		CFGScale:      7.5,
		Style:         "realistic",
	}

	// 应用一致性控制
	if config != nil {
		req.ConsistencyLevel = config
	}

	// 调用图像生成API
	result, err := s.provider.ImageGenerate(context.Background(), &ImageGenerateRequest{
		Model:   "stable-diffusion-xl",
		Prompt: req.Prompt,
	})

	if err != nil {
		return "", err
	}

	return result.URL, nil
}

// buildCharacterPrompt 构建角色提示词
func (s *ImageService) buildCharacterPrompt(charName, expression, pose string) string {

	sb.WriteString(fmt.Sprintf("portrait of %s", charName))
	sb.WriteString(fmt.Sprintf(", expression: %s", expression))
	sb.WriteString(fmt.Sprintf(", pose: %s", pose))
	sb.WriteString(", high detail, professional photography, studio lighting")

	return sb.String()
}

// GenerateSceneImage 生成场景图像
func (s *ImageService) GenerateSceneImage(
	location string,
	timeOfDay string,
	weather string,
	lighting string,
	characters []string,
) (string, error) {

	sb.WriteString(fmt.Sprintf("%s", location))

	if timeOfDay != "" {
		sb.WriteString(fmt.Sprintf(", %s", timeOfDay))
	}

	if weather != "" {
		sb.WriteString(fmt.Sprintf(", %s weather", weather))
	}

	if lighting != "" {
		sb.WriteString(fmt.Sprintf(", %s lighting", lighting))
	}

	if len(characters) > 0 {
		sb.WriteString(fmt.Sprintf(", with %s in the scene", strings.Join(characters, ", ")))
	}

	prompt := sb.String()

	result, err := s.provider.ImageGenerate(context.Background(), &ImageGenerateRequest{
		Model:   "stable-diffusion-xl",
		Prompt: prompt,
	})

	if err != nil {
		return "", err
	}

	return result.URL, nil
}

// ============================================
// LoRA Service - LoRA训练和管理
// ============================================

type LoRAService struct {
	modelRepo interface{}
}

func NewLoRAService(modelRepo interface{}) *LoRAService {
	return &LoRAService{modelRepo: modelRepo}
}

// LoRAModel LoRA模型
type LoRAModel struct {
	ID          string  `json:"id"`
	CharacterID uint    `json:"character_id"`
	Name        string  `json:"name"`
	ModelPath   string  `json:"model_path"`
	Weight      float64 `json:"weight"`
	Quality     float64 `json:"quality"`
	Status      string  `json:"status"` // training/ready/failed
	CreatedAt   string  `json:"created_at"`
}

// TrainCharacterLoRA 训练角色LoRA
func (s *LoRAService) TrainCharacterLoRA(
	characterID uint,
	characterName string,
	trainingImages []string,
) (*LoRAModel, error) {
	// 简化实现：创建LoRA模型记录
	model := &LoRAModel{
		ID:          fmt.Sprintf("lora_%d_%d", characterID, time.Now().Unix()),
		CharacterID: characterID,
		Name:        fmt.Sprintf("%s_LoRA", characterName),
		Weight:      0.8,
		Quality:     0.0, // 训练完成后更新
		Status:      "training",
		CreatedAt:   time.Now().Format("2006-01-02 15:04:05"),
	}

	return model, nil
}

// GetCharacterLoRA 获取角色LoRA
func (s *LoRAService) GetCharacterLoRA(characterID uint) (*LoRAModel, error) {
	// 简化实现
	return &LoRAModel{
		ID:          fmt.Sprintf("lora_%d", characterID),
		CharacterID: characterID,
		Name:        "default_lora",
		Weight:      0.8,
		Quality:     0.85,
		Status:      "ready",
	}, nil
}

// AIProvider AI提供者接口
type AIProvider interface {
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, error)
	ImageGenerate(ctx context.Context, req *ImageGenerateRequest) (*ImageResponse, error)
}

// ============================================
// Video Enhancement Service - 视频增强服务
// ============================================

type VideoEnhancementService struct {
	imageService *ImageService
}

func NewVideoEnhancementService(imageService *ImageService) *VideoEnhancementService {
	return &VideoEnhancementService{imageService: imageService}
}

// EnhancementType 增强类型
type EnhancementType string

const (
	FrameInterpolation EnhancementType = "frame_interpolation" // 帧插值
	SuperResolution   EnhancementType = "super_resolution"   // 超分辨率
	VideoStabilize   EnhancementType = "video_stabilize"    // 视频稳定
	ColorGrading     EnhancementType = "color_grading"      // 色彩增强
	StyleTransfer    EnhancementType = "style_transfer"     // 风格迁移
)

// EnhancementConfig 增强配置
type EnhancementConfig struct {
	Type EnhancementType `json:"type"`

	// 帧插值配置
	TargetFPS int `json:"target_fps,omitempty"` // 目标帧率

	// 超分辨率配置
	ScaleFactor float64 `json:"scale_factor,omitempty"` // 放大倍数 2x/4x

	// 色彩增强配置
	ColorGradePreset string `json:"color_grade_preset,omitempty"` // cinematic/vibrant/muted

	// 风格迁移配置
	StylePreset string `json:"style_preset,omitempty"` // anime/oil_painting/watercolor
}

// EnhancementJob 增强任务
type EnhancementJob struct {
	ID          string             `json:"id"`
	VideoID     uint              `json:"video_id"`
	Type       EnhancementType    `json:"type"`
	Config     *EnhancementConfig `json:"config"`
	Status     string            `json:"status"` // pending/processing/completed/failed
	Progress   float64           `json:"progress"`
	ResultURL  string            `json:"result_url,omitempty"`
	Error      string            `json:"error,omitempty"`
	CreatedAt string            `json:"created_at"`
}

// EnhanceVideo 增强视频
func (s *VideoEnhancementService) EnhanceVideo(
	videoURL string,
	configs []EnhancementConfig,
) ([]*EnhancementJob, error) {
	jobs := make([]*EnhancementJob, 0, len(configs))

	for _, config := range configs {
		job := &EnhancementJob{
			ID:        fmt.Sprintf("enhance_%d_%s", time.Now().UnixNano(), config.Type),
			Type:      config.Type,
			Config:    &config,
			Status:    "pending",
			Progress:  0,
			CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		}

		// 模拟处理
		go s.processEnhancement(job, videoURL)

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// processEnhancement 处理增强任务
func (s *VideoEnhancementService) processEnhancement(job *EnhancementJob, videoURL string) {
	job.Status = "processing"

	// 模拟处理过程
	for i := 0; i <= 100; i += 10 {
		job.Progress = float64(i)
		time.Sleep(500 * time.Millisecond)
	}

	job.Status = "completed"
	job.ResultURL = fmt.Sprintf("https://example.com/enhanced/%s.mp4", job.ID)
}

// RecommendEnhancements 推荐增强方案
func (s *VideoEnhancementService) RecommendEnhancements(videoInfo *struct {
	FPS        int     `json:"fps"`
	Resolution string  `json:"resolution"`
	Duration   float64 `json:"duration"`
	Style      string  `json:"style"`
}) []*EnhancementConfig {
	configs := make([]*EnhancementConfig, 0)

	// 帧率优化
	if videoInfo.FPS < 30 {
		configs = append(configs, &EnhancementConfig{
			Type:      FrameInterpolation,
			TargetFPS: 60,
		})
	}

	// 分辨率优化
	if videoInfo.Resolution == "720p" || videoInfo.Resolution == "1080p" {
		configs = append(configs, &EnhancementConfig{
			Type:        SuperResolution,
			ScaleFactor: 2.0,
		})
	}

	// 色彩优化
	configs = append(configs, &EnhancementConfig{
		Type:             ColorGrading,
		ColorGradePreset: "cinematic",
	})

	return configs
}

// ============================================
// Helper Functions
// ============================================
}
