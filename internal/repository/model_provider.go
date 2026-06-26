package repository

import (
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// ============================================
// Model Repositories
// ============================================

// ModelProviderRepository 模型提供商仓库
type ModelProviderRepository struct {
	db *gorm.DB
}

func NewModelProviderRepository(db *gorm.DB) *ModelProviderRepository {
	return &ModelProviderRepository{db: db}
}

// List 获取提供商列表
func (r *ModelProviderRepository) List() ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// ListSystem 获取系统预置提供商列表（仅 tenant_id=0）
func (r *ModelProviderRepository) ListSystem() ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Where("tenant_id = 0").Order("id").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// ListByTenant 获取租户提供商列表（含系统级 tenant_id=0）
func (r *ModelProviderRepository) ListByTenant(tenantID uint) ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	if err := r.db.Where("tenant_id = ? OR tenant_id = 0", tenantID).Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// GetByID 根据ID获取提供商
func (r *ModelProviderRepository) GetByID(id uint) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.First(&provider, id).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// GetByIDAndTenant 根据ID和租户获取提供商（仅租户自己的或系统级）
func (r *ModelProviderRepository) GetByIDAndTenant(id uint, tenantID uint) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.Where("id = ? AND (tenant_id = ? OR tenant_id = 0)", id, tenantID).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}


// ListByModelType 获取拥有指定类型模型的提供商列表（含系统级 tenant_id=0）
func (r *ModelProviderRepository) ListByModelType(tenantID uint, modelType string) ([]*model.ModelProvider, error) {
	var providers []*model.ModelProvider
	var err error
	if modelType == "voice" {
		// 只查租户自己的 provider（tenant_id > 0），系统级是模板不直接暴露
		err = r.db.Where(
			"tenant_id = ? AND deleted_at IS NULL AND is_active = 1 AND voices_json != '' AND voices_json != '[]'",
			tenantID,
		).Find(&providers).Error
	} else {
		err = r.db.Where(
			"(tenant_id = ? OR tenant_id = 0) AND deleted_at IS NULL AND id IN (SELECT DISTINCT provider_id FROM ink_ai_model WHERE type = ? AND is_active = 1 AND deleted_at IS NULL)",
			tenantID, modelType,
		).Find(&providers).Error
	}
	return providers, err
}


// GetSystemProvider 获取系统级提供商（tenant_id=0）
func (r *ModelProviderRepository) GetSystemProvider(name string) (*model.ModelProvider, error) {
	var provider model.ModelProvider
	if err := r.db.Where("name = ? AND tenant_id = 0", name).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// Create 创建提供商
func (r *ModelProviderRepository) Create(provider *model.ModelProvider) error {
	// 清理同 (tenant_id, name) 的历史软删除记录，避免唯一索引冲突。
	r.db.Unscoped().
		Where("tenant_id = ? AND name = ? AND deleted_at IS NOT NULL", provider.TenantID, provider.Name).
		Delete(&model.ModelProvider{}) //nolint:errcheck
	return r.db.Create(provider).Error
}

// Update 更新提供商
func (r *ModelProviderRepository) Update(provider *model.ModelProvider) error {
	return r.db.Save(provider).Error
}

// Delete 硬删除模型提供商（Unscoped，跳过软删除），确保再次创建同名提供商不会冲突。
// 先级联硬删除关联的 AIModel 记录，避免外键约束报错。
func (r *ModelProviderRepository) Delete(id uint) error {
	if err := r.db.Unscoped().Where("provider_id = ?", id).Delete(&model.AIModel{}).Error; err != nil {
		return err
	}
	return r.db.Unscoped().Delete(&model.ModelProvider{}, id).Error
}

// UpdateHealthStatus 更新健康状态
func (r *ModelProviderRepository) UpdateHealthStatus(id uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"health_check": status,
			"last_checked": time.Now(),
		}).Error
}

// AIModelRepository AI模型仓库
type AIModelRepository struct {
	db *gorm.DB
}

