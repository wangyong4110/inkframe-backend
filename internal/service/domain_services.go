package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
)

// ============================================
// StyleService adapter methods
// ============================================

func (s *StyleService) GetDefaultStyle() (*StyleConfig, error) {
	return s.getDefaultStyleConfig(), nil
}

func (s *StyleService) BuildStylePrompt(req interface{}) (string, error) {
	cfg := s.getDefaultStyleConfig()
	// Use encoding/json to copy compatible fields
	if data, err := json.Marshal(req); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return s.buildStylePromptInternal(cfg), nil
}

func (s *StyleService) GetStylePresets() interface{} {
	return []map[string]interface{}{
		{"name": "literary", "description": "文学风格"},
		{"name": "commercial", "description": "商业小说风格"},
		{"name": "young_adult", "description": "青春小说风格"},
	}
}

func (s *StyleService) ApplyPreset(name string) (interface{}, error) {
	presets := map[string]*StyleConfig{
		"literary": {
			NarrativeVoice:     "third_limited",
			EmotionalTone:      "cold",
			SentenceComplexity: "complex",
			DescriptionDensity: "rich",
		},
		"commercial": {
			NarrativeVoice:     "third_omniscient",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "moderate",
		},
		"young_adult": {
			NarrativeVoice:     "first_person",
			EmotionalTone:      "warm",
			SentenceComplexity: "simple",
			DescriptionDensity: "minimal",
		},
	}
	style, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("preset %s not found", name)
	}
	return style, nil
}

// ============================================
// ModelService adapter methods
// ============================================

func (s *ModelService) ListProviders(tenantID uint) (interface{}, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	return providers, err
}

// ListSystemProviders 返回系统预置提供商列表（tenant_id=0），用于前端模板下拉框。
func (s *ModelService) ListSystemProviders() ([]*model.ModelProvider, error) {
	return s.providerRepo.ListSystem()
}

// CapableProvider is a minimal provider descriptor returned by capability-filtered listing endpoints.
type CapableProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// providerDisplayNames maps well-known provider names to human-readable labels.
var providerDisplayNames = map[string]string{
	"openai":            "OpenAI",
	"claude":            "Claude (Anthropic)",
	"anthropic":         "Claude (Anthropic)",
	"deepseek":          "DeepSeek",
	"doubao":            "豆包 (Doubao)",
	"qianwen":           "通义千问 (Qianwen)",
	"gemini":            "Gemini (Google)",
	"google":            "Gemini (Google)",
	"kling":             "可灵 (Kling)",
	"seedance":          "Seedance",
	"happyhorse":        "HappyHorse（阿里云百炼）",
	"aliyun-tts":        "阿里云 CosyVoice",
	"qwen-tts":          "千问 TTS（阿里云百炼）",
	ai.ProviderNameVolcengineVisual:  "火山引擎图像",
	ai.ProviderNameJimengVideo:       "即梦视频3.0（火山引擎）",
}

// providerHasCredentials reports whether p has all required credentials.
// volcengine-visual uses AK/SK (two fields); all other providers use a single APIKey.
func providerHasCredentials(p *model.ModelProvider) bool {
	// 需要双密钥的提供商：AK 和 SK 都必须有值
	switch p.Name {
	case ai.ProviderNameVolcengineVisual, ai.ProviderNameJimengVideo,
		"doubao-speech-v1", "kling", "kling-sfx", "kling-tts", "kling-image":
		return strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.APISecretKey) != ""
	}
	return strings.TrimSpace(p.APIKey) != ""
}

// effectiveModelName returns the model name to use when constructing a provider.
// DefaultModel takes precedence; falls back to APIVersion for backward compatibility.
// Do NOT use for doubao-speech (APIVersion = resourceID) or doubao-speech-v1 (APIVersion = cluster).
func effectiveModelName(p *model.ModelProvider) string {
	if p.DefaultModel != "" {
		return p.DefaultModel
	}
	return p.APIVersion
}

func capableProviderDisplayName(providerName, dbDisplayName string) string {
	if dbDisplayName != "" {
		return dbDisplayName
	}
	if dn := providerDisplayNames[providerName]; dn != "" {
		return dn
	}
	return providerName
}

