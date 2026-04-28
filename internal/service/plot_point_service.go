package service

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)


// PlotPointService 剧情点服务
type PlotPointService struct {
	repo        *repository.PlotPointRepository
	aiService   *AIService
	chapterRepo *repository.ChapterRepository // optional, for AIExtractFromNovel
}

func NewPlotPointService(repo *repository.PlotPointRepository, aiService *AIService) *PlotPointService {
	return &PlotPointService{repo: repo, aiService: aiService}
}

// WithChapterRepo 注入章节仓库（可选，用于 AIExtractFromNovel）
func (s *PlotPointService) WithChapterRepo(r *repository.ChapterRepository) *PlotPointService {
	s.chapterRepo = r
	return s
}

// List 获取章节的所有剧情点
func (s *PlotPointService) List(chapterID uint) ([]*model.PlotPoint, error) {
	return s.repo.ListByChapter(chapterID)
}

// ListByNovel 获取小说级剧情点（可按类型/未解决过滤）
func (s *PlotPointService) ListByNovel(novelID uint, ppType string, onlyUnresolved bool) ([]*model.PlotPoint, error) {
	return s.repo.ListByNovel(novelID, ppType, onlyUnresolved)
}

// Create 手动创建剧情点
func (s *PlotPointService) Create(pp *model.PlotPoint) error {
	return s.repo.Create(pp)
}

// Update 更新剧情点
func (s *PlotPointService) Update(id uint, req *model.UpdatePlotPointRequest) (*model.PlotPoint, error) {
	pp, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Type != "" {
		pp.Type = req.Type
	}
	if req.Description != "" {
		pp.Description = req.Description
	}
	if req.Characters != nil {
		b, err := json.Marshal(req.Characters)
		if err != nil {
			return nil, fmt.Errorf("marshal characters: %w", err)
		}
		pp.Characters = string(b)
	}
	if req.Locations != nil {
		b, err := json.Marshal(req.Locations)
		if err != nil {
			return nil, fmt.Errorf("marshal locations: %w", err)
		}
		pp.Locations = string(b)
	}
	if req.IsResolved != nil {
		pp.IsResolved = *req.IsResolved
	}
	if req.ResolvedIn != nil {
		pp.ResolvedIn = req.ResolvedIn
	}
	if err := s.repo.Update(pp); err != nil {
		return nil, err
	}
	return pp, nil
}

// Delete 删除剧情点
func (s *PlotPointService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// MarkResolved 标记剧情点已解决
func (s *PlotPointService) MarkResolved(id uint, resolvedInChapterID uint) (*model.PlotPoint, error) {
	pp, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	pp.IsResolved = true
	pp.ResolvedIn = &resolvedInChapterID
	if err := s.repo.Update(pp); err != nil {
		return nil, err
	}
	return pp, nil
}

// AIExtractFromNovel 从小说所有章节中提取剧情点（跳过已有剧情点的章节）
func (s *PlotPointService) AIExtractFromNovel(tenantID, novelID uint) ([]*model.PlotPoint, error) {
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}

	var all []*model.PlotPoint
	for _, ch := range chapters {
		if ch.Content == "" {
			continue
		}
		// 跳过已有剧情点的章节
		existing, _ := s.repo.ListByChapter(ch.ID)
		if len(existing) > 0 {
			all = append(all, existing...)
			continue
		}
		pps, err := s.ExtractFromChapter(tenantID, ch)
		if err != nil {
			log.Printf("PlotPointService.AIExtractFromNovel: chapter %d: %v", ch.ID, err)
			continue
		}
		all = append(all, pps...)
	}
	return all, nil
}

// ExtractFromChapter 使用AI从章节内容提取剧情点并保存
func (s *PlotPointService) ExtractFromChapter(tenantID uint, chapter *model.Chapter) ([]*model.PlotPoint, error) {
	if chapter.Content == "" {
		return nil, fmt.Errorf("chapter content is empty")
	}

	prompt := fmt.Sprintf(`请从以下章节内容中提取关键剧情点，返回JSON数组格式：
{
  "plot_points": [
    {
      "type": "conflict/climax/resolution/twist/foreshadow",
      "description": "剧情点描述",
      "characters": ["角色名1", "角色名2"],
      "locations": ["地点"]
    }
  ]
}
章节内容：%s`, chapter.Content)

	result, err := s.aiService.GenerateWithProvider(tenantID, chapter.NovelID, "plot_extraction", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("AI extraction failed: %w", err)
	}

	var plotResult struct {
		PlotPoints []struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Characters  []string `json:"characters"`
			Locations   []string `json:"locations"`
		} `json:"plot_points"`
	}

	if err := json.Unmarshal([]byte(extractJSON(result)), &plotResult); err != nil {
		log.Printf("PlotPointService.ExtractFromChapter: parse error: %v, raw: %.200s", err, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	pps := make([]*model.PlotPoint, 0, len(plotResult.PlotPoints))
	for _, p := range plotResult.PlotPoints {
		chars, _ := json.Marshal(p.Characters)
		locs, _ := json.Marshal(p.Locations)
		pps = append(pps, &model.PlotPoint{
			TenantID:    tenantID,
			NovelID:     chapter.NovelID,
			ChapterID:   chapter.ID,
			Type:        p.Type,
			Description: p.Description,
			Characters:  string(chars),
			Locations:   string(locs),
		})
	}

	if err := s.repo.BatchCreate(pps); err != nil {
		return nil, fmt.Errorf("failed to save plot points: %w", err)
	}
	return pps, nil
}
