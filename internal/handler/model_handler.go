package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// ModelHandler 模型管理处理器
type ModelHandler struct {
	modelService *service.ModelService
}

func NewModelHandler(modelService *service.ModelService) *ModelHandler {
	return &ModelHandler{modelService: modelService}
}

// ListProviders 获取提供商列表
// GET /api/v1/model-providers
func (h *ModelHandler) ListProviders(c *gin.Context) {
	providers, err := h.modelService.ListProviders()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    providers,
	})
}

// CreateProvider 创建提供商
// POST /api/v1/model-providers
func (h *ModelHandler) CreateProvider(c *gin.Context) {
	var req model.CreateModelProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	provider, err := h.modelService.CreateProvider(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    provider,
	})
}

// UpdateProvider 更新提供商
// PUT /api/v1/model-providers/:id
func (h *ModelHandler) UpdateProvider(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider id"})
		return
	}

	var req model.UpdateModelProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	provider, err := h.modelService.UpdateProvider(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    provider,
	})
}

// DeleteProvider 删除提供商
// DELETE /api/v1/model-providers/:id
func (h *ModelHandler) DeleteProvider(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider id"})
		return
	}

	if err := h.modelService.DeleteProvider(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// TestProvider 测试提供商连接
// POST /api/v1/model-providers/:id/test
func (h *ModelHandler) TestProvider(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider id"})
		return
	}

	result, err := h.modelService.TestProvider(uint(id))
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

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
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

	models, err := h.modelService.ListModels(providerId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    models,
	})
}

// CreateModel 创建模型
// POST /api/v1/models
func (h *ModelHandler) CreateModel(c *gin.Context) {
	var req model.CreateAIModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	modelEntity, err := h.modelService.CreateModel(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    modelEntity,
	})
}

// UpdateModel 更新模型
// PUT /api/v1/models/:id
func (h *ModelHandler) UpdateModel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}

	var req model.UpdateAIModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	modelEntity, err := h.modelService.UpdateModel(uint(id), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    modelEntity,
	})
}

// DeleteModel 删除模型
// DELETE /api/v1/models/:id
func (h *ModelHandler) DeleteModel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}

	if err := h.modelService.DeleteModel(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}

// TestModel 测试模型
// POST /api/v1/models/:id/test
func (h *ModelHandler) TestModel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}

	result, err := h.modelService.TestModel(uint(id))
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

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    result,
	})
}

// GetTaskConfig 获取任务配置
// GET /api/v1/task-configs/:task
func (h *ModelHandler) GetTaskConfig(c *gin.Context) {
	task := c.Param("task")

	config, err := h.modelService.GetTaskConfig(task)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task config not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    config,
	})
}

// UpdateTaskConfig 更新任务配置
// PUT /api/v1/task-configs/:task
func (h *ModelHandler) UpdateTaskConfig(c *gin.Context) {
	task := c.Param("task")

	var req model.UpdateTaskConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	config, err := h.modelService.UpdateTaskConfig(task, &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    config,
	})
}

// ListExperiments 获取对比实验列表
// GET /api/v1/experiments
func (h *ModelHandler) ListExperiments(c *gin.Context) {
	experiments, err := h.modelService.ListExperiments()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    experiments,
	})
}

// CreateExperiment 创建对比实验
// POST /api/v1/experiments
func (h *ModelHandler) CreateExperiment(c *gin.Context) {
	var req model.CreateModelComparisonRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	experiment, err := h.modelService.CreateExperiment(&req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"code":    0,
		"message": "success",
		"data":    experiment,
	})
}

// GetExperiment 获取实验详情
// GET /api/v1/experiments/:id
func (h *ModelHandler) GetExperiment(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid experiment id"})
		return
	}

	experiment, err := h.modelService.GetExperiment(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "experiment not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data":    experiment,
	})
}

// StartExperiment 开始实验
// POST /api/v1/experiments/:id/start
func (h *ModelHandler) StartExperiment(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid experiment id"})
		return
	}

	if err := h.modelService.StartExperiment(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
	})
}
