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
	ai.ProviderNameVolcengineVisual: "火山引擎图像",
}

// providerHasCredentials reports whether p has all required credentials.
// volcengine-visual uses AK/SK (two fields); all other providers use a single APIKey.
func providerHasCredentials(p *model.ModelProvider) bool {
	// 需要双密钥的提供商：AK 和 SK 都必须有值
	switch p.Name {
	case ai.ProviderNameVolcengineVisual, "doubao-speech-v1",
		"kling", "kling-sfx", "kling-tts", "kling-image":
		return strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.APISecretKey) != ""
	}
	return strings.TrimSpace(p.APIKey) != ""
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

// listCapableProviders returns active, key-bearing providers whose Type field matches typeFilter.
func (s *ModelService) listCapableProviders(tenantID uint, typeFilter string) ([]CapableProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	var result []CapableProvider
	for _, p := range providers {
		if !p.IsActive || !strings.EqualFold(p.Type, typeFilter) {
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

// ListCapableProviders returns active, credentialed providers matching the given type (e.g. "LLM", "IMAGE").
func (s *ModelService) ListCapableProviders(tenantID uint, providerType string) ([]CapableProvider, error) {
	return s.listCapableProviders(tenantID, providerType)
}

func (s *ModelService) GetProvider(id uint, tenantID uint) (*model.ModelProvider, error) {
	return s.providerRepo.GetByIDAndTenant(id, tenantID)
}

// suitableTasksForProviderType returns the suitable_tasks JSON string for a provider type.
func suitableTasksForProviderType(providerType string) string {
	switch strings.ToLower(providerType) {
	case "image":
		return `["image_gen"]`
	case "img2img":
		return `["img2img_gen"]`
	case "video":
		return `["video_gen"]`
	case "embedding":
		return `["embedding"]`
	case "voice", "tts":
		return `["voice_gen"]`
	case "sfx":
		return `["sfx_gen"]`
	default: // "llm" and anything unrecognised
		return `["chapter"]`
	}
}

// seedProviderModel upserts a default AIModel row for the given provider if api_version is set.
func (s *ModelService) seedProviderModel(provider *model.ModelProvider) {
	if provider.APIVersion == "" {
		return
	}
	tasks := suitableTasksForProviderType(provider.Type)
	existing, _ := s.modelRepo.List(&provider.ID, 0)
	for _, m := range existing {
		if m.Name == provider.APIVersion {
			return // already seeded
		}
	}
	m := &model.AIModel{
		ProviderID:    provider.ID,
		Name:          provider.APIVersion,
		DisplayName:   provider.APIVersion,
		SuitableTasks: tasks,
		IsActive:      true,
		IsAvailable:   true,
	}
	_ = s.modelRepo.Create(m)
}

// SeedAllProviders seeds AIModel rows for every existing provider that has an
// api_version set but no matching model row yet. Also fixes existing rows that
// were created with is_available=false due to a prior bug in CreateModel.
// Call once at startup.
func (s *ModelService) SeedAllProviders() {
	providers, err := s.providerRepo.List()
	if err != nil {
		return
	}
	for _, p := range providers {
		s.seedProviderModel(p)
	}
	// One-time fix: activate any manually-created models that have is_active=true
	// but is_available=false (created before the bug was fixed).
	all, err := s.modelRepo.List(nil, 0)
	if err != nil {
		return
	}
	for _, m := range all {
		if m.IsActive && !m.IsAvailable {
			m.IsAvailable = true
			_ = s.modelRepo.Update(m)
		}
	}
}

func (s *ModelService) CreateProvider(req *model.CreateModelProviderRequest, tenantID uint) (*model.ModelProvider, error) {
	provider := &model.ModelProvider{
		TenantID:     tenantID,
		Name:         req.Name,
		DisplayName:  req.DisplayName,
		Type:         req.Type,
		APIEndpoint:  req.APIEndpoint,
		APIKey:       req.APIKey,
		APISecretKey: req.APISecretKey,
		APIVersion:   req.APIVersion,
		IsActive:     req.IsActive,
		Timeout:      req.Timeout,
		Concurrency:  req.Concurrency,
		RateLimit:    req.RateLimit,
	}
	if err := s.providerRepo.Create(provider); err != nil {
		return nil, err
	}
	s.seedProviderModel(provider)
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
	if req.Type != "" {
		provider.Type = req.Type
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
	if req.Timeout != nil {
		provider.Timeout = *req.Timeout
	}
	if req.Concurrency != nil {
		provider.Concurrency = *req.Concurrency
	}
	if req.RateLimit != nil {
		provider.RateLimit = *req.RateLimit
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
	return models, err
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
		ProviderID:    req.ProviderID,
		Name:          req.Name,
		SuitableTasks: req.TaskTypes,
		MaxTokens:     req.MaxTokens,
		CostPer1K:     req.CostPer1K,
		IsActive:      true,
		IsAvailable:   true,
	}
	return m, s.modelRepo.Create(m)
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
	if req.TaskTypes != "" {
		m.SuitableTasks = req.TaskTypes
	}
	if req.MaxTokens != 0 {
		m.MaxTokens = req.MaxTokens
	}
	if req.CostPer1K != 0 {
		m.CostPer1K = req.CostPer1K
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
	return s.modelRepo.Delete(id)
}


func (s *ModelService) TestModel(id uint) (interface{}, error) {
	return map[string]interface{}{"status": "ok", "model_id": id}, nil
}

func (s *ModelService) GetTaskConfig(taskType string) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	return cfg, err
}

func (s *ModelService) UpdateTaskConfig(taskType string, req *model.UpdateTaskConfigRequest) (interface{}, error) {
	cfg, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		return nil, err
	}
	if req.PrimaryModelID != 0 {
		cfg.PrimaryModelID = req.PrimaryModelID
	}
	if req.MaxTokens != 0 {
		cfg.MaxTokens = req.MaxTokens
	}
	if req.Temperature != 0 {
		cfg.Temperature = req.Temperature
	}
	if req.TopP != 0 {
		cfg.TopP = req.TopP
	}
	return cfg, s.taskRepo.Update(cfg)
}

func (s *ModelService) ListExperiments() (interface{}, error) {
	experiments, err := s.experimentRepo.List(100)
	return experiments, err
}

func (s *ModelService) CreateExperiment(req *model.CreateModelComparisonRequest) (interface{}, error) {
	experiment := &model.ModelComparisonExperiment{
		Name:     req.Name,
		TaskType: req.TaskType,
		Status:   "pending",
	}
	return experiment, s.experimentRepo.Create(experiment)
}

func (s *ModelService) GetExperiment(id uint) (interface{}, error) {
	return s.experimentRepo.GetByID(id)
}

func (s *ModelService) StartExperiment(id uint) error {
	return nil
}

func (s *ModelService) GetAvailableModels(taskType string) ([]*model.AIModel, error) {
	return s.modelRepo.GetAvailableByTaskType(taskType)
}

func (s *ModelService) SelectModel(taskType, strategy string) (*model.AIModel, error) {
	models, err := s.modelRepo.GetAvailableByTaskType(taskType)
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

// ============================================
// ForeshadowService adapter methods
// ============================================

func (s *ForeshadowService) GetForeshadows(novelID uint, chapterNo int) ([]*ForeshadowItem, error) {
	return s.CheckForeshadowStatus(novelID, chapterNo)
}

func (s *ForeshadowService) MarkFulfilledByID(novelID, foreshadowID, chapterID uint) error {
	chapter := &model.Chapter{ID: chapterID}
	return s.MarkFulfilled(novelID, foreshadowID, chapter)
}

// ============================================
// TimelineService adapter methods
// ============================================

func (s *TimelineService) GetTimeline(novelID uint) (*Timeline, error) {
	return s.BuildTimeline(novelID)
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

func (s *WorldviewService) GetWorldview(id uint) (*model.Worldview, error) {
	return s.worldviewRepo.GetByID(id)
}

func (s *WorldviewService) ListWorldviews(page, pageSize int, genre string) ([]*model.Worldview, int64, error) {
	return s.worldviewRepo.List(page, pageSize, genre)
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

func (s *WorldviewService) GetEntity(id uint) (*model.WorldviewEntity, error) {
	var entity model.WorldviewEntity
	if err := s.worldviewRepo.DB().First(&entity, id).Error; err != nil {
		return nil, err
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

// GenerateWorldview AI生成世界观
func (s *WorldviewService) GenerateWorldview(tenantID uint, novelID uint, genre string, hints []string) (*model.Worldview, error) {
	prompt := fmt.Sprintf(`请为【%s】类型的小说生成一个完整、详细的世界观设定。`, genre)

	// 若传入 novelID，优先从小说数据构建上下文
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			prompt = fmt.Sprintf("请根据以下小说信息，为该小说生成一个完整、详细且与之高度契合的世界观设定。\n")
			prompt += fmt.Sprintf("【小说名称】%s\n", novel.Title)
			prompt += fmt.Sprintf("【题材类型】%s\n", novel.Genre)
			if novel.Description != "" {
				prompt += fmt.Sprintf("【小说简介】%s\n", novel.Description)
			}
			if novel.StylePrompt != "" {
				prompt += fmt.Sprintf("【写作风格】%s\n", novel.StylePrompt)
			}
			genre = novel.Genre
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

请严格按以下 JSON 格式返回，所有字段均需填写，内容尽量详实（每字段不少于100字）：
{
  "name": "世界观名称（富有特色的专有名词，非类型名）",
  "description": "世界观总体概述，包括核心世界观念、整体氛围和主要冲突主题",
  "magic_system": "修炼/魔法/异能体系的详细描述，包括力量来源、境界划分、修炼方式、天花板设定",
  "geography": "世界地理格局描述，包括主要大陆/区域、重要城市/圣地、地理特征与禁区",
  "history": "世界历史背景，包括重大历史事件、时代更迭、上古传说、现存历史遗留问题",
  "culture": "世界的文化风俗，包括种族/文明构成、宗教信仰、礼仪习俗、价值观念",
  "technology": "世界的科技/炼器/阵法水平，与修炼体系的关系，普通人与修炼者的生活差异",
  "rules": "世界运行的核心规则与禁忌，包括天道法则、禁术禁地、不可违背的世界规律",
  "cheat_system": "主角金手指/系统描述（可选，无则留空）：系统名称、核心功能、等级/点数机制、触发条件"
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
		Culture     string `json:"culture"`
		Technology  string `json:"technology"`
		Rules       string `json:"rules"`
		CheatSystem string `json:"cheat_system"`
	}
	if err := json.Unmarshal([]byte(extractJSON(result)), &data); err != nil {
		logger.Printf("GenerateWorldview: failed to parse AI response: %v, raw: %.300s", err, result)
	}

	name := data.Name
	if name == "" {
		name = genre + "世界"
	}

	return &model.Worldview{
		UUID:        uuid.New().String(),
		Name:        name,
		Genre:       genre,
		Description: data.Description,
		MagicSystem: data.MagicSystem,
		Geography:   data.Geography,
		History:     data.History,
		Culture:     data.Culture,
		Technology:  data.Technology,
		Rules:       data.Rules,
		CheatSystem: data.CheatSystem,
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

func (s *TenantService) AddMember(tenantID, userID uint, role string) error {
	tu := &model.TenantUser{
		TenantID: tenantID,
		UserID:   userID,
		Role:     role,
		Status:   "active",
	}
	return s.tenantUserRepo.Create(tu)
}

func (s *TenantService) RemoveMember(tenantID, userID uint) error {
	return s.tenantUserRepo.DeleteByTenantAndUser(tenantID, userID)
}

func (s *TenantService) UpdateMemberRole(tenantID, userID uint, role string) error {
	return s.tenantUserRepo.UpdateRole(tenantID, userID, role)
}
