package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// McpService MCP 工具管理服务
type McpService struct {
	db *gorm.DB
}

func NewMcpService(db *gorm.DB) *McpService {
	return &McpService{db: db}
}

// ListTools 获取所有 MCP 工具
func (s *McpService) ListTools() ([]*model.McpTool, error) {
	var tools []*model.McpTool
	if err := s.db.Order("id asc").Find(&tools).Error; err != nil {
		return nil, err
	}
	return tools, nil
}

// CreateTool 创建 MCP 工具
func (s *McpService) CreateTool(req *model.CreateMcpToolRequest) (*model.McpTool, error) {
	headersJSON, err := marshalJSON(req.Headers)
	if err != nil {
		return nil, fmt.Errorf("invalid headers: %w", err)
	}
	envJSON, err := marshalJSON(req.Env)
	if err != nil {
		return nil, fmt.Errorf("invalid env: %w", err)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	tool := &model.McpTool{
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		Description:   req.Description,
		TransportType: req.TransportType,
		Endpoint:      req.Endpoint,
		Headers:       headersJSON,
		Env:           envJSON,
		Timeout:       timeout,
		IsActive:      req.IsActive,
	}
	if tool.DisplayName == "" {
		tool.DisplayName = tool.Name
	}
	if err := s.db.Create(tool).Error; err != nil {
		return nil, err
	}
	return tool, nil
}

// UpdateTool 更新 MCP 工具
func (s *McpService) UpdateTool(id uint, req *model.UpdateMcpToolRequest) (*model.McpTool, error) {
	var tool model.McpTool
	if err := s.db.First(&tool, id).Error; err != nil {
		return nil, err
	}
	if req.DisplayName != "" {
		tool.DisplayName = req.DisplayName
	}
	if req.Description != "" {
		tool.Description = req.Description
	}
	if req.TransportType != "" {
		tool.TransportType = req.TransportType
	}
	if req.Endpoint != "" {
		tool.Endpoint = req.Endpoint
	}
	if req.Headers != nil {
		if h, err := marshalJSON(req.Headers); err == nil {
			tool.Headers = h
		}
	}
	if req.Env != nil {
		if e, err := marshalJSON(req.Env); err == nil {
			tool.Env = e
		}
	}
	if req.Timeout > 0 {
		tool.Timeout = req.Timeout
	}
	if req.IsActive != nil {
		tool.IsActive = *req.IsActive
	}
	if err := s.db.Save(&tool).Error; err != nil {
		return nil, err
	}
	return &tool, nil
}

// DeleteTool 删除 MCP 工具（系统内置工具不可删除）
func (s *McpService) DeleteTool(id uint) error {
	var tool model.McpTool
	if err := s.db.First(&tool, id).Error; err != nil {
		return err
	}
	if tool.IsSystem {
		return fmt.Errorf("system tools cannot be deleted")
	}
	// 先删除绑定关系，再删除工具本身
	if err := s.db.Where("mcp_tool_id = ?", id).Delete(&model.ModelMcpBinding{}).Error; err != nil {
		return fmt.Errorf("failed to remove tool bindings: %w", err)
	}
	return s.db.Delete(&tool).Error
}

// TestTool 测试 MCP 工具连通性
func (s *McpService) TestTool(id uint) (map[string]interface{}, error) {
	var tool model.McpTool
	if err := s.db.First(&tool, id).Error; err != nil {
		return nil, err
	}
	start := time.Now()
	result := map[string]interface{}{
		"status":     "ok",
		"latency_ms": 0,
	}

	// 对 HTTP/SSE 工具执行简单的 GET 探测
	if tool.TransportType == "http" || tool.TransportType == "sse" {
		client := &http.Client{Timeout: time.Duration(tool.Timeout) * time.Second}
		resp, err := client.Get(tool.Endpoint)
		latency := time.Since(start).Milliseconds()
		result["latency_ms"] = latency
		if err != nil {
			result["status"] = "error"
			result["error"] = err.Error()
			return result, nil
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			result["status"] = "error"
			result["error"] = fmt.Sprintf("server returned HTTP %d", resp.StatusCode)
		}
		return result, nil
	}

	// stdio 工具无法远程探测，直接返回 ok
	result["latency_ms"] = time.Since(start).Milliseconds()
	return result, nil
}

// GetToolModels 获取绑定到某 MCP 工具的所有模型
func (s *McpService) GetToolModels(toolID uint) ([]*model.AIModel, error) {
	var bindings []model.ModelMcpBinding
	if err := s.db.Where("mcp_tool_id = ?", toolID).Find(&bindings).Error; err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return []*model.AIModel{}, nil
	}
	ids := make([]uint, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.ModelID)
	}
	var models []*model.AIModel
	if err := s.db.Where("id IN ?", ids).Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// GetModelTools 获取模型绑定的所有 MCP 工具
