package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/inkframe/inkframe-backend/internal/logger"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// EffectiveItem 有效道具（合并项目级和章节级覆盖）
type EffectiveItem struct {
	model.Item
	ChapterOverride   *model.ChapterItem `json:"chapter_override,omitempty"`
	EffectiveLocation string             `json:"effective_location"`
	EffectiveOwner    string             `json:"effective_owner"`
}

// ItemService 道具服务
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

// CreateItem 创建项目级道具。
// novel_id+name 有唯一索引（uniq_item_novel_name），而删除道具是软删除（deleted_at 置位，
// 行仍占用这个唯一索引）——如果不检查就直接 Create，删除后用同名重新创建会撞唯一索引报
// MySQL 1062 错误。这里先查一次（含软删除记录）：命中软删除记录就恢复并用新请求覆盖字段，
// 命中活跃记录则返回明确的重名错误，而不是让原始 SQL 错误往上抛。
func (s *ItemService) CreateItem(novelID uint, req *model.CreateItemRequest) (*model.Item, error) {
	if existing, err := s.itemRepo.FindByNovelAndNameUnscoped(novelID, req.Name); err == nil && existing != nil {
		if !existing.DeletedAt.Valid {
			return nil, fmt.Errorf("道具「%s」已存在", req.Name)
		}
		if err := s.itemRepo.RestoreByID(existing.ID); err != nil {
			return nil, fmt.Errorf("restore soft-deleted item: %w", err)
		}
		existing.DeletedAt.Valid = false
		existing.Location = req.Location
		existing.Owner = req.Owner
		existing.VisualPrompt = req.VisualPrompt
		// 清空旧的图片字段：用户是在创建一个"新"道具（只是复用了同名的旧行），不应该带着
		// 已删除道具残留的参考图/生成图，否则画面和新填的描述对不上。
		existing.ImageURL = ""
		existing.ReferenceImageURL = ""
		return existing, s.itemRepo.Update(existing)
	}

	item := &model.Item{
		NovelID:      novelID,
		UUID:         uuid.New().String(),
		Name:         req.Name,
		Location:     req.Location,
		Owner:        req.Owner,
		VisualPrompt: req.VisualPrompt,
	}
	return item, s.itemRepo.Create(item)
}

// GetItem 获取道具详情
func (s *ItemService) GetItem(id uint) (*model.Item, error) {
	return s.itemRepo.GetByID(id)
}

// ListItems 列出项目下所有道具
func (s *ItemService) ListItems(novelID uint) ([]*model.Item, error) {
	return s.itemRepo.ListByNovel(novelID)
}

// ListItemsPaged 分页列出项目下的道具，返回数据列表和总数
func (s *ItemService) ListItemsPaged(novelID uint, page, pageSize int) ([]*model.Item, int64, error) {
	return s.itemRepo.ListByNovelPaged(novelID, page, pageSize)
}

// UpdateItem 更新道具
func (s *ItemService) UpdateItem(id uint, req *model.UpdateItemRequest) (*model.Item, error) {
	item, err := s.itemRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}
	if req.Name != "" {
		item.Name = req.Name
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
	return item, s.itemRepo.Update(item)
}

// BatchDeleteItems 批量删除道具，仅删除属于指定小说的道具
func (s *ItemService) BatchDeleteItems(novelID uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.itemRepo.BatchDeleteByNovel(novelID, ids)
}

// DeleteItem 删除道具及其所有章节覆盖记录
func (s *ItemService) DeleteItem(id uint) error {
	if err := s.itemRepo.DeleteChapterItemsByItem(id); err != nil {
		return err
	}
	return s.itemRepo.Delete(id)
}

// GenerateItemImage 为道具生成图像
// generateItemImageCore is the shared AI call for item image generation.
// It builds the prompt, filters the reference URL to HTTP(S) only, sets up storage context,
// and calls the AI. Used by both GenerateItemImage and BatchGenerateImages.
func (s *ItemService) generateItemImageCore(ctx context.Context, tenantID uint, item *model.Item, provider, novelTitle, imageStyle string) (string, error) {
	prompt := item.VisualPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("%s，奇幻道具插画，精细细节，概念艺术", item.Name)
	}
	aiRefURL := item.ReferenceImageURL
	if !strings.HasPrefix(aiRefURL, "http://") && !strings.HasPrefix(aiRefURL, "https://") {
		aiRefURL = ""
	}
	if novelTitle != "" {
		ctx = WithImageStorageHint(ctx, ImageStorageHint{NovelTitle: novelTitle})
	}
	return s.aiService.GenerateCharacterThreeView(ctx, tenantID, provider, prompt+itemRefFormatRules, aiRefURL, imageStyle, "", "")
}

