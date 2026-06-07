package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"strings"
	"sync"
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
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

// UpdateSceneAnchorReq 更新请求
type UpdateSceneAnchorReq struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Description    string `json:"description"`
	Variant        string `json:"variant"`
	ParentAnchorID *uint  `json:"parent_anchor_id"`
}

func (s *SceneAnchorService) Get(id uint) (*model.SceneAnchor, error) {
	return s.repo.GetByID(id)
}

// GetByID 通过 ID 获取场景锚点（与 Get 等价，供内部服务调用）
func (s *SceneAnchorService) GetByID(id uint) (*model.SceneAnchor, error) {
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

// SetRefImage 锁定参考图（强制覆盖）
func (s *SceneAnchorService) SetRefImage(id uint, imageURL string, shotID *uint) error {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return err
	}
	anchor.RefImageURL = imageURL
	now := time.Now()
	anchor.RefImageLockedAt = &now
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
	fragment := anchor.Description
	if anchor.Name != "" && fragment != "" {
		fragment = fmt.Sprintf("[scene: %s] %s", anchor.Name, fragment)
	}
	// 追加 PromptLock 锁定关键词（风格/色调/光线等约束）
	if anchor.PromptLock != "" {
		fragment = fragment + ", " + anchor.PromptLock
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
	Variant     string `json:"variant"`      // day/night/winter/battle 等变体标签
	ParentName  string `json:"parent_name"`  // 父级锚点名称（变体时填写，用于解析 ParentAnchorID）
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

// parseAnchorJSONResultWithFallback attempts full parse first; on failure tries
// streaming partial decode so that a truncated or partially invalid JSON array
// still yields whatever valid entries were produced before the error.
func parseAnchorJSONResultWithFallback(raw string) ([]extractedAnchor, error) {
	// 1. Full parse (including wrapped-object variants)
	if result, err := parseAnchorJSONResult(raw); err == nil {
		return result, nil
	}
	// 2. Partial recovery via streaming decoder
	cleaned := extractJSON(strings.TrimSpace(raw))
	dec := json.NewDecoder(strings.NewReader(cleaned))
	// Consume the opening '[' token
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("anchor JSON fully unparseable: %v", err)
	}
	var partial []extractedAnchor
	for dec.More() {
		var item extractedAnchor
		if err := dec.Decode(&item); err == nil && item.Name != "" {
			partial = append(partial, item)
		}
	}
	if len(partial) > 0 {
		logger.Printf("[SceneAnchor] partial JSON recovery: got %d anchors", len(partial))
		return partial, nil
	}
	return nil, fmt.Errorf("anchor JSON fully unparseable")
}

// ExtractFromChapter 调用 LLM 提取章节中的场景锚点，去重后批量创建
func (s *SceneAnchorService) ExtractFromChapter(ctx context.Context, tenantID, novelID uint, novelTitle, chapterContent string) ([]*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] ExtractFromChapter: novelID=%d contentLen=%d", novelID, len(chapterContent))
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

	// 获取提示词语言配置
	promptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, nErr := s.novelRepo.GetByID(novelID); nErr == nil && novel.PromptLanguage != "" {
			promptLanguage = novel.PromptLanguage
		}
	}

	// 渲染 prompt
	anchorPrompt, err := renderPrompt("scene_anchor_extract", map[string]interface{}{
		"NovelTitle":      novelTitle,
		"ExistingAnchors": existingEntries,
		"ChapterContent":  truncateForPrompt(chapterContent, 8000),
		"PromptLanguage":  promptLanguage,
	})
	if err != nil {
		return nil, fmt.Errorf("render scene_anchor_extract: %w", err)
	}

	// 调用 LLM（带租户 ID，确保使用正确的 provider）
	jsonStr, err := s.aiSvc.generateJSONForTenant(tenantID, novelID, "scene_anchor_extract", anchorPrompt, 2)
	if err != nil {
		return nil, fmt.Errorf("LLM extract anchors: %w", err)
	}

	// 解析 JSON（支持部分恢复）
	extracted, err := parseAnchorJSONResultWithFallback(jsonStr)
	if err != nil {
		logger.Errorf("[SceneAnchorService] ExtractFromChapter: JSON parse failed: %v, jsonStr=%q", err, jsonStr)
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	// 构建规范化名称集合（用于语义去重：忽略大小写 + 空格）
	normalizedNames := make(map[string]bool, len(existing))
	for name := range existingNames {
		normalizedNames[normalizeAnchorName(name)] = true
	}

	// 批量创建（改进去重：精确匹配 + 规范化匹配 + 子串包含匹配）
	created := make([]*model.SceneAnchor, 0, len(extracted))
	for _, e := range extracted {
		if e.Name == "" {
			continue
		}
		normName := normalizeAnchorName(e.Name)
		// 精确匹配
		if existingNames[e.Name] {
			continue
		}
		// 规范化匹配（大小写/空格变体）
		if normalizedNames[normName] {
			continue
		}
		// 子串包含匹配（防止"皇宫"和"皇宫正殿"重复注册）
		if anchorNameOverlaps(normName, normalizedNames) {
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
			Variant:     e.Variant,
		}
		// 解析父级锚点 ID（变体场景）
		if e.ParentName != "" {
			for _, a := range existing {
				if a.Name == e.ParentName || normalizeAnchorName(a.Name) == normalizeAnchorName(e.ParentName) {
					id := a.ID
					anchor.ParentAnchorID = &id
					break
				}
			}
		}
		if err := s.repo.Create(anchor); err != nil {
			logger.Errorf("[SceneAnchorService] create anchor %q: %v", e.Name, err)
			continue
		}
		created = append(created, anchor)
		existingNames[e.Name] = true
		normalizedNames[normName] = true
	}

	logger.Printf("[SceneAnchorService] ExtractFromChapter done: novelID=%d created=%d", novelID, len(created))
	return created, nil
}

