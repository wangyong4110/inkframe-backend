package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/storage"
)

type AIService struct {
	modelRepo     *repository.AIModelRepository
	taskRepo      *repository.TaskModelConfigRepository
	aiManager     *ai.ModelManager
	providerRepo  *repository.ModelProviderRepository
	novelRepo     *repository.NovelRepository
	storageSvc    storage.Service
	taskRouting   TaskRouting
	providerCache sync.Map      // key: "tenantID:providerName" → providerCacheEntry
	imageSem      chan struct{} // nil = unlimited; set via WithImageConcurrency
	semMu         sync.RWMutex  // protects imageSem replacement
	stopCh        chan struct{} // closed by Shutdown() to stop background goroutines
}

func NewAIService(
	modelRepo *repository.AIModelRepository,
	taskRepo *repository.TaskModelConfigRepository,
	aiManager *ai.ModelManager,
	providerRepo ...*repository.ModelProviderRepository,
) *AIService {
	svc := &AIService{
		modelRepo: modelRepo,
		taskRepo:  taskRepo,
		aiManager: aiManager,
		stopCh:    make(chan struct{}),
	}
	if len(providerRepo) > 0 {
		svc.providerRepo = providerRepo[0]
	}
	svc.startProviderCacheCleanup()
	return svc
}

// Shutdown stops background goroutines (call on server exit).
func (s *AIService) Shutdown() {
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
}

// startProviderCacheCleanup 启动 providerCache 的后台定期清理（每 10 分钟扫描一次，删除已过期条目）。
func (s *AIService) startProviderCacheCleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				s.providerCache.Range(func(k, v interface{}) bool {
					if entry, ok := v.(providerCacheEntry); ok && now.After(entry.expiresAt) {
						s.providerCache.Delete(k)
					}
					return true
				})
			case <-s.stopCh:
				return
			}
		}
	}()
}

// WithNovelRepo 注入小说仓库，用于在生成时读取小说级 AI 配置
func (s *AIService) WithNovelRepo(repo *repository.NovelRepository) *AIService {
	s.novelRepo = repo
	return s
}

// WithStorage 注入媒体存储服务，供图片生成后持久化使用
func (s *AIService) WithStorage(svc storage.Service) *AIService {
	s.storageSvc = svc
	return s
}

// WithTaskRouting 设置各任务类型优先使用的 provider（来自 config.yaml ai.tasks）
func (s *AIService) WithTaskRouting(tr TaskRouting) *AIService {
	s.taskRouting = tr
	return s
}

// WithImageConcurrency 设置图像生成的最大并发数。
// n ≤ 0 时不限制并发（仅受 AI 提供商限速约束）。
func (s *AIService) WithImageConcurrency(n int) *AIService {
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	}
	return s
}

// SetImageConcurrency 运行时动态调整图像并发度（线程安全）。
func (s *AIService) SetImageConcurrency(n int) {
	s.semMu.Lock()
	defer s.semMu.Unlock()
	if n > 0 {
		s.imageSem = make(chan struct{}, n)
	} else {
		s.imageSem = nil
	}
}

// Generate 生成内容（使用系统级提供商，tenantID=0）
func (s *AIService) Generate(novelID uint, taskType string, prompt string) (string, error) {
	return s.GenerateWithProvider(0, novelID, taskType, prompt, "")
}

// GetDefaultProviderName 返回当前默认 provider 名称
func (s *AIService) GetDefaultProviderName() string {
	if s.aiManager == nil {
		return "unknown"
	}
	p, err := s.aiManager.GetProvider("")
	if err != nil {
		return "unknown"
	}
	return p.GetName()
}

