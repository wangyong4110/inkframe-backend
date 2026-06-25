package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// VoiceEntry 单条音色元数据，存储于 ModelProvider.VoicesJSON（JSON 数组）。
// 模型-音色的支持关系（如 qwen-tts 哪些音色支持哪个 model 参数）硬编码在各 TTS Provider 实现里。
type VoiceEntry struct {
	ID       string  `json:"id"`                  // 调用 TTS API 时使用的音色 ID
	Name     string  `json:"name"`                // 展示名称
	Gender   string  `json:"gender,omitempty"`    // male / female / neutral
	AgeGroup string  `json:"age_group,omitempty"` // child / teen / adult / elder
	Quality  float64 `json:"quality,omitempty"`
}

// ModelProvider 模型提供商
type ModelProvider struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	TenantID    uint   `json:"tenant_id" gorm:"index;default:0;comment:0=系统级,>0=租户私有;uniqueIndex:idx_provider_name_tenant"`
	Name        string `json:"name" gorm:"size:50;not null;uniqueIndex:idx_provider_name_tenant"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	// cloud=云端, local=本地

	// API配置
	APIEndpoint  string `json:"api_endpoint" gorm:"size:500"`
	APIKey       string `json:"api_key" gorm:"type:text"`
	APISecretKey string `json:"api_secret_key" gorm:"type:text"` // AK/SK 鉴权的 SecretKey（如火山引擎即梦AI）
	APIVersion   string `json:"api_version" gorm:"size:50"`      // 协议版本号（仅 Azure 使用）
	DefaultModel string `json:"default_model" gorm:"size:200;default:''"`  // 默认模型名称（替代 APIVersion 的模型名用途）

	// 限制
	RateLimit   int `json:"rate_limit"` // 请求/分钟
	Timeout     int `json:"timeout" gorm:"default:0"`     // HTTP 请求超时（秒），0=使用默认值300s
	Concurrency int `json:"concurrency" gorm:"default:0"` // 最大并发调用数，0=不限制

	// 元数据（系统级模板字段，由 seedAIModels 写入，用户无需填写）
	NeedsSecretKey bool   `json:"needs_secret_key" gorm:"default:false"` // 是否需要 AK/SK 双密钥鉴权
	StaticModels   string `json:"static_models,omitempty" gorm:"type:text"` // JSON 字符串，不支持 /models 端点时的内置模型列表
	VoicesJSON     string `json:"voices_json,omitempty" gorm:"type:text"`   // []VoiceEntry JSON，仅 TTS 类提供商

	// 同源分组（如 "kling" 组含 kling/kling-sfx/kling-tts/kling-image/kling-i2i）
	GroupName        string `json:"group_name" gorm:"size:100;index;default:''"`
	IsGroupCanonical bool   `json:"is_group_canonical" gorm:"default:false"` // 是否为该组的 UI 代表

	// 状态
	IsActive    bool   `json:"is_active" gorm:"default:true"`
	HealthCheck string `json:"health_check" gorm:"size:20;default:ok"`
	// ok=正常, degraded=降级, down=宕机

	LastChecked *time.Time `json:"last_checked"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (ModelProvider) TableName() string {
	return "ink_model_provider"
}

// ParseVoices 解析 VoicesJSON 为 []VoiceEntry。
func (p *ModelProvider) ParseVoices() []VoiceEntry {
	if p.VoicesJSON == "" {
		return nil
	}
	var voices []VoiceEntry
	if err := json.Unmarshal([]byte(p.VoicesJSON), &voices); err != nil {
		return nil
	}
	return voices
}

// AIModel AI模型
type AIModel struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	ProviderID uint           `json:"provider_id" gorm:"index;not null;uniqueIndex:uniq_provider_model,priority:1"`
	Provider   *ModelProvider `json:"provider,omitempty" gorm:"foreignKey:ProviderID"`

	Name        string `json:"name" gorm:"size:100;not null;uniqueIndex:uniq_provider_model,priority:2"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Type        string `json:"type" gorm:"size:50;default:''"` // llm / image / img2img / voice / video / embedding / sfx / music

	// 性能指标
	MaxTokens int     `json:"max_tokens"`
	Quality   float64 `json:"quality"`          // 0.0-1.0，用于模型选择策略
	CostPer1K float64 `json:"cost_per_1k_tokens"` // 每千 token 费用，用于成本优先策略

	// 调用限制（0=使用供应商默认值）
	Timeout     int `json:"timeout" gorm:"default:0"`     // HTTP 超时秒数
	Concurrency int `json:"concurrency" gorm:"default:0"` // 最大并发调用数
	RateLimit   int `json:"rate_limit" gorm:"default:0"`  // 请求/分钟

	// 音色专属元数据（仅 type='voice' 时有意义）
	Gender   string `json:"gender" gorm:"size:20;default:''"` // male / female / neutral
	AgeGroup string `json:"age_group" gorm:"size:20;default:''"` // child / teen / adult / elder

	// 状态
	IsActive bool `json:"is_active" gorm:"default:true"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (AIModel) TableName() string {
	return "ink_ai_model"
}

