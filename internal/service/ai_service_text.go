package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/metrics"
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
	provider, provName, err := s.loadDBProviderByType(tenantID, "embedding")
	if err != nil || provider == nil {
		return nil, fmt.Errorf("no embedding provider configured")
	}
	release, acquireErr := s.acquireProviderSlot(ctx, tenantID, provName)
	if acquireErr != nil {
		return nil, acquireErr
	}
	defer release()
	return provider.Embed(ctx, text)
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, novelID, taskType, prompt, "")
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
func (s *AIService) GenerateWithProvider(tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	return s.GenerateWithProviderCtx(context.Background(), tenantID, novelID, taskType, prompt, providerName, overrides...)
}

// GenerateWithProviderCtx is like GenerateWithProvider but respects an external context.
// Use this when the caller holds a cancellable context (e.g. async task with cancel support).
func (s *AIService) GenerateWithProviderCtx(ctx context.Context, tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	config, providerName, resolvedModel, novelGenre := s.resolveNovelAIConfig(tenantID, novelID, providerName)

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

	// taskType 温度调整必须在 overrides/自动选型之前生效，这样显式 override 才能在最后
	// 覆盖掉 taskType 的温度下限/上限（与原实现顺序一致：override 优先级最高）。
	providerName, resolvedModel = s.applyOverridesAndAutoSelect(&config, taskType, tenantID, providerName, resolvedModel, overrides)

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

	sysPmt := ""
	if chapterTaskTypes[taskType] {
		sysPmt = buildNovelWritingSystemPrompt(novelGenre)
	} else if jsonOnlyTaskTypes[taskType] {
		sysPmt = jsonOnlySystemPrompt
	}

	effectiveProvider := providerName
	if effectiveProvider == "" {
		effectiveProvider = "default"
	}
	metrics.AIRequestsInFlight.WithLabelValues(taskType, effectiveProvider).Inc()
	callStart := time.Now()
	result, resp, err := s.callAIWithProviderSys(ctx, tenantID, prompt, sysPmt, &config, providerName, resolvedModel)
	elapsed := time.Since(callStart).Seconds()
	metrics.AIRequestsInFlight.WithLabelValues(taskType, effectiveProvider).Dec()

	if err != nil {
		metrics.AIRequestsTotal.WithLabelValues(taskType, effectiveProvider, "error").Inc()
		return "", fmt.Errorf("AI generation failed: %w", err)
	}
	metrics.AIRequestsTotal.WithLabelValues(taskType, effectiveProvider, "success").Inc()
	metrics.AIRequestDuration.WithLabelValues(taskType, effectiveProvider).Observe(elapsed)
	if resp.InputTokens > 0 {
		metrics.AITokensTotal.WithLabelValues(taskType, effectiveProvider, "prompt").Add(float64(resp.InputTokens))
	}
	if resp.Tokens > 0 {
		metrics.AITokensTotal.WithLabelValues(taskType, effectiveProvider, "completion").Add(float64(resp.Tokens))
	}
	s.logUsage(tenantID, &config, taskType, resp, time.Since(callStart).Milliseconds())
	return result, nil
}

// resolveNovelAIConfig 读取小说级 AI 配置（temperature/topP/maxTokens/model）填入 config；
// 未显式指定 provider 时，尝试把 novel.AIConfig.AIModel 解析成对应且有凭据的 provider。
// 由 GenerateWithProviderCtx（额外在此基础上应用 taskType 温度上下限）和 resolveTaskConfig
// （直接应用 overrides）共用。
func (s *AIService) resolveNovelAIConfig(tenantID, novelID uint, providerName string) (config taskConfig, resolvedProviderName, resolvedModel, novelGenre string) {
	config = taskConfig{Temperature: 0.7, MaxTokens: 0}
	resolvedProviderName = providerName
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			novelGenre = novel.Meta.Genre
			if novel.AIConfig.Temperature > 0 {
				config.Temperature = novel.AIConfig.Temperature
			}
			if novel.AIConfig.TopP > 0 {
				config.TopP = novel.AIConfig.TopP
			}
			if novel.AIConfig.MaxTokens > 0 {
				config.MaxTokens = novel.AIConfig.MaxTokens
			}
			if resolvedProviderName == "" && novel.AIConfig.AIModel != "" {
				// resolvedModel 只有在真正找到该模型对应且有凭据的 provider 时才设置——
				// 否则会出现"model 名字留着旧值，但 provider 被自动换成别的、完全不
				// 相关的 provider"的错位请求。找不到时把 resolvedModel 留空，交给下面
				// "Auto-select from active models" 从该租户已激活的模型里选一个。
				if pName := s.resolveProviderFromModel(tenantID, novel.AIConfig.AIModel); pName != "" {
					resolvedModel = novel.AIConfig.AIModel
					resolvedProviderName = pName
				}
			}
		}
	}
	return config, resolvedProviderName, resolvedModel, novelGenre
}

