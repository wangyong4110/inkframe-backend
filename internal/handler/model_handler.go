package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// staticProviderModels 对不提供 OpenAI 兼容 /models 端点的提供商，
// 按端点前缀返回内置静态模型列表（语音、视频等专用 API）。
var staticProviderModels = map[string][]string{
	"openspeech.bytedance.com": {"seed-tts-2.0", "seed-tts-1.0"},
	"api.klingai.com":          {"kling-v1-6", "kling-v1-5", "kling-v1"},
}

// ModelHandler 模型管理处理器
type ModelHandler struct {
	modelService *service.ModelService
	auditSvc     *service.AuditService
}

func NewModelHandler(modelService *service.ModelService) *ModelHandler {
	return &ModelHandler{modelService: modelService}
}

// WithAuditService injects the audit service.
func (h *ModelHandler) WithAuditService(svc *service.AuditService) *ModelHandler {
	h.auditSvc = svc
	return h
}

// ListProviders 获取提供商列表
// GET /api/v1/model-providers
func (h *ModelHandler) ListProviders(c *gin.Context) {
	providers, err := h.modelService.ListProviders(getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Mask API keys and annotate has_key before returning
	type providerView struct {
		*model.ModelProvider
		HasKey bool `json:"has_key"`
	}
	if list, ok := providers.([]*model.ModelProvider); ok {
		views := make([]providerView, len(list))
		for i, p := range list {
			hasKey := providerHasCredentialsRaw(p)
			p.APIKey = maskAPIKey(p.APIKey)
			p.APISecretKey = maskAPIKey(p.APISecretKey)
			views[i] = providerView{p, hasKey}
		}
		respondOK(c, views)
		return
	}

	respondOK(c, providers)
}

// ListCapableProviders returns providers matching ?type= (e.g. LLM, IMAGE).
// GET /api/v1/model-providers/capable?type=LLM
func (h *ModelHandler) ListCapableProviders(c *gin.Context) {
	providerType := c.Query("type")
	providers, err := h.modelService.ListCapableProviders(getTenantID(c), providerType)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, providers)
}

// GetProvider 获取单个提供商
// GET /api/v1/model-providers/:id
func (h *ModelHandler) GetProvider(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	provider, err := h.modelService.GetProvider(uint(id), getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusNotFound, "provider not found")
		return
	}

	provider.APIKey = maskAPIKey(provider.APIKey)
	provider.APISecretKey = maskAPIKey(provider.APISecretKey)
	respondOK(c, provider)
}

// CreateProvider 创建提供商
// POST /api/v1/model-providers
func (h *ModelHandler) CreateProvider(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	var req model.CreateModelProviderRequest
	if !bindJSON(c, &req) {
		return
	}

	tenantID := getTenantID(c)
	provider, err := h.modelService.CreateProvider(&req, tenantID)
	if err != nil {
		if isDuplicateKeyError(err) {
			// 查找已有 provider 的 ID，返回给前端便于直接跳转编辑页
			existingID := uint(0)
			if existing, e := h.modelService.FindProviderByName(req.Name, tenantID); e == nil {
				existingID = existing.ID
			}
			c.JSON(http.StatusConflict, gin.H{
				"code":        409,
				"message":     "该名称的提供商已存在，请直接编辑已有提供商",
				"existing_id": existingID,
			})
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     tenantID,
			UserID:       getUserID(c),
			Action:       "provider.create",
			ResourceType: "provider",
			ResourceID:   provider.ID,
			ResourceName: provider.Name,
			IP:           c.ClientIP(),
		})
	}

	provider.APIKey = maskAPIKey(provider.APIKey)
	respondCreated(c, provider)
}

