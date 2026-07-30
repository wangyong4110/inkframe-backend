package main

import (
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/vector"

	// 注册所有 AI 供应商工厂到 ai.Registry，init() 自动执行。
	_ "github.com/inkframe/inkframe-backend/docs"
	_ "github.com/inkframe/inkframe-backend/internal/ai/anthropic"
	_ "github.com/inkframe/inkframe-backend/internal/ai/dashscope"
	_ "github.com/inkframe/inkframe-backend/internal/ai/deepseek"
	_ "github.com/inkframe/inkframe-backend/internal/ai/doubao"
	_ "github.com/inkframe/inkframe-backend/internal/ai/elevenlabs"
	_ "github.com/inkframe/inkframe-backend/internal/ai/hunyuan"
	_ "github.com/inkframe/inkframe-backend/internal/ai/kling"
	_ "github.com/inkframe/inkframe-backend/internal/ai/minimax"
	_ "github.com/inkframe/inkframe-backend/internal/ai/openai"
	_ "github.com/inkframe/inkframe-backend/internal/ai/volcengine"
)

// initAIModule 初始化 AI 模块。
// 所有 AI 提供商均通过“模型管理”页面由租户配置，从数据库按需加载；
// 不再从 config.yaml 或环境变量静态注册提供商。
func initAIModule(_ *config.Config) {
	logger.Println("initAIModule: all providers loaded from DB per-tenant (no static registration)")
}

// initVectorStore 初始化向量存储
// 优先使用 config.yaml 的 vector_db 配置；API Key 敏感字段走环境变量。
func initVectorStore(cfg *config.Config) *vector.StoreManager {
	manager := vector.NewStoreManager(nil)

	switch cfg.VectorDB.Type {
	case "dashvector":
		apiKey := getEnv("DASHVECTOR_API_KEY", cfg.VectorDB.APIKey)
		dashStore := vector.NewDashVectorStore(cfg.VectorDB.Endpoint, apiKey)
		manager.RegisterStore("dashvector", dashStore)
		logger.Printf("VectorStore: DashVector @ %s", cfg.VectorDB.Endpoint)

	case "chroma":
		chromaStore := vector.NewChromaStore(cfg.VectorDB.Endpoint, cfg.VectorDB.APIKey)
		manager.RegisterStore("chroma", chromaStore)
		logger.Printf("VectorStore: Chroma @ %s", cfg.VectorDB.Endpoint)

	default: // "qdrant" 或未填，向后兼容
		endpoint := getEnv("QDRANT_ENDPOINT", cfg.VectorDB.Endpoint)
		if endpoint == "" {
			endpoint = "localhost:6333"
		}
		apiKey := getEnv("QDRANT_API_KEY", cfg.VectorDB.APIKey)
		qdrantStore := vector.NewQdrantStore(endpoint, apiKey)
		manager.RegisterStore("qdrant", qdrantStore)
		logger.Printf("VectorStore: Qdrant @ %s", endpoint)
	}

	return manager
}
