package model

import (
	"time"

	"gorm.io/gorm"
)

// VoiceEntry 单条音色元数据（见 builtin_voices.go）。
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

// BeforeSave encrypts APIKey and APISecretKey before persisting to the database.
func (p *ModelProvider) BeforeSave(tx *gorm.DB) error {
	if p.APIKey != "" {
		enc, err := EncryptField(p.APIKey)
		if err != nil {
			return err
		}
		p.APIKey = enc
	}
	if p.APISecretKey != "" {
		enc, err := EncryptField(p.APISecretKey)
		if err != nil {
			return err
		}
		p.APISecretKey = enc
	}
	return nil
}

// AfterFind decrypts APIKey and APISecretKey after loading from the database.
func (p *ModelProvider) AfterFind(tx *gorm.DB) error {
	if p.APIKey != "" {
		dec, err := DecryptField(p.APIKey)
		if err == nil {
			p.APIKey = dec
		}
	}
	if p.APISecretKey != "" {
		dec, err := DecryptField(p.APISecretKey)
		if err == nil {
			p.APISecretKey = dec
		}
	}
	return nil
}


// AIModel AI模型
type AIModel struct {
	ID         uint           `json:"id" gorm:"primaryKey"`
	ProviderID uint           `json:"provider_id" gorm:"index;not null;uniqueIndex:uniq_provider_model,priority:1"`
	Provider   *ModelProvider `json:"provider,omitempty" gorm:"foreignKey:ProviderID"`

	Name        string `json:"name" gorm:"size:100;not null;uniqueIndex:uniq_provider_model,priority:2"`
	DisplayName string `json:"display_name" gorm:"size:100"`
	Type        string `json:"type" gorm:"size:50;default:''"` // llm / image / img2img / voice / video / embedding / sfx / music

	// 音色元数据（仅 voice 类型，从 VoiceEntry 填充，不存 DB）
	Gender   string `json:"gender,omitempty" gorm:"-"`   // male / female / neutral
	AgeGroup string `json:"age_group,omitempty" gorm:"-"` // child / teen / adult / elder

	// 性能指标
	MaxTokens int     `json:"max_tokens"`
	Quality   float64 `json:"quality"` // 0.0-1.0，用于模型选择策略

	// 调用限制（0=使用供应商默认值）
	Timeout     int `json:"timeout" gorm:"default:0"`     // HTTP 超时秒数
	Concurrency int `json:"concurrency" gorm:"default:0"` // 最大并发调用数
	RateLimit   int `json:"rate_limit" gorm:"default:0"`  // 请求/分钟

	// 状态
	IsActive bool `json:"is_active" gorm:"default:false"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
}

func (AIModel) TableName() string {
	return "ink_ai_model"
}

// ModelComparisonExperiment 模型对比实验
type ModelComparisonExperiment struct {
	ID       uint `json:"id" gorm:"primaryKey"`
	TenantID uint `json:"tenant_id" gorm:"index;not null;default:0"` // 0=系统级, >0=租户私有
	UUID     string `json:"uuid" gorm:"uniqueIndex;size:36"`
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
	WinnerModelID *uint `json:"winner_model_id"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
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

	// 质量评分（仅 QualityScore 被 RunExperiment 写入，其余评分维度待 AI 评审接入后启用）
	QualityScore float64 `json:"quality_score" gorm:"type:decimal(5,4)"`

	Latency float64 `json:"latency"` // 秒

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
}

type UpdateModelProviderRequest struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	APIEndpoint  string `json:"api_endpoint"`
	APIKey       string `json:"api_key"`
	APISecretKey string `json:"api_secret_key"`
	APIVersion   string `json:"api_version"`
	IsActive     *bool  `json:"is_active"`
}

type CreateAIModelRequest struct {
	ProviderID  uint    `json:"provider_id" binding:"required"`
	ModelID     string  `json:"model_id" binding:"required"`
	Name        string  `json:"name" binding:"required"`
	DisplayName string  `json:"display_name"`
	Type        string  `json:"type"`
	Quality     float64 `json:"quality"`   // 0.0–1.0，0=不设置
	MaxTokens   int     `json:"max_tokens"`
	Timeout     int     `json:"timeout"`
	Concurrency int     `json:"concurrency"`
	RateLimit   int     `json:"rate_limit"`
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
	IsActive    *bool    `json:"is_active"`
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
