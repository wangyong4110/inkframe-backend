package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// McpHandler MCP 工具处理器
type McpHandler struct {
	mcpService *service.McpService
}

func NewMcpHandler(mcpService *service.McpService) *McpHandler {
	return &McpHandler{mcpService: mcpService}
}

// ListMcpTools 获取所有 MCP 工具
// GET /api/v1/mcp-tools
func (h *McpHandler) ListMcpTools(c *gin.Context) {
	tools, err := h.mcpService.ListTools()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": tools})
}

// CreateMcpTool 创建 MCP 工具
// POST /api/v1/mcp-tools
func (h *McpHandler) CreateMcpTool(c *gin.Context) {
	var req model.CreateMcpToolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tool, err := h.mcpService.CreateTool(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "message": "success", "data": tool})
}

// UpdateMcpTool 更新 MCP 工具
// PUT /api/v1/mcp-tools/:id
func (h *McpHandler) UpdateMcpTool(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req model.UpdateMcpToolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tool, err := h.mcpService.UpdateTool(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": tool})
}

// DeleteMcpTool 删除 MCP 工具
// DELETE /api/v1/mcp-tools/:id
func (h *McpHandler) DeleteMcpTool(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := h.mcpService.DeleteTool(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "deleted"})
}

// TestMcpTool 测试 MCP 工具连通性
// POST /api/v1/mcp-tools/:id/test
func (h *McpHandler) TestMcpTool(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	result, err := h.mcpService.TestTool(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": result})
}

// GetMcpToolModels 获取绑定到某 MCP 工具的所有模型
// GET /api/v1/mcp-tools/:id/models
func (h *McpHandler) GetMcpToolModels(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	models, err := h.mcpService.GetToolModels(uint(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": models})
}

// GetModelMcpTools 获取模型绑定的所有 MCP 工具
// GET /api/v1/models/:id/mcp-tools
func (h *McpHandler) GetModelMcpTools(c *gin.Context) {
	modelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}
	tools, err := h.mcpService.GetModelTools(uint(modelID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": tools})
}

// BindMcpTool 将 MCP 工具绑定到模型
// POST /api/v1/models/:id/mcp-tools
func (h *McpHandler) BindMcpTool(c *gin.Context) {
	modelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}
	var req struct {
		ToolID uint `json:"tool_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.mcpService.BindTool(uint(modelID), req.ToolID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "bound"})
}

// UnbindMcpTool 解除模型与 MCP 工具的绑定
// DELETE /api/v1/models/:id/mcp-tools/:tool_id
func (h *McpHandler) UnbindMcpTool(c *gin.Context) {
	modelID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}
	toolID, err := strconv.ParseUint(c.Param("tool_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tool id"})
		return
	}
	if err := h.mcpService.UnbindTool(uint(modelID), uint(toolID)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "unbound"})
}