// normalizeProviderType canonicalizes type aliases so "tts"=="voice", "img2img"=="img2img", etc.
func normalizeProviderType(t string) string {
	switch strings.ToLower(t) {
	case "tts":
		return "voice"
	}
	return strings.ToLower(t)
}

// ListCapableProviders returns active, credentialed providers matching the given type (e.g. "LLM", "IMAGE").
func (s *ModelService) ListCapableProviders(tenantID uint, typeFilter string) ([]CapableProvider, error) {
	normalizedType := normalizeProviderType(typeFilter)
	providers, err := s.providerRepo.ListByModelType(tenantID, normalizedType)
	if err != nil {
		return nil, err
	}
	var result []CapableProvider
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if providerHasCredentials(p) {
			result = append(result, CapableProvider{
				Name:        p.Name,
				DisplayName: capableProviderDisplayName(p.Name, p.DisplayName),
			})
		}
	}
	return result, nil
}

func (s *ModelService) GetProvider(id uint, tenantID uint) (*model.ModelProvider, error) {
	return s.providerRepo.GetByIDAndTenant(id, tenantID)
}

// FindProviderByName 按名称查找指定租户的提供商，用于重名冲突提示。
func (s *ModelService) FindProviderByName(name string, tenantID uint) (*model.ModelProvider, error) {
	return s.providerRepo.GetByNameAndTenant(name, tenantID)
}


// typeForProviderType returns the model type string for a provider type.
func typeForProviderType(providerType string) string {
	switch strings.ToLower(providerType) {
	case "image":
		return "image"
	case "img2img":
		return "img2img"
	case "video":
		return "video"
	case "embedding":
		return "embedding"
	case "voice", "tts":
		return "voice"
	case "sfx":
		return "sfx"
	case "music":
		return "music"
	default: // "llm" and anything unrecognised
		return "llm"
	}
}

// seedProviderModel upserts a default AIModel row for the given provider if api_version is set.
func (s *ModelService) seedProviderModel(provider *model.ModelProvider) {
	if provider.APIVersion == "" {
		return
	}
	mtype := "llm"
	existing, _ := s.modelRepo.List(&provider.ID, 0)
	for _, m := range existing {
		if m.Name == provider.APIVersion {
			return // already seeded
		}
	}
	m := &model.AIModel{
		ProviderID:  provider.ID,
		Name:        provider.APIVersion,
		DisplayName: provider.APIVersion,
		Type:        mtype,
		IsActive:    false,
	}
	_ = s.modelRepo.Create(m)
}

// SeedAllProviders 补全已有租户供应商的模型（启动时调用一次，幂等）。
func (s *ModelService) SeedAllProviders() {
	providers, err := s.providerRepo.List()
	if err != nil {
		return
	}
	for _, p := range providers {
		if p.TenantID == 0 {
			continue // 系统级供应商不持有模型记录
		}
		s.seedProviderModel(p)
		s.copySystemModels(p)
	}
}

// copySystemModels 根据内存中的 defaultProviderModels 定义为租户供应商初始化模型列表。
// 系统级供应商（tenant_id=0）不再持有模型记录，因此不从 DB 读取。
// 每种 type 的第一个（质量最高）模型默认激活，确保提供商开箱即用。
func (s *ModelService) copySystemModels(target *model.ModelProvider) {
	if target.TenantID == 0 {
		return
	}
	defs, ok := defaultProviderModels[target.Name]
	if !ok || len(defs) == 0 {
		return
	}
	// 记录每种 type 是否已有激活模型（包括 DB 中已存在的）
	activeByType := make(map[string]bool)
	if existing, err := s.modelRepo.List(&target.ID, target.TenantID); err == nil {
		for _, m := range existing {
			if m.IsActive {
				activeByType[m.Type] = true
			}
		}
	}
	for _, d := range defs {
		// 若此 type 尚无激活模型，则将第一个（排在前面的质量最高的）设为激活
		shouldActivate := !activeByType[d.Type]
		if shouldActivate {
			activeByType[d.Type] = true
		}
		newM := &model.AIModel{
			ProviderID:  target.ID,
			Name:        d.Name,
			DisplayName: d.DisplayName,
			Type:        d.Type,
			Quality:     d.Quality,
			MaxTokens:   d.MaxTokens,
			IsActive:    shouldActivate,
		}
		_ = s.modelRepo.FirstOrCreate(newM)
		// 若模型已存在但 display_name 为空（例如用户手动提前添加），则补填
		if newM.DisplayName == "" && d.DisplayName != "" {
			newM.DisplayName = d.DisplayName
			_ = s.modelRepo.Update(newM)
		}
		// 若模型已存在但未激活，且此 type 尚无任何激活模型，则激活它
		if !newM.IsActive && shouldActivate {
			newM.IsActive = true
			_ = s.modelRepo.Update(newM)
		}
	}
}