// getTenantProvider 按租户加载提供商（带缓存，TTL 5 分钟）
func (s *AIService) getTenantProvider(tenantID uint, providerName string) (ai.AIProvider, error) {
	if s.providerRepo == nil {
		return s.aiManager.GetProvider(providerName)
	}

	cacheKey := fmt.Sprintf("%d:%s", tenantID, providerName)

	// 检查缓存
	if v, ok := s.providerCache.Load(cacheKey); ok {
		entry, assertOK := v.(providerCacheEntry)
		if !assertOK {
			s.providerCache.Delete(cacheKey)
		} else if time.Now().Before(entry.expiresAt) {
			return entry.provider, nil
		} else {
			s.providerCache.Delete(cacheKey)
		}
	}

	// 从 DB 加载（租户私有 + 系统级）
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return s.aiManager.GetProvider(providerName)
	}

	// 优先租户私有，其次系统级（优先选择有 credentials 的 provider）
	var tenantMatch, systemMatch *model.ModelProvider
	for _, p := range providers {
		// 跳过已禁用的提供商
		if !p.IsActive {
			continue
		}
		// 当未指定具体提供商时，跳过图像/视频/语音/嵌入/多能力类型（这些不做文本生成）
		if providerName == "" {
			t := strings.ToLower(p.Type)
			if t == "image" || t == "video" || t == "voice" || t == "embedding" || t == "sfx" {
				continue
			}
		}
		if providerName == "" || p.Name == providerName {
			if p.TenantID == tenantID && tenantID != 0 {
				if tenantMatch == nil || (!providerHasCredentials(tenantMatch) && providerHasCredentials(p)) {
					tenantMatch = p
				}
				if providerHasCredentials(tenantMatch) {
					break
				}
			}
			if p.TenantID == 0 {
				// 优先选有凭证的系统级 provider，已有有凭证的则不覆盖
				if systemMatch == nil {
					systemMatch = p
				} else if !providerHasCredentials(systemMatch) && providerHasCredentials(p) {
					systemMatch = p
				}
			}
		}
	}
	matched := tenantMatch
	if matched == nil {
		matched = systemMatch
	}

	if matched == nil {
		// DB 中无配置，降级到内存 aiManager
		return s.aiManager.GetProvider(providerName)
	}

	// Validate credentials before constructing the provider.
	if !providerHasCredentials(matched) {
		logger.Printf("getTenantProvider: DB provider %q missing credentials, falling back to in-memory manager", matched.Name)
		return s.aiManager.GetProvider(providerName)
	}

	// Instantiate the provider.
	// 名称优先匹配已知 key；对自定义名称（如"豆包图片"）则根据 endpoint 推断构造器。
	timeout := ai.ResolveTimeout(matched.Timeout)
	var provider ai.AIProvider
	switch matched.Name {
	case ai.ProviderNameVolcengineVisual:
		provider = ai.NewVolcengineVisualProvider(matched.APIKey, matched.APISecretKey)
	case "openai":
		provider = ai.NewOpenAIProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "anthropic":
		provider = ai.NewAnthropicProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "google":
		provider = ai.NewGoogleProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao":
		provider = ai.NewDoubaoProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "doubao-speech":
		// APIKey = X-Api-Key, APIVersion = resourceID（如 "seed-tts-2.0"）
		provider = ai.NewDoubaoSpeechProvider(matched.APIKey, matched.APIVersion)
	case "doubao-speech-v1":
		// APIKey = appID, APISecretKey = access_token, APIVersion = cluster（默认 volcano_tts）
		provider = ai.NewDoubaoSpeechV1Provider(matched.APIKey, matched.APISecretKey, matched.APIVersion)
	case "deepseek":
		provider = ai.NewDeepSeekProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "qianwen":
		provider = ai.NewQianwenProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
	case "azure":
		provider = ai.NewAzureProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, "", timeout)
	default:
		// 自定义名称：按 endpoint 推断底层 API 格式
		ep := strings.ToLower(matched.APIEndpoint)
		switch {
		case strings.Contains(ep, "volces.com") || strings.Contains(ep, "volcengine"):
			// 火山方舟 / 豆包系列（OpenAI 兼容格式）
			logger.Printf("getTenantProvider: provider %q mapped to doubao constructor via endpoint", matched.Name)
			provider = ai.NewDoubaoProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case strings.Contains(ep, "azure.com") || strings.Contains(ep, "openai.azure"):
			logger.Printf("getTenantProvider: provider %q mapped to azure constructor via endpoint", matched.Name)
			provider = ai.NewAzureProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, "", timeout)
		case strings.Contains(ep, "anthropic.com"):
			logger.Printf("getTenantProvider: provider %q mapped to anthropic constructor via endpoint", matched.Name)
			provider = ai.NewAnthropicProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		case matched.APIEndpoint != "":
			// 有自定义 endpoint → 按 OpenAI 兼容格式通用处理
			logger.Printf("getTenantProvider: provider %q using OpenAI-compatible constructor for endpoint %s", matched.Name, matched.APIEndpoint)
			provider = ai.NewOpenAIProvider(matched.APIKey, matched.APIEndpoint, matched.APIVersion, timeout)
		default:
			logger.Printf("getTenantProvider: unrecognized provider %q with no endpoint — falling back to static aiManager", matched.Name)
			return s.aiManager.GetProvider(providerName)
		}
	}

	// 包装重试
	provider = ai.NewRetryProvider(provider, 3, 500*time.Millisecond)

	// 写入缓存
	s.providerCache.Store(cacheKey, providerCacheEntry{
		provider:  provider,
		expiresAt: time.Now().Add(5 * time.Minute),
	})

	return provider, nil
}

// CheckAvailability 检查指定租户是否有可用的 LLM 提供商（用于 pipeline 预检）
func (s *AIService) CheckAvailability(tenantID uint) error {
	_, err := s.getTenantProvider(tenantID, "")
	return err
}

// InvalidateProviderCache 删除指定提供商在所有租户下的缓存，供 DeleteProvider/UpdateProvider 调用。
func (s *AIService) InvalidateProviderCache(providerName string) {
	s.providerCache.Range(func(k, _ any) bool {
		key, ok := k.(string)
		// key format: "tenantID:providerName"
		if ok && strings.HasSuffix(key, ":"+providerName) {
			s.providerCache.Delete(k)
		}
		return true
	})
}

