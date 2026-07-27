package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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

const (
	// knowledgeVectorCollection is the single shared vector-store collection all knowledge base
	// entries (across every tenant/novel) are stored in. Isolation between tenants/novels relies
	// entirely on the novel_id payload filter applied in syncVector/SearchKnowledge, not on
	// separate collections — see the EnsureCollection call in syncVector for the multi-tenant
	// dimension caveat this implies (different tenants configuring different embedding models
	// would produce different vector dimensions in the same collection).
	knowledgeVectorCollection = "knowledge_base"

	// maxEmbedInputRunes caps the text sent to embed() as a defensive guard against embedding
	// API token limits (commonly ~8K tokens). This is a rune-count heuristic, not an exact
	// token count — most CJK tokenizers spend close to or more than 1 token per character, so
	// erring conservative here is safer than trying to match a specific tokenizer's math.
	// Content (mediumtext, up to 16MB) is truncated to fit; Title is always kept in full since
	// the DB schema already caps it at 255 chars.
	maxEmbedInputRunes = 4000
)

// buildEmbedText assembles the text passed to embed(), truncating Content (not Title) so the
// combined rune count stays within maxEmbedInputRunes. Without this, a long manually-imported
// worldview document or a verbose plot-point description would either get silently truncated
// server-side by the embedding provider (only the first N tokens actually influence the
// resulting vector) or rejected outright with a provider-specific "input too long" error that
// StoreKnowledge/UpdateKnowledge would surface verbatim, giving no hint that length is the issue.
func buildEmbedText(title, content string) string {
	budget := maxEmbedInputRunes - len([]rune(title)) - 1 // -1 for the joining space
	if budget < 0 {
		budget = 0
	}
	contentRunes := []rune(content)
	if len(contentRunes) > budget {
		content = string(contentRunes[:budget])
	}
	return title + " " + content
}

// knowledgeBaseRepo is the subset of *repository.KnowledgeBaseRepository that KnowledgeService
// depends on. Named (rather than duplicated as an anonymous interface literal in both the
// struct field and NewKnowledgeService's parameter, as this used to be) so adding a method
// only requires editing one place.
type knowledgeBaseRepo interface {
	Create(kb *model.KnowledgeBase) error
	Search(keyword string, limit int) ([]*model.KnowledgeBase, error)
	GetByNovel(novelID uint) ([]*model.KnowledgeBase, error)
	ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error)
	GetByID(id uint) (*model.KnowledgeBase, error)
	Update(kb *model.KnowledgeBase) error
	Delete(id uint) error
	ListBySourceChapter(novelID, chapterID uint) ([]*model.KnowledgeBase, error)
	DeleteBySourceChapter(novelID, chapterID uint) error
	IncrementUsageCount(id uint) error
}

// KnowledgeService 知识库服务
type KnowledgeService struct {
	kbRepo      knowledgeBaseRepo
	vectorStore *vector.StoreManager
	aiClient    ai.AIProvider
	aiSvc       *AIService    // optional: used for per-model concurrency-controlled embedding
	cache       *redis.Client // optional: for cross-instance idempotency in ExtractAndStorePlotPoints

	// ensureCollectionOnce/ensureCollectionErr memoize the one-time "does knowledge_base exist,
	// create it if not" check (see ensureCollection) so we don't round-trip to the vector store
	// on every single write — a missing/misconfigured collection is a deploy-time problem, not
	// a per-request one.
	ensureCollectionOnce sync.Once
	ensureCollectionErr  error

	// searchLimit/minScore are configurable (see config.KnowledgeBaseConfig) instead of the
	// hardcoded limit=3/MinScore=0.6 this used to have; 0 means "not configured, use the
	// built-in default" (see defaultSearchLimit/defaultMinScore constants below).
	searchLimit int
	minScore    float32
}

// Built-in defaults used when KnowledgeBaseConfig.SearchLimit/MinScore are left at zero
// (i.e. not set in config.yaml).
const (
	defaultSearchLimit = 3
	defaultMinScore    = 0.6
)

