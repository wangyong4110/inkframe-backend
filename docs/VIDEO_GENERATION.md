# InkFrame - 视频生成系统技术方案

## 📋 概述

本文档描述基于 InkFrame 生成的小说内容，自动生成高质量视频（图片序列视频/动画视频）的完整技术方案。核心挑战是确保**角色视觉一致性**和**场景视觉一致性**，以及视频的**连续性和流畅性**。

### 核心特性

- 🎬 **智能视频生成**：从小说文本自动生成分镜和视频
- 👤 **角色一致性**：同一角色在不同场景、表情、动作下保持视觉一致
- 🌆 **场景一致性**：同一场景在不同视角、光照下保持视觉一致
- 🎨 **风格统一**：整个视频保持一致的视觉风格
- 🔊 **自动配音**：AI 生成角色配音、背景音乐、音效
- ✏️ **交互编辑**：可视化的视频编辑器和分镜调整工具
- 🚀 **高性能**：支持批量生成和并行处理

---

## 🏗️ 系统架构

### 整体架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                   Frontend (Vue 3 + Nuxt 3)                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ 视频编辑器   │  │ 角色管理     │  │ 场景管理     │          │
│  │ - 分镜预览   │  │ - 角色设计   │  │ - 场景设计   │          │
│  │ - 时间轴     │  │ - 形象库     │  │ - 场景库     │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
                              ▲
                              │ REST API / WebSocket
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Backend (Go + Gin)                           │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │                   Video Generation Service                 │  │
│  └──────────────────────────────────────────────────────────┘  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │  Storyboard  │  │  Character   │  │  Scene       │          │
│  │  Service     │  │  Visual      │  │  Visual      │          │
│  │              │  │  Service     │  │  Service     │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │  Consistency │  │  Video       │  │  Audio       │          │
│  │  Controller  │  │  Generator   │  │  Generator   │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
         ▲               ▲               ▲               ▲
         │               │               │               │
         ▼               ▼               ▼               ▼
┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│   MySQL     │  │   Redis     │  │  MinIO      │  │   Vector DB │
│  (元数据)    │  │  (缓存)     │  │ (视频存储)  │  │  (特征存储) │
└─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘
                              │
                              ▼
                    ┌─────────────────┐
                    │  AI Providers   │
                    │ - Stable Diff.  │
                    │ - Midjourney    │
                    │ - D-ID/HeyGen   │
                    │ - ElevenLabs    │
                    │ - Suno AI       │
                    └─────────────────┘
```

---

## 🎨 角色视觉一致性系统

### 1. 角色视觉定义

#### 1.1 数据模型

```go
// 角色视觉设计
type CharacterVisualDesign struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"` // 关联角色表

    // 外观描述（文本）
    AppearanceDescription string `json:"appearance_description" gorm:"type:text"`

    // 视觉特征（结构化）
    FacialFeatures string `json:"facial_features" gorm:"type:text"` // JSON: {eye_type, nose_type, mouth_type, face_shape}
    HairStyle     string `json:"hair_style" gorm:"type:text"` // JSON: {color, length, style}
    SkinTone      string `json:"skin_tone"`
    BodyType      string `json:"body_type"`
    Age           int    `json:"age"`
    Gender        string `json:"gender"`

    // 服装与装备
    Outfit        string `json:"outfit" gorm:"type:text"` // JSON: [{type, color, style, description}]
    Accessories   string `json:"accessories" gorm:"type:text"` // JSON
    Weapons       string `json:"weapons" gorm:"type:text"` // JSON

    // 视觉风格
    ArtStyle      string `json:"art_style"` // realistic, anime, cartoon, 3d, watercolor, etc.
    ColorPalette  string `json:"color_palette" gorm:"type:text"` // JSON: [{color, usage, frequency}]

    // 视觉参考图像
    ReferenceImageURLs string `json:"reference_image_urls" gorm:"type:text"` // JSON
    GeneratedImages    string `json:"generated_images" gorm:"type:text"` // JSON: {base: [], expression: {}, action: {}, angle: {}}

    // LoRA/微调模型
    LoraModelID  string `json:"lora_model_id"` // 专门训练的角色 LoRA 模型 ID
    LoraWeight   float64 `json:"lora_weight"`  // LoRA 权重

    // Embedding（用于一致性控制）
    VisualEmbedding []float64 `json:"visual_embedding" gorm:"type:json"` // 视觉特征向量

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// 角色表情库
type CharacterExpressionLibrary struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`

    ExpressionType string `json:"expression_type"` // happy, sad, angry, surprised, neutral, etc.
    Description    string `json:"description"`

    // 视觉参考
    ReferenceImage string `json:"reference_image"` // MinIO URL
    GeneratedImage string `json:"generated_image"` // MinIO URL

    // 提示词
    Prompt string `json:"prompt" gorm:"type:text"`

    CreatedAt time.Time `json:"created_at"`
}

// 角色动作库
type CharacterPoseLibrary struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`

    PoseType      string `json:"pose_type"` // standing, sitting, fighting, walking, etc.
    Description   string `json:"description"`
    Category      string `json:"category"` // idle, action, cinematic

    // 视觉参考
    ReferenceImage string `json:"reference_image"` // MinIO URL
    GeneratedImage string `json:"generated_image"` // MinIO URL

    // 提示词
    Prompt string `json:"prompt" gorm:"type:text"`

    CreatedAt time.Time `json:"created_at"`
}

// 角色视角库
type CharacterAngleLibrary struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`

    AngleType     string `json:"angle_type"` // front, side, back, 3/4, top, bottom
    CameraAngle   string `json:"camera_angle"` // eye-level, low, high, dutch

    // 视觉参考
    ReferenceImage string `json:"reference_image"` // MinIO URL
    GeneratedImage string `json:"generated_image"` // MinIO URL

    // 提示词
    Prompt string `json:"prompt" gorm:"type:text"`

    CreatedAt time.Time `json:"created_at"`
}
```

#### 1.2 角色视觉生成流程

