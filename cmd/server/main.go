package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/router"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/vector"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	// 1. 加载配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Printf("Config file not found, using defaults")
		cfg = config.DefaultConfig()
	}

	// 2. 初始化数据库
	db, err := initDatabase(cfg)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}

	// 3. 自动迁移
	if err := autoMigrate(db); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	// 4. 初始化Redis
	redisClient := initRedis(cfg)

	// 5. 初始化AI模块
	aiManager := initAIModule(cfg)

	// 6. 初始化向量存储
	vectorStore := initVectorStore(cfg)

	// 7. 初始化仓库层
	repos := initRepositories(db, redisClient)

	// 7.5. 种子系统级 AI provider（tenant_id=0），用于租户无私有配置时的兜底
	seedSystemProviders(repos.ModelProviderRepo, cfg)

	// 8. 初始化服务层
	services := initServices(db, repos, aiManager, vectorStore, cfg)

	// 9. 初始化处理器
	handlers := initHandlers(services)

	// 10. 设置路由
	r := router.SetupRouter(&router.Config{
		JWTSecret:        cfg.Server.JWTSecret,
		NovelHandler:     handlers.NovelHandler,
		ChapterHandler:   handlers.ChapterHandler,
		CharacterHandler: handlers.CharacterHandler,
		VideoHandler:     handlers.VideoHandler,
		ModelHandler:     handlers.ModelHandler,
		McpHandler:       handlers.McpHandler,
		StyleHandler:     handlers.StyleHandler,
		ContextHandler:   handlers.ContextHandler,
		AuthHandler:      handlers.AuthHandler,
		ImportHandler:    handlers.ImportHandler,
		WorldviewHandler: handlers.WorldviewHandler,
		TenantHandler:    handlers.TenantHandler,
	})

	// 11. 设置Gin模式
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 12. 创建服务器
	srv := &http.Server{
		Addr:           cfg.Server.GetAddr(),
		Handler:        r,
		ReadTimeout:    cfg.Server.ReadTimeout,
		WriteTimeout:   cfg.Server.WriteTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
	}

	// 13. 启动服务器
	go func() {
		log.Printf("Server starting on %s", cfg.Server.GetAddr())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	// 14. 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// 15. 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	// 16. 关闭数据库连接
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close()
	}

	// 17. 关闭Redis连接
	if redisClient != nil {
		redisClient.Close()
	}

	log.Println("Server exited")
}

// initDatabase 初始化数据库
func initDatabase(cfg *config.Config) (*gorm.DB, error) {
	dsn := cfg.Database.GetDSN()

	gormLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                 logger.Info,
			IgnoreRecordNotFoundError: true,
			Colorful:                 true,
		},
	)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: gormLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	// 设置连接池
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get database instance: %w", err)
	}

	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return db, nil
}

// autoMigrate 自动迁移
func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.Tenant{},
		&model.User{},
		&model.TenantUser{},
		&model.TenantProject{},
		&model.Novel{},
		&model.Chapter{},
		&model.PlotPoint{},
		&model.Character{},
		&model.CharacterAppearance{},
		&model.CharacterStateSnapshot{},
		&model.Worldview{},
		&model.WorldviewEntity{},
		&model.ReferenceNovel{},
		&model.ReferenceChapter{},
		&model.KnowledgeBase{},
		&model.PromptTemplate{},
		&model.AIModel{},
		&model.ModelProvider{},
		&model.TaskModelConfig{},
		&model.ModelComparisonExperiment{},
		&model.ExperimentResult{},
		&model.ModelUsageLog{},
		&model.Video{},
		&model.StoryboardShot{},
		&model.CharacterVisualDesign{},
		&model.SceneVisualDesign{},
		&model.QualityReport{},
		&model.ReviewTask{},
		&model.ChapterVersion{},
		&model.FeedbackRecord{},
		&model.McpTool{},
		&model.ModelMcpBinding{},
	)
}

