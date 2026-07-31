package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
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

	// EnsureCollection 确保指定 collection 存在（不存在则以给定维度创建），已存在则是 no-op。
	// 幂等；调用方应缓存"已确认存在"的结果，避免每次写入都发一次请求。
	// 不保证纠正维度不匹配——如果 collection 已存在但维度与调用方期望的不同，后续 Store 调用
	// 会在向量库层面报错，调用方需要据此判断是否是维度冲突（例如租户切换了不同的 embedding 模型）。
	EnsureCollection(ctx context.Context, name string, dimension int) error
}

// StoreRequest 存储请求
type StoreRequest struct {
	Collection string                 `json:"collection"`
	ID         string                 `json:"id"`
	Vector     []float32              `json:"vector"`
	Payload    map[string]interface{} `json:"payload"`
}

// StoreResponse 存储响应
type StoreResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// SearchRequest 搜索请求
type SearchRequest struct {
	Collection string                 `json:"collection"`
	Query      string                 `json:"query"`     // 文本查询（会自动向量化）
	Vector     []float32              `json:"vector"`    // 向量查询
	Limit      int                    `json:"limit"`     // 返回数量
	Filters    map[string]interface{} `json:"filters"`   // 过滤条件
	MinScore   float32                `json:"min_score"` // 最小相似度
}

// SearchResult 搜索结果
type SearchResult struct {
	ID      string                 `json:"id"`
	Score   float32                `json:"score"`
	Payload map[string]interface{} `json:"payload"`
}

// VectorItem 向量项
type VectorItem struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector"`
	Payload map[string]interface{} `json:"payload"`
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

// DefaultStore 默认向量存储（按注册名称字母序取第一个，确保结果确定）
func (m *StoreManager) DefaultStore() VectorStore {
	if len(m.stores) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m.stores))
	for k := range m.stores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m.stores[keys[0]]
}

// StoreAndSearch 存储并搜索（一步到位）
func (m *StoreManager) StoreAndSearch(ctx context.Context, collection string, text string, payload map[string]interface{}, limit int) ([]*SearchResult, error) {
	store := m.DefaultStore()
	if store == nil {
		return nil, fmt.Errorf("no vector store available")
	}

	if m.embedder == nil {
		return nil, fmt.Errorf("embedder not configured")
	}

	// 向量化
	vector, err := m.embedder.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	// 存储
	id := fmt.Sprintf("%d", time.Now().UnixNano())
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

// QdrantStore Qdrant 向量数据库实现（真实 HTTP API）
type QdrantStore struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewQdrantStore(endpoint, apiKey string) *QdrantStore {
	return &QdrantStore{
		endpoint: endpoint,
		apiKey:   apiKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (s *QdrantStore) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}

	url := s.endpoint + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		httpReq.Header.Set("api-key", s.apiKey)
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

func (s *QdrantStore) HealthCheck(ctx context.Context) error {
	_, status, err := s.doRequest(ctx, "GET", "/", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("qdrant health check failed: status %d", status)
	}
	return nil
}

// EnsureCollection checks GET /collections/{name}; if it 404s, creates it via
// PUT /collections/{name} with the given vector size (cosine distance).
func (s *QdrantStore) EnsureCollection(ctx context.Context, name string, dimension int) error {
	_, status, err := s.doRequest(ctx, "GET", "/collections/"+name, nil)
	if err != nil {
		return fmt.Errorf("qdrant collection check failed: %w", err)
	}
	if status == http.StatusOK {
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("qdrant collection check failed: status %d", status)
	}
	body := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     dimension,
			"distance": "Cosine",
		},
	}
	_, createStatus, err := s.doRequest(ctx, "PUT", "/collections/"+name, body)
	if err != nil {
		return fmt.Errorf("qdrant collection create failed: %w", err)
	}
	if createStatus != http.StatusOK {
		return fmt.Errorf("qdrant collection create failed: status %d", createStatus)
	}
	return nil
}

