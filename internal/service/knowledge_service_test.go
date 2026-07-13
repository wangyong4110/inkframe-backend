package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/vector"
)

// ── test doubles ──────────────────────────────────────────────────────────────

// mockKBRepo is an in-memory stand-in for knowledgeBaseRepo.
type mockKBRepo struct {
	mu            sync.Mutex
	byID          map[uint]*model.KnowledgeBase
	nextID        uint
	incrementCh   chan uint // receives the id on every IncrementUsageCount call, for tests that need to synchronize with the background goroutine in bumpUsageCount
	searchResults []*model.KnowledgeBase
	searchErr     error
	createErr     error
}

func newMockKBRepo() *mockKBRepo {
	return &mockKBRepo{
		byID:        make(map[uint]*model.KnowledgeBase),
		incrementCh: make(chan uint, 64),
	}
}

func (m *mockKBRepo) Create(kb *model.KnowledgeBase) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	kb.ID = m.nextID
	cp := *kb
	m.byID[kb.ID] = &cp
	return nil
}

func (m *mockKBRepo) Search(keyword string, limit int) ([]*model.KnowledgeBase, error) {
	return m.searchResults, m.searchErr
}

func (m *mockKBRepo) GetByNovel(novelID uint) ([]*model.KnowledgeBase, error) { return nil, nil }

func (m *mockKBRepo) ListByNovelPaged(novelID uint, page, pageSize int) ([]*model.KnowledgeBase, int64, error) {
	return nil, 0, nil
}

func (m *mockKBRepo) GetByID(id uint) (*model.KnowledgeBase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kb, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("kb %d not found", id)
	}
	cp := *kb
	return &cp, nil
}

func (m *mockKBRepo) Update(kb *model.KnowledgeBase) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *kb
	m.byID[kb.ID] = &cp
	return nil
}

func (m *mockKBRepo) Delete(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, id)
	return nil
}

func (m *mockKBRepo) ListBySourceChapter(novelID, chapterID uint) ([]*model.KnowledgeBase, error) {
	return nil, nil
}

func (m *mockKBRepo) DeleteBySourceChapter(novelID, chapterID uint) error { return nil }

func (m *mockKBRepo) IncrementUsageCount(id uint) error {
	select {
	case m.incrementCh <- id:
	default:
	}
	return nil
}

// fakeVectorStore is an in-memory stand-in for vector.VectorStore.
type fakeVectorStore struct {
	mu                      sync.Mutex
	items                   map[string]*vector.VectorItem
	storeErr                error
	searchErr               error
	ensureCollectionCalls   int
	ensureCollectionErr     error
	ensureCollectionDimSeen int
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{items: make(map[string]*vector.VectorItem)}
}

func (f *fakeVectorStore) Store(ctx context.Context, req *vector.StoreRequest) (*vector.StoreResponse, error) {
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[req.ID] = &vector.VectorItem{ID: req.ID, Vector: req.Vector, Payload: req.Payload}
	return &vector.StoreResponse{ID: req.ID, Status: "stored"}, nil
}

func (f *fakeVectorStore) Search(ctx context.Context, req *vector.SearchRequest) ([]*vector.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var results []*vector.SearchResult
	for _, item := range f.items {
		if !payloadMatchesFilters(item.Payload, req.Filters) {
			continue
		}
		results = append(results, &vector.SearchResult{ID: item.ID, Score: 0.99, Payload: item.Payload})
		if req.Limit > 0 && len(results) >= req.Limit {
			break
		}
	}
	return results, nil
}

func (f *fakeVectorStore) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	return nil
}

func (f *fakeVectorStore) Get(ctx context.Context, id string) (*vector.VectorItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.items[id], nil
}

func (f *fakeVectorStore) HealthCheck(ctx context.Context) error { return nil }

func (f *fakeVectorStore) EnsureCollection(ctx context.Context, name string, dimension int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCollectionCalls++
	f.ensureCollectionDimSeen = dimension
	return f.ensureCollectionErr
}