// UpdateProvider 更新提供商
// PUT /api/v1/model-providers/:id
func (h *ModelHandler) UpdateProvider(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	var req model.UpdateModelProviderRequest
	if !bindJSON(c, &req) {
		return
	}

	provider, err := h.modelService.UpdateProvider(uint(id), getTenantID(c), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     getTenantID(c),
			UserID:       getUserID(c),
			Action:       "provider.update",
			ResourceType: "provider",
			ResourceID:   uint(id),
			ResourceName: provider.Name,
			IP:           c.ClientIP(),
		})
	}

	provider.APIKey = maskAPIKey(provider.APIKey)
	provider.APISecretKey = maskAPIKey(provider.APISecretKey)
	respondOK(c, provider)
}

// DeleteProvider 删除提供商
// DELETE /api/v1/model-providers/:id
func (h *ModelHandler) DeleteProvider(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if err := h.modelService.DeleteProvider(uint(id), getTenantID(c)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     getTenantID(c),
			UserID:       getUserID(c),
			Action:       "provider.delete",
			ResourceType: "provider",
			ResourceID:   uint(id),
			IP:           c.ClientIP(),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// TestProvider 测试提供商连接
// POST /api/v1/model-providers/:id/test
func (h *ModelHandler) TestProvider(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	result, err := h.modelService.TestProvider(uint(id), getTenantID(c))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code":    0,
			"message": "success",
			"data": gin.H{
				"success": false,
				"error":   err.Error(),
			},
		})
		return
	}

	respondOK(c, result)
}

// ListModels 获取模型列表
// GET /api/v1/models
func (h *ModelHandler) ListModels(c *gin.Context) {
	providerIdStr := c.Query("provider_id")
	var providerId *uint
	if providerIdStr != "" {
		if id, err := strconv.ParseUint(providerIdStr, 10, 32); err == nil {
			providerId = new(uint)
			*providerId = uint(id)
		}
	}

	models, err := h.modelService.ListModels(providerId, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, models)
}

// CreateModel 创建模型
// POST /api/v1/models
func (h *ModelHandler) CreateModel(c *gin.Context) {
	var req model.CreateAIModelRequest
	if !bindJSON(c, &req) {
		return
	}

	modelEntity, err := h.modelService.CreateModel(&req, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     getTenantID(c),
			UserID:       getUserID(c),
			Action:       "model.create",
			ResourceType: "model",
			ResourceID:   modelEntity.ID,
			ResourceName: modelEntity.Name,
			IP:           c.ClientIP(),
		})
	}

	respondCreated(c, modelEntity)
}

// UpdateModel 更新模型
// PUT /api/v1/models/:id
func (h *ModelHandler) UpdateModel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// Fix 5: Verify the model belongs to the requesting tenant before updating.
	existing, err := h.modelService.GetModel(uint(id))
	if err != nil || existing == nil {
		respondErr(c, http.StatusNotFound, "model not found")
		return
	}
	if existing.Provider != nil && existing.Provider.TenantID != 0 && existing.Provider.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "unauthorized")
		return
	}

	var req model.UpdateAIModelRequest
	if !bindJSON(c, &req) {
		return
	}

	modelEntity, err := h.modelService.UpdateModel(uint(id), getTenantID(c), &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     getTenantID(c),
			UserID:       getUserID(c),
			Action:       "model.update",
			ResourceType: "model",
			ResourceID:   uint(id),
			ResourceName: modelEntity.Name,
			IP:           c.ClientIP(),
		})
	}

	respondOK(c, modelEntity)
}

// DeleteModel 删除模型
// DELETE /api/v1/models/:id
func (h *ModelHandler) DeleteModel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// Fix 5: Verify the model belongs to the requesting tenant before deleting.
	existing, err := h.modelService.GetModel(uint(id))
	if err != nil || existing == nil {
		respondErr(c, http.StatusNotFound, "model not found")
		return
	}
	if existing.Provider != nil && existing.Provider.TenantID != 0 && existing.Provider.TenantID != getTenantID(c) {
		respondErr(c, http.StatusForbidden, "unauthorized")
		return
	}

	if err := h.modelService.DeleteModel(uint(id), getTenantID(c)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogEntry(service.AuditEntry{
			TenantID:     getTenantID(c),
			UserID:       getUserID(c),
			Action:       "model.delete",
			ResourceType: "model",
			ResourceID:   uint(id),
			IP:           c.ClientIP(),
		})
	}

	respondOK(c, nil)
}