// GenerateRefImage 使用 AI 图像生成为锚点生成参考图并锁定
func (s *SceneAnchorService) GenerateRefImage(ctx context.Context, tenantID, id uint, providerName string) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("anchor not found: %w", err)
	}

	// 查询小说的图片风格（用于模型选择）和标题（用于 OSS 路径）
	imageStyle := ""
	aspectRatio := ""
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(anchor.NovelID); err == nil {
			imageStyle = novel.ImageStyle
			aspectRatio = novel.VideoConf().VideoAspectRatio
			if novel.Title != "" {
				ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novel.Title})
			}
		}
	}

	// 组装图像生成 prompt（注入场景描述 + PromptLock + 标准化场景生成词）
	prompt := anchor.Description
	if anchor.PromptLock != "" {
		prompt += ", " + anchor.PromptLock
	}
	sceneType := anchor.Type
	if sceneType == "" {
		sceneType = "exterior"
	}
	sceneSuffix := "establishing shot, " + sceneType + " scene, no humans, no people, no figures, " +
		"cinematic composition, three depth layers foreground midground background, " +
		"architectural detail, atmospheric lighting, masterpiece, best quality, ultra-detailed, 8k uhd, " +
		"photorealistic, sharp focus"
	if prompt != "" {
		prompt += ", " + sceneSuffix
	} else {
		prompt = sceneSuffix
	}
	// negative prompt 通过 imageStyle 字段携带（GenerateCharacterThreeView 支持）
	// 此处额外追加场景专用 negative 词到 prompt 注释段（不同模型处理方式不同，此为兜底）
	negativeHint := " | negative: person, people, human, man, woman, figure, silhouette, character, " +
		"blurry, low quality, watermark, text, floating objects, modern elements"
	prompt += negativeHint

	sizeOverride := imageAspectRatioToSize(aspectRatio, "master")
	imageURL, err := s.aiSvc.GenerateCharacterThreeView(ctx, tenantID, providerName, prompt, "", imageStyle, "", sizeOverride)
	if err != nil {
		return nil, fmt.Errorf("generate ref image: %w", err)
	}

	if err := s.SetRefImage(id, imageURL, nil); err != nil {
		return nil, fmt.Errorf("save ref image: %w", err)
	}

	return s.repo.GetByID(id)
}