```go
type CharacterVisualService struct {
    db           *gorm.DB
    aiClient     AIClient
    vectorStore  VectorStore
    minio        *minio.Client
}

// 生成角色完整视觉设计
func (s *CharacterVisualService) GenerateCharacterVisual(characterID uint, novelID uint) (*CharacterVisualDesign, error) {
    // 1. 获取角色信息
    character := s.getCharacter(characterID)

    // 2. 从小说中提取角色描述
    descriptions := s.extractCharacterDescriptions(novelID, characterID)

    // 3. 生成角色基础形象
    baseImage, visualFeatures := s.generateBaseImage(descriptions, character)

    // 4. 生成多种表情
    expressions := s.generateExpressionGallery(baseImage, character)

    // 5. 生成多种动作姿态
    poses := s.generatePoseGallery(baseImage, character)

    // 6. 生成多种视角
    angles := s.generateAngleGallery(baseImage, character)

    // 7. 训练角色 LoRA 模型（可选，用于更高一致性）
    loraModelID, loraWeight := s.trainCharacterLora(baseImage, visualFeatures)

    // 8. 提取视觉 Embedding
    embedding := s.extractVisualEmbedding(baseImage)

    // 9. 保存设计
    design := &CharacterVisualDesign{
        CharacterID:          characterID,
        AppearanceDescription: descriptions,
        FacialFeatures:       visualFeatures.Facial,
        HairStyle:           visualFeatures.Hair,
        SkinTone:            visualFeatures.SkinTone,
        BodyType:            visualFeatures.BodyType,
        Age:                 character.Age,
        Gender:              character.Gender,
        ArtStyle:            "realistic", // 可配置
        ReferenceImageURLs:  []string{baseImage.URL},
        GeneratedImages:     s.compileGallery(expressions, poses, angles),
        LoraModelID:         loraModelID,
        LoraWeight:          loraWeight,
        VisualEmbedding:     embedding,
    }

    s.db.Create(design)

    return design, nil
}

// 生成角色基础形象
func (s *CharacterVisualService) generateBaseImage(descriptions []string, character *Character) (*Image, *VisualFeatures) {
    // 1. 构建详细提示词
    prompt := s.buildCharacterPrompt(descriptions, character)

    // 2. 使用高质量模型生成
    image := s.aiClient.GenerateImage(prompt, AIClientOptions{
        Model:       "stable-diffusion-xl",
        Size:        "1024x1024",
        Steps:       50,
        CFGScale:    7.5,
        NegativePrompt: "ugly, deformed, extra limbs, blurry, bad anatomy",
    })

    // 3. 使用 Vision AI 分析视觉特征
    features := s.aiClient.AnalyzeVisualFeatures(image)

    return image, features
}

// 构建角色提示词
func (s *CharacterVisualService) buildCharacterPrompt(descriptions []string, character *Character) string {
    prompt := fmt.Sprintf("Professional character portrait, %s, ", character.Name)

    // 添加详细描述
    prompt += strings.Join(descriptions, ", ")

    // 添加风格修饰
    prompt += ", highly detailed, 8k, cinematic lighting, sharp focus, realistic style"

    return prompt
}

// 生成表情库
func (s *CharacterVisualService) generateExpressionGallery(baseImage *Image, character *Character) []CharacterExpressionLibrary {
    expressions := []string{
        "neutral expression",
        "happy smiling",
        "sad crying",
        "angry fierce",
        "surprised shocked",
        "fearful worried",
        "determined focused",
        "confused puzzled",
        "excited enthusiastic",
        "calm peaceful",
    }

    library := []CharacterExpressionLibrary{}

    for _, expr := range expressions {
        // 使用 IP-Adapter 或 ControlNet 保持一致性
        prompt := fmt.Sprintf("%s, %s expression, keeping same character appearance and features", baseImage.Prompt, expr)

        image := s.aiClient.GenerateImage(prompt, AIClientOptions{
            Model:            "stable-diffusion-xl",
            Size:             "1024x1024",
            ReferenceImage:   baseImage.URL,
            ReferenceWeight:  0.8,
        })

        library = append(library, CharacterExpressionLibrary{
            CharacterID:    character.ID,
            ExpressionType: expr,
            Description:    expr,
            ReferenceImage: baseImage.URL,
            GeneratedImage: image.URL,
            Prompt:         prompt,
        })
    }

    return library
}

// 训练角色 LoRA 模型
func (s *CharacterVisualService) trainCharacterLora(baseImage *Image, features *VisualFeatures) (string, float64) {
    // 使用 DreamBooth 或类似技术训练角色特定的 LoRA
    // 这是一个异步任务

    task := &LoraTrainingTask{
        CharacterID:     baseImage.CharacterID,
        BaseImage:       baseImage.URL,
        TrainingImages:  s.collectTrainingImages(baseImage.CharacterID),
        TrainingSteps:   1000,
    }

    s.startLoraTraining(task)

    return task.ModelID, 0.7 // 返回模型 ID 和建议权重
}
```

---

## 🌆 场景视觉一致性系统

### 2.1 场景视觉定义

```go
// 场景视觉设计
type SceneVisualDesign struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    SceneID     uint   `json:"scene_id"` // 关联场景表

    // 场景描述（文本）
    SceneDescription string `json:"scene_description" gorm:"type:text"`

    // 场景元素
    Environment    string `json:"environment" gorm:"type:text"` // JSON: {location, time_of_day, weather}
    Architecture   string `json:"architecture" gorm:"type:text"` // JSON
    Props          string `json:"props" gorm:"type:text"` // JSON: [{name, position, description}]
    Lighting       string `json:"lighting" gorm:"type:text"` // JSON: {type, intensity, color, direction}
    Atmosphere     string `json:"atmosphere"` // mysterious, peaceful, tense, etc.

    // 视觉风格
    ArtStyle       string `json:"art_style"`
    ColorPalette   string `json:"color_palette" gorm:"type:text"` // JSON
    Mood           string `json:"mood"` // horror, romance, adventure, etc.

    // 视觉参考图像
    ReferenceImageURLs string `json:"reference_image_urls" gorm:"type:text"` // JSON
    GeneratedImages    string `json:"generated_images" gorm:"type:text"` // JSON: {base: [], wide: [], detail: {}}

    // LoRA/微调模型
    LoraModelID  string `json:"lora_model_id"` // 场景 LoRA 模型
    LoraWeight   float64 `json:"lora_weight"`

    // Embedding
    VisualEmbedding []float64 `json:"visual_embedding" gorm:"type:json"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// 场景视角库
type SceneAngleLibrary struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    SceneID     uint   `json:"scene_id"`

    AngleType   string `json:"angle_type"` // wide, medium, close-up, extreme_close_up
    Description string `json:"description"`

    // 摄像机参数
    CameraDistance string `json:"camera_distance"` // 远景、中景、近景
    CameraAngle   string `json:"camera_angle"` // 俯视、平视、仰视
    DepthOfField  string `json:"depth_of_field"` // 景深

    // 视觉参考
    ReferenceImage string `json:"reference_image"`
    GeneratedImage string `json:"generated_image"`

    // 提示词
    Prompt string `json:"prompt" gorm:"type:text"`

    CreatedAt time.Time `json:"created_at"`
}