// TestModel 测试模型
// POST /api/v1/models/:id/test
func (h *ModelHandler) TestModel(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	// Fix 10: Pass tenantID so the service uses tenant-specific provider credentials.
	result, err := h.modelService.TestModel(uint(id), getTenantID(c))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code":    0,
			"message": "success",
			"data": gin.H{
				"success": false,
				"error":   err.Error(),
			},
		})
		return
	}

	respondOK(c, result)
}

// GetAvailableModels 获取任务可用模型
// GET /api/v1/models/available/:task_type
func (h *ModelHandler) GetAvailableModels(c *gin.Context) {
	taskType := c.Param("task_type")

	models, err := h.modelService.GetAvailableModels(taskType, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, models)
}

// SelectModel 选择模型
// POST /api/v1/models/select
func (h *ModelHandler) SelectModel(c *gin.Context) {
	var req struct {
		TaskType string `json:"task_type" binding:"required"`
		Strategy string `json:"strategy"`
	}
	if !bindJSON(c, &req) {
		return
	}

	selected, err := h.modelService.SelectModel(req.TaskType, req.Strategy, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, selected)
}

// GetTaskConfig 获取任务配置
// GET /api/v1/task-configs/:task
func (h *ModelHandler) GetTaskConfig(c *gin.Context) {
	task := c.Param("task")

	config, err := h.modelService.GetTaskConfig(task)
	if err != nil {
		respondErr(c, http.StatusNotFound, "task config not found")
		return
	}

	respondOK(c, config)
}

// UpdateTaskConfig 更新任务配置
// PUT /api/v1/task-configs/:task
func (h *ModelHandler) UpdateTaskConfig(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	task := c.Param("task")

	var req model.UpdateTaskConfigRequest
	if !bindJSON(c, &req) {
		return
	}

	config, err := h.modelService.UpdateTaskConfig(task, &req)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, config)
}