// initRedis 初始化Redis
func initRedis(cfg *config.Config) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.GetRedisAddr(),
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	})

	// 测试连接
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("Warning: Redis connection failed: %v", err)
		return nil
	}

	log.Println("Redis connected successfully")
	return client
}

// initAIModule 初始化AI模块
func initAIModule(cfg *config.Config) *ai.ModelManager {
	manager := ai.NewModelManager()

	// 注册 OpenAI
	openaiProvider := ai.NewOpenAIProvider(
		getEnv("OPENAI_API_KEY", ""),
		"",
		"gpt-4",
	)
	manager.RegisterProvider("openai", openaiProvider)

	// 注册 Claude
	claudeProvider := ai.NewAnthropicProvider(
		getEnv("ANTHROPIC_API_KEY", ""),
		"",
		"claude-3-opus-20240229",
	)
	manager.RegisterProvider("anthropic", claudeProvider)

	// 注册 Google
	googleProvider := ai.NewGoogleProvider(
		getEnv("GOOGLE_API_KEY", ""),
		"",
		"gemini-pro",
	)
	manager.RegisterProvider("google", googleProvider)

	// 注册豆包（含 Seedream 图像）
	doubaoProvider := ai.NewDoubaoProvider(
		getEnv("DOUBAO_API_KEY", ""),
		cfg.AI.Doubao.Endpoint,
		cfg.AI.Doubao.Model,
	)
	manager.RegisterProvider("doubao", doubaoProvider)

	// 注册 DeepSeek
	deepseekProvider := ai.NewDeepSeekProvider(
		getEnv("DEEPSEEK_API_KEY", ""),
		cfg.AI.DeepSeek.Endpoint,
		cfg.AI.DeepSeek.Model,
	)
	manager.RegisterProvider("deepseek", deepseekProvider)

	// 注册通义千问（含 Wan 图像）
	qianwenProvider := ai.NewQianwenProvider(
		getEnv("QIANWEN_API_KEY", ""),
		cfg.AI.Qianwen.Endpoint,
		cfg.AI.Qianwen.Model,
	)
	manager.RegisterProvider("qianwen", qianwenProvider)

	// 设置默认
	manager.SetDefault("openai")

	// 为所有 Provider 包装指数退避重试（最多 3 次，基础延迟 500ms）
	for _, name := range manager.ListProviders() {
		if err := manager.WrapWithRetry(name, 3, 500*time.Millisecond); err != nil {
			log.Printf("Warning: failed to wrap provider %s with retry: %v", name, err)
		}
	}

	return manager
}

// initVideoProviders 初始化视频生成提供者
// 返回可用的 VideoProvider 列表，供视频服务按需选用
func initVideoProviders(cfg *config.Config) map[string]ai.VideoProvider {
	providers := make(map[string]ai.VideoProvider)

	// Kling 快手可灵
	klingKey := getEnv("KLING_API_KEY", "")
	if klingKey != "" {
		providers["kling"] = ai.NewKlingProvider(klingKey, "")
	}

	// Seedance 字节跳动火山引擎
	seedanceKey := getEnv("SEEDANCE_API_KEY", cfg.AI.Seedance.APIKey)
	if seedanceKey != "" {
		providers["seedance"] = ai.NewSeedanceProvider(seedanceKey, cfg.AI.Seedance.Endpoint)
	}

	log.Printf("Initialized video providers: %d registered", len(providers))
	return providers
}

// initVectorStore 初始化向量存储
func initVectorStore(cfg *config.Config) *vector.StoreManager {
	manager := vector.NewStoreManager(nil)

	// 注册 Qdrant
	qdrantStore := vector.NewQdrantStore(
		getEnv("QDRANT_ENDPOINT", "localhost:6333"),
		getEnv("QDRANT_API_KEY", ""),
	)
	manager.RegisterStore("qdrant", qdrantStore)

	// 注册 Chroma
	chromaStore := vector.NewChromaStore(
		getEnv("CHROMA_ENDPOINT", "localhost:8000"),
	)
	manager.RegisterStore("chroma", chromaStore)

	return manager
}