// applyOverridesAndAutoSelect 应用调用方显式传入的 overrides，然后——若此时仍未锁定
// provider/model——从该租户已激活的模型里按任务类型自动选一个，最后用解析出的模型的
// AIModel.MaxTokens 补齐仍为 0 的 config.MaxTokens。
func (s *AIService) applyOverridesAndAutoSelect(config *taskConfig, taskType string, tenantID uint, providerName, resolvedModel string, overrides []StoryboardOverrides) (string, string) {
	if len(overrides) > 0 {
		o := overrides[0]
		if o.MaxTokens > 0 {
			config.MaxTokens = o.MaxTokens
		}
		if o.Temperature > 0 {
			config.Temperature = o.Temperature
		}
		if o.TimeoutSeconds > 0 {
			config.TimeoutSeconds = o.TimeoutSeconds
		}
	}

	// Auto-select from active models when no provider is explicitly requested.
	if resolvedModel == "" && providerName == "" && s.modelRepo != nil {
		if candidates, err := s.modelRepo.GetAvailableByTaskType(taskType, tenantID); err == nil && len(candidates) > 0 {
			selected := selectBalanced(candidates)
			if selected != nil && selected.Provider != nil {
				resolvedModel = selected.Name
				providerName = selected.Provider.Name
				if config.MaxTokens == 0 && selected.MaxTokens > 0 {
					config.MaxTokens = selected.MaxTokens
				}
			}
		}
	}

	if config.MaxTokens == 0 && resolvedModel != "" && s.modelRepo != nil {
		if m, err := s.modelRepo.GetByName(resolvedModel); err == nil && m != nil && m.MaxTokens > 0 {
			config.MaxTokens = m.MaxTokens
		}
	}

	return providerName, resolvedModel
}

// resolveTaskConfig 提取 GenerateWithProviderCtx 中的配置解析逻辑，供多轮/流式方法复用。
// 返回已填充好参数的 config、最终 providerName、resolvedModel。
func (s *AIService) resolveTaskConfig(tenantID uint, novelID uint, taskType string, providerName string, overrides []StoryboardOverrides) (taskConfig, string, string) {
	config, providerName, resolvedModel, _ := s.resolveNovelAIConfig(tenantID, novelID, providerName)
	providerName, resolvedModel = s.applyOverridesAndAutoSelect(&config, taskType, tenantID, providerName, resolvedModel, overrides)
	return config, providerName, resolvedModel
}

// GenerateWithMessagesCtx calls the AI with a full conversation history (messages array).
// Unlike GenerateWithProviderCtx which takes a single prompt string, this method passes
// the complete message thread natively so the model sees proper role-based multi-turn context.
func (s *AIService) GenerateWithMessagesCtx(ctx context.Context, tenantID uint, taskType string, messages []ai.ChatMessage, systemPrompt string, overrides ...StoryboardOverrides) (string, error) {
	config, providerName, resolvedModel := s.resolveTaskConfig(tenantID, 0, taskType, "", overrides)

	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return "", fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
	}
	if resolvedModel != "" {
		req.Model = resolvedModel
	}
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}

	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
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
	s.logUsage(tenantID, &config, taskType, resp, elapsed.Milliseconds())
	return resp.Content, nil
}

// StreamWithMessagesCtx streams AI response tokens for a multi-turn conversation.
// It returns a channel that emits content chunks; the caller must drain the channel fully.
// The last item may carry an empty Content with a non-empty Error field.
func (s *AIService) StreamWithMessagesCtx(ctx context.Context, tenantID uint, taskType string, messages []ai.ChatMessage, systemPrompt string, overrides ...StoryboardOverrides) (<-chan *ai.GenerateResponse, error) {
	config, providerName, resolvedModel := s.resolveTaskConfig(tenantID, 0, taskType, "", overrides)

	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		return nil, fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return nil, fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}

	req := &ai.GenerateRequest{
		Messages:     messages,
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
		Stream:       true,
	}
	if resolvedModel != "" {
		req.Model = resolvedModel
	}
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}

	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
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

// resolveProviderFromModel 通过模型名（如 "deepseek-chat"）在 DB 中查找对应的 provider name（如 "deepseek"）
// 若查找失败则静默返回空字符串（由 getTenantProvider 兜底选择第一个可用 provider）
func (s *AIService) resolveProviderFromModel(tenantID uint, modelName string) string {
	if s.modelRepo == nil {
		return ""
	}
	m, err := s.modelRepo.GetByName(modelName)
	if err != nil || m == nil || m.Provider == nil {
		return ""
	}
	providerName := m.Provider.Name
	// 确认该 provider 对当前租户可用（有凭证）
	if s.providerRepo != nil {
		providers, err := s.providerRepo.ListByTenant(tenantID)
		if err == nil {
			for _, p := range providers {
				if p.Name == providerName && p.IsActive && providerHasCredentials(p) {
					return providerName
				}
			}
		}
		return "" // provider 无凭证，让 getTenantProvider 自动选择
	}
	return providerName
}

