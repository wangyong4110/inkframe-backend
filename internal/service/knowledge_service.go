package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/vector"
	"github.com/redis/go-redis/v9"
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
	aiSvc       *AIService    // optional: used for per-model concurrency-controlled embedding
	cache       *redis.Client // optional: for cross-instance idempotency in ExtractAndStorePlotPoints
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

// WithRedis enables cross-instance idempotency in ExtractAndStorePlotPoints via Redis SETNX.
func (s *KnowledgeService) WithRedis(c *redis.Client) *KnowledgeService {
	s.cache = c
	return s
}

// WithAIService 注入 AIService，启用 per-provider 并发控制的向量嵌入。
func (s *KnowledgeService) WithAIService(svc *AIService) *KnowledgeService {
	s.aiSvc = svc
	return s
}

// embed 统一嵌入入口：优先走 aiSvc（含并发限流），回退到裸 aiClient。
func (s *KnowledgeService) embed(ctx context.Context, tenantID uint, text string) ([]float32, error) {
	if s.aiSvc != nil {
		return s.aiSvc.Embed(ctx, tenantID, text)
	}
	if s.aiClient != nil {
		return s.aiClient.Embed(ctx, text)
	}
	return nil, fmt.Errorf("no embedding provider available")
}

// GetByNovel 获取小说的所有知识条目
func (s *KnowledgeService) GetByNovel(ctx context.Context, novelID uint) ([]*model.KnowledgeBase, error) {
	return s.kbRepo.GetByNovel(novelID)
}

// GetByNovelPaged 分页获取小说的知识条目，返回数据、总数
func (s *KnowledgeService) GetByNovelPaged(ctx context.Context, novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error) {
	return s.kbRepo.ListByNovelPaged(novelID, page, pageSize)
}

// bulkImportConcurrency bounds how many items embed/store concurrently. StoreKnowledge's
// embedding call already goes through AIService's per-provider rate-limited slot queue (see
// acquireProviderSlot in ai_service.go), so firing this many at once just keeps that queue fed
// instead of paying embedding-API round-trip latency one item at a time.
const bulkImportConcurrency = 8

// BulkImport 批量导入知识条目，跳过 title/content 为空的条目，返回成功入库数量。
// 每条的 embedding + 向量写入是网络调用，逐条同步执行时延迟会随条目数线性叠加；这里用有界
// 并发（bulkImportConcurrency）把它们摊开跑，DB 写入和向量写入仍然是每条各自独立完成。
func (s *KnowledgeService) BulkImport(ctx context.Context, novelID uint, items []KnowledgeImportItem) (int, error) {
	var imported atomic.Int32
	sem := make(chan struct{}, bulkImportConcurrency)
	var wg sync.WaitGroup

	for _, item := range items {
		if item.Title == "" || item.Content == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(item KnowledgeImportItem) {
			defer wg.Done()
			defer func() { <-sem }()
			kb := &model.KnowledgeBase{
				Type:    item.Type,
				Title:   item.Title,
				Content: item.Content,
				Tags:    item.Tags,
				NovelID: &novelID,
			}
			if err := s.StoreKnowledge(ctx, kb); err != nil {
				logger.Errorf("KnowledgeService.BulkImport: failed to store item %q: %v", item.Title, err)
				return
			}
			imported.Add(1)
		}(item)
	}
	wg.Wait()
	return int(imported.Load()), nil
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
	return s.syncVector(ctx, kb)
}

// syncVector (重新)嵌入 kb.Title+kb.Content 并写入向量库，ID 与 kb.ID 一致。
// 写入前先尝试删除同 ID 的旧向量点：Qdrant 的 PUT /points 本身是 upsert，删不删都一样；
// 但 Chroma 的 /add 端点在 ID 已存在时会报错而不是覆盖，所以统一先删再写，三种后端下都能
// 得到"覆盖旧向量"的效果——和 ExtractAndStorePlotPoints 里 replace-on-rerun 用的是同一套手法。
// 调用方共用此方法，保证 create 和 update 路径的向量写入语义完全一致：
//   - 嵌入失败：返回错误（DB 记录已落地，数据不丢，但调用方能感知向量没同步）。
//   - 删除/写入向量失败：仅记录警告，不影响返回值。
func (s *KnowledgeService) syncVector(ctx context.Context, kb *model.KnowledgeBase) error {
	if s.vectorStore == nil || (s.aiClient == nil && s.aiSvc == nil) {
		return nil
	}
	store := s.vectorStore.DefaultStore()
	if store == nil {
		return nil
	}

	text := kb.Title + " " + kb.Content
	vec, embedErr := s.embed(ctx, kb.TenantID, text)
	if embedErr != nil {
		return fmt.Errorf("KnowledgeService: embedding failed for kb %d: %w", kb.ID, embedErr)
	}

	idStr := fmt.Sprintf("%d", kb.ID)
	if delErr := store.Delete(ctx, idStr); delErr != nil {
		logger.Errorf("KnowledgeService: vector delete kb %d failed (continuing to store): %v", kb.ID, delErr)
	}

	payload := map[string]interface{}{
		"id":       kb.ID,
		"type":     kb.Type,
		"title":    kb.Title,
		"content":  kb.Content,
		"novel_id": kb.NovelID,
	}
	if _, storeErr := store.Store(ctx, &vector.StoreRequest{
		Collection: "knowledge_base",
		ID:         idStr,
		Vector:     vec,
		Payload:    payload,
	}); storeErr != nil {
		// 向量写入失败：仅记录警告，DB 记录已成功，返回 nil
		logger.Errorf("KnowledgeService: vector store error for kb %d: %v", kb.ID, storeErr)
	}
	return nil
}