func payloadMatchesFilters(payload, filters map[string]interface{}) bool {
	for k, v := range filters {
		pv, ok := payload[k]
		if !ok {
			return false
		}
		// novel_id may be stored as *uint in the payload (see syncVector) vs. a plain value in
		// the filter (see SearchKnowledge) — normalize both to a comparable form.
		if fmt.Sprintf("%v", derefIfPointer(pv)) != fmt.Sprintf("%v", derefIfPointer(v)) {
			return false
		}
	}
	return true
}

func derefIfPointer(v interface{}) interface{} {
	switch p := v.(type) {
	case *uint:
		if p == nil {
			return nil
		}
		return *p
	default:
		return v
	}
}

// fakeAIProvider implements ai.AIProvider with a configurable Embed and no-op everything else.
type fakeAIProvider struct {
	embedFn func(ctx context.Context, text string) ([]float32, error)
}

func (f *fakeAIProvider) Generate(ctx context.Context, req *ai.GenerateRequest) (*ai.GenerateResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeAIProvider) GenerateStream(ctx context.Context, req *ai.GenerateRequest) (<-chan *ai.GenerateResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if f.embedFn != nil {
		return f.embedFn(ctx, text)
	}
	return []float32{0.1, 0.2, 0.3}, nil
}
func (f *fakeAIProvider) ImageGenerate(ctx context.Context, req *ai.ImageGenerateRequest) (*ai.ImageResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeAIProvider) AudioGenerate(ctx context.Context, req *ai.AudioGenerateRequest) (*ai.AudioResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeAIProvider) GetName() string                       { return "fake" }
func (f *fakeAIProvider) GetModels() []string                   { return nil }
func (f *fakeAIProvider) HealthCheck(ctx context.Context) error { return nil }

// newTestKnowledgeService wires a KnowledgeService directly (same package, so private fields
// are settable) against the given repo/vector store/AI provider fakes.
func newTestKnowledgeService(repo *mockKBRepo, vs *fakeVectorStore, aiClient ai.AIProvider) *KnowledgeService {
	sm := vector.NewStoreManager(nil)
	if vs != nil {
		sm.RegisterStore("fake", vs)
	}
	return &KnowledgeService{
		kbRepo:      repo,
		vectorStore: sm,
		aiClient:    aiClient,
	}
}

// ── buildEmbedText ────────────────────────────────────────────────────────────

func TestBuildEmbedText_PreservesTitleTruncatesContent(t *testing.T) {
	title := "标题"
	longContent := make([]rune, maxEmbedInputRunes+500)
	for i := range longContent {
		longContent[i] = '字'
	}
	got := buildEmbedText(title, string(longContent))
	gotRunes := []rune(got)
	if len(gotRunes) > maxEmbedInputRunes {
		t.Errorf("buildEmbedText result has %d runes, want <= %d", len(gotRunes), maxEmbedInputRunes)
	}
	if !containsRunes(got, []rune(title)) {
		t.Errorf("buildEmbedText dropped the title; got prefix %q", string(gotRunes[:min20(len(gotRunes))]))
	}
}

func TestBuildEmbedText_ShortContentUnmodified(t *testing.T) {
	got := buildEmbedText("标题", "短内容")
	want := "标题 短内容"
	if got != want {
		t.Errorf("buildEmbedText(%q, %q) = %q, want %q", "标题", "短内容", got, want)
	}
}

func TestBuildEmbedText_MultiByteBoundarySafe(t *testing.T) {
	// Every rune here is a multi-byte UTF-8 character; if truncation were byte-based instead of
	// rune-based, slicing mid-character would produce invalid UTF-8.
	content := ""
	for i := 0; i < maxEmbedInputRunes+10; i++ {
		content += "中"
	}
	got := buildEmbedText("", content)
	for i, r := range got {
		if r == '�' {
			t.Fatalf("buildEmbedText produced invalid UTF-8 (replacement char) at byte offset %d", i)
		}
	}
}

