package service

import (
	"context"
	"fmt"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/commons"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// taskConfig holds per-call AI generation parameters (replaces the removed TaskModelConfig DB model).
type taskConfig struct {
	Temperature    float64
	TopP           float64
	TopK           int
	MaxTokens      int
	TimeoutSeconds int
	// PrimaryModelID is used only for usage logging; 0 is acceptable.
	PrimaryModelID uint
}

// Embed 对文本执行向量嵌入，使用 DB 中配置的 embedding 类型提供商。
// 调用受 per-provider 并发队列约束。
func (s *AIService) Embed(ctx context.Context, tenantID uint, text string) ([]float32, error) {
	if s.providerRepo == nil {
		return nil, fmt.Errorf("provider repository not configured")
	}
	provider, model, err := s.getTenantProvider(tenantID, commons.Embedding, "")
	if err != nil || provider == nil {
		return nil, fmt.Errorf("no embedding provider configured")
	}
	release, acquireErr := s.acquireModelSlotByName(ctx, tenantID, model.Name)
	if acquireErr != nil {
		return nil, acquireErr
	}
	defer release()
	return provider.Embed(ctx, text)
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, taskType, prompt)
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
func (s *AIService) GenerateWithProvider(tenantID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProviderCtx(context.Background(), tenantID, taskType, prompt)
}

// GenerateWithProviderCtx is like GenerateWithProvider but respects an external context.
// Use this when the caller holds a cancellable context (e.g. async task with cancel support).
func (s *AIService) GenerateWithProviderCtx(ctx context.Context, tenantID uint, taskType string, prompt string) (string, error) {
	//config, providerName, resolvedModel, novelGenre := s.resolveNovelAIConfig(tenantID, novelID, providerName)

	config := taskConfig{}
	switch taskType {
	// 结构化提取/审查任务：需要严格 JSON 输出，用低温度
	case "character_state", "scene_anchor_extract", "storyboard_review", "sfx_analyze", "chapter_end_state":
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	// 小说正文创作任务：需要充足的创意探索空间，温度不低于 0.8
	case "chapter", "scene_outline", "chapter_outline":
		if config.Temperature < 0.8 {
			config.Temperature = 0.8
		}
	// 编辑/精修任务：精确替换词语、统一风格，不需要高创意多样性，温度上限 0.4
	case "refinement":
		if config.Temperature > 0.4 {
			config.Temperature = 0.4
		}
	// 其他创意生成任务：需要多样性和表达力，温度不低于 0.5
	case "storyboard", "character", "worldview":
		if config.Temperature < 0.5 {
			config.Temperature = 0.5
		}
	}

	// 任务类型兜底 MaxTokens（仅在配置链全为 0 时生效）。
	if config.MaxTokens == 0 {
		switch taskType {
		case "storyboard":
			config.MaxTokens = 16384
		case "outline":
			config.MaxTokens = 32768
		case "chapter_review", "storyboard_review":
			config.MaxTokens = 8192
		case "chapter_end_state":
			config.MaxTokens = 2048
		case "storyboard_arc":
			config.MaxTokens = 6000
		case "screenplay_generate":
			// 分场剧本是整章一次性生成（不像 storyboard 按内容分段调用），需要覆盖全章
			// 所有场次+每场详细节拍，输出规模与 storyboard 同量级，之前没有兜底值导致
			// config.MaxTokens 停留在 0（未配置 provider/model 时），生成中途被截断，
			// 只产出章节前半部分的场次。
			config.MaxTokens = 16384
		}
	}

	resp, err := s.callAIWithProviderSys(ctx, tenantID, prompt, &config)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}
	return resp.Content, nil
}

// GenerateWithMessagesCtx calls the AI with a full conversation history (messages array).
// Unlike GenerateWithProviderCtx which takes a single prompt string, this method passes
// the complete message thread natively so the model sees proper role-based multi-turn context.
func (s *AIService) GenerateWithMessagesCtx(ctx context.Context, tenantID uint, taskType string, messages []ai.ChatMessage, systemPrompt string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}
	provider, m, err := s.getTenantProvider(tenantID, commons.LLM, "")
	if err != nil {
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return "", fmt.Errorf("AI provider resolved to nil for %q", provider.GetName())
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    m.MaxTokens,
		//Temperature:  m.Temperature,
		//TopP:         m.TopP,
	}

	timeoutDur := 5 * time.Minute
	if m.Timeout > 0 {
		timeoutDur = time.Duration(m.Timeout) * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeoutDur)
	defer cancel()

	logger.Printf("[AI/chat] provider=%s maxTokens=%d temperature=%.2f msgs=%d calling...",
		provider.GetName(), req.MaxTokens, req.Temperature, len(messages))

	callStart := time.Now()
	resp, err := provider.Generate(tctx, req)
	elapsed := time.Since(callStart)
	if err != nil {
		logger.Errorf("[AI/chat] provider=%s elapsed=%s err=%v", provider.GetName(), elapsed.Round(time.Millisecond), err)
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	resp.FinishTime = elapsed.Milliseconds()
	logger.Printf("[AI/chat] provider=%s elapsed=%s respLen=%d in=%d out=%d",
		provider.GetName(), elapsed.Round(time.Millisecond), len(resp.Content), resp.InputTokens, resp.Tokens)
	if resp.Content == "" {
		return "", fmt.Errorf("provider returned empty content (stop_reason=%s)", resp.StopReason)
	}
	s.logUsage(tenantID, m.ID, taskType, resp, elapsed.Milliseconds())
	return resp.Content, nil
}

