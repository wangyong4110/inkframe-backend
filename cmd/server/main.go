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
	"github.com/inkframe/inkframe-backend/internal/api"
	"github.com/inkframe/inkframe-backend/internal/api/handler"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	// 1. 加载配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 2. 初始化数据库
	db, err := initDatabase(cfg)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}

	// 3. 初始化Redis
	redisClient := initRedis(cfg)

	// 4. 初始化仓库层
	repos := initRepositories(db, redisClient)

	// 5. 初始化服务层
	services := initServices(repos)

	// 6. 初始化处理器
	handlers := initHandlers(services)

	// 7. 设置路由
	router := api.SetupRouter(
		handlers.novelHandler,
		handlers.chapterHandler,
		handlers.modelHandler,
		handlers.videoHandler,
		handlers.healthHandler,
	)

	// 8. 设置Gin模式
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 9. 创建服务器
	srv := &http.Server{
		Addr:           cfg.Server.GetAddr(),
		Handler:        router,
		ReadTimeout:    cfg.Server.ReadTimeout,
		WriteTimeout:   cfg.Server.WriteTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
	}

	// 10. 启动服务器
	go func() {
		log.Printf("Server starting on %s", cfg.Server.GetAddr())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	// 11. 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// 12. 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	// 13. 关闭数据库连接
	sqlDB, _ := db.DB()
	if sqlDB != nil {
		sqlDB.Close()
	}

	// 14. 关闭Redis连接
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

	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

	return db, nil
}

// initRedis 初始化Redis
func initRedis(cfg *config.Config) *redis.Client {
	if cfg.Redis.Host == "" {
		return nil
	}

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

// Repositories 仓库层
type Repositories struct {
	NovelRepo         *repository.NovelRepository
	ChapterRepo      *repository.ChapterRepository
	CharacterRepo    *repository.CharacterRepository
	WorldviewRepo    *repository.WorldviewRepository
	AIModelRepo      *repository.AIModelRepository
	TaskModelConfigRepo *repository.TaskModelConfigRepository
	VideoRepo        *repository.VideoRepository
	StoryboardRepo   *repository.StoryboardRepository
	KnowledgeBaseRepo *repository.KnowledgeBaseRepository
}

// initRepositories 初始化仓库层
func initRepositories(db *gorm.DB, redis *redis.Client) *Repositories {
	return &Repositories{
		NovelRepo:         repository.NewNovelRepository(db, redis),
		ChapterRepo:      repository.NewChapterRepository(db, redis),
		CharacterRepo:    repository.NewCharacterRepository(db),
		WorldviewRepo:    repository.NewWorldviewRepository(db),
		AIModelRepo:      repository.NewAIModelRepository(db),
		TaskModelConfigRepo: repository.NewTaskModelConfigRepository(db),
		VideoRepo:        repository.NewVideoRepository(db),
		StoryboardRepo:   repository.NewStoryboardRepository(db),
		KnowledgeBaseRepo: repository.NewKnowledgeBaseRepository(db),
	}
}

// Services 服务层
type Services struct {
	NovelService    *service.NovelService
	QualityService  *service.QualityService
	VideoService    *service.VideoService
	ModelService    *service.ModelService
}

// initServices 初始化服务层
func initServices(repos *Repositories) *Services {
	// AI服务
	aiService := service.NewAIService(repos.AIModelRepo, repos.TaskModelConfigRepo)

	// 小说服务
	novelService := service.NewNovelService(repos.NovelRepo, repos.ChapterRepo, aiService)

	// 质量服务
	qualityService := service.NewQualityService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, aiService)

	// 视频服务
	videoService := service.NewVideoService(repos.VideoRepo, repos.StoryboardRepo, repos.ChapterRepo, aiService)

	// 模型服务
	modelService := service.NewModelService(
		repos.AIModelRepo,
		&struct {
			*repository.ModelProviderRepository
		}{repository.NewModelProviderRepository(nil)},
		repos.TaskModelConfigRepo,
		&struct {
			*repository.ModelComparisonRepository
		}{repository.NewModelComparisonRepository(nil)},
	)

	return &Services{
		NovelService:   novelService,
		QualityService: qualityService,
		VideoService:   videoService,
		ModelService:   modelService,
	}
}

// Handlers 处理器
type Handlers struct {
	novelHandler   *handler.NovelHandler
	chapterHandler *handler.ChapterHandler
	modelHandler  *handler.ModelHandler
	videoHandler  *handler.VideoHandler
	healthHandler *handler.HealthHandler
}

// initHandlers 初始化处理器
func initHandlers(services *Services) *Handlers {
	return &Handlers{
		novelHandler:   handler.NewNovelHandler(services.NovelService),
		chapterHandler: handler.NewChapterHandler(services.NovelService),
		modelHandler:  handler.NewModelHandler(services.ModelService),
		videoHandler:  handler.NewVideoHandler(services.VideoService),
		healthHandler: handler.NewHealthHandler(),
	}
}
