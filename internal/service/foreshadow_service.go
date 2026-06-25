package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

type ForeshadowCRUDService struct {
	repo        *repository.ForeshadowRepository
	aiService   *AIService
	novelRepo   *repository.NovelRepository
	chapterRepo *repository.ChapterRepository
}

func NewForeshadowCRUDService(repo *repository.ForeshadowRepository) *ForeshadowCRUDService {
	return &ForeshadowCRUDService{repo: repo}
}

func (s *ForeshadowCRUDService) WithAIDeps(aiSvc *AIService, novelRepo *repository.NovelRepository, chapterRepo *repository.ChapterRepository) *ForeshadowCRUDService {
	s.aiService = aiSvc
	s.novelRepo = novelRepo
	s.chapterRepo = chapterRepo
	return s
}

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
)
	if err != nil {
		return nil, fmt.Errorf("AI extraction: %w", err)
	}

	type foreshadowJSON struct {
		Title                 string `json:"title"`
		Description           string `json:"description"`
		PlantedChapterNo      int    `json:"planted_chapter_no"`
		PayoffChapterNo       int    `json:"payoff_chapter_no"`
		ActualPayoffChapterNo int    `json:"actual_payoff_chapter_no"`
		Status                string `json:"status"`
		Level                 string `json:"level"`
		ForeshadowType        string `json:"foreshadow_type"`
		Tags                  string `json:"tags"`
		Confidence            string `json:"confidence"`
		CharacterNames        string `json:"character_names"`
	}

	raw := extractJSON(result)
	var items []foreshadowJSON
	var wrapped struct {
		Foreshadows []foreshadowJSON `json:"foreshadows"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Foreshadows) > 0 {
		items = wrapped.Foreshadows
	} else if err2 := json.Unmarshal([]byte(raw), &items); err2 != nil {
		logger.Errorf("ForeshadowCRUDService.AIExtractFromNovel: parse error: %v, raw: %.200s", err2, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	existing, _ := s.repo.ListByNovel(novelID)
	existingTitles := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		existingTitles[e.Title] = struct{}{}
	}

	validLevels := map[string]bool{"main": true, "sub": true, "detail": true}
	validTypes := map[string]bool{"prop": true, "dialogue": true, "behavior": true, "scene": true, "prophecy": true}
	validConf := map[string]bool{"high": true, "medium": true, "low": true}

	var created []*model.Foreshadow
	for _, item := range items {
		if item.Title == "" {
			continue
		}
		if _, dup := existingTitles[item.Title]; dup {
			continue
		}
		status := item.Status
		if status == "" {
			status = "planted"
		}
		level := item.Level
		if !validLevels[level] {
			level = "sub"
		}
		fsType := item.ForeshadowType
		if !validTypes[fsType] {
			fsType = ""
		}
		conf := item.Confidence
		if !validConf[conf] {
			conf = "medium"
		}
		f := &model.Foreshadow{
			NovelID:               novelID,
			Title:                 item.Title,
			Description:           item.Description,
			Status:                status,
			Tags:                  item.Tags,
			PayoffChapterNo:       item.PayoffChapterNo,
			ActualPayoffChapterNo: item.ActualPayoffChapterNo,
			Level:                 level,
			ForeshadowType:        fsType,
			Confidence:            conf,
		}
		if item.PlantedChapterNo > 0 {
			f.PlantedChapterNo = item.PlantedChapterNo
			if chID, ok := chapterNoToID[item.PlantedChapterNo]; ok {
				f.PlantedChapterID = &chID
			}
		}
		if item.ActualPayoffChapterNo > 0 {
			if chID, ok := chapterNoToID[item.ActualPayoffChapterNo]; ok {
				f.ActualPayoffChapterID = &chID
			}
		}
		// 将角色名字作为 CharacterIDs 占位存储（前端可后续绑定真实 ID）
		if item.CharacterNames != "" {
			namesJSON, _ := json.Marshal([]string{item.CharacterNames})
			f.CharacterIDs = string(namesJSON)
		}
		if err := s.repo.Create(f); err != nil {
			logger.Errorf("ForeshadowCRUDService.AIExtractFromNovel: create %q: %v", f.Title, err)
			continue
		}
		existingTitles[item.Title] = struct{}{}
		created = append(created, f)
	}
	logger.Printf("[ForeshadowCRUDService] AIExtractFromNovel: novelID=%d created=%d", novelID, len(created))
	return created, nil
}

func (s *ForeshadowCRUDService) Create(ctx context.Context, f *model.Foreshadow) error {
	return s.repo.Create(f)
}

func (s *ForeshadowCRUDService) ListByNovel(ctx context.Context, novelID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *ForeshadowCRUDService) ListUnfulfilled(ctx context.Context, novelID uint) ([]*model.Foreshadow, error) {
	return s.repo.ListUnfulfilled(novelID)
}

func (s *ForeshadowCRUDService) Update(ctx context.Context, id uint, updates map[string]interface{}) (*model.Foreshadow, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if v, ok := updates["title"].(string); ok && v != "" {
		f.Title = v
	}
	if v, ok := updates["description"].(string); ok {
		f.Description = v
	}
	if v, ok := updates["status"].(string); ok && v != "" {
		f.Status = v
	}
	if v, ok := updates["tags"].(string); ok {
		f.Tags = v
	}
	if v, ok := updates["level"].(string); ok && v != "" {
		f.Level = v
	}
	if v, ok := updates["foreshadow_type"].(string); ok {
		f.ForeshadowType = v
	}
	if v, ok := updates["importance"].(string); ok && v != "" {
		f.Importance = v
	}
	if v, ok := updates["confidence"].(string); ok && v != "" {
		f.Confidence = v
	}
	if v, ok := updates["character_ids"].(string); ok {
		f.CharacterIDs = v
	}
	if v, ok := updates["payoff_notes"].(string); ok {
		f.PayoffNotes = v
	}
	if v, ok := updates["planted_chapter_no"].(float64); ok {
		f.PlantedChapterNo = int(v)
	}
	if v, ok := updates["planted_chapter_id"].(float64); ok {
		if v > 0 {
			uid := uint(v)
			f.PlantedChapterID = &uid
		} else {
			f.PlantedChapterID = nil
		}
	}
	if v, ok := updates["payoff_chapter_no"].(float64); ok {
		f.PayoffChapterNo = int(v)
	}
	if v, ok := updates["actual_payoff_chapter_no"].(float64); ok {
		f.ActualPayoffChapterNo = int(v)
	}
	if v, ok := updates["actual_payoff_chapter_id"].(float64); ok {
		if v > 0 {
			uid := uint(v)
			f.ActualPayoffChapterID = &uid
		} else {
			f.ActualPayoffChapterID = nil
		}
	}
	if v, ok := updates["payoff_quality"].(float64); ok {
		f.PayoffQuality = int(v)
	}
	if v, ok := updates["parent_id"].(float64); ok {
		if v > 0 {
			uid := uint(v)
			f.ParentID = &uid
		} else {
			f.ParentID = nil
		}
	}
	if v, ok := updates["linked_hook_id"].(float64); ok {
		if v > 0 {
			uid := uint(v)
			f.LinkedHookID = &uid
		} else {
			f.LinkedHookID = nil
		}
	}
	if v, ok := updates["linked_arc_id"].(float64); ok {
		if v > 0 {
			uid := uint(v)
			f.LinkedArcID = &uid
		} else {
			f.LinkedArcID = nil
		}
	}
	return f, s.repo.Update(f)
}

func (s *ForeshadowCRUDService) Delete(ctx context.Context, id uint) error {
	return s.repo.Delete(id)
}

// ReinforcementRecord 强化记录
type ReinforcementRecord struct {
	ChapterNo int    `json:"chapter_no"`
	Note      string `json:"note"`
}

// AddReinforcement 在指定章节添加伏笔强化记录
func (s *ForeshadowCRUDService) AddReinforcement(ctx context.Context, id uint, chapterNo int, note string) (*model.Foreshadow, error) {
	f, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	var records []ReinforcementRecord
	if f.ReinforcementChapters != "" {
		_ = json.Unmarshal([]byte(f.ReinforcementChapters), &records)
	}
	// 去重：同章节已有记录则更新 note
	found := false
	for i, r := range records {
		if r.ChapterNo == chapterNo {
			records[i].Note = note
			found = true
			break
		}
	}
	if !found {
		records = append(records, ReinforcementRecord{ChapterNo: chapterNo, Note: note})
	}
	data, _ := json.Marshal(records)
	f.ReinforcementChapters = string(data)
	return f, s.repo.Update(f)
}

// ForeshadowTreeNode 伏笔树节点
type ForeshadowTreeNode struct {
	*model.Foreshadow
	Children []*ForeshadowTreeNode `json:"children,omitempty"`
}

// GetTree 返回按父子关系组织的伏笔树结构
func (s *ForeshadowCRUDService) GetTree(ctx context.Context, novelID uint) ([]*ForeshadowTreeNode, error) {
	list, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	nodeMap := make(map[uint]*ForeshadowTreeNode, len(list))
	for _, f := range list {
		nodeMap[f.ID] = &ForeshadowTreeNode{Foreshadow: f}
	}
	var roots []*ForeshadowTreeNode
	for _, node := range nodeMap {
		if node.ParentID != nil {
			if parent, ok := nodeMap[*node.ParentID]; ok {
				parent.Children = append(parent.Children, node)
				continue
			}
		}
		roots = append(roots, node)
	}
	return roots, nil
}

// ForeshadowStats 伏笔统计数据
type ForeshadowStats struct {
	Total         int            `json:"total"`
	Planted       int            `json:"planted"`
	PaidOff       int            `json:"paid_off"`
	Abandoned     int            `json:"abandoned"`
	Overdue       int            `json:"overdue"`
	NarrativeDebt float64        `json:"narrative_debt"` // planted/total 叙事债务率 0-1
	ByLevel       map[string]int `json:"by_level"`
	ByType        map[string]int `json:"by_type"`
	ByConfidence  map[string]int `json:"by_confidence"`
}

func (s *ForeshadowCRUDService) GetStats(ctx context.Context, novelID uint, currentChapterNo int) (*ForeshadowStats, error) {
	list, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	stats := &ForeshadowStats{
		ByLevel:      make(map[string]int),
		ByType:       make(map[string]int),
		ByConfidence: make(map[string]int),
	}
	for _, f := range list {
		stats.Total++
		switch f.Status {
		case "planted":
			stats.Planted++
			if f.PayoffChapterNo > 0 && currentChapterNo > f.PayoffChapterNo+3 {
				stats.Overdue++
			}
		case "paid_off":
			stats.PaidOff++
		case "abandoned":
			stats.Abandoned++
		}
		if f.Level != "" {
			stats.ByLevel[f.Level]++
		}
		if f.ForeshadowType != "" {
			stats.ByType[f.ForeshadowType]++
		}
		conf := f.Confidence
		if conf == "" {
			conf = "medium"
		}
		stats.ByConfidence[conf]++
	}
	if stats.Total > 0 {
		stats.NarrativeDebt = float64(stats.Planted) / float64(stats.Total)
	}
	return stats, nil
}
