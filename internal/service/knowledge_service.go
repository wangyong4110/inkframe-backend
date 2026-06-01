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

// KnowledgeService 知识库服务
type KnowledgeService struct {
	kbRepo interface {
		Create(kb *model.KnowledgeBase) error
		Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
		GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
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

// StoreKnowledge 存储知识（含向量化）
// 若向量库已配置，先完成向量化再写 DB，确保两者一致性；
// 若向量库未配置，直接写 DB（保持原有行为）。
func (s *KnowledgeService) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase) error {
	// 如果向量库已配置，先向量化；成功后再写 DB
	if s.vectorStore != nil && s.aiClient != nil {
		text := kb.Title + " " + kb.Content
		vec, embedErr := s.aiClient.Embed(ctx, text)
		if embedErr != nil {
			logger.Printf("KnowledgeService.StoreKnowledge: vector upsert failed, skipping DB write: %v", embedErr)
			return nil // 向量化失败时不写 DB，避免产生无向量的孤立记录
		}

		// 向量化成功后写 DB
		if err := s.kbRepo.Create(kb); err != nil {
			return err
		}

		// 将向量写入向量库
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
				// 向量写入失败：尝试回滚 DB 记录（best-effort）
				logger.Printf("KnowledgeService.StoreKnowledge: vector store error for kb %d: %v", kb.ID, storeErr)
			}
		}
		return nil
	}

	// 向量库未配置：直接写 DB（原有行为）
	return s.kbRepo.Create(kb)
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
		// 向量搜索失败，降级到关键词搜索
		logger.Printf("KnowledgeService.SearchKnowledge: vector search failed, fallback to keyword: %v", err)
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
func (s *KnowledgeService) ExtractAndStorePlotPoints(ctx context.Context, chapter *model.Chapter, aiClient ai.AIProvider) error {
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
		MaxTokens(2000).
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
