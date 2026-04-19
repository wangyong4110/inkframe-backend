package service

import (
	\\"fmt\\"
	\\"strings\\"
	\\"time\\"

	\\"github.com/inkframe/inkframe-backend/internal/model\\"
)

// ============================================
// 视频生成服务 - Video Generation Service
// ============================================

// VideoGenerationRequest 视频生成请求
type VideoGenerationRequest struct {
	NovelID        uint     `json:\\"novel_id\\"`
	ChapterID      uint     `json:\\"chapter_id\\"`
	Title          string   `json:\\"title\\"`
	Resolution     string   `json:\\"resolution\\"`      // 720p, 1080p, 4k
	FrameRate      int      `json:\\"frame_rate\\"`      // 24, 30, 60
	AspectRatio    string   `json:\\"aspect_ratio\\"`    // 16:9, 9:16, 1:1
	ArtStyle       string   `json:\\"art_style\\"`       // realistic, anime, cartoon
	ColorGrade     string   `json:\\"color_grade\\"`    // cinematic, vintage, vibrant
}

// VideoGenerationResult 视频生成结果
type VideoGenerationResult struct {
	VideoID       uint   `json:\\"video_id\\"`
	Status        string `json:\\"status\\"`        // planning, storyboard, generating, rendering, completed
	Progress      float64 `json:\\"progress\\"`   // 0-100
	TotalShots    int    `json:\\"total_shots\\"`
	GeneratedShots int    `json:\\"generated_shots\\"`
	ErrorMessage  string `json:\\"error_message,omitempty\\"`
}

// ============================================
// 1. 智能分镜生成器
// ============================================

// ShotType 镜头类型
type ShotType string

const (
	ShotWide       ShotType = \\"wide\\"        // 远景
	ShotMedium     ShotType = \\"medium\\"      // 中景
	ShotCloseUp    ShotType = \\"close_up\\"   // 近景
	ShotExtreme    ShotType = \\"extreme\\"     // 特写
	ShotPOV        ShotType = \\"pov\\"         // 主观镜头
)

// ShotAngle 镜头角度
type ShotAngle string

const (
	AngleEyeLevel  ShotAngle = \\"eye_level\\"   // 平视
	AngleLow       ShotAngle = \\"low\\"         // 仰视
	AngleHigh      ShotAngle = \\"high\\"       // 俯视
	AngleDutch     ShotAngle = \\"dutch\\"       // 荷兰角
)

// CameraMovement 摄像机运动
type CameraMovement string

const (
	CamStatic  CameraMovement = \\"static\\"   // 静止
	CamPan     CameraMovement = \\"pan\\"      // 摇镜
	CamTilt    CameraMovement = \\"tilt\\"     // 俯仰
	CamZoom    CameraMovement = \\"zoom\\"     // 变焦
	CamDolly   CameraMovement = \\"dolly\\"    // 推拉
	CamTrack   CameraMovement = \\"track\\"    // 跟踪
)

// StoryboardShot 分镜
type StoryboardShot struct {
	ID             uint              `json:\\"id\\"`
	ShotNo         int               `json:\\"shot_no\\"`
	Description    string            `json:\\"description\\"`
	Dialogue       string            `json:\\"dialogue,omitempty\\"`
	ShotType       ShotType          `json:\\"shot_type\\"`
	ShotAngle      ShotAngle         `json:\\"shot_angle\\"`
	CameraMovement CameraMovement    `json:\\"camera_movement\\"`
	Duration       float64           `json:\\"duration\\"`       // 秒
	Characters     []ShotCharacter    `json:\\"characters\\"`
	Scene          string            `json:\\"scene\\"`
	Lighting       string            `json:\\"lighting\\"`
	Emotion        string            `json:\\"emotion\\"`
	Prompt         string            `json:\\"prompt\\"`         // 图像生成提示词
	NegativePrompt string            `json:\\"negative_prompt\\"`
	Status         string            `json:\\"status\\"`         // pending, generating, completed, failed
}

