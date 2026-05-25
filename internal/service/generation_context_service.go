package service

import (
	"context"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
)


// ============================================
// StoryboardService 分镜服务（handler-facing）
// ============================================

// StoryboardOverrides 允许调用方覆盖单次分镜生成的 AI 参数（0 表示使用系统默认值）
type StoryboardOverrides struct {
	MaxTokens      int     // 输出 token 上限，0=系统默认（≥4096）
	Temperature    float64 // 生成温度，0=系统默认（0.1）
	TimeoutSeconds int     // 单次 AI 调用超时（秒），0=系统默认（300s）
	VoiceMode      string  // 配音模式：""/"both"=对白+旁白（默认），"narration"=仅旁白，"dialogue"=仅对白
}

// buildChapterOverrides 从请求参数和小说项目配置构建 AI 参数覆盖。
// 优先级：请求参数 > 项目配置 > 系统默认。
func buildChapterOverrides(req *model.GenerateChapterRequest, novel *model.Novel) StoryboardOverrides {
	o := StoryboardOverrides{
		MaxTokens:      req.MaxTokens,
		Temperature:    req.Temperature,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = novel.MaxTokens
	}
	if o.Temperature == 0 {
		o.Temperature = novel.Temperature
	}
	if o.TimeoutSeconds == 0 {
		o.TimeoutSeconds = novel.TimeoutSeconds
	}
	return o
}

type StoryboardService struct {
	videoService *VideoService
	aiService    *AIService
}

func NewStoryboardService(videoService *VideoService, aiService *AIService) *StoryboardService {
	return &StoryboardService{videoService: videoService, aiService: aiService}
}

func (s *StoryboardService) GenerateStoryboard(videoID, chapterID uint, characters []string, style, provider, userPrompt string, progressFn func(int), overrides StoryboardOverrides) (interface{}, error) {
	var chapterIDPtr *uint
	if chapterID != 0 {
		chapterIDPtr = &chapterID
	}
	shots, err := s.videoService.GenerateStoryboard(videoID, provider, userPrompt, progressFn, overrides, chapterIDPtr)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"video_id":   videoID,
		"chapter_id": chapterID,
		"shots":      shots,
		"total":      len(shots),
	}, nil
}

// ReviewStoryboard 调用 AI 对分镜脚本进行专业审查
func (s *StoryboardService) ReviewStoryboard(tenantID, videoID uint, provider string, previousScore float64) (*model.StoryboardReview, uint, error) {
	return s.videoService.ReviewStoryboard(tenantID, videoID, provider, previousScore)
}

// OptimizeStoryboardFromReview 根据审查报告一键优化分镜
func (s *StoryboardService) OptimizeStoryboardFromReview(tenantID, videoID uint, review *model.StoryboardReview, provider string) (int, error) {
	return s.videoService.OptimizeStoryboardFromReview(tenantID, videoID, review, provider)
}

// ListReviewRecords 返回审查历史列表
func (s *StoryboardService) ListReviewRecords(videoID uint) ([]*model.StoryboardReviewRecord, error) {
	return s.videoService.ListReviewRecords(videoID)
}

// RollbackReview 回滚某次审查应用
func (s *StoryboardService) RollbackReview(tenantID, videoID, recordID uint) (int, error) {
	return s.videoService.RollbackReview(tenantID, videoID, recordID)
}

func (s *StoryboardService) AnalyzeEmotions(content string) (interface{}, error) {
	prompt := fmt.Sprintf("请分析以下内容的情感曲线：\n%s", content)
	result, err := s.aiService.Generate(0, "analysis", prompt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"analysis": result,
		"content":  content[:min(100, len(content))],
	}, nil
}

// ============================================
// QualityControlService adapter methods
// ============================================

// CheckChapter handler-compatible wrapper — delegates to the real AI+rule-based check.
func (s *QualityControlService) CheckChapter(id uint) (*QualityReport, error) {
	chapter, err := s.chapterRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("chapter %d not found: %w", id, err)
	}
	novel, err := s.novelRepo.GetByID(chapter.NovelID)
	if err != nil {
		return nil, fmt.Errorf("novel %d not found: %w", chapter.NovelID, err)
	}
	return s.CheckChapterQuality(context.Background(), chapter, novel)
}

// ============================================
// VideoEnhancementService adapter methods
// ============================================

// EnhanceVideo handler-compatible wrapper (accepts model.EnhancementConfig)
func (s *VideoEnhancementService) EnhanceVideo(videoURL string, enhancements []model.EnhancementConfig) (interface{}, error) {
	configs := make([]EnhancementConfig, 0, len(enhancements))
	for _, e := range enhancements {
		configs = append(configs, EnhancementConfig{
			Type:      EnhancementType(e.Type),
			Enabled:   e.Enabled,
			Intensity: e.Intensity,
		})
	}
	return s.EnhanceVideoWithConfigs(videoURL, configs)
}

// GetRecommendations handler-compatible wrapper
func (s *VideoEnhancementService) GetRecommendations(fps int, resolution string, duration int, style string) (interface{}, error) {
	return map[string]interface{}{
		"fps":        fps,
		"resolution": resolution,
		"duration":   duration,
		"style":      style,
		"recommendations": []map[string]interface{}{
			{"type": "frame_interpolation", "priority": "high", "reason": "提升流畅度"},
			{"type": "super_resolution", "priority": "medium", "reason": "提升画质"},
		},
	}, nil
}

// ============================================
// CharacterArcService adapter methods
// ============================================

func (s *CharacterArcService) GetAllArcs(novelID uint) (interface{}, error) {
	characters, err := s.charRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	arcs := make([]interface{}, 0, len(characters))
	for _, c := range characters {
		arc, _ := s.GetCharacterArc(novelID, c.ID)
		arcs = append(arcs, arc)
	}
	return arcs, nil
}

func (s *CharacterArcService) UpdateArc(novelID, characterID uint, currentStage int, note string) (interface{}, error) {
	arc, err := s.GetCharacterArc(novelID, characterID)
	if err != nil {
		return nil, err
	}
	arc.CurrentStage = currentStage
	return arc, nil
}

// ============================================
// GenerationContextService adapter methods
// ============================================

func (s *GenerationContextService) BuildGenerationPrompt(novelID uint, chapterNo int, style, extraPrompt string, maxContextLen int) (string, error) {
	ctx, err := s.GetContext(novelID, chapterNo)
	if err != nil {
		return "", err
	}
	var sc *StyleConfig
	if style != "" {
		sc = &StyleConfig{NarrativeVoice: style}
	}
	prompt := s.buildGenerationPrompt(ctx, chapterNo, sc, extraPrompt)
	return prompt, nil
}

func (s *GenerationContextService) GetContextPreview(novelID uint) (interface{}, error) {
	ctx, err := s.GetContext(novelID, 0)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"novel_id":      novelID,
		"total_context": fmt.Sprintf("%d chars", len(ctx.GlobalSummary)),
		"summary":       ctx.GlobalSummary,
	}, nil
}