// ListExperiments 获取对比实验列表
// GET /api/v1/experiments
func (h *ModelHandler) ListExperiments(c *gin.Context) {
	experiments, err := h.modelService.ListExperiments(getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondOK(c, experiments)
}

// CreateExperiment 创建对比实验
// POST /api/v1/experiments
func (h *ModelHandler) CreateExperiment(c *gin.Context) {
	var req model.CreateModelComparisonRequest
	if !bindJSON(c, &req) {
		return
	}

	experiment, err := h.modelService.CreateExperiment(&req, getTenantID(c))
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	respondCreated(c, experiment)
}

// GetExperiment 获取实验详情
// GET /api/v1/experiments/:id
func (h *ModelHandler) GetExperiment(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	experiment, err := h.modelService.GetExperiment(uint(id))
	if err != nil {
		respondErr(c, http.StatusNotFound, "experiment not found")
		return
	}

	respondOK(c, experiment)
}

// StartExperiment 开始实验
// POST /api/v1/experiments/:id/start
func (h *ModelHandler) StartExperiment(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}

	if err := h.modelService.StartExperiment(uint(id)); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// FetchProviderModels 从云商 API 拉取模型 ID 列表（OpenAI 兼容 /models 端点）。
// 支持两种模式：
//   - 直接模式：传入 endpoint + api_key（添加新提供商时使用）
//   - ID 模式：传入 provider_id，后端从 DB 读取凭证（编辑已有提供商时使用）
//
// POST /api/v1/model-providers/fetch-models
func (h *ModelHandler) FetchProviderModels(c *gin.Context) {
	var req struct {
		ProviderID uint   `json:"provider_id"`
		Endpoint   string `json:"endpoint"`
		APIKey     string `json:"api_key"`
	}
	if !bindJSON(c, &req) {
		return
	}

	endpoint := req.Endpoint
	apiKey := req.APIKey

	// ID 模式：从 DB 读取凭证，并优先使用 DB 中的静态模型列表
	if req.ProviderID > 0 {
		p, err := h.modelService.GetProvider(req.ProviderID, getTenantID(c))
		if err != nil {
			respondErr(c, http.StatusNotFound, "provider not found")
			return
		}
		// 若 DB 中已配置静态模型列表（不支持 /models 端点的提供商），直接返回
		if p.StaticModels != "" {
			var staticList []string
			if jsonErr := json.Unmarshal([]byte(p.StaticModels), &staticList); jsonErr == nil && len(staticList) > 0 {
				respondOK(c, gin.H{"models": staticList})
				return
			}
		}
		if endpoint == "" {
			endpoint = p.APIEndpoint
		}
		if apiKey == "" {
			apiKey = p.APIKey
		}
	}

	if endpoint == "" {
		respondBadRequest(c, "endpoint is required")
		return
	}

	// SSRF 防护：校验 endpoint 只允许外网 http/https 地址
	if err := validateEndpointURL(endpoint); err != nil {
		respondBadRequest(c, "invalid endpoint: "+err.Error())
		return
	}

	// Ollama 本地服务特殊处理：无需 API Key，使用 /api/tags 获取已安装模型列表
	isOllama := strings.Contains(endpoint, ":11434") || providerNameFromEndpoint(endpoint) == "ollama"
	if !isOllama && apiKey == "" {
		respondBadRequest(c, "api_key is required")
		return
	}
	if isOllama {
		models, err := fetchOllamaModels(c.Request.Context(), endpoint)
		if err != nil {
			respondErr(c, http.StatusBadGateway, "failed to reach Ollama: "+err.Error())
			return
		}
		respondOK(c, gin.H{"models": models})
		return
	}

	// 对不支持 OpenAI /models 端点的提供商，直接返回内置静态列表
	for host, models := range staticProviderModels {
		if strings.Contains(endpoint, host) {
			respondOK(c, gin.H{"models": models})
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/models", nil)
	if err != nil {
		respondBadRequest(c, "invalid endpoint: "+err.Error())
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		respondErr(c, http.StatusBadGateway, "failed to reach provider: "+err.Error())
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		respondErr(c, http.StatusBadGateway, "provider returned error: "+string(body))
		return
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		respondErr(c, http.StatusBadGateway, "invalid response from provider")
		return
	}

	ids := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}

	respondOK(c, gin.H{"models": ids})
}

// ListProviderTemplates 返回系统预置的提供商模板列表（tenant_id=0），
// 供前端"添加提供商"下拉框使用，不含 API Key 等敏感字段。
//
// GET /api/v1/model-providers/templates
func (h *ModelHandler) ListProviderTemplates(c *gin.Context) {
	templates, err := h.modelService.ListSystemProviders()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to list templates")
		return
	}

	type providerTemplate struct {
		Name                string              `json:"name"`
		DisplayName         string              `json:"display_name"`
		APIEndpoint         string              `json:"api_endpoint"`
		NeedsSecretKey      bool                `json:"needs_secret_key"`
		NoAPIKey            bool                `json:"no_api_key,omitempty"`
		NeedsAPIVersion     bool                `json:"needs_api_version,omitempty"`
		DeploymentBased     bool                `json:"deployment_based,omitempty"`
		APIVersionHint      string              `json:"api_version_hint,omitempty"`
		ConfigHint          string              `json:"config_hint,omitempty"`
		StaticModels        []string            `json:"static_models,omitempty"`        // 所有模型展平列表（向后兼容）
		StaticModelsByType  map[string][]string `json:"static_models_by_type,omitempty"` // 按类型分组
	}

	result := make([]providerTemplate, 0, len(templates))
	for _, p := range templates {
		t := providerTemplate{
			Name:           p.Name,
			DisplayName:    p.DisplayName,
			APIEndpoint:    p.APIEndpoint,
			NeedsSecretKey: p.NeedsSecretKey,
			NoAPIKey:       p.Name == "ollama",
		}
		if p.Name == "azure" {
			t.NeedsAPIVersion = true
			t.DeploymentBased = true
			t.APIVersionHint = "2025-01-01-preview"
			t.ConfigHint = "Endpoint 格式：https://<resource>.openai.azure.com/openai；API Version 填写 Azure REST API 版本；模型名称填写 Azure 部署名（与 Azure 门户中创建的 Deployment name 一致）"
		}
		if p.StaticModels != "" {
			// 新格式：map[string][]string；兼容旧格式 []string
			var byType map[string][]string
			if err := json.Unmarshal([]byte(p.StaticModels), &byType); err == nil && len(byType) > 0 {
				t.StaticModelsByType = byType
				// 展平，去重
				seen := map[string]struct{}{}
				for _, models := range byType {
					for _, m := range models {
						if _, ok := seen[m]; !ok {
							seen[m] = struct{}{}
							t.StaticModels = append(t.StaticModels, m)
						}
					}
				}
			} else {
				json.Unmarshal([]byte(p.StaticModels), &t.StaticModels) //nolint:errcheck
			}
		}
		result = append(result, t)
	}

	respondOK(c, result)
}


// TestModelPrompt 用指定提供商生成文本（前端「生成测试」功能）
// POST /api/v1/models/test-prompt
func (h *ModelHandler) TestModelPrompt(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	var req struct {
		ProviderID uint   `json:"provider_id" binding:"required"`
		Prompt     string `json:"prompt" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	tenantID := getTenantID(c)

	start := time.Now()
	content, tokens, err := h.modelService.TestGeneratePrompt(c.Request.Context(), tenantID, req.ProviderID, req.Prompt)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{
		"content":    content,
		"tokens":     tokens,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// GetTaskMappings 返回任务-提供商映射表
// GET /api/v1/models/task-mappings
func (h *ModelHandler) GetTaskMappings(c *gin.Context) {
	mappings := h.modelService.GetTaskProviderMappings()
	respondOK(c, mappings)
}

// UpdateTaskMapping 更新任务-提供商映射
// PUT /api/v1/models/task-mappings
func (h *ModelHandler) UpdateTaskMapping(c *gin.Context) {
	if !isAdminOrOwner(c) {
		respondErr(c, http.StatusForbidden, "admin or owner role required")
		return
	}

	var req struct {
		TaskType   string `json:"task_type" binding:"required"`
		ProviderID *uint  `json:"provider_id"`
	}
	if !bindJSON(c, &req) {
		return
	}
	var pid uint
	if req.ProviderID != nil {
		pid = *req.ProviderID
	}
	if err := h.modelService.SetTaskProviderMapping(req.TaskType, pid); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"ok": true})
}

// validateEndpointURL 验证 endpoint URL 防止 SSRF 攻击。
// 仅允许 http/https scheme，拒绝私有 IP 和 localhost。
func validateEndpointURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https URLs are allowed, got %q", u.Scheme)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip != nil {
		privateRanges := []string{
			"10.", "172.16.", "172.17.", "172.18.", "172.19.", "172.20.",
			"172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.",
			"172.27.", "172.28.", "172.29.", "172.30.", "172.31.",
			"192.168.", "127.", "169.254.", "::1", "fc", "fd",
		}
		ipStr := ip.String()
		for _, prefix := range privateRanges {
			if strings.HasPrefix(ipStr, prefix) {
				return fmt.Errorf("requests to private/internal IP addresses are not allowed")
			}
		}
	}
	if host == "localhost" || host == "0.0.0.0" {
		return fmt.Errorf("requests to localhost are not allowed")
	}
	return nil
}

// providerNameFromEndpoint 从 endpoint 推断提供商名称（用于无 API Key 检测）
func providerNameFromEndpoint(endpoint string) string {
	if strings.Contains(endpoint, "ollama") {
		return "ollama"
	}
	return ""
}

// fetchOllamaModels 从 Ollama /api/tags 获取已安装模型名称列表
func fetchOllamaModels(ctx context.Context, endpoint string) ([]string, error) {
	// endpoint 可能是 http://localhost:11434/v1，需要去掉 /v1 部分
	baseURL := strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1")
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names, nil
}