// Repositories 仓库层
type Repositories struct {
	NovelRepo             *repository.NovelRepository
	ChapterRepo           *repository.ChapterRepository
	CharacterRepo         *repository.CharacterRepository
	WorldviewRepo         *repository.WorldviewRepository
	AIModelRepo           *repository.AIModelRepository
	TaskModelConfigRepo   *repository.TaskModelConfigRepository
	VideoRepo             *repository.VideoRepository
	StoryboardRepo        *repository.StoryboardRepository
	KnowledgeBaseRepo     *repository.KnowledgeBaseRepository
	ModelProviderRepo     *repository.ModelProviderRepository
	ModelComparisonRepo   *repository.ModelComparisonRepository
	ReviewTaskRepo        *repository.ReviewTaskRepository
	ChapterVersionRepo    *repository.ChapterVersionRepository
	SnapshotRepo          *repository.CharacterStateSnapshotRepository
	UserRepo              *repository.UserRepository
	TenantRepo            *repository.TenantRepository
	TenantUserRepo        *repository.TenantUserRepository
}

// initRepositories 初始化仓库层
func initRepositories(db *gorm.DB, redis *redis.Client) *Repositories {
	return &Repositories{
		NovelRepo:            repository.NewNovelRepository(db, redis),
		ChapterRepo:          repository.NewChapterRepository(db, redis),
		CharacterRepo:        repository.NewCharacterRepository(db),
		WorldviewRepo:        repository.NewWorldviewRepository(db),
		AIModelRepo:          repository.NewAIModelRepository(db),
		TaskModelConfigRepo:  repository.NewTaskModelConfigRepository(db),
		VideoRepo:            repository.NewVideoRepository(db),
		StoryboardRepo:       repository.NewStoryboardRepository(db),
		KnowledgeBaseRepo:    repository.NewKnowledgeBaseRepository(db),
		ModelProviderRepo:    repository.NewModelProviderRepository(db),
		ModelComparisonRepo:  repository.NewModelComparisonRepository(db),
		ReviewTaskRepo:       repository.NewReviewTaskRepository(db),
		ChapterVersionRepo:   repository.NewChapterVersionRepository(db),
		SnapshotRepo:         repository.NewCharacterStateSnapshotRepository(db),
		UserRepo:             repository.NewUserRepository(db),
		TenantRepo:           repository.NewTenantRepository(db),
		TenantUserRepo:       repository.NewTenantUserRepository(db),
	}
}

// Services 服务层
type Services struct {
	McpService                *service.McpService
	NovelService              *service.NovelService
	ChapterService            *service.ChapterService
	CharacterService          *service.CharacterService
	WorldviewService          *service.WorldviewService
	QualityControlService     *service.QualityControlService
	VideoService              *service.VideoService
	ModelService              *service.ModelService
	PromptService             *service.PromptService
	ContinuityService         *service.ContinuityService
	KnowledgeService          *service.KnowledgeService
	ReviewTaskService         *service.ReviewTaskService
	ChapterVersionService     *service.ChapterVersionService
	ForeshadowService         *service.ForeshadowService
	TimelineService           *service.TimelineService
	CharacterArcService       *service.CharacterArcService
	StyleService              *service.StyleService
	GenerationContextService  *service.GenerationContextService
	ImageGenerationService    *service.ImageGenerationService
	StoryboardService         *service.StoryboardService
	VideoEnhancementService        *service.VideoEnhancementService
	CharacterConsistencyService    *service.CharacterConsistencyService
	FrameGeneratorService          *service.FrameGeneratorService
	ConsistencyValidatorService    *service.ConsistencyValidatorService
	BGMService                *service.BGMService
	CrawlerService            *crawler.NovelCrawler
	NovelImportService        *service.NovelImportService
	NovelToVideoService       *service.NovelToVideoService
	AuthService               *service.AuthService
	TenantService             *service.TenantService
}