// ShotCharacter 分镜中的角色
type ShotCharacter struct {
	CharacterID uint   `json:\\"character_id\\"`
	Name        string `json:\\"name\\"`
	Expression  string `json:\\"expression\\"` // happy, sad, angry, etc.
	Pose        string `json:\\"pose\\"`       // standing, sitting, fighting, etc.
	Position     string `json:\\"position\\"`   // left, center, right
}

// IntelligentStoryboardService 智能分镜服务
type IntelligentStoryboardService struct {
	aiService *AIService
}

// NewIntelligentStoryboardService 创建智能分镜服务
func NewIntelligentStoryboardService(aiService *AIService) *IntelligentStoryboardService {
	return &IntelligentStoryboardService{
		aiService: aiService,
	}
}

// GenerateStoryboard 生成分镜
func (s *IntelligentStoryboardService) GenerateStoryboard(chapter *model.Chapter, characters []*model.Character, config *VideoGenerationRequest) ([]*StoryboardShot, error) {
	shots := []*StoryboardShot{}

	// 1. 分析章节内容，提取场景
	scenes := s.analyzeChapterScenes(chapter.Content)

	// 2. 分析情感曲线
	emotions := s.analyzeEmotions(chapter.Content)

	// 3. 为每个场景生成镜头
	currentShot := 1
	for _, scene := range scenes {
		// 确定镜头数量
		shotCount := s.determineShotCount(scene, emotions)

		for i := 0; i < shotCount; i++ {
			shot := s.generateShot(scene, i, shotCount, currentShot, characters, config)
			shots = append(shots, shot)
			currentShot++
		}
	}

	return shots, nil
}

// SceneAnalysis 场景分析
type SceneAnalysis struct {
	Type         string   `json:\\"type\\"`          // dialogue, action, description, transition
	Description  string   `json:\\"description\\"`
	Dialogue     string   `json:\\"dialogue,omitempty\\"`
	Characters   []string `json:\\"characters\\"`
	Location     string   `json:\\"location\\"`
	TimeOfDay    string   `json:\\"time_of_day\\"`
	Emotion      string   `json:\\"emotion\\"`
	Intensity    float64  `json:\\"intensity\\"`     // 0-1
	Pacing       string   `json:\\"pacing\\"`         // fast, medium, slow
}

// 分析章节场景
func (s *IntelligentStoryboardService) analyzeChapterScenes(content string) []*SceneAnalysis {
	scenes := []*SceneAnalysis{}

	// 简化实现:按段落分割
	paragraphs := strings.Split(content, \\"\n\n\\")

	currentScene := &SceneAnalysis{
		Type:        \\"description\\",
		Intensity:  0.5,
		Pacing:     \\"medium\\",
	}

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if len(para) == 0 {
			continue
		}

		// 检测对话
		if strings.Contains(para, \\"\\"\\") {
			dialogues := s.extractDialogues(para)
			for _, d := range dialogues {
				if len(d) > 10 {
					currentScene.Type = \\"dialogue\\"
					currentScene.Dialogue = d
					currentScene.Intensity = 0.6
				}
			}
		}

		// 检测动作
		actionMarkers := []string{\\"打\\", \\"跑\\", \\"跳\\", \\"走\\", \\"飞\\", \\"攻击\\", \\"战斗\\"}
		for _, marker := range actionMarkers {
			if strings.Contains(para, marker) {
				currentScene.Type = \\"action\\"
				currentScene.Intensity = 0.9
				currentScene.Pacing = \\"fast\\"
				break
			}
		}

		// 检测场景描述
		if currentScene.Type == \\"description\\" && len(para) > 50 {
			currentScene.Description = para
		}

		// 每3-5个段落作为一个场景
		if len(scenes) > 0 && len(scenes[len(scenes)-1].Description) > 0 {
			scenes = append(scenes, currentScene)
			currentScene = &SceneAnalysis{
				Type:        \\"description\\",
				Intensity:  0.5,
				Pacing:     \\"medium\\",
			}
		}
	}

	// 添加最后一个场景
	if len(currentScene.Description) > 0 || currentScene.Dialogue != \\"\\" {
		scenes = append(scenes, currentScene)
	}

	return scenes
}

