package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/inkframe/inkframe-backend/internal/ai"
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

// StoreKnowledge 存储知识（含向量化）
func (s *KnowledgeService) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase) error {
	// 存储到数据库
	if err := s.kbRepo.Create(kb); err != nil {
		return err
	}

	// 向量化并存入向量库
	if s.vectorStore != nil && s.aiClient != nil {
		text := kb.Title + " " + kb.Content
		vec, err := s.aiClient.Embed(ctx, text)
		if err != nil {
			log.Printf("KnowledgeService.StoreKnowledge: embed error for kb %d: %v", kb.ID, err)
			// 不因为向量化失败就整体失败，降级处理
		} else {
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
					log.Printf("KnowledgeService.StoreKnowledge: vector store error for kb %d: %v", kb.ID, storeErr)
				}
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
		log.Printf("KnowledgeService.SearchKnowledge: vector search failed, fallback to keyword: %v", err)
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

// ExtractAndStorePlotPoints 提取并存储剧情点
func (s *KnowledgeService) ExtractAndStorePlotPoints(ctx context.Context, chapter *model.Chapter, aiClient ai.AIProvider) error {
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
			Type:    "plot_point",
			Title:   pp.Type + ": " + pp.Description[:min(50, len(pp.Description))],
			Content: pp.Description,
			Tags:    string(charJSON),
			NovelID: &chapter.NovelID,
		}

		s.StoreKnowledge(ctx, kb)
	}

	return nil
}