// 场景时间变化库（用于表现时间流逝）
type SceneTimeVariation struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    SceneID     uint   `json:"scene_id"`

    TimeOfDay   string `json:"time_of_day"` // dawn, morning, noon, afternoon, dusk, night
    Description string `json:"description"`

    // 视觉参考
    ReferenceImage string `json:"reference_image"`
    GeneratedImage string `json:"generated_image"`

    // 提示词
    Prompt string `json:"prompt" gorm:"type:text"`

    CreatedAt time.Time `json:"created_at"`
}
```

#### 2.2 场景视觉生成流程

```go
type SceneVisualService struct {
    db          *gorm.DB
    aiClient    AIClient
    vectorStore VectorStore
    minio       *minio.Client
}

// 生成场景视觉设计
func (s *SceneVisualService) GenerateSceneVisual(sceneID uint, novelID uint) (*SceneVisualDesign, error) {
    // 1. 获取场景信息
    scene := s.getScene(sceneID)

    // 2. 从小说中提取场景描述
    descriptions := s.extractSceneDescriptions(novelID, sceneID)

    // 3. 分析环境元素
    environment := s.analyzeEnvironment(descriptions)

    // 4. 生成场景基础图像
    baseImage := s.generateBaseSceneImage(descriptions, environment)

    // 5. 生成多视角图像
    angles := s.generateSceneAngles(baseImage, scene)

    // 6. 生成时间变化图像
    timeVariations := s.generateTimeVariations(baseImage, scene)

    // 7. 提取视觉 Embedding
    embedding := s.extractVisualEmbedding(baseImage)

    // 8. 保存设计
    design := &SceneVisualDesign{
        SceneID:          sceneID,
        SceneDescription: descriptions,
        Environment:      environment,
        ArtStyle:         "realistic",
        Mood:             s.determineMood(descriptions),
        ReferenceImageURLs: []string{baseImage.URL},
        GeneratedImages:  s.compileSceneGallery(angles, timeVariations),
        VisualEmbedding:  embedding,
    }

    s.db.Create(design)

    return design, nil
}

// 生成场景基础图像
func (s *SceneVisualService) generateBaseSceneImage(descriptions []string, environment *Environment) *Image {
    prompt := fmt.Sprintf("Detailed scene, %s, ", strings.Join(descriptions, ", "))

    // 添加环境信息
    prompt += fmt.Sprintf(", %s time, %s weather, ",
        environment.TimeOfDay, environment.Weather)

    // 添加光影和氛围
    prompt += fmt.Sprintf("%s lighting, %s atmosphere, ",
        environment.Lighting, environment.Atmosphere)

    // 添加质量和风格
    prompt += "cinematic composition, highly detailed, 8k, photorealistic, dramatic lighting"

    image := s.aiClient.GenerateImage(prompt, AIClientOptions{
        Model: "stable-diffusion-xl",
        Size:  "1920x1080",
        Steps: 50,
    })

    return image
}

// 生成场景多视角
func (s *SceneVisualService) generateSceneAngles(baseImage *Image, scene *Scene) []SceneAngleLibrary {
    angles := []struct {
        Type     string
        Distance string
        Angle    string
        Prompt   string
    }{
        {
            Type:     "wide_shot",
            Distance: "远景",
            Angle:    "平视",
            Prompt:   "wide angle shot, showing full environment, establishing shot",
        },
        {
            Type:     "medium_shot",
            Distance: "中景",
            Angle:    "平视",
            Prompt:   "medium shot, balanced composition, showing main area",
        },
        {
            Type:     "close_up",
            Distance: "近景",
            Angle:    "平视",
            Prompt:   "close-up shot, focusing on important details",
        },
        {
            Type:     "low_angle",
            Distance: "中景",
            Angle:    "仰视",
            Prompt:   "low angle shot, dramatic perspective, looking up",
        },
        {
            Type:     "high_angle",
            Distance: "中景",
            Angle:    "俯视",
            Prompt:   "high angle shot, overview perspective, looking down",
        },
    }

    library := []SceneAngleLibrary{}

    for _, angle := range angles {
        prompt := fmt.Sprintf("%s, %s, %s, keeping same scene appearance and style",
            baseImage.Prompt, angle.Prompt, angle.Description)

        image := s.aiClient.GenerateImage(prompt, AIClientOptions{
            Model:           "stable-diffusion-xl",
            Size:            "1920x1080",
            ReferenceImage:  baseImage.URL,
            ReferenceWeight: 0.7,
        })

        library = append(library, SceneAngleLibrary{
            SceneID:        scene.ID,
            AngleType:      angle.Type,
            Description:    angle.Prompt,
            CameraDistance: angle.Distance,
            CameraAngle:    angle.Angle,
            ReferenceImage: baseImage.URL,
            GeneratedImage: image.URL,
            Prompt:         prompt,
        })
    }

    return library
}
```

---

## 🎬 视频生成系统

### 3.1 分镜生成

```go
// 分镜
type StoryboardShot struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    VideoID     uint   `json:"video_id"`
    ShotNo      int    `json:"shot_no"`
    ChapterID   uint   `json:"chapter_id"`

    // 文本内容
    Description string `json:"description"` // 文本描述
    Dialogue    string `json:"dialogue" gorm:"type:text"` // 对话内容

    // 视觉配置
    CameraType   string `json:"camera_type"` // static, pan, zoom, tracking, dolly
    CameraAngle  string `json:"camera_angle"` // eye-level, low, high, dutch
    ShotSize     string `json:"shot_size"` // wide, medium, close-up, extreme_close_up

    // 时长
    Duration     float64 `json:"duration"` // 秒

    // 角色与场景
    Characters   string `json:"characters" gorm:"type:text"` // JSON: [{character_id, expression, pose, position}]
    Scene        string `json:"scene" gorm:"type:text"` // JSON: {scene_id, angle, lighting}

    // AI 生成参数
    Prompt       string `json:"prompt" gorm:"type:text"` // 图像生成提示词
    NegativePrompt string `json:"negative_prompt" gorm:"type:text"`

    // 生成的帧
    Frames       string `json:"frames" gorm:"type:text"` // JSON: [{frame_no, image_url}]

    Status       string `json:"status"` // pending, generating, completed, failed
    Progress     float64 `json:"progress"`

    CreatedAt    time.Time `json:"created_at"`
}

