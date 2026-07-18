package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"gorm.io/gorm"
)

// dalleCapture records the last request body the fake DALL-E-compatible image
// endpoint received, so tests can assert on the prompt GenerateThreeViewSheet built.
type dalleCapture struct {
	mu   sync.Mutex
	body map[string]interface{}
}

func (c *dalleCapture) prompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, _ := c.body["prompt"].(string)
	return p
}

// newThreeViewTestService spins up an ImageGenerationService backed by an in-memory
// SQLite ModelProvider/AIModel pair (mirrors the DB-is-source-of-truth wiring
// AIService expects — see loadDBImageProviderEntries) and a fake DALL-E-compatible
// HTTP endpoint, so GenerateThreeViewSheet's real provider-resolution and
// prompt-building code runs end to end without a network call.
func newThreeViewTestService(t *testing.T) (*ImageGenerationService, *dalleCapture, uint) {
	t.Helper()

	capture := &dalleCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// AIService also fires an async provider health check (GET .../models) right after
		// construction (see startProviderHealthCheck) which hits this same server — ignore
		// anything that isn't the image-generation call so it can't race with the capture.
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "images/generations") {
			w.WriteHeader(http.StatusOK)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		capture.mu.Lock()
		capture.body = body
		capture.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"created": 1,
			"data":    []map[string]string{{"url": "https://cdn.example.com/sheet.png"}},
		})
	}))
	t.Cleanup(server.Close)

	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ModelProvider{}, &model.AIModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	const tenantID = uint(1)
	providerRepo := repository.NewModelProviderRepository(db)
	provider := &model.ModelProvider{
		TenantID:     tenantID,
		Name:         "openai",
		APIKey:       "test-key",
		APIEndpoint:  server.URL,
		DefaultModel: "dall-e-3",
		IsActive:     true,
	}
	if err := providerRepo.Create(provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	aiModel := &model.AIModel{
		ProviderID: provider.ID,
		Name:       "dall-e-3",
		Type:       "image",
		IsActive:   true,
	}
	if err := db.Create(aiModel).Error; err != nil {
		t.Fatalf("create ai model: %v", err)
	}

	aiSvc := NewAIService(nil, ai.NewModelManager(), providerRepo)
	t.Cleanup(aiSvc.Shutdown)

	return NewImageGenerationService(aiSvc), capture, tenantID
}

func TestGenerateThreeViewSheet_HumanoidLayout(t *testing.T) {
	svc, capture, tenantID := newThreeViewTestService(t)

	sheet, err := svc.GenerateThreeViewSheet(t.Context(), tenantID,
		"艾莉丝", "身穿银色铠甲的年轻女骑士，金色长发，蓝色眼睛", "", "", "openai")
	if err != nil {
		t.Fatalf("GenerateThreeViewSheet: %v", err)
	}
	if sheet.SheetURL != "https://cdn.example.com/sheet.png" {
		t.Errorf("SheetURL = %q, want the URL returned by the provider", sheet.SheetURL)
	}
	if !strings.Contains(sheet.Description, "艾莉丝") {
		t.Errorf("Description = %q, want it to mention the character name", sheet.Description)
	}

	prompt := capture.prompt()
	if !strings.Contains(prompt, "艾莉丝") {
		t.Errorf("prompt missing name badge text: %q", prompt)
	}
	if !strings.Contains(prompt, "正面、四分之三侧面、背面") {
		t.Errorf("prompt missing humanoid three-view layout rules: %q", prompt)
	}
	if strings.Contains(prompt, "毛发/皮肤质感") {
		t.Errorf("humanoid appearance must not trigger the animal layout: %q", prompt)
	}
}

func TestGenerateThreeViewSheet_AnimalLayout(t *testing.T) {
	svc, capture, tenantID := newThreeViewTestService(t)

	_, err := svc.GenerateThreeViewSheet(t.Context(), tenantID,
		"小狐", "一只毛茸茸的橙色狐狸，尾巴蓬松", "", "", "openai")
	if err != nil {
		t.Fatalf("GenerateThreeViewSheet: %v", err)
	}

	prompt := capture.prompt()
	if !strings.Contains(prompt, "毛发/皮肤质感") {
		t.Errorf("animal appearance should trigger the animal layout: %q", prompt)
	}
	if strings.Contains(prompt, "A-Pose") {
		t.Errorf("animal layout must not include the humanoid A-Pose rule: %q", prompt)
	}
}

func TestGenerateThreeViewSheet_AnthropomorphicStaysHumanoid(t *testing.T) {
	svc, capture, tenantID := newThreeViewTestService(t)

	_, err := svc.GenerateThreeViewSheet(t.Context(), tenantID,
		"猫娘", "一只拟人化的猫娘，穿着女仆装，兽耳兽尾", "", "", "openai")
	if err != nil {
		t.Fatalf("GenerateThreeViewSheet: %v", err)
	}

	prompt := capture.prompt()
	if !strings.Contains(prompt, "A-Pose") {
		t.Errorf("anthropomorphic keywords should keep the humanoid layout: %q", prompt)
	}
}

func TestGenerateThreeViewSheet_CondensesLongAppearance(t *testing.T) {
	svc, capture, tenantID := newThreeViewTestService(t)

	words := make([]string, 200)
	for i := range words {
		words[i] = "detail"
	}
	longAppearance := strings.Join(words, " ")

	_, err := svc.GenerateThreeViewSheet(t.Context(), tenantID,
		"角色", longAppearance, "", "", "openai")
	if err != nil {
		t.Fatalf("GenerateThreeViewSheet: %v", err)
	}

	// The full appearance text is embedded twice: once condensed to 80 words as the
	// leading anchor, once condensed to 40 words inside the layout's face description.
	// Isolate the leading segment (everything before the layout rules) to check the
	// 80-word cap independently of the embedded 40-word copy.
	prompt := capture.prompt()
	leading, _, found := strings.Cut(prompt, "，格式：")
	if !found {
		t.Fatalf("prompt missing expected layout marker: %q", prompt)
	}
	if got := strings.Count(leading, "detail"); got != 80 {
		t.Errorf("expected the leading appearance anchor to be condensed to 80 words, got %d: %q", got, leading)
	}
}

func TestGenerateThreeViewSheet_PropagatesProviderError(t *testing.T) {
	svc, _, tenantID := newThreeViewTestService(t)

	_, err := svc.GenerateThreeViewSheet(t.Context(), tenantID,
		"角色", "普通外观", "", "", "provider-not-configured")
	if err == nil {
		t.Fatal("expected an error for an unconfigured provider, got nil")
	}
}
