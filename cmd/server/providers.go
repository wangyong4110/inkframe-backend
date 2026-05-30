package main

import (
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/vector"
)

// initAIModule 返回空 ModelManager。
// 所有 AI 提供商均通过"模型管理"页面由租户配置，从数据库按需加载；
// 不再从 config.yaml 或环境变量静态注册提供商。
func initAIModule(_ *config.Config) *ai.ModelManager {
	manager := ai.NewModelManager()
	logger.Println("initAIModule: all providers loaded from DB per-tenant (no static registration)")
	return manager
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
		chromaStore := vector.NewChromaStore(cfg.VectorDB.Endpoint)
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