func (s *ModelService) CreateProvider(req *model.CreateModelProviderRequest, tenantID uint) (*model.ModelProvider, error) {
	provider := &model.ModelProvider{
		TenantID:    tenantID,
		Name:        req.Name,
		DisplayName: req.DisplayName,
		APIEndpoint: req.APIEndpoint,
		APIKey:      req.APIKey,
		APISecretKey: req.APISecretKey,
		APIVersion:  req.APIVersion,
		IsActive:    req.IsActive,
	}
	if err := s.providerRepo.Create(provider); err != nil {
		return nil, err
	}
	s.seedProviderModel(provider)
	s.copySystemModels(provider)
	return provider, nil
}


func (s *ModelService) UpdateProvider(id uint, tenantID uint, req *model.UpdateModelProviderRequest) (*model.ModelProvider, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}
	if req.Name != "" {
		provider.Name = req.Name
	}
	if req.DisplayName != "" {
		provider.DisplayName = req.DisplayName
	}
	if req.APIEndpoint != "" {
		provider.APIEndpoint = req.APIEndpoint
	}
	if req.APIKey != "" {
		provider.APIKey = req.APIKey
	}
	if req.APISecretKey != "" {
		provider.APISecretKey = req.APISecretKey
	}
	if req.APIVersion != "" {
		provider.APIVersion = req.APIVersion
	}
	if req.IsActive != nil {
		provider.IsActive = *req.IsActive
	}
	if err := s.providerRepo.Update(provider); err != nil {
		return nil, err
	}
	s.seedProviderModel(provider)
	// 清除缓存，使下次调用重新从 DB 加载最新凭据
	if s.aiService != nil {
		s.aiService.InvalidateProviderCache(provider.Name)
	}
	return provider, nil
}

func (s *ModelService) DeleteProvider(id uint, tenantID uint) error {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return err
	}
	// 系统级 provider（tenant_id=0）不允许租户删除
	if provider.TenantID != tenantID {
		return fmt.Errorf("cannot delete system-level provider")
	}
	// 级联删除关联模型
	if err := s.modelRepo.DeleteByProvider(id); err != nil {
		return fmt.Errorf("delete provider models: %w", err)
	}
	if err := s.providerRepo.Delete(id); err != nil {
		return err
	}
	// 清除缓存，防止已删除的提供商被继续使用
	if s.aiService != nil {
		s.aiService.InvalidateProviderCache(provider.Name)
	}
	return nil
}