// Store 通过 Qdrant PUT /collections/{collection}/points 存储向量
func (s *QdrantStore) Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error) {
	point := map[string]interface{}{
		"id":      req.ID,
		"vector":  req.Vector,
		"payload": req.Payload,
	}
	body := map[string]interface{}{
		"points": []interface{}{point},
	}

	path := fmt.Sprintf("/collections/%s/points", req.Collection)
	respBody, status, err := s.doRequest(ctx, "PUT", path, body)
	if err != nil {
		return nil, fmt.Errorf("qdrant store request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("qdrant store failed: status %d, body: %s", status, string(respBody))
	}

	return &StoreResponse{
		ID:     req.ID,
		Status: "stored",
	}, nil
}

// Search 通过 Qdrant POST /collections/{collection}/points/search 语义搜索
func (s *QdrantStore) Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error) {
	body := map[string]interface{}{
		"vector":          req.Vector,
		"limit":           req.Limit,
		"with_payload":    true,
		"score_threshold": req.MinScore,
	}

	if len(req.Filters) > 0 {
		mustConditions := []map[string]interface{}{}
		for k, v := range req.Filters {
			mustConditions = append(mustConditions, map[string]interface{}{
				"key":   k,
				"match": map[string]interface{}{"value": v},
			})
		}
		body["filter"] = map[string]interface{}{
			"must": mustConditions,
		}
	}

	path := fmt.Sprintf("/collections/%s/points/search", req.Collection)
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("qdrant search request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("qdrant search failed: status %d, body: %s", status, string(respBody))
	}

	var qdrantResp struct {
		Result []struct {
			ID      interface{}            `json:"id"`
			Score   float32                `json:"score"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}

	if err := json.Unmarshal(respBody, &qdrantResp); err != nil {
		return nil, fmt.Errorf("qdrant search parse failed: %w", err)
	}

	results := make([]*SearchResult, 0, len(qdrantResp.Result))
	for _, r := range qdrantResp.Result {
		results = append(results, &SearchResult{
			ID:      fmt.Sprintf("%v", r.ID),
			Score:   r.Score,
			Payload: r.Payload,
		})
	}
	return results, nil
}

// Delete 通过 Qdrant POST /collections/{collection}/points/delete 删除向量
func (s *QdrantStore) Delete(ctx context.Context, id string) error {
	body := map[string]interface{}{
		"points": []string{id},
	}
	path := fmt.Sprintf("/collections/%s/points/delete", "knowledge_base")
	_, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("qdrant delete failed: status %d", status)
	}
	return nil
}

// Get 通过 Qdrant GET /collections/{collection}/points/{id} 获取向量
func (s *QdrantStore) Get(ctx context.Context, id string) (*VectorItem, error) {
	path := fmt.Sprintf("/collections/%s/points/%s", "knowledge_base", id)
	respBody, status, err := s.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("qdrant get failed: status %d", status)
	}

	var result struct {
		Result struct {
			ID      interface{}            `json:"id"`
			Vector  []float32              `json:"vector"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	return &VectorItem{
		ID:      fmt.Sprintf("%v", result.Result.ID),
		Vector:  result.Result.Vector,
		Payload: result.Result.Payload,
	}, nil
}

// ChromaStore Chroma 向量数据库实现（真实 HTTP API，对接 Chroma OSS v2 REST API）
// 官方文档：https://docs.trychroma.com/reference/chroma-api
//
// v2 API 采用 tenant/database/collection 三级路径，且记录级操作（add/query/get/delete）
// 都要求 collection 的 UUID，不能直接用集合名称——本实现在每次操作前按名称
// get-or-create 解析出 UUID 并缓存，避免每次调用都多一次往返。
// tenant/database 固定使用 Chroma 的默认值 "default_tenant"/"default_database"
// （本地/自托管单租户部署无需自定义，对应 docker run -p 8000:8000 chromadb/chroma 的默认状态）。
type ChromaStore struct {
	endpoint string
	apiKey   string
	tenant   string
	database string
	client   *http.Client

	collMu    sync.Mutex
	collCache map[string]string // collection 名称 -> UUID
}

func NewChromaStore(endpoint, apiKey string) *ChromaStore {
	return &ChromaStore{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		tenant:   "default_tenant",
		database: "default_database",
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		collCache: make(map[string]string),
	}
}

func (s *ChromaStore) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}

	url := s.endpoint + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		httpReq.Header.Set("X-Chroma-Token", s.apiKey)
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

func (s *ChromaStore) HealthCheck(ctx context.Context) error {
	_, status, err := s.doRequest(ctx, "GET", "/api/v2/healthcheck", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("chroma health check failed: status %d", status)
	}
	return nil
}

// EnsureCollection is effectively a no-op: Store/Search/Delete already resolve (get-or-create)
// the collection lazily via resolveCollectionID. dimension is accepted for interface parity
// but unused — Chroma infers dimension from the first vector it receives, not upfront.
func (s *ChromaStore) EnsureCollection(ctx context.Context, name string, dimension int) error {
	_, err := s.resolveCollectionID(ctx, name)
	return err
}

// resolveCollectionID 按名称 get-or-create 一个 collection，返回其 UUID（内存缓存，避免重复往返）。
func (s *ChromaStore) resolveCollectionID(ctx context.Context, name string) (string, error) {
	s.collMu.Lock()
	if id, ok := s.collCache[name]; ok {
		s.collMu.Unlock()
		return id, nil
	}
	s.collMu.Unlock()

	path := fmt.Sprintf("/api/v2/tenants/%s/databases/%s/collections", s.tenant, s.database)
	body := map[string]interface{}{
		"name":          name,
		"get_or_create": true,
	}
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return "", fmt.Errorf("chroma create-or-get collection request failed: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("chroma create-or-get collection failed: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("chroma create-or-get collection parse failed: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("chroma create-or-get collection %q returned empty id", name)
	}

	s.collMu.Lock()
	s.collCache[name] = result.ID
	s.collMu.Unlock()
	return result.ID, nil
}

// Store 通过 Chroma POST /collections/{id}/add 存储向量。
// 注意：Chroma metadata（即 req.Payload）只支持标量值（string/int/float/bool），
// 不支持嵌套 object/array，调用方需确保传入扁平结构。
func (s *ChromaStore) Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error) {
	collID, err := s.resolveCollectionID(ctx, req.Collection)
	if err != nil {
		return nil, err
	}

	body := map[string]interface{}{
		"ids":        []string{req.ID},
		"embeddings": [][]float32{req.Vector},
	}
	if len(req.Payload) > 0 {
		body["metadatas"] = []map[string]interface{}{req.Payload}
	}

	path := fmt.Sprintf("/api/v2/tenants/%s/databases/%s/collections/%s/add", s.tenant, s.database, collID)
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("chroma store request failed: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("chroma store failed: status %d, body: %s", status, string(respBody))
	}

	return &StoreResponse{
		ID:     req.ID,
		Status: "stored",
	}, nil
}

// Search 通过 Chroma POST /collections/{id}/query 语义搜索。
func (s *ChromaStore) Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error) {
	collID, err := s.resolveCollectionID(ctx, req.Collection)
	if err != nil {
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	body := map[string]interface{}{
		"query_embeddings": [][]float32{req.Vector},
		"n_results":        limit,
		"include":          []string{"metadatas", "distances"},
	}
	if where := buildChromaWhere(req.Filters); where != nil {
		body["where"] = where
	}

	path := fmt.Sprintf("/api/v2/tenants/%s/databases/%s/collections/%s/query", s.tenant, s.database, collID)
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("chroma search request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("chroma search failed: status %d, body: %s", status, string(respBody))
	}

	// query 的响应按"每个 query_embedding 一行"嵌套一层数组；这里只发了 1 个向量，取第 0 行。
	var chromaResp struct {
		IDs       [][]string                 `json:"ids"`
		Distances [][]float32                `json:"distances"`
		Metadatas [][]map[string]interface{} `json:"metadatas"`
	}
	if err := json.Unmarshal(respBody, &chromaResp); err != nil {
		return nil, fmt.Errorf("chroma search parse failed: %w", err)
	}
	if len(chromaResp.IDs) == 0 {
		return []*SearchResult{}, nil
	}
	ids := chromaResp.IDs[0]
	var distances []float32
	if len(chromaResp.Distances) > 0 {
		distances = chromaResp.Distances[0]
	}
	var metadatas []map[string]interface{}
	if len(chromaResp.Metadatas) > 0 {
		metadatas = chromaResp.Metadatas[0]
	}

	results := make([]*SearchResult, 0, len(ids))
	for i, id := range ids {
		// Chroma 返回的是距离（distance，越小越相似），Qdrant 返回的是相似度分数（score，越大越相似）。
		// 用 1/(1+distance) 转成 0~1 的相似度分数，使 req.MinScore 阈值判断在两种 provider 下语义一致。
		var score float32
		if len(distances) > i {
			score = 1.0 / (1.0 + distances[i])
		}
		if req.MinScore > 0 && score < req.MinScore {
			continue
		}
		var payload map[string]interface{}
		if len(metadatas) > i {
			payload = metadatas[i]
		}
		results = append(results, &SearchResult{
			ID:      id,
			Score:   score,
			Payload: payload,
		})
	}
	return results, nil
}

// Delete 通过 Chroma POST /collections/{id}/delete 删除向量。
// VectorStore 接口的 Delete 不带 collection 参数，与 QdrantStore 保持一致，固定操作 "knowledge_base" 集合。
func (s *ChromaStore) Delete(ctx context.Context, id string) error {
	collID, err := s.resolveCollectionID(ctx, "knowledge_base")
	if err != nil {
		return err
	}
	body := map[string]interface{}{
		"ids": []string{id},
	}
	path := fmt.Sprintf("/api/v2/tenants/%s/databases/%s/collections/%s/delete", s.tenant, s.database, collID)
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return fmt.Errorf("chroma delete request failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("chroma delete failed: status %d, body: %s", status, string(respBody))
	}
	return nil
}

// Get 通过 Chroma POST /collections/{id}/get 按 ID 获取向量。
// VectorStore 接口的 Get 不带 collection 参数，与 QdrantStore 保持一致，固定操作 "knowledge_base" 集合。
func (s *ChromaStore) Get(ctx context.Context, id string) (*VectorItem, error) {
	collID, err := s.resolveCollectionID(ctx, "knowledge_base")
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{
		"ids":     []string{id},
		"include": []string{"embeddings", "metadatas"},
	}
	path := fmt.Sprintf("/api/v2/tenants/%s/databases/%s/collections/%s/get", s.tenant, s.database, collID)
	respBody, status, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("chroma get request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("chroma get failed: status %d, body: %s", status, string(respBody))
	}

	var result struct {
		IDs        []string                 `json:"ids"`
		Embeddings [][]float32              `json:"embeddings"`
		Metadatas  []map[string]interface{} `json:"metadatas"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("chroma get parse failed: %w", err)
	}
	if len(result.IDs) == 0 {
		return nil, nil // 未找到：与 QdrantStore.Get 对 404 的行为保持一致
	}

	item := &VectorItem{ID: result.IDs[0]}
	if len(result.Embeddings) > 0 {
		item.Vector = result.Embeddings[0]
	}
	if len(result.Metadatas) > 0 {
		item.Payload = result.Metadatas[0]
	}
	return item, nil
}

// buildChromaWhere 把通用的 filters map 转成 Chroma 的 where 过滤语法。
// 单个过滤条件用 {key: value} 简写（等价于 $eq）；多个条件必须用 $and 显式包裹
// （Chroma 不支持在 where 顶层直接放多个 key）。按 key 排序保证输出确定、可测试。
func buildChromaWhere(filters map[string]interface{}) map[string]interface{} {
	if len(filters) == 0 {
		return nil
	}
	if len(filters) == 1 {
		for k, v := range filters {
			return map[string]interface{}{k: v}
		}
	}
	keys := make([]string, 0, len(filters))
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	conds := make([]map[string]interface{}, 0, len(keys))
	for _, k := range keys {
		conds = append(conds, map[string]interface{}{k: filters[k]})
	}
	return map[string]interface{}{"$and": conds}
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