// initServices 初始化服务层
func initServices(db *gorm.DB, repos *Repositories, aiManager *ai.ModelManager, vectorStore *vector.StoreManager, cfg *config.Config) *Services {
	// AI服务（注入 providerRepo 以支持按租户加载 AK/SK）
	aiService := service.NewAIService(repos.AIModelRepo, repos.TaskModelConfigRepo, aiManager, repos.ModelProviderRepo)

	// 小说服务
	novelService := service.NewNovelService(repos.NovelRepo, repos.ChapterRepo, aiService).
		WithCharacterRepos(repos.CharacterRepo, repos.SnapshotRepo)

	// 章节服务
	// chapterService is wired after generationContextService is built (see below)

	// 角色服务
	characterService := service.NewCharacterService(repos.CharacterRepo, aiService)

	// 世界观服务
	worldviewService := service.NewWorldviewService(repos.WorldviewRepo, aiService)

	// 质量控制服务
	qualityControlService := service.NewQualityControlService(aiManager, repos.ChapterRepo, repos.NovelRepo)

	// 视频服务
	videoProviders := initVideoProviders(cfg)
	videoService := service.NewVideoService(repos.VideoRepo, repos.StoryboardRepo, repos.ChapterRepo, repos.CharacterRepo, repos.NovelRepo, repos.TenantRepo, aiService, videoProviders)

	// 模型服务（注入 aiService 以支持 TestProvider 实例化验证）
	modelService := service.NewModelService(
		repos.AIModelRepo,
		repos.ModelProviderRepo,
		repos.TaskModelConfigRepo,
		repos.ModelComparisonRepo,
		aiService,
	)

	// 提示词服务
	promptService := service.NewPromptService(nil)

	// 连续性检查服务
	continuityService := service.NewContinuityService(repos.CharacterRepo, repos.ChapterRepo)

	// 知识库服务（传入 AI provider 用于向量化）
	var defaultAIProvider ai.AIProvider
	if aiManager != nil {
		var providerErr error
		defaultAIProvider, providerErr = aiManager.GetProvider("")
		if providerErr != nil {
			log.Printf("Warning: could not load default AI provider: %v — knowledge base embedding will be unavailable", providerErr)
		}
	}
	if defaultAIProvider == nil {
		log.Printf("Warning: no default AI provider available; knowledge base embedding disabled")
	}
	knowledgeService := service.NewKnowledgeService(repos.KnowledgeBaseRepo, vectorStore, defaultAIProvider)

	// 审核任务服务
	reviewTaskService := service.NewReviewTaskService(repos.ReviewTaskRepo)

	// 章节版本服务
	chapterVersionService := service.NewChapterVersionService(repos.ChapterVersionRepo, repos.ChapterRepo)

	// 伏笔服务
	foreshadowService := service.NewForeshadowService(repos.KnowledgeBaseRepo, aiService)

	// 时间线服务
	timelineService := service.NewTimelineService(repos.ChapterRepo)

	// 角色弧光服务
	characterArcService := service.NewCharacterArcService(repos.CharacterRepo, repos.SnapshotRepo)

	// 风格服务
	styleService := service.NewStyleService(nil)

	// 生成上下文服务
	generationContextService := service.NewGenerationContextService(
		repos.NovelRepo,
		repos.ChapterRepo,
		repos.CharacterRepo,
		characterArcService,
		foreshadowService,
	)

	// 章节服务（需要 generationContextService 以构建富上下文 prompt）
	chapterService := service.NewChapterService(repos.ChapterRepo, repos.NovelRepo, aiService, generationContextService)

	// 图像生成服务
	imageGenerationService := service.NewImageGenerationService(aiService)

	// 图像服务（用于视频生成）
	imageService := service.NewImageService(nil)

	// 智能分镜服务（用于小说转视频）
	intelligentStoryboardService := service.NewIntelligentStoryboardService(aiService, imageService)

	// 分镜服务（handler层使用）
	storyboardService := service.NewStoryboardService(videoService, aiService)

	// 视频增强服务（传入临时工作目录）
	videoEnhancementService := service.NewVideoEnhancementService(imageService, "/tmp/inkframe-enhance")

	// BGM 服务（bgmDir 为空时无BGM；可通过 BGM_DIR 环境变量或配置指定本地 BGM 目录）
	bgmService := service.NewBGMService(getEnv("BGM_DIR", ""))

	// 角色一致性服务
	characterConsistencyService := service.NewCharacterConsistencyService(imageService, nil, aiService)
	videoService.WithConsistencyService(characterConsistencyService)
	videoService.WithBGMService(bgmService)

	// 帧生成服务
	frameGeneratorService := service.NewFrameGeneratorService(aiService)

	// 一致性验证服务
	consistencyValidatorService := service.NewConsistencyValidatorService(aiService)

	// 爬虫服务
	crawlerService := crawler.NewNovelCrawler(nil)

	// 导入服务
	novelImportService := service.NewNovelImportService(repos.NovelRepo, repos.ChapterRepo, crawlerService)

	// 小说转视频服务
	novelToVideoService := service.NewNovelToVideoService(
		novelImportService,
		intelligentStoryboardService,
		frameGeneratorService,
		videoEnhancementService,
		consistencyValidatorService,
		repos.NovelRepo,
		repos.ChapterRepo,
		repos.VideoRepo,
		repos.StoryboardRepo,
	)

	// 认证服务
	authService := service.NewAuthService(
		repos.UserRepo,
		repos.TenantRepo,
		repos.TenantUserRepo,
		cfg.Server.JWTSecret,
		cfg.Server.JWTExpiry,
	)

	// 租户服务
	tenantService := service.NewTenantService(repos.TenantRepo, repos.TenantUserRepo)

	// MCP 服务（直接注入 db，轻量无依赖）
	mcpService := service.NewMcpService(db)

	return &Services{
		McpService:                 mcpService,
		NovelService:               novelService,
		ChapterService:             chapterService,
		CharacterService:           characterService,
		WorldviewService:           worldviewService,
		QualityControlService:      qualityControlService,
		VideoService:               videoService,
		ModelService:               modelService,
		PromptService:              promptService,
		ContinuityService:          continuityService,
		KnowledgeService:           knowledgeService,
		ReviewTaskService:          reviewTaskService,
		ChapterVersionService:      chapterVersionService,
		ForeshadowService:         foreshadowService,
		TimelineService:            timelineService,
		CharacterArcService:        characterArcService,
		StyleService:               styleService,
		GenerationContextService:    generationContextService,
		ImageGenerationService:     imageGenerationService,
		StoryboardService:          storyboardService,
		VideoEnhancementService:     videoEnhancementService,
		CharacterConsistencyService: characterConsistencyService,
		FrameGeneratorService:       frameGeneratorService,
		ConsistencyValidatorService: consistencyValidatorService,
		BGMService:                 bgmService,
		CrawlerService:             crawlerService,
		NovelImportService:         novelImportService,
		NovelToVideoService:        novelToVideoService,
		AuthService:                authService,
		TenantService:              tenantService,
	}
}