// TaskModelConfig 任务模型配置
type TaskModelConfig struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TaskType string `json:"task_type" gorm:"size:50;index;not null"`

	PrimaryModelID    uint           `json:"primary_model_id"`
	PrimaryModel      *AIModel       `json:"primary_model,omitempty" gorm:"foreignKey:PrimaryModelID"`
	PrimaryProviderID uint           `json:"primary_provider_id" gorm:"default:0"` // 显式绑定 provider，消除同名模型歧义
	PrimaryProvider   *ModelProvider `json:"primary_provider,omitempty" gorm:"foreignKey:PrimaryProviderID"`
	FallbackModelIDs  string         `json:"fallback_model_ids" gorm:"type:text"` // JSON数组

	// 参数
	Temperature    float64 `json:"temperature" gorm:"type:decimal(3,2)"`
	TopP           float64 `json:"top_p" gorm:"type:decimal(3,2)"`
	TopK           int     `json:"top_k"`
	MaxTokens      int     `json:"max_tokens"`
	TimeoutSeconds int     `json:"timeout_seconds" gorm:"default:0"` // 0=使用硬编码默认值(300s)
	SystemPrompt   string  `json:"system_prompt" gorm:"type:text"`

	// 限制
	MaxCostPerTask float64 `json:"max_cost_per_task"`

	// 策略
	Strategy string `json:"strategy" gorm:"size:20;default:balanced"`
	// quality_first=质量优先, cost_first=成本优先, balanced=平衡, custom=自定义

	IsActive bool `json:"is_active" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (TaskModelConfig) TableName() string {
	return "ink_task_model_config"
}

// ModelComparisonExperiment 模型对比实验
type ModelComparisonExperiment struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	UUID        string `json:"uuid" gorm:"uniqueIndex;size:36"`
	Name        string `json:"name" gorm:"size:255;not null"`
	Description string `json:"description" gorm:"type:text"`

	TaskType  string `json:"task_type" gorm:"size:50;index"`
	InputData string `json:"input_data" gorm:"type:text"` // JSON

	ModelIDs   string `json:"model_ids" gorm:"type:text"`  // JSON数组
	Parameters string `json:"parameters" gorm:"type:text"` // JSON

	// 状态
	Status   string  `json:"status" gorm:"size:20;default:pending"`
	Progress float64 `json:"progress" gorm:"type:decimal(5,2);default:0"`

	// 结果
	Results       string `json:"results" gorm:"type:text"` // JSON
	WinnerModelID *uint  `json:"winner_model_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ModelComparisonExperiment) TableName() string {
	return "ink_model_comparison_experiment"
}

// ExperimentResult 实验结果
type ExperimentResult struct {
	ID           uint                       `json:"id" gorm:"primaryKey"`
	ExperimentID uint                       `json:"experiment_id" gorm:"index;not null"`
	Experiment   *ModelComparisonExperiment `json:"experiment,omitempty" gorm:"foreignKey:ExperimentID"`
	ModelID      uint                       `json:"model_id" gorm:"index;not null"`
	Model        *AIModel                   `json:"model,omitempty" gorm:"foreignKey:ModelID"`

	Output string `json:"output" gorm:"type:text"`

	// 质量评分
	QualityScore     float64 `json:"quality_score" gorm:"type:decimal(5,4)"`
	RelevanceScore   float64 `json:"relevance_score" gorm:"type:decimal(5,4)"`
	CreativityScore  float64 `json:"creativity_score" gorm:"type:decimal(5,4)"`
	ConsistencyScore float64 `json:"consistency_score" gorm:"type:decimal(5,4)"`

	// 成本
	TokensUsed int     `json:"tokens_used"`
	Cost       float64 `json:"cost"`
	Latency    float64 `json:"latency"` // 秒

	// 用户评价
	UserRating  *int   `json:"user_rating,omitempty"` // 1-5
	UserComment string `json:"user_comment" gorm:"type:text"`

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	CreatedAt time.Time `json:"created_at"`
}

func (ExperimentResult) TableName() string {
	return "ink_experiment_result"
}

// ModelUsageLog 模型使用记录
type ModelUsageLog struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	TenantID uint   `json:"tenant_id" gorm:"index;default:0"`
	ModelID  uint   `json:"model_id" gorm:"index:idx_usage_model_time,priority:1;default:0"` // 软引用，0=未关联
	TaskType string `json:"task_type" gorm:"size:50;index"`

	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	Cost         float64 `json:"cost"`
	Latency      float64 `json:"latency"` // 秒

	Success bool   `json:"success" gorm:"default:true"`
	Error   string `json:"error" gorm:"type:text"`

	QualityScore *float64 `json:"quality_score,omitempty"`

	CreatedAt time.Time `json:"created_at" gorm:"index:idx_usage_model_time,priority:2"`
}

func (ModelUsageLog) TableName() string {
	return "ink_model_usage_log"
}