// SearchKnowledge 搜索知识（优先向量语义搜索，降级到关键词）
func (s *KnowledgeService) SearchKnowledge(ctx context.Context, query string, limit int, novelID *uint) ([]*model.KnowledgeBase, error) {
	// 尝试向量语义搜索
	if s.vectorStore != nil && (s.aiClient != nil || s.aiSvc != nil) {
		vec, err := s.embed(ctx, 0, query)
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
						metrics.KnowledgeSearchTotal.WithLabelValues("vector").Inc()
						return kbs, nil
					}
				}
			}
		}
		// 3a: 区分 embed 失败 vs 向量搜索无结果
		if err != nil {
			logger.Errorf("KnowledgeService.SearchKnowledge: embed failed, fallback to keyword: %v", err)
		} else {
			logger.Printf("KnowledgeService.SearchKnowledge: vector search returned no results, fallback to keyword")
		}
	}

	// 关键词搜索降级
	metrics.KnowledgeSearchTotal.WithLabelValues("keyword").Inc()
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
	textChanged := (title != "" && title != kb.Title) || (content != "" && content != kb.Content)
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
	// 嵌入文本是 Title+Content 拼接的，只有它们变了才需要重新向量化；仅改 tags 不必重新嵌入。
	// 这里只记警告、不把 syncVector 的错误当成本次更新失败：DB 已经改好了（编辑的核心目的已达成），
	// 向量暂时没跟上只是搜索结果会短暂陈旧，不该让调用方以为这次编辑没生效（handler 层目前把
	// UpdateKnowledge 的任何错误都映射成 404，若在这里返回错误会让一次成功的编辑显示为"未找到"）。
	if textChanged {
		if err := s.syncVector(ctx, kb); err != nil {
			logger.Errorf("KnowledgeService.UpdateKnowledge: vector sync failed for kb %d: %v", kb.ID, err)
		}
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
	extractStatus := "success"
	defer func() { metrics.KnowledgeExtractTotal.WithLabelValues(extractStatus).Inc() }()
	// 跨实例幂等性：心跳锁（60s base TTL）防止重复写入；实例崩溃后60s内自动释放。
	if s.cache != nil {
		lockKey := fmt.Sprintf("lock:kv:pp:%d", chapter.ID)
		lock, ok, lockErr := acquireDistLock(s.cache, lockKey, 60*time.Second)
		if lockErr != nil {
			logger.Errorf("KnowledgeService.ExtractAndStorePlotPoints: Redis lock error: %v", lockErr)
			// 非致命：继续执行（最多重复写入，不会丢数据）
		} else if !ok {
			logger.Printf("KnowledgeService.ExtractAndStorePlotPoints: chapter %d already processing by another instance, skip", chapter.ID)
			extractStatus = "skipped"
			return nil
		} else {
			defer lock.release()
		}
	}

	if aiClient == nil {
		aiClient = s.aiClient
	}
	if aiClient == nil {
		extractStatus = "error"
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
					logger.Errorf("ExtractAndStorePlotPoints: vector delete kb %d failed: %v", kb.ID, delErr)
				}
				cancel()
			}
		}
	}
	// 清除该章节的旧剧情点记录
	if err := s.kbRepo.DeleteBySourceChapter(chapter.NovelID, chapter.ID); err != nil {
		logger.Errorf("ExtractAndStorePlotPoints: cleanup failed: %v", err)
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

	var llmContent string
	if s.aiSvc != nil {
		// 走统一限流队列
		var genErr error
		llmContent, genErr = s.aiSvc.GenerateWithProviderCtx(ctx, 0, chapter.NovelID, "analysis", prompt, "",
			StoryboardOverrides{Temperature: 0.3})
		if genErr != nil {
			extractStatus = "error"
			return genErr
		}
	} else {
		// 降级：直接调用裸 provider（无限流）
		if aiClient == nil {
			aiClient = s.aiClient
		}
		req := ai.NewGenerateRequestBuilder().
			UserMessage(prompt).
			Temperature(0.3).
			Build()
		resp, genErr := aiClient.Generate(ctx, req)
		if genErr != nil {
			extractStatus = "error"
			return genErr
		}
		llmContent = resp.Content
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

	if err := json.Unmarshal([]byte(llmContent), &result); err != nil {
		extractStatus = "error"
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
			logger.Errorf("ExtractAndStorePlotPoints: store failed: %v", err)
		}
	}

	return nil
}
