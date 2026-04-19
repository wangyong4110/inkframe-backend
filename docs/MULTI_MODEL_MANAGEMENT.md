# InkFrame - 多模型管理系统技术方案

## 📋 概述

InkFrame 支持集成多种 LLM（大语言模型），允许用户在不同步骤选择合适的模型，并提供模型效果对比功能。通过智能的模型选择策略和 A/B 测试系统，优化生成质量和成本。

### 核心特性

- 🔄 **多模型支持**：支持 OpenAI、Claude、Gemini、Llama、Qwen 等主流模型
- 🎯 **智能模型选择**：根据任务类型自动选择最优模型
- 📊 **效果对比**：A/B 测试和模型效果对比分析
- 💰 **成本优化**：平衡质量和成本，自动选择性价比最高的模型
- 🔧 **灵活配置**：每个步骤可独立配置模型和参数
- 📈 **性能监控**：实时监控模型性能和质量指标
- 🧪 **实验功能**：支持模型实验和对比

---

## 🏗️ 系统架构

### 整体架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                   Model Management Layer                       │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ Model        │  │ Model        │  │ Model        │          │
│  │ Registry    │  │ Selector     │  │ Comparer     │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
                              ▲
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Task Execution Layer                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ Novel        │  │ Character    │  │ Worldview    │          │
│  │ Generation  │  │ Management   │  │ Generation   │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ Storyboard   │  │ Video Frame  │  │ Audio        │          │
│  │ Generation  │  │ Generation   │  │ Generation   │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
                              ▲
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Model Provider Layer                        │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ OpenAI       │  │ Anthropic    │  │ Google       │          │
│  │ Provider    │  │ Provider     │  │ Provider     │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐          │
│  │ Meta (Llama) │  │ Alibaba      │  │ Local Model  │          │
│  │ Provider    │  │ (Qwen)       │  │ Provider     │          │
│  └──────────────┘  └──────────────┘  └──────────────┘          │
└─────────────────────────────────────────────────────────────────┘
```

---

## 🗄️ 数据库设计

### 1. 模型配置

```go
// 模型提供商
type ModelProvider struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    Name        string `json:"name"` // OpenAI, Anthropic, Google, Meta, Alibaba, Local
    DisplayName string `json:"display_name"`
    Type        string `json:"type"` // cloud, local, hybrid

    // API 配置
    APIEndpoint string `json:"api_endpoint"`
    APIKey      string `json:"api_key" gorm:"type:text"`
    APIVersion  string `json:"api_version"`

    // 限制
    RateLimit   int    `json:"rate_limit"` // 请求/分钟
    MaxTokens   int    `json:"max_tokens"`
    CostPer1K   float64 `json:"cost_per_1k_tokens"` // 美元/1K tokens

    // 状态
    IsActive    bool   `json:"is_active"`
    HealthCheck string `json:"health_check"` // ok, degraded, down
    LastChecked time.Time `json:"last_checked"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// 模型定义
type AIModel struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ProviderID  uint   `json:"provider_id"`

    Name        string `json:"name"` // gpt-4, claude-3-opus, gemini-pro, etc.
    DisplayName string `json:"display_name"`
    Version     string `json:"version"`

    // 能力特征
    Capabilities string `json:"capabilities" gorm:"type:text"` // JSON: {text, vision, audio, code, reasoning}
    
    // 性能指标
    MaxTokens      int     `json:"max_tokens"`
    ContextWindow  int     `json:"context_window"` // 上下文窗口大小
    Speed          float64 `json:"speed"` // tokens/秒
    Quality        float64 `json:"quality"` // 0.0 - 1.0 质量评分
    CostPer1K      float64 `json:"cost_per_1k"`

    // 适用任务
    SuitableTasks string `json:"suitable_tasks" gorm:"type:text"` // JSON: [novel, character, worldview, dialogue, etc.]

    // 默认参数
    DefaultTemperature float64 `json:"default_temperature"`
    DefaultTopP       float64 `json:"default_top_p"`
    DefaultMaxTokens  int    `json:"default_max_tokens"`

    // 状态
    IsActive    bool   `json:"is_active"`
    IsAvailable bool   `json:"is_available"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}
```

### 2. 任务模型配置

```go
// 任务类型
type TaskType string

const (
    TaskTypeNovelGeneration    TaskType = "novel_generation"
    TaskTypeChapterGeneration  TaskType = "chapter_generation"
    TaskTypeCharacterCreation  TaskType = "character_creation"
    TaskTypeWorldviewCreation  TaskType = "worldview_creation"
    TaskTypeDialogueGeneration TaskType = "dialogue_generation"
    TaskTypeStoryboarding      TaskType = "storyboarding"
    TaskTypeFrameGeneration    TaskType = "frame_generation"
    TaskTypeImageGeneration    TaskType = "image_generation"
    TaskTypeAudioGeneration    TaskType = "audio_generation"
    TaskTypeMusicGeneration    TaskType = "music_generation"
)

