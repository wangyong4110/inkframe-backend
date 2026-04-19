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

	// 8. 初始化服务层
	services := initServices(repos, aiManager, vectorStore)

	// 9. 初始化处理器
	handlers := initHandlers(services)

	// 初始化租户处理器
	tenantHandler := handler.NewTenantHandler(services.TenantService)

	// 10. 设置路由
	r := router.SetupRouter(&router.Config{
		NovelHandler:      handlers.NovelHandler,
		ChapterHandler:   handlers.ChapterHandler,
		CharacterHandler: handlers.CharacterHandler,
		VideoHandler:    handlers.VideoHandler,
		ModelHandler:    handlers.ModelHandler,
		StyleHandler:    handlers.StyleHandler,
		ContextHandler:  handlers.ContextHandler,
		TenantHandler:   tenantHandler,
	})

	// 11. 设置Gin模式
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 12. 创建服务器
	srv := &http.Server{
		Addr:           ":8080",
		Handler:        r,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	// 13. 启动服务器
	go func() {
		log.Printf("Server starting on :8080")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	)
}

// initRedis 初始化Redis
func initRedis(cfg *config.Config) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
		PoolSize: 100,
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

	// 设置默认
	manager.SetDefault("openai")

	return manager
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
	}
}

// Services 服务层
type Services struct {
	TenantService               *service.TenantService
	NovelService               *service.NovelService
	ChapterService             *service.ChapterService
	CharacterService           *service.CharacterService
	WorldviewService           *service.WorldviewService
	QualityService             *service.QualityControlService
	QualityControlService      *service.QualityControlService
	VideoService               *service.VideoService
	ModelService               *service.ModelService
	PromptService              *service.PromptService
	ContinuityService         *service.ContinuityService
	KnowledgeService           *service.KnowledgeService
	ReviewTaskService         *service.ReviewTaskService
	ChapterVersionService      *service.ChapterVersionService
	ForeshadowService         *service.ForeshadowService
	TimelineService            *service.TimelineService
	CharacterArcService       *service.CharacterArcService
	StyleService              *service.StyleService
	GenerationContextService   *service.GenerationContextService
	ImageGenerationService     *service.ImageGenerationService
	StoryboardService          *service.IntelligentStoryboardService
	VideoEnhancementService    *service.VideoEnhancementService
	FrameGeneratorService      *service.FrameGeneratorService
	ConsistencyValidatorService *service.ConsistencyValidatorService
	CrawlerService             *crawler.NovelCrawler
	ImportService             *service.NovelImportService
	NovelToVideoService        *service.NovelToVideoService
}