func NewAIModelRepository(db *gorm.DB) *AIModelRepository {
	return &AIModelRepository{db: db}
}

// taskTypeToModelType maps the caller-facing task type string to ink_ai_model.type values.
func taskTypeToModelType(taskType string) string {
	switch taskType {
	case "voice_gen":
		return "voice"
	case "image_gen":
		return "image"
	case "img2img_gen":
		return "img2img"
	case "video_gen":
		return "video"
	case "music_gen":
		return "music"
	case "sfx_gen", "sfx":
		return "sfx"
	case "embedding":
		return "embedding"
	default:
		return "llm"
	}
}

// GetAvailableByTaskType 获取任务可用的模型。
// voice_gen 任务类型从 ink_model_provider.voices_json 读取音色列表，其余任务类型从 ink_ai_model 查询。
// tenantID > 0 时只返回该租户自己的模型 + 系统模型（tenant_id=0）；
// tenantID = 0 时仅返回系统模型。
func (r *AIModelRepository) GetAvailableByTaskType(taskType string, tenantID uint) ([]*model.AIModel, error) {
	modelType := taskTypeToModelType(taskType)
	if modelType == "voice" {
		return r.getVoicesFromProviders(tenantID)
	}

	var models []*model.AIModel
	credCond := "(CASE WHEN p.needs_secret_key = 1 " +
		"THEN (p.api_key != '' AND p.api_secret_key != '') " +
		"ELSE p.api_key != '' END)"
	query := r.db.Preload("Provider").
		Joins("JOIN ink_model_provider p ON p.id = ink_ai_model.provider_id AND p.deleted_at IS NULL").
		Where("ink_ai_model.is_active = ? AND ink_ai_model.type = ?"+
			" AND "+credCond, true, modelType)
	if tenantID > 0 {
		query = query.Where("p.tenant_id = 0 OR p.tenant_id = ?", tenantID)
	} else {
		query = query.Where("p.tenant_id = 0")
	}
	if err := query.Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// GetVoicesFromProvider 返回指定 provider 的音色列表（构造为虚拟 AIModel）。
// 用于 voice_gen 任务：只展示用户在任务配置中明确选定的 TTS provider 的音色。
func (r *AIModelRepository) GetVoicesFromProvider(tenantID, providerID uint) ([]*model.AIModel, error) {
	credCond := "(CASE WHEN needs_secret_key = 1 " +
		"THEN (api_key != '' AND api_secret_key != '') " +
		"ELSE api_key != '' END)"
	var providers []*model.ModelProvider
	if err := r.db.Where(
		"id = ? AND tenant_id = ? AND deleted_at IS NULL AND is_active = 1 AND voices_json != '' AND voices_json != '[]' AND "+credCond,
		providerID, tenantID,
	).Find(&providers).Error; err != nil {
		return nil, err
	}
	var result []*model.AIModel
	for _, p := range providers {
		for _, v := range p.ParseVoices() {
			result = append(result, &model.AIModel{
				ProviderID:  p.ID,
				Provider:    p,
				Name:        v.ID,
				DisplayName: v.Name,
				Type:        "voice",
				Gender:      v.Gender,
				AgeGroup:    v.AgeGroup,
				Quality:     v.Quality,
				IsActive:    true,
			})
		}
	}
	return result, nil
}

// getVoicesFromProviders 已废弃，保留仅供内部回退，不再直接调用。
func (r *AIModelRepository) getVoicesFromProviders(tenantID uint) ([]*model.AIModel, error) {
	credCond := "(CASE WHEN needs_secret_key = 1 " +
		"THEN (api_key != '' AND api_secret_key != '') " +
		"ELSE api_key != '' END)"
	q := r.db.Where("deleted_at IS NULL AND is_active = 1 AND voices_json != '' AND voices_json != '[]' AND " + credCond)
	if tenantID > 0 {
		q = q.Where("tenant_id = ?", tenantID)
	} else {
		return nil, nil
	}
	var providers []*model.ModelProvider
	if err := q.Find(&providers).Error; err != nil {
		return nil, err
	}
	var result []*model.AIModel
	for _, p := range providers {
		for _, v := range p.ParseVoices() {
			result = append(result, &model.AIModel{
				ProviderID:  p.ID,
				Provider:    p,
				Name:        v.ID,
				DisplayName: v.Name,
				Type:        "voice",
				Gender:      v.Gender,
				AgeGroup:    v.AgeGroup,
				Quality:     v.Quality,
				IsActive:    true,
			})
		}
	}
	return result, nil
}

// GetByID 根据ID获取模型
func (r *AIModelRepository) GetByID(id uint) (*model.AIModel, error) {
	var model model.AIModel
	if err := r.db.Preload("Provider").First(&model, id).Error; err != nil {
		return nil, err
	}
	return &model, nil
}

// GetByName 按模型名称查找（如 "deepseek-chat"），返回第一个匹配的活跃模型及其提供商
func (r *AIModelRepository) GetByName(name string) (*model.AIModel, error) {
	var m model.AIModel
	if err := r.db.Preload("Provider").Where("name = ? AND is_active = ?", name, true).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// List 获取模型列表，支持按提供商和租户过滤。
// tenantID=0 时不进行租户过滤（仅限内部调用）。
func (r *AIModelRepository) List(providerID *uint, tenantID uint) ([]*model.AIModel, error) {
	var models []*model.AIModel
	query := r.db.Preload("Provider").
		Joins("JOIN ink_model_provider p ON p.id = ink_ai_model.provider_id AND p.deleted_at IS NULL").
		Where("ink_ai_model.is_active = 1")

	if tenantID > 0 {
		query = query.Where("p.tenant_id = 0 OR p.tenant_id = ?", tenantID)
	}
	if providerID != nil {
		query = query.Where("ink_ai_model.provider_id = ?", *providerID)
	}

	if err := query.Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// Create 创建模型
func (r *AIModelRepository) Create(model *model.AIModel) error {
	return r.db.Create(model).Error
}

// FirstOrCreate 幂等创建：若 (provider_id, name) 已存在则不重复插入。
func (r *AIModelRepository) FirstOrCreate(m *model.AIModel) error {
	return r.db.Where("provider_id = ? AND name = ?", m.ProviderID, m.Name).
		FirstOrCreate(m).Error
}

// Update 更新模型
func (r *AIModelRepository) Update(model *model.AIModel) error {
	return r.db.Save(model).Error
}

// Delete 删除AI模型
func (r *AIModelRepository) Delete(id uint) error {
	return r.db.Delete(&model.AIModel{}, id).Error
}

// DeleteByProvider 删除指定提供商的所有模型
func (r *AIModelRepository) DeleteByProvider(providerID uint) error {
	return r.db.Where("provider_id = ?", providerID).Delete(&model.AIModel{}).Error
}

// UpdateHealthStatus 更新健康状态
func (r *AIModelRepository) UpdateHealthStatus(providerID uint, status string) error {
	return r.db.Model(&model.ModelProvider{}).Where("id = ?", providerID).
		Updates(map[string]interface{}{
			"health_check": status,
			"last_checked": time.Now(),
		}).Error
}

// LogUsage 记录使用（忽略外键约束错误，使用日志为非关键数据）
func (r *AIModelRepository) LogUsage(log *model.ModelUsageLog) error {
	err := r.db.Create(log).Error
	if err != nil && isForeignKeyError(err) {
		return nil // model_id 引用不存在时静默跳过，不影响主流程
	}
	return err
}

// GetUsageStats 获取使用统计
func (r *AIModelRepository) GetUsageStats(modelID uint, startTime, endTime time.Time) (*UsageStats, error) {
	var stats UsageStats
	type aggRow struct {
		TotalRequests int
		SuccessCount  int
		TotalTokens   int
		TotalCost     float64
		TotalLatency  float64
	}
	var row aggRow
	err := r.db.Model(&model.ModelUsageLog{}).
		Select("COUNT(*) AS total_requests, SUM(CASE WHEN success THEN 1 ELSE 0 END) AS success_count, "+
			"COALESCE(SUM(total_tokens), 0) AS total_tokens, COALESCE(SUM(cost), 0) AS total_cost, "+
			"COALESCE(SUM(latency), 0) AS total_latency").
		Where("model_id = ? AND created_at BETWEEN ? AND ?", modelID, startTime, endTime).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	stats.TotalRequests = row.TotalRequests
	stats.SuccessCount = row.SuccessCount
	stats.TotalTokens = row.TotalTokens
	stats.TotalCost = row.TotalCost
	stats.TotalLatency = row.TotalLatency
	if stats.TotalRequests > 0 {
		stats.AverageLatency = stats.TotalLatency / float64(stats.TotalRequests)
		stats.SuccessRate = float64(stats.SuccessCount) / float64(stats.TotalRequests)
	}
	return &stats, nil
}

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests  int
	SuccessCount   int
	TotalTokens    int
	TotalCost      float64
	TotalLatency   float64
	AverageLatency float64
	SuccessRate    float64
}

// TaskModelConfigRepository 任务模型配置仓库
type TaskModelConfigRepository struct {
	db *gorm.DB
}

func NewTaskModelConfigRepository(db *gorm.DB) *TaskModelConfigRepository {
	return &TaskModelConfigRepository{db: db}
}

// GetByTaskType 获取任务配置
func (r *TaskModelConfigRepository) GetByTaskType(taskType string) (*model.TaskModelConfig, error) {
	var config model.TaskModelConfig
	if err := r.db.Preload("PrimaryModel").
		Where("task_type = ? AND is_active = ?", taskType, true).
		First(&config).Error; err != nil {
		return nil, err
	}
	return &config, nil
}

// Create 创建配置
func (r *TaskModelConfigRepository) Create(config *model.TaskModelConfig) error {
	return r.db.Create(config).Error
}

// Update 更新配置
func (r *TaskModelConfigRepository) Update(config *model.TaskModelConfig) error {
	return r.db.Save(config).Error
}

// ModelComparisonRepository 模型对比仓库
type ModelComparisonRepository struct {
	db *gorm.DB
}

func NewModelComparisonRepository(db *gorm.DB) *ModelComparisonRepository {
	return &ModelComparisonRepository{db: db}
}

// Create 创建对比实验
func (r *ModelComparisonRepository) Create(exp *model.ModelComparisonExperiment) error {
	return r.db.Create(exp).Error
}

// GetByID 获取实验
func (r *ModelComparisonRepository) GetByID(id uint) (*model.ModelComparisonExperiment, error) {
	var exp model.ModelComparisonExperiment
	if err := r.db.First(&exp, id).Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

// Update 更新实验
func (r *ModelComparisonRepository) Update(exp *model.ModelComparisonExperiment) error {
	return r.db.Save(exp).Error
}

// List 获取实验列表（tenantID=0 表示系统管理员，返回全部；>0 返回本租户及系统级实验）
func (r *ModelComparisonRepository) List(limit int, tenantID uint) ([]*model.ModelComparisonExperiment, error) {
	var experiments []*model.ModelComparisonExperiment
	q := r.db.Order("created_at DESC").Limit(limit)
	if tenantID > 0 {
		q = q.Where("tenant_id = ? OR tenant_id = 0", tenantID)
	}
	if err := q.Find(&experiments).Error; err != nil {
		return nil, err
	}
	return experiments, nil
}

// AddResult 添加实验结果
func (r *ModelComparisonRepository) AddResult(result *model.ExperimentResult) error {
	return r.db.Create(result).Error
}

// GetResults 获取实验结果
func (r *ModelComparisonRepository) GetResults(experimentID uint) ([]*model.ExperimentResult, error) {
	var results []*model.ExperimentResult
	if err := r.db.Preload("Model").Where("experiment_id = ?", experimentID).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}