// McpTool MCP 工具配置
type McpTool struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	TenantID      uint   `json:"tenant_id" gorm:"index;not null;default:1"`
	Name          string `json:"name" gorm:"size:100;uniqueIndex"`
	DisplayName   string `json:"display_name" gorm:"size:100"`
	Description   string `json:"description" gorm:"type:text"`
	TransportType string `json:"transport_type" gorm:"size:20"` // http, sse, stdio
	Endpoint      string `json:"endpoint" gorm:"size:500"`
	Headers       string `json:"headers" gorm:"type:text"` // JSON map[string]string
	Env           string `json:"env" gorm:"type:text"`     // JSON map[string]string (stdio only)
	Timeout       int    `json:"timeout" gorm:"default:30"`
	IsActive      bool   `json:"is_active" gorm:"default:true"`
	IsSystem      bool   `json:"is_system" gorm:"default:false"` // 系统内置工具不可删除
	Schema        string `json:"schema" gorm:"type:text"`        // JSON 工具能力描述

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (McpTool) TableName() string {
	return "ink_mcp_tool"
}

// ModelMcpBinding 模型 <-> MCP 工具绑定关系
type ModelMcpBinding struct {
	ID        uint `json:"id" gorm:"primaryKey"`
	ModelID   uint `json:"model_id" gorm:"index;uniqueIndex:idx_model_mcp,priority:1;not null"`
	McpToolID uint `json:"tool_id" gorm:"index;uniqueIndex:idx_model_mcp,priority:2;not null"`
	Enabled   bool `json:"enabled" gorm:"default:true"`

	CreatedAt time.Time `json:"created_at"`
}

func (ModelMcpBinding) TableName() string {
	return "ink_model_mcp_binding"
}

// ─── AI Model / Provider / MCP DTOs ───────────────────────────────────────────

type CreateModelProviderRequest struct {
	Name         string `json:"name" binding:"required"`
	DisplayName  string `json:"display_name"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     bool   `json:"is_active"`
	Timeout      int    `json:"timeout"`     // HTTP 超时秒数，0=默认300s
	Concurrency  int    `json:"concurrency"` // 最大并发调用数，0=不限制
	RateLimit    int    `json:"rate_limit"`  // 请求/分钟，0=不限制
}

type UpdateModelProviderRequest struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     *bool  `json:"is_active"`
	Timeout      *int   `json:"timeout"`     // HTTP 超时秒数；nil=不修改
	Concurrency  *int   `json:"concurrency"` // 最大并发调用数；nil=不修改
	RateLimit    *int   `json:"rate_limit"`  // 请求/分钟；nil=不修改
}

type CreateAIModelRequest struct {
	ProviderID  uint    `json:"provider_id" binding:"required"`
	ModelID     string  `json:"model_id" binding:"required"`
	Name        string  `json:"name" binding:"required"`
	Type        string  `json:"type"`
	MaxTokens   int     `json:"max_tokens"`
	Timeout     int     `json:"timeout"`
	Concurrency int     `json:"concurrency"`
	RateLimit   int     `json:"rate_limit"`
	CostPer1K   float64 `json:"cost_per_1k"`
	IsDefault   bool    `json:"is_default"`
}

type UpdateAIModelRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Type        string   `json:"type"` // llm / image / img2img / voice / video / embedding / sfx / music
	MaxTokens   int      `json:"max_tokens"`
	Quality     *float64 `json:"quality"` // 0.0–1.0; nil = no change
	Timeout     *int     `json:"timeout"`
	Concurrency *int     `json:"concurrency"`
	RateLimit   *int     `json:"rate_limit"`
	CostPer1K   float64  `json:"cost_per_1k"`
	IsDefault   *bool    `json:"is_default"`
	IsActive    *bool    `json:"is_active"`
}

type UpdateTaskConfigRequest struct {
	PrimaryModelID    uint    `json:"primary_model_id"`
	PrimaryProviderID uint    `json:"primary_provider_id"` // 显式绑定 provider，消除同名模型歧义
	FallbackModelIDs  string  `json:"fallback_model_ids"`
	MaxTokens         int     `json:"max_tokens"`
	Temperature       float64 `json:"temperature"`
	TopP              float64 `json:"top_p"`
}

type CreateModelComparisonRequest struct {
	Name       string `json:"name" binding:"required"`
	TaskType   string `json:"task_type" binding:"required"`
	ModelIDs   []uint `json:"model_ids"`
	TestPrompt string `json:"test_prompt"`
	Iterations int    `json:"iterations"`
}

type CreateMcpToolRequest struct {
	Name          string            `json:"name" binding:"required"`
	DisplayName   string            `json:"display_name"`
	Description   string            `json:"description"`
	TransportType string            `json:"transport_type" binding:"required"` // http/sse/stdio
	Endpoint      string            `json:"endpoint" binding:"required"`
	Headers       map[string]string `json:"headers"`
	Env           map[string]string `json:"env"`
	Timeout       int               `json:"timeout"`
	IsActive      bool              `json:"is_active"`
}

type UpdateMcpToolRequest struct {
	DisplayName   string            `json:"display_name"`
	Description   string            `json:"description"`
	TransportType string            `json:"transport_type"`
	Endpoint      string            `json:"endpoint"`
	Headers       map[string]string `json:"headers"`
	Env           map[string]string `json:"env"`
	Timeout       int               `json:"timeout"`
	IsActive      *bool             `json:"is_active"`
}
