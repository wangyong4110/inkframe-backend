package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ForeshadowCRUDService 专用伏笔表的 CRUD 服务（ink_foreshadow 表）
// 与旧的 ForeshadowService（基于 KnowledgeBase tag）并存，新 API 优先使用此服务。
type ForeshadowCRUDService struct {
	repo        *repository.ForeshadowRepository
	aiService   *AIService
	novelRepo   *repository.NovelRepository
	chapterRepo *repository.ChapterRepository
}

func NewForeshadowCRUDService(repo *repository.ForeshadowRepository) *ForeshadowCRUDService {
	return &ForeshadowCRUDService{repo: repo}
}

// WithAIDeps 注入 AI 提取所需依赖（可选）
func (s *ForeshadowCRUDService) WithAIDeps(aiSvc *AIService, novelRepo *repository.NovelRepository, chapterRepo *repository.ChapterRepository) *ForeshadowCRUDService {
	s.aiService = aiSvc
	s.novelRepo = novelRepo
	s.chapterRepo = chapterRepo
	return s
}

// AIExtractFromNovel 使用 AI 从小说章节摘要中提取伏笔（已有伏笔时跳过）
func (s *ForeshadowCRUDService) AIExtractFromNovel(ctx context.Context, tenantID, novelID uint) ([]*model.Foreshadow, error) {
	if s.aiService == nil || s.novelRepo == nil || s.chapterRepo == nil {
		return nil, fmt.Errorf("AI dependencies not configured")
	}

	novel, err := s.novelRepo.GetByID(novelID)
	if err != nil {
		return nil, fmt.Errorf("novel not found: %w", err)
	}

	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}

	summariesText := buildChapterSummariesText(chapters, 20, 8000)
	if summariesText == "" {
		// 章节尚无摘要/内容/大纲时，降级使用小说简介或风格描述作为上下文
		fallback := novel.Description
		if fallback == "" {
			fallback = novel.StylePrompt
		}
		if fallback == "" {
			return nil, fmt.Errorf("no chapter content available for extraction")
		}
		summariesText = "【小说简介/大纲】\n" + truncateForPrompt(fallback, 3000)
	}

	chapterNoToID := make(map[int]uint, len(chapters))
	for _, ch := range chapters {
		chapterNoToID[ch.ChapterNo] = ch.ID
	}

	prompt, err := renderPrompt("extract_foreshadows", map[string]interface{}{
		"NovelTitle": novel.Title,
		"Genre":      novel.Genre,
		"Summaries":  summariesText,
	})
	if err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "extract_foreshadows", prompt, "",
		StoryboardOverrides{})
	if err != nil {
		return nil, fmt.Errorf("AI extraction: %w", err)
	}

	type foreshadowJSON struct {
		Title            string `json:"title"`
		Description      string `json:"description"`
		PlantedChapterNo int    `json:"planted_chapter_no"`
		Status           string `json:"status"`
		Tags             string `json:"tags"`
	}

	raw := extractJSON(result)
	var items []foreshadowJSON
	var wrapped struct {
		Foreshadows []foreshadowJSON `json:"foreshadows"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Foreshadows) > 0 {
		items = wrapped.Foreshadows
	} else if err2 := json.Unmarshal([]byte(raw), &items); err2 != nil {
		logger.Errorf("ForeshadowCRUDService.AIExtractFromNovel: parse error: %v, raw: %.200s", err, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	// 按 title 去重：已有同名伏笔直接跳过
	existing, _ := s.repo.ListByNovel(novelID, tenantID)
	existingTitles := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		existingTitles[e.Title] = struct{}{}
	}

	var created []*model.Foreshadow
	for _, item := range items {
		if item.Title == "" {
			continue
		}
		if _, dup := existingTitles[item.Title]; dup {
			logger.Printf("ForeshadowCRUDService.AIExtractFromNovel: skip duplicate %q", item.Title)
			continue
		}
		status := item.Status
		if status == "" {
			status = "planted"
		}
		f := &model.Foreshadow{
			TenantID:    tenantID,
			NovelID:     novelID,
			Title:       item.Title,
			Description: item.Description,
			Status:      status,
			Tags:        item.Tags,
		}
		if item.PlantedChapterNo > 0 {
			if chID, ok := chapterNoToID[item.PlantedChapterNo]; ok {
				f.PlantedChapterID = &chID
			}
		}
		if err := s.repo.Create(f); err != nil {
			logger.Errorf("ForeshadowCRUDService.AIExtractFromNovel: create %q: %v", f.Title, err)
			continue
		}
		existingTitles[item.Title] = struct{}{} // 防止同批次重名
		created = append(created, f)
	}
	logger.Printf("[ForeshadowCRUDService] AIExtractFromNovel: novelID=%d created=%d", novelID, len(created))
	return created, nil
}

func (s *ForeshadowCRUDService) Create(ctx context.Context, f *model.Foreshadow) error {
	return s.repo.Create(f)
}

func (s *ForeshadowCRUDService) ListByNovel(ctx context.Context, novelID uint, tenantID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListByNovel(novelID, tenantID)
}

func (s *ForeshadowCRUDService) ListUnfulfilled(ctx context.Context, novelID uint, tenantID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListUnfulfilled(novelID, tenantID)
}

func (s *ForeshadowCRUDService) Update(ctx context.Context, id uint, updates map[string]interface{}) (*model.Foreshadow, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if v, ok := updates["title"].(string); ok && v != "" {
		f.Title = v
	}
	if v, ok := updates["description"].(string); ok && v != "" {
		f.Description = v
	}
	if v, ok := updates["status"].(string); ok && v != "" {
		f.Status = v
	}
	if v, ok := updates["tags"].(string); ok {
		f.Tags = v
	}
	return f, s.repo.Update(f)
}

func (s *ForeshadowCRUDService) Delete(ctx context.Context, id uint) error {
	return s.repo.Delete(id)
}