// 视频
type Video struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    NovelID     uint   `json:"novel_id"`
    ChapterID   uint   `json:"chapter_id"`

    Title       string `json:"title"`
    Description string `json:"description" gorm:"type:text"`

    // 视频配置
    Type        string `json:"type"` // image_sequence, animation, live_action
    Resolution  string `json:"resolution"` // 1080p, 4k
    FrameRate   int    `json:"frame_rate"` // 24, 30, 60
    AspectRatio string `json:"aspect_ratio"` // 16:9, 9:16, 1:1

    // 艺术风格
    ArtStyle    string `json:"art_style"`
    ColorGrade  string `json:"color_grade"`

    // 时长
    Duration    float64 `json:"duration"` // 秒

    // 文件路径
    VideoPath   string `json:"video_path"` // MinIO URL
    Thumbnail   string `json:"thumbnail"` // MinIO URL

    // 统计
    TotalShots  int    `json:"total_shots"`
    TotalFrames int    `json:"total_frames"`

    // 生成状态
    Status      string `json:"status"` // planning, storyboard, generating, rendering, completed
    Progress    float64 `json:"progress"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}
```

#### 3.2 分镜生成服务

```go
type StoryboardService struct {
    db       *gorm.DB
    aiClient AIClient
    novelRepo NovelRepository
}

// 从小说章节生成分镜
func (s *StoryboardService) GenerateStoryboard(novelID uint, chapterNo int) (*Video, []StoryboardShot, error) {
    // 1. 获取章节内容
    chapter := s.novelRepo.GetChapter(novelID, chapterNo)

    // 2. 分析章节，提取关键场景和对话
    scenes := s.analyzeChapterScenes(chapter)

    // 3. 为每个场景生成分镜
    storyboard := []StoryboardShot{}

    for _, scene := range scenes {
        // 3.1 分析场景节奏，确定分镜数量
        shotCount := s.determineShotCount(scene)

        // 3.2 生成每个分镜
        for i := 0; i < shotCount; i++ {
            shot := s.generateShot(scene, i, chapter)
            storyboard = append(storyboard, *shot)
        }
    }

    // 4. 创建视频记录
    video := &Video{
        NovelID:     novelID,
        ChapterID:   chapter.ID,
        Title:       fmt.Sprintf("第 %d 章 - %s", chapterNo, chapter.Title),
        Description: chapter.Summary,
        Type:        "image_sequence", // 默认类型
        Resolution:  "1080p",
        FrameRate:   24,
        AspectRatio: "16:9",
        ArtStyle:    "realistic",
        Status:      "storyboard",
        TotalShots:  len(storyboard),
    }

    s.db.Create(video)

    // 5. 保存分镜
    for i := range storyboard {
        storyboard[i].VideoID = video.ID
        s.db.Create(&storyboard[i])
    }

    return video, storyboard, nil
}

// 分析章节场景
func (s *StoryboardService) analyzeChapterScenes(chapter *Chapter) []SceneAnalysis {
    // 使用 AI 分析章节，识别：
    // - 场景转换
    // - 对话段落
    // - 动作描述
    // - 情绪变化

    analysis := s.aiClient.AnalyzeChapterStructure(chapter.Content)

    return analysis.Scenes
}

// 生成单个分镜
func (s *StoryboardService) generateShot(scene *SceneAnalysis, index int, chapter *Chapter) *StoryboardShot {
    shot := &StoryboardShot{
        ShotNo:      index + 1,
        ChapterID:   chapter.ID,
        Description: scene.Description,
        Dialogue:    scene.Dialogue,
        Duration:    s.estimateShotDuration(scene, index),
    }

    // 确定摄像机角度和镜头类型
    shot.CameraAngle = s.selectCameraAngle(scene, index)
    shot.CameraType = s.selectCameraType(scene, index)
    shot.ShotSize = s.selectShotSize(scene, index)

    // 确定角色和场景
    shot.Characters = s.determineCharacters(scene)
    shot.Scene = s.determineScene(scene)

    // 生成图像生成提示词
    shot.Prompt = s.buildImagePrompt(shot)
    shot.NegativePrompt = "ugly, deformed, blurry, bad composition"

    return shot
}

// 构建图像生成提示词
func (s *StoryboardService) buildImagePrompt(shot *StoryboardShot) string {
    prompt := ""

    // 添加场景描述
    prompt += shot.Description + ", "

    // 添加摄像机信息
    prompt += fmt.Sprintf("%s shot, %s angle, ", shot.ShotSize, shot.CameraAngle)

    // 添加角色信息
    characters := s.parseCharacters(shot.Characters)
    for _, char := range characters {
        prompt += fmt.Sprintf("%s with %s expression, %s pose, ",
            char.Name, char.Expression, char.Pose)
    }

    // 添加场景信息
    scene := s.parseScene(shot.Scene)
    prompt += fmt.Sprintf("in %s, %s lighting, ", scene.Name, scene.Lighting)

    // 添加风格和质量
    prompt += "cinematic, highly detailed, photorealistic, dramatic lighting, 8k"

    return prompt
}
```

---

### 3.3 帧生成服务