// GenerateWithVision 使用 Vision AI 分析图像内容。
// 使用该租户配置的默认 LLM provider（与其他未显式指定 provider 的调用一致），不再硬编码
// 偏好 anthropic/openai——那样会无视用户在模型管理里为 LLM 任务实际配置的 provider。
// 若解析出的 provider/模型不支持视觉输入，应让 API 报错，而不是绕开配置去挑一个
// "看起来支持视觉"的 provider。
func (s *AIService) GenerateWithVision(tenantID uint, prompt string, imageURLs []string) (string, error) {
	provider, err := s.getTenantProvider(tenantID, "")
	if err != nil {
		return "", fmt.Errorf("failed to get AI provider for vision: %w", err)
	}

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{
			{
				Role:      "user",
				Content:   prompt,
				ImageURLs: imageURLs,
			},
		},
		Temperature: 0.1,
	}

	resp, err := provider.Generate(context.Background(), req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	return resp.Content, nil
}

// callAIWithProvider 调用指定 Provider 的 AI 接口
// parentCtx 作为父 context；timeout 会在其上叠加（不会超出父 context 的 deadline）。
// modelOverride 可选，非空时会覆盖 provider 的默认模型（用于小说项目级 ai_model 配置）
// buildNovelWritingSystemPrompt returns the system prompt for novel prose generation tasks.
// It injects a brief genre-specific writing identity so the model anchors its creative voice.
func buildNovelWritingSystemPrompt(genre string) string {
	genreLabel := genreSystemLabel(genre)
	base := "你是一位专业的中文小说作家，以卓越的叙事技艺创作高质量长篇小说。"
	if genreLabel != "" {
		base += genreLabel
	}
	return base + `

你的创作核心原则：
- 让角色通过行动、细节、对话潜台词表达情绪，而非直接陈述内心状态
- 通过人物独特的内在矛盾驱动情节，每个选择都源自角色深层动机
- 每个场景既完成当下叙事目标，又为后续埋下有机伏笔

输出规则（任何情况下不得违反）：
- 只输出小说正文，从章节标题行开始，到正文自然结束为止
- 禁止任何开场白（"好的""当然可以""非常抱歉""由于篇幅限制"等）
- 禁止在正文外输出大纲、章节摘要、写作建议或元注释
- 禁止声明字数/篇幅限制，禁止请求用户确认续写
- 禁止在正文结束后追加任何说明文字
- 字数不足时直接写到章末钩子，不得截断并附注"待续"类说明`
}

// jsonOnlySystemPrompt is injected for structured JSON output tasks.
// It suppresses chain-of-thought reasoning that reasoning models (e.g. DeepSeek-R1) emit by default.
const jsonOnlySystemPrompt = `你是一个严格的JSON生成引擎。

规则（任何情况下不得违反）：
- 只输出纯JSON，不输出任何分析、推理、思考过程或说明文字
- 禁止输出"我们被要求""根据分析""综上所述"等任何前缀或后缀
- 禁止markdown代码块（不要用` + "```" + `包裹）
- 直接以 [ 或 { 开始，以 ] 或 } 结束
- 每个键值对必须用英文冒号 : 分隔（"key": value），不得省略冒号
- 禁止添加 schema 示例中未定义的额外字段`

// chapterTaskTypes is the set of task types that generate novel prose.
var chapterTaskTypes = map[string]bool{
	"chapter": true, "chapter_outline": true, "scene_outline": true,
}

// jsonOnlyTaskTypes is the set of task types that must output pure JSON.
// These tasks get jsonOnlySystemPrompt injected to suppress reasoning model chain-of-thought.
var jsonOnlyTaskTypes = map[string]bool{
	"storyboard": true, "character": true, "worldview": true,
	"character_state": true, "scene_anchor_extract": true,
	"storyboard_review": true, "sfx_analyze": true,
	"chapter_review": true, "extract_items": true,
	"chapter_end_state": true, // 章末状态快照：纯 JSON，抑制推理模型思维链
	"outline":           true, // 大纲生成：强制纯 JSON，防止 DeepSeek 输出思考过程或缺失冒号
	// 角色/道具/世界观提取——均输出 JSON，需抑制推理模型的思维链输出
	"extract_characters":          true,
	"extract_character_names":     true,
	"consolidate_character_names": true,
	"generate_character_profile":  true,
	"extract_minor_characters":    true,
	"extract_chapter_items":       true,
	"extract_worldview":           true,
	"extract_foreshadows":         true,
}