// Handlers 处理器
type Handlers struct {
	NovelHandler      *handler.NovelHandler
	ChapterHandler    *handler.ChapterHandler
	CharacterHandler  *handler.CharacterHandler
	VideoHandler      *handler.VideoHandler
	ModelHandler      *handler.ModelHandler
	McpHandler        *handler.McpHandler
	StyleHandler      *handler.StyleHandler
	ContextHandler    *handler.ContextHandler
	AuthHandler       *handler.AuthHandler
	ImportHandler     *handler.ImportHandler
	WorldviewHandler  *handler.WorldviewHandler
	TenantHandler     *handler.TenantHandler
}

// initHandlers 初始化处理器
func initHandlers(services *Services) *Handlers {
	return &Handlers{
		NovelHandler:     handler.NewNovelHandler(
			services.NovelService,
			services.ChapterService,
			services.ForeshadowService,
			services.TimelineService,
			services.QualityControlService,
		),
		ChapterHandler: handler.NewChapterHandler(
			services.ChapterService,
			services.ChapterVersionService,
			services.QualityControlService,
		),
		CharacterHandler: handler.NewCharacterHandler(
			services.CharacterService,
			services.CharacterArcService,
			services.ImageGenerationService,
		),
		VideoHandler: handler.NewVideoHandler(
			services.VideoService,
			services.StoryboardService,
			services.VideoEnhancementService,
			services.CharacterConsistencyService,
		),
		ModelHandler: handler.NewModelHandler(services.ModelService),
		McpHandler:   handler.NewMcpHandler(services.McpService),
		StyleHandler: handler.NewStyleHandler(services.StyleService),
		ContextHandler: handler.NewContextHandler(services.GenerationContextService),
		AuthHandler:      handler.NewAuthHandler(services.AuthService),
		ImportHandler:    handler.NewImportHandler(services.NovelImportService, services.NovelToVideoService),
		WorldviewHandler: handler.NewWorldviewHandler(services.WorldviewService),
		TenantHandler:    handler.NewTenantHandler(services.TenantService),
	}
}

