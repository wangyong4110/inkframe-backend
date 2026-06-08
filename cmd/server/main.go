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
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/router"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// startRecalcLoop runs fn every hour in a background goroutine; panics are recovered.
// The goroutine exits when quit is closed.
func startRecalcLoop(tag string, quit <-chan struct{}, fn func() error) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[%s] goroutine panic: %v", tag, r)
			}
		}()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := fn(); err != nil {
					logger.Errorf("[%s] recalc error: %v", tag, err)
				}
			case <-quit:
				return
			}
		}
	}()
}

func main() {
	// 初始化 zap logger（开发模式：彩色可读格式）
	logger.Init(true)
	defer logger.Sync()

	// 1. 加载配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Printf("Config file not found, using defaults")
		cfg = config.DefaultConfig()
	}

	// 1a. 安全校验：JWT secret 必须至少 32 字符
	if len(cfg.Server.JWTSecret) < 32 {
		log.Fatalf("FATAL: jwt_secret must be at least 32 characters (currently %d). Set a strong secret in config.yaml.", len(cfg.Server.JWTSecret))
	}

	// 2. 初始化数据库
	db, err := initDatabase(cfg)
	if err != nil {
		logger.Fatalf("Failed to init database: %v", err)
	}

	// 3. 自动迁移（GORM AutoMigrate 只增列不删列，开发环境安全运行）
	// 注意：列重命名需先执行 migrations/001_fix_model_provider_columns.sql
	if err := autoMigrate(db); err != nil {
		logger.Fatalf("Failed to migrate database: %v", err)
	}

	// 3a. 执行幂等 schema 清理（回填租户ID、迁移视频配置、删除废弃列）
	runSchemaCleanup(db)

	// 3b. 预置默认数据（INSERT IGNORE，幂等安全）
	seedDefaultData(db)
	seedAIModels(db)
	seedWebSearchMcpTool(db, cfg)
	seedWikiSearchMcpTool(db, cfg)
	seedStoryPatternMcpTool(db, cfg)
	seedImageRefSearchMcpTool(db, cfg)
	seedColorPaletteMcpTool(db, cfg)

	// 4. 初始化Redis
	redisClient := initRedis(cfg)

	// 5. 初始化AI模块（含图像生成提供者注册）
	aiManager := initAIModule(cfg)

	// 6. 初始化向量存储
	vectorStore := initVectorStore(cfg)

	// 7. 初始化仓库层
	repos := initRepositories(db, redisClient)

	// 8. 初始化服务层
	services := initServices(db, repos, aiManager, vectorStore, cfg, redisClient)

	// 9. 初始化默认测试账户（仅开发模式）
	if cfg.Server.Mode != "release" {
		seedDefaultUser(services)
	}

	// 10. 初始化存储服务（OSS 优先；未配置 OSS 时存 DB；均无时退回本地文件）
	storageSvc := storage.New(storage.Config{
		Type:      cfg.Storage.Type,
		Endpoint:  cfg.Storage.Endpoint,
		AccessKey: getEnv("OSS_ACCESS_KEY", cfg.Storage.AccessKey),
		SecretKey: getEnv("OSS_SECRET_KEY", cfg.Storage.SecretKey),
		Bucket:    cfg.Storage.Bucket,
		BaseURL:   cfg.Storage.BaseURL,
		LocalDir:  "./uploads",
		LocalBase: "/uploads",
	}, db)
	logger.Printf("Storage: type=%s", cfg.Storage.Type)

	// 注入 MCP 服务到章节生成（用于联网搜索）
	services.ChapterService.WithMcpService(services.McpService)

	// 注入存储服务
	services.VideoService.WithStorage(storageSvc)
	services.AIService.WithStorage(storageSvc)
	{
		serverHost := cfg.Server.Host
		if serverHost == "" || serverHost == "0.0.0.0" {
			serverHost = "127.0.0.1"
		}
		services.AIService.WithServerBaseURL(fmt.Sprintf("http://%s:%d", serverHost, cfg.Server.Port))
	}
	services.BGMService.WithStorage(storageSvc)
	services.AssetService.WithStorage(storageSvc)
	services.VideoService.WithSceneAnchorService(services.SceneAnchorService)
	services.VideoService.WithSegmentRepo(repos.ShotVoiceSegmentRepo).WithTaskService(services.TaskService)
	services.VideoService.WithReviewRecordRepo(repos.ReviewRecordRepo)
	services.VideoService.WithIgnoredIssueRepo(repos.IgnoredReviewIssueRepo)
	services.VideoService.WithVideoSocial(repos.VideoLikeRepo, repos.VideoCommentRepo)
	services.NovelService.WithNovelSocial(repos.NovelLikeRepo, repos.NovelCommentRepo)
	services.NovelImportService.WithDB(db).WithStorage(storageSvc).WithAnalysisService(services.NovelAnalysisService).WithAIService(services.AIService).WithNotificationService(services.NotificationService).WithCrawlJobRepo(repos.NovelCrawlJobRepo)

	// SFX 音效服务（降级链：素材库 → AI文生音效 → 本地库 → AudioLDM → Freesound → Pixabay → BBC → 爱给网 → ElevenLabs）
	// 所有 API Key 均通过"模型管理"页面配置，不再从环境变量读取。
	crawlProxyURL := getEnv("CRAWL_PROXY_URL", cfg.Crawl.ProxyURL)
	services.AssetService.WithCrawlProxy(crawlProxyURL)
	services.AssetService.WithUnsplashKey(getEnv("UNSPLASH_ACCESS_KEY", cfg.Crawl.UnsplashKey))
	services.AssetService.WithFreesoundKey(getEnv("FREESOUND_API_KEY", ""))
	services.AssetService.WithPixabayKey(getEnv("PIXABAY_API_KEY", ""))
	services.AssetService.WithPexelsKey(getEnv("PEXELS_API_KEY", cfg.Crawl.PexelsKey))
	services.AssetService.RecoverOrphanedCrawlJobs()

	sfxService := service.NewSFXService(services.AIService, storageSvc, repos.StoryboardRepo, service.SFXServiceConfig{
		SFXDir:   getEnv("SFX_DIR", ""),
		ProxyURL: crawlProxyURL,
	})
	sfxService.WithSFXItemRepo(repos.ShotSFXItemRepo)
	sfxService.WithAssetRepo(repos.AssetRepo, repos.TagRepo)
	services.SFXService = sfxService
	services.VideoService.WithSFXService(sfxService)

	// 11. 初始化处理器
	handlers := initHandlers(services, storageSvc, db, repos, cfg)

	// 11b. 设置Gin模式（必须在 SetupRouter 之前）
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 12. 设置路由
	r := router.SetupRouter(&router.Config{
		JWTSecret:          cfg.Server.JWTSecret,
		AllowedOrigins:     cfg.Server.AllowedOrigins,
		TrustedProxies:     cfg.Server.TrustedProxies,
		RedisClient:        redisClient,
		DB:                 db,
		AIService:          services.AIService,
		NovelHandler:       handlers.NovelHandler,
		ChapterHandler:     handlers.ChapterHandler,
		CharacterHandler:   handlers.CharacterHandler,
		VideoHandler:       handlers.VideoHandler,
		ModelHandler:       handlers.ModelHandler,
		McpHandler:         handlers.McpHandler,
		StyleHandler:       handlers.StyleHandler,
		ContextHandler:     handlers.ContextHandler,
		AuthHandler:        handlers.AuthHandler,
		ImportHandler:      handlers.ImportHandler,
		WorldviewHandler:   handlers.WorldviewHandler,
		TenantHandler:      handlers.TenantHandler,
		ItemHandler:        handlers.ItemHandler,
		SkillHandler:       handlers.SkillHandler,
		UploadHandler:      handlers.UploadHandler,
		PlotPointHandler:   handlers.PlotPointHandler,
		TaskHandler:        handlers.TaskHandler,
		MediaHandler:       handlers.MediaHandler,
		SceneAnchorHandler: handlers.SceneAnchorHandler,
		SystemHandler:      handlers.SystemHandler,
		FsHandler:          handlers.FsHandler,
		RewriteHandler:     handlers.RewriteHandler,
		PlatformHandler:    handlers.PlatformHandler,
		AssetHandler:       handlers.AssetHandler,
		ImageHandler:       handlers.ImageHandler,
		WebSearchHandler:      handlers.WebSearchHandler,
		WikiSearchHandler:     handlers.WikiSearchHandler,
		StoryPatternHandler:   handlers.StoryPatternHandler,
		ImageRefSearchHandler: handlers.ImageRefSearchHandler,
		ColorPaletteHandler:   handlers.ColorPaletteHandler,
		NotificationHandler:   handlers.NotificationHandler,
		KnowledgeHandler:      handlers.KnowledgeHandler,
		DramaticHandler:       handlers.DramaticHandler,
		DashboardHandler:      handlers.DashboardHandler,
		ForeshadowHandler:     handlers.ForeshadowHandler,
		WebhookHandler:        handlers.WebhookHandler,
		AuditHandler:          handlers.AuditHandler,
		OutlineReviewHandler:  handlers.OutlineReviewHandler,
	})

	// 12. 创建服务器
	srv := &http.Server{
		Addr:           cfg.Server.GetAddr(),
		Handler:        r,
		ReadTimeout:    cfg.Server.ReadTimeout,
		WriteTimeout:   cfg.Server.WriteTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
	}

	// 13. 创建服务器根 context（在此后所有后台任务使用，shutdown 时统一取消）
	serverRootCtx, serverRootCancel := context.WithCancel(context.Background())
	defer serverRootCancel()

	// 13b. 启动服务器
	safeGo("http-server", func() {
		logger.Printf("Server starting on %s", cfg.Server.GetAddr())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Server failed to start: %v", err)
		}
	})

	// 后台定时任务：每小时重新计算热度分（带优雅退出）
	hotScoreQuit := make(chan struct{})
	startRecalcLoop("hot-score", hotScoreQuit, services.VideoService.RecalcVideoHotScores)
	startRecalcLoop("novel-hot-score", hotScoreQuit, services.NovelService.RecalcNovelHotScores)

	// 后台定时任务：每 30 分钟清理超时的分片上传会话（防内存泄漏）
	safeGo("chunk-cleanup", handler.CleanupChunkStore)

	// 后台定时任务：每 24 小时清理 7 天前的章节历史版本
	startRecalcLoop("chapter-version-cleanup", hotScoreQuit, func() error {
		cutoff := time.Now().AddDate(0, 0, -7)
		n, err := repos.ChapterVersionRepo.DeleteOlderThan(cutoff)
		if err != nil {
			return err
		}
		if n > 0 {
			logger.Printf("[chapter-version-cleanup] deleted %d versions older than %s", n, cutoff.Format("2006-01-02"))
		}
		return nil
	})

	// 注册可续跑任务类型，然后启动任务恢复（必须在所有服务 wiring 完成后调用）
	// Pass serverRootCtx so resumed goroutines can be cancelled on graceful shutdown.
	if services.TaskService != nil {
		registerTaskResumeHandlers(services, repos)
		services.TaskService.Boot(serverRootCtx)
	}

	// 启动时重置所有卡在 "rewriting" 状态超过 30 分钟的章节改写任务，
	// 以恢复因服务崩溃或意外重启而中断的改写管道。
	if services.RewriteService != nil {
		services.RewriteService.ResetStaleChapters()
	}

	// 恢复服务重启前仍处于 "generating" 状态的视频轮询任务（非阻塞）。
	go services.VideoService.RecoverActivePollTasks()

	// 14. 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Println("Shutting down server...")

	// 15. 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	// 16. 关闭后台服务 goroutines
	// Cancel server root context first so all resumed task goroutines stop promptly.
	serverRootCancel()
	close(hotScoreQuit)
	services.VideoService.Shutdown()
	services.NovelService.Shutdown()
	if services.NovelAnalysisService != nil {
		services.NovelAnalysisService.Shutdown()
	}
	if services.AIService != nil {
		services.AIService.Shutdown()
	}
	if services.TaskService != nil {
		services.TaskService.Shutdown()
	}

	// 17. 关闭数据库连接
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close()
	}

	// 18. 关闭Redis连接
	if redisClient != nil {
		redisClient.Close()
	}

	logger.Println("Server exited")
}

// initDatabase 初始化数据库
func initDatabase(cfg *config.Config) (*gorm.DB, error) {
	dsn := cfg.Database.GetDSN()

	gormLogger := gormlogger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  gormlogger.Warn, // Warn: 仅打印慢查询和错误，不打印普通 SELECT
			IgnoreRecordNotFoundError: true,
			Colorful:                  true,
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
	sqlDB.SetConnMaxIdleTime(15 * time.Minute)

	return db, nil
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
		if cfg.Redis.GetRedisAddr() != "" {
			logger.Errorf("WARNING: Redis connection failed: %v. JWT blacklist, caching, and rate limiting will be degraded.", err)
		}
		// Return nil so dependent code uses fallbacks gracefully
		return nil
	}

	logger.Println("Redis connected successfully")
	return client
}

// getEnv 获取环境变量
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// safeGo 启动后台 goroutine，panic 时仅记录日志而不让整个进程崩溃。
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("PANIC in goroutine %s: %v", name, r)
			}
		}()
		fn()
	}()
}