// GenerateWithProvider 使用指定 Provider 生成内容（providerName 为空则使用默认）
// 参数优先级：小说项目配置 > 任务配置 > 内置默认值
func (s *AIService) GenerateWithProvider(tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	// 获取任务配置（基础层）
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{
			Temperature: 0.7,
			MaxTokens:   0, // 0 = 不限制，由 AI provider 使用其模型默认值
		}
	}
	// 复制一份避免污染缓存
	config := *base

	// 应用小说项目级 AI 配置（覆盖任务默认值）
	var resolvedModel string
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			// 若小说配置了特定 AI 模型且调用方未指定 provider，
			// 通过模型名反查对应 provider 并将模型名透传给 API
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	// JSON 结构化输出任务降温：高温度会产生格式漂移（多余 markdown / 说明文字），触发 retry 更慢。
	// MaxTokens 不设强制下限，由任务配置 / 小说配置 / 调用方覆盖自行决定。
	switch taskType {
	case "storyboard", "character", "worldview", "character_state", "scene_anchor_extract", "storyboard_review", "sfx_analyze":
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	}

	// 应用调用方传入的覆盖参数（优先级最高，覆盖任务配置和小说配置）
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

	// 章节写作类任务注入 system prompt，阻止模型输出大纲/前言/元注释
	sysPmt := ""
	if chapterTaskTypes[taskType] {
		sysPmt = novelWritingSystemPrompt
	}

	// 调用真实AI API
	result, err := s.callAIWithProviderSys(context.Background(), tenantID, prompt, sysPmt, &config, providerName, resolvedModel)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}

	// 记录使用
	s.logUsage(&config, prompt, result)

	return result, nil
}