// initServices 初始化服务层
func initServices(repos *Repositories, aiManager *ai.ModelManager, vectorStore *vector.StoreManager) *Services {
	// 租户服务
	tenantService := service.NewTenantService()

	// AI服务
	aiService := service.NewAIService(repos.AIModelRepo, repos.TaskModelConfigRepo)

	// 小说服务
	novelService := service.NewNovelService(repos.NovelRepo, repos.ChapterRepo, aiService)

	// 章节服务
	chapterService := service.NewChapterService(repos.ChapterRepo, aiService)

	// 角色服务
	characterService := service.NewCharacterService(repos.CharacterRepo, aiService)

	// 世界观服务
	worldviewService := service.NewWorldviewService(repos.WorldviewRepo, aiService)

	// 质量控制服务
	qualityService := service.NewQualityControlService(aiManager)

	// 视频服务
	videoService := service.NewVideoService(repos.VideoRepo, repos.StoryboardRepo, repos.ChapterRepo, aiService)

	// 模型服务
	modelService := service.NewModelService(
		repos.AIModelRepo,
		repos.ModelProviderRepo,
		repos.TaskModelConfigRepo,
		repos.ModelComparisonRepo,
	)

	// 提示词服务
	promptService := service.NewPromptService(nil)

	// 连续性检查服务
	continuityService := service.NewContinuityService(repos.CharacterRepo, repos.ChapterRepo)

	// 知识库服务
	knowledgeService := service.NewKnowledgeService(repos.KnowledgeBaseRepo, vectorStore)

	// 审核任务服务
	reviewTaskService := service.NewReviewTaskService(repos.ReviewTaskRepo)

	// 章节版本服务
	chapterVersionService := service.NewChapterVersionService(repos.KnowledgeBaseRepo, repos.ChapterRepo)

	// 伏笔服务
	foreshadowService := service.NewForeshadowService()

	// 时间线服务
	timelineService := service.NewTimelineService()

	// 角色弧光服务
	characterArcService := service.NewCharacterArcService()

	// 风格服务
	styleService := service.NewStyleService()

	// 生成上下文服务
	generationContextService := service.NewGenerationContextService(
		novelService,
		chapterService,
		foreshadowService,
		timelineService,
		characterArcService,
	)

	// 图像生成服务
	imageGenerationService := service.NewImageGenerationService(aiManager)

	// 分镜服务
	storyboardService := service.NewIntelligentStoryboardService(aiService)

	// 视频增强服务
	videoEnhancementService := service.NewVideoEnhancementService(aiService)

	// 帧生成服务
	frameGeneratorService := service.NewFrameGeneratorService(aiService)

	// 一致性验证服务
	consistencyValidatorService := service.NewConsistencyValidatorService(aiService)

	// 质量控制服务（详细版）
	qualityControlService := service.NewQualityControlService(aiService)

	// 爬虫服务
	crawlerService := crawler.NewNovelCrawler(nil)

	// 导入服务
	importService := service.NewNovelImportService(
		repos.NovelRepo,
		repos.ChapterRepo,
		crawlerService,
	)

	// 小说转视频服务
	novelToVideoService := service.NewNovelToVideoService(
		importService,
		storyboardService,
		frameGeneratorService,
		videoEnhancementService,
		consistencyValidatorService,
		repos.NovelRepo,
		repos.ChapterRepo,
		repos.VideoRepo,
	)

	return &Services{
		TenantService:               tenantService,
		NovelService:               novelService,
		ChapterService:             chapterService,
		CharacterService:           characterService,
		WorldviewService:           worldviewService,
		QualityService:             qualityService,
		QualityControlService:      qualityControlService,
		VideoService:              videoService,
		ModelService:              modelService,
		PromptService:             promptService,
		ContinuityService:         continuityService,
		KnowledgeService:          knowledgeService,
		ReviewTaskService:         reviewTaskService,
		ChapterVersionService:     chapterVersionService,
		ForeshadowService:        foreshadowService,
		TimelineService:           timelineService,
		CharacterArcService:      characterArcService,
		StyleService:              styleService,
		GenerationContextService:  generationContextService,
		ImageGenerationService:    imageGenerationService,
		StoryboardService:         storyboardService,
		VideoEnhancementService:  videoEnhancementService,
		FrameGeneratorService:    frameGeneratorService,
		ConsistencyValidatorService: consistencyValidatorService,
		CrawlerService:            crawlerService,
		ImportService:            importService,
		NovelToVideoService:      novelToVideoService,
	}
}

// Handlers 处理器
type Handlers struct {
	NovelHandler       *handler.NovelHandler
	ChapterHandler     *handler.ChapterHandler
	CharacterHandler   *handler.CharacterHandler
	VideoHandler       *handler.VideoHandler
	ModelHandler       *handler.ModelHandler
	StyleHandler       *handler.StyleHandler
	ContextHandler     *handler.ContextHandler
	ImportHandler      *handler.ImportHandler
}

// initHandlers 初始化处理器
func initHandlers(services *Services) *Handlers {
	return &Handlers{
		NovelHandler:     handler.NewNovelHandler(
			services.NovelService,
			services.ChapterService,
			services.ForeshadowService,
			services.TimelineService,
		),
		ChapterHandler: handler.NewChapterHandler(
			services.ChapterService,
			services.ChapterVersionService,
			services.QualityService,
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
		),
		ModelHandler: handler.NewModelHandler(services.ModelService),
		StyleHandler: handler.NewStyleHandler(services.StyleService),
		ContextHandler: handler.NewContextHandler(services.GenerationContextService),
		ImportHandler: handler.NewImportHandler(services.ImportService, services.NovelToVideoService),
	}
}

// getEnv 获取环境变量
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
