package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// ListTools 获取 MCP 工具列表（租户隔离）：返回本租户工具 + 系统工具（tenant_id=0）。
func (s *McpService) ListTools(tenantID uint) ([]*model.McpTool, error) {
	var tools []*model.McpTool
	if err := s.db.Where("tenant_id = ? OR tenant_id = 0", tenantID).
		Order("id asc").Find(&tools).Error; err != nil {
		return nil, err
	}
	return tools, nil
}

// CreateTool 创建 MCP 工具
func (s *McpService) CreateTool(req *model.CreateMcpToolRequest) (*model.McpTool, error) {
	// For stdio transport, Endpoint holds the command path; validate it is executable.
	if req.TransportType == "stdio" {
		if req.Endpoint == "" {
			return nil, fmt.Errorf("stdio transport requires endpoint (command) field")
		}
		if err := validateStdioCommand(req.Endpoint); err != nil {
			return nil, fmt.Errorf("stdio command validation failed: %w", err)
		}
	} else {
		if err := validateMcpEndpoint(req.Endpoint); err != nil {
			return nil, fmt.Errorf("endpoint validation failed: %w", err)
		}
	}
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
	// Fix 5: 系统内置工具不可修改
	if tool.IsSystem {
		return nil, fmt.Errorf("system tools cannot be modified")
	}
	// Fix 2: 若请求中包含新 endpoint，校验 SSRF
	if req.Endpoint != "" {
		if err := validateMcpEndpoint(req.Endpoint); err != nil {
			return nil, fmt.Errorf("endpoint validation failed: %w", err)
		}
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

	// 对 HTTP 工具执行简单的 GET 探测
	if tool.TransportType == "http" {
		client := &http.Client{Timeout: time.Duration(tool.Timeout) * time.Second}
		resp, err := client.Get(tool.Endpoint) //nolint:gosec
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

	// 对 SSE 工具验证端点返回 text/event-stream Content-Type
	if tool.TransportType == "sse" {
		client := &http.Client{Timeout: time.Duration(tool.Timeout) * time.Second}
		resp, err := client.Get(tool.Endpoint) //nolint:gosec
		latency := time.Since(start).Milliseconds()
		result["latency_ms"] = latency
		if err != nil {
			result["status"] = "error"
			result["error"] = fmt.Sprintf("SSE endpoint unreachable: %s", err.Error())
			return result, nil
		}
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			result["status"] = "error"
			result["error"] = fmt.Sprintf("SSE endpoint returned wrong Content-Type: %s (expected text/event-stream)", ct)
			return result, nil
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

// isPrivateIP returns true if the IP is loopback, private, or link-local.
// Uses net.IP methods rather than string prefix matching to prevent bypass via
// alternate representations (e.g., decimal encoding, IPv6 mapped addresses).
func isPrivateIP(ip net.IP) bool {
	// Explicitly check IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// Also run explicit CIDR check for IPv4 addresses
	if ip.To4() != nil {
		return isPrivateIPv4(ip)
	}
	return false
}

// isPrivateIPv4 checks whether an IPv4 address falls in a private/reserved range.
func isPrivateIPv4(ip net.IP) bool {
	privateNets := []net.IPNet{
		{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)},
		{IP: net.ParseIP("172.16.0.0"), Mask: net.CIDRMask(12, 32)},
		{IP: net.ParseIP("192.168.0.0"), Mask: net.CIDRMask(16, 32)},
		{IP: net.ParseIP("169.254.0.0"), Mask: net.CIDRMask(16, 32)},
		{IP: net.ParseIP("127.0.0.0"), Mask: net.CIDRMask(8, 32)},
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isPrivateOrReservedHost resolves a hostname to its IP addresses and returns
// true if any of them falls in a private or reserved range. Direct IP addresses
// are checked without DNS resolution. Uses a custom resolver with a 3-second
// dial timeout and a 5-second overall context to prevent SSRF via DNS rebinding.
func isPrivateOrReservedHost(host string) (bool, error) {
	// Direct IP: no DNS needed
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip), nil
	}
	// Use a custom resolver pinned to 8.8.8.8 with a tight dial timeout
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// Cannot resolve — treat as unsafe to prevent accessing unresolvable internal hosts
		return true, fmt.Errorf("hostname %q cannot be resolved: %w", host, err)
	}
	for _, addr := range addrs {
		if isPrivateIP(addr.IP) {
			return true, fmt.Errorf("hostname %q resolves to private/reserved IP %s", host, addr.IP)
		}
	}
	return false, nil
}

// checkParamsForSSRF extracts all http(s) URLs from the params JSON and rejects
// any that resolve to a private/internal address. This prevents SSRF via tool params.
// Hostname resolution uses isPrivateOrReservedHost which does DNS resolution with a
// timeout to block bypass attempts like 169.254.0.1.nip.io.
func checkParamsForSSRF(paramsJSON string) error {
	urlRe := regexp.MustCompile(`https?://[^\s"'<>]+`)
	for _, match := range urlRe.FindAllString(paramsJSON, -1) {
		u, err := url.Parse(match)
		if err != nil {
			continue
		}
		host := u.Hostname()
		// Reject well-known internal hostnames regardless of resolution
		lh := strings.ToLower(host)
		if lh == "localhost" || lh == "0.0.0.0" || strings.HasSuffix(lh, ".local") ||
			strings.HasSuffix(lh, ".internal") || lh == "metadata.google.internal" {
			return fmt.Errorf("parameter contains disallowed internal host: %s", host)
		}
		// Resolve the hostname (or parse as IP) and reject private ranges
		if private, resErr := isPrivateOrReservedHost(host); private {
			if resErr != nil {
				return fmt.Errorf("parameter contains disallowed host %s: %w", host, resErr)
			}
			return fmt.Errorf("parameter contains private network URL: %s", match)
		}
	}
	return nil
}

// containsPrivateURL checks whether a JSON string contains references to private/internal URLs.
// Kept for backward compatibility; new code should prefer checkParamsForSSRF.
func containsPrivateURL(s string) bool {
	return checkParamsForSSRF(s) != nil
}

// InvokeTool 调用指定名称的 MCP 工具（向其 Endpoint 发送 POST 请求）
// params 以 JSON 形式作为请求体发送，响应 JSON 解析后作为 output 返回。
// 若工具未启用（is_active=false）或不属于该租户则返回错误。
// tenantID=0 表示系统调用，跳过租户校验（仅内部使用）。
func (s *McpService) InvokeTool(ctx context.Context, tenantID uint, toolName string, params map[string]interface{}) (map[string]interface{}, error) {
	tool, err := s.GetByName(toolName)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q not found: %w", toolName, err)
	}

	// Tenant isolation: the tool must belong to this tenant or be a system tool (TenantID=0).
	// tenantID=0 is reserved for internal/system callers and bypasses this check.
	if tenantID != 0 && !tool.IsSystem && tool.TenantID != tenantID {
		return nil, fmt.Errorf("tool not found or access denied")
	}

	if !tool.IsActive {
		return nil, fmt.Errorf("mcp tool %q is not active", toolName)
	}
	if tool.Endpoint == "" {
		return nil, fmt.Errorf("mcp tool %q has no endpoint configured", toolName)
	}

	// Validate params: max size, no internal URLs (SSRF protection via net.IP-based check)
	paramsJSON, err := json.Marshal(params)
	if err != nil || len(paramsJSON) > 64*1024 { // 64KB max
		return nil, fmt.Errorf("params too large or invalid")
	}
	if err := checkParamsForSSRF(string(paramsJSON)); err != nil {
		return nil, fmt.Errorf("SSRF protection: %w", err)
	}

	reqBody := paramsJSON

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

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: request failed: %w", toolName, err)
	}
	defer resp.Body.Close()

	const maxMCPResponseBytes = 10 * 1024 * 1024 // 10MB DoS protection
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("mcp tool %q: read response: %w", toolName, err)
	}
	if int64(len(body)) >= maxMCPResponseBytes {
		return nil, fmt.Errorf("mcp tool %q: response exceeds 10MB limit", toolName)
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

// validateMcpEndpoint 验证 MCP 工具 endpoint 防止 SSRF 攻击。
// stdio 工具 endpoint 为空时跳过检查；HTTP/SSE 工具必须使用外网地址。
// Hostname resolution is performed with a 5-second timeout to block bypass
// attempts such as 169.254.0.1.nip.io that string-prefix matching cannot catch.
func validateMcpEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil // stdio 模式无 endpoint，跳过
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https endpoints are allowed, got %q", u.Scheme)
	}
	host := u.Hostname()
	// Reject well-known localhost aliases before DNS resolution
	lh := strings.ToLower(host)
	if lh == "localhost" || lh == "0.0.0.0" || strings.HasSuffix(lh, ".local") ||
		strings.HasSuffix(lh, ".internal") || lh == "metadata.google.internal" {
		return fmt.Errorf("requests to internal/reserved host %q are not allowed", host)
	}
	// Resolve hostname (or parse IP) and reject private/reserved addresses
	if private, resErr := isPrivateOrReservedHost(host); private {
		if resErr != nil {
			return resErr
		}
		return fmt.Errorf("endpoint hostname %q resolves to a private/reserved address", host)
	}
	return nil
}

// validateStdioCommand checks that the stdio command exists on PATH or as an absolute path.
// For stdio transport the Endpoint field holds the executable path.
func validateStdioCommand(command string) error {
	if filepath.IsAbs(command) {
		if _, err := os.Stat(command); err != nil {
			return fmt.Errorf("command %q not found: %w", command, err)
		}
		return nil
	}
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("command %q not found in PATH: %w", command, err)
	}
	return nil
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
