package vector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// These tests exercise each backend's HTTP client logic against a fake server standing in for
// the real Qdrant/Chroma/DashVector API — no real vector DB is reachable in this environment.
// They verify request shape (method/path/body) and response parsing, plus the specific
// EnsureCollection contract (added to fix the "knowledge_base collection is never
// auto-provisioned" gap): exists → no create call; missing → attempts creation with the given
// dimension.

// recordingHandler wraps an http.HandlerFunc and records every request's method+path+body for
// assertions, while still letting the test control what gets returned per call.
type recordingHandler struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(reqNum int, method, path string, body map[string]interface{}) (status int, respBody interface{})
}

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]interface{}
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body) // GET requests have no body; ignore decode errors

	h.mu.Lock()
	h.requests = append(h.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
	n := len(h.requests)
	h.mu.Unlock()

	status, respBody := h.respond(n, r.Method, r.URL.Path, body)
	w.WriteHeader(status)
	if respBody != nil {
		_ = json.NewEncoder(w).Encode(respBody)
	}
}

func (h *recordingHandler) calls() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

// ── Qdrant ────────────────────────────────────────────────────────────────────

func TestQdrantStore_StoreSendsUpsertRequest(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"status": "ok"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewQdrantStore(srv.URL, "")
	resp, err := store.Store(context.Background(), &StoreRequest{
		Collection: "knowledge_base",
		ID:         "42",
		Vector:     []float32{0.1, 0.2},
		Payload:    map[string]interface{}{"title": "t"},
	})
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	if resp.ID != "42" {
		t.Errorf("Store response ID = %q, want %q", resp.ID, "42")
	}
	calls := h.calls()
	if len(calls) != 1 || calls[0].Method != "PUT" || calls[0].Path != "/collections/knowledge_base/points" {
		t.Fatalf("unexpected request: %+v", calls)
	}
	points, ok := calls[0].Body["points"].([]interface{})
	if !ok || len(points) != 1 {
		t.Fatalf("expected exactly one point in request body, got %v", calls[0].Body["points"])
	}
}

