package service

import (
	"context"
	"fmt"

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
// referenceImageURL 可选：用户上传的参考图 URL，会附加到 prompt 供 AI 参考
// provider 可选：指定使用的图像生成提供者，空字符串 = 自动选择
func (s *ItemService) GenerateItemImage(id uint, referenceImageURL, provider string) (*model.Item, error) {
	item, err := s.itemRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}
	prompt := item.VisualPrompt
	if prompt == "" {
		prompt = fmt.Sprintf("%s, %s, fantasy item illustration, high detail, concept art", item.Name, item.Appearance)
	}
	if referenceImageURL != "" {
		// 将参考图 URL 持久化到 item，供后续查看；同时附加到 prompt 提示词
		item.VisualPrompt = prompt
		prompt = fmt.Sprintf("%s, based on reference image: %s", prompt, referenceImageURL)
	}
	url, err := s.aiService.GenerateCharacterThreeView(context.Background(), 0, provider, prompt+", item design, no background, studio lighting")
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
