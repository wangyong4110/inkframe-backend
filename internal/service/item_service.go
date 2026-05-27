package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// EffectiveItem 有效物品（合并项目级和章节级覆盖）
type EffectiveItem struct {
	model.Item
	ChapterOverride   *model.ChapterItem `json:"chapter_override,omitempty"`
	EffectiveLocation string             `json:"effective_location"`
	EffectiveOwner    string             `json:"effective_owner"`
}

// ItemService 物品服务
type ItemService struct {
	itemRepo        *repository.ItemRepository
	chapterItemRepo *repository.ChapterItemRepository
	chapterRepo     *repository.ChapterRepository
	novelRepo       *repository.NovelRepository // optional, for title/genre in AI prompts
	aiService       *AIService
}

func NewItemService(
	itemRepo *repository.ItemRepository,
	chapterItemRepo *repository.ChapterItemRepository,
	chapterRepo *repository.ChapterRepository,
	aiService *AIService,
) *ItemService {
	return &ItemService{
		itemRepo:        itemRepo,
		chapterItemRepo: chapterItemRepo,
		chapterRepo:     chapterRepo,
		aiService:       aiService,
	}
}

// WithNovelRepo 注入小说仓库（可选，用于 AI 提示词中携带标题/类型）
func (s *ItemService) WithNovelRepo(r *repository.NovelRepository) *ItemService {
	s.novelRepo = r
	return s
}

// CreateItem 创建项目级物品
func (s *ItemService) CreateItem(novelID uint, req *model.CreateItemRequest) (*model.Item, error) {
	item := &model.Item{
		NovelID:      novelID,
		UUID:         uuid.New().String(),
		Name:         req.Name,
		Description:  req.Description,
		Location:     req.Location,
		Owner:        req.Owner,
		VisualPrompt: req.VisualPrompt,
		Status:       req.Status,
	}
	if item.Status == "" {
		item.Status = "active"
	}
	return item, s.itemRepo.Create(item)
}

// GetItem 获取物品详情
func (s *ItemService) GetItem(id uint) (*model.Item, error) {
	return s.itemRepo.GetByID(id)
}

// ListItems 列出项目下所有物品
func (s *ItemService) ListItems(novelID uint) ([]*model.Item, error) {
	return s.itemRepo.ListByNovel(novelID)
}

// UpdateItem 更新物品
func (s *ItemService) UpdateItem(id uint, req *model.UpdateItemRequest) (*model.Item, error) {
	item, err := s.itemRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}
	if req.Name != "" {
		item.Name = req.Name
	}
	if req.Description != "" {
		item.Description = req.Description
	}
	if req.Location != "" {
		item.Location = req.Location
	}
	if req.Owner != "" {
		item.Owner = req.Owner
	}
	if req.VisualPrompt != "" {
		item.VisualPrompt = req.VisualPrompt
	}
	if req.ImageURL != "" {
		item.ImageURL = req.ImageURL
	}
	if req.ReferenceImageURL != "" {
		item.ReferenceImageURL = req.ReferenceImageURL
	}
	if req.Status != "" {
		item.Status = req.Status
	}
	return item, s.itemRepo.Update(item)
}

// DeleteItem 删除物品
func (s *ItemService) DeleteItem(id uint) error {
	return s.itemRepo.Delete(id)
}

// GenerateItemImage 为物品生成图像
// generateItemImageCore is the shared AI call for item image generation.
// It builds the prompt, filters the reference URL to HTTP(S) only, sets up storage context,
// and calls the AI. Used by both GenerateItemImage and BatchGenerateImages.
func (s *ItemService) generateItemImageCore(ctx context.Context, tenantID uint, item *model.Item, provider, novelTitle, imageStyle string) (string, error) {
	prompt := item.VisualPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("%s，%s，奇幻物品插画，精细细节，概念艺术", item.Name, item.Description)
	}
	aiRefURL := item.ReferenceImageURL
	if !strings.HasPrefix(aiRefURL, "http://") && !strings.HasPrefix(aiRefURL, "https://") {
		aiRefURL = ""
	}
	if novelTitle != "" {
		ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novelTitle})
	}
	return s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt+"，物品设计，白色背景，摄影棚光效", aiRefURL, imageStyle, "")
}

