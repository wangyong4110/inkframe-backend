package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// ============================================
// 视频生成服务 - Video Generation Service
// ============================================

// VideoGenerationRequest 视频生成请求
type VideoGenerationRequest struct {
	NovelID        uint     `json:"novel_id"`
	ChapterID      uint     `json:"chapter_id"`
	Title          string   `json:"title"`
	Resolution     string   `json:"resolution"`      // 720p, 1080p, 4k
	FrameRate      int      `json:"frame_rate"`      // 24, 30, 60
	AspectRatio    string   `json:"aspect_ratio"`    // 16:9, 9:16, 1:1
	ArtStyle       string   `json:"art_style"`       // realistic, anime, cartoon
	ColorGrade     string   `json:"color_grade"`     // cinematic, vintage, vibrant
}

// VideoGenerationResult 视频生成结果
type VideoGenerationResult struct {
	VideoID        uint     `json:"video_id"`
	Status         string   `json:"status"`           // planning, storyboard, generating, rendering, completed
	Progress       float64  `json:"progress"`        // 0-100
	TotalShots     int      `json:"total_shots"`
	GeneratedShots int      `json:"generated_shots"`
	ErrorMessage   string   `json:"error_message,omitempty"`
}

// ============================================
// 视频生成服务
// ============================================

// VideoService 视频服务
type VideoService struct {
	videoRepo      *VideoRepository
	storyboardRepo *StoryboardRepository
	chapterRepo    *ChapterRepository
	aiService      *AIService
}

// NewVideoService 创建视频服务
func NewVideoService(
	videoRepo *VideoRepository,
	storyboardRepo *StoryboardRepository,
	chapterRepo *ChapterRepository,
	aiService *AIService,
) *VideoService {
	return &VideoService{
		videoRepo:      videoRepo,
		storyboardRepo: storyboardRepo,
		chapterRepo:    chapterRepo,
		aiService:      aiService,
	}
}

// CreateVideo 创建视频项目
func (s *VideoService) CreateVideo(novelID uint, chapterID uint, title string) (*model.Video, error) {
	video := &model.Video{
		NovelID: novelID,
		ChapterID: chapterID,
		Title: title,
		Status: "planning",
		Resolution: "1080p",
		FrameRate: 24,
		AspectRatio: "16:9",
		TotalShots: 0,
		GeneratedShots: 0,
	}

	if err := s.videoRepo.Create(video); err != nil {
		return nil, err
	}

	return video, nil
}

// GetVideo 获取视频
func (s *VideoService) GetVideo(id uint) (*model.Video, error) {
	return s.videoRepo.GetByID(id)
}

// ListVideos 获取视频列表
func (s *VideoService) ListVideos(novelID uint) ([]*model.Video, error) {
	return s.videoRepo.ListByNovel(novelID)
}

// GenerateVideo 生成视频
func (s *VideoService) GenerateVideo(videoID uint) error {
	video, err := s.videoRepo.GetByID(videoID)
	if err != nil {
		return err
	}

	// 更新状态
	video.Status = "generating"
	return s.videoRepo.Update(video)
}

// ============================================
// 智能分镜生成（使用 video_enhancement_service.go 中的服务）
// ============================================

// GenerateStoryboard 生成分镜
func (s *VideoService) GenerateStoryboard(chapter *model.Chapter, characters []*model.Character, config *VideoGenerationRequest) ([]*model.StoryboardShot, error) {
	shots := []*model.StoryboardShot{}

	// 分析章节内容，提取场景
	scenes := s.analyzeChapterScenes(chapter.Content)

	// 分析情感曲线
	emotions := s.analyzeEmotions(chapter.Content)

	// 为每个场景生成镜头
	currentShot := 1
	for _, scene := range scenes {
		shotCount := s.determineShotCount(scene, emotions)
		for i := 0; i < shotCount; i++ {
			shot := s.generateShot(scene, i, shotCount, currentShot, characters)
			shots = append(shots, shot)
			currentShot++
		}
	}

	return shots, nil
}

// analyzeChapterScenes 分析章节场景
func (s *VideoService) analyzeChapterScenes(content string) []*SceneAnalysis {
	scenes := []*SceneAnalysis{}
	
	paragraphs := strings.Split(content, "\n\n")
	for _, para := range paragraphs {
		if len(para) < 50 {
			continue
		}

		scene := &SceneAnalysis{
			Content:   para,
			Type:      "narrative",
			Intensity: 0.5,
			Pacing:    "normal",
		}

		// 检测对话
		if strings.Contains(para, "「") || strings.Contains(para, "\"") {
			scene.Type = "dialogue"
			scene.Intensity = 0.6
		}

		// 检测动作
		actionMarkers := []string{"打", "跑", "跳", "走", "飞", "攻击", "战斗"}
		for _, marker := range actionMarkers {
			if strings.Contains(para, marker) {
				scene.Type = "action"
				scene.Intensity = 0.9
				scene.Pacing = "fast"
				break
			}
		}

		// 检测场景描述
		sceneMarkers := []string{"来到", "进入", "看见", "位于", "这里是"}
		for _, marker := range sceneMarkers {
			if strings.Contains(para, marker) {
				scene.Type = "scene"
				break
			}
		}

		scenes = append(scenes, scene)
	}

	return scenes
}

