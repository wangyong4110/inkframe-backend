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
	"github.com/inkframe/inkframe-backend/internal/metrics"
	"github.com/inkframe/inkframe-backend/internal/router"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// startRecalcLoop runs fn every hour in a background goroutine; panics are recovered.
// If rdb is non-nil, a Redis SETNX lock (TTL 55min) ensures only one instance executes
// per cycle — preventing duplicate DB writes in multi-instance deployments.
// The goroutine exits when quit is closed.
func startRecalcLoop(tag string, quit <-chan struct{}, rdb *redis.Client, fn func() error) {
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
				if rdb != nil {
					lockKey := "recalc:lock:" + tag
					ok, err := rdb.SetNX(context.Background(), lockKey, "1", 55*time.Minute).Result()
					if err != nil || !ok {
						continue // another instance is already running this cycle
					}
				}
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
	// 启动前临时使用 debug logger，配置加载后按 server.mode 重新初始化
	logger.Init(true)
	defer logger.Sync()

	// 1. 加载配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		logger.Printf("Config file not found, using defaults")
		cfg = config.DefaultConfig()
	}

	// 按 server.mode 重新初始化 logger：debug → 彩色+DEBUG级别；其他 → 纯文本+INFO级别
	logger.Init(cfg.Server.Mode == "debug")
	defer logger.Sync()

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

	// 3b. 预置默认数据（INSERT IGNORE，幂等安全）
	seedDefaultData(db)
	seedAIModels(db)
	initSystemAdmin(db, cfg)
	seedWebSearchMcpTool(db, cfg)
	seedWikiSearchMcpTool(db, cfg)
	seedStoryPatternMcpTool(db, cfg)
	seedImageRefSearchMcpTool(db, cfg)
	seedColorPaletteMcpTool(db, cfg)
	seedKnowledgeSearchMcpTool(db, cfg)
	seedCharacterLookupMcpTool(db, cfg)
	seedPromptEnhanceMcpTool(db, cfg)

	// 4. 初始化Redis
	redisClient := initRedis(cfg)

	// 4a. 注册 DB/Redis 连接池 Prometheus Collector（每次 Scrape 读取实时连接池数据）
	if sqlDB, err2 := db.DB(); err2 == nil {
		prometheus.MustRegister(metrics.NewDBStatsCollector(sqlDB))
	}
	if redisClient != nil {
		prometheus.MustRegister(metrics.NewRedisStatsCollector(redisClient))
	}

	// 5. 初始化AI模块（含图像生成提供者注册）
	aiManager := initAIModule(cfg)

	// 6. 初始化向量存储
	vectorStore := initVectorStore(cfg)

	// 7. 初始化仓库层
	repos := initRepositories(db, redisClient)

	// 7b. 预置内置短剧模板（幂等，按名称 upsert）
	service.SeedBuiltinTemplates(repos.DramaTemplateRepo)

	// 8. 初始化服务层
	services := initServices(db, repos, aiManager, vectorStore, cfg, redisClient)

	// SeedAllProviders 已在 initServices/wiring.go 中调用，此处不重复执行

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
	// dbMediaReader 始终基于 DB，用于读取历史 /api/v1/media/* 资产并上传到 OSS
	// 当 storageSvc 为 OSS 时，OSS.Get() 无法处理相对路径，需要单独的 DB reader
	dbMediaReader := storage.New(storage.Config{}, db)
	services.VideoService.WithDBMediaReader(dbMediaReader)
	services.AIService.WithStorage(storageSvc)
	services.AIService.WithDBMediaReader(dbMediaReader)
	if cfg.Server.PublicURL != "" {
		services.AIService.WithServerBaseURL(cfg.Server.PublicURL)
		services.VideoService.WithBackendBaseURL(cfg.Server.PublicURL)
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

	sfxService := service.NewSFXService(services.AIService, storageSvc, repos.StoryboardRepo, service.SFXServiceConfig{
		SFXDir:   getEnv("SFX_DIR", ""),
		ProxyURL: crawlProxyURL,
	})
	sfxService.WithSFXItemRepo(repos.ShotSFXItemRepo)
	sfxService.WithAssetRepo(repos.AssetRepo, repos.TagRepo)
	sfxService.WithRedis(redisClient) // Fix: cross-instance SFX query cache sharing
	services.SFXService = sfxService
	services.VideoService.WithSFXService(sfxService)

	// 11. 初始化处理器
	handlers := initHandlers(services, storageSvc, db, repos, cfg, redisClient)

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
		KnowledgeHandler:       handlers.KnowledgeHandler,
		KnowledgeToolHandler:   handlers.KnowledgeToolHandler,
		CharacterLookupHandler: handlers.CharacterLookupHandler,
		PromptEnhanceHandler:   handlers.PromptEnhanceHandler,
		DramaticHandler:       handlers.DramaticHandler,
		DashboardHandler:      handlers.DashboardHandler,
		ForeshadowHandler:     handlers.ForeshadowHandler,
		WebhookHandler:        handlers.WebhookHandler,
		AuditHandler:          handlers.AuditHandler,
		OutlineReviewHandler:  handlers.OutlineReviewHandler,
		CollabHandler:         handlers.CollabHandler,
		SysAdminHandler:       handlers.SysAdminHandler,
		SensitiveWordHandler:  handlers.SensitiveWordHandler,
		FeedbackHandler:       handlers.FeedbackHandler,
		DramaTemplateHandler:  handlers.DramaTemplateHandler,
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
	startRecalcLoop("hot-score", hotScoreQuit, redisClient, services.VideoService.RecalcVideoHotScores)
	startRecalcLoop("novel-hot-score", hotScoreQuit, redisClient, services.NovelService.RecalcNovelHotScores)

	// 后台定时任务：每 30 分钟清理超时的分片上传会话（防内存泄漏）
	safeGo("chunk-cleanup", handler.CleanupChunkStore)

	// 后台定时任务：每 24 小时清理 7 天前的章节历史版本
	startRecalcLoop("chapter-version-cleanup", hotScoreQuit, redisClient, func() error {
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

	// 后台定时任务：每月1日重置租户爬取配额（带 Redis 月度锁，多实例只执行一次）
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Now().Day() != 1 {
					continue
				}
				if redisClient != nil {
					monthKey := "crawl-quota-reset:" + time.Now().Format("2006-01")
					ok, _ := redisClient.SetNX(context.Background(), monthKey, "1", 35*24*time.Hour).Result()
					if !ok {
						continue // 本月已被其他实例重置
					}
				}
				if err := repos.TenantQuotaRepo.ResetMonthlyCrawl(); err != nil {
					logger.Errorf("[crawl-quota-monthly-reset] %v", err)
				} else {
					logger.Printf("[crawl-quota-monthly-reset] reset crawl_used_this_month for all tenants")
				}
			case <-hotScoreQuit:
				return
			}
		}
	}()

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

	// 恢复因服务崩溃或重启而卡在 "uploading" 状态超过 30 分钟的外部平台发布记录。
	// 多实例环境下通过 Redis SETNX 保证每条记录只被一个实例处理。
	go services.PlatformPublishService.RecoverStalePublishRecords(serverRootCtx)

	// 恢复因服务崩溃或重启而中断的小说爬取任务（status="running" 的孤儿任务）。
	go services.NovelImportService.RecoverStaleCrawlJobs(serverRootCtx)

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

	// Prometheus GORM 回调：记录每次 DB 操作的耗时和次数。
	// 使用 Statement.Table 作为表名标签（GORM 解析后可用）。
	registerGORMMetrics(db)

	return db, nil
}

