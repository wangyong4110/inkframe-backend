package vector

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/inkframe/inkframe-backend/internal/model"
)

// VectorStore 向量存储接口
type VectorStore interface {
	// Store 存储向量
	Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error)

	// Search 搜索相似向量
	Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error)

	// Delete 删除向量
	Delete(ctx context.Context, id string) error

	// Get 获取向量
	Get(ctx context.Context, id string) (*VectorItem, error)

	// HealthCheck 健康检查
	HealthCheck(ctx context.Context) error
}

// StoreRequest 存储请求
type StoreRequest struct {
	Collection string                 `json:"collection"`
	ID        string                 `json:"id"`
	Vector    []float32              `json:"vector"`
	Payload   map[string]interface{} `json:"payload"`
}

// StoreResponse 存储响应
type StoreResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// SearchRequest 搜索请求
type SearchRequest struct {
	Collection string                 `json:"collection"`
	Query      string                 `json:"query"`       // 文本查询（会自动向量化）
	Vector     []float32              `json:"vector"`      // 向量查询
	Limit      int                    `json:"limit"`       // 返回数量
	Filters    map[string]interface{} `json:"filters"`     // 过滤条件
	MinScore   float32                `json:"min_score"`   // 最小相似度
}

// SearchResult 搜索结果
type SearchResult struct {
	ID     string                 `json:"id"`
	Score  float32                `json:"score"`
	Payload map[string]interface{} `json:"payload"`
}

// VectorItem 向量项
type VectorItem struct {
	ID      string                  `json:"id"`
	Vector  []float32               `json:"vector"`
	Payload map[string]interface{}  `json:"payload"`
}

// Embedder 向量化器接口
type Embedder interface {
	// Embed 向量化文本
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch 批量向量化
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// GetDimension 获取向量维度
	GetDimension() int
}

// StoreManager 向量存储管理器
type StoreManager struct {
	stores   map[string]VectorStore
	embedder Embedder
}

func NewStoreManager(embedder Embedder) *StoreManager {
	return &StoreManager{
		stores:   make(map[string]VectorStore),
		embedder: embedder,
	}
}

// RegisterStore 注册向量存储
func (m *StoreManager) RegisterStore(name string, store VectorStore) {
	m.stores[name] = store
}

// GetStore 获取向量存储
func (m *StoreManager) GetStore(name string) (VectorStore, error) {
	store, ok := m.stores[name]
	if !ok {
		return nil, fmt.Errorf("vector store not found: %s", name)
	}
	return store, nil
}

// DefaultStore 默认向量存储
func (m *StoreManager) DefaultStore() VectorStore {
	for _, store := range m.stores {
		return store
	}
	return nil
}

// StoreAndSearch 存储并搜索（一步到位）
func (m *StoreManager) StoreAndSearch(ctx context.Context, collection string, text string, payload map[string]interface{}, limit int) ([]*SearchResult, error) {
	store := m.DefaultStore()
	if store == nil {
		return nil, fmt.Errorf("no vector store available")
	}

	// 向量化
	vector, err := m.embedder.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	// 存储
	id := fmt.Sprintf("%d", timeNow().UnixNano())
	_, err = store.Store(ctx, &StoreRequest{
		Collection: collection,
		ID:         id,
		Vector:     vector,
		Payload:    payload,
	})
	if err != nil {
		return nil, err
	}

	// 搜索
	return store.Search(ctx, &SearchRequest{
		Collection: collection,
		Vector:     vector,
		Limit:      limit,
	})
}

// timeNow 获取当前时间
func timeNow() interface{ UnixNano() int64 } {
	return &timeWrapper{}
}

type timeWrapper struct{}

func (t *timeWrapper) UnixNano() int64 {
	return int64(0) // 实际使用 time.Now().UnixNano()
}

// QdrantStore Qdrant 向量数据库实现
type QdrantStore struct {
	endpoint string
	apiKey   string
}

func NewQdrantStore(endpoint, apiKey string) *QdrantStore {
	return &QdrantStore{
		endpoint: endpoint,
		apiKey:   apiKey,
	}
}