func NewKnowledgeService(
	kbRepo knowledgeBaseRepo,
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

// WithSearchConfig 设置检索的返回条数上限和语义搜索最小相似度阈值。
// 传 0 表示"使用内置默认值"（defaultSearchLimit/defaultMinScore），不会覆盖已有设置为 0。
func (s *KnowledgeService) WithSearchConfig(limit int, minScore float32) *KnowledgeService {
	s.searchLimit = limit
	s.minScore = minScore
	return s
}

// DefaultSearchLimit 返回配置的检索条数上限（未配置时回退到 defaultSearchLimit）。
// 供 ChapterService 等调用方在触发 knowledge_search 工具时使用，避免各处重复硬编码同一个值。
func (s *KnowledgeService) DefaultSearchLimit() int {
	if s.searchLimit > 0 {
		return s.searchLimit
	}
	return defaultSearchLimit
}

func (s *KnowledgeService) effectiveMinScore() float32 {
	if s.minScore > 0 {
		return s.minScore
	}
	return defaultMinScore
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

	text := buildEmbedText(kb.Title, kb.Content)
	vec, embedErr := s.embed(ctx, kb.TenantID, text)
	if embedErr != nil {
		return fmt.Errorf("KnowledgeService: embedding failed for kb %d: %w", kb.ID, embedErr)
	}

	// One-time-per-process check: does the collection exist? Create it if not. Failure here
	// is logged loudly but non-fatal — Store() below is the authoritative attempt, and its own
	// error (if any) already carries the vector store's specific reason (e.g. dimension
	// mismatch from a tenant using a different embedding model than whichever call happened
	// to create the collection first — see knowledgeVectorCollection's doc comment).
	s.ensureCollectionOnce.Do(func() {
		s.ensureCollectionErr = store.EnsureCollection(ctx, knowledgeVectorCollection, len(vec))
		if s.ensureCollectionErr != nil {
			logger.Errorf("KnowledgeService: collection %q missing and auto-create failed: %v — "+
				"检查向量库连接，或手动创建该 collection（维度需与当前 embedding 模型输出一致，本次为 %d 维）",
				knowledgeVectorCollection, s.ensureCollectionErr, len(vec))
		}
	})

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
		Collection: knowledgeVectorCollection,
		ID:         idStr,
		Vector:     vec,
		Payload:    payload,
	}); storeErr != nil {
		// 向量写入失败：仅记录警告，DB 记录已成功，返回 nil
		logger.Errorf("KnowledgeService: vector store error for kb %d: %v", kb.ID, storeErr)
		return nil
	}

	// 向量写入成功：记录本次嵌入内容的哈希和时间。目前没有代码读取这两个字段做决策——它们是为
	// 将来的维护任务预留的信号：例如批量扫描 VectorHash 是否与当前 Title+Content 的哈希一致，
	// 找出"DB 改了但因为某种原因向量没跟上"的条目并重新同步，或者按 EmbeddedAt 做定期刷新。
	hash := contentHash(text)
	if hash != kb.VectorHash || kb.EmbeddedAt == nil {
		now := time.Now()
		kb.VectorHash = hash
		kb.EmbeddedAt = &now
		if updErr := s.kbRepo.Update(kb); updErr != nil {
			logger.Errorf("KnowledgeService: failed to persist VectorHash/EmbeddedAt for kb %d: %v", kb.ID, updErr)
		}
	}
	return nil
}

// contentHash returns the hex-encoded SHA-256 of text — used only as a change-detection
// fingerprint (VectorHash), not for anything security-sensitive.
func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// SearchKnowledge 搜索知识：语义（向量）和关键词两路并行召回，再用 RRF（Reciprocal Rank
// Fusion）融合排序去重，而不是"语义搜索有结果就用语义，否则才做关键词"的二选一 fallback。
// 二选一的问题：语义相关但关键词没对上（或反过来）的条目，只要另一条路"恰好有结果"就会被
// 完全漏掉；混合检索能同时吃到两边的召回，同时命中两条路的条目会自然排到更靠前。
// 任一路失败（embed 出错、向量库不可用等）不影响另一路正常工作，只有两路都失败才报错。
func (s *KnowledgeService) SearchKnowledge(ctx context.Context, query string, limit int, novelID *uint) ([]*model.KnowledgeBase, error) {
	// 每路都多取一些候选（limit*2），融合排序后再截断到 limit，避免两路召回的条目重叠度低时
	// 结果凑不够 limit 条。
	overfetch := limit * 2
	if overfetch < limit {
		overfetch = limit // 防御 limit*2 溢出（limit 极大时），退化为不 overfetch
	}

	var vectorRanked []*model.KnowledgeBase // 按相似度从高到低排列
	if s.vectorStore != nil && (s.aiClient != nil || s.aiSvc != nil) {
		vec, err := s.embed(ctx, 0, buildEmbedText("", query))
		if err != nil {
			logger.Errorf("KnowledgeService.SearchKnowledge: embed failed, continuing with keyword-only: %v", err)
		} else if store := s.vectorStore.DefaultStore(); store != nil {
			filters := map[string]interface{}{}
			if novelID != nil {
				filters["novel_id"] = *novelID
			}
			vectorResults, searchErr := store.Search(ctx, &vector.SearchRequest{
				Collection: knowledgeVectorCollection,
				Vector:     vec,
				Limit:      overfetch,
				Filters:    filters,
				MinScore:   s.effectiveMinScore(),
			})
			if searchErr != nil {
				logger.Errorf("KnowledgeService.SearchKnowledge: vector search failed, continuing with keyword-only: %v", searchErr)
			}
			for _, vr := range vectorResults {
				id, ok := kbIDFromPayload(vr.Payload)
				if !ok {
					continue
				}
				kb, kbErr := s.kbRepo.GetByID(id)
				if kbErr != nil {
					continue
				}
				// 过滤掉不属于目标小说的结果
				if kb.NovelID != nil && novelID != nil && *kb.NovelID != *novelID {
					continue
				}
				vectorRanked = append(vectorRanked, kb)
			}
			if len(vectorRanked) > 0 {
				metrics.KnowledgeSearchTotal.WithLabelValues("vector").Inc()
			}
		}
	}

	// 关键词检索始终执行（不再是"仅在语义那路完全失败/零结果时才做"），用于混合排序召回。
	keywordResults, kwErr := s.kbRepo.Search(query, overfetch)
	if kwErr != nil {
		if len(vectorRanked) == 0 {
			// 两路都不可用（向量路要么没配置要么本次失败，关键词路直接报错）才真正报错。
			return nil, kwErr
		}
		logger.Errorf("KnowledgeService.SearchKnowledge: keyword search failed, returning vector-only results: %v", kwErr)
	}
	var keywordRanked []*model.KnowledgeBase
	for _, kb := range keywordResults {
		if novelID != nil && (kb.NovelID == nil || *kb.NovelID != *novelID) {
			continue
		}
		keywordRanked = append(keywordRanked, kb)
	}
	if len(keywordRanked) > 0 {
		metrics.KnowledgeSearchTotal.WithLabelValues("keyword").Inc()
	}

	merged := rrfMerge(vectorRanked, keywordRanked, limit)
	s.bumpUsageCount(merged)
	return merged, nil
}