// GenerateWithProviderCtx is like GenerateWithProvider but respects an external context.
// Use this when the caller holds a cancellable context (e.g. async task with cancel support).
func (s *AIService) GenerateWithProviderCtx(ctx context.Context, tenantID uint, novelID uint, taskType string, prompt string, providerName string, overrides ...StoryboardOverrides) (string, error) {
	base, err := s.taskRepo.GetByTaskType(taskType)
	if err != nil {
		base = &model.TaskModelConfig{Temperature: 0.7, MaxTokens: 0}
	}
	config := *base

	var resolvedModel string
	if novelID > 0 && s.novelRepo != nil {
		if novel, err := s.novelRepo.GetByID(novelID); err == nil {
			if novel.Temperature > 0 {
				config.Temperature = novel.Temperature
			}
			if novel.TopP > 0 {
				config.TopP = novel.TopP
			}
			if novel.TopK > 0 {
				config.TopK = novel.TopK
			}
			if novel.MaxTokens > 0 {
				config.MaxTokens = novel.MaxTokens
			}
			if providerName == "" && novel.AIModel != "" {
				resolvedModel = novel.AIModel
				if pName := s.resolveProviderFromModel(tenantID, novel.AIModel); pName != "" {
					providerName = pName
				}
			}
		}
	}

	switch taskType {
	case "storyboard", "character", "worldview", "character_state", "scene_anchor_extract", "storyboard_review", "sfx_analyze":
		if config.Temperature > 0.2 {
			config.Temperature = 0.1
		}
	}

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

	// 大纲生成需要较多输出 token（100 章大纲 ≈ 20K+ 字符），
	// 若用户或项目配置的 maxTokens 低于 16384，自动提升到 16384，避免 JSON 截断。
	if taskType == "outline" && config.MaxTokens > 0 && config.MaxTokens < 16384 {
		config.MaxTokens = 16384
	}

	sysPmt := ""
	if chapterTaskTypes[taskType] {
		sysPmt = novelWritingSystemPrompt
	}

	result, err := s.callAIWithProviderSys(ctx, tenantID, prompt, sysPmt, &config, providerName, resolvedModel)
	if err != nil {
		return "", fmt.Errorf("AI generation failed: %w", err)
	}
	s.logUsage(&config, prompt, result)
	return result, nil
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

// callAI 调用AI接口（使用系统级 provider）
func (s *AIService) callAI(prompt string, config *model.TaskModelConfig) (string, error) {
	return s.callAIWithProvider(context.Background(), 0, prompt, config, "")
}

// GenerateWithVision 使用 Vision AI 分析图像内容
// 优先使用 anthropic（claude-3），其次 openai（gpt-4o），都失败则用默认 provider
func (s *AIService) GenerateWithVision(prompt string, imageURLs []string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider
	var err error
	for _, name := range []string{"anthropic", "openai"} {
		provider, err = s.aiManager.GetProvider(name)
		if err == nil {
			break
		}
	}
	if err != nil {
		provider, err = s.aiManager.GetProvider("")
		if err != nil {
			return "", fmt.Errorf("failed to get AI provider for vision: %w", err)
		}
	}

	req := &ai.GenerateRequest{
		Messages: []ai.ChatMessage{
			{
				Role:      "user",
				Content:   prompt,
				ImageURLs: imageURLs,
			},
		},
		MaxTokens:   512,
		Temperature: 0.1,
	}

	visionCtx, visionCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer visionCancel()
	resp, err := provider.Generate(visionCtx, req)
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
// novelWritingSystemPrompt is injected as the system role for all chapter/scene writing tasks.
// It prevents the model from adding preambles, outlines, or meta-commentary.
const novelWritingSystemPrompt = `你是一个小说正文生成引擎，只负责按指令输出小说正文内容。

严格规则（任何情况下不得违反）：
- 输出内容只能是小说正文，从章节标题行开始，到正文自然结束为止
- 禁止任何开场白，如"好的""当然可以""非常抱歉""由于篇幅限制"等
- 禁止在正文外输出大纲、章节摘要、写作建议或元注释
- 禁止声明字数/篇幅限制，禁止请求用户确认续写
- 禁止在正文结束后追加任何说明文字
- 字数不足时直接写到章末钩子，不得截断并附注"待续"类说明`

// chapterTaskTypes is the set of task types that generate novel prose.
var chapterTaskTypes = map[string]bool{
	"chapter": true, "chapter_outline": true, "scene_outline": true,
}

func (s *AIService) callAIWithProvider(parentCtx context.Context, tenantID uint, prompt string, config *model.TaskModelConfig, providerName string, modelOverride ...string) (string, error) {
	return s.callAIWithProviderSys(parentCtx, tenantID, prompt, "", config, providerName, modelOverride...)
}

func (s *AIService) callAIWithProviderSys(parentCtx context.Context, tenantID uint, prompt string, systemPrompt string, config *model.TaskModelConfig, providerName string, modelOverride ...string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	provider, err := s.getTenantProvider(tenantID, providerName)
	if err != nil {
		logger.Printf("callAIWithProvider: getTenantProvider failed (tenant=%d, provider=%q): %v", tenantID, providerName, err)
		return "", fmt.Errorf("failed to get AI provider: %w", err)
	}

	req := &ai.GenerateRequest{
		Messages:     []ai.ChatMessage{{Role: "user", Content: prompt}},
		SystemPrompt: systemPrompt,
		MaxTokens:    config.MaxTokens,
		Temperature:  config.Temperature,
		TopP:         config.TopP,
	}
	if len(modelOverride) > 0 && modelOverride[0] != "" {
		req.Model = modelOverride[0]
	}
	// Claude 不支持 top_k，仅在非 Anthropic provider 时传入
	if provider.GetName() != "anthropic" {
		req.TopK = config.TopK
	}
	// MaxTokens == 0 时不设限，由各 provider 实现决定是否传给 API（doubao/qianwen 已做 >0 判断）

	// 超时：优先使用 config.TimeoutSeconds（由调用方注入），否则默认 5 分钟。
	timeoutDur := 5 * time.Minute
	if config.TimeoutSeconds > 0 {
		timeoutDur = time.Duration(config.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeoutDur)
	defer cancel()

	logger.Printf("[AI] provider=%s maxTokens=%d temperature=%.2f calling...", provider.GetName(), req.MaxTokens, req.Temperature)
	callStart := time.Now()
	resp, err := provider.Generate(ctx, req)
	elapsed := time.Since(callStart).Round(time.Millisecond)
	if err != nil {
		logger.Printf("[AI] provider=%s elapsed=%s err=%v", provider.GetName(), elapsed, err)
		return "", err
	}
	if resp.Error != "" {
		logger.Printf("[AI] provider=%s elapsed=%s providerErr=%s", provider.GetName(), elapsed, resp.Error)
		return "", fmt.Errorf("provider error: %s", resp.Error)
	}
	logger.Printf("[AI] provider=%s elapsed=%s maxTokens=%d respLen=%d stopReason=%q", provider.GetName(), elapsed, req.MaxTokens, len(resp.Content), resp.StopReason)

	return resp.Content, nil
}

// generateJSONForTenant 带 tenantID 的 JSON 生成重试（最多重试 maxRetries 次）
func (s *AIService) generateJSONForTenant(tenantID, novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("generateJSONForTenant: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.GenerateWithProvider(tenantID, novelID, taskType, p, "")
		if err != nil {
			lastErr = err
			continue
		}
		cleaned := extractJSON(result)
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		logger.Printf("generateJSONForTenant: %v", lastErr)
	}
	return "", fmt.Errorf("generateJSONForTenant failed after %d attempts: %w", maxRetries+1, lastErr)
}

// generateWithRetry 带容错重试的 JSON 生成（最多重试 2 次）
func (s *AIService) generateWithRetry(novelID uint, taskType, prompt string, maxRetries int) (string, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		p := prompt
		if attempt > 0 {
			p = prompt + "\n\n⚠️ 重要提示：请只返回纯 JSON，不要包含任何 markdown 代码块（```）或说明文字。"
			logger.Printf("generateWithRetry: attempt %d for taskType=%s, novelID=%d", attempt+1, taskType, novelID)
		}
		result, err := s.Generate(novelID, taskType, p)
		if err != nil {
			lastErr = err
			continue
		}
		// 尝试提取 JSON
		cleaned := extractJSON(result)
		// 验证是否为有效 JSON
		var v interface{}
		if jsonErr := json.Unmarshal([]byte(cleaned), &v); jsonErr == nil {
			return cleaned, nil
		}
		lastErr = fmt.Errorf("invalid JSON on attempt %d: %s", attempt+1, cleaned[:min(100, len(cleaned))])
		logger.Printf("generateWithRetry: %v", lastErr)
	}
	return "", fmt.Errorf("generateWithRetry failed after %d attempts: %w", maxRetries+1, lastErr)
}

// logUsage 记录使用
func (s *AIService) logUsage(config *model.TaskModelConfig, prompt, result string) {
	if s.modelRepo == nil {
		return
	}
	inputTokens := countChineseChars(prompt)
	outputTokens := countChineseChars(result)

	log := &model.ModelUsageLog{
		ModelID:      config.PrimaryModelID,
		TaskType:     "generation",
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         float64(inputTokens+outputTokens) / 1000 * 0.01,
		Latency:      1.5,
		Success:      true,
	}

	s.modelRepo.LogUsage(log)
}

// GenerateImage 调用AI生成图像
func (s *AIService) GenerateImage(prompt string, options *ImageGenerationOptions) (*GeneratedImage, error) {
	if s.aiManager == nil {
		return nil, fmt.Errorf("AI manager not initialized")
	}
	provider, err := s.aiManager.GetProvider("")
	if err != nil {
		return nil, fmt.Errorf("get AI provider failed: %w", err)
	}
	req := &ai.ImageGenerateRequest{
		Prompt:         prompt,
		NegativePrompt: options.NegativePrompt,
		Size:           options.Size,
		Steps:          options.Steps,
		CFGScale:       options.CFGScale,
	}
	resp, err := provider.ImageGenerate(context.Background(), req)
	if err != nil {
		return nil, err
	}
	persistURL := s.uploadImageToStorage(context.Background(), 0, resp.URL)
	return &GeneratedImage{
		URL:    persistURL,
		Width:  resp.Width,
		Height: resp.Height,
	}, nil
}

// knownImageCapableProviders 已知支持图像生成的提供者及其默认模型，用于 DB 动态加载的回退路径。
var knownImageCapableProviders = []ai.ImageProviderEntry{
	{ProviderName: "doubao", Model: "seedream-3-0-t2i-250415", Size: "1024x1024"},
	{ProviderName: "qianwen", Model: "wanx2.1-t2i-turbo", Size: "1024x1024"},
	{ProviderName: "openai", Model: "dall-e-3", Size: "1024x1024"},
	{ProviderName: "volcengine-visual", Model: ai.VolcModelText2ImgV3, Size: "1328x1328"},
}

// loadDBImageProviderEntries 从 DB 加载 IMAGE 类型的提供者列表，使用实际配置的模型名称（APIVersion）。
// 避免 knownImageCapableProviders 硬编码模型与用户实际配置不匹配的问题。
// volcengine-visual 排在末尾：它需要服务端下载参考图，私有 OSS URL 会导致 403 失败。
func (s *AIService) loadDBImageProviderEntries(tenantID uint) []ai.ImageProviderEntry {
	if s.providerRepo == nil {
		return nil
	}
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil
	}
	defaultSizeMap := map[string]string{
		"doubao":                        "1024x1024",
		"qianwen":                       "1024x1024",
		"openai":                        "1024x1024",
		ai.ProviderNameVolcengineVisual: "1328x1328",
	}
	var primary, volcengine []ai.ImageProviderEntry
	seen := map[string]bool{}
	for _, p := range providers {
		if !p.IsActive {
			logger.Printf("loadDBImageProviderEntries: skip provider %q (inactive)", p.Name)
			continue
		}
		pt := strings.ToLower(p.Type)
		if pt != "image" {
			continue // non-image providers are expected, no need to log
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBImageProviderEntries: skip IMAGE provider %q (missing credentials)", p.Name)
			continue
		}
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		size := defaultSizeMap[p.Name]
		if size == "" {
			size = "1024x1024"
		}
		entry := ai.ImageProviderEntry{ProviderName: p.Name, Model: p.APIVersion, Size: size}
		logger.Printf("loadDBImageProviderEntries: adding IMAGE provider %q model=%q size=%s (tenantID=%d)", p.Name, p.APIVersion, size, tenantID)
		// volcengine-visual 依赖服务端下载参考图，排到最后作为兜底
		if p.Name == ai.ProviderNameVolcengineVisual {
			volcengine = append(volcengine, entry)
		} else {
			primary = append(primary, entry)
		}
	}
	result := append(primary, volcengine...)
	if len(result) == 0 {
		logger.Printf("loadDBImageProviderEntries: no IMAGE providers found for tenantID=%d (total providers checked: %d)", tenantID, len(providers))
	}
	return result
}

// selectImageModel returns the model to use for the given entry.
// For volcengine-visual: referenceImage → DreamO; style=="realistic" → PortraitPhoto.
// selectImageModel 根据提供者、参考图、风格和一致性权重选择合适的图像生成模型。
// consistencyWeight: 0-1，≥0.7 使用 DreamO（角色特征保持），<0.7 使用 SeedEditV3（指令编辑）
func selectImageModel(entry ai.ImageProviderEntry, referenceImage, style string, consistencyWeight ...float64) string {
	if entry.ProviderName == ai.ProviderNameVolcengineVisual {
		// volcengine-visual 始终用内置 req_key，不依赖用户填写的 APIVersion
		if referenceImage != "" {
			// 写实风格：即使有参考图也使用 PortraitPhoto，保证生成真实感肖像
			if style == "realistic" {
				return ai.VolcModelPortraitPhoto
			}
			weight := 1.0
			if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
				weight = consistencyWeight[0]
			}
			if weight >= 0.7 {
				return ai.VolcModelDreamO
			}
			return ai.VolcModelSeedEditV3
		}
		// 无参考图时：PortraitPhoto 是 I2I 模型，必须有 image_input 才能正常工作；
		// 无论什么风格都使用 Text2ImgV3（文生图），这样 prompt/negative_prompt 才能完整生效。
		return ai.VolcModelText2ImgV3
	}
	return entry.Model
}

