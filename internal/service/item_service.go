package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

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

// CreateItem 创建项目级物品
func (s *ItemService) CreateItem(novelID uint, req *model.CreateItemRequest) (*model.Item, error) {
	item := &model.Item{
		NovelID:      novelID,
		UUID:         uuid.New().String(),
		Name:         req.Name,
		Category:     req.Category,
		Description:  req.Description,
		Appearance:   req.Appearance,
		Location:     req.Location,
		Owner:        req.Owner,
		Significance: req.Significance,
		Abilities:    req.Abilities,
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
	if req.Category != "" {
		item.Category = req.Category
	}
	if req.Description != "" {
		item.Description = req.Description
	}
	if req.Appearance != "" {
		item.Appearance = req.Appearance
	}
	if req.Location != "" {
		item.Location = req.Location
	}
	if req.Owner != "" {
		item.Owner = req.Owner
	}
	if req.Significance != "" {
		item.Significance = req.Significance
	}
	if req.Abilities != "" {
		item.Abilities = req.Abilities
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
// referenceImageURL 可选：用户上传的参考图 URL（已存入 OSS），作为 AI 参考图使用
// provider 可选：指定使用的图像生成提供者，空字符串 = 自动选择
func (s *ItemService) GenerateItemImage(tenantID, id uint, referenceImageURL, provider string) (*model.Item, error) {
	item, err := s.itemRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}
	prompt := item.VisualPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("%s，%s，奇幻物品插画，精细细节，概念艺术", item.Name, item.Appearance)
	}
	// Persist new reference URL; fall back to previously saved one.
	if referenceImageURL != "" {
		item.ReferenceImageURL = referenceImageURL
	}
	// Only absolute HTTP(S) URLs can be fetched by remote AI APIs; skip local/relative paths.
	aiRefURL := item.ReferenceImageURL
	if !strings.HasPrefix(aiRefURL, "http://") && !strings.HasPrefix(aiRefURL, "https://") {
		aiRefURL = ""
	}
	if aiRefURL != "" {
		log.Printf("GenerateItemImage: item=%d using reference image %s", id, aiRefURL)
	} else {
		log.Printf("GenerateItemImage: item=%d no valid reference image, generating without reference", id)
	}
	url, err := s.aiService.GenerateCharacterThreeView(context.Background(), tenantID, provider, prompt+"，物品设计，白色背景，摄影棚光效", aiRefURL)
	if err != nil {
		return nil, fmt.Errorf("generate image failed: %w", err)
	}
	item.ImageURL = url
	return item, s.itemRepo.Update(item)
}

// AIExtractFromNovel 使用 AI 从章节内容中提取物品（按 novel_id+name upsert）
func (s *ItemService) AIExtractFromNovel(tenantID, novelID uint) ([]*model.Item, error) {
	chapters, err := s.chapterRepo.ListByNovel(novelID)
	if err != nil {
		return nil, fmt.Errorf("failed to load chapters: %w", err)
	}
	novelContent := collectContent(chapters, len(chapters), 8000)
	if novelContent == "" {
		return nil, fmt.Errorf("no chapter content available")
	}

	existing, _ := s.itemRepo.ListByNovel(novelID)
	byName := make(map[string]*model.Item, len(existing))
	for _, it := range existing {
		byName[it.Name] = it
	}

	existingJSON := marshalExistingNames(existing, func(it *model.Item) any {
		return struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		}{it.Name, it.Category}
	})
	var existingSection string
	if existingJSON != "" {
		existingSection = "\n已有物品（必须使用完全相同的 name 字段，不得改名或创建重名物品）：" + existingJSON + "\n仅当小说中出现了已有物品之外的重要物品时，才新增条目。\n"
	}

	prompt := fmt.Sprintf(`请从以下小说内容中提取出现的重要物品/道具，以 JSON 数组格式返回：
[
  {"name":"物品名","category":"weapon/armor/treasure/artifact/tool/document/consumable/other","description":"物品描述","owner":"持有者","significance":"重要性"}
]
%s只返回 JSON 数组，不要添加任何说明文字。
小说内容：%s`, existingSection, novelContent)

	result, err := s.aiService.GenerateWithProvider(tenantID, novelID, "item_extraction", prompt, "")
	if err != nil {
		return nil, fmt.Errorf("AI extraction failed: %w", err)
	}

	var extracted []struct {
		Name         string `json:"name"`
		Category     string `json:"category"`
		Description  string `json:"description"`
		Owner        string `json:"owner"`
		Significance string `json:"significance"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &extracted); err != nil {
		log.Printf("ItemService.AIExtractFromNovel: parse error: %v, raw: %.200s", err, result)
		return nil, fmt.Errorf("failed to parse AI response")
	}

	upserted := make([]*model.Item, 0, len(extracted))
	for _, e := range extracted {
		if e.Name == "" {
			continue
		}
		if it, ok := byName[e.Name]; ok {
			req := &model.UpdateItemRequest{
				Category: it.Category, Description: it.Description,
				Owner: it.Owner, Significance: it.Significance,
			}
			var changed bool
			if v, ok := fillIfEmpty(it.Category, e.Category); ok { req.Category = v; changed = true }
			if v, ok := fillIfEmpty(it.Description, e.Description); ok { req.Description = v; changed = true }
			if v, ok := fillIfEmpty(it.Owner, e.Owner); ok { req.Owner = v; changed = true }
			if v, ok := fillIfEmpty(it.Significance, e.Significance); ok { req.Significance = v; changed = true }
			if !changed {
				upserted = append(upserted, it)
				continue
			}
			updated, err := s.UpdateItem(it.ID, req)
			if err != nil {
				log.Printf("ItemService.AIExtractFromNovel: update %s: %v", e.Name, err)
				continue
			}
			upserted = append(upserted, updated)
		} else {
			item := &model.Item{
				NovelID: novelID, UUID: uuid.New().String(), Name: e.Name,
				Category: e.Category, Description: e.Description,
				Owner: e.Owner, Significance: e.Significance, Status: "active",
			}
			if err := s.itemRepo.Create(item); err != nil {
				log.Printf("ItemService.AIExtractFromNovel: create %s: %v", e.Name, err)
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