// getEnv 获取环境变量
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// seedSystemProviders 将 env/config 中的 AI provider 配置写入 DB（tenant_id=0 系统级）。
// 使用 upsert 语义：仅在 API key 非空时写入，已存在则更新。
func seedSystemProviders(providerRepo *repository.ModelProviderRepository, cfg *config.Config) {
	type providerSeed struct {
		name     string
		endpoint string
		apiKey   string
		model    string
	}
	seeds := []providerSeed{
		{"openai", cfg.AI.OpenAI.Endpoint, cfg.AI.OpenAI.APIKey, cfg.AI.OpenAI.Model},
		{"anthropic", cfg.AI.Anthropic.Endpoint, cfg.AI.Anthropic.APIKey, cfg.AI.Anthropic.Model},
		{"google", cfg.AI.Google.Endpoint, cfg.AI.Google.APIKey, cfg.AI.Google.Model},
		{"doubao", cfg.AI.Doubao.Endpoint, cfg.AI.Doubao.APIKey, cfg.AI.Doubao.Model},
		{"deepseek", cfg.AI.DeepSeek.Endpoint, cfg.AI.DeepSeek.APIKey, cfg.AI.DeepSeek.Model},
		{"qianwen", cfg.AI.Qianwen.Endpoint, cfg.AI.Qianwen.APIKey, cfg.AI.Qianwen.Model},
	}
	for _, s := range seeds {
		if s.apiKey == "" {
			continue
		}
		existing, _ := providerRepo.GetSystemProvider(s.name)
		if existing != nil {
			existing.APIEndpoint = s.endpoint
			existing.APIKey = s.apiKey
			existing.APIVersion = s.model
			existing.IsActive = true
			if err := providerRepo.Update(existing); err != nil {
				log.Printf("seedSystemProviders: update %s failed: %v", s.name, err)
			}
		} else {
			p := &model.ModelProvider{
				TenantID:    0,
				Name:        s.name,
				DisplayName: s.name,
				Type:        "llm",
				APIEndpoint: s.endpoint,
				APIKey:      s.apiKey,
				APIVersion:  s.model,
				IsActive:    true,
			}
			if err := providerRepo.Create(p); err != nil {
				log.Printf("seedSystemProviders: create %s failed: %v", s.name, err)
			}
		}
	}
}