// EditRefImageWithInstruction 以文生图模型（DreamO）重新生成参考图，原图作为参考保持风格一致性。
// instruction 为自然语言提示词，如"让场景更暗，增加烟雾"
func (s *SceneAnchorService) EditRefImageWithInstruction(ctx context.Context, tenantID, id uint, instruction string) (*model.SceneAnchor, error) {
	anchor, err := s.repo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("anchor not found: %w", err)
	}
	if anchor.RefImageURL == "" {
		return nil, fmt.Errorf("no ref image to edit; generate one first")
	}

	// 读取小说画面风格，保持风格一致
	imageStyle := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(anchor.NovelID); e == nil {
			imageStyle = novel.ImageStyle
		}
	}

	// consistencyWeight=0.7 → GenerateCharacterThreeViewMulti 自动选用 DreamO（文生图+参考图）
	// scale = weight * 10 = 7（中等参考强度，兼顾提示词创意与原图风格一致性）
	imageURL, err := s.aiSvc.GenerateCharacterThreeViewMulti(
		ctx, tenantID, "", instruction,
		[]string{anchor.RefImageURL},
		imageStyle, "", "", 0.7,
	)
	if err != nil {
		return nil, fmt.Errorf("edit ref image: %w", err)
	}

	if err := s.SetRefImage(id, imageURL, nil); err != nil {
		return nil, fmt.Errorf("save edited ref image: %w", err)
	}
	logger.Printf("[SceneAnchorService] EditRefImageWithInstruction: anchor %d edited, url=%s", id, imageURL)
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

// BatchGenerateRefImages 批量为小说的场景锚点生成参考图。
// force=false：跳过已有参考图的锚点；force=true：全量重新生成（风格变更时使用）。
// 外层并发度固定为 3（避免大批量时无限创建 goroutine），内层 imageSem 进一步限流。
func (s *SceneAnchorService) BatchGenerateRefImages(ctx context.Context, tenantID, novelID uint, provider string, force bool, progressFn func(int)) (succeeded, failed int, err error) {
	anchors, err := s.repo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list anchors: %w", err)
	}

	var todo []*model.SceneAnchor
	for _, a := range anchors {
		if force || a.RefImageURL == "" {
			todo = append(todo, a)
		}
	}
	total := len(todo)

	const outerConcurrency = 3
	sem := make(chan struct{}, outerConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, anchor := range todo {
		anchor := anchor
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if _, genErr := s.GenerateRefImage(ctx, tenantID, anchor.ID, provider); genErr != nil {
				logger.Errorf("[SceneAnchorService] BatchGenerateRefImages: anchor %d (%s) failed: %v", anchor.ID, anchor.Name, genErr)
				mu.Lock()
				failed++
				done++
				cur := done
				mu.Unlock()
				if progressFn != nil && total > 0 {
					progressFn(cur * 99 / total)
				}
				return
			}
			mu.Lock()
			succeeded++
			done++
			cur := done
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[SceneAnchorService] BatchGenerateRefImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
	return succeeded, failed, nil
}

// AIExtractAllFromNovel 批量从小说所有章节中提取场景锚点（并发 3 goroutine）。
// 无章节数量上限，支持增量提取（已有同名锚点自动跳过）。
func (s *SceneAnchorService) AIExtractAllFromNovel(tenantID, novelID uint, progressFn func(int)) ([]*model.SceneAnchor, error) {
	logger.Printf("[SceneAnchorService] AIExtractAllFromNovel: novelID=%d", novelID)
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

	// 收集所有有内容的章节（无数量上限，支持全量提取）
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" {
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
	total := len(candidates)
	var done int

	for _, ch := range candidates {
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			anchors, err := s.ExtractFromChapter(ctx, tenantID, novelID, novelTitle, ch.Content)
			mu.Lock()
			done++
			if err != nil {
				logger.Errorf("[SceneAnchorService] AIExtractAllFromNovel chapter %d: %v", ch.ID, err)
				failCount++
			} else {
				allCreated = append(allCreated, anchors...)
			}
			cur := done
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[SceneAnchorService] AIExtractAllFromNovel done: novelID=%d created=%d", novelID, len(allCreated))
	if failCount == len(candidates) {
		return nil, fmt.Errorf("所有章节场景锚点提取均失败，请检查 AI 提供商配置")
	}
	return allCreated, nil
}

// normalizeAnchorName 规范化场景名称用于去重比较（转小写，去除多余空格）
func normalizeAnchorName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), ""))
}

// anchorNameOverlaps 检测 normName 与 existing 集合中是否存在高重叠（防止同质化锚点）。
// 若 normName 是某个已有名称的子串，或某个已有名称是 normName 的子串，视为重叠。
func anchorNameOverlaps(normName string, existing map[string]bool) bool {
	for en := range existing {
		if len(en) >= 2 && len(normName) >= 2 {
			if strings.Contains(normName, en) || strings.Contains(en, normName) {
				return true
			}
		}
	}
	return false
}