// bumpUsageCount increments UsageCount for every entry actually returned to a caller, so
// usage_count reflects real retrieval hits instead of staying permanently at 0 (it was defined
// on the model and had a repo method to increment it, but nothing ever called that method).
// Fire-and-forget in the background: this is bookkeeping, not something the caller should wait
// on or fail the search over.
func (s *KnowledgeService) bumpUsageCount(results []*model.KnowledgeBase) {
	if len(results) == 0 {
		return
	}
	ids := make([]uint, len(results))
	for i, kb := range results {
		ids[i] = kb.ID
	}
	go func() {
		for _, id := range ids {
			if err := s.kbRepo.IncrementUsageCount(id); err != nil {
				logger.Errorf("KnowledgeService: IncrementUsageCount(%d) failed: %v", id, err)
			}
		}
	}()
}

// kbIDFromPayload extracts the "id" field from a vector search result payload. JSON-decoded
// numbers surface as float64; some backends may already give us a uint. Returns ok=false for
// anything else (missing/zero/wrong-typed id).
func kbIDFromPayload(payload map[string]interface{}) (uint, bool) {
	idVal, ok := payload["id"]
	if !ok {
		return 0, false
	}
	var id uint
	switch v := idVal.(type) {
	case float64:
		id = uint(v)
	case uint:
		id = v
	default:
		return 0, false
	}
	return id, id > 0
}

// rrfMerge combines two best-first-ranked result lists via Reciprocal Rank Fusion:
// score(doc) = Σ 1/(k + rank) over every list the doc appears in (rank is 0-based position in
// that list). k=60 is the constant used in the original RRF paper/TREC usage — it dampens how
// much a single list's rank swings the fused score, so a doc ranked #1 by keyword but absent
// from vector results doesn't automatically dominate. Docs present in BOTH lists accumulate
// score from both terms and naturally float to the top, which is the point of hybrid retrieval:
// reward agreement between semantic and lexical relevance without requiring it.
func rrfMerge(vectorRanked, keywordRanked []*model.KnowledgeBase, limit int) []*model.KnowledgeBase {
	const k = 60.0
	scores := make(map[uint]float64)
	items := make(map[uint]*model.KnowledgeBase)
	accumulate := func(ranked []*model.KnowledgeBase) {
		for rank, kb := range ranked {
			scores[kb.ID] += 1.0 / (k + float64(rank))
			items[kb.ID] = kb
		}
	}
	accumulate(vectorRanked)
	accumulate(keywordRanked)

	ids := make([]uint, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // 分数相同时按 ID 稳定排序，避免 map 遍历顺序导致结果抖动
	})
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	result := make([]*model.KnowledgeBase, 0, len(ids))
	for _, id := range ids {
		result = append(result, items[id])
	}
	return result
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
		kvLockKey := lockKey("kv", "pp", chapter.ID)
		lock, ok, lockErr := acquireDistLock(s.cache, kvLockKey, 60*time.Second)
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
		llmContent, genErr = s.aiSvc.GenerateWithProviderCtx(ctx, chapter.TenantID, "analysis", prompt)
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