func (s *QdrantStore) HealthCheck(ctx context.Context) error {
	// 简化实现
	return nil
}

func (s *QdrantStore) Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error) {
	// 实现 Qdrant 存储逻辑
	// 实际需要调用 Qdrant REST API
	return &StoreResponse{
		ID:     req.ID,
		Status: "stored",
	}, nil
}

func (s *QdrantStore) Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error) {
	// 实现 Qdrant 搜索逻辑
	// 实际需要调用 Qdrant REST API
	return []*SearchResult{}, nil
}

func (s *QdrantStore) Delete(ctx context.Context, id string) error {
	// 实现 Qdrant 删除逻辑
	return nil
}

func (s *QdrantStore) Get(ctx context.Context, id string) (*VectorItem, error) {
	// 实现 Qdrant 获取逻辑
	return nil, nil
}

// ChromaStore Chroma 向量数据库实现
type ChromaStore struct {
	endpoint string
}

func NewChromaStore(endpoint string) *ChromaStore {
	return &ChromaStore{
		endpoint: endpoint,
	}
}

func (s *ChromaStore) HealthCheck(ctx context.Context) error {
	return nil
}

func (s *ChromaStore) Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error) {
	// 实现 Chroma 存储逻辑
	return &StoreResponse{
		ID:     req.ID,
		Status: "stored",
	}, nil
}

func (s *ChromaStore) Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error) {
	// 实现 Chroma 搜索逻辑
	return []*SearchResult{}, nil
}

func (s *ChromaStore) Delete(ctx context.Context, id string) error {
	return nil
}

func (s *ChromaStore) Get(ctx context.Context, id string) (*VectorItem, error) {
	return nil, nil
}

// KnowledgeBaseVector 知识库向量操作
type KnowledgeBaseVector struct {
	store VectorStore
}

func NewKnowledgeBaseVector(store VectorStore) *KnowledgeBaseVector {
	return &KnowledgeBaseVector{store: store}
}

// StoreKnowledge 存储知识
func (h *KnowledgeBaseVector) StoreKnowledge(ctx context.Context, kb *model.KnowledgeBase, vector []float32) error {
	payload := map[string]interface{}{
		"id":       kb.ID,
		"type":     kb.Type,
		"title":    kb.Title,
		"content":  kb.Content,
		"novel_id": kb.NovelID,
	}

	_, err := h.store.Store(ctx, &StoreRequest{
		Collection: "knowledge_base",
		ID:        fmt.Sprintf("%d", kb.ID),
		Vector:    vector,
		Payload:   payload,
	})

	return err
}

// SearchKnowledge 搜索知识
func (h *KnowledgeBaseVector) SearchKnowledge(ctx context.Context, query string, limit int, filters map[string]interface{}) ([]*SearchResult, error) {
	return h.store.Search(ctx, &SearchRequest{
		Collection: "knowledge_base",
		Query:      query,
		Limit:      limit,
		Filters:    filters,
	})
}

// CollectionManager 集合管理器
type CollectionManager struct {
	store VectorStore
}

func NewCollectionManager(store VectorStore) *CollectionManager {
	return &CollectionManager{store: store}
}

// CreateCollection 创建集合
func (m *CollectionManager) CreateCollection(ctx context.Context, name string, dimension int, description string) error {
	// 实际实现需要调用向量数据库 API
	return nil
}

// DeleteCollection 删除集合
func (m *CollectionManager) DeleteCollection(ctx context.Context, name string) error {
	return nil
}

// ListCollections 列出集合
func (m *CollectionManager) ListCollections(ctx context.Context) ([]string, error) {
	return []string{}, nil
}

// CollectionInfo 集合信息
type CollectionInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	VectorCount int    `json:"vector_count"`
	Dimension   int    `json:"dimension"`
}

// Helper Functions

// ParsePayload 解析载荷
func ParsePayload(data []byte) (map[string]interface{}, error) {
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SerializePayload 序列化载荷
func SerializePayload(payload map[string]interface{}) ([]byte, error) {
	return json.Marshal(payload)
}