func (s *AIService) callAIWithProvider(parentCtx context.Context, tenantID uint, prompt string, config *taskConfig, providerName string, modelOverride ...string) (string, error) {
	content, _, err := s.callAIWithProviderSys(parentCtx, tenantID, prompt, "", config, providerName, modelOverride...)
	return content, err
}

func (s *AIService) callAIWithProviderSys(parentCtx context.Context, tenantID uint, prompt string, systemPrompt string, config *taskConfig, providerName string, modelOverride ...string) (string, *ai.GenerateResponse, error) {
	if s.aiManager == nil {
		return "", nil, fmt.Errorf("AI manager not initialized")
	}

	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		logger.Errorf("callAIWithProvider: getTenantProvider failed (tenant=%d, provider=%q): %v", tenantID, providerName, err)
		return "", nil, fmt.Errorf("failed to get AI provider: %w", err)
	}
	if provider == nil {
		return "", nil, fmt.Errorf("AI provider resolved to nil for %q", providerName)
	}
	// 按 (tenantID, modelName) 进行共享并发控制 + 速率限制，同时限制 MaxTokens。
	// 注意：槽位获取必须在创建执行超时 ctx 之前，避免排队等待消耗执行超时配额。
	// Acquire 内部使用独立的 maxQueueWait 计时器（默认30分钟），不受下方执行超时影响。
	modelName := ""
	if len(modelOverride) > 0 {
		modelName = modelOverride[0]
	}
	if modelName != "" && s.modelRepo != nil {
		if m, err2 := s.modelRepo.GetByName(modelName); err2 == nil {
			if lim := s.getModelLimiter(tenantID, modelName, m.Concurrency, m.RateLimit); lim != nil {
				if acquireErr := lim.Acquire(parentCtx); acquireErr != nil {
					return "", nil, fmt.Errorf("model %s: %w", modelName, acquireErr)
				}
				defer lim.Release()
			}
			// 防止调用方传入超过模型限制的 max_tokens
			if m.MaxTokens > 0 && config.MaxTokens > m.MaxTokens {
				config.MaxTokens = m.MaxTokens
			}
		}
	}

	// 执行超时：在获取并发槽之后创建，确保超时只计算实际 API 调用时间。
	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeoutDur)
	defer cancel()

	req := &ai.GenerateRequest{
		Messages:     []ai.ChatMessage{{Role: "user", Content: prompt}},
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
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
		return "", nil, err
	}
	if resp.Error != "" {
		logger.Errorf("[AI] provider=%s elapsed=%s providerErr=%s", provider.GetName(), elapsed.Round(time.Millisecond), resp.Error)
		return "", nil, fmt.Errorf("provider error: %s", resp.Error)
	}
	resp.FinishTime = elapsed.Milliseconds()
	logger.Printf("[AI] provider=%s elapsed=%s maxTokens=%d respLen=%d in=%d out=%d stopReason=%q",
		provider.GetName(), elapsed.Round(time.Millisecond), req.MaxTokens, len(resp.Content),
		resp.InputTokens, resp.Tokens, resp.StopReason)

	if resp.Content == "" {
		return "", nil, fmt.Errorf("provider returned empty content (stop_reason=%s)", resp.StopReason)
	}
	return resp.Content, resp, nil
}

// generateJSONForTenantCtx 带 tenantID 的 JSON 生成重试（最多重试 maxRetries 次），支持 context 取消/超时。
func (s *AIService) generateJSONForTenantCtx(ctx context.Context, tenantID, novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("generateJSONForTenantCtx: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.GenerateWithProviderCtx(ctx, tenantID, novelID, taskType, p, "")
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = err
			// 4xx provider errors (e.g. "max_tokens too large") are not retryable
			if strings.Contains(err.Error(), "provider error:") {
				logger.Errorf("generateJSONForTenantCtx: non-retryable provider error on attempt %d taskType=%s: %v", attempt+1, taskType, err)
				break
			}
			continue
		}
		cleaned := extractJSONAuto(result)
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		logger.Errorf("generateJSONForTenantCtx: %v", lastErr)
	}
	return "", fmt.Errorf("generateJSONForTenantCtx failed after %d attempts: %w", maxRetries+1, lastErr)
}

// logUsage records a ModelUsageLog entry using token counts and latency from the response.
// Fix 1: accepts tenantID and uses resp.ActualModelID when available (Fix 4).
func (s *AIService) logUsage(tenantID uint, config *taskConfig, taskType string, resp *ai.GenerateResponse, latencyMs int64) {
	if s.modelRepo == nil || resp == nil {
		return
	}
	modelID := config.PrimaryModelID
	if resp.ActualModelID > 0 {
		modelID = resp.ActualModelID
	}
	entry := &model.ModelUsageLog{
		TenantID:     tenantID,
		ModelID:      modelID,
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