// 分析情感
func (s *IntelligentStoryboardService) analyzeEmotions(content string) []string {
	emotions := []string{}

	// 简化情感分析
	emotionMarkers := map[string][]string{
		\\"紧张\\": {\\"紧张\\", \\"心跳\\", \\"害怕\\", \\"恐惧\\", \\"担忧\\"},
		\\"愤怒\\": {\\"愤怒\\", \\"生气\\", \\"怒火\\", \\"气愤\\"},
		\\"悲伤\\": {\\"悲伤\\", \\"难过\\", \\"伤心\\", \\"痛苦\\", \\"哭泣\\"},
		\\"快乐\\": {\\"高兴\\", \\"开心\\", \\"快乐\\", \\"喜悦\\", \\"欢笑\\"},
		\\"平静\\": {\\"平静\\", \\"宁静\\", \\"安静\\", \\"祥和\\"},
	}

	paragraphs := strings.Split(content, \\"\n\n\\")
	for _, para := range paragraphs {
		found := false
		for emotion, markers := range emotionMarkers {
			for _, marker := range markers {
				if strings.Contains(para, marker) {
					emotions = append(emotions, emotion)
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			emotions = append(emotions, \\"neutral\\")
		}
	}

	return emotions
}

// 确定镜头数量
func (s *IntelligentStoryboardService) determineShotCount(scene *SceneAnalysis, emotions []string) int {
	baseCount := 1

	// 根据场景类型调整
	switch scene.Type {
	case \\"action\\":
		baseCount = 3 // 动作场景需要更多镜头
	case \\"dialogue\\":
		baseCount = 2 // 对话场景需要切换镜头
	case \\"description\\":
		baseCount = 1 // 描述场景可以少一些镜头
	}

	// 根据强度调整
	if scene.Intensity > 0.7 {
		baseCount++
	}

	// 根据节奏调整
	if scene.Pacing == \\"fast\\" {
		baseCount++
	}

	return baseCount
}

// 生成单个镜头
func (s *IntelligentStoryboardService) generateShot(
	scene *SceneAnalysis,
	index int,
	total int,
	shotNo int,
	characters []*model.Character,
	config *VideoGenerationRequest,
) *StoryboardShot {
	shot := &StoryboardShot{
		ID:          uint(shotNo),
		ShotNo:      shotNo,
		Description: scene.Description,
		Dialogue:    scene.Dialogue,
		Emotion:     scene.Emotion,
		Status:      \\"pending\\",
	}

	// 确定镜头类型
	shot.ShotType = s.selectShotType(scene, index, total)

	// 确定镜头角度
	shot.ShotAngle = s.selectShotAngle(scene, index)

	// 确定摄像机运动
	shot.CameraMovement = s.selectCameraMovement(scene)

	// 确定时长
	shot.Duration = s.estimateDuration(scene)

	// 确定角色
	if len(characters) > 0 {
		for i, char := range characters {
			charShot := ShotCharacter{
				CharacterID: char.ID,
				Name:        char.Name,
				Expression:  s.determineExpression(scene.Emotion),
				Pose:        s.determinePose(scene),
				Position:    s.determinePosition(i, len(characters)),
			}
			shot.Characters = append(shot.Characters, charShot)
		}
	}

	// 确定场景和灯光
	shot.Scene = scene.Location
	shot.Lighting = s.determineLighting(scene)

	// 生成提示词
	shot.Prompt = s.buildPrompt(shot, config)
	shot.NegativePrompt = \\"ugly, deformed, extra limbs, blurry, bad anatomy, low quality\\"

	return shot
}

// 选择镜头类型
func (s *IntelligentStoryboardService) selectShotType(scene *SceneAnalysis, index, total int) ShotType {
	// 第一个镜头通常用远景建立场景
	if index == 0 && total > 1 {
		return ShotWide
	}

	// 对话场景
	if scene.Type == \\"dialogue\\" {
		if index%2 == 0 {
			return ShotMedium
		}
		return ShotCloseUp
	}

	// 动作场景
	if scene.Type == \\"action\\" {
		if scene.Intensity > 0.8 {
			return ShotExtreme
		}
		return ShotMedium
	}

	// 描述场景
	return ShotMedium
}

// 选择镜头角度
func (s *IntelligentStoryboardService) selectShotAngle(scene *SceneAnalysis, index int) ShotAngle {
	// 紧张场景可以用荷兰角
	if scene.Emotion == \\"紧张\\" || scene.Emotion == \\"愤怒\\" {
		if scene.Intensity > 0.7 {
			return AngleDutch
		}
	}

	// 平静场景用平视
	if scene.Emotion == \\"平静\\" || scene.Emotion == \\"快乐\\" {
		return AngleEyeLevel
	}

	// 根据镜头位置调整
	switch index % 3 {
	case 0:
		return AngleEyeLevel
	case 1:
		return AngleLow
	default:
		return AngleHigh
	}
}

// 选择摄像机运动
func (s *IntelligentStoryboardService) selectCameraMovement(scene *SceneAnalysis) CameraMovement {
	if scene.Type == \\"action\\" && scene.Intensity > 0.7 {
		return CamTrack
	}

	if scene.Type == \\"dialogue\\" {
		return CamStatic // 对话场景通常保持稳定
	}

	if scene.Intensity > 0.6 {
		return CamPan
	}

	return CamStatic
}

// 估算时长
func (s *IntelligentStoryboardService) estimateDuration(scene *SceneAnalysis) float64 {
	baseDuration := 4.0

	// 根据场景类型调整
	switch scene.Type {
	case \\"action\\":
		baseDuration = 3.0
	case \\"dialogue\\":
		baseDuration = 5.0
	}

	// 根据强度调整
	if scene.Intensity > 0.7 {
		baseDuration -= 1.0
	} else if scene.Intensity < 0.4 {
		baseDuration += 1.0
	}

	// 根据节奏调整
	if scene.Pacing == \\"fast\\" {
		baseDuration -= 0.5
	} else if scene.Pacing == \\"slow\\" {
		baseDuration += 1.0
	}

	// 根据对话长度调整
	if scene.Dialogue != \\"\\" {
		// 假设每秒可以说10个字
		baseDuration = float64(len(scene.Dialogue)) / 10.0
		if baseDuration < 3.0 {
			baseDuration = 3.0
		}
		if baseDuration > 10.0 {
			baseDuration = 10.0
		}
	}

	return baseDuration
}

// 确定表情
func (s *IntelligentStoryboardService) determineExpression(emotion string) string {
	emotionMap := map[string]string{
		\\"紧张\\":  \\"worried\\",
		\\"愤怒\\":  \\"angry\\",
		\\"悲伤\\":  \\"sad\\",
		\\"快乐\\":  \\"happy\\",
		\\"平静\\":  \\"calm\\",
		\\"neutral\\": \\"neutral\\",
	}

	if expr, ok := emotionMap[emotion]; ok {
		return expr
	}
	return \\"neutral\\"
}

// 确定姿态
func (s *IntelligentStoryboardService) determinePose(scene *SceneAnalysis) string {
	switch scene.Type {
	case \\"action\\":
		return \\"standing\\"
	case \\"dialogue\\":
		return \\"standing\\"
	default:
		return \\"standing\\"
	}
}

// 确定位置
func (s *IntelligentStoryboardService) determinePosition(index, total int) string {
	if total == 1 {
		return \\"center\\"
	}
	if total == 2 {
		if index == 0 {
			return \\"left\\"
		}
		return \\"right\\"
	}
	positions := []string{\\"left\\", \\"center\\", \\"right\\"}
	return positions[index%3]
}

// 确定灯光
func (s *IntelligentStoryboardService) determineLighting(scene *SceneAnalysis) string {
	switch scene.Emotion {
	case \\"紧张\\", \\"恐惧\\":
		return \\"dramatic\\"
	case \\"快乐\\", \\"平静\\":
		return \\"soft\\"
	case \\"愤怒\\":
		return \\"high_contrast\\"
	default:
		return \\"natural\\"
	}
}

// 构建提示词
func (s *IntelligentStoryboardService) buildPrompt(shot *StoryboardShot, config *VideoGenerationRequest) string {
	prompt := \\"\\"

	// 添加场景描述
	prompt += shot.Description + \\", \\"

	// 添加镜头信息
	prompt += fmt.Sprintf(\\"%s shot, %s angle, \\", shot.ShotType, shot.ShotAngle)

	// 添加摄像机运动
	if shot.CameraMovement != CamStatic {
		prompt += fmt.Sprintf(\\"camera %s, \\", shot.CameraMovement)
	}

	// 添加角色
	for _, char := range shot.Characters {
		prompt += fmt.Sprintf(\\"%s with %s expression, %s pose, \\", char.Name, char.Expression, char.Pose)
	}

	// 添加灯光
	prompt += fmt.Sprintf(\\"%s lighting, \\", shot.Lighting)

	// 添加场景
	if shot.Scene != \\"\\" {
		prompt += fmt.Sprintf(\\"in %s, \\", shot.Scene)
	}

	// 添加情感
	prompt += fmt.Sprintf(\\"%s atmosphere, \\", shot.Emotion)

	// 添加风格和质量
	switch config.ArtStyle {
	case \\"anime\\":
		prompt += \\"anime style, vibrant colors, detailed\\"
	case \\"cartoon\\":
		prompt += \\"cartoon style, playful\\"
	default:
		prompt += \\"cinematic, highly detailed, photorealistic\\"
	}

	// 添加分辨率和质量
	switch config.Resolution {
	case \\"4k\\":
		prompt += \\", 4k, ultra detailed\\"
	case \\"1080p\\":
		prompt += \\", 1080p, high quality\\"
	default:
		prompt += \\", 720p\\"
	}

	return prompt
}

// ============================================
// 2. 视频增强服务
// ============================================

// EnhancementType 增强类型
type EnhancementType string

const (
	EnhanceFrameInterpolation EnhancementType = \\"frame_interpolation\\" // 帧插值
	EnhanceSuperResolution    EnhancementType = \\"super_resolution\\"    // 超分辨率
	EnhanceColorCorrection    EnhancementType = \\"color_correction\\"    // 色彩校正
	EnhanceStabilization      EnhancementType = \\"stabilization\\"      // 稳定化
	EnhanceDenoising          EnhancementType = \\"denoising\\"          // 降噪
	EnhanceStyleTransfer      EnhancementType = \\"style_transfer\\"     // 风格迁移
)

// EnhancementConfig 增强配置
type EnhancementConfig struct {
	Type          EnhancementType `json:\\"type\\"`
	Enabled       bool           `json:\\"enabled\\"`
	Intensity     float64        `json:\\"intensity\\"` // 0-1
	TargetFPS     int            `json:\\"target_fps,omitempty\\"`
	ScaleFactor   float64        `json:\\"scale_factor,omitempty\\"`
	StylePreset   string         `json:\\"style_preset,omitempty\\"`
}

// VideoEnhancementService 视频增强服务
type VideoEnhancementService struct {
	aiService *AIService
}

// NewVideoEnhancementService 创建视频增强服务
func NewVideoEnhancementService(aiService *AIService) *VideoEnhancementService {
	return &VideoEnhancementService{
		aiService: aiService,
	}
}

// GetDefaultEnhancements 获取默认增强配置
func (s *VideoEnhancementService) GetDefaultEnhancements() []*EnhancementConfig {
	return []*EnhancementConfig{
		{
			Type:      EnhanceFrameInterpolation,
			Enabled:   true,
			Intensity: 0.7,
			TargetFPS: 60,
		},
		{
			Type:      EnhanceSuperResolution,
			Enabled:   true,
			Intensity: 0.8,
			ScaleFactor: 1.5,
		},
		{
			Type:      EnhanceColorCorrection,
			Enabled:   true,
			Intensity: 0.6,
		},
		{
			Type:      EnhanceStabilization,
			Enabled:   true,
			Intensity: 0.5,
		},
	}
}

// GetRecommendedEnhancements 获取推荐增强配置
func (s *VideoEnhancementService) GetRecommendedEnhancements(videoInfo *model.Video) []*EnhancementConfig {
	configs := s.GetDefaultEnhancements()

	// 根据分辨率调整
	switch videoInfo.Resolution {
	case \\"720p\\":
		for _, cfg := range configs {
			if cfg.Type == EnhanceSuperResolution {
				cfg.ScaleFactor = 2.0
			}
		}
	case \\"4k\\":
		for _, cfg := range configs {
			if cfg.Type == EnhanceSuperResolution {
				cfg.Enabled = false
			}
		}
	}

	// 根据帧率调整
	if videoInfo.FrameRate < 30 {
		for _, cfg := range configs {
			if cfg.Type == EnhanceFrameInterpolation {
				cfg.TargetFPS = 60
			}
		}
	}

	return configs
}

// ApplyEnhancement 应用增强
func (s *VideoEnhancementService) ApplyEnhancement(videoID uint, enhancement *EnhancementConfig) error {
	// 获取视频信息
	video := &model.Video{} // TODO: 从数据库获取

	// 根据增强类型应用不同的处理
	switch enhancement.Type {
	case EnhanceFrameInterpolation:
		return s.applyFrameInterpolation(video, enhancement)
	case EnhanceSuperResolution:
		return s.applySuperResolution(video, enhancement)
	case EnhanceColorCorrection:
		return s.applyColorCorrection(video, enhancement)
	case EnhanceStabilization:
		return s.applyStabilization(video, enhancement)
	}

	return nil
}

// 应用帧插值
func (s *VideoEnhancementService) applyFrameInterpolation(video *model.Video, config *EnhancementConfig) error {
	// TODO: 实现帧插值逻辑
	// 可以使用 AI 模型如 FILM、SoftSplat 等
	return nil
}

// 应用超分辨率
func (s *VideoEnhancementService) applySuperResolution(video *model.Video, config *EnhancementConfig) error {
	// TODO: 实现超分辨率逻辑
	// 可以使用 Real-ESRGAN、Waifu2x 等模型
	return nil
}

// 应用色彩校正
func (s *VideoEnhancementService) applyColorCorrection(video *model.Video, config *EnhancementConfig) error {
	// TODO: 实现色彩校正逻辑
	return nil
}

// 应用稳定化
func (s *VideoEnhancementService) applyStabilization(video *model.Video, config *EnhancementConfig) error {
	// TODO: 实现稳定化逻辑
	// 可以使用 vid.stab 或 AI 稳定化模型
	return nil
}

// ============================================
// 3. 帧生成服务
// ============================================

// FrameGenerationRequest 帧生成请求
type FrameGenerationRequest struct {
	Shot          *StoryboardShot      `json:\\"shot\\"`
	Characters    []*CharacterVisual   `json:\\"characters\\"`
	SceneVisual   *SceneVisual        `json:\\"scene_visual\\"`
	ConsistencyConfig *ConsistencyConfig `json:\\"consistency_config\\"`
}

// CharacterVisual 角色视觉
type CharacterVisual struct {
	CharacterID    uint    `json:\\"character_id\\"`
	Name           string  `json:\\"name\\"`
	BaseImageURL   string  `json:\\"base_image_url\\"`
	LoraModelID    string  `json:\\"lora_model_id,omitempty\\"`
	LoraWeight     float64 `json:\\"lora_weight\\"`
	ExpressionImages map[string]string `json:\\"expression_images\\"`
}

// SceneVisual 场景视觉
type SceneVisual struct {
	SceneID        uint    `json:\\"scene_id\\"`
	Name           string  `json:\\"name\\"`
	BaseImageURL   string  `json:\\"base_image_url\\"`
	LoraModelID    string  `json:\\"lora_model_id,omitempty\\"`
	LoraWeight     float64 `json:\\"lora_weight\\"`
}

// ConsistencyConfig 一致性配置
type ConsistencyConfig struct {
	UseLora        bool    `json:\\"use_lora\\"`
	UseIPAdapter   bool    `json:\\"use_ip_adapter\\"`
	UseControlNet  bool    `json:\\"use_control_net\\"`
	ReferenceWeight float64 `json:\\"reference_weight\\"` // 0-1
	LoraWeight      float64 `json:\\"lora_weight\\"`
}

// FrameGeneratorService 帧生成服务
type FrameGeneratorService struct {
	aiService *AIService
}

// NewFrameGeneratorService 创建帧生成服务
func NewFrameGeneratorService(aiService *AIService) *FrameGeneratorService {
	return &FrameGeneratorService{
		aiService: aiService,
	}
}

// GenerateFrame 生成单帧
func (s *FrameGeneratorService) GenerateFrame(req *FrameGenerationRequest) (*GeneratedFrame, error) {
	frame := &GeneratedFrame{}

	// 1. 构建提示词
	prompt := s.buildFramePrompt(req)

	// 2. 设置生成选项
	options := &ImageGenerationOptions{
		Prompt:          prompt,
		NegativePrompt:   req.Shot.NegativePrompt,
		Size:            \\"1920x1080\\",
		Steps:           50,
		CFGScale:        7.5,
	}

	// 3. 应用一致性控制
	if req.ConsistencyConfig != nil {
		// 使用 LoRA
		if req.ConsistencyConfig.UseLora && len(req.Characters) > 0 {
			for _, char := range req.Characters {
				if char.LoraModelID != \\"\\" {
					options.LoraModels = append(options.LoraModels, LoraModel{
						ID:     char.LoraModelID,
						Weight: req.ConsistencyConfig.LoraWeight * char.LoraWeight,
					})
				}
			}
		}

		// 使用 IP-Adapter
		if req.ConsistencyConfig.UseIPAdapter && len(req.Characters) > 0 {
			options.ReferenceImages = []string{}
			for _, char := range req.Characters {
				if char.BaseImageURL != \\"\\" {
					options.ReferenceImages = append(options.ReferenceImages, char.BaseImageURL)
				}
			}
			options.ReferenceWeight = req.ConsistencyConfig.ReferenceWeight
		}
	}

	// 4. 生成图像
	image, err := s.aiService.GenerateImage(prompt, options)
	if err != nil {
		return nil, err
	}

	frame.ImageURL = image.URL
	frame.Prompt = prompt

	return frame, nil
}

// 构建帧提示词
func (s *FrameGeneratorService) buildFramePrompt(req *FrameGenerationRequest) string {
	prompt := req.Shot.Prompt

	// 添加摄像机运动信息
	if req.Shot.CameraMovement != CamStatic {
		prompt += fmt.Sprintf(\\", camera %s movement\\", req.Shot.CameraMovement)
	}

	// 添加角色详细信息
	for _, char := range req.Characters {
		prompt += fmt.Sprintf(\\", %s with consistent appearance\\", char.Name)
	}

	return prompt
}

// GeneratedFrame 生成的帧
type GeneratedFrame struct {
	FrameNo     int    `json:\\"frame_no\\"`
	ImageURL    string `json:\\"image_url\\"`
	Prompt      string `json:\\"prompt\\"`
	GeneratedAt time.Time `json:\\"generated_at\\"`
}

// ============================================
// 4. 图像生成选项
// ============================================

// ImageGenerationOptions 图像生成选项
type ImageGenerationOptions struct {
	Prompt          string      `json:\\"prompt\\"`
	NegativePrompt  string      `json:\\"negative_prompt,omitempty\\"`
	Size            string      `json:\\"size,omitempty\\"`      // 512x512, 1024x1024, etc.
	Steps           int         `json:\\"steps,omitempty\\"`
	CFGScale        float64     `json:\\"cfg_scale,omitempty\\"`
	LoraModels      []LoraModel `json:\\"lora_models,omitempty\\"`
	ReferenceImages []string    `json:\\"reference_images,omitempty\\"`
	ReferenceWeight float64     `json:\\"reference_weight,omitempty\\"`
}

// LoraModel LoRA模型
type LoraModel struct {
	ID     string  `json:\\"id\\"`
	Weight float64 `json:\\"weight\\"`
}

// GeneratedImage 生成的图像
type GeneratedImage struct {
	URL     string  `json:\\"url\\"`
	Width   int     `json:\\"width\\"`
	Height  int     `json:\\"height\\"`
	Seed    int64   `json:\\"seed,omitempty\\"`
}

// ============================================
// 5. 一致性验证服务
// ============================================

// ConsistencyValidationResult 一致性验证结果
type ConsistencyValidationResult struct {
	OverallScore     float64              `json:\\"overall_score\\"`
	CharacterScores  map[uint]float64     `json:\\"character_scores\\"`  // character_id -> score
	SceneScore       float64              `json:\\"scene_score\\"`
	Issues           []ConsistencyIssue   `json:\\"issues\\"`
}

// ConsistencyIssue 一致性问题
type ConsistencyIssue struct {
	Type        string  `json:\\"type\\"`         // appearance_drift, missing_element, style_drift
	Severity    string  `json:\\"severity\\"`     // high, medium, low
	Description string  `json:\\"description\\"`
	Location    string  `json:\\"location\\"`
	Suggestion  string  `json:\\"suggestion\\"`
}

// ConsistencyValidatorService 一致性验证服务
type ConsistencyValidatorService struct {
	aiService *AIService
}

// NewConsistencyValidatorService 创建一致性验证服务
func NewConsistencyValidatorService(aiService *AIService) *ConsistencyValidatorService {
	return &ConsistencyValidatorService{
		aiService: aiService,
	}
}

// ValidateConsistency 验证一致性
func (s *ConsistencyValidatorService) ValidateConsistency(
	frameURL string,
	characters []*CharacterVisual,
	scene *SceneVisual,
) *ConsistencyValidationResult {
	result := &ConsistencyValidationResult{
		CharacterScores: make(map[uint]float64),
		Issues:          []ConsistencyIssue{},
	}

	// 1. 验证角色一致性
	for _, char := range characters {
		score := s.validateCharacterConsistency(frameURL, char)
		result.CharacterScores[char.CharacterID] = score

		if score < 0.8 {
			result.Issues = append(result.Issues, ConsistencyIssue{
				Type:        \\"appearance_drift\\",
				Severity:    \\"medium\\",
				Description: fmt.Sprintf(\\"角色 %s 的外观一致性不足 (%.2f)\\", char.Name, score),
				Suggestion:  \\"使用更高的 LoRA 权重或参考图像权重\\",
			})
		}
	}

	// 2. 验证场景一致性
	if scene != nil {
		result.SceneScore = s.validateSceneConsistency(frameURL, scene)

		if result.SceneScore < 0.7 {
			result.Issues = append(result.Issues, ConsistencyIssue{
				Type:        \\"style_drift\\",
				Severity:    \\"medium\\",
				Description: fmt.Sprintf(\\"场景一致性不足 (%.2f)\\", result.SceneScore),
				Suggestion:  \\"使用场景 LoRA 或调整提示词\\",
			})
		}
	}

	// 3. 计算总体得分
	total := 0.0
	count := len(result.CharacterScores)
	if scene != nil {
		total += result.SceneScore
		count++
	}
	for _, score := range result.CharacterScores {
		total += score
	}
	result.OverallScore = total / float64(count)

	return result
}

// 验证角色一致性
func (s *ConsistencyValidatorService) validateCharacterConsistency(frameURL string, char *CharacterVisual) float64 {
	// TODO: 使用 Vision AI 分析角色外观一致性
	// 返回 0-1 的相似度分数
	return 0.85 // 简化实现
}

// 验证场景一致性
func (s *ConsistencyValidatorService) validateSceneConsistency(frameURL string, scene *SceneVisual) float64 {
	// TODO: 使用 Vision AI 分析场景一致性
	return 0.85 // 简化实现
}
