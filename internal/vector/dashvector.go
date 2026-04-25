package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DashVectorStore 阿里云 DashVector 向量数据库实现
// API 文档: https://help.aliyun.com/zh/dashvector/
type DashVectorStore struct {
	endpoint string // e.g. https://vrs-cn-xxx.dashvector.cn-hangzhou.aliyuncs.com
	apiKey   string
	client   *http.Client
}

func NewDashVectorStore(endpoint, apiKey string) *DashVectorStore {
	return &DashVectorStore{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// dashResponse DashVector 统一响应结构
type dashResponse struct {
	Code      int             `json:"code"`
	RequestID string          `json:"request_id"`
	Message   string          `json:"message"`
	Output    json.RawMessage `json:"output"`
}

func (s *DashVectorStore) doRequest(ctx context.Context, method, path string, body interface{}) (*dashResponse, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}

	url := s.endpoint + "/v1" + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("dashvector-auth-token", s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("dashvector server error: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result dashResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("dashvector parse response failed: %w", err)
	}

	if result.Code != 0 {
		return nil, fmt.Errorf("dashvector error %d: %s", result.Code, result.Message)
	}

	return &result, nil
}

func (s *DashVectorStore) HealthCheck(ctx context.Context) error {
	_, err := s.doRequest(ctx, "GET", "/collections", nil)
	return err
}

// Store 插入/更新向量 (upsert)
// POST /v1/collections/{collection}/docs
func (s *DashVectorStore) Store(ctx context.Context, req *StoreRequest) (*StoreResponse, error) {
	body := map[string]interface{}{
		"docs": []map[string]interface{}{
			{
				"id":     req.ID,
				"vector": req.Vector,
				"fields": req.Payload,
			},
		},
	}

	path := fmt.Sprintf("/collections/%s/docs", req.Collection)
	_, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("dashvector store failed: %w", err)
	}

	return &StoreResponse{
		ID:     req.ID,
		Status: "stored",
	}, nil
}

// Search 向量相似搜索
// POST /v1/collections/{collection}/query
func (s *DashVectorStore) Search(ctx context.Context, req *SearchRequest) ([]*SearchResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	body := map[string]interface{}{
		"vector":         req.Vector,
		"topk":           limit,
		"include_vector": false,
	}

	if len(req.Filters) > 0 {
		body["filter"] = buildDashVectorFilter(req.Filters)
	}

	path := fmt.Sprintf("/collections/%s/query", req.Collection)
	resp, err := s.doRequest(ctx, "POST", path, body)
	if err != nil {
		return nil, fmt.Errorf("dashvector search failed: %w", err)
	}

	// output 是数组
	var hits []struct {
		ID     string                 `json:"id"`
		Score  float32                `json:"score"`
		Fields map[string]interface{} `json:"fields"`
	}
	if err := json.Unmarshal(resp.Output, &hits); err != nil {
		return nil, fmt.Errorf("dashvector search parse failed: %w", err)
	}

	results := make([]*SearchResult, 0, len(hits))
	for _, h := range hits {
		if req.MinScore > 0 && h.Score < req.MinScore {
			continue
		}
		results = append(results, &SearchResult{
			ID:      h.ID,
			Score:   h.Score,
			Payload: h.Fields,
		})
	}

	return results, nil
}

// Delete 删除向量
// DELETE /v1/collections/{collection}/docs  body: {"ids": [...]}
func (s *DashVectorStore) Delete(ctx context.Context, id string) error {
	// DashVector 的 Delete 需要 collection 名称，但接口只传 id
	// 此处默认使用 knowledge_base，与 KnowledgeBaseVector 保持一致
	body := map[string]interface{}{
		"ids": []string{id},
	}
	_, err := s.doRequest(ctx, "DELETE", "/collections/knowledge_base/docs", body)
	return err
}

// Get 获取单条向量
// GET /v1/collections/{collection}/docs/{id}
func (s *DashVectorStore) Get(ctx context.Context, id string) (*VectorItem, error) {
	path := fmt.Sprintf("/collections/knowledge_base/docs/%s", id)
	resp, err := s.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("dashvector get failed: %w", err)
	}

	var doc struct {
		ID     string                 `json:"id"`
		Vector []float32              `json:"vector"`
		Fields map[string]interface{} `json:"fields"`
	}
	if err := json.Unmarshal(resp.Output, &doc); err != nil {
		return nil, fmt.Errorf("dashvector get parse failed: %w", err)
	}

	return &VectorItem{
		ID:      doc.ID,
		Vector:  doc.Vector,
		Payload: doc.Fields,
	}, nil
}

// buildDashVectorFilter 将 map 过滤条件转为 DashVector SQL-like 过滤字符串
// e.g. {"novel_id": 1, "type": "plot"} → "novel_id = 1 AND type = 'plot'"
func buildDashVectorFilter(filters map[string]interface{}) string {
	parts := make([]string, 0, len(filters))
	for k, v := range filters {
		switch val := v.(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%s = '%s'", k, val))
		case int, int32, int64, uint, uint32, uint64, float32, float64:
			parts = append(parts, fmt.Sprintf("%s = %v", k, val))
		case bool:
			if val {
				parts = append(parts, fmt.Sprintf("%s = true", k))
			} else {
				parts = append(parts, fmt.Sprintf("%s = false", k))
			}
		default:
			parts = append(parts, fmt.Sprintf("%s = %v", k, val))
		}
	}
	return strings.Join(parts, " AND ")
}