// GenerateCharacterThreeView 使用图像生成 API 生成角色/场景视图图像。
// style: 图片风格（"realistic"/"anime"/"ink_painting" 等），影响 Volcengine 模型选择。
// 空字符串表示使用提供者默认模型。
// consistencyWeight（可选）: 0-1，角色一致性强度；默认 1.0（严格）。
//
//	≥0.7 → DreamO（角色特征保持），<0.7 → SeedEditV3（指令编辑，scale 线性映射 1-10）
func (s *AIService) GenerateCharacterThreeView(ctx context.Context, tenantID uint, providerName, prompt, referenceImage, style, negativePrompt string, consistencyWeight ...float64) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	// 并发限流：若配置了 image_concurrency，则在此处等待令牌
	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	weight := 1.0
	if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
		weight = consistencyWeight[0]
	}
	// SeedEditV3 的 scale 参数范围 1-10；以 weight 线性映射
	cfgScale := 1.0 + weight*9.0

	// 指定提供者时：直接加载并调用，不走遍历逻辑
	if providerName != "" {
		// 找到对应的 entry（model/size）
		var entry *ai.ImageProviderEntry
		for _, e := range knownImageCapableProviders {
			if e.ProviderName == providerName {
				entry = &e
				break
			}
		}
		// 也在静态注册列表里找
		if entry == nil {
			for _, e := range s.aiManager.GetImageProviders() {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			return "", fmt.Errorf("unknown image provider: %s", providerName)
		}
		provider, err := s.aiManager.GetProvider(providerName)
		if err != nil {
			// 静态 manager 无此 provider，尝试 DB
			provider, err = s.getTenantProvider(tenantID, providerName)
			if err != nil {
				return "", fmt.Errorf("image provider %q not available: %w", providerName, err)
			}
		}
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             selectImageModel(*entry, referenceImage, style, weight),
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              entry.Size,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		})
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", fmt.Errorf("image generation failed: %s", resp.Error)
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}

	// DB 模式（providerRepo 存在）时：DB 是唯一权威来源，完全忽略静态 aiManager 的图像提供者。
	// 这样删除/更改 DB 中的提供者可以立即生效，不受 env/config.yaml 中通用 AI key 的干扰。
	// 纯静态模式（无 DB）：读 aiManager 静态列表，为空时回退硬编码表。
	var entries []ai.ImageProviderEntry
	useDB := s.providerRepo != nil
	if useDB {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}

	if len(entries) == 0 {
		return "", fmt.Errorf("no image providers configured")
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if useDB {
			// 从 DB 动态加载提供者（带租户感知和缓存）
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		model := selectImageModel(e, referenceImage, style, weight)
		logger.Printf("GenerateCharacterThreeView: trying provider=%s model=%s refImage=%v", e.ProviderName, model, referenceImage != "")
		resp, err := provider.ImageGenerate(ctx, &ai.ImageGenerateRequest{
			Model:             model,
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              e.Size,
			ReferenceImage:    referenceImage,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		})
		if err != nil {
			logger.Printf("GenerateCharacterThreeView: provider=%s failed: %v", e.ProviderName, err)
			lastErr = err
			continue
		}
		if resp.Error != "" {
			logger.Printf("GenerateCharacterThreeView: provider=%s error: %s", e.ProviderName, resp.Error)
			lastErr = fmt.Errorf("image generation failed: %s", resp.Error)
			continue
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}
	return "", fmt.Errorf("no image provider available: %w", lastErr)
}

// GenerateCharacterThreeViewMulti 与 GenerateCharacterThreeView 相同，但支持传入多张参考图和自定义尺寸。
// referenceImages：多张参考图 URL，直接传给支持多图的 API（如 DreamO image_urls[]），无需调用方拼接合图。
// size：图片尺寸（"WxH" 格式，如 "1024x576"），覆盖提供者默认尺寸；为空时使用提供者默认值。
// 若 referenceImages 为空，退化为纯文本生成。
func (s *AIService) GenerateCharacterThreeViewMulti(ctx context.Context, tenantID uint, providerName, prompt string, referenceImages []string, style, negativePrompt, size string, consistencyWeight ...float64) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	s.semMu.RLock()
	sem := s.imageSem
	s.semMu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	weight := 1.0
	if len(consistencyWeight) > 0 && consistencyWeight[0] > 0 {
		weight = consistencyWeight[0]
	}
	cfgScale := 1.0 + weight*9.0

	// 取第一张图作为 selectImageModel 的存在性判断
	firstRef := ""
	if len(referenceImages) > 0 {
		firstRef = referenceImages[0]
	}

	buildReq := func(model, entrySize string) *ai.ImageGenerateRequest {
		sz := size // 优先使用调用方传入的尺寸（基于 AspectRatio+QualityTier 计算）
		if sz == "" {
			sz = entrySize
		}
		return &ai.ImageGenerateRequest{
			Model:             model,
			Prompt:            prompt,
			NegativePrompt:    negativePrompt,
			Size:              sz,
			ReferenceImage:    firstRef,
			ReferenceImages:   referenceImages,
			CFGScale:          cfgScale,
			ConsistencyWeight: weight,
		}
	}

	if providerName != "" {
		var entry *ai.ImageProviderEntry
		for _, e := range knownImageCapableProviders {
			if e.ProviderName == providerName {
				entry = &e
				break
			}
		}
		if entry == nil {
			for _, e := range s.aiManager.GetImageProviders() {
				if e.ProviderName == providerName {
					entry = &e
					break
				}
			}
		}
		if entry == nil {
			return "", fmt.Errorf("unknown image provider: %s", providerName)
		}
		provider, err := s.aiManager.GetProvider(providerName)
		if err != nil {
			provider, err = s.getTenantProvider(tenantID, providerName)
			if err != nil {
				return "", fmt.Errorf("image provider %q not available: %w", providerName, err)
			}
		}
		resp, err := provider.ImageGenerate(ctx, buildReq(selectImageModel(*entry, firstRef, style, weight), entry.Size))
		if err != nil {
			return "", err
		}
		if resp.Error != "" {
			return "", fmt.Errorf("image generation failed: %s", resp.Error)
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}

	var entries []ai.ImageProviderEntry
	useDB := s.providerRepo != nil
	if useDB {
		entries = s.loadDBImageProviderEntries(tenantID)
	} else {
		entries = s.aiManager.GetImageProviders()
		if len(entries) == 0 {
			entries = knownImageCapableProviders
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no image providers configured")
	}

	var lastErr error
	for _, e := range entries {
		var provider ai.AIProvider
		var err error
		if useDB {
			provider, err = s.getTenantProvider(tenantID, e.ProviderName)
		} else {
			provider, err = s.aiManager.GetProvider(e.ProviderName)
		}
		if err != nil {
			lastErr = err
			continue
		}
		model := selectImageModel(e, firstRef, style, weight)
		logger.Printf("GenerateCharacterThreeViewMulti: trying provider=%s model=%s refs=%d", e.ProviderName, model, len(referenceImages))
		resp, err := provider.ImageGenerate(ctx, buildReq(model, e.Size))
		if err != nil {
			logger.Printf("GenerateCharacterThreeViewMulti: provider=%s failed: %v", e.ProviderName, err)
			lastErr = err
			continue
		}
		if resp.Error != "" {
			logger.Printf("GenerateCharacterThreeViewMulti: provider=%s error: %s", e.ProviderName, resp.Error)
			lastErr = fmt.Errorf("image generation failed: %s", resp.Error)
			continue
		}
		return s.uploadImageToStorage(ctx, tenantID, resp.URL), nil
	}
	return "", fmt.Errorf("no image provider available: %w", lastErr)
}

// imageStorageHintKey is the context key for ImageStorageHint.
type imageStorageHintKey struct{}

// ImageStorageHint carries novel/chapter metadata for OSS key building.
type ImageStorageHint struct {
	NovelTitle string
	ChapterNo  int // 0 = novel-level, non-zero = chapter-level
}

// WithImageStorageHint enriches a context with novel/chapter metadata for OSS key building.
func WithImageStorageHint(ctx context.Context, hint ImageStorageHint) context.Context {
	return context.WithValue(ctx, imageStorageHintKey{}, hint)
}

// uploadImageToStorage 下载 AI 模型返回的临时图片 URL 并上传到持久存储（OSS/本地/DB）。
// storageSvc 为 nil 或上传失败时降级返回原 imgURL（非致命）。
// OSS key 格式：
//   - 有小说+章节信息：novels/{title}/chapters/{no}/images/{uuid}.ext
//   - 有小说信息：     novels/{title}/images/{uuid}.ext
//   - 无信息（降级）：  images/{tenantID}/{uuid}.ext
func (s *AIService) uploadImageToStorage(ctx context.Context, tenantID uint, imgURL string) string {
	if s.storageSvc == nil || imgURL == "" {
		return imgURL
	}
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, imgURL, nil)
	if err != nil {
		logger.Printf("uploadImageToStorage: build request: %v", err)
		return imgURL
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Printf("uploadImageToStorage: download %s: %v", imgURL, err)
		return imgURL
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Printf("uploadImageToStorage: read body: %v", err)
		return imgURL
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = "image/jpeg"
	}
	ext := imageExtFromContentType(ct)
	filename := uuid.New().String() + ext
	logger.Printf("uploadImageToStorage: generated filename=%q from imgURL=%q", filename, imgURL)

	var key string
	if hint, ok := ctx.Value(imageStorageHintKey{}).(ImageStorageHint); ok && hint.NovelTitle != "" {
		sanitized := sanitizeStorageName(hint.NovelTitle)
		if sanitized != "" {
			if hint.ChapterNo > 0 {
				key = fmt.Sprintf("novels/%s/chapters/%d/images/%s", sanitized, hint.ChapterNo, filename)
			} else {
				key = fmt.Sprintf("novels/%s/images/%s", sanitized, filename)
			}
		}
	}
	if key == "" {
		key = fmt.Sprintf("images/%d/%s", tenantID, filename)
	}

	persistURL, uploadErr := s.storageSvc.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), ct)
	if uploadErr != nil {
		logger.Printf("uploadImageToStorage: upload failed (falling back to original URL): %v", uploadErr)
		return imgURL
	}
	logger.Printf("uploadImageToStorage: persisted %s → %s", imgURL, persistURL)
	return persistURL
}