// registerGORMMetrics 注册 GORM 回调，采集 DB 查询指标到 Prometheus。
// 使用 InstanceSet/InstanceGet（不产生 clone）在同一 Statement 的 before/after 回调间传递计时起点。
func registerGORMMetrics(db *gorm.DB) {
	const startKey = "metrics:start"

	// before 回调：记录开始时间到 Statement 的 InstanceSettings（不 clone，跨回调可见）
	beforeFn := func(db *gorm.DB) { db.InstanceSet(startKey, time.Now()) }

	afterFn := func(sqlOp string) func(*gorm.DB) {
		return func(db *gorm.DB) {
			v, ok := db.InstanceGet(startKey)
			if !ok {
				return
			}
			elapsed := time.Since(v.(time.Time)).Seconds()
			table := db.Statement.Table
			if table == "" {
				table = "unknown"
			}
			errLabel := "false"
			if db.Error != nil {
				errLabel = "true"
			}
			metrics.DBQueriesTotal.WithLabelValues(table, sqlOp, errLabel).Inc()
			metrics.DBQueryDuration.WithLabelValues(table, sqlOp).Observe(elapsed)
		}
	}

	db.Callback().Create().Before("gorm:create").Register("metrics:before_create", beforeFn)
	db.Callback().Create().After("gorm:after_create").Register("metrics:after_create", afterFn("INSERT"))
	db.Callback().Query().Before("gorm:query").Register("metrics:before_query", beforeFn)
	db.Callback().Query().After("gorm:after_query").Register("metrics:after_query", afterFn("SELECT"))
	db.Callback().Update().Before("gorm:update").Register("metrics:before_update", beforeFn)
	db.Callback().Update().After("gorm:after_update").Register("metrics:after_update", afterFn("UPDATE"))
	db.Callback().Delete().Before("gorm:delete").Register("metrics:before_delete", beforeFn)
	db.Callback().Delete().After("gorm:after_delete").Register("metrics:after_delete", afterFn("DELETE"))
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