// StreamWithMessagesCtx streams AI response tokens for a multi-turn conversation.
// It returns a channel that emits content chunks; the caller must drain the channel fully.
// The last item may carry an empty Content with a non-empty Error field.
func (s *AIService) StreamWithMessagesCtx(ctx context.Context, tenantID uint, messages []ai.ChatMessage, systemPrompt string, overrides ...StoryboardOverrides) (<-chan *ai.GenerateResponse, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, m, err := s.getTenantProvider(tenantID, commons.LLM, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get AI provider: %w", err)
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    m.MaxTokens,
		//Temperature:  config.Temperature,
		//TopP:         config.TopP,
		Stream: true,
	}
	timeoutDur := 5 * time.Minute
	if m.Timeout > 0 {
		timeoutDur = time.Duration(m.Timeout) * time.Second
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeoutDur)

	ch, err := provider.GenerateStream(streamCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Wrap the provider channel to ensure cancel is called when the stream ends.
	wrapped := make(chan *ai.GenerateResponse, 64)
	go func() {
		defer cancel()
		defer close(wrapped)
		for chunk := range ch {
			wrapped <- chunk
		}
	}()

	return wrapped, nil
}

func (s *AIService) callAIWithProviderSys(ctx context.Context, tenantID uint, prompt string, config *taskConfig, modelOverride ...string) (*ai.GenerateResponse, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}

	var modelName string
	if len(modelOverride) > 0 {
		modelName = modelOverride[0]
	}
	provider, m, err := s.getTenantProvider(tenantID, commons.LLM, modelName)
	if err != nil {
		logger.Errorf("callAIWithProvider: getTenantProvider failed (tenant=%d): %v", tenantID, err)
		return nil, fmt.Errorf("failed to get AI provider: %w", err)
	}
	release, err := s.acquireModelSlotByName(ctx, tenantID, modelName)
	if err != nil {
		return nil, err
	}
	defer release()
	// 防止调用方传入超过模型限制的 max_tokens
	if m.MaxTokens > 0 && config.MaxTokens > m.MaxTokens {
		config.MaxTokens = m.MaxTokens
	}

	// 执行超时：在获取并发槽之后创建，确保超时只计算实际 API 调用时间。
	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutDur)
	defer cancel()

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{{Role: "user", Content: prompt}},
		//SystemPrompt: systemPrompt,
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
		TopP:        config.TopP,
	}
	if modelName != "" {
		req.Model = modelName
	}
	// Claude 不支持 top_k，仅在非 Anthropic provider 时传入
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}

	logger.Printf("[AI] provider=%s maxTokens=%d temperature=%.2f calling...", provider.GetName(), req.MaxTokens, req.Temperature)
	callStart := time.Now()
	resp, err := provider.Generate(ctx, req)
	elapsed := time.Since(callStart)
	if err != nil {
		logger.Errorf("[AI] provider=%s elapsed=%s err=%v", provider.GetName(), elapsed.Round(time.Millisecond), err)
		return nil, err
	}
	if resp.Error != "" {
		logger.Errorf("[AI] provider=%s elapsed=%s providerErr=%s", provider.GetName(), elapsed.Round(time.Millisecond), resp.Error)
		return nil, fmt.Errorf("provider error: %s", resp.Error)
	}
	resp.FinishTime = elapsed.Milliseconds()
	logger.Printf("[AI] provider=%s elapsed=%s maxTokens=%d respLen=%d in=%d out=%d stopReason=%q",
		provider.GetName(), elapsed.Round(time.Millisecond), req.MaxTokens, len(resp.Content),
		resp.InputTokens, resp.Tokens, resp.StopReason)

	if resp.Content == "" {
		return nil, fmt.Errorf("provider returned empty content (stop_reason=%s)", resp.StopReason)
	}
	return resp, nil
}

// logUsage records a ModelUsageLog entry using token counts and latency from the response.
// Fix 1: accepts tenantID and uses resp.ActualModelID when available (Fix 4).
func (s *AIService) logUsage(tenantID uint, modelId uint, taskType string, resp *ai.GenerateResponse, latencyMs int64) {
	if s.modelRepo == nil || resp == nil {
		return
	}
	entry := &model.ModelUsageLog{
		TenantID:     tenantID,
		ModelID:      modelId,
		TaskType:     taskType,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.Tokens,
		TotalTokens:  resp.InputTokens + resp.Tokens,
		Cost:         0, // 无 cost_per_1k_tokens 数据源，暂记 0，待后续在 AIModel 补充单价字段
		Latency:      float64(latencyMs) / 1000,
		Success:      true,
	}
	if err := s.modelRepo.LogUsage(entry); err != nil {
		logger.Errorf("[AI] logUsage failed: %v", err)
	}
}