// itemRefFormatRules 是道具参考图的版式+规则文案，拼在 item.VisualPrompt（外观描述）之后。
const itemRefFormatRules = "，格式：道具设定图，横版4:3。画面只展示同一道具的四个视角：一个最能体现外观特征的主视角，以及正面、侧面、背面三视图。主视角主体最大最清晰，三视图完整展示道具整体造型与结构。纯白背景。" +
	"规则：禁止出现任何文字、字母、数字、标注、说明文字、材质色条、尺寸线、箭头、局部细节特写面板或爆炸分解图。道具在所有视角中必须保持完全一致。无环境元素，无角色，不添加水印。"

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
			imageStyle = novel.AIConfig.ImageStyle
		}
	}
	url, err := s.generateItemImageCore(context.Background(), tenantID, item, provider, novelTitle, imageStyle)
	if err != nil {
		return nil, fmt.Errorf("generate image failed: %w", err)
	}
	item.ImageURL = url
	return item, s.itemRepo.Update(item)
}

// AIExtractFromNovel 使用 AI 从章节内容中提取道具（按 novel_id+name upsert）
// BatchGenerateImages 批量为小说的道具生成图像。
// force=false：跳过已有图片的道具；force=true：全量重新生成（风格变更时使用）。
// 并发度由 AIService.imageSem 统一管控（系统设置 image_concurrency）。
func (s *ItemService) BatchGenerateImages(tenantID, novelID uint, provider string, force bool, progressFn func(int)) (succeeded, failed int, err error) {
	items, err := s.itemRepo.ListByNovel(novelID)
	if err != nil {
		return 0, 0, fmt.Errorf("list items: %w", err)
	}

	var todo []*model.Item
	for _, it := range items {
		if force || it.ImageURL == "" {
			todo = append(todo, it)
		}
	}
	total := len(todo)

	var novelTitle, imageStyle string
	if s.novelRepo != nil {
		if novel, e := s.novelRepo.GetByID(novelID); e == nil {
			novelTitle = novel.Title
			imageStyle = novel.AIConfig.ImageStyle
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
				logger.Errorf("[ItemService] BatchGenerateImages: item %d (%s) failed: %v", item.ID, item.Name, genErr)
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
				logger.Errorf("[ItemService] BatchGenerateImages: save item %d: %v", item.ID, saveErr)
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

// GenerateChapterImages 仅为本章绑定的选定道具生成图像，不影响该小说的其他道具。
// itemIDs 与 novelID 做交集校验，避免跨小说/租户的越权生成。
func (s *ItemService) GenerateChapterImages(tenantID, novelID uint, itemIDs []uint, provider string, progressFn func(int)) (succeeded, failed int, err error) {
	all, e := s.itemRepo.ListByNovel(novelID)
	if e != nil {
		return 0, 0, fmt.Errorf("list items: %w", e)
	}
	idSet := make(map[uint]bool, len(itemIDs))
	for _, id := range itemIDs {
		idSet[id] = true
	}
	var items []*model.Item
	for _, it := range all {
		if idSet[it.ID] {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return 0, 0, nil
	}
	total := len(items)

	var novelTitle, imageStyle string
	if s.novelRepo != nil {
		if novel, e2 := s.novelRepo.GetByID(novelID); e2 == nil {
			novelTitle = novel.Title
			imageStyle = novel.AIConfig.ImageStyle
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int

	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			url, genErr := s.generateItemImageCore(context.Background(), tenantID, item, provider, novelTitle, imageStyle)
			mu.Lock()
			done++
			cur := done
			if genErr != nil {
				logger.Errorf("[ItemService] GenerateChapterImages: item %d (%s) failed: %v", item.ID, item.Name, genErr)
				failed++
			} else {
				item.ImageURL = url
				if saveErr := s.itemRepo.Update(item); saveErr != nil {
					logger.Errorf("[ItemService] GenerateChapterImages: save item %d: %v", item.ID, saveErr)
					failed++
				} else {
					succeeded++
				}
			}
			mu.Unlock()
			if progressFn != nil && total > 0 {
				progressFn(cur * 99 / total)
			}
		}()
	}
	wg.Wait()
	logger.Printf("[ItemService] GenerateChapterImages: novelID=%d succeeded=%d failed=%d", novelID, succeeded, failed)
	return succeeded, failed, nil
}

func (s *ItemService) AIExtractFromNovel(ctx context.Context, tenantID, novelID uint) ([]*model.Item, error) {
	chapters, err := s.chapterRepo.ListByNovelWithContent(novelID)
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
	if s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelTitle = novel.Title
			novelGenre = novel.Meta.Genre
		}
	}
	if summariesText == "" {
		summariesText = fmt.Sprintf("这是一部%s类型的小说《%s》，请根据类型惯例设计主要道具道具。", novelGenre, novelTitle)
	}

	existing, _ := s.itemRepo.ListByNovel(novelID)
	byName := make(map[string]*model.Item, len(existing))
	for _, it := range existing {
		byName[it.Name] = it
	}

	existingJSON := marshalExistingNames(existing, func(it *model.Item) any {
		return struct {
			Name string `json:"name"`
		}{it.Name}
	})

	// 使用与分析流程相同的富格式 extract_items.j2
	itemsPrompt, err := renderPrompt("extract_items", map[string]interface{}{
		"NovelTitle": novelTitle,
		"Genre":      novelGenre,
		"Summaries":  summariesText,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_items: %w", err)
	}
	if existingJSON != "" {
		itemsPrompt += "\n\n注意：已有道具如下，必须复用原名，不得改名或重复创建：\n" + existingJSON
	}

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "extract_items", itemsPrompt, "",
		StoryboardOverrides{})
	if err != nil {
		return nil, fmt.Errorf("AI extraction failed: %w", err)
	}

	var extracted []analysisItemJSON
	if err := json.Unmarshal([]byte(extractJSON(strings.TrimSpace(result))), &extracted); err != nil {
		logger.Errorf("ItemService.AIExtractFromNovel: parse error: %v, raw: %.200s", err, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	upserted := make([]*model.Item, 0, len(extracted))
	for _, e := range extracted {
		if e.Name == "" {
			continue
		}
		if it, ok := byName[e.Name]; ok {
			// 更新：用 AI 数据填充空缺字段
			var changed bool
			if v, ok := fillIfEmpty(it.Location, e.Location); ok { it.Location = v; changed = true }
			if v, ok := fillIfEmpty(it.Owner, e.Owner); ok { it.Owner = v; changed = true }
			if v, ok := fillIfEmpty(it.VisualPrompt, s.aiService.FilterPrompt(e.VisualPrompt)); ok { it.VisualPrompt = v; changed = true }
			if !changed {
				upserted = append(upserted, it)
				continue
			}
			if err := s.itemRepo.Update(it); err != nil {
				logger.Errorf("ItemService.AIExtractFromNovel: update %s: %v", e.Name, err)
				continue
			}
			upserted = append(upserted, it)
		} else {
			// Deduplication: skip if an item with the same title already exists for this novel.
			if existing, err := s.itemRepo.GetByTitleAndNovel(e.Name, novelID); err == nil && existing != nil {
				upserted = append(upserted, existing)
				continue
			}
			item := &model.Item{
				NovelID:      novelID,
				UUID:         uuid.New().String(),
				Name:         e.Name,
				Location:     e.Location,
				Owner:        e.Owner,
				VisualPrompt: s.aiService.FilterPrompt(e.VisualPrompt),
			}
			if err := s.itemRepo.Create(item); err != nil {
				logger.Errorf("ItemService.AIExtractFromNovel: create %s: %v", e.Name, err)
				continue
			}
			upserted = append(upserted, item)
		}
	}
	return upserted, nil
}

// UpsertChapterItem 创建或更新章节级道具覆盖
func (s *ItemService) UpsertChapterItem(novelID, chapterID, itemID uint, req *model.UpsertChapterItemRequest) (*model.ChapterItem, error) {
	// Validate that the base item belongs to the novel before writing the override.
	item, err := s.itemRepo.GetByID(itemID)
	if err != nil || item.NovelID != novelID {
		return nil, fmt.Errorf("item does not belong to novel")
	}
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

// DeleteChapterItem 删除章节级道具覆盖（回退到项目级）
func (s *ItemService) DeleteChapterItem(chapterID, itemID uint) error {
	return s.chapterItemRepo.Delete(chapterID, itemID)
}

// ListEffectiveItems 获取章节的有效道具列表。
// 只返回已通过 AI 提取或手动绑定（ink_chapter_item 有记录）的道具，
// 章节级覆盖字段（location/owner）优先于项目级默认值。
func (s *ItemService) ListEffectiveItems(novelID uint, chapterID uint) ([]*EffectiveItem, error) {
	// 只取本章绑定的 ChapterItem 记录
	chapterItems, err := s.chapterItemRepo.ListByChapter(chapterID)
	if err != nil {
		return nil, err
	}
	if len(chapterItems) == 0 {
		return []*EffectiveItem{}, nil
	}

	// 收集需要查询的 item ID
	itemIDs := make([]uint, 0, len(chapterItems))
	ciMap := make(map[uint]*model.ChapterItem, len(chapterItems))
	for _, ci := range chapterItems {
		itemIDs = append(itemIDs, ci.ItemID)
		ciMap[ci.ItemID] = ci
	}

	// 批量获取项目级道具
	items, err := s.itemRepo.ListByIDs(itemIDs)
	if err != nil {
		return nil, err
	}

	result := make([]*EffectiveItem, 0, len(items))
	for _, item := range items {
		ei := &EffectiveItem{
			Item:              *item,
			EffectiveLocation: item.Location,
			EffectiveOwner:    item.Owner,
		}
		if ci, ok := ciMap[item.ID]; ok {
			ei.ChapterOverride = ci
			if ci.Location != "" {
				ei.EffectiveLocation = ci.Location
			}
			if ci.Owner != "" {
				ei.EffectiveOwner = ci.Owner
			}
		}
		result = append(result, ei)
	}
	return result, nil
}

// extractItemsFromContent 从章节内容中提取道具（纯 AI 提取，不操作 DB）
// extractItemsFromContent 单章道具提取的共享核心：渲染 extract_chapter_items.j2、调用 LLM、解析
// JSON。同时供 AIExtractAllFromNovel（分析流水线，逐章并发调用，不落库，由调用方合并去重后
// 统一入库）和 AIExtractChapterItems（章节页面手动触发，单章调用，自己落库）复用——此前这两处
// 各自写了一份几乎相同的渲染+调用+解析代码，只有 userPrompt/MaxTokens 等细节不同。
func (s *ItemService) extractItemsFromContent(
	ctx context.Context,
	tenantID, novelID uint,
	novelTitle, genre, content, userPrompt string,
	existingNames []string,
) ([]analysisItemJSON, error) {
	chItemsPrompt, err := renderPrompt("extract_chapter_items", map[string]interface{}{
		"NovelTitle":    novelTitle,
		"Genre":         genre,
		"ExistingNames": existingNames,
		"Content":       content,
		"UserPrompt":    userPrompt,
	})
	if err != nil {
		return nil, fmt.Errorf("render extract_chapter_items: %w", err)
	}

	result, err := s.aiService.GenerateWithProviderCtx(ctx, tenantID, novelID, "extract_chapter_items", chItemsPrompt, "",
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

// AIExtractAllFromNovel 逐章并发提取道具：先并发 AI 提取，再统一去重、入库
func (s *ItemService) AIExtractAllFromNovel(ctx context.Context, tenantID, novelID uint) ([]*model.Item, error) {
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
			novelGenre = novel.Meta.Genre
		}
	}

	// 已有道具名单（用于 AI prompt 去重提示）
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
			select {
			case <-ctx.Done():
				results[idx] = chResult{err: ctx.Err()}
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			content := c.Content
			if content == "" {
				content = c.Summary
			}
			items, err := s.extractItemsFromContent(ctx, tenantID, novelID, novelTitle, novelGenre, content, "", existingNames)
			results[idx] = chResult{items, err}
		}(i, ch)
	}
	wg.Wait()

	// 合并：统计每个道具出现在多少章节，只保留 ≥2 章的道具
	type itemEntry struct {
		item      analysisItemJSON
		chapterCt int
	}
	itemMap := make(map[string]*itemEntry) // key = lowercase name
	for _, r := range results {
		if r.err != nil {
			if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
				logger.Warnf("[ItemService.AIExtractAllFromNovel] novelID=%d chapter extraction cancelled: %v", novelID, r.err)
			} else {
				logger.Errorf("[ItemService.AIExtractAllFromNovel] novelID=%d chapter extract error: %v", novelID, r.err)
			}
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
	// 已存在 DB 的道具跳过，新道具只保留出现在 ≥2 章的
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
	upserted := make([]*model.Item, 0, len(allItems))
	for _, e := range allItems {
		if e.Name == "" {
			continue
		}
		item := &model.Item{
			NovelID:      novelID,
			UUID:         uuid.New().String(),
			Name:         e.Name,
			Location:     e.Location,
			Owner:        e.Owner,
			VisualPrompt: e.VisualPrompt,
		}
		if err := s.itemRepo.Create(item); err != nil {
			logger.Errorf("ItemService.AIExtractAllFromNovel: create %q: %v", e.Name, err)
			continue
		}
		upserted = append(upserted, item)
	}
	logger.Printf("[ItemService] AIExtractAllFromNovel done: novelID=%d created=%d", novelID, len(upserted))
	return upserted, nil
}

// AIExtractChapterItems 从单章内容中提取道具，写入 ink_item + ink_chapter_item
func (s *ItemService) AIExtractChapterItems(tenantID, novelID, chapterID uint, userPrompt string) ([]*model.Item, error) {
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
			novelGenre = novel.Meta.Genre
		}
	}

	existing, _ := s.itemRepo.ListByNovel(novelID)
	existingNames := make([]string, 0, len(existing))
	existingNameSet := make(map[string]bool, len(existing))
	for _, it := range existing {
		existingNames = append(existingNames, it.Name)
		existingNameSet[strings.ToLower(it.Name)] = true
	}

	// 渲染+调用 LLM+解析 JSON 复用 extractItemsFromContent（与分析流水线的
	// AIExtractAllFromNovel 共享同一份实现，见该函数上的注释）。
	items, err := s.extractItemsFromContent(context.Background(), tenantID, novelID, novelTitle, novelGenre, content, userPrompt, existingNames)
	if err != nil {
		return nil, fmt.Errorf("AI extract chapter items: %w", err)
	}
	logger.Printf("[ItemService] AIExtractChapterItems: chapterID=%d AI returned %d items", chapterID, len(items))

	var created []*model.Item
	for _, it := range items {
		if it.Name == "" || existingNameSet[strings.ToLower(it.Name)] {
			continue
		}
		item := &model.Item{
			NovelID:      novelID,
			UUID:         uuid.New().String(),
			Name:         it.Name,
			Location:     it.Location,
			Owner:        it.Owner,
			VisualPrompt: s.aiService.FilterPrompt(it.VisualPrompt),
		}
		if e := s.itemRepo.Create(item); e != nil {
			// existingNameSet 是函数开头一次性查出来的快照，如果另一次并发的章节提取
			// （不同章节同时提到同一个道具）在这之间抢先建了同名道具，这里会撞
			// uniq_item_novel_name 唯一索引报 1062——不是真正的失败，去把并发那边
			// 刚建好的道具找出来，继续关联到本章节，而不是直接丢弃这次提取结果。
			if isDuplicateKeyError(e) {
				if winner, findErr := s.itemRepo.GetByTitleAndNovel(it.Name, novelID); findErr == nil && winner != nil {
					item = winner
				} else {
					logger.Errorf("ItemService.AIExtractChapterItems: create %q raced but lookup failed: %v", it.Name, findErr)
					continue
				}
			} else {
				logger.Errorf("ItemService.AIExtractChapterItems: create %q: %v", it.Name, e)
				continue
			}
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
			logger.Errorf("ItemService.AIExtractChapterItems: link chapter: %v", e)
		}
		created = append(created, item)
	}
	logger.Printf("[ItemService] AIExtractChapterItems done: chapterID=%d created=%d", chapterID, len(created))
	return created, nil
}

// GenerateItemInfo 根据道具名称（及用户可选的草稿提示）AI 生成视觉提示词。
// 用于"添加道具"弹窗的一键填充：仅返回生成结果，不落库，由前端展示后随用户确认的表单一起走 CreateItem 创建。
func (s *ItemService) GenerateItemInfo(tenantID, novelID uint, name, userHint string) (visualPrompt string, err error) {
	novelTitle, novelGenre := novelPromptContext(s.novelRepo, novelID)

	rendered, tplErr := renderPrompt("generate_item_info", map[string]interface{}{
		"NovelTitle": novelTitle,
		"Genre":      novelGenre,
		"ItemName":   name,
		"UserHint":   userHint,
	})
	if tplErr != nil {
		return "", fmt.Errorf("render generate_item_info: %w", tplErr)
	}

	result, genErr := s.aiService.GenerateWithProvider(tenantID, novelID, "generate_item_info", rendered, "")
	if genErr != nil {
		return "", fmt.Errorf("AI generate item info: %w", genErr)
	}

	type itemInfoJSON struct {
		VisualPrompt string `json:"visual_prompt"`
	}
	var info itemInfoJSON
	cleaned := extractJSON(strings.TrimSpace(result))
	if parseErr := json.Unmarshal([]byte(cleaned), &info); parseErr != nil {
		logger.Errorf("[ItemService] GenerateItemInfo: parse error: %v, raw: %.300s", parseErr, result)
		return "", fmt.Errorf("parse item info JSON: %w", parseErr)
	}

	return s.aiService.FilterPrompt(info.VisualPrompt), nil
}