func containsRunes(haystack string, needle []rune) bool {
	hs := []rune(haystack)
	if len(needle) > len(hs) {
		return false
	}
	for i := 0; i <= len(hs)-len(needle); i++ {
		match := true
		for j := range needle {
			if hs[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func min20(n int) int {
	if n < 20 {
		return n
	}
	return 20
}

// ── rrfMerge ──────────────────────────────────────────────────────────────────

func TestRRFMerge_OverlapRanksHighest(t *testing.T) {
	shared := &model.KnowledgeBase{ID: 1}
	vecOnly := &model.KnowledgeBase{ID: 2}
	kwOnly := &model.KnowledgeBase{ID: 3}

	vectorRanked := []*model.KnowledgeBase{shared, vecOnly} // shared ranked 1st by vector
	keywordRanked := []*model.KnowledgeBase{kwOnly, shared} // shared ranked 2nd by keyword

	got := rrfMerge(vectorRanked, keywordRanked, 10)
	if len(got) != 3 {
		t.Fatalf("rrfMerge returned %d results, want 3", len(got))
	}
	if got[0].ID != 1 {
		t.Errorf("expected the doc present in both lists (id=1) to rank first, got id=%d", got[0].ID)
	}
}

func TestRRFMerge_RespectsLimit(t *testing.T) {
	var vectorRanked []*model.KnowledgeBase
	for i := uint(1); i <= 5; i++ {
		vectorRanked = append(vectorRanked, &model.KnowledgeBase{ID: i})
	}
	got := rrfMerge(vectorRanked, nil, 2)
	if len(got) != 2 {
		t.Fatalf("rrfMerge with limit=2 returned %d results, want 2", len(got))
	}
}

func TestRRFMerge_EmptyInputsReturnsEmpty(t *testing.T) {
	got := rrfMerge(nil, nil, 10)
	if len(got) != 0 {
		t.Fatalf("rrfMerge(nil, nil, 10) = %d results, want 0", len(got))
	}
}

// ── StoreKnowledge ────────────────────────────────────────────────────────────

func TestStoreKnowledge_DBFailureAbortsBeforeVectorWrite(t *testing.T) {
	repo := newMockKBRepo()
	repo.createErr = fmt.Errorf("db unavailable")
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	err := svc.StoreKnowledge(context.Background(), &model.KnowledgeBase{Title: "t", Content: "c"})
	if err == nil {
		t.Fatal("expected error when DB create fails, got nil")
	}
	if len(vs.items) != 0 {
		t.Errorf("vector store should never have been written to; got %d items", len(vs.items))
	}
}

func TestStoreKnowledge_EmbedFailureReturnsError(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	aiClient := &fakeAIProvider{embedFn: func(ctx context.Context, text string) ([]float32, error) {
		return nil, fmt.Errorf("embedding API down")
	}}
	svc := newTestKnowledgeService(repo, vs, aiClient)

	err := svc.StoreKnowledge(context.Background(), &model.KnowledgeBase{Title: "t", Content: "c"})
	if err == nil {
		t.Fatal("expected error when embedding fails, got nil")
	}
}

func TestStoreKnowledge_VectorStoreFailureDoesNotFailCall(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	vs.storeErr = fmt.Errorf("qdrant unreachable")
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	kb := &model.KnowledgeBase{Title: "t", Content: "c"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge should not fail when only the vector Store() call fails: %v", err)
	}
	if kb.ID == 0 {
		t.Error("DB record should still have been created with a valid ID")
	}
}

func TestStoreKnowledge_EnsureCollectionCalledOncePerProcess(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	for i := 0; i < 3; i++ {
		if err := svc.StoreKnowledge(context.Background(), &model.KnowledgeBase{Title: "t", Content: "c"}); err != nil {
			t.Fatalf("StoreKnowledge #%d failed: %v", i, err)
		}
	}
	if vs.ensureCollectionCalls != 1 {
		t.Errorf("EnsureCollection called %d times, want exactly 1 (memoized via sync.Once)", vs.ensureCollectionCalls)
	}
	if vs.ensureCollectionDimSeen != 3 { // fakeAIProvider's default Embed returns a 3-element vector
		t.Errorf("EnsureCollection called with dimension=%d, want 3 (len of the embedded vector)", vs.ensureCollectionDimSeen)
	}
}

// ── UpdateKnowledge ───────────────────────────────────────────────────────────

func TestUpdateKnowledge_TagsOnlyChangeSkipsVectorSync(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	embedCalls := 0
	aiClient := &fakeAIProvider{embedFn: func(ctx context.Context, text string) ([]float32, error) {
		embedCalls++
		return []float32{0.1}, nil
	}}
	svc := newTestKnowledgeService(repo, vs, aiClient)

	kb := &model.KnowledgeBase{Title: "t", Content: "c"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}
	embedCallsAfterCreate := embedCalls

	if _, err := svc.UpdateKnowledge(context.Background(), kb.ID, nil, "", "", "new-tags"); err != nil {
		t.Fatalf("UpdateKnowledge failed: %v", err)
	}
	if embedCalls != embedCallsAfterCreate {
		t.Errorf("UpdateKnowledge with only tags changed should not re-embed; embed called %d more time(s)", embedCalls-embedCallsAfterCreate)
	}
}

func TestUpdateKnowledge_ContentChangeResyncsVector(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	kb := &model.KnowledgeBase{Title: "t", Content: "old content"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}
	if _, err := svc.UpdateKnowledge(context.Background(), kb.ID, nil, "", "new content", ""); err != nil {
		t.Fatalf("UpdateKnowledge failed: %v", err)
	}
	item, err := vs.Get(context.Background(), fmt.Sprintf("%d", kb.ID))
	if err != nil || item == nil {
		t.Fatalf("expected vector item for kb %d to exist after update", kb.ID)
	}
}

func TestUpdateKnowledge_VectorSyncFailureDoesNotFailUpdate(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	kb := &model.KnowledgeBase{Title: "t", Content: "old content"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}

	vs.storeErr = fmt.Errorf("vector store down")
	updated, err := svc.UpdateKnowledge(context.Background(), kb.ID, nil, "", "new content", "")
	if err != nil {
		t.Fatalf("UpdateKnowledge should not fail just because vector sync failed: %v", err)
	}
	if updated.Content != "new content" {
		t.Errorf("DB update should have gone through regardless of vector failure; got content %q", updated.Content)
	}
}

func TestUpdateKnowledge_WrongNovelRejected(t *testing.T) {
	repo := newMockKBRepo()
	svc := newTestKnowledgeService(repo, newFakeVectorStore(), &fakeAIProvider{})

	novelA := uint(1)
	novelB := uint(2)
	kb := &model.KnowledgeBase{Title: "t", Content: "c", NovelID: &novelA}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}
	if _, err := svc.UpdateKnowledge(context.Background(), kb.ID, &novelB, "new title", "", ""); err == nil {
		t.Error("expected UpdateKnowledge to reject a novelID that doesn't own this entry")
	}
}

// ── SearchKnowledge ───────────────────────────────────────────────────────────

func TestSearchKnowledge_HybridMergesVectorAndKeyword(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	// kb1 will be found by both paths; kb2 only by vector; kb3 only by keyword.
	kb1 := &model.KnowledgeBase{Title: "共同命中", Content: "x"}
	kb2 := &model.KnowledgeBase{Title: "仅向量命中", Content: "y"}
	kb3 := &model.KnowledgeBase{Title: "仅关键词命中", Content: "z"}
	for _, kb := range []*model.KnowledgeBase{kb1, kb2, kb3} {
		if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
			t.Fatalf("StoreKnowledge failed: %v", err)
		}
	}
	// fakeVectorStore.Search returns every stored item unconditionally (no real similarity
	// scoring) — kb3 was never stored to the vector at all in this test setup, only kb1/kb2's
	// IDs are pre-populated below to simulate "kb3 has no vector entry".
	if err := vs.Delete(context.Background(), fmt.Sprintf("%d", kb3.ID)); err != nil {
		t.Fatalf("setup: failed to remove kb3 from fake vector store: %v", err)
	}
	repo.searchResults = []*model.KnowledgeBase{kb1, kb3} // keyword path "finds" kb1 and kb3

	results, err := svc.SearchKnowledge(context.Background(), "query", 10, nil)
	if err != nil {
		t.Fatalf("SearchKnowledge failed: %v", err)
	}
	seen := map[uint]bool{}
	for _, kb := range results {
		seen[kb.ID] = true
	}
	for _, id := range []uint{kb1.ID, kb2.ID, kb3.ID} {
		if !seen[id] {
			t.Errorf("expected kb %d to be present in hybrid results, results=%v", id, idsOf(results))
		}
	}
	if results[0].ID != kb1.ID {
		t.Errorf("expected kb1 (hit by both paths) to rank first, got id=%d as first result", results[0].ID)
	}
}

func TestSearchKnowledge_NovelIsolation(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	novelA := uint(1)
	novelB := uint(2)
	kbA := &model.KnowledgeBase{Title: "novel A entry", Content: "c", NovelID: &novelA}
	kbB := &model.KnowledgeBase{Title: "novel B entry", Content: "c", NovelID: &novelB}
	for _, kb := range []*model.KnowledgeBase{kbA, kbB} {
		if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
			t.Fatalf("StoreKnowledge failed: %v", err)
		}
	}
	repo.searchResults = []*model.KnowledgeBase{kbA, kbB}

	results, err := svc.SearchKnowledge(context.Background(), "query", 10, &novelA)
	if err != nil {
		t.Fatalf("SearchKnowledge failed: %v", err)
	}
	for _, kb := range results {
		if kb.NovelID == nil || *kb.NovelID != novelA {
			t.Errorf("SearchKnowledge(novelID=%d) leaked an entry from another novel: id=%d novelID=%v", novelA, kb.ID, kb.NovelID)
		}
	}
}

func TestSearchKnowledge_KeywordErrorFallsBackToVectorOnly(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	kb := &model.KnowledgeBase{Title: "t", Content: "c"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}
	repo.searchErr = fmt.Errorf("keyword index down")

	results, err := svc.SearchKnowledge(context.Background(), "query", 10, nil)
	if err != nil {
		t.Fatalf("SearchKnowledge should not fail when vector path still works: %v", err)
	}
	if len(results) != 1 || results[0].ID != kb.ID {
		t.Errorf("expected vector-only fallback to still return kb %d, got %v", kb.ID, idsOf(results))
	}
}

func TestSearchKnowledge_BothPathsFailReturnsError(t *testing.T) {
	repo := newMockKBRepo()
	repo.searchErr = fmt.Errorf("keyword index down")
	// No vector store registered at all → vector path is skipped entirely.
	svc := newTestKnowledgeService(repo, nil, nil)

	if _, err := svc.SearchKnowledge(context.Background(), "query", 10, nil); err == nil {
		t.Error("expected an error when both keyword and vector paths are unavailable")
	}
}

func TestSearchKnowledge_IncrementsUsageCount(t *testing.T) {
	repo := newMockKBRepo()
	vs := newFakeVectorStore()
	svc := newTestKnowledgeService(repo, vs, &fakeAIProvider{})

	kb := &model.KnowledgeBase{Title: "t", Content: "c"}
	if err := svc.StoreKnowledge(context.Background(), kb); err != nil {
		t.Fatalf("StoreKnowledge failed: %v", err)
	}
	repo.searchResults = []*model.KnowledgeBase{kb}

	if _, err := svc.SearchKnowledge(context.Background(), "query", 10, nil); err != nil {
		t.Fatalf("SearchKnowledge failed: %v", err)
	}

	select {
	case id := <-repo.incrementCh:
		if id != kb.ID {
			t.Errorf("IncrementUsageCount called with id=%d, want %d", id, kb.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for the background bumpUsageCount goroutine to call IncrementUsageCount")
	}
}

func idsOf(results []*model.KnowledgeBase) []uint {
	ids := make([]uint, len(results))
	for i, kb := range results {
		ids[i] = kb.ID
	}
	return ids
}
