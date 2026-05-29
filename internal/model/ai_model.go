package model

import (
	"time"

	"gorm.io/gorm"
)

// ModelProvider 模型提供商
type ModelProvider struct {
	ID          uint   `json:"id" gorm:"primaryKey"`
	TenantID    uint   `json:"tenant_id" gorm:"index;default:0;comment:0=系统级,>0=租户私有;uniqueIndex:idx_provider_name_tenant"`
	Name        string `json:"name" gorm:"size:50;not null;uniqueIndex:idx_provider_name_tenant"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Type        string `json:"type" gorm:"size:20"`
	// cloud=云端, local=本地

	// API配置
	APIEndpoint  string `json:"api_endpoint" gorm:"size:500"`
	APIKey       string `json:"api_key" gorm:"type:text"`
	APISecretKey string `json:"api_secret_key" gorm:"type:text"` // AK/SK 鉴权的 SecretKey（如火山引擎即梦AI）
	APIVersion   string `json:"api_version" gorm:"size:50"`      // 也用于存储默认模型名称

	// 限制
	RateLimit int     `json:"rate_limit"` // 请求/分钟
	MaxTokens int     `json:"max_tokens"`
	CostPer1K float64 `json:"cost_per_1k_tokens"`
	Timeout   int     `json:"timeout" gorm:"default:0"` // HTTP 请求超时（秒），0=使用默认值300s

	// 元数据（系统级模板字段，由 seedAIModels 写入，用户无需填写）
	NeedsSecretKey bool   `json:"needs_secret_key" gorm:"default:false"` // 是否需要 AK/SK 双密钥鉴权
	StaticModels   string `json:"static_models,omitempty" gorm:"type:text"` // JSON 字符串，不支持 /models 端点时的内置模型列表

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

// AIModel AI模型
type AIModel struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	ProviderID uint           `json:"provider_id" gorm:"index;not null"`
	Provider   *ModelProvider `json:"provider,omitempty" gorm:"foreignKey:ProviderID"`

	Name        string `json:"name" gorm:"size:100;not null"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Version     string `json:"version" gorm:"size:50"`
	Type        string `json:"type" gorm:"size:50;default:''"` // e.g. chat/image/voice/embedding

	// 能力
	Capabilities string `json:"capabilities" gorm:"type:text"` // JSON

	// 性能指标
	MaxTokens     int     `json:"max_tokens"`
	ContextWindow int     `json:"context_window"`
	Speed         float64 `json:"speed"`   // tokens/秒
	Quality       float64 `json:"quality"` // 0.0-1.0
	CostPer1K     float64 `json:"cost_per_1k_tokens"`

	// 适用任务
	SuitableTasks string `json:"suitable_tasks" gorm:"type:text"` // JSON数组

	// 默认参数
	DefaultTemperature float64 `json:"default_temperature" gorm:"type:decimal(3,2)"`
	DefaultTopP        float64 `json:"default_top_p" gorm:"type:decimal(3,2)"`
	DefaultTopK        int     `json:"default_top_k"`

	// 状态
	IsActive    bool `json:"is_active" gorm:"default:true"`
	IsAvailable bool `json:"is_available" gorm:"default:true"`

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
	TaskType string `json:"task_type" gorm:"size:50;uniqueIndex;not null"`

	PrimaryModelID   uint     `json:"primary_model_id"`
	PrimaryModel     *AIModel `json:"primary_model,omitempty" gorm:"foreignKey:PrimaryModelID"`
	FallbackModelIDs string   `json:"fallback_model_ids" gorm:"type:text"` // JSON数组

	// 参数
	Temperature    float64 `json:"temperature" gorm:"type:decimal(3,2)"`
	TopP           float64 `json:"top_p" gorm:"type:decimal(3,2)"`
	TopK           int     `json:"top_k"`
	MaxTokens      int     `json:"max_tokens"`
	TimeoutSeconds int     `json:"timeout_seconds" gorm:"default:0"` // 0=使用硬编码默认值(300s)
	SystemPrompt   string  `json:"system_prompt" gorm:"type:text"`

	// 限制
	MaxCostPerTask float64 `json:"max_cost_per_task"`

	// 质量要求
	MinQualityScore float64 `json:"min_quality_score" gorm:"type:decimal(3,2)"`

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
// ModelID 仅作为软引用（无外键约束），允许关联的 AIModel 被删除而不级联报错。
type ModelUsageLog struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
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
	Type         string `json:"type" binding:"required"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     bool   `json:"is_active"`
	Timeout      int    `json:"timeout"` // HTTP 超时秒数，0=默认300s
}

type UpdateModelProviderRequest struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Type         string `json:"type"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     *bool  `json:"is_active"`
	Timeout      *int   `json:"timeout"` // HTTP 超时秒数，0=默认300s；nil=不修改
}

type CreateAIModelRequest struct {
	ProviderID uint    `json:"provider_id" binding:"required"`
	ModelID    string  `json:"model_id" binding:"required"`
	Name       string  `json:"name" binding:"required"`
	TaskTypes  string  `json:"task_types"`
	MaxTokens  int     `json:"max_tokens"`
	CostPer1K  float64 `json:"cost_per_1k"`
	IsDefault  bool    `json:"is_default"`
}

type UpdateAIModelRequest struct {
	Name      string  `json:"name"`
	TaskTypes string  `json:"task_types"`
	MaxTokens int     `json:"max_tokens"`
	CostPer1K float64 `json:"cost_per_1k"`
	IsDefault *bool   `json:"is_default"`
}

type UpdateTaskConfigRequest struct {
	PrimaryModelID   uint    `json:"primary_model_id"`
	FallbackModelIDs string  `json:"fallback_model_ids"`
	MaxTokens        int     `json:"max_tokens"`
	Temperature      float64 `json:"temperature"`
	TopP             float64 `json:"top_p"`
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