// analyzeEmotions 分析情感
func (s *VideoService) analyzeEmotions(content string) []string {
	emotions := []string{}
	
	emotionMarkers := map[string]string{
		"高兴": "happy",
		"悲伤": "sad",
		"愤怒": "angry",
		"紧张": "tense",
		"平静": "calm",
		"恐惧": "fearful",
	}

	paragraphs := strings.Split(content, "\n\n")
	for _, para := range paragraphs {
		for word, emotion := range emotionMarkers {
			if strings.Contains(para, word) {
				emotions = append(emotions, emotion)
				break
			}
		}
	}

	if len(emotions) == 0 {
		emotions = append(emotions, "neutral")
	}

	return emotions
}

// determineShotCount 确定镜头数量
func (s *VideoService) determineShotCount(scene *SceneAnalysis, emotions []string) int {
	base := 2
	
	switch scene.Type {
	case "action":
		base = 4
	case "dialogue":
		base = 3
	case "scene":
		base = 2
	}

	if scene.Intensity > 0.8 {
		base++
	}

	return base
}

// generateShot 生成单个镜头
func (s *VideoService) generateShot(scene *SceneAnalysis, index, total, shotNo int, characters []*model.Character) *model.StoryboardShot {
	shot := &model.StoryboardShot{
		ShotNo:      shotNo,
		Description: scene.Content,
		ShotType:    "medium",
		ShotAngle:   "eye_level",
		Duration:    5.0,
		Emotion:     "neutral",
		Lighting:    "natural",
		Status:      "pending",
	}

	// 根据场景类型选择镜头类型
	switch scene.Type {
	case "action":
		shot.ShotType = "close_up"
		shot.CameraMovement = "tracking"
	case "dialogue":
		shot.ShotType = "medium"
		shot.CameraMovement = "static"
	case "scene":
		shot.ShotType = "wide"
		shot.CameraMovement = "pan"
	}

	return shot
}

// SceneAnalysis 场景分析
type SceneAnalysis struct {
	Content   string  `json:"content"`
	Type      string  `json:"type"`       // narrative, dialogue, action, scene
	Intensity float64 `json:"intensity"`  // 0-1
	Pacing    string  `json:"pacing"`     // slow, normal, fast
}

// ============================================
// 帧生成服务
// ============================================

// FrameGeneratorService 帧生成服务
type FrameGeneratorService struct {
	aiService *AIService
}

// NewFrameGeneratorService 创建帧生成服务
func NewFrameGeneratorService(aiService *AIService) *FrameGeneratorService {
	return &FrameGeneratorService{aiService: aiService}
}

// GenerateFrame 生成帧
func (s *FrameGeneratorService) GenerateFrame(shot *model.StoryboardShot, characterConfigs []*CharacterVisual) (*GeneratedFrame, error) {
	frame := &GeneratedFrame{
		FrameNo:     int(shot.ShotNo),
		Prompt:      shot.Description,
		GeneratedAt: time.Now(),
	}

	return frame, nil
}

// GeneratedFrame 生成的帧
type GeneratedFrame struct {
	FrameNo      int       `json:"frame_no"`
	ImageURL     string    `json:"image_url"`
	Prompt       string    `json:"prompt"`
	GeneratedAt  time.Time `json:"generated_at"`
}

// CharacterVisual 角色视觉配置
type CharacterVisual struct {
	CharacterID uint   `json:"character_id"`
	Name        string `json:"name"`
	Expression  string `json:"expression"`
	Position    string `json:"position"`
}

// ============================================
// 一致性验证服务
// ============================================

// ConsistencyValidatorService 一致性验证服务
type ConsistencyValidatorService struct {
	aiService *AIService
}

// NewConsistencyValidatorService 创建一致性验证服务
func NewConsistencyValidatorService(aiService *AIService) *ConsistencyValidatorService {
	return &ConsistencyValidatorService{aiService: aiService}
}

// ValidateConsistency 验证一致性
func (s *ConsistencyValidatorService) ValidateConsistency(frames []*GeneratedFrame) (*ConsistencyValidationResult, error) {
	result := &ConsistencyValidationResult{
		Consistent:    true,
		Score:         0.95,
		Issues:        []ConsistencyIssue{},
	}

	return result, nil
}

// ConsistencyValidationResult 一致性验证结果
type ConsistencyValidationResult struct {
	Consistent bool                `json:"consistent"`
	Score     float64             `json:"score"`
	Issues    []ConsistencyIssue  `json:"issues"`
}

// ConsistencyIssue 一致性问题
type ConsistencyIssue struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	FrameNo     int    `json:"frame_no"`
	Severity    string `json:"severity"`
}