// referenceImageURL 可选：用户上传的参考图 URL（已存入 OSS），作为 AI 参考图使用
// provider 可选：指定使用的图像生成提供者，空字符串 = 自动选择
func (s *ItemService) GenerateItemImage(tenantID, id uint, referenceImageURL, provider string) (*model.Item, error) {
	item, err := s.itemRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}
	// Persist new reference URL; fall back to previously saved one.
	if referenceImageURL != "" {
		item.ReferenceImageURL = referenceImageURL
	}
	// Log whether a valid reference image will be used.
	aiRefURL := item.ReferenceImageURL
	if strings.HasPrefix(aiRefURL, "http://") || strings.HasPrefix(aiRefURL, "https://") {
		logger.Printf("GenerateItemImage: item=%d using reference image %s", id, aiRefURL)
	} else {
		logger.Printf("GenerateItemImage: item=%d no valid reference image, generating without reference", id)
	}
	var novelTitle, imageStyle string
	if s.novelRepo != nil && item.NovelID > 0 {
		if novel, e := s.novelRepo.GetByID(item.NovelID); e == nil {
			novelTitle = novel.Title
			imageStyle = novel.ImageStyle
		}
	}
	url, err := s.generateItemImageCore(context.Background(), tenantID, item, provider, novelTitle, imageStyle)
	if err != nil {
		return nil, fmt.Errorf("generate image failed: %w", err)
	}
	item.ImageURL = url
	return item, s.itemRepo.Update(item)
}