func (s *McpService) GetModelTools(modelID uint) ([]*model.McpTool, error) {
	var bindings []model.ModelMcpBinding
	if err := s.db.Where("model_id = ?", modelID).Find(&bindings).Error; err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return []*model.McpTool{}, nil
	}
	ids := make([]uint, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.McpToolID)
	}
	var tools []*model.McpTool
	if err := s.db.Where("id IN ?", ids).Find(&tools).Error; err != nil {
		return nil, err
	}
	return tools, nil
}

// BindTool 绑定 MCP 工具到模型（幂等）
func (s *McpService) BindTool(modelID, toolID uint) error {
	var binding model.ModelMcpBinding
	err := s.db.Where("model_id = ? AND mcp_tool_id = ?", modelID, toolID).First(&binding).Error
	if err == nil {
		// 已存在，确保 enabled
		binding.Enabled = true
		return s.db.Save(&binding).Error
	}
	return s.db.Create(&model.ModelMcpBinding{
		ModelID:   modelID,
		McpToolID: toolID,
		Enabled:   true,
	}).Error
}

// UnbindTool 解除绑定
func (s *McpService) UnbindTool(modelID, toolID uint) error {
	return s.db.Where("model_id = ? AND mcp_tool_id = ?", modelID, toolID).
		Delete(&model.ModelMcpBinding{}).Error
}

// GetByName 按工具名称查找 MCP 工具
func (s *McpService) GetByName(name string) (*model.McpTool, error) {
	var tool model.McpTool
	if err := s.db.Where("name = ?", name).First(&tool).Error; err != nil {
		return nil, err
	}
	return &tool, nil
}

// InvokeTool 调用指定名称的 MCP 工具（向其 Endpoint 发送 POST 请求）
// params 以 JSON 形式作为请求体发送，响应 JSON 解析后作为 output 返回。
// 若工具未启用（is_active=false）则返回错误。
func (s *McpService) InvokeTool(ctx context.Context, toolName string, params map[string]interface{}) (map[string]interface{}, error) {
	tool, err := s.GetByName(toolName)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q not found: %w", toolName, err)
	}
	if !tool.IsActive {
		return nil, fmt.Errorf("mcp tool %q is not active", toolName)
	}
	if tool.Endpoint == "" {
		return nil, fmt.Errorf("mcp tool %q has no endpoint configured", toolName)
	}

	reqBody, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: marshal params: %w", toolName, err)
	}

	timeout := time.Duration(tool.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tool.Endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: create request: %w", toolName, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add custom headers from tool configuration if any
	if tool.Headers != "" {
		var headers map[string]string
		if jsonErr := json.Unmarshal([]byte(tool.Headers), &headers); jsonErr == nil {
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: request failed: %w", toolName, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: read response: %w", toolName, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp tool %q: HTTP %d: %s", toolName, resp.StatusCode, string(body))
	}

	var output map[string]interface{}
	if err := json.Unmarshal(body, &output); err != nil {
		return nil, fmt.Errorf("mcp tool %q: parse response: %w", toolName, err)
	}
	return output, nil
}

// marshalJSON 将 map 序列化为 JSON 字符串；nil map 返回空字符串
func marshalJSON(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