```go
type FrameGeneratorService struct {
    db          *gorm.DB
    aiClient    AIClient
    minio       *minio.Client
    vectorStore VectorStore
}

// 生成分镜的所有帧
func (s *FrameGeneratorService) GenerateFrames(shot *StoryboardShot) error {
    // 1. 更新状态
    shot.Status = "generating"
    s.db.Save(shot)

    // 2. 确定帧数
    frameCount := int(shot.Duration * float64(shot.FrameRate))

    // 3. 获取角色和场景视觉设计
    characterVisuals := s.getCharacterVisuals(shot.Characters)
    sceneVisual := s.getSceneVisual(shot.Scene)

    // 4. 生成帧序列
    frames := []Frame{}

    for i := 0; i < frameCount; i++ {
        frame := s.generateFrame(shot, i, frameCount, characterVisuals, sceneVisual)
        frames = append(frames, frame)

        // 更新进度
        shot.Progress = float64(i+1) / float64(frameCount) * 100
        if i%10 == 0 {
            s.db.Save(shot)
        }
    }

    // 5. 保存帧
    shot.Frames = s.serializeFrames(frames)
    shot.Status = "completed"
    s.db.Save(shot)

    return nil
}

// 生成单个帧
func (s *FrameGeneratorService) generateFrame(
    shot *StoryboardShot,
    frameNo int,
    totalFrames int,
    characterVisuals []*CharacterVisualDesign,
    sceneVisual *SceneVisualDesign,
) *Frame {
    // 1. 计算当前帧的插值位置
    progress := float64(frameNo) / float64(totalFrames)

    // 2. 确定摄像机运动
    cameraOffset := s.calculateCameraMovement(shot, progress)

    // 3. 确定角色动作插值
    characterStates := s.interpolateCharacterActions(shot, characterVisuals, progress)

    // 4. 构建提示词
    prompt := s.buildFramePrompt(shot, cameraOffset, characterStates, sceneVisual)

    // 5. 生成图像
    image := s.generateImageWithConsistency(prompt, characterVisuals, sceneVisual)

    // 6. 上传到 MinIO
    url, _ := s.minio.UploadImage(image)

    return &Frame{
        FrameNo: frameNo,
        ImageURL: url,
        Prompt:   prompt,
    }
}

// 构建帧提示词
func (s *FrameGeneratorService) buildFramePrompt(
    shot *StoryboardShot,
    cameraOffset *CameraOffset,
    characterStates []CharacterState,
    sceneVisual *SceneVisualDesign,
) string {
    prompt := shot.Prompt

    // 添加摄像机运动
    if cameraOffset != nil {
        if cameraOffset.Pan != 0 {
            prompt += fmt.Sprintf(", camera pan %s", s.offsetDirection(cameraOffset.Pan))
        }
        if cameraOffset.Zoom != 0 {
            prompt += fmt.Sprintf(", camera zoom %s", s.offsetDirection(cameraOffset.Zoom))
        }
    }

    // 添加角色状态
    for _, state := range characterStates {
        if state.Expression != "" {
            prompt += fmt.Sprintf(", %s expression", state.Expression)
        }
        if state.Pose != "" {
            prompt += fmt.Sprintf(", %s pose", state.Pose)
        }
        if state.Position != "" {
            prompt += fmt.Sprintf(", at %s", state.Position)
        }
    }

    return prompt
}

// 使用一致性控制生成图像
func (s *FrameGeneratorService) generateImageWithConsistency(
    prompt string,
    characterVisuals []*CharacterVisualDesign,
    sceneVisual *SceneVisualDesign,
) *Image {
    options := AIClientOptions{
        Model:       "stable-diffusion-xl",
        Size:        "1920x1080",
        Steps:       50,
        CFGScale:    7.5,
    }

    // 使用 IP-Adapter 保持角色一致性
    if len(characterVisuals) > 0 {
        options.ReferenceImages = []string{}
        for _, char := range characterVisuals {
            options.ReferenceImages = append(options.ReferenceImages, char.BaseImage)
        }
        options.ReferenceWeight = 0.8
    }

    // 使用场景 LoRA 保持场景一致性
    if sceneVisual.LoraModelID != "" {
        options.LoraModels = []LoraConfig{
            {ModelID: sceneVisual.LoraModelID, Weight: sceneVisual.LoraWeight},
        }
    }

    // 使用角色 LoRA 保持角色一致性
    for _, char := range characterVisuals {
        if char.LoraModelID != "" {
            options.LoraModels = append(options.LoraModels, LoraConfig{
                ModelID: char.LoraModelID,
                Weight:  char.LoraWeight,
            })
        }
    }

    image := s.aiClient.GenerateImage(prompt, options)

    return image
}

// 插值角色动作
func (s *FrameGeneratorService) interpolateCharacterActions(
    shot *StoryboardShot,
    characterVisuals []*CharacterVisualDesign,
    progress float64,
) []CharacterState {
    states := []CharacterState{}

    characters := s.parseCharacters(shot.Characters)

    for i, char := range characters {
        visual := characterVisuals[i]

        // 确定起始和结束状态
        startState := s.determineStartState(visual, char)
        endState := s.determineEndState(visual, char)

        // 线性插值
        state := CharacterState{
            CharacterID: char.ID,
            Expression:  s.interpolateExpression(startState.Expression, endState.Expression, progress),
            Pose:        s.interpolatePose(startState.Pose, endState.Pose, progress),
            Position:    s.interpolatePosition(startState.Position, endState.Position, progress),
        }

        states = append(states, state)
    }

    return states
}
```

---

### 3.4 视频渲染服务

```go
type VideoRendererService struct {
    db          *gorm.DB
    minio       *minio.Client
    ffmpeg      *FFmpegProcessor
}

// 渲染视频
func (s *VideoRendererService) RenderVideo(videoID uint) error {
    // 1. 获取视频和所有分镜
    video := s.getVideo(videoID)
    shots := s.getVideoShots(videoID)

    // 2. 更新状态
    video.Status = "rendering"
    s.db.Save(video)

    // 3. 按顺序渲染每个分镜
    for i, shot := range shots {
        // 3.1 下载所有帧
        frames := s.downloadFrames(shot)

        // 3.2 渲染分镜视频片段
        clipPath := s.renderShotClip(frames, shot)

        // 3.3 添加过渡效果（如果需要）
        if i > 0 {
            clipPath = s.addTransition(clipPath, shots[i-1].ClipPath)
        }

        // 3.4 更新分镜状态
        shot.ClipPath = clipPath
        s.db.Save(shot)

        // 更新进度
        video.Progress = float64(i+1) / float64(len(shots)) * 100
        s.db.Save(video)
    }

    // 4. 合并所有片段
    finalVideoPath := s.mergeClips(shots)

    // 5. 添加音频
    finalVideoPath = s.addAudio(finalVideoPath, video)

    // 6. 上传到 MinIO
    url, _ := s.minio.UploadVideo(finalVideoPath)

    // 7. 生成缩略图
    thumbnail := s.generateThumbnail(finalVideoPath)
    thumbnailURL, _ := s.minio.UploadImage(thumbnail)

    // 8. 更新视频状态
    video.VideoPath = url
    video.Thumbnail = thumbnailURL
    video.Status = "completed"
    video.TotalFrames = s.countTotalFrames(shots)
    video.Duration = s.calculateTotalDuration(shots)
    s.db.Save(video)

    return nil
}

// 渲染单个分镜片段
func (s *VideoRendererService) renderShotClip(frames []Frame, shot *StoryboardShot) string {
    // 使用 FFmpeg 将帧序列转换为视频
    inputPattern := s.saveFramesTemp(frames)

    outputPath := fmt.Sprintf("/tmp/shot_%d.mp4", shot.ID)

    cmd := s.ffmpeg.Command(
        "-framerate", fmt.Sprintf("%d", 24),
        "-i", inputPattern,
        "-c:v", "libx264",
        "-pix_fmt", "yuv420p",
        "-crf", "23",
        "-preset", "medium",
        "-r", fmt.Sprintf("%d", 24),
        outputPath,
    )

    err := cmd.Run()
    if err != nil {
        return ""
    }

    return outputPath
}

// 添加音频
func (s *VideoRendererService) addAudio(videoPath string, video *Video) string {
    // 1. 生成背景音乐
    bgmPath := s.generateBackgroundMusic(video)

    // 2. 生成音效
    sfxPath := s.generateSoundEffects(video)

    // 3. 添加音轨
    outputPath := fmt.Sprintf("/tmp/video_%d_with_audio.mp4", video.ID)

    cmd := s.ffmpeg.Command(
        "-i", videoPath,
        "-i", bgmPath,
        "-i", sfxPath,
        "-filter_complex", "[1:a]volume=0.3[a1];[2:a]volume=0.5[a2];[0:a][a1][a2]amix=inputs=3:duration=first[aout]",
        "-map", "0:v",
        "-map", "[aout]",
        "-c:v", "copy",
        "-c:a", "aac",
        "-shortest",
        outputPath,
    )

    cmd.Run()

    return outputPath
}
```

