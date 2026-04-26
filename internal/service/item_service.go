package service

import (
	"context"
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
		ei := &EffectiveItem{Item: *item}
		if override, ok := overrideMap[item.ID]; ok {
			ei.ChapterOverride = override
			// 章节级优先
			if override.Location != "" {
				ei.EffectiveLocation = override.Location
			} else {
				ei.EffectiveLocation = item.Location
			}
			if override.Owner != "" {
				ei.EffectiveOwner = override.Owner
			} else {
				ei.EffectiveOwner = item.Owner
			}
		} else {
			ei.EffectiveLocation = item.Location
			ei.EffectiveOwner = item.Owner
		}
		result = append(result, ei)
	}
	return result, nil
}