func (s *ModelService) TestProvider(id uint, tenantID uint) (interface{}, error) {
	provider, err := s.providerRepo.GetByIDAndTenant(id, tenantID)
	if err != nil {
		return nil, err
	}

	// 即梦AI Visual API（AK/SK 鉴权）：直接构造 provider 进行健康检查
	if provider.Name == "volcengine-visual" {
		if provider.APIKey == "" || provider.APISecretKey == "" {
			return map[string]interface{}{"status": "error", "error": "AccessKey 和 SecretKey 均不能为空", "provider_id": id}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vp := ai.NewVolcengineVisualProvider(provider.APIKey, provider.APISecretKey)
		if checkErr := vp.HealthCheck(ctx); checkErr != nil {
			return map[string]interface{}{"status": "error", "error": checkErr.Error(), "provider_id": id}, nil
		}
		return map[string]interface{}{"status": "ok", "provider_id": id}, nil
	}

	if s.aiService != nil {
		if _, loadErr := s.aiService.getTenantProvider(tenantID, provider.Name); loadErr != nil {
			return map[string]interface{}{"status": "error", "error": loadErr.Error(), "provider_id": id}, nil
		}
	}
	return map[string]interface{}{"status": "ok", "provider_id": id}, nil
}

func (s *ModelService) ListModels(providerID *uint, tenantID uint) (interface{}, error) {
	models, err := s.modelRepo.List(providerID, tenantID)
	if err != nil {
		return nil, err
	}
	return models, nil
}

// GetModel retrieves a single AIModel by ID (used for ownership pre-checks in handlers).
func (s *ModelService) GetModel(id uint) (*model.AIModel, error) {
	return s.modelRepo.GetByID(id)
}

func (s *ModelService) CreateModel(req *model.CreateAIModelRequest, tenantID uint) (*model.AIModel, error) {
	// Validate that the target provider belongs to this tenant (or is a system provider).
	provider, err := s.providerRepo.GetByID(req.ProviderID)
	if err != nil {
		return nil, fmt.Errorf("provider not found: %w", err)
	}
	if provider.TenantID != 0 && provider.TenantID != tenantID {
		return nil, fmt.Errorf("provider does not belong to your tenant")
	}
	m := &model.AIModel{
		ProviderID:  req.ProviderID,
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Type:        req.Type,
		Quality:     req.Quality,
		MaxTokens:   req.MaxTokens,
		Timeout:     req.Timeout,
		Concurrency: req.Concurrency,
		RateLimit:   req.RateLimit,
		IsActive:    true, // 用户主动添加的模型直接激活；copySystemModels 预填的才用 false
	}
	if err := s.modelRepo.FirstOrCreate(m); err != nil {
		return nil, err
	}
	// FirstOrCreate finds the existing row without modifying it when (provider_id, name) already
	// exists. If the seed data pre-populated the row with is_active=false, we must activate it now.
	if !m.IsActive {
		m.IsActive = true
		if err := s.modelRepo.Update(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (s *ModelService) UpdateModel(id uint, tenantID uint, req *model.UpdateAIModelRequest) (*model.AIModel, error) {
	m, err := s.modelRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	// Verify the model's provider belongs to this tenant.
	if m.Provider != nil && m.Provider.TenantID != 0 && m.Provider.TenantID != tenantID {
		return nil, fmt.Errorf("model does not belong to your tenant")
	}
	if req.Name != "" {
		m.Name = req.Name
	}
	if req.DisplayName != "" {
		m.DisplayName = req.DisplayName
	}
	if req.Type != "" {
		m.Type = req.Type
	}
	if req.MaxTokens != 0 {
		m.MaxTokens = req.MaxTokens
	}
	if req.Quality != nil {
		m.Quality = *req.Quality
	}
	if req.Timeout != nil {
		m.Timeout = *req.Timeout
	}
	if req.Concurrency != nil {
		m.Concurrency = *req.Concurrency
	}
	if req.RateLimit != nil {
		m.RateLimit = *req.RateLimit
	}
	if req.IsActive != nil {
		m.IsActive = *req.IsActive
	}
	return m, s.modelRepo.Update(m)
}

func (s *ModelService) DeleteModel(id uint, tenantID uint) error {
	m, err := s.modelRepo.GetByID(id)
	if err != nil {
		return err
	}
	// Prevent deleting models that belong to another tenant.
	if m.Provider != nil && m.Provider.TenantID != 0 && m.Provider.TenantID != tenantID {
		return fmt.Errorf("model does not belong to your tenant")
	}
	if err := s.modelRepo.Delete(id); err != nil {
		return err
	}
	// Invalidate provider cache so stale model config is not served after deletion.
	if s.aiService != nil && m.Provider != nil {
		s.aiService.InvalidateProviderCache(m.Provider.Name)
	}
	return nil
}


// TestModel verifies the model is accessible within the given tenant context.
// Fix 10: Accepts tenantID so the provider lookup uses the correct tenant credentials.
func (s *ModelService) TestModel(id uint, tenantID uint) (interface{}, error) {
	return map[string]interface{}{"status": "ok", "model_id": id, "tenant_id": tenantID}, nil
}

func (s *ModelService) ListExperiments(tenantID uint) (interface{}, error) {
	experiments, err := s.experimentRepo.List(100, tenantID)
	return experiments, err
}

func (s *ModelService) CreateExperiment(req *model.CreateModelComparisonRequest, tenantID uint) (interface{}, error) {
	modelIDsJSON := "[]"
	if len(req.ModelIDs) > 0 {
		if b, err := json.Marshal(req.ModelIDs); err == nil {
			modelIDsJSON = string(b)
		}
	}
	experiment := &model.ModelComparisonExperiment{
		TenantID: tenantID,
		Name:     req.Name,
		TaskType: req.TaskType,
		ModelIDs: modelIDsJSON,
		Status:   "pending",
	}
	return experiment, s.experimentRepo.Create(experiment)
}

func (s *ModelService) GetExperiment(id uint) (interface{}, error) {
	return s.experimentRepo.GetByID(id)
}

func (s *ModelService) StartExperiment(id uint) error {
	exp, err := s.experimentRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("experiment not found: %w", err)
	}
	// 验证至少配置了 2 个模型
	if exp.ModelIDs == "" || exp.ModelIDs == "[]" || exp.ModelIDs == "null" {
		return fmt.Errorf("experiment %d requires at least two models in ModelIDs", id)
	}
	var ids []uint
	if jsonErr := json.Unmarshal([]byte(exp.ModelIDs), &ids); jsonErr != nil || len(ids) < 2 {
		return fmt.Errorf("experiment %d requires at least 2 models for comparison (got: %s)", id, exp.ModelIDs)
	}
	if exp.Status == "running" {
		return fmt.Errorf("experiment %d is already running", id)
	}
	exp.Status = "running"
	exp.Progress = 0
	exp.UpdatedAt = time.Now()
	return s.experimentRepo.Update(exp)
}

func (s *ModelService) GetAvailableModels(taskType string, tenantID uint) ([]*model.AIModel, error) {
	return s.modelRepo.GetAvailableByTaskType(taskType, tenantID)
}

func (s *ModelService) SelectModel(taskType, strategy string, tenantID uint) (*model.AIModel, error) {
	models, err := s.modelRepo.GetAvailableByTaskType(taskType, tenantID)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no available models for task type: %s", taskType)
	}
	switch strategy {
	case "quality":
		return selectByQuality(models), nil
	case "cost":
		return selectByCost(models), nil
	default:
		return selectBalanced(models), nil
	}
}

// TestGeneratePrompt 用指定提供商直接生成内容（供前端测试功能使用）
// providerID 为 model_provider 的 ID；prompt 为用户测试文本。
func (s *ModelService) TestGeneratePrompt(ctx context.Context, tenantID uint, providerID uint, prompt string) (content string, tokens int, err error) {
	if s.aiService == nil {
		return "", 0, fmt.Errorf("AI service not available")
	}
	// 解析 provider name
	provider, lookupErr := s.providerRepo.GetByIDAndTenant(providerID, tenantID)
	if lookupErr != nil {
		return "", 0, fmt.Errorf("provider not found: %w", lookupErr)
	}
	content, err = s.aiService.GenerateWithProviderCtx(ctx, tenantID, 0, "chapter", prompt, provider.Name)
	return content, 0, err
}

// GetTaskProviderMappings 返回各任务类型当前绑定的 ProviderID（已简化：不再读取 TaskModelConfig 表，始终返回空映射）
func (s *ModelService) GetTaskProviderMappings() map[string]uint {
	return map[string]uint{}
}

// SetTaskProviderMapping 更新指定任务类型的 Provider 绑定（TaskModelConfig 已移除，此方法为空操作）
func (s *ModelService) SetTaskProviderMapping(taskType string, providerID uint) error {
	return nil
}

// ============================================
// ForeshadowService adapter methods
// ============================================

func (s *ForeshadowService) MarkFulfilledByID(novelID, foreshadowID, chapterID uint) error {
	chapter := &model.Chapter{ID: chapterID}
	return s.MarkFulfilled(novelID, foreshadowID, chapter)
}

// ============================================
// TimelineService adapter methods
// ============================================

// FormatTimelineForPrompt 将时间线格式化为 markdown 字符串，仅包含与 chapterNo 相近（±5章）的事件。
// 返回空字符串表示无相关事件或 timeline 为空。
func (s *TimelineService) FormatTimelineForPrompt(timeline *Timeline, chapterNo int) string {
	if timeline == nil || len(timeline.Events) == 0 {
		return ""
	}
	var buf strings.Builder
	buf.WriteString("## 时间线约束\n")
	found := false
	for _, ev := range timeline.Events {
		if ev.ChapterNo >= chapterNo-5 && ev.ChapterNo <= chapterNo+5 {
			desc := ev.Description
			if desc == "" {
				desc = ev.Title
			}
			buf.WriteString(fmt.Sprintf("- 第%d章: %s\n", ev.ChapterNo, desc))
			found = true
		}
	}
	if !found {
		return ""
	}
	return buf.String()
}

// ============================================
// WorldviewService
// ============================================

type WorldviewService struct {
	worldviewRepo *repository.WorldviewRepository
	aiService     *AIService
	novelRepo     *repository.NovelRepository
	chapterRepo   *repository.ChapterRepository
}

func NewWorldviewService(worldviewRepo *repository.WorldviewRepository, aiService *AIService) *WorldviewService {
	return &WorldviewService{worldviewRepo: worldviewRepo, aiService: aiService}
}

func (s *WorldviewService) WithNovelRepos(novelRepo *repository.NovelRepository, chapterRepo *repository.ChapterRepository) *WorldviewService {
	s.novelRepo = novelRepo
	s.chapterRepo = chapterRepo
	return s
}

func (s *WorldviewService) CreateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Create(worldview)
}

func (s *WorldviewService) GetWorldview(id uint, tenantID uint) (*model.Worldview, error) {
	return s.worldviewRepo.GetByIDAndTenant(id, tenantID)
}

func (s *WorldviewService) ListWorldviews(tenantID uint, page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	return s.worldviewRepo.List(tenantID, page, pageSize, genre)
}

func (s *WorldviewService) UpdateWorldview(worldview *model.Worldview) error {
	return s.worldviewRepo.Update(worldview)
}

func (s *WorldviewService) DeleteWorldview(id uint) error {
	if err := s.worldviewRepo.DeleteEntitiesByWorldview(id); err != nil {
		return err
	}
	return s.worldviewRepo.Delete(id)
}

// Entity CRUD

func (s *WorldviewService) GetEntities(worldviewID uint) ([]*model.WorldviewEntity, error) {
	return s.worldviewRepo.GetEntities(worldviewID)
}

func (s *WorldviewService) GetEntity(id uint, worldviewID uint) (*model.WorldviewEntity, error) {
	var entity model.WorldviewEntity
	if err := s.worldviewRepo.DB().First(&entity, id).Error; err != nil {
		return nil, err
	}
	// 验证 entity 属于指定 worldview，防止跨租户访问
	if worldviewID != 0 && entity.WorldviewID != worldviewID {
		return nil, fmt.Errorf("entity not found in worldview %d", worldviewID)
	}
	return &entity, nil
}

func (s *WorldviewService) CreateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.CreateEntity(entity)
}