---

## 🔊 音频生成系统

### 4.1 配音生成

```go
type VoiceGenerationService struct {
    db       *gorm.DB
    aiClient AIClient
}

// 角色配音配置
type CharacterVoiceProfile struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    CharacterID uint   `json:"character_id"`

    // 声音特征
    VoiceType   string `json:"voice_type"` // male, female, child, elderly
    Age         string `json:"age"` // young, middle, old
    Tone        string `json:"tone"` // deep, high, soft, harsh
    Accent      string `json:"accent"` // regional accent

    // 配音模型
    TTSModel    string `json:"tts_model"` // eleven_labs, azure, google, amazon
    VoiceID     string `json:"voice_id"` // 模型特定的声音 ID

    // 参数
    Speed       float64 `json:"speed"` // 0.5 - 2.0
    Pitch       float64 `json:"pitch"` // -20.0 - 20.0
    Emotion     string `json:"emotion"` // neutral, happy, sad, angry, excited

    CreatedAt   time.Time `json:"created_at"`
}

// 生成角色配音
func (s *VoiceGenerationService) GenerateVoiceover(shot *StoryboardShot) string {
    // 1. 获取角色配音配置
    characters := s.parseCharacters(shot.Characters)

    // 2. 为每个角色的对话生成音频
    audioTracks := []AudioTrack{}

    for _, char := range characters {
        if char.Dialogue != "" {
            voiceProfile := s.getCharacterVoiceProfile(char.ID)

            audio := s.generateDialogueAudio(char.Dialogue, voiceProfile)
            audioTracks = append(audioTracks, audio)
        }
    }

    // 3. 混合音频
    mixedAudio := s.mixAudioTracks(audioTracks)

    return mixedAudio
}

// 生成对话音频
func (s *VoiceGenerationService) generateDialogueAudio(dialogue string, profile *CharacterVoiceProfile) string {
    options := TTSOptions{
        Model:   profile.TTSModel,
        VoiceID: profile.VoiceID,
        Speed:   profile.Speed,
        Pitch:   profile.Pitch,
        Emotion: profile.Emotion,
    }

    audio := s.aiClient.TextToSpeech(dialogue, options)

    return audio.URL
}
```

### 4.2 背景音乐生成

```go
type MusicGenerationService struct {
    db       *gorm.DB
    aiClient AIClient
}

// 生成背景音乐
func (s *MusicGenerationService) GenerateBackgroundMusic(video *Video) string {
    // 1. 确定音乐风格
    mood := s.determineMusicalMood(video)

    // 2. 生成音乐
    music := s.aiClient.GenerateMusic(MusicOptions{
        Duration: video.Duration,
        Mood:     mood,
        Genre:    s.selectGenre(video.ArtStyle),
        Tempo:    s.selectTempo(mood),
        Loop:     true,
    })

    return music.URL
}

// 确定音乐情绪
func (s *MusicGenerationService) determineMusicalMood(video *Video) string {
    // 从章节内容分析情绪
    chapter := s.getChapter(video.ChapterID)
    mood := s.aiClient.AnalyzeEmotionalMood(chapter.Content)

    return mood
}
```

### 4.3 音效生成

```go
type SoundEffectService struct {
    db       *gorm.DB
    aiClient AIClient
}

// 生成音效
func (s *SoundEffectService) GenerateSoundEffects(video *Video) string {
    // 1. 分析视频内容，确定需要的音效
    soundTypes := s.analyzeRequiredSounds(video)

    // 2. 生成或获取音效
    soundEffects := []SoundEffect{}

    for _, soundType := range soundTypes {
        effect := s.getOrGenerateSoundEffect(soundType)
        soundEffects = append(soundEffects, effect)
    }

    // 3. 根据时间轴混合音效
    mixedAudio := s.mixSoundEffects(soundEffects, video)

    return mixedAudio
}
```

---

## 🎯 一致性控制系统

### 5.1 角色一致性验证

```go
type CharacterConsistencyValidator struct {
    db          *gorm.DB
    aiClient    AIClient
    vectorStore VectorStore
}

// 验证角色视觉一致性
func (v *CharacterConsistencyValidator) ValidateCharacterConsistency(frame *Frame, expectedCharacters []uint) *ConsistencyReport {
    report := &ConsistencyReport{
        Issues: []ConsistencyIssue{},
    }

    // 1. 提取帧中的角色
    detectedCharacters := v.detectCharactersInFrame(frame)

    // 2. 对比期望的角色
    for _, expectedCharID := range expectedCharacters {
        expected := v.getCharacterVisual(expectedCharID)
        detected := v.findCharacterInFrame(detectedCharacters, expectedCharID)

        if detected == nil {
            // 缺少期望的角色
            report.Issues = append(report.Issues, ConsistencyIssue{
                Type:        "missing_character",
                Severity:    "high",
                Description: fmt.Sprintf("期望的角色 %s 未在帧中检测到", expected.Name),
                Location:    fmt.Sprintf("帧 %d", frame.FrameNo),
            })
        } else {
            // 验证角色外观一致性
            similarity := v.calculateCharacterSimilarity(detected.Embedding, expected.VisualEmbedding)

            if similarity < 0.85 {
                report.Issues = append(report.Issues, ConsistencyIssue{
                    Type:        "character_appearance_drift",
                    Severity:    "medium",
                    Description: fmt.Sprintf("角色 %s 的外观一致性不足 (%.2f)", expected.Name, similarity),
                    Location:    fmt.Sprintf("帧 %d", frame.FrameNo),
                    Suggestion:  "使用更高的参考权重或角色 LoRA",
                })
            }
        }
    }

    // 3. 检测意外出现的角色
    for _, detected := range detectedCharacters {
        if !v.isExpectedCharacter(detected.ID, expectedCharacters) {
            report.Issues = append(report.Issues, ConsistencyIssue{
                Type:        "unexpected_character",
                Severity:    "low",
                Description: fmt.Sprintf("意外的角色出现在帧中"),
                Location:    fmt.Sprintf("帧 %d", frame.FrameNo),
            })
        }
    }

    return report
}

// 计算角色相似度
func (v *CharacterConsistencyValidator) calculateCharacterSimilarity(
    detectedEmbedding []float64,
    expectedEmbedding []float64,
) float64 {
    // 使用余弦相似度
    similarity := v.cosineSimilarity(detectedEmbedding, expectedEmbedding)
    return similarity
}
```