// 任务模型配置
type TaskModelConfig struct {
    ID          uint     `json:"id" gorm:"primaryKey"`
    TaskType    TaskType `json:"task_type"`

    // 主要模型
    PrimaryModelID   uint `json:"primary_model_id"`
    PrimaryModel     *AIModel `json:"primary_model" gorm:"foreignKey:PrimaryModelID"`

    // 备选模型（用于故障转移）
    FallbackModelIDs string `json:"fallback_model_ids" gorm:"type:text"` // JSON

    // 参数配置
    Temperature      float64 `json:"temperature"`
    TopP            float64 `json:"top_p"`
    TopK            int     `json:"top_k"`
    MaxTokens       int     `json:"max_tokens"`
    SystemPrompt    string  `json:"system_prompt" gorm:"type:text"`

    // 成本限制
    MaxCostPerTask  float64 `json:"max_cost_per_task"` // 单个任务最大成本

    // 质量要求
    MinQualityScore float64 `json:"min_quality_score"` // 0.0 - 1.0

    // 优先级策略
    Strategy string `json:"strategy"` // quality_first, cost_first, balanced

    IsActive bool   `json:"is_active"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

### 3. 模型对比实验

```go
// 对比实验
type ModelComparisonExperiment struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    Name        string `json:"name"`
    Description string `json:"description" gorm:"type:text"`

    // 配置
    TaskType    TaskType `json:"task_type"`
    InputData   string  `json:"input_data" gorm:"type:text"` // JSON: 测试输入

    // 对比的模型
    ModelIDs    string `json:"model_ids" gorm:"type:text"` // JSON: [model_id1, model_id2, ...]

    // 参数配置
    Parameters  string `json:"parameters" gorm:"type:text"` // JSON: {model_id: {params}}

    // 状态
    Status      string `json:"status"` // pending, running, completed, failed
    Progress    float64 `json:"progress"`

    // 结果
    Results     string `json:"results" gorm:"type:text"` // JSON: 模型对比结果
    WinnerModelID *uint  `json:"winner_model_id"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// 实验结果
type ExperimentResult struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ExperimentID uint `json:"experiment_id"`
    ModelID     uint   `json:"model_id"`
    Model       *AIModel `json:"model" gorm:"foreignKey:ModelID"`

    // 生成结果
    Output      string `json:"output" gorm:"type:text"`

    // 质量指标
    QualityScore float64 `json:"quality_score"` // 0.0 - 1.0
    RelevanceScore float64 `json:"relevance_score"` // 0.0 - 1.0
    CreativityScore float64 `json:"creativity_score"` // 0.0 - 1.0
    ConsistencyScore float64 `json:"consistency_score"` // 0.0 - 1.0

    // 成本指标
    TokensUsed  int     `json:"tokens_used"`
    Cost        float64 `json:"cost"`
    Latency     float64 `json:"latency"` // 秒

    // 用户评价
    UserRating  *int    `json:"user_rating"` // 1-5
    UserComment string  `json:"user_comment" gorm:"type:text"`

    CreatedAt   time.Time `json:"created_at"`
}
```

### 4. 模型使用统计

```go
// 模型使用记录
type ModelUsageLog struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ModelID     uint   `json:"model_id"`
    TaskType    TaskType `json:"task_type"`

    // 请求信息
    InputTokens int    `json:"input_tokens"`
    OutputTokens int   `json:"output_tokens"`
    TotalTokens  int    `json:"total_tokens"`

    // 成本
    Cost        float64 `json:"cost"`

    // 性能
    Latency     float64 `json:"latency"` // 秒
    Success     bool   `json:"success"`
    Error       string `json:"error" gorm:"type:text"`

    // 质量评估
    QualityScore *float64 `json:"quality_score,omitempty"`

    CreatedAt   time.Time `json:"created_at"`
}

// 模型性能统计
type ModelPerformanceStats struct {
    ID          uint   `json:"id" gorm:"primaryKey"`
    ModelID     uint   `json:"model_id"`
    TaskType    TaskType `json:"task_type"`
    Date        time.Time `json:"date"`

    // 统计数据
    TotalRequests int     `json:"total_requests"`
    SuccessRate   float64 `json:"success_rate"`
    AverageLatency float64 `json:"average_latency"` // 秒

    // 资源使用
    TotalTokens  int     `json:"total_tokens"`
    TotalCost    float64 `json:"total_cost"`
    AverageCost  float64 `json:"average_cost"` // 每次请求平均成本

    // 质量评分
    AverageQuality float64 `json:"average_quality"` // 0.0 - 1.0
    UserRatingAvg float64 `json:"user_rating_avg"` // 1.0 - 5.0

    CreatedAt   time.Time `json:"created_at"`
}
```

---

## 🔧 核心模块设计

### 1. 模型注册表

```go
type ModelRegistry struct {
    db           *gorm.DB
    providers    map[string]ModelProvider
    models       map[uint]AIModel
    taskConfigs  map[TaskType]*TaskModelConfig
    cache        *redis.Client
}

// 注册模型提供商
func (r *ModelRegistry) RegisterProvider(provider *ModelProvider) error {
    // 保存到数据库
    if err := r.db.Create(provider).Error; err != nil {
        return err
    }

    // 初始化客户端
    client := r.createClient(provider)

    r.providers[provider.Name] = *provider

    return nil
}

// 创建客户端
func (r *ModelRegistry) createClient(provider *ModelProvider) ModelClient {
    switch provider.Name {
    case "OpenAI":
        return NewOpenAIClient(provider.APIKey, provider.APIEndpoint)
    case "Anthropic":
        return NewAnthropicClient(provider.APIKey, provider.APIEndpoint)
    case "Google":
        return NewGoogleClient(provider.APIKey, provider.APIEndpoint)
    case "Meta":
        return NewLlamaClient(provider.APIKey, provider.APIEndpoint)
    case "Alibaba":
        return NewQwenClient(provider.APIKey, provider.APIEndpoint)
    case "Local":
        return NewLocalClient(provider.APIEndpoint)
    default:
        return nil
    }
}

// 注册模型
func (r *ModelRegistry) RegisterModel(model *AIModel) error {
    if err := r.db.Create(model).Error; err != nil {
        return err
    }

    r.models[model.ID] = *model

    return nil
}

// 获取可用模型
func (r *ModelRegistry) GetAvailableModels(taskType TaskType) []*AIModel {
    var models []*AIModel

    for _, model := range r.models {
        if !model.IsActive || !model.IsAvailable {
            continue
        }

        // 检查是否适合该任务
        if r.isSuitableForTask(model, taskType) {
            models = append(models, &model)
        }
    }

    return models
}

// 检查模型是否适合任务
func (r *ModelRegistry) isSuitableForTask(model *AIModel, taskType TaskType) bool {
    var suitableTasks []string
    json.Unmarshal([]byte(model.SuitableTasks), &suitableTasks)

    for _, task := range suitableTasks {
        if TaskType(task) == taskType {
            return true
        }
    }

    return false
}

// 健康检查
func (r *ModelRegistry) HealthCheck() map[string]ProviderHealth {
    health := make(map[string]ProviderHealth)

    for name, provider := range r.providers {
        health[name] = r.checkProviderHealth(&provider)
    }

    return health
}

// 检查提供商健康状态
func (r *ModelRegistry) checkProviderHealth(provider *ModelProvider) ProviderHealth {
    client := r.createClient(provider)

    // 发送测试请求
    testPrompt := "Hello, world!"
    startTime := time.Now()

    _, err := client.GenerateCompletion(&CompletionRequest{
        Prompt:    testPrompt,
        MaxTokens: 10,
    })

    latency := time.Since(startTime).Seconds()

    health := ProviderHealth{
        Name:       provider.Name,
        Status:     "ok",
        Latency:    latency,
        LastChecked: time.Now(),
    }

    if err != nil {
        health.Status = "down"
        health.Error = err.Error()
    } else if latency > 5.0 {
        health.Status = "degraded"
    }

    // 更新数据库
    provider.HealthCheck = health.Status
    provider.LastChecked = health.LastChecked
    r.db.Save(provider)

    return health
}
```

### 2. 智能模型选择器

```go
type ModelSelector struct {
    registry    *ModelRegistry
    db          *gorm.DB
    cache       *redis.Client
}

// 选择模型
func (s *ModelSelector) SelectModel(
    taskType TaskType,
    strategy SelectionStrategy,
    constraints *ModelConstraints,
) (*ModelSelection, error) {
    // 1. 获取可用模型
    availableModels := s.registry.GetAvailableModels(taskType)

    if len(availableModels) == 0 {
        return nil, fmt.Errorf("no available models for task type: %s", taskType)
    }

    // 2. 根据策略选择
    var selected *AIModel

    switch strategy {
    case StrategyQualityFirst:
        selected = s.selectByQuality(availableModels)
    case StrategyCostFirst:
        selected = s.selectByCost(availableModels)
    case StrategyBalanced:
        selected = s.selectBalanced(availableModels)
    case StrategyCustom:
        selected = s.selectCustom(availableModels, constraints)
    default:
        selected = s.selectBalanced(availableModels)
    }

    // 3. 应用约束
    if constraints != nil {
        if !s.checkConstraints(selected, constraints) {
            selected = s.findAlternativeModel(availableModels, selected, constraints)
        }
    }

    selection := &ModelSelection{
        Model:        selected,
        Strategy:     strategy,
        Reason:       s.generateSelectionReason(selected, strategy),
        Alternatives: availableModels,
    }

    return selection, nil
}

// 按质量优先选择
func (s *ModelSelector) selectByQuality(models []*AIModel) *AIModel {
    var best *AIModel
    bestScore := 0.0

    for _, model := range models {
        // 综合评分：质量 + 速度
        score := model.Quality*0.7 + (model.Speed/100.0)*0.3

        if score > bestScore {
            bestScore = score
            best = model
        }
    }

    return best
}

// 按成本优先选择
func (s *ModelSelector) selectByCost(models []*AIModel) *AIModel {
    var best *AIModel
    bestScore := math.MaxFloat64

    for _, model := range models {
        // 成本评分：越低越好
        if model.CostPer1K < bestScore {
            bestScore = model.CostPer1K
            best = model
        }
    }

    return best
}

// 平衡选择
func (s *ModelSelector) selectBalanced(models []*AIModel) *AIModel {
    var best *AIModel
    bestScore := 0.0

    for _, model := range models {
        // 平衡评分：质量/成本比
        score := (model.Quality * 100) / model.CostPer1K

        if score > bestScore {
            bestScore = score
            best = model
        }
    }

    return best
}

// 自定义选择
func (s *ModelSelector) selectCustom(
    models []*AIModel,
    constraints *ModelConstraints,
) *AIModel {
    scored := []ModelScore{}

    for _, model := range models {
        score := s.calculateCustomScore(model, constraints)
        scored = append(scored, ModelScore{
            Model: model,
            Score: score,
        })
    }

    // 按分数排序
    sort.Slice(scored, func(i, j int) bool {
        return scored[i].Score > scored[j].Score
    })

    return scored[0].Model
}

// 计算自定义分数
func (s *ModelSelector) calculateCustomScore(
    model *AIModel,
    constraints *ModelConstraints,
) float64 {
    score := 0.0

    // 质量权重
    score += model.Quality * constraints.QualityWeight

    // 成本权重（反向）
    score += (1.0 / model.CostPer1K) * constraints.CostWeight

    // 速度权重
    score += (model.Speed / 100.0) * constraints.SpeedWeight

    // 上下文窗口权重
    if model.ContextWindow >= constraints.MinContextWindow {
        score += 0.1 * constraints.ContextWeight
    }

    return score
}

// 检查约束
func (s *ModelSelector) checkConstraints(
    model *AIModel,
    constraints *ModelConstraints,
) bool {
    // 检查成本约束
    if constraints.MaxCostPerTask > 0 {
        estimatedCost := s.estimateCost(model, constraints.EstimatedTokens)
        if estimatedCost > constraints.MaxCostPerTask {
            return false
        }
    }

    // 检查上下文窗口
    if constraints.MinContextWindow > 0 {
        if model.ContextWindow < constraints.MinContextWindow {
            return false
        }
    }

    // 检查最低质量
    if constraints.MinQuality > 0 {
        if model.Quality < constraints.MinQuality {
            return false
        }
    }

    return true
}

// 查找替代模型
func (s *ModelSelector) findAlternativeModel(
    models []*AIModel,
    preferred *AIModel,
    constraints *ModelConstraints,
) *AIModel {
    for _, model := range models {
        if model.ID == preferred.ID {
            continue
        }

        if s.checkConstraints(model, constraints) {
            return model
        }
    }

    return nil
}
```

### 3. 模型对比系统

```go
type ModelComparer struct {
    registry *ModelRegistry
    db       *gorm.DB
    cache    *redis.Client
}

// 创建对比实验
func (c *ModelComparer) CreateComparison(
    name string,
    taskType TaskType,
    inputData interface{},
    modelIDs []uint,
    parameters map[uint]CompletionRequest,
) (*ModelComparisonExperiment, error) {
    // 1. 序列化输入数据
    inputDataJSON, _ := json.Marshal(inputData)

    // 2. 序列化参数
    parametersJSON, _ := json.Marshal(parameters)

    // 3. 创建实验记录
    experiment := &ModelComparisonExperiment{
        Name:        name,
        TaskType:    taskType,
        InputData:   string(inputDataJSON),
        ModelIDs:    fmt.Sprintf("%v", modelIDs),
        Parameters:  string(parametersJSON),
        Status:      "pending",
    }

    if err := c.db.Create(experiment).Error; err != nil {
        return nil, err
    }

    return experiment, nil
}

// 运行对比实验
func (c *ModelComparer) RunComparison(experimentID uint) error {
    // 1. 获取实验配置
    experiment := c.getExperiment(experimentID)

    // 2. 更新状态
    experiment.Status = "running"
    c.db.Save(experiment)

    // 3. 解析配置
    var modelIDs []uint
    json.Unmarshal([]byte(experiment.ModelIDs), &modelIDs)

    var parameters map[uint]CompletionRequest
    json.Unmarshal([]byte(experiment.Parameters), &parameters)

    var inputData interface{}
    json.Unmarshal([]byte(experiment.InputData), &inputData)

    // 4. 并行运行所有模型
    results := make(chan *ExperimentResult, len(modelIDs))

    for _, modelID := range modelIDs {
        go func(id uint) {
            result := c.runModel(id, experiment, parameters[id], inputData)
            results <- result
        }(modelID)
    }

    // 5. 收集结果
    experimentResults := []*ExperimentResult{}
    for i := 0; i < len(modelIDs); i++ {
        result := <-results
        experimentResults = append(experimentResults, result)
    }

    // 6. 保存结果
    for _, result := range experimentResults {
        c.db.Create(result)
    }

    // 7. 分析结果并选择获胜者
    winner := c.analyzeResults(experimentResults)

    // 8. 序列化结果
    resultsJSON, _ := json.Marshal(experimentResults)

    // 9. 更新实验状态
    experiment.Status = "completed"
    experiment.Results = string(resultsJSON)
    experiment.WinnerModelID = winner.ModelID
    experiment.Progress = 100.0
    c.db.Save(experiment)

    return nil
}

// 运行单个模型
func (c *ModelComparer) runModel(
    modelID uint,
    experiment *ModelComparisonExperiment,
    params CompletionRequest,
    inputData interface{},
) *ExperimentResult {
    // 1. 获取模型
    model := c.registry.GetModel(modelID)

    // 2. 获取客户端
    client := c.registry.GetClient(model.ProviderID)

    // 3. 记录开始时间
    startTime := time.Now()

    // 4. 调用模型
    response, err := client.GenerateCompletion(&params)
    latency := time.Since(startTime).Seconds()

    // 5. 计算成本
    cost := c.calculateCost(model, params.PromptTokens, response.CompletionTokens)

    // 6. 评估质量
    qualityScores := c.assessQuality(response.Text, experiment.TaskType)

    result := &ExperimentResult{
        ExperimentID: experiment.ID,
        ModelID:      modelID,
        Output:       response.Text,
        QualityScore: qualityScores.Overall,
        RelevanceScore: qualityScores.Relevance,
        CreativityScore: qualityScores.Creativity,
        ConsistencyScore: qualityScores.Consistency,
        TokensUsed:   response.CompletionTokens,
        Cost:         cost,
        Latency:      latency,
        Success:      err == nil,
    }

    if err != nil {
        result.Error = err.Error()
    }

    // 7. 保存到数据库
    c.db.Create(result)

    return result
}

// 评估质量
func (c *ModelComparer) assessQuality(
    output string,
    taskType TaskType,
) *QualityScores {
    scores := &QualityScores{}

    // 使用 AI 评估器
    assessment := c.aiAssessQuality(output, taskType)

    scores.Overall = assessment.Overall
    scores.Relevance = assessment.Relevance
    scores.Creativity = assessment.Creativity
    scores.Consistency = assessment.Consistency

    return scores
}

// 分析结果
func (c *ModelComparer) analyzeResults(results []*ExperimentResult) *ExperimentResult {
    var winner *ExperimentResult
    bestScore := 0.0

    for _, result := range results {
        if !result.Success {
            continue
        }

        // 综合评分
        score := result.QualityScore * 0.5 +
            (1.0 - result.Cost/10.0) * 0.3 + // 成本越低越好
            (1.0 - result.Latency/60.0) * 0.2 // 延迟越低越好

        if score > bestScore {
            bestScore = score
            winner = result
        }
    }

    return winner
}

// 获取对比报告
func (c *ModelComparer) GetComparisonReport(experimentID uint) *ComparisonReport {
    // 1. 获取实验
    experiment := c.getExperiment(experimentID)

    // 2. 获取结果
    var results []ExperimentResult
    c.db.Where("experiment_id = ?", experimentID).Find(&results)

    // 3. 生成报告
    report := &ComparisonReport{
        ExperimentID:   experimentID,
        ExperimentName: experiment.Name,
        TaskType:       experiment.TaskType,
        Results:        results,
        Winner:         c.findWinner(results),
        Summary:        c.generateSummary(results),
    }

    return report
}
```

### 4. 模型执行器

```go
type ModelExecutor struct {
    registry *ModelRegistry
    selector *ModelSelector
    comparer *ModelComparer
    db       *gorm.DB
    cache    *redis.Client
}

// 执行任务
func (e *ModelExecutor) ExecuteTask(
    taskType TaskType,
    input *TaskInput,
    options *ExecutionOptions,
) (*TaskResult, error) {
    // 1. 选择模型
    strategy := options.Strategy
    if strategy == "" {
        strategy = StrategyBalanced
    }

    selection, err := e.selector.SelectModel(
        taskType,
        strategy,
        options.Constraints,
    )

    if err != nil {
        return nil, err
    }

    // 2. 获取客户端
    client := e.registry.GetClient(selection.Model.ProviderID)

    // 3. 构建请求
    request := e.buildRequest(input, selection.Model, options.Parameters)

    // 4. 记录开始时间
    startTime := time.Now()

    // 5. 执行请求
    response, err := client.GenerateCompletion(request)
    latency := time.Since(startTime).Seconds()

    if err != nil {
        // 尝试故障转移
        return e.handleFallback(taskType, input, options, err)
    }

    // 6. 计算成本
    cost := e.calculateCost(selection.Model, request.PromptTokens, response.CompletionTokens)

    // 7. 记录使用日志
    log := &ModelUsageLog{
        ModelID:       selection.Model.ID,
        TaskType:      taskType,
        InputTokens:   request.PromptTokens,
        OutputTokens:  response.CompletionTokens,
        TotalTokens:   request.PromptTokens + response.CompletionTokens,
        Cost:          cost,
        Latency:       latency,
        Success:       true,
    }

    e.db.Create(log)

    // 8. 返回结果
    result := &TaskResult{
        Output:       response.Text,
        ModelUsed:    selection.Model,
        TokensUsed:   response.CompletionTokens,
        Cost:         cost,
        Latency:      latency,
        Selection:    selection,
    }

    return result, nil
}

// 故障转移
func (e *ModelExecutor) handleFallback(
    taskType TaskType,
    input *TaskInput,
    options *ExecutionOptions,
    originalError error,
) (*TaskResult, error) {
    // 1. 获取任务配置
    config := e.getTaskConfig(taskType)

    // 2. 解析备选模型
    var fallbackModelIDs []uint
    json.Unmarshal([]byte(config.FallbackModelIDs), &fallbackModelIDs)

    // 3. 尝试备选模型
    for _, modelID := range fallbackModelIDs {
        model := e.registry.GetModel(modelID)

        // 获取客户端
        client := e.registry.GetClient(model.ProviderID)

        // 构建请求
        request := e.buildRequest(input, model, options.Parameters)

        // 执行请求
        response, err := client.GenerateCompletion(request)

        if err == nil {
            // 成功，返回结果
            cost := e.calculateCost(model, request.PromptTokens, response.CompletionTokens)
            latency := time.Since(time.Now()).Seconds()

            result := &TaskResult{
                Output:       response.Text,
                ModelUsed:    model,
                TokensUsed:   response.CompletionTokens,
                Cost:         cost,
                Latency:      latency,
                FallbackUsed: true,
                OriginalError: originalError.Error(),
            }

            return result, nil
        }
    }

    // 所有备选模型都失败
    return nil, originalError
}

// 批量执行任务（用于对比）
func (e *ModelExecutor) ExecuteBatch(
    taskType TaskType,
    input *TaskInput,
    modelIDs []uint,
) []*TaskResult {
    results := make(chan *TaskResult, len(modelIDs))

    for _, modelID := range modelIDs {
        go func(id uint) {
            model := e.registry.GetModel(id)
            client := e.registry.GetClient(model.ProviderID)

            request := e.buildRequest(input, model, nil)

            startTime := time.Now()
            response, err := client.GenerateCompletion(request)
            latency := time.Since(startTime).Seconds()

            result := &TaskResult{
                Output:    response.Text,
                ModelUsed: model,
                TokensUsed: response.CompletionTokens,
                Cost:      e.calculateCost(model, request.PromptTokens, response.CompletionTokens),
                Latency:   latency,
                Success:   err == nil,
            }

            if err != nil {
                result.Error = err.Error()
            }

            results <- result
        }(modelID)
    }

    // 收集结果
    finalResults := []*TaskResult{}
    for i := 0; i < len(modelIDs); i++ {
        finalResults = append(finalResults, <-results)
    }

    return finalResults
}
```

---

## 🎨 前端界面设计

### 模型管理页面

```vue
<template>
  <div class="model-manager">
    <div class="header">
      <h2>模型管理</h2>
      <Button @click="handleAddProvider">添加提供商</Button>
      <Button @click="handleAddModel">添加模型</Button>
      <Button @click="handleCreateComparison">创建对比实验</Button>
    </div>

    <!-- 提供商列表 -->
    <div class="providers-section">
      <h3>模型提供商</h3>
      <div class="provider-list">
        <div
          v-for="provider in providers"
          :key="provider.id"
          class="provider-card"
          :class="{ 'health-error': provider.healthCheck !== 'ok' }"
        >
          <div class="provider-info">
            <h4>{{ provider.display_name }}</h4>
            <div class="status">
              <span :class="`status-${provider.healthCheck}`">
                {{ provider.healthCheck }}
              </span>
            </div>
          </div>
          <div class="provider-stats">
            <div class="stat">
              <span class="label">速率限制:</span>
              <span class="value">{{ provider.rate_limit }}/min</span>
            </div>
            <div class="stat">
              <span class="label">成本:</span>
              <span class="value">${{ provider.cost_per_1k }}/1K</span>
            </div>
          </div>
          <div class="provider-actions">
            <Button size="small" @click="handleEditProvider(provider)">编辑</Button>
            <Button size="small" @click="handleTestProvider(provider)">测试</Button>
          </div>
        </div>
      </div>
    </div>

    <!-- 模型列表 -->
    <div class="models-section">
      <h3>AI 模型</h3>
      <div class="model-grid">
        <div
          v-for="model in models"
          :key="model.id"
          class="model-card"
          :class="{ 'active': selectedModel?.id === model.id }"
          @click="handleSelectModel(model)"
        >
          <div class="model-header">
            <h4>{{ model.display_name }}</h4>
            <div class="provider-badge">{{ model.provider_name }}</div>
          </div>
          <div class="model-stats">
            <div class="stat">
              <span class="label">质量:</span>
              <div class="progress-bar">
                <div class="progress" :style="{ width: model.quality * 100 + '%' }"></div>
              </div>
              <span class="value">{{ (model.quality * 100).toFixed(0) }}%</span>
            </div>
            <div class="stat">
              <span class="label">速度:</span>
              <span class="value">{{ model.speed.toFixed(0) }} tok/s</span>
            </div>
            <div class="stat">
              <span class="label">成本:</span>
              <span class="value">${{ model.cost_per_1k }}/1K</span>
            </div>
            <div class="stat">
              <span class="label">上下文:</span>
              <span class="value">{{ model.context_window.toLocaleString() }}</span>
            </div>
          </div>
          <div class="model-capabilities">
            <span
              v-for="cap in model.capabilities"
              :key="cap"
              class="capability-badge"
            >
              {{ cap }}
            </span>
          </div>
          <div class="model-actions">
            <Button size="small" @click.stop="handleTestModel(model)">测试</Button>
            <Button size="small" @click.stop="handleCompareModel(model)">对比</Button>
          </div>
        </div>
      </div>
    </div>

    <!-- 任务配置 -->
    <div class="task-config-section" v-if="selectedModel">
      <h3>任务配置</h3>
      <div class="task-config">
        <div class="config-row">
          <label>任务类型:</label>
          <Select v-model="taskConfig.task_type">
            <Option value="novel_generation">小说生成</Option>
            <Option value="chapter_generation">章节生成</Option>
            <Option value="character_creation">角色创建</Option>
            <Option value="dialogue_generation">对话生成</Option>
            <Option value="storyboarding">分镜生成</Option>
          </Select>
        </div>
        <div class="config-row">
          <label>选择策略:</label>
          <Select v-model="taskConfig.strategy">
            <Option value="quality_first">质量优先</Option>
            <Option value="cost_first">成本优先</Option>
            <Option value="balanced">平衡模式</Option>
            <Option value="custom">自定义</Option>
          </Select>
        </div>
        <div class="config-row" v-if="taskConfig.strategy === 'custom'">
          <label>质量权重:</label>
          <Slider v-model="taskConfig.quality_weight" :min="0" :max="1" :step="0.1" />
          <label>成本权重:</label>
          <Slider v-model="taskConfig.cost_weight" :min="0" :max="1" :step="0.1" />
          <label>速度权重:</label>
          <Slider v-model="taskConfig.speed_weight" :min="0" :max="1" :step="0.1" />
        </div>
        <div class="config-row">
          <Button @click="handleSaveConfig">保存配置</Button>
        </div>
      </div>
    </div>

    <!-- 对比实验列表 -->
    <div class="experiments-section">
      <h3>对比实验</h3>
      <div class="experiment-list">
        <div
          v-for="experiment in experiments"
          :key="experiment.id"
          class="experiment-card"
          @click="handleViewExperiment(experiment)"
        >
          <div class="experiment-header">
            <h4>{{ experiment.name }}</h4>
            <span :class="`status-${experiment.status}`">{{ experiment.status }}</span>
          </div>
          <div class="experiment-info">
            <div class="info-item">
              <span class="label">任务:</span>
              <span class="value">{{ experiment.task_type }}</span>
            </div>
            <div class="info-item">
              <span class="label">模型数量:</span>
              <span class="value">{{ experiment.model_ids.length }}</span>
            </div>
            <div class="info-item" v-if="experiment.winner_model_id">
              <span class="label">获胜者:</span>
              <span class="value winner">{{ experiment.winner_model_name }}</span>
            </div>
          </div>
          <div class="experiment-progress" v-if="experiment.status === 'running'">
            <div class="progress-bar">
              <div class="progress" :style="{ width: experiment.progress + '%' }"></div>
            </div>
            <span class="value">{{ experiment.progress.toFixed(0) }}%</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
const providers = ref([])
const models = ref([])
const selectedModel = ref(null)
const taskConfig = ref({
  task_type: 'novel_generation',
  strategy: 'balanced',
  quality_weight: 0.5,
  cost_weight: 0.3,
  speed_weight: 0.2,
})
const experiments = ref([])

// 加载提供商
const loadProviders = async () => {
  const data = await $fetch('/api/model-providers')
  providers.value = data
}

// 加载模型
const loadModels = async () => {
  const data = await $fetch('/api/models')
  models.value = data
}

// 加载实验
const loadExperiments = async () => {
  const data = await $fetch('/api/model-comparisons')
  experiments.value = data
}

// 选择模型
const handleSelectModel = (model) => {
  selectedModel.value = model
}

// 保存配置
const handleSaveConfig = async () => {
  await $fetch('/api/task-model-config', {
    method: 'POST',
    body: taskConfig.value,
  })
}

// 创建对比实验
const handleCreateComparison = () => {
  // 打开创建对话框
}

// 查看实验
const handleViewExperiment = (experiment) => {
  // 显示实验详情
}

// 加载数据
onMounted(() => {
  loadProviders()
  loadModels()
  loadExperiments()
})
</script>

<style scoped>
.model-manager {
  padding: 2rem;
  background: #f5f5f5;
  min-height: 100vh;
}

.header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 2rem;
}

.section {
  margin-bottom: 2rem;
}

.provider-list,
.model-grid,
.experiment-list {
  display: grid;
  gap: 1rem;
}

.model-grid {
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
}

.card {
  background: white;
  border-radius: 8px;
  padding: 1.5rem;
  box-shadow: 0 2px 8px rgba(0,0,0,0.1);
}

.stat {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin-bottom: 0.5rem;
}

.progress-bar {
  flex: 1;
  height: 6px;
  background: #e0e0e0;
  border-radius: 3px;
  overflow: hidden;
}

.progress {
  height: 100%;
  background: #3b82f6;
  transition: width 0.3s;
}

.badge {
  display: inline-block;
  padding: 0.25rem 0.5rem;
  border-radius: 4px;
  font-size: 0.75rem;
  background: #e0e0e0;
}

.status-ok { color: #22c55e; }
.status-degraded { color: #f59e0b; }
.status-down { color: #ef4444; }

.winner {
  color: #22c55e;
  font-weight: bold;
}
</style>
```

---

## 📊 API 设计

### 模型管理 API

```
GET    /api/model-providers              # 获取提供商列表
POST   /api/model-providers              # 添加提供商
GET    /api/model-providers/:id          # 获取提供商详情
PUT    /api/model-providers/:id          # 更新提供商
DELETE /api/model-providers/:id          # 删除提供商
POST   /api/model-providers/:id/test     # 测试提供商
GET    /api/model-providers/health        # 健康检查

GET    /api/models                       # 获取模型列表
POST   /api/models                       # 添加模型
GET    /api/models/:id                   # 获取模型详情
PUT    /api/models/:id                   # 更新模型
DELETE /api/models/:id                   # 删除模型
GET    /api/models/:id/test              # 测试模型
GET    /api/models/available/:task_type  # 获取可用模型（按任务）

GET    /api/task-model-configs            # 获取任务配置列表
POST   /api/task-model-configs            # 创建任务配置
GET    /api/task-model-configs/:id        # 获取配置详情
PUT    /api/task-model-configs/:id        # 更新配置
DELETE /api/task-model-configs/:id        # 删除配置
```

### 模型选择 API

```
POST   /api/model/select                 # 选择模型
GET    /api/model/recommendations/:task_type  # 获取推荐模型
POST   /api/model/validate-selection      # 验证模型选择
```

### 模型对比 API

```
GET    /api/model-comparisons             # 获取对比实验列表
POST   /api/model-comparisons             # 创建对比实验
GET    /api/model-comparisons/:id         # 获取实验详情
POST   /api/model-comparisons/:id/run     # 运行实验
DELETE /api/model-comparisons/:id         # 删除实验
GET    /api/model-comparisons/:id/report  # 获取对比报告
GET    /api/model-comparisons/:id/results # 获取实验结果
```

### 任务执行 API

```
POST   /api/tasks/execute                # 执行任务
POST   /api/tasks/execute-batch          # 批量执行（对比）
GET    /api/tasks/:id                    # 获取任务结果
POST   /api/tasks/:id/rerun              # 重新执行任务
```

### 统计 API

```
GET    /api/model-usage                   # 获取使用记录
GET    /api/model-usage/aggregate         # 获取使用统计
GET    /api/model-usage/:model_id         # 获取指定模型使用记录
GET    /api/model-performance            # 获取性能统计
GET    /api/model-performance/:model_id   # 获取指定模型性能
GET    /api/model-performance/:model_id/trends  # 获取性能趋势
```

---

## 🚀 部署方案

### 环境变量配置

```bash
# OpenAI
OPENAI_API_KEY=sk-xxx
OPENAI_API_ENDPOINT=https://api.openai.com/v1

# Anthropic
ANTHROPIC_API_KEY=sk-ant-xxx
ANTHROPIC_API_ENDPOINT=https://api.anthropic.com/v1

# Google
GOOGLE_API_KEY=xxx
GOOGLE_API_ENDPOINT=https://generativelanguage.googleapis.com/v1

# Meta (Llama)
LLAMA_API_KEY=xxx
LLAMA_API_ENDPOINT=https://api.meta.com/llama/v1

# Alibaba (Qwen)
QWEN_API_KEY=xxx
QWEN_API_ENDPOINT=https://dashscope.aliyuncs.com/api/v1

# Local Models
LOCAL_MODEL_ENDPOINT=http://localhost:11434/v1

# Redis
REDIS_HOST=localhost:6379

# Database
DB_HOST=localhost:3306
DB_USER=root
DB_PASSWORD=password
DB_NAME=inkframe
```

---

## 📝 开发路线图

### Phase 1: 基础模型管理 - 2 周

- [ ] 模型提供商注册和管理
- [ ] 模型注册和配置
- [ ] 基础模型执行
- [ ] 使用记录和统计

### Phase 2: 智能选择系统 - 2 周

- [ ] 模型选择器实现
- [ ] 多种选择策略
- [ ] 约束系统
- [ ] 故障转移机制

### Phase 3: 对比实验系统 - 2 周

- [ ] 对比实验创建
- [ ] 并行执行
- [ ] 结果分析
- [ ] 报告生成

### Phase 4: 前端界面 - 2 周

- [ ] 模型管理页面
- [ ] 对比实验界面
- [ ] 实时监控
- [ ] 配置管理

### Phase 5: 优化和扩展 - 2 周

- [ ] 性能优化
- [ ] 缓存策略
- [ ] 更多模型集成
- [ ] 自定义模型支持

**总计：10 周（约 2.5 个月）**

---

## 📈 预期效果

### 功能指标

| 指标 | 目标值 |
|-----|-------|
| 支持模型数量 | 10+ |
| 支持提供商数量 | 6+ |
| 任务类型数量 | 10+ |
| 选择策略数量 | 4+ |
| 对比实验并发数 | 5+ |
| 模型切换成功率 | 99% |

### 性能指标

| 指标 | 目标值 |
|-----|-------|
| 模型选择延迟 | < 100ms |
| 故障转移时间 | < 5s |
| 对比实验执行时间 | < 2分钟/模型 |
| 缓存命中率 | > 80% |

### 质量指标

| 指标 | 目标值 |
|-----|-------|
| 模型选择准确率 | > 90% |
| 故障转移成功率 | > 95% |
| 对比结果一致性 | > 85% |

---

## 🎯 总结

InkFrame 多模型管理系统提供了：

1. **灵活的模型集成**：支持 OpenAI、Claude、Gemini、Llama、Qwen 等主流模型
2. **智能选择策略**：质量优先、成本优先、平衡模式、自定义策略
3. **强大的对比功能**：A/B 测试、效果对比、自动评估
4. **完善的监控体系**：性能统计、成本分析、质量评估
5. **可靠的质量保证**：故障转移、健康检查、约束验证

通过这套系统，用户可以根据不同任务选择最合适的模型，优化质量和成本的平衡。

---

**InkFrame Multi-Model** - 灵活、智能、高效的模型管理平台 🤖✨
