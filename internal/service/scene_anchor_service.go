package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// SceneAnchorService 场景锚点服务
// 将命名场景的视觉描述、风格 token 和参考图固定下来，
// 在分镜图像生成时强制注入，确保跨镜头布景一致。
type SceneAnchorService struct {
	repo        *repository.SceneAnchorRepository
	shotRepo    *repository.StoryboardRepository
	novelRepo   *repository.NovelRepository
	chapterRepo *repository.ChapterRepository // optional, for AIExtractAllFromNovel
	aiSvc       *AIService
}

func NewSceneAnchorService(repo *repository.SceneAnchorRepository, shotRepo *repository.StoryboardRepository, aiSvc *AIService, novelRepo *repository.NovelRepository) *SceneAnchorService {
	return &SceneAnchorService{repo: repo, shotRepo: shotRepo, aiSvc: aiSvc, novelRepo: novelRepo}
}

// WithChapterRepo 注入章节仓库（可选，用于批量提取所有章节的场景锚点）
func (s *SceneAnchorService) WithChapterRepo(r *repository.ChapterRepository) *SceneAnchorService {
	s.chapterRepo = r
	return s
}

// CreateSceneAnchorReq 创建请求
type CreateSceneAnchorReq struct {
	Name           string `json:"name" binding:"required"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	PromptLock     string `json:"prompt_lock"`
	StyleTokens    string `json:"style_tokens"`
	Notes          string `json:"notes"`
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

// UpdateSceneAnchorReq 更新请求
type UpdateSceneAnchorReq struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	PromptLock     string `json:"prompt_lock"`
	StyleTokens    string `json:"style_tokens"`
	Notes          string `json:"notes"`
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

func (s *SceneAnchorService) Get(id uint) (*model.SceneAnchor, error) {
	return s.repo.GetByID(id)
}

func (s *SceneAnchorService) ListByNovel(novelID uint) ([]*model.SceneAnchor, error) {
	return s.repo.ListByNovel(novelID)
}

func (s *SceneAnchorService) Create(tenantID, novelID uint, req CreateSceneAnchorReq) (*model.SceneAnchor, error) {
	anchor := &model.SceneAnchor{
		TenantID:       tenantID,
		NovelID:        novelID,
		Name:           req.Name,
		Type:           req.Type,
		Description:    req.Description,
		PromptLock:     req.PromptLock,
		StyleTokens:    req.StyleTokens,
		Notes:          req.Notes,
		Variant:        req.Variant,
		ParentAnchorID: req.ParentAnchorID,
	}
	if err := s.repo.Create(anchor); err != nil {
		return nil, err
	}
	return anchor, nil
}

func (s *SceneAnchorService) Update(id uint, req UpdateSceneAnchorReq) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		anchor.Name = req.Name
	}
	if req.Type != "" {
		anchor.Type = req.Type
	}
	if req.Description != "" {
		anchor.Description = req.Description
	}
	if req.PromptLock != "" {
		anchor.PromptLock = req.PromptLock
	}
	if req.StyleTokens != "" {
		anchor.StyleTokens = req.StyleTokens
	}
	if req.Notes != "" {
		anchor.Notes = req.Notes
	}
	if req.Variant != "" {
		anchor.Variant = req.Variant
	}
	if req.ParentAnchorID != nil {
		anchor.ParentAnchorID = req.ParentAnchorID
	}
	if err := s.repo.Update(anchor); err != nil {
		return nil, err
	}
	return anchor, nil
}

func (s *SceneAnchorService) Delete(id uint) error {
	return s.repo.Delete(id)
}

// SetRefImage 锁定参考图（强制覆盖，可选绑定 shotID）
func (s *SceneAnchorService) SetRefImage(id uint, imageURL string, shotID *uint) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	anchor.RefImageURL = imageURL
	now := time.Now()
	anchor.RefImageLockedAt = &now
	if shotID != nil {
		anchor.RefImageShotID = shotID
	}
	return s.repo.Update(anchor)
}

// AutoSetRefImage 首次自动锁定参考图（仅当 RefImageURL 为空时更新）
func (s *SceneAnchorService) AutoSetRefImage(id uint, imageURL string) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	if anchor.RefImageURL != "" {
		return nil // already locked
	}
	anchor.RefImageURL = imageURL
	now := time.Now()
	anchor.RefImageLockedAt = &now
	return s.repo.Update(anchor)
}

// BuildPromptFragment 返回拼接好的 prompt 片段和参考图URL
// 供分镜图像生成时注入 ImagePromptConfig
func (s *SceneAnchorService) BuildPromptFragment(id uint) (promptFragment string, refImageURL string, err error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return "", "", err
	}
	var parts []string
	if anchor.Description != "" {
		parts = append(parts, anchor.Description)
	}
	if anchor.PromptLock != "" {
		parts = append(parts, anchor.PromptLock)
	}
	if anchor.StyleTokens != "" {
		parts = append(parts, anchor.StyleTokens)
	}
	fragment := strings.Join(parts, ", ")
	if anchor.Name != "" && fragment != "" {
		fragment = fmt.Sprintf("[scene: %s] %s", anchor.Name, fragment)
	}
	return fragment, anchor.RefImageURL, nil
}

// SetShotAnchor 绑定分镜到场景锚点
func (s *SceneAnchorService) SetShotAnchor(shotID uint, anchorID *uint) error {
	shot, err := s.shotRepo.GetByID(shotID)
	if err != nil {
		return err
	}
	shot.SceneAnchorID = anchorID
	return s.shotRepo.Update(shot)
}

// extractedAnchor LLM 返回的 JSON 锚点结构
type extractedAnchor struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	PromptLock  string `json:"prompt_lock"`
	StyleTokens string `json:"style_tokens"`
}

// parseAnchorJSONResult parses the LLM response into []extractedAnchor.
// Handles both bare arrays and wrapped objects like {"scene_anchors":[...]}.
func parseAnchorJSONResult(raw string) ([]extractedAnchor, error) {
	cleaned := extractJSON(strings.TrimSpace(raw))
	var result []extractedAnchor
	if err := json.Unmarshal([]byte(cleaned), &result); err == nil {
		return result, nil
	}
	// Try wrapped object: {"scene_anchors":[...]} or {"data":[...]} etc.
	var wrapper map[string]json.RawMessage
	if json.Unmarshal([]byte(cleaned), &wrapper) == nil {
		for _, v := range wrapper {
			if json.Unmarshal(v, &result) == nil {
				return result, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot parse as anchor array: %.200s", raw)
}

// ExtractFromChapter 调用 LLM 提取章节中的场景锚点，去重后批量创建
func (s *SceneAnchorService) ExtractFromChapter(ctx context.Context, tenantID, novelID uint, novelTitle, chapterContent string) ([]*model.SceneAnchor, error) {
	log.Printf("[SceneAnchorService] ExtractFromChapter: novelID=%d contentLen=%d", novelID, len(chapterContent))
	_ = ctx // 未来可传 context 给 AI provider

	// 获取已存在锚点（去重用）
	existing, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("list existing anchors: %w", err)
	}

	type existingEntry struct {
		Name        string
		Description string
	}
	existingEntries := make([]existingEntry, 0, len(existing))
	existingNames := make(map[string]bool, len(existing))
	for _, a := range existing {
		existingEntries = append(existingEntries, existingEntry{Name: a.Name, Description: a.Description})
		existingNames[a.Name] = true
	}

	// 渲染 prompt
	tmplStr := loadPromptTemplate("scene_anchor_extract.tmpl")
	tmpl, err := template.New("scene_anchor_extract").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("parse scene_anchor_extract.tmpl: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"NovelTitle":      novelTitle,
		"ExistingAnchors": existingEntries,
		"ChapterContent":  truncateForPrompt(chapterContent, 8000),
	}); err != nil {
		return nil, fmt.Errorf("render scene_anchor_extract.tmpl: %w", err)
	}

	// 调用 LLM（带租户 ID，确保使用正确的 provider）
	jsonStr, err := s.aiSvc.generateJSONForTenant(tenantID, novelID, "scene_anchor_extract", buf.String(), 2)
	if err != nil {
		return nil, fmt.Errorf("LLM extract anchors: %w", err)
	}

	// 解析 JSON
	extracted, err := parseAnchorJSONResult(jsonStr)
	if err != nil {
		log.Printf("[SceneAnchorService] ExtractFromChapter: JSON parse failed: %v, jsonStr=%q", err, jsonStr)
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	// 批量创建（跳过已存在名称）
	created := make([]*model.SceneAnchor, 0, len(extracted))
	for _, e := range extracted {
		if existingNames[e.Name] {
			continue
		}
		anchorType := e.Type
		if anchorType == "" {
			anchorType = "exterior"
		}
		anchor := &model.SceneAnchor{
			TenantID:    tenantID,
			NovelID:     novelID,
			Name:        e.Name,
			Type:        anchorType,
			Description: e.Description,
			PromptLock:  e.PromptLock,
			StyleTokens: e.StyleTokens,
		}
		if err := s.repo.Create(anchor); err != nil {
			log.Printf("[SceneAnchorService] create anchor %q: %v", e.Name, err)
			continue
		}
		created = append(created, anchor)
		existingNames[e.Name] = true
	}

	log.Printf("[SceneAnchorService] ExtractFromChapter done: novelID=%d created=%d", novelID, len(created))
	return created, nil
}

// GenerateRefImage 使用 AI 图像生成为锚点生成参考图并锁定
func (s *SceneAnchorService) GenerateRefImage(ctx context.Context, tenantID, id uint, providerName string) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("anchor not found: %w", err)
	}

	// 查询小说的图片风格（用于模型选择）
	imageStyle := ""
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(anchor.NovelID); err == nil {
			imageStyle = novel.ImageStyle
		}
	}

	// 组装图像生成 prompt
	var parts []string
	if anchor.Description != "" {
		parts = append(parts, anchor.Description)
	}
	if anchor.PromptLock != "" {
		parts = append(parts, anchor.PromptLock)
	}
	if anchor.StyleTokens != "" {
		parts = append(parts, anchor.StyleTokens)
	}
	parts = append(parts, "scene background, no characters, cinematic composition")
	prompt := strings.Join(parts, ", ")

	imageURL, err := s.aiSvc.GenerateCharacterThreeView(ctx, tenantID, providerName, prompt, "", imageStyle, "")
	if err != nil {
		return nil, fmt.Errorf("generate ref image: %w", err)
	}

	if err := s.SetRefImage(id, imageURL, nil); err != nil {
		return nil, fmt.Errorf("save ref image: %w", err)
	}

	return s.repo.GetByID(id)
}

// UpdateStats 更新锚点使用统计（usage_count++，avg_cons_score 滚动平均）
func (s *SceneAnchorService) UpdateStats(id uint, score float64) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	n := float64(anchor.UsageCount)
	anchor.AvgConsScore = (anchor.AvgConsScore*n + score) / (n + 1)
	anchor.UsageCount++
	return s.repo.Update(anchor)
}

// AIExtractAllFromNovel 批量从小说前 10 章中提取场景锚点（并发 3 goroutine）
func (s *SceneAnchorService) AIExtractAllFromNovel(tenantID, novelID uint) ([]*model.SceneAnchor, error) {
	log.Printf("[SceneAnchorService] AIExtractAllFromNovel: novelID=%d", novelID)
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapterRepo not configured")
	}
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, fmt.Errorf("list chapters: %w", err)
	}

	novelTitle := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
		}
	}

	const maxCh = 10
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" && len(candidates) < maxCh {
			candidates = append(candidates, ch)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	ctx := context.Background()
	const maxConcurrent = 3
	sem := make(chan struct{}, maxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	allCreated := make([]*model.SceneAnchor, 0)
	failCount := 0

	for _, ch := range candidates {
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			anchors, err := s.ExtractFromChapter(ctx, tenantID, novelID, novelTitle, ch.Content)
			if err != nil {
				log.Printf("[SceneAnchorService] AIExtractAllFromNovel chapter %d: %v", ch.ID, err)
				mu.Lock()
				failCount++
				mu.Unlock()
				return
			}
			mu.Lock()
			allCreated = append(allCreated, anchors...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	log.Printf("[SceneAnchorService] AIExtractAllFromNovel done: novelID=%d created=%d", novelID, len(allCreated))
	if failCount == len(candidates) {
		return nil, fmt.Errorf("所有章节场景锚点提取均失败，请检查 AI 提供商配置")
	}
	return allCreated, nil
}