### 5.2 场景一致性验证

```go
type SceneConsistencyValidator struct {
    db          *gorm.DB
    aiClient    AIClient
    vectorStore VectorStore
}

// 验证场景一致性
func (v *SceneConsistencyValidator) ValidateSceneConsistency(frame *Frame, expectedScene *SceneVisualDesign) *ConsistencyReport {
    report := &ConsistencyReport{
        Issues: []ConsistencyIssue{},
    }

    // 1. 提取帧的场景特征
    sceneFeatures := v.extractSceneFeatures(frame)

    // 2. 对比期望的场景
    similarity := v.cosineSimilarity(sceneFeatures.Embedding, expectedScene.VisualEmbedding)

    if similarity < 0.8 {
        report.Issues = append(report.Issues, ConsistencyIssue{
            Type:        "scene_drift",
            Severity:    "medium",
            Description: fmt.Sprintf("场景一致性不足 (%.2f)", similarity),
            Location:    fmt.Sprintf("帧 %d", frame.FrameNo),
            Suggestion:  "使用场景 LoRA 或更高的参考权重",
        })
    }

    // 3. 验证关键元素
    keyElements := v.extractKeyElements(frame, expectedScene)
    for _, element := range keyElements {
        if !element.IsPresent {
            report.Issues = append(report.Issues, ConsistencyIssue{
                Type:        "missing_scene_element",
                Severity:    "low",
                Description: fmt.Sprintf("场景关键元素 %s 缺失", element.Name),
                Location:    fmt.Sprintf("帧 %d", frame.FrameNo),
            })
        }
    }

    return report
}
```

---

## 🛠️ 前端视频编辑器

### 6.1 编辑器架构

```vue
<template>
  <div class="video-editor">
    <!-- 顶部工具栏 -->
    <div class="toolbar">
      <Button @click="handlePreview">预览</Button>
      <Button @click="handleExport">导出</Button>
      <Button @click="handleAutoGenerate">自动生成</Button>
      <div class="ai-status">
        <span v-if="isGenerating">生成中... {{ progress }}%</span>
      </div>
    </div>

    <!-- 主工作区 -->
    <div class="workspace">
      <!-- 左侧：角色与场景面板 -->
      <div class="sidebar-left">
        <CharacterPanel :characters="characters" @select="handleCharacterSelect" />
        <ScenePanel :scenes="scenes" @select="handleSceneSelect" />
      </div>

      <!-- 中间：预览区 -->
      <div class="preview-area">
        <div class="video-preview">
          <video v-if="currentVideo" :src="currentVideo" controls />
          <div v-else class="placeholder">选择分镜预览</div>
        </div>

        <!-- 分镜列表 -->
        <div class="storyboard">
          <div
            v-for="shot in shots"
            :key="shot.id"
            class="shot-card"
            :class="{ active: selectedShot === shot.id }"
            @click="handleShotSelect(shot)"
          >
            <img :src="shot.thumbnail" />
            <div class="shot-info">
              <span class="shot-number">{{ shot.shotNo }}</span>
              <span class="shot-duration">{{ shot.duration }}s</span>
            </div>
          </div>
        </div>
      </div>

      <!-- 右侧：属性面板 -->
      <div class="sidebar-right">
        <ShotProperties v-if="selectedShot" :shot="selectedShot" @update="handleShotUpdate" />
        <CharacterProperties v-if="selectedCharacter" :character="selectedCharacter" @update="handleCharacterUpdate" />
        <SceneProperties v-if="selectedScene" :scene="selectedScene" @update="handleSceneUpdate" />
      </div>
    </div>

    <!-- 底部：时间轴 -->
    <div class="timeline">
      <Timeline
        :shots="shots"
        :current-time="currentTime"
        @seek="handleSeek"
        @shot-update="handleShotUpdate"
      />
    </div>
  </div>
</template>

<script setup>
const shots = ref([])
const characters = ref([])
const scenes = ref([])
const selectedShot = ref(null)
const selectedCharacter = ref(null)
const selectedScene = ref(null)
const currentVideo = ref(null)
const isGenerating = ref(false)
const progress = ref(0)

// 自动生成视频
const handleAutoGenerate = async () => {
  isGenerating.value = true
  progress.value = 0

  try {
    const result = await $fetch('/api/videos/generate', {
      method: 'POST',
      body: {
        novel_id: route.params.id,
        chapter_no: route.params.chapter,
      }
    })

    // 通过 WebSocket 接收进度
    const ws = new WebSocket(result.websocket_url)
    ws.onmessage = (event) => {
      const data = JSON.parse(event.data)
      progress.value = data.progress

      if (data.status === 'completed') {
        currentVideo.value = data.video_url
        isGenerating.value = false
        ws.close()
      }
    }
  } catch (error) {
    console.error('生成失败:', error)
    isGenerating.value = false
  }
}

// 预览视频
const handlePreview = () => {
  // 播放当前选中的分镜或整个视频
}

// 导出视频
const handleExport = async () => {
  const result = await $fetch('/api/videos/export', {
    method: 'POST',
    body: {
      video_id: route.params.video_id,
      format: 'mp4',
    }
  })

  // 下载视频
  window.open(result.download_url)
}
</script>

<style scoped>
.video-editor {
  display: flex;
  flex-direction: column;
  height: 100vh;
  background: #1a1a1a;
  color: #fff;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 1rem;
  padding: 1rem;
  border-bottom: 1px solid #333;
}

.workspace {
  display: flex;
  flex: 1;
  overflow: hidden;
}

.sidebar-left,
.sidebar-right {
  width: 300px;
  padding: 1rem;
  overflow-y: auto;
  background: #222;
}

.preview-area {
  flex: 1;
  display: flex;
  flex-direction: column;
  padding: 1rem;
}

.video-preview {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #000;
  border-radius: 8px;
  margin-bottom: 1rem;
}

.storyboard {
  display: flex;
  gap: 0.5rem;
  overflow-x: auto;
  padding: 0.5rem;
}

.shot-card {
  flex-shrink: 0;
  width: 160px;
  cursor: pointer;
  border: 2px solid transparent;
  border-radius: 4px;
  overflow: hidden;
}

.shot-card.active {
  border-color: #3b82f6;
}

.shot-card img {
  width: 100%;
  height: 90px;
  object-fit: cover;
}

.shot-info {
  display: flex;
  justify-content: space-between;
  padding: 0.5rem;
  background: #333;
}

.timeline {
  height: 200px;
  border-top: 1px solid #333;
}
</style>
```

