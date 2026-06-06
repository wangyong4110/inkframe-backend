package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/vector"
)

// KnowledgeImportItem 知识批量导入的单个条目
type KnowledgeImportItem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Tags    string `json:"tags,omitempty"`
}

// KnowledgeService 知识库服务
type KnowledgeService struct {
	kbRepo interface {
		Create(kb *model.KnowledgeBase) error
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
		ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error)
		GetByID(id uint) (*model.KnowledgeBase, error)
		Update(kb *model.KnowledgeBase) error
		Delete(id uint) error
		ListBySourceChapter(novelID, chapterID uint) ([]*model.KnowledgeBase, error)
		DeleteBySourceChapter(novelID, chapterID uint) error
	}
	vectorStore *vector.StoreManager
	aiClient    ai.AIProvider
}

func NewKnowledgeService(
	kbRepo interface {
		Create(kb *model.KnowledgeBase) error
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
		ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error)
		GetByID(id uint) (*model.KnowledgeBase, error)
		Update(kb *model.KnowledgeBase) error
		Delete(id uint) error
		ListBySourceChapter(novelID, chapterID uint) ([]*model.KnowledgeBase, error)
		DeleteBySourceChapter(novelID, chapterID uint) error
	},
	vectorStore *vector.StoreManager,
	aiClient ai.AIProvider,
) *KnowledgeService {
	return &KnowledgeService{
		kbRepo:      kbRepo,
		vectorStore: vectorStore,
		aiClient:    aiClient,
	}
}

// GetByNovel 获取小说的所有知识条目
func (s *KnowledgeService) GetByNovel(ctx context.Context, novelID uint) ([]*model.KnowledgeBase, error) {
	return s.kbRepo.GetByNovel(novelID)
}

// GetByNovelPaged 分页获取小说的知识条目，返回数据、总数
func (s *KnowledgeService) GetByNovelPaged(ctx context.Context, novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error) {
	return s.kbRepo.ListByNovelPaged(novelID, page, pageSize)
}

// BulkImport 批量导入知识条目，跳过 title/content 为空的条目，返回成功入库数量
func (s *KnowledgeService) BulkImport(ctx context.Context, novelID uint, items []KnowledgeImportItem) (int, error) {
	imported := 0
	for _, item := range items {
		if item.Title == "" || item.Content == "" {
			continue
		}
		kb := &model.KnowledgeBase{
			Type:    item.Type,
			Title:   item.Title,
			Content: item.Content,
			Tags:    item.Tags,
			NovelID: &novelID,
		}
		if err := s.StoreKnowledge(ctx, kb); err != nil {
			logger.Printf("KnowledgeService.BulkImport: failed to store item %q: %v", item.Title, err)
			continue
		}
		imported++
	}
	return imported, nil
}

// StoreKnowledge 存储知识（含向量化）
// DB 是真实数据源（source of truth）：
//   - 总是先写 DB；失败则立即返回错误。
//   - 若向量库已配置，DB 写入成功后再写向量；向量写入失败仅记录警告，不影响返回值。
//   - 嵌入（embedding）失败时返回实际错误，不静默忽略。
func (s *KnowledgeService) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase) error {
	// 先写 DB（source of truth）
	if err := s.kbRepo.Create(kb); err != nil {
		return err
	}

	// 若向量库已配置，追加写向量（不影响主流程）
	if s.vectorStore != nil && s.aiClient != nil {
		text := kb.Title + " " + kb.Content
		vec, embedErr := s.aiClient.Embed(ctx, text)
		if embedErr != nil {
			// 嵌入失败：返回实际错误，让调用方感知（DB 记录已存在，数据不丢失）
			return fmt.Errorf("KnowledgeService.StoreKnowledge: embedding failed for kb %d: %w", kb.ID, embedErr)
		}

		store := s.vectorStore.DefaultStore()
		if store != nil {
			payload := map[string]interface{}{
				"id":       kb.ID,
				"type":     kb.Type,
				"title":    kb.Title,
				"content":  kb.Content,
				"novel_id": kb.NovelID,
			}
			_, storeErr := store.Store(ctx, &vector.StoreRequest{
				Collection: "knowledge_base",
				ID:         fmt.Sprintf("%d", kb.ID),
				Vector:     vec,
				Payload:    payload,
			})
			if storeErr != nil {
				// 向量写入失败：仅记录警告，DB 记录已成功，返回 nil
				logger.Printf("KnowledgeService.StoreKnowledge: vector store error for kb %d: %v", kb.ID, storeErr)
			}
		}
	}
	return nil
}