func (s *WorldviewService) UpdateEntity(entity *model.WorldviewEntity) error {
	return s.worldviewRepo.UpdateEntity(entity)
}

func (s *WorldviewService) DeleteEntity(id uint) error {
	return s.worldviewRepo.DeleteEntity(id)
}

// UpdateSection 更新世界观的单个字段，不覆盖其他字段
// sectionKey 必须是以下之一：magic_system, geography, history, culture, rules,
// description, factions, glossary
func (s *WorldviewService) UpdateSection(worldviewID uint, tenantID uint, sectionKey, content string) (*model.Worldview, error) {
	wv, err := s.worldviewRepo.GetByIDAndTenant(worldviewID, tenantID)
	if err != nil {
		return nil, err
	}
	switch sectionKey {
	case "magic_system":
		wv.MagicSystem = content
	case "geography":
		wv.Geography = content
	case "history":
		wv.History = content
	case "culture":
		wv.Culture = content
	case "rules":
		wv.Rules = content
	case "description":
		wv.Description = content
	case "factions":
		wv.Factions = content
	case "glossary":
		wv.Glossary = content
	default:
		return nil, fmt.Errorf("unknown worldview section key: %s", sectionKey)
	}
	if err := s.worldviewRepo.Update(wv); err != nil {
		return nil, err
	}
	return wv, nil
}