// AIExtractFromNovel 使用 AI 从章节内容中提取物品（按 novel_id+name upsert）
// BatchGenerateImages 批量为小说的物品生成图像（跳过已有 ImageURL 的物品）。
// 并发度由 AIService.imageSem 统一管控（config.yaml ai.image_concurrency）。
func (s *ItemService) BatchGenerateImages(tenantID, novelID uint, provider string, progressFn func(int)) (succeeded, failed int, err error) {
	items, err := s.itemRepo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list items: %w", err)
	}

	var todo []*model.Item
	for _, it := range items {
		if it.ImageURL == "" {
			todo = append(todo, it)
		}
	}
	total := len(todo)

	var novelTitle, imageStyle string
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			imageStyle = novel.ImageStyle
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, item := range todo {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			url, genErr := s.generateItemImageCore(context.Background(), tenantID, item, provider, novelTitle, imageStyle)
			if genErr != nil {
				logger.Printf("[ItemService] BatchGenerateImages: item %d (%s) failed: %v", item.ID, item.Name, genErr)
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
			item.ImageURL = url
			if saveErr := s.itemRepo.Update(item); saveErr != nil {
				logger.Printf("[ItemService] BatchGenerateImages: save item %d: %v", item.ID, saveErr)
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
	logger.Printf("[ItemService] BatchGenerateImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
	return succeeded, failed, nil
}

func (s *ItemService) AIExtractFromNovel(tenantID, novelID uint) ([]*model.Item, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}

	// 优先使用章节摘要（前 15 章，8000 字），无摘要时降级用原始内容
	summariesText := buildChapterSummariesText(chapters, 15, 8000)
	if summariesText == "" {
		summariesText = collectContent(chapters, 5, 5000)
	}

	// 获取小说标题/类型
	novelTitle := "本小说"
	novelGenre := ""
	promptLanguage := "zh"
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
			if novel.PromptLanguage != "" {
				promptLanguage = novel.PromptLanguage
			}
		}
	}
	if summariesText == "" {
		summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计主要物品道具。", novelGenre, novelTitle)
	}

	existing, _ := s.itemRepo.ListByNovel(novelID)
	byName := make(map[string]*model.Item, len(existing))
	for _, it := range existing {
		byName[it.Name] = it
	}

	existingJSON := marshalExistingNames(existing, func(it *model.Item) any {
		return struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}{it.Name, it.Description}
	})

	// 使用与分析流程相同的富格式 extract_items.j2
	itemsPrompt, err := renderPrompt("extract_items", map[string]interface{}{
		"NovelTitle":     novelTitle,
		"Genre":          novelGenre,
		"Summaries":      summariesText,
		"PromptLanguage": promptLanguage,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_items: %w", err)
	}
	if existingJSON != "" {
		itemsPrompt += "\n\n注意：已有物品如下，必须复用原名，不得改名或重复创建：\n" + existingJSON
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_items", itemsPrompt, "",
		StoryboardOverrides{MaxTokens: 8192})
	if err != nil {
		return nil, fmt.Errorf("AI extraction failed: %w", err)
	}

	var extracted []analysisItemJSON
	if err := json.Unmarshal([]byte(extractJSON(strings.TrimSpace(result))), &extracted); err != nil {
		logger.Printf("ItemService.AIExtractFromNovel: parse error: %v, raw: %.200s", err, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	upserted := make([]*model.Item, 0, len(extracted))
	for _, e := range extracted {
		if e.Name == "" {
			continue
		}
		// 校正 category
		validCat := map[string]bool{"weapon": true, "treasure": true, "tool": true, "document": true, "artifact": true, "other": true}
		if !validCat[e.Category] {
			e.Category = "other"
		}

		extractedDesc := buildItemDescription(e.Category, e.Appearance, e.Description)
		if it, ok := byName[e.Name]; ok {
			// 更新：用 AI 数据填充空缺字段
			var changed bool
			if v, ok := fillIfEmpty(it.Description, extractedDesc); ok { it.Description = v; changed = true }
			if v, ok := fillIfEmpty(it.Location, e.Location); ok { it.Location = v; changed = true }
			if v, ok := fillIfEmpty(it.Owner, e.Owner); ok { it.Owner = v; changed = true }
			if v, ok := fillIfEmpty(it.VisualPrompt, e.VisualPrompt); ok { it.VisualPrompt = v; changed = true }
			if !changed {
				upserted = append(upserted, it)
				continue
			}
			if err := s.itemRepo.Update(it); err != nil {
				logger.Printf("ItemService.AIExtractFromNovel: update %s: %v", e.Name, err)
				continue
			}
			upserted = append(upserted, it)
		} else {
			item := &model.Item{
				NovelID:      novelID,
				UUID:         uuid.New().String(),
				Name:         e.Name,
				Description:  extractedDesc,
				Location:     e.Location,
				Owner:        e.Owner,
				VisualPrompt: e.VisualPrompt,
				Status:       "active",
			}
			if err := s.itemRepo.Create(item); err != nil {
				logger.Printf("ItemService.AIExtractFromNovel: create %s: %v", e.Name, err)
				continue
			}
			upserted = append(upserted, item)
		}
	}
	return upserted, nil
}

// UpsertChapterItem 创建或更新章节级物品覆盖
func (s *ItemService) UpsertChapterItem(novelID, chapterID, itemID uint, req *model.UpsertChapterItemRequest) (*model.ChapterItem, error) {
	ci := &model.ChapterItem{
		ItemID:    itemID,
		ChapterID: chapterID,
		NovelID:   novelID,
		Location:  req.Location,
		Owner:     req.Owner,
		Condition: req.Condition,
		Notes:     req.Notes,
	}
	if err := s.chapterItemRepo.Upsert(ci); err != nil {
		return nil, err
	}
	// return the saved record
	saved, err := s.chapterItemRepo.GetByChapterAndItem(chapterID, itemID)
	if err != nil {
		return ci, nil
	}
	return saved, nil
}

// DeleteChapterItem 删除章节级物品覆盖（回退到项目级）
func (s *ItemService) DeleteChapterItem(chapterID, itemID uint) error {
	return s.chapterItemRepo.Delete(chapterID, itemID)
}

// ListEffectiveItems 获取章节的有效物品列表（章节级覆盖优先，不存在则用项目级）
func (s *ItemService) ListEffectiveItems(novelID uint, chapterID uint) ([]*EffectiveItem, error) {
	// 获取所有项目级物品
	items, err := s.itemRepo.ListByNovel(novelID)
	if err != nil {
		return nil, err
	}
	// 获取本章节的所有覆盖
	chapterItems, err := s.chapterItemRepo.ListByChapter(chapterID)
	if err != nil {
		chapterItems = nil // non-fatal
	}
	overrideMap := make(map[uint]*model.ChapterItem, len(chapterItems))
	for _, ci := range chapterItems {
		overrideMap[ci.ItemID] = ci
	}

	result := make([]*EffectiveItem, 0, len(items))
	for _, item := range items {
		ei := &EffectiveItem{
			Item:              *item,
			EffectiveLocation: item.Location,
			EffectiveOwner:    item.Owner,
		}
		if override, ok := overrideMap[item.ID]; ok {
			ei.ChapterOverride = override
			if override.Location != "" {
				ei.EffectiveLocation = override.Location
			}
			if override.Owner != "" {
				ei.EffectiveOwner = override.Owner
			}
		}
		result = append(result, ei)
	}
	return result, nil
}

// extractItemsFromContent 从章节内容中提取物品（纯 AI 提取，不操作 DB）
func (s *ItemService) extractItemsFromContent(
	tenantID, novelID uint,
	novelTitle, genre, content string,
	existingNames []string,
) ([]analysisItemJSON, error) {
	chItemsPrompt, err := renderPrompt("extract_chapter_items", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"ExistingNames": existingNames,
		"Content":       content,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_chapter_items: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_chapter_items", chItemsPrompt, "",
		StoryboardOverrides{MaxTokens: 8192})
	if err != nil {
		return nil, err
	}

	cleaned := extractJSON(strings.TrimSpace(result))
	var items []analysisItemJSON
	if err := json.Unmarshal([]byte(cleaned), &items); err != nil {
		// 部分恢复
		dec := json.NewDecoder(strings.NewReader(cleaned))
		if _, e := dec.Token(); e == nil {
			for dec.More() {
				var item analysisItemJSON
				if dec.Decode(&item) == nil && item.Name != "" {
					items = append(items, item)
				}
			}
		}
	}
	valid := items[:0]
	for _, it := range items {
		if it.Name != "" {
			valid = append(valid, it)
		}
	}
	return valid, nil
}

// AIExtractAllFromNovel 逐章并发提取物品：先并发 AI 提取，再统一去重、入库
func (s *ItemService) AIExtractAllFromNovel(tenantID, novelID uint) ([]*model.Item, error) {
	logger.Printf("[ItemService] AIExtractAllFromNovel: novelID=%d", novelID)
	if s.chapterRepo == nil {
		return nil, fmt.Errorf("chapter repository not configured")
	}
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
	if err != nil {
		return nil, fmt.Errorf("load chapters: %w", err)
	}

	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	// 已有物品名单（用于 AI prompt 去重提示）
	existing, _ := s.itemRepo.ListByNovel(novelID)
	existingNames := make([]string, 0, len(existing))
	byName := make(map[string]*model.Item, len(existing))
	for _, it := range existing {
		existingNames = append(existingNames, it.Name)
		byName[strings.ToLower(it.Name)] = it
	}

	// 过滤有内容的章节（最多 10 章）
	const maxChapters = 10
	const concurrency = 3
	var candidates []*model.Chapter
	for _, ch := range chapters {
		if ch.Content != "" || ch.Summary != "" {
			candidates = append(candidates, ch)
			if len(candidates) >= maxChapters {
				break
			}
		}
	}
	if len(candidates) == 0 {
		logger.Printf("[ItemService] AIExtractAllFromNovel: novelID=%d no chapter content, skip", novelID)
		return nil, nil
	}

	// 并发提取（纯 AI 调用，不操作 DB）
	type chResult struct {
		items []analysisItemJSON
		err   error
	}
	results := make([]chResult, len(candidates))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, ch := range candidates {
		wg.Add(1)
		go func(idx int, c *model.Chapter) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content := c.Content
			if content == "" {
				content = c.Summary
			}
			items, err := s.extractItemsFromContent(tenantID, novelID, novelTitle, novelGenre, content, existingNames)
			results[idx] = chResult{items, err}
		}(i, ch)
	}
	wg.Wait()

	// 合并：统计每个物品出现在多少章节，只保留 ≥2 章的物品
	type itemEntry struct {
		item      analysisItemJSON
		chapterCt int
	}
	itemMap := make(map[string]*itemEntry) // key = lowercase name
	for _, r := range results {
		if r.err != nil {
			logger.Printf("ItemService.AIExtractAllFromNovel: chapter extract error: %v", r.err)
			continue
		}
		seenThisChapter := make(map[string]bool)
		for _, it := range r.items {
			key := strings.ToLower(it.Name)
			if seenThisChapter[key] {
				continue
			}
			seenThisChapter[key] = true
			if e, ok := itemMap[key]; ok {
				e.chapterCt++
			} else {
				itemMap[key] = &itemEntry{item: it, chapterCt: 1}
			}
		}
	}
	// 已存在 DB 的物品跳过，新物品只保留出现在 ≥2 章的
	var allItems []analysisItemJSON
	for key, e := range itemMap {
		if byName[key] != nil {
			continue // already in DB
		}
		if e.chapterCt >= 2 {
			allItems = append(allItems, e.item)
		}
	}
	logger.Printf("[ItemService] AIExtractAllFromNovel: chapters processed=%d, candidate items=%d, freq-filtered=%d", len(candidates), len(itemMap), len(allItems))

	// 统一入库（单线程，无竞争）
	validCat := map[string]bool{"weapon": true, "treasure": true, "tool": true, "document": true, "artifact": true, "other": true}
	upserted := make([]*model.Item, 0, len(allItems))
	for _, e := range allItems {
		if e.Name == "" {
			continue
		}
		if !validCat[e.Category] {
			e.Category = "other"
		}
		item := &model.Item{
			NovelID:      novelID,
			UUID:         uuid.New().String(),
			Name:         e.Name,
			Description:  buildItemDescription(e.Category, e.Appearance, e.Description),
			Location:     e.Location,
			Owner:        e.Owner,
			VisualPrompt: e.VisualPrompt,
			Status:       "active",
		}
		if err := s.itemRepo.Create(item); err != nil {
			logger.Printf("ItemService.AIExtractAllFromNovel: create %q: %v", e.Name, err)
			continue
		}
		upserted = append(upserted, item)
	}
	logger.Printf("[ItemService] AIExtractAllFromNovel done: novelID=%d created=%d", novelID, len(upserted))
	return upserted, nil
}

// AIExtractChapterItems 从单章内容中提取物品，写入 ink_item + ink_chapter_item
func (s *ItemService) AIExtractChapterItems(tenantID, novelID, chapterID uint) ([]*model.Item, error) {
	logger.Printf("[ItemService] AIExtractChapterItems: novelID=%d chapterID=%d", novelID, chapterID)
	chapter, err := s.chapterRepo.GetByID(chapterID)
	if err != nil {
		return nil, fmt.Errorf("chapter not found: %w", err)
	}
	content := chapter.Content
	if content == "" {
		content = chapter.Summary
	}
	if content == "" {
		return nil, fmt.Errorf("chapter has no content")
	}

	novelTitle := "本小说"
	novelGenre := ""
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			novelGenre = novel.Genre
		}
	}

	existing, _ := s.itemRepo.ListByNovel(novelID)
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, it := range existing {
		existingNames = append(existingNames, it.Name)
		existingNameSet[strings.ToLower(it.Name)] = true
	}

	chItemsPrompt2, err := renderPrompt("extract_chapter_items", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         novelGenre,
		"ExistingNames": existingNames,
		"Content":       content,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_chapter_items: %w", err)
	}

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "extract_chapter_items", chItemsPrompt2, "",
		StoryboardOverrides{MaxTokens: 8192})
	if err != nil {
		return nil, fmt.Errorf("AI extract chapter items: %w", err)
	}

	type itemJSON struct {
		Name         string `json:"name"`
		Category     string `json:"category"`
		Appearance   string `json:"appearance"`
		Description  string `json:"description"`
		Location     string `json:"location"`
		Owner        string `json:"owner"`
		VisualPrompt string `json:"visual_prompt"`
	}
	var items []itemJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if err := json.Unmarshal([]byte(cleaned), &items); err != nil {
		return nil, fmt.Errorf("parse items JSON: %w", err)
	}

	var created []*model.Item
	for _, it := range items {
		if it.Name == "" || existingNameSet[strings.ToLower(it.Name)] {
			continue
		}
		item := &model.Item{
			NovelID:      novelID,
			UUID:         uuid.New().String(),
			Name:         it.Name,
			Description:  buildItemDescription(it.Category, it.Appearance, it.Description),
			Location:     it.Location,
			Owner:        it.Owner,
			VisualPrompt: it.VisualPrompt,
			Status:       "active",
		}
		if e := s.itemRepo.Create(item); e != nil {
			logger.Printf("ItemService.AIExtractChapterItems: create %q: %v", it.Name, e)
			continue
		}
		existingNameSet[strings.ToLower(it.Name)] = true
		// 关联章节
		ci := &model.ChapterItem{
			ItemID:    item.ID,
			ChapterID: chapterID,
			NovelID:   novelID,
			Location:  it.Location,
			Owner:     it.Owner,
		}
		if e := s.chapterItemRepo.Upsert(ci); e != nil {
			logger.Printf("ItemService.AIExtractChapterItems: link chapter: %v", e)
		}
		created = append(created, item)
	}
	logger.Printf("[ItemService] AIExtractChapterItems done: chapterID=%d created=%d", chapterID, len(created))
	return created, nil
}

// buildItemDescription 将外观和叙事说明合并为统一描述字段。
// appearance 始终置于首段，description 紧随其后。
func buildItemDescription(category, appearance, description string) string {
	var parts []string
	if appearance != "" {
		parts = append(parts, "【外观】"+appearance)
	}
	if description != "" {
		parts = append(parts, description)
	} else if category != "" {
		parts = append(parts, category)
	}
	return strings.Join(parts, "\n")
}