// SearchKnowledge 搜索知识（优先向量语义搜索，降级到关键词）
func (s *KnowledgeService) SearchKnowledge(ctx context.Context, query string, limit int, novelID *uint) ([]*model.KnowledgeBase, error) {
	// 尝试向量语义搜索
	if s.vectorStore != nil && s.aiClient != nil {
		vec, err := s.aiClient.Embed(ctx, query)
		if err == nil {
			store := s.vectorStore.DefaultStore()
			if store != nil {
				filters := map[string]interface{}{}
				if novelID != nil {
					filters["novel_id"] = *novelID
				}
				vectorResults, searchErr := store.Search(ctx, &vector.SearchRequest{
					Collection: "knowledge_base",
					Vector:     vec,
					Limit:      limit,
					Filters:    filters,
					MinScore:   0.6,
				})
				if searchErr == nil && len(vectorResults) > 0 {
					// 从向量结果中获取 KB 对象
					kbs := make([]*model.KnowledgeBase, 0, len(vectorResults))
					for _, vr := range vectorResults {
						if idVal, ok := vr.Payload["id"]; ok {
							var id uint
							switch v := idVal.(type) {
							case float64:
								id = uint(v)
							case uint:
								id = v
							}
							if id > 0 {
								kb, kbErr := s.kbRepo.GetByID(id)
								if kbErr == nil {
									// 3b: 过滤掉不属于目标小说的结果
									if kb.NovelID != nil && novelID != nil && *kb.NovelID != *novelID {
										continue
									}
									kbs = append(kbs, kb)
								}
							}
						}
					}
					if len(kbs) > 0 {
						return kbs, nil
					}
				}
			}
		}
		// 3a: 区分 embed 失败 vs 向量搜索无结果
		if err != nil {
			logger.Printf("KnowledgeService.SearchKnowledge: embed failed, fallback to keyword: %v", err)
		} else {
			logger.Printf("KnowledgeService.SearchKnowledge: vector search returned no results, fallback to keyword")
		}
	}

	// 关键词搜索降级
	results, err := s.kbRepo.Search(query, limit)
	if err != nil {
		return nil, err
	}

	if novelID != nil {
		var filtered []*model.KnowledgeBase
		for _, kb := range results {
			if kb.NovelID != nil && *kb.NovelID == *novelID {
				filtered = append(filtered, kb)
			}
		}
		results = filtered
	}

	return results, nil
}

// UpdateKnowledge 更新知识条目（标题/内容/标签）
func (s *KnowledgeService) UpdateKnowledge(ctx context.Context, id uint, novelID *uint, title, content, tags string) (*model.KnowledgeBase, error) {
	kb, err := s.kbRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("knowledge entry not found")
	}
	// Verify this entry belongs to the expected novel to prevent cross-novel access.
	if novelID != nil && (kb.NovelID == nil || *kb.NovelID != *novelID) {
		return nil, fmt.Errorf("knowledge entry does not belong to the specified novel")
	}
	if title != "" {
		kb.Title = title
	}
	if content != "" {
		kb.Content = content
	}
	if tags != "" {
		kb.Tags = tags
	}
	if err := s.kbRepo.Update(kb); err != nil {
		return nil, err
	}
	return kb, nil
}

// DeleteKnowledge 删除单条知识条目
func (s *KnowledgeService) DeleteKnowledge(ctx context.Context, id uint, novelID *uint) error {
	kb, err := s.kbRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("knowledge entry not found")
	}
	if novelID != nil && (kb.NovelID == nil || *kb.NovelID != *novelID) {
		return fmt.Errorf("knowledge entry does not belong to the specified novel")
	}
	return s.kbRepo.Delete(id)
}

// ExtractAndStorePlotPoints 提取并存储剧情点
// 每次运行前先清除该章节的旧记录，避免重复（replace-on-rerun 语义）
// aiClient 为 nil 时使用服务内部的 s.aiClient
func (s *KnowledgeService) ExtractAndStorePlotPoints(ctx context.Context, chapter *model.Chapter, aiClient ai.AIProvider) error {
	if aiClient == nil {
		aiClient = s.aiClient
	}
	if aiClient == nil {
		return fmt.Errorf("ExtractAndStorePlotPoints: no AI provider available")
	}
	// 先删除向量存储中该章节的旧记录，再删除 DB 记录
	if s.vectorStore != nil {
		store := s.vectorStore.DefaultStore()
		if store != nil {
			existing, _ := s.kbRepo.ListBySourceChapter(chapter.NovelID, chapter.ID)
			for _, kb := range existing {
				ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
				if delErr := store.Delete(ctx2, fmt.Sprintf("%d", kb.ID)); delErr != nil {
					logger.Printf("ExtractAndStorePlotPoints: vector delete kb %d failed: %v", kb.ID, delErr)
				}
				cancel()
			}
		}
	}
	// 清除该章节的旧剧情点记录
	if err := s.kbRepo.DeleteBySourceChapter(chapter.NovelID, chapter.ID); err != nil {
		logger.Printf("ExtractAndStorePlotPoints: cleanup failed: %v", err)
	}
	// 使用 AI 提取剧情点
	prompt := fmt.Sprintf(`从以下章节内容中提取关键剧情点，返回JSON数组格式：
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

	req := ai.NewGenerateRequestBuilder().
		UserMessage(prompt).
		Temperature(0.3).
		Build()

	resp, err := aiClient.Generate(ctx, req)
	if err != nil {
		return err
	}

	// 解析结果
	var result struct {
		PlotPoints []struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Characters  []string `json:"characters"`
			Locations   []string `json:"locations"`
		} `json:"plot_points"`
	}

	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return err
	}

	// 存储剧情点
	for _, pp := range result.PlotPoints {
		charJSON, _ := json.Marshal(pp.Characters)

		kb := &model.KnowledgeBase{
			Type:            "plot_point",
			Title:           pp.Type + ": " + pp.Description[:min(50, len(pp.Description))],
			Content:         pp.Description,
			Tags:            string(charJSON),
			NovelID:         &chapter.NovelID,
			SourceChapterID: &chapter.ID,
		}

		if err := s.StoreKnowledge(ctx, kb); err != nil {
			logger.Printf("ExtractAndStorePlotPoints: store failed: %v", err)
		}
	}

	return nil
}