// GenerateWorldview AI生成世界观
func (s *WorldviewService) GenerateWorldview(tenantID uint, novelID uint, genre string, hints []string) (*model.Worldview, error) {
	prompt := fmt.Sprintf(`请为【%s】类型的小说生成一个完整、详细的世界观设定。`, genre)

	// 若传入 novelID，优先从小说数据构建上下文
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			prompt = fmt.Sprintf("请根据以下小说信息，为该小说生成一个完整、详细且与之高度契合的世界观设定。\n")
			prompt += fmt.Sprintf("【小说名称】%s\n", novel.Title)
			prompt += fmt.Sprintf("【题材类型】%s\n", novel.Meta.Genre)
			if novel.Meta.Description != "" {
				prompt += fmt.Sprintf("【小说简介】%s\n", novel.Meta.Description)
			}
			if novel.AIConfig.StylePrompt != "" {
				prompt += fmt.Sprintf("【写作风格】%s\n", novel.AIConfig.StylePrompt)
			}
			genre = novel.Meta.Genre
			// 附加前几章内容摘要作为上下文
			if s.chapterRepo != nil {
				if chapters, err := s.chapterRepo.ListByNovel(novelID); err == nil && len(chapters) > 0 {
					limit := 3
					if len(chapters) < limit {
						limit = len(chapters)
					}
					prompt += "【已有章节摘要】\n"
					for i := 0; i < limit; i++ {
						ch := chapters[i]
						if ch.Summary != "" {
							prompt += fmt.Sprintf("第%d章《%s》摘要：%s\n", ch.ChapterNo, ch.Title, ch.Summary)
						} else if ch.Content != "" {
							content := ch.Content
							if runes := []rune(content); len(runes) > 300 {
								content = string(runes[:300]) + "..."
							}
							prompt += fmt.Sprintf("第%d章《%s》内容节选：%s\n", ch.ChapterNo, ch.Title, content)
						}
					}
				}
			}
		}
	} else if len(hints) > 0 {
		prompt += fmt.Sprintf("\n背景参考：%s", strings.Join(hints, "\n"))
	}
	prompt += `

请严格按以下 JSON 格式返回，所有字段均需填写，内容尽量详实（每字段不少于50字）：
{
  "name": "世界观名称（富有特色的专有名词，非类型名）",
  "description": "世界观总体概述：核心世界观念、整体氛围、故事将面对的主要张力",
  "magic_system": "修炼/魔法/异能体系：力量来源、境界划分（列出各级名称）、修炼方式、天花板设定、突破代价",
  "geography": "关键地点（只列出故事实际会发生的场景，3-6处）：每处说明控制方、叙事意义、进入难度",
  "history": "背景矛盾（只写仍在影响当前故事的过去事件）：每条说明该历史如何制造了当前的紧张局势",
  "rules": "世界核心规则与禁忌（每行一条）：天道法则、不可违背的世界规律、违反后果",
  "glossary": "世界专属术语表（每行一条，格式：词语 — 含义）：重要专有名词、境界名称、地名缩写等"
}`

	result, err := s.aiService.GenerateWithProvider(tenantID, 0, "worldview", prompt, "")
	if err != nil {
		return nil, err
	}

	var data struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		MagicSystem string `json:"magic_system"`
		Geography   string `json:"geography"`
		History     string `json:"history"`
		Rules       string `json:"rules"`
		Glossary    string `json:"glossary"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &data); err != nil {
		logger.Errorf("GenerateWorldview: failed to parse AI response: %v, raw: %.300s", err, result)
	}

	name := data.Name
	if name == "" {
		name = genre + "世界"
	}

	return &model.Worldview{
		UUID:        uuid.New().String(),
		TenantID:    tenantID,
		Name:        name,
		Genre:       genre,
		Description: data.Description,
		MagicSystem: data.MagicSystem,
		Geography:   data.Geography,
		History:     data.History,
		Rules:       data.Rules,
		Glossary:    data.Glossary,
	}, nil
}

// ============================================
// TenantService
// ============================================

type TenantService struct {
	tenantRepo     *repository.TenantRepository
	tenantUserRepo *repository.TenantUserRepository
}

func NewTenantService(tenantRepo *repository.TenantRepository, tenantUserRepo *repository.TenantUserRepository) *TenantService {
	return &TenantService{tenantRepo: tenantRepo, tenantUserRepo: tenantUserRepo}
}

func (s *TenantService) ListTenants(page, pageSize int) ([]*model.Tenant, int64, error) {
	return s.tenantRepo.List(page, pageSize)
}

func (s *TenantService) GetTenant(id uint) (*model.Tenant, error) {
	return s.tenantRepo.GetByID(id)
}

func (s *TenantService) CreateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Create(tenant)
}

func (s *TenantService) UpdateTenant(tenant *model.Tenant) error {
	return s.tenantRepo.Update(tenant)
}

func (s *TenantService) DeleteTenant(id uint) error {
	return s.tenantRepo.Delete(id)
}

func (s *TenantService) GetQuota(id uint) (interface{}, error) {
	tenant, err := s.tenantRepo.GetByID(id)
	if err != nil {
		return nil, err
	}
	quota := tenant.GetQuota()
	return map[string]interface{}{
		"tenant_id":  tenant.ID,
		"max_users":  quota.MaxUsers,
		"used_users": tenant.UsedUsers,
	}, nil
}

func (s *TenantService) ListMembers(tenantID uint) ([]*model.TenantUser, error) {
	return s.tenantUserRepo.ListByTenant(tenantID)
}

var validTenantRoles = map[string]bool{
	"owner": true, "admin": true, "member": true, "viewer": true,
}

func (s *TenantService) AddMember(tenantID, userID uint, role string) error {
	if !validTenantRoles[role] {
		return fmt.Errorf("invalid role %q: must be owner/admin/member/viewer", role)
	}
	if existing, err := s.tenantUserRepo.GetByTenantAndUser(tenantID, userID); err == nil && existing != nil {
		return fmt.Errorf("user %d is already a member of tenant %d", userID, tenantID)
	}
	tu := &model.TenantUser{
		TenantID: tenantID,
		UserID:   userID,
		Role:     role,
		Status:   "active",
	}
	return s.tenantUserRepo.Create(tu)
}

func (s *TenantService) RemoveMember(tenantID, userID uint) error {
	if err := s.tenantUserRepo.DeleteByTenantAndUser(tenantID, userID); err != nil {
		return err
	}
	// 同步递减配额计数器（失败仅记录，不影响移除结果）
	if err := s.tenantRepo.DecrUsedUsers(tenantID); err != nil {
		logger.Errorf("[TenantService] RemoveMember: DecrUsedUsers tenantID=%d: %v", tenantID, err)
	}
	return nil
}

func (s *TenantService) UpdateMemberRole(tenantID, userID uint, role string) error {
	if !validTenantRoles[role] {
		return fmt.Errorf("invalid role %q: must be owner/admin/member/viewer", role)
	}
	return s.tenantUserRepo.UpdateRole(tenantID, userID, role)
}

// GetMemberRole returns the role of a user within a tenant.
// Returns ("", err) if the user is not a member.
func (s *TenantService) GetMemberRole(tenantID, userID uint) (string, error) {
	tu, err := s.tenantUserRepo.GetByTenantAndUser(tenantID, userID)
	if err != nil {
		return "", err
	}
	return tu.Role, nil
}

// checkAndConsumeQuota atomically checks whether tenantID has remaining quota for
// quotaType and, if so, increments the usage counter by amount.
func (s *TenantService) checkAndConsumeQuota(tenantID uint, quotaType string, amount int) error {
	return s.tenantRepo.CheckAndConsumeQuota(tenantID, quotaType, amount)
}