---

## 📊 API 设计

### 视频管理 API

```
POST   /api/videos                    # 创建视频项目
GET    /api/videos/:id                # 获取视频详情
PUT    /api/videos/:id                # 更新视频配置
DELETE /api/videos/:id                # 删除视频

POST   /api/videos/:id/generate       # 生成视频
GET    /api/videos/:id/progress       # 获取生成进度
POST   /api/videos/:id/export         # 导出视频

GET    /api/videos/:id/storyboard     # 获取分镜列表
POST   /api/videos/:id/storyboard     # 创建分镜
GET    /api/storyboards/:shot         # 获取分镜详情
PUT    /api/storyboards/:shot         # 更新分镜
DELETE /api/storyboards/:shot         # 删除分镜

POST   /api/storyboards/:shot/generate-frames  # 生成分镜帧
GET    /api/storyboards/:shot/frames           # 获取帧列表
```

### 角色视觉管理 API

```
GET    /api/characters/:id/visual     # 获取角色视觉设计
POST   /api/characters/:id/visual     # 创建视觉设计
PUT    /api/characters/:id/visual     # 更新视觉设计

POST   /api/characters/:id/visual/generate  # 生成角色视觉
GET    /api/characters/:id/expressions       # 获取表情库
POST   /api/characters/:id/expressions       # 添加表情
GET    /api/characters/:id/poses             # 获取动作库
POST   /api/characters/:id/poses             # 添加动作
```

### 场景视觉管理 API

```
GET    /api/scenes/:id/visual          # 获取场景视觉设计
POST   /api/scenes/:id/visual          # 创建视觉设计
PUT    /api/scenes/:id/visual          # 更新视觉设计

POST   /api/scenes/:id/visual/generate      # 生成场景视觉
GET    /api/scenes/:id/angles                # 获取视角库
POST   /api/scenes/:id/angles                # 添加视角
```

### 音频管理 API

```
GET    /api/characters/:id/voice       # 获取配音配置
POST   /api/characters/:id/voice       # 创建配音配置
PUT    /api/characters/:id/voice       # 更新配音配置

POST   /api/stories/:id/generate-voiceover    # 生成配音
POST   /api/stories/:id/generate-music        # 生成背景音乐
POST   /api/stories/:id/generate-sfx          # 生成音效
```

### WebSocket

```
WS /ws/videos/:id/generation        # 视频生成进度推送
WS /ws/storyboards/:id/frames      # 帧生成进度推送
```

---

## 🚀 部署方案

### Docker Compose（扩展）

```yaml
# 新增服务
video-generator:
  build:
    context: .
    dockerfile: deployments/Dockerfile.video
  ports:
    - "8082:8080"
  environment:
    - DB_HOST=mysql
    - REDIS_HOST=redis
    - MINIO_ENDPOINT=minio:9000
    - VECTOR_STORE_URL=http://weaviate:8080
    - STABLE_DIFFUSION_URL=http://stable-diffusion:7860
  depends_on:
    - mysql
    - redis
    - minio
    - weaviate
    - stable-diffusion

stable-diffusion:
  image: stabilityai/stable-diffusion:latest
  ports:
    - "7860:7860"
  volumes:
    - stable_diffusion_models:/models

ffmpeg-server:
  image: jrottenberg/ffmpeg:latest
  ports:
    - "8083:8080"

volumes:
  stable_diffusion_models:
```

---

## 📝 开发路线图

### Phase 1: 基础视觉设计 - 4 周

- [ ] 角色视觉生成系统
- [ ] 场景视觉生成系统
- [ ] 表情库和动作库生成
- [ ] 基础图像生成集成

### Phase 2: 分镜系统 - 3 周

- [ ] 文本到分镜转换
- [ ] 分镜编辑器
- [ ] 摄像机配置
- [ ] 分镜预览

### Phase 3: 视频生成 - 4 周

- [ ] 帧序列生成
- [ ] 一致性控制（角色、场景）
- [ ] 视频渲染
- [ ] 基础导出

### Phase 4: 音频系统 - 3 周

- [ ] 角色配音生成
- [ ] 背景音乐生成
- [ ] 音效生成
- [ ] 音频混合

### Phase 5: 高级功能 - 4 周

- [ ] LoRA 训练集成
- [ ] 高级一致性验证
- [ ] 视频后处理（调色、特效）
- [ ] 批量生成优化

**总计：18 周（约 4.5 个月）**

---

## 📈 预期效果

### 一致性指标

| 指标 | 目标值 | 说明 |
|-----|-------|------|
| 角色视觉一致性 | > 90% | 同一角色在不同场景下的相似度 |
| 场景视觉一致性 | > 85% | 同一场景在不同视角下的相似度 |
| 帧间连续性 | > 80% | 相邻帧的平滑过渡 |
| 表情准确性 | > 85% | 生成的表情符合对话情绪 |

### 性能指标

| 指标 | 目标值 |
|-----|-------|
| 单帧生成时间 | < 10 秒 |
| 分镜生成时间 | < 2 分钟/分镜 |
| 视频渲染速度 | 实时播放的 2x |
| 内存占用 | < 16GB |

---

## 🎯 总结

InkFrame 视频生成系统通过以下核心机制保证角色和场景的一致性：

1. **角色视觉一致性系统**
   - 角色状态快照和 LoRA 训练
   - 表情库、动作库、视角库
   - IP-Adapter 参考控制

2. **场景视觉一致性系统**
   - 场景视觉设计和多视角生成
   - 环境、光影、氛围控制
   - 场景 LoRA 微调

3. **智能一致性控制**
   - 实时视觉相似度验证
   - 自动检测和修复
   - 人工审核和调整

4. **完整的视频生成流程**
   - 文本到分镜自动转换
   - 帧序列生成和渲染
   - 音频生成和混合

通过这套系统，用户可以基于生成的小说，自动生成高质量、一致性强的视频内容。

---

**InkFrame Video** - 让小说变成生动的影像 🎬