func TestQdrantStore_StoreNonOKStatusReturnsError(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusInternalServerError, map[string]interface{}{"error": "boom"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewQdrantStore(srv.URL, "")
	if _, err := store.Store(context.Background(), &StoreRequest{Collection: "knowledge_base", ID: "1"}); err == nil {
		t.Error("expected an error on non-200 response, got nil")
	}
}

func TestQdrantStore_SearchSendsFiltersAndScoreThreshold(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{
			"result": []map[string]interface{}{
				{"id": float64(7), "score": 0.91, "payload": map[string]interface{}{"novel_id": float64(3)}},
			},
		}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewQdrantStore(srv.URL, "")
	results, err := store.Search(context.Background(), &SearchRequest{
		Collection: "knowledge_base",
		Vector:     []float32{0.1},
		Limit:      5,
		Filters:    map[string]interface{}{"novel_id": 3},
		MinScore:   0.6,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 || results[0].ID != "7" {
		t.Fatalf("unexpected results: %+v", results)
	}

	calls := h.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 request, got %d", len(calls))
	}
	if calls[0].Body["score_threshold"] != 0.6 {
		t.Errorf("request body score_threshold = %v, want 0.6", calls[0].Body["score_threshold"])
	}
	filter, ok := calls[0].Body["filter"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a filter object in the request body, got %v", calls[0].Body["filter"])
	}
	must, ok := filter["must"].([]interface{})
	if !ok || len(must) != 1 {
		t.Fatalf("expected exactly one 'must' condition, got %v", filter["must"])
	}
}

func TestQdrantStore_EnsureCollection_ExistsSkipsCreate(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"result": map[string]interface{}{}}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewQdrantStore(srv.URL, "")
	if err := store.EnsureCollection(context.Background(), "knowledge_base", 1536); err != nil {
		t.Fatalf("EnsureCollection failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 1 || calls[0].Method != "GET" {
		t.Fatalf("expected exactly one GET check and no create call, got %+v", calls)
	}
}

func TestQdrantStore_EnsureCollection_MissingCreatesWithDimension(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		if n == 1 {
			return http.StatusNotFound, nil // GET check: collection doesn't exist yet
		}
		return http.StatusOK, map[string]interface{}{"result": true} // PUT create
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewQdrantStore(srv.URL, "")
	if err := store.EnsureCollection(context.Background(), "knowledge_base", 1536); err != nil {
		t.Fatalf("EnsureCollection failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 2 {
		t.Fatalf("expected a GET check followed by a PUT create, got %+v", calls)
	}
	if calls[1].Method != "PUT" {
		t.Errorf("second call method = %q, want PUT", calls[1].Method)
	}
	vectors, ok := calls[1].Body["vectors"].(map[string]interface{})
	if !ok {
		t.Fatalf("create request missing 'vectors' config: %v", calls[1].Body)
	}
	if size, _ := vectors["size"].(float64); int(size) != 1536 {
		t.Errorf("create request vectors.size = %v, want 1536", vectors["size"])
	}
}

// ── Chroma ────────────────────────────────────────────────────────────────────

func TestChromaStore_StoreResolvesCollectionThenAdds(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		if n == 1 {
			return http.StatusOK, map[string]interface{}{"id": "coll-uuid-123"} // resolveCollectionID (get_or_create)
		}
		return http.StatusCreated, map[string]interface{}{} // add
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewChromaStore(srv.URL, "")
	_, err := store.Store(context.Background(), &StoreRequest{
		Collection: "knowledge_base",
		ID:         "9",
		Vector:     []float32{0.5},
		Payload:    map[string]interface{}{"title": "t"},
	})
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 2 {
		t.Fatalf("expected resolve-collection then add, got %+v", calls)
	}
	if calls[0].Body["get_or_create"] != true {
		t.Errorf("resolveCollectionID request should set get_or_create=true, got %v", calls[0].Body["get_or_create"])
	}
	if calls[1].Path != "/api/v2/tenants/default_tenant/databases/default_database/collections/coll-uuid-123/add" {
		t.Errorf("unexpected add path: %s", calls[1].Path)
	}
}

func TestChromaStore_ResolveCollectionIDIsCached(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"id": "coll-uuid-123"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewChromaStore(srv.URL, "")
	for i := 0; i < 3; i++ {
		if _, err := store.Store(context.Background(), &StoreRequest{Collection: "knowledge_base", ID: "1"}); err != nil {
			t.Fatalf("Store #%d failed: %v", i, err)
		}
	}
	calls := h.calls()
	resolveCalls := 0
	for _, c := range calls {
		if c.Path == "/api/v2/tenants/default_tenant/databases/default_database/collections" {
			resolveCalls++
		}
	}
	if resolveCalls != 1 {
		t.Errorf("resolveCollectionID hit the server %d times across 3 Store calls, want 1 (should be cached)", resolveCalls)
	}
}

func TestChromaStore_SearchConvertsDistanceToScore(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		if n == 1 {
			return http.StatusOK, map[string]interface{}{"id": "coll-uuid-123"}
		}
		return http.StatusOK, map[string]interface{}{
			"ids":       [][]string{{"a", "b"}},
			"distances": [][]float32{{0.0, 4.0}}, // score = 1/(1+distance): 1.0, 0.2
			"metadatas": [][]map[string]interface{}{{{"title": "x"}, {"title": "y"}}},
		}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewChromaStore(srv.URL, "")
	results, err := store.Search(context.Background(), &SearchRequest{
		Collection: "knowledge_base",
		Vector:     []float32{0.1},
		Limit:      10,
		MinScore:   0.5, // should exclude "b" (score 0.2)
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 || results[0].ID != "a" {
		t.Fatalf("expected MinScore to filter out the low-score result, got %+v", results)
	}
	if results[0].Score != 1.0 {
		t.Errorf("Score = %v, want 1.0 (distance 0 → score 1/(1+0))", results[0].Score)
	}
}

func TestChromaStore_EnsureCollectionDelegatesToGetOrCreate(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"id": "coll-uuid-123"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewChromaStore(srv.URL, "")
	if err := store.EnsureCollection(context.Background(), "knowledge_base", 1536); err != nil {
		t.Fatalf("EnsureCollection failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 1 || calls[0].Body["get_or_create"] != true {
		t.Fatalf("expected a single get_or_create request, got %+v", calls)
	}
}

// ── DashVector ────────────────────────────────────────────────────────────────

func TestDashVectorStore_StoreSendsDocsPayload(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"code": 0, "request_id": "r1"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewDashVectorStore(srv.URL, "")
	_, err := store.Store(context.Background(), &StoreRequest{
		Collection: "knowledge_base",
		ID:         "5",
		Vector:     []float32{0.3},
		Payload:    map[string]interface{}{"title": "t"},
	})
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 1 || calls[0].Path != "/v1/collections/knowledge_base/docs" {
		t.Fatalf("unexpected request: %+v", calls)
	}
	docs, ok := calls[0].Body["docs"].([]interface{})
	if !ok || len(docs) != 1 {
		t.Fatalf("expected exactly one doc in request body, got %v", calls[0].Body["docs"])
	}
}

func TestDashVectorStore_StoreBusinessErrorReturnsError(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		// DashVector signals failure via a non-zero business "code" even on HTTP 200.
		return http.StatusOK, map[string]interface{}{"code": 13, "message": "collection not found"}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewDashVectorStore(srv.URL, "")
	if _, err := store.Store(context.Background(), &StoreRequest{Collection: "knowledge_base", ID: "1"}); err == nil {
		t.Error("expected an error when the response has a non-zero business code, got nil")
	}
}

func TestDashVectorStore_SearchFiltersByMinScoreClientSide(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{
			"code": 0,
			"output": []map[string]interface{}{
				{"id": "a", "score": 0.9, "fields": map[string]interface{}{"title": "x"}},
				{"id": "b", "score": 0.1, "fields": map[string]interface{}{"title": "y"}},
			},
		}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewDashVectorStore(srv.URL, "")
	results, err := store.Search(context.Background(), &SearchRequest{
		Collection: "knowledge_base",
		Vector:     []float32{0.1},
		Limit:      10,
		MinScore:   0.5,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 || results[0].ID != "a" {
		t.Fatalf("expected MinScore to filter out the low-score result, got %+v", results)
	}
}

func TestDashVectorStore_EnsureCollection_ExistsSkipsCreate(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		return http.StatusOK, map[string]interface{}{"code": 0, "output": map[string]interface{}{}}
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewDashVectorStore(srv.URL, "")
	if err := store.EnsureCollection(context.Background(), "knowledge_base", 1536); err != nil {
		t.Fatalf("EnsureCollection failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 1 || calls[0].Method != "GET" {
		t.Fatalf("expected exactly one GET check and no create call, got %+v", calls)
	}
}

func TestDashVectorStore_EnsureCollection_MissingCreatesWithDimension(t *testing.T) {
	h := &recordingHandler{respond: func(n int, method, path string, body map[string]interface{}) (int, interface{}) {
		if n == 1 {
			return http.StatusOK, map[string]interface{}{"code": 13, "message": "not found"} // GET check fails
		}
		return http.StatusOK, map[string]interface{}{"code": 0} // POST create succeeds
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := NewDashVectorStore(srv.URL, "")
	if err := store.EnsureCollection(context.Background(), "knowledge_base", 1536); err != nil {
		t.Fatalf("EnsureCollection failed: %v", err)
	}
	calls := h.calls()
	if len(calls) != 2 {
		t.Fatalf("expected a GET check followed by a POST create, got %+v", calls)
	}
	if calls[1].Method != "POST" || calls[1].Path != "/v1/collections" {
		t.Errorf("second call = %s %s, want POST /v1/collections", calls[1].Method, calls[1].Path)
	}
	if dim, _ := calls[1].Body["dimension"].(float64); int(dim) != 1536 {
		t.Errorf("create request dimension = %v, want 1536", calls[1].Body["dimension"])
	}
}