// imageExtFromContentType 根据 Content-Type 返回图片文件扩展名。
func imageExtFromContentType(ct string) string {
	switch {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return ".jpg"
	case strings.Contains(ct, "png"):
		return ".png"
	case strings.Contains(ct, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

// AudioGenerate 调用默认 AI provider 生成 TTS 音频，返回本地文件路径（file:// URL）
func (s *AIService) AudioGenerate(ctx context.Context, text, voice string) (string, error) {
	return s.AudioGenerateWithOptions(ctx, 0, text, voice, 1.0, "")
}

// AudioGenerateWithOptions 支持语速、风格和语言/方言的 TTS 生成。
// Provider 选取顺序（DB 模式优先，与图像生成保持一致）：
//  1. DB 中租户配置的 voice/tts 类型 provider
//  2. config.yaml ai.tasks.tts 指定的 provider（静态模式兜底）
//  3. env-var 注册的默认 provider（静态模式兜底）
func (s *AIService) AudioGenerateWithOptions(ctx context.Context, tenantID uint, text, voice string, speed float64, style string, language ...string) (string, error) {
	if s.aiManager == nil {
		return "", fmt.Errorf("AI manager not initialized")
	}

	var provider ai.AIProvider

	// 1. DB 模式：扫描 voice/tts 类型的 provider（与图像生成逻辑对称）
	if s.providerRepo != nil {
		if p, err := s.loadDBVoiceProvider(tenantID); err == nil && p != nil {
			provider = p
		}
	}

	// 2. 静态模式：config.yaml task routing
	if provider == nil && s.taskRouting.TTS != "" {
		if p, err := s.aiManager.GetProvider(s.taskRouting.TTS); err == nil {
			provider = p
		}
	}
	// 注意：不再兜底到默认 LLM provider，LLM 提供商通常不支持 /audio/speech 接口，
	// 兜底只会产生 404 错误，不如直接给用户明确的配置提示。

	if provider == nil {
		return "", fmt.Errorf("未配置语音合成提供商，请在「模型管理」中添加一个类型为 voice 或 tts 的 AI 提供商（如豆包语音、OpenAI TTS 等）并填写 API Key")
	}

	if voice == "" {
		voice = "alloy"
	}
	if speed <= 0 {
		speed = 1.0
	}
	lang := ""
	if len(language) > 0 {
		lang = language[0]
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     text,
		Voice:    voice,
		Speed:    speed,
		Emotion:  style,
		Language: lang,
	})
	if err != nil {
		return "", err
	}
	return resp.URL, nil
}

// GenerateSFX 使用 DB 中配置的 sfx 类型提供商生成音效，返回 CDN URL 和时长（秒）。
// prompt: 音效描述，如 "春节烟花声"；duration: 期望时长（秒，3.0~10.0）。
func (s *AIService) GenerateSFX(ctx context.Context, tenantID uint, prompt string, duration float64) (string, float64, error) {
	provider, err := s.loadDBSFXProvider(tenantID)
	if err != nil {
		return "", 0, err
	}
	resp, err := provider.AudioGenerate(ctx, &ai.AudioGenerateRequest{
		Text:     prompt,
		Duration: duration,
	})
	if err != nil {
		return "", 0, err
	}
	return resp.URL, resp.Duration, nil
}

// loadDBSFXProvider 从 DB 中取第一个有效的 sfx 类型提供商（文生音效）。
func (s *AIService) loadDBSFXProvider(tenantID uint) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		if pt := strings.ToLower(p.Type); pt != "sfx" {
			continue
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBSFXProvider: skip sfx provider %q (missing credentials)", p.Name)
			continue
		}
		provider, err := s.getTenantProvider(tenantID, p.Name)
		if err != nil {
			logger.Printf("loadDBSFXProvider: failed to instantiate provider %q: %v", p.Name, err)
			continue
		}
		logger.Printf("loadDBSFXProvider: using sfx provider %q", p.Name)
		return provider, nil
	}
	return nil, fmt.Errorf("no sfx providers configured in DB")
}

// loadDBVoiceProvider 从 DB 中取第一个有效的 voice/tts 类型提供商。
func (s *AIService) loadDBVoiceProvider(tenantID uint) (ai.AIProvider, error) {
	providers, err := s.providerRepo.ListByTenant(tenantID)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if !p.IsActive {
			continue
		}
		t := strings.ToLower(p.Type)
		if t != "voice" && t != "tts" {
			continue
		}
		if !providerHasCredentials(p) {
			logger.Printf("loadDBVoiceProvider: skip voice provider %q (missing credentials)", p.Name)
			continue
		}
		provider, err := s.getTenantProvider(tenantID, p.Name)
		if err != nil {
			logger.Printf("loadDBVoiceProvider: failed to instantiate provider %q: %v", p.Name, err)
			continue
		}
		logger.Printf("loadDBVoiceProvider: using voice provider %q model=%q", p.Name, p.APIVersion)
		return provider, nil
	}
	return nil, fmt.Errorf("no voice providers configured in DB")
}

// QualityService 质量服务
