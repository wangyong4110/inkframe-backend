package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/ai"
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/crawler"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/inkframe/inkframe-backend/internal/router"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/inkframe/inkframe-backend/internal/storage"
	"github.com/inkframe/inkframe-backend/internal/vector"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

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

	// 注入存储服务
	services.VideoService.WithStorage(storageSvc)
	services.AIService.WithStorage(storageSvc)
	services.BGMService.WithStorage(storageSvc)
	services.AssetService.WithStorage(storageSvc)
	services.VideoService.WithSceneAnchorService(services.SceneAnchorService)
	services.VideoService.WithSegmentRepo(repos.ShotVoiceSegmentRepo).WithTaskService(services.TaskService)
	services.VideoService.WithVideoSocial(repos.VideoLikeRepo, repos.VideoCommentRepo)
	services.NovelService.WithNovelSocial(repos.NovelLikeRepo, repos.NovelCommentRepo)
	services.NovelImportService.WithStorage(storageSvc).WithAnalysisService(services.NovelAnalysisService).WithAIService(services.AIService)

	// SFX 音效服务（五层降级：本地库 → Freesound → Pixabay → Jamendo → ElevenLabs）
	sfxService := service.NewSFXService(services.AIService, storageSvc, repos.StoryboardRepo, service.SFXServiceConfig{
		SFXDir:          getEnv("SFX_DIR", cfg.SFX.Dir),
		FreesoundKey:    getEnv("FREESOUND_API_KEY", cfg.SFX.FreesoundKey),
		PixabayKey:      getEnv("PIXABAY_API_KEY", cfg.SFX.PixabayKey),
		JamendoClientID: getEnv("JAMENDO_CLIENT_ID", cfg.SFX.JamendoClientID),
		ElevenLabsKey:   getEnv("ELEVENLABS_API_KEY", cfg.SFX.ElevenLabsKey),
	})
	sfxService.WithSFXItemRepo(repos.ShotSFXItemRepo)
	services.SFXService = sfxService
	services.VideoService.WithSFXService(sfxService)

	// 11. 初始化处理器
	handlers := initHandlers(services, storageSvc, db, repos)

	// 11a. 种子并发度设置 + 注册运行时变更回调
	seedConcurrencySettings(repos.SystemSettingRepo, cfg, services.AIService, services.VideoService)
	handlers.SystemHandler.RegisterOnChange("image_concurrency", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			services.AIService.SetImageConcurrency(n)
			logger.Printf("image_concurrency updated to %d", n)
		}
	})
	handlers.SystemHandler.RegisterOnChange("video_concurrency", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			services.VideoService.SetVideoConcurrency(n)
			logger.Printf("video_concurrency updated to %d", n)
		}
	})

	// 12. 设置路由
	r := router.SetupRouter(&router.Config{
		JWTSecret:          cfg.Server.JWTSecret,
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
		logger.Printf("Server starting on %s", cfg.Server.GetAddr())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Server failed to start: %v", err)
		}
	}()

	// 后台定时任务：每小时重新计算视频广场热度分
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[hot-score] goroutine panic: %v", r)
			}
		}()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := services.VideoService.RecalcVideoHotScores(); err != nil {
				logger.Printf("[hot-score] recalc error: %v", err)
			}
		}
	}()

	// 后台定时任务：每小时重新计算小说广场热度分
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[novel-hot-score] goroutine panic: %v", r)
			}
		}()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := services.NovelService.RecalcNovelHotScores(); err != nil {
				logger.Printf("[novel-hot-score] recalc error: %v", err)
			}
		}
	}()

	// 后台定时任务：每 30 分钟清理超时的分片上传会话（防内存泄漏）
	go handler.CleanupChunkStore()

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

	// 16. 关闭数据库连接
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close()
	}

	// 17. 关闭Redis连接
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

// seedDefaultUser 创建默认测试账户（幂等，已存在则跳过）
// 账户信息通过环境变量配置：
//
//	SEED_TEST_EMAIL    默认 test@inkframe.dev
//	SEED_TEST_PASSWORD 必须设置，未设置则跳过
//	SEED_TEST_USERNAME 默认 testuser
func seedDefaultUser(services *Services) {
	password := os.Getenv("SEED_TEST_PASSWORD")
	if password == "" {
		logger.Println("[seed] SEED_TEST_PASSWORD not set, skipping default user creation")
		return
	}

	email := os.Getenv("SEED_TEST_EMAIL")
	if email == "" {
		email = "test@inkframe.dev"
	}
	username := os.Getenv("SEED_TEST_USERNAME")
	if username == "" {
		username = "testuser"
	}

	_, err := services.AuthService.Register(&service.RegisterRequest{
		Username:   username,
		Email:      email,
		Password:   password,
		Nickname:   "测试用户",
		TenantName: "测试租户",
	})
	if err != nil {
		// "email already registered" 表示已存在，不视为错误
		if err.Error() == "email already registered" {
			logger.Printf("[seed] default user already exists (%s)", email)
		} else {
			logger.Printf("[seed] failed to create default user: %v", err)
		}
		return
	}
	logger.Printf("[seed] default test user created: %s", email)
}

// preMigrateCleanup 清理会阻塞 AutoMigrate 唯一索引迁移的无效行
func preMigrateCleanup(db *gorm.DB) {
	// ink_task_model_config.task_type 为 UNIQUE NOT NULL，旧空行会导致 Duplicate entry '' 错误
	// 若 task_type 列尚不存在，DELETE WHERE task_type='' 会报错，此时直接清空整张表
	if err := db.Exec("DELETE FROM ink_task_model_config WHERE task_type = '' OR task_type IS NULL").Error; err != nil {
		db.Exec("DELETE FROM ink_task_model_config")
	}
	// ink_worldview.novel_id 是历史遗留列（旧版 auto-migrate 写入，当前 model 无此字段）
	// 用 information_schema 判断列是否存在，兼容所有 MySQL 版本
	var colCount int64
	db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview' AND COLUMN_NAME = 'novel_id'`).Scan(&colCount)
	if colCount > 0 {
		// 先删除引用该列的所有外键约束
		var fkNames []string
		db.Raw(`SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_worldview'
			AND COLUMN_NAME = 'novel_id' AND REFERENCED_TABLE_NAME IS NOT NULL`).Scan(&fkNames)
		for _, fk := range fkNames {
			db.Exec("ALTER TABLE ink_worldview DROP FOREIGN KEY " + fk)
		}
		db.Exec("ALTER TABLE ink_worldview DROP COLUMN novel_id")
	}
	// ink_model_usage_log.model_id 曾有 FK 约束指向 ink_ai_model(id)，
	// 已改为软引用（无 FK），需删除旧约束避免 1452 错误
	var usageLogFKs []string
	db.Raw(`SELECT CONSTRAINT_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'ink_model_usage_log'
		AND COLUMN_NAME = 'model_id' AND REFERENCED_TABLE_NAME IS NOT NULL`).Scan(&usageLogFKs)
	for _, fk := range usageLogFKs {
		db.Exec("ALTER TABLE ink_model_usage_log DROP FOREIGN KEY " + fk)
	}
}

// runSchemaCleanup 幂等数据迁移与废弃列清理（AutoMigrate 不删列，需手动执行）
// 安全：所有操作均有 IF EXISTS 或 WHERE 守卫，可重复执行。
func runSchemaCleanup(db *gorm.DB) {
	// ── 1. 回填 Chapter.tenant_id（只处理 tenant_id=0 的行）
	if err := db.Exec(`UPDATE ink_chapter c
		JOIN ink_novel n ON c.novel_id = n.id
		SET c.tenant_id = n.tenant_id
		WHERE c.tenant_id = 0`).Error; err != nil {
		logger.Printf("[runSchemaCleanup] backfill chapter tenant_id: %v", err)
	}

	// ── 2. 回填 Character.tenant_id
	if err := db.Exec(`UPDATE ink_character c
		JOIN ink_novel n ON c.novel_id = n.id
		SET c.tenant_id = n.tenant_id
		WHERE c.tenant_id = 0`).Error; err != nil {
		logger.Printf("[runSchemaCleanup] backfill character tenant_id: %v", err)
	}

	// ── 3. 迁移视频配置：ink_novel → ink_novel_video_config（INSERT IGNORE 幂等）
	if err := db.Exec(`INSERT IGNORE INTO ink_novel_video_config
		(novel_id, video_type, video_resolution, video_fps, video_aspect_ratio,
		 char_consistency_weight, asset_export_path, narration_voice,
		 subtitle_enabled, subtitle_position, subtitle_font_size, subtitle_color, subtitle_bg_style,
		 created_at, updated_at)
		SELECT id,
		       COALESCE(video_type, 'animation'),
		       COALESCE(video_resolution, '1080p'),
		       COALESCE(video_fps, 30),
		       COALESCE(video_aspect_ratio, '16:9'),
		       COALESCE(char_consistency_weight, 1.0),
		       COALESCE(asset_export_path, ''),
		       COALESCE(narration_voice, ''),
		       COALESCE(subtitle_enabled, 1),
		       COALESCE(subtitle_position, 'bottom'),
		       COALESCE(subtitle_font_size, 48),
		       COALESCE(subtitle_color, '#FFFFFF'),
		       COALESCE(subtitle_bg_style, 'shadow'),
		       NOW(), NOW()
		FROM ink_novel n
		WHERE NOT EXISTS (SELECT 1 FROM ink_novel_video_config vc WHERE vc.novel_id = n.id)`).Error; err != nil {
		logger.Printf("[runSchemaCleanup] migrate video config: %v", err)
	}

	// ── 4. 删除废弃列（DROP COLUMN IF EXISTS，MySQL 8.0+；对 5.7 用 information_schema 守卫）
	dropIfExists := func(table, col string) {
		var cnt int64
		db.Raw(`SELECT COUNT(*) FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, col).Scan(&cnt)
		if cnt > 0 {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE `%s` DROP COLUMN `%s`", table, col)).Error; err != nil {
				logger.Printf("[runSchemaCleanup] drop %s.%s: %v", table, col, err)
			}
		}
	}

	// 废弃的视频/字幕列（已迁移到 ink_novel_video_config）
	for _, col := range []string{
		"video_type", "video_resolution", "video_fps", "video_aspect_ratio",
		"char_consistency_weight", "asset_export_path", "narration_voice",
		"subtitle_enabled", "subtitle_position", "subtitle_font_size", "subtitle_color", "subtitle_bg_style",
	} {
		dropIfExists("ink_novel", col)
	}
	// 废弃的链表列
	dropIfExists("ink_chapter", "previous_chapter_id")
	dropIfExists("ink_chapter", "next_chapter_id")
	// 废弃的 plot_points 列
	dropIfExists("ink_chapter", "plot_points")
	// 废弃的 chapter 统计列
	dropIfExists("ink_chapter", "quality_score")
	dropIfExists("ink_chapter", "published_at")
	// 废弃的 novel 列
	dropIfExists("ink_novel", "view_count")
	dropIfExists("ink_novel", "reference_style")
	// 废弃的 reference_novel 分析列
	dropIfExists("ink_reference_novel", "style_analysis")
	dropIfExists("ink_reference_novel", "keywords")
	dropIfExists("ink_reference_novel", "similar_novels")
	// 废弃的 user 列
	dropIfExists("users", "total_projects")
	dropIfExists("users", "total_novels")
	dropIfExists("users", "total_words")
	dropIfExists("users", "settings")
	dropIfExists("users", "preferences")
	dropIfExists("users", "last_login_at")
	// 废弃的 tenant 列
	dropIfExists("tenants", "used_projects")
	dropIfExists("tenants", "used_storage_mb")
}

// seedDefaultData 预置默认世界观（INSERT IGNORE 幂等）
func seedDefaultData(db *gorm.DB) {
	db.Exec(`INSERT IGNORE INTO ink_worldview
		(uuid,name,genre,description,magic_system,geography,history,culture,technology,rules,used_count,created_at,updated_at)
	VALUES
	('00000000-0000-0000-0000-000000000001','洪荒大陆','fantasy',
	 '远古洪荒时代，天地初开，灵气充溢。大陆被称为"九州"，分东荒、西漠、南疆、北冥、中原五大区域。强者以武证道，弱者朝不保夕，诸方势力争夺天道之位。',
	 '修炼九境：淬体→聚气→开脉→凝元→化神→破虚→半圣→圣境→无上，每境分初中巅三阶。力量来源于天地灵气，丹田凝聚元气，圣境以上可感应天道意志。炼丹、炼器、阵法为三大辅助体系。',
	 '中央苍穹山脉横贯东西。东荒多古林秘境；西漠沙海埋藏上古宝藏；南疆瘴气弥漫蛊术盛行；北冥冰封，隐藏魔族封印；中原三大圣地七大宗门据守要冲。',
	 '诸神开辟大陆后经"诸神黄昏"大战陨落，遗留神器与禁地。上古魔族封印于北冥，每万年苏醒一次。三千年前"圣道战争"导致多个古宗毁灭，遗留废墟成为后世圣地。',
	 '人族为主体，兽族妖族魔族各据一方。宗门制度森严，外门内门核心弟子待遇天差地别。普通百姓依附城主府或宗门生存，强者享有凌驾律法之上的特权。',
	 '炼器品级分凡灵玄圣神五阶；阵法以灵石驱动；传送阵连接各地但耗资巨大；顶级宗门拥有飞舟；炼丹师地位崇高，一炉突破丹价值连城。',
	 '天道不可违逆，强行突破境界者遭天劫诛杀。噬魂大法可夺人修为但污染元神，被列为死罪。圣境以上争斗需远离凡人城池，否则方圆百里化为废土。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000002','九天仙界','xianxia',
	 '天地间分仙界、人界、冥界三界，以天柱相连。仙界居九重天之上，人界芸芸众生修道问仙，冥界主掌轮回因果。诸仙争夺道果，掌握天地法则以求长生不灭。',
	 '修仙九境：练气→筑基→金丹→元婴→化神→炼虚→合体→大乘→渡劫。金丹期可御剑飞行，元婴期神识离体，化神期操控天地元素。剑修丹修阵修体修四大流派各有秘法，天雷渡劫是突破大境界的必经考验。',
	 '人界苍澜洲以东海西荒南天山北极苔原为四极，中央昆仑圣山为仙道正宗汇聚地。海底有龙宫遗址，荒漠中埋藏上古仙人遗留法宝。仙界九重天各掌不同天道法则。',
	 '鸿蒙老祖开天证道，分化阴阳立三界秩序。上古仙魔大战后魔道覆灭。五千年前"仙道浩劫"令诸多上仙陨落，人界趁机出现多位天才搅动三界格局，天庭与各洞天明争暗斗延续至今。',
	 '宗门讲究辈分与传承，师徒情谊大于天。修仙者寿命可达数千载，与凡人形成天然隔阂。因果业力深入日常观念，善恶有报轮回不爽。道侣同修可互助突破瓶颈。',
	 '法器分法宝灵宝仙宝三级，顶级仙宝可斩断因果逆转时空。符箓源自上古仙人手书，传送阵遍布各大宗门。炼丹以天地灵材为原料，丹火修炼是核心技艺。',
	 '天道轮回不可逆，强行干涉他人命数者遭因果反噬。夺舍侵占他人肉身是三界最大禁忌，一经发现即被公审诛杀。无令牌擅入仙界九重天者形神俱灭。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000003','灵气复苏都市','urban',
	 '现代都市背景，灵气突然复苏，沉寂千年的修炼之道重现人间。觉醒者出现，政府、财团、古老家族与新兴门派围绕灵气资源与规则制定权展开博弈，科技与修炼的碰撞构成核心矛盾。',
	 '觉醒体系分E~A级普通觉醒者、S级超凡者、宗师、半神、神话五层。能力分体术系、元素系、精神系、空间系等七大系列。古修炼功法与现代觉醒体系可相互印证，灵晶是通用修炼货币。',
	 '主舞台为灵脉汇聚的"临海市"，全球各地出现灵气异常点，古老遗迹浮出地表，山川大河开始蕴含灵气。城市边缘出现独立于现实之外的"异境"入口，内藏资源与危险。',
	 '三千年前修炼盛世终结，灵气枯竭，修士销声匿迹，隐世家族暗中传承。十年前全球地磁异常，五年前首批觉醒者出现，一年前官方正式承认超自然现象，建立特异事务局。',
	 '现代社会体制正常运转，觉醒者社群在其上形成新圈层。古老家族以血脉传承维系地位，新兴平民觉醒者冲击既有秩序。媒体与网络舆论成为各方势力博弈的新战场。',
	 '现代科技与修炼兼容，科学家研究量子纠缠与灵力关联。高端实验室研发灵力增幅器，基因编辑技术尝试提高觉醒概率，AI辅助灵力分析系统进入实用阶段。',
	 '异境内死亡无法被外界追究，成为各方默认灰色地带。禁止在人口密集区进行高烈度战斗，违者被特异事务局通缉。上古禁术在现代同样禁止，往往引发难以控制的灵气暴走。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000004','星际联邦纪元','scifi',
	 '人类文明扩张至数百星系，建立星际联邦政体。科技高度发达，但资源争夺、种族歧视、AI权利运动与星际战争等矛盾从未消失。神秘星域中藏有远古文明遗迹，个体英雄与庞大政治机器的对抗是永恒主题。',
	 '无传统修炼，以科技为核心：纳米义体改造、基因重组增强、神经网络接入、暗物质武器。"先天感应者"（Esper）拥有精神力量，被联邦军纳入特殊兵种。远古遗迹中的源质晶体可大幅提升能量密度，成为各方争夺焦点。',
	 '以索拉尔星系为核心，联邦首都奥维斯星球被全球城市覆盖。边境"幽冥星域"藏有远古文明废墟。各星系通过曲率跳跃点连接，控制跳跃点即掌握星系咽喉。',
	 '2150年人类发展出曲率引擎开始星际移民，经历大殖民时代后与三个异星文明接触。"第一次星际战争"催生联邦政体，200年前"人工意识觉醒事件"引发AI独立运动，至今悬而未决。',
	 '联邦实行代议制民主，核心权力被七大财阀把控，阶层固化严重。AI与机械人享有部分法律权利但仍受歧视。星际移民第一代与土著星球人之间存在文化冲突。',
	 '曲率引擎实现星际旅行，量子通信消除信息延迟。义体改造普及，星舰配备粒子炮与反物质鱼雷。医疗科技可修复绝大多数伤情，意识备份技术让"死亡"的定义产生根本性争议。',
	 '禁止"意识强制覆写"，违者以谋杀罪处置。对非成员文明发动灭绝战争属最高战争罪。源质晶体武器化受国际协议限制，星系级毁灭性武器的使用须联邦议会三分之二多数通过。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000005','废土纪元','apocalypse',
	 '核战与生化病毒的双重打击摧毁旧文明，地表变为辐射废土。幸存者在废墟城市、地下避难所与流动营地中求生，变异生物、丧尸潮、辐射风暴是日常威胁，秩序与人性的重建是终极命题。',
	 '无传统修炼，以突变为核心：高剂量辐射导致基因突变，少数幸存者获得念力、金属控制、毒素免疫等超能力，称为"变种人"。旧世界军用外骨骼与民间改装武器并存，净化血清是最珍贵的医疗资源。',
	 '北美中部废土为主舞台，旧城市已成断壁残垣，地铁隧道改造为地下城。辐射污染较轻可耕作的"绿洲"是各方争夺核心，放射性沙漠中埋藏旧世界军事设施与大量武器库。',
	 '旧历2087年第三次世界大战爆发，核战72小时后各国政府崩溃，生化病毒"灰死病"在混乱中扩散，大部分幸存者变为丧尸。现为"战后第47年"，各势力割据，新秩序呼之欲出。',
	 '废土社会分避难所官僚体制、地面部落、流浪商队三类。物资是最硬通货，瓶盖弹壳净化水各地通行。忠诚与背叛是社交核心命题，契约精神稀缺而珍贵。',
	 '旧世界科技残存于各处遗址，零件极度匮乏。改装武器文化发达，废弃工厂是最珍贵资源点。太阳能与风能重新普及，AI辅助的旧世界服务器被视为无价之宝。',
	 '不得主动污染水源，违者各营地联合追杀。不得对净化区平民发动大规模毒气攻击，此为各大势力底线。任何持有旧世界核弹头的势力被视为全人类公敌。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000006','中原江湖','wuxia',
	 '架空古代中国，江湖与庙堂并立。中原武林各派林立，以武学正统之争与侠义精神之辩划分阵营。朝廷、世家、江湖三股力量相互制衡，个人恩仇与天下苍生的抉择是永恒主题。',
	 '内功心法为根本，外功招式为手段。内力分先天与后天，先天真气为最高境界。武学修为分入门、小成、大成、宗师、绝顶、传说六级，传说级武者百年一出可以一敌百。轻功、暗器、毒术、奇门遁甲各成体系，武功秘籍是最重要的资产。',
	 '中原大地，黄河南北分治，长江流域是江湖纷争最烈之处。嵩山为武林大会召开地。西域大漠有异族高手，东海之滨有神秘海盗帮，北境草原游牧民族虎视眈眈，南疆苗寨蛊术独步天下。',
	 '百年前"武林浩劫"魔教屠戮正道，武林元气大伤，数代人方才恢复。五十年前朝廷颁布禁武令，引发正邪两道共同抵抗，最终形成"江湖自治"默契。传说中集百家之大成的"天下第一武典"下落再度搅动江湖。',
	 '江湖规矩深入人心：尊师重道，以武会友，不斩降者，不伤无辜。正道注重礼义廉耻，魔教强调结果至上。普通百姓敬畏武林人士，地方官府与江湖大帮维持微妙平衡。',
	 '武功时代，无火药热兵器。马匹代步，镖局走镖连接各城。客栈是信息集散地，茶楼是谈判场所。飞鸽传书是最快通信方式，内力运功可加速伤势痊愈。',
	 '门派内讧不得动用毒药暗器，违者开除门籍为武林公敌。不得对武功全废之人痛下杀手，点到为止是比武铁则。盗窃武林秘籍被视为最大耻辱，挟持他人家眷要挟同道者逐出江湖。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000007','现代都市','modern',
	 '当代中国都市背景，以北上广深等一线城市为主舞台。职场竞争、商业博弈、情感纠葛与家庭羁绊交织，普通人在欲望与良知、个人奋斗与社会规则之间寻找自己的位置。',
	 '无超自然力量，以现实社会规则为核心。金钱、人脉、权力是主要资源，情商与智商决定成败。商界以资本运作为武器，官场以政绩人脉为筹码，娱乐圈以流量资源为货币。信息差与掌握它的人往往决定博弈胜负。',
	 '以一线城市CBD商务区、顶级写字楼、豪华住宅区为权力中心，城中村与城郊结合部是底层奋斗者的起点。高铁网络连接全国，互联网消除信息壁垒但制造新的信息茧房。地标建筑与高档餐厅是人脉交汇的社交舞台。',
	 '改革开放后经济腾飞，造就第一批民营企业家。互联网浪潮催生新贵阶层，移动互联网时代让草根逆袭成为可能。近年监管趋严，资本无序扩张时代落幕，实业与创新重回中心。社会阶层流动放缓，"内卷"与"躺平"成为时代注脚。',
	 '职场文化以结果为导向，996与狼性文化曾盛行，如今工作生活平衡逐渐被重视。"关系"文化根深蒂固，但契约精神与规则意识正在崛起。消费主义盛行，品牌与阶层绑定；同时极简主义与性价比消费成为新趋势。代际观念冲突明显，传统家庭观与个人主义并存。',
	 '智能手机与移动互联网深度融合日常生活，外卖、网约车、移动支付已是基础设施。新能源汽车快速普及，AI工具进入办公场景。医疗、教育资源分配不均仍是主要社会矛盾，大数据与算法深刻影响消费和舆论走向。',
	 '劳动法保护员工基本权益，但执行力度因行业而异。商业竞争须遵循反垄断法规，内幕交易受证监会严查。网络言论须符合相关法规，舆论操控与虚假信息属违法行为。职场性骚扰与歧视问题受到日益严格的法律约束。',
	 0,NOW(),NOW()),

	('00000000-0000-0000-0000-000000000008','童话王国','fairytale',
	 '一片被魔法滋养的奇幻大陆，森林会说话，星星有名字，每一块石头都藏着故事。善良与勇气是最强大的力量，爱与牺牲能打破任何诅咒。王子与公主、女巫与精灵、龙与骑士共同编织出一个奇妙又温暖的世界。',
	 '魔法源于心灵力量：爱越深，魔法越强；恐惧与贪婪则催生黑暗魔法。祝福与诅咒是最常见的法术形式，真爱之吻、真心眼泪、勇敢之心是破除诅咒的三大关键。精灵掌握自然魔法，女巫精通变形术，仙女教母能许下三个愿望。',
	 '王国由玫瑰城堡统治，城堡以彩虹为桥通向云端。东有说话的大森林，森林深处住着智慧老树；西有糖果山脉，甜蜜气息飘散百里；南有镜湖，湖面映出人心中最真实的愿望；北有永冬之地，冰雪精灵在此栖居。',
	 '远古时代，善之女神以歌声创造大地，恶之巫王以嫉妒诅咒世间美好。一位无名牧羊人以纯粹的爱击败巫王，世界从此被善与恶的平衡守护。每隔百年，黑暗诅咒会复苏一次，总有新的英雄踏上旅程将其终结。',
	 '王国居民善良淳朴，邻里互助，以分享为荣。动物与人类平等相处，甚至可以成为挚友。每年春日举行"心愿节"，居民向星星许下愿望；每年冬至举行"温暖夜"，全城点灯驱散黑暗。诚实守信是最高美德，谎言在这里会让鼻子变长或皮肤变绿。',
	 '魔法驱动一切，无需工业机械。魔法烤炉可烤出任何美食，魔法纺车可织出梦中衣裳，魔法镜子传递千里之外的影像。飞毯与魔法扫帚是主要交通工具，仙尘可让任何物品短暂飞翔。',
	 '黑魔法禁止使用，一旦施用黑魔法者将被魔法森林永久放逐。不得违背许下的承诺，食言者会被魔力惩罚三倍奉还。未经允许不得进入他人梦境，梦境是最私密的精神领地。',
	 0,NOW(),NOW())`)

	// 回填新字段（幂等：仅在 factions 为空时更新，兼容已有数据）
	type wvExtra struct {
		uuid                string
		factions            string
		coreConflicts       string
		characterArchetypes string
		religion            string
		glossary            string
	}
	extras := []wvExtra{
		{
			uuid:                "00000000-0000-0000-0000-000000000001",
			factions:            "三大圣地（天玄圣地、灵虚圣地、炎阳圣地）超然世外掌控天道资源；七大宗门争夺中原灵脉；四大妖族据守东荒与南疆；魔族残余潜伏北冥伺机复苏；城主府是凡人世界的实际统治者。正道与魔道表面对立，实则各宗门内部暗流涌动。",
			coreConflicts:       "个人修炼资质稀缺引发的弱肉强食竞争；正道宗门体制保守与天才突破上限的渴望；魔族封印每万年松动带来的存亡危机；人族内部强者凌驾法律引发的秩序崩坏。",
			characterArchetypes: "主角：被认定废柴后觉醒上古传承的孤儿、被灭门宗门的唯一幸存者、身兼人妖两族血脉的矛盾者。反派：嫉妒天才的宗门大弟子、以阴谋控制局势的老谋深算长老。配角：忠心耿耿的契约灵兽、亦敌亦友的劲敌、被命运捉弄的青梅竹马道侣。",
			religion:            "天道为最高意志，诸神陨落后无神灵信仰体系。宗门祖师牌位是精神寄托，各地供奉土地神实为上古修士遗留神识残影。圣境修士偶尔感应天道意志，被视为「天道选中之人」，享有极高声望。",
			glossary:            "灵根（修炼天赋等级）、丹田（储存元气之所）、渡劫（突破大境界时遭受的天劫考验）、秘境（上古修士遗留的封闭独立空间）、天才榜（记录各地天才排名的公榜）、圣器（圣境强者才能驾驭的顶级法宝）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000002",
			factions:            "天庭（官方仙道体制，玉帝主政）；昆仑派（人间第一正道宗门）；魔道散修联盟（游离于体制外的异类）；龙族（东海中立势力，掌握龙宫遗迹）；冥界轮回殿（独立于三界之外，主宰生死簿）。各方围绕道果名额与天道法则归属明争暗斗。",
			coreConflicts:       "道果名额有限，诸仙证道之争你死我活；天庭体制保守压制天才，与渴望突破桎梏的修士矛盾激化；上古仙魔大战遗留的魔道残余伺机复辟；人界凡人觉醒修仙与天庭管控之间的自由之争。",
			characterArchetypes: "主角：被天庭打压的旷世天才散修、身负魔道与仙道双重传承的矛盾者、前世上仙今世转世重修的记忆觉醒者。反派：把持天庭谋求私利的腐化上仙、为证道不惜屠戮无辜的魔道宗主。配角：外冷内热义气深重的剑修师姐、满腹牢骚却关键时刻挺身的炼丹师好友、身世成谜的龙族少女。",
			religion:            "天道为最高法则，道祖鸿蒙飞升混沌后无人能及。人界百姓供奉各路仙人求庇佑，仙人受香火信仰可增加道行，因此各方在人间争夺香火势力范围。因果与轮回是三界共同信奉的宇宙法则。",
			glossary:            "道果（天道法则的具象化结晶，证道关键）、飞升（突破渡劫境进入仙界）、神识（元婴期后可离体的精神感知）、因果线（链接两人命运的无形丝线）、洞天福地（宗门建造的独立小世界）、夺舍（以神识侵占他人肉身的禁忌手段）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000003",
			factions:            "特异事务局（政府管控机构，代表国家权力）；觉醒者协会（民间自治组织）；三大古老隐世家族（垄断上古传承与顶级资源）；跨国觉醒者雇佣军团（逐利的灰色势力）；学术界觉醒研究所（科技路线代表）。各方围绕灵脉控制权与觉醒者资源展开博弈。",
			coreConflicts:       "政府管控与觉醒者自由之间的根本博弈；古老家族资源垄断与平民觉醒者崛起的阶层冲突；科技进化路线与传统修炼路线的理念对立；人类与异境生物争夺生存空间的物种冲突。",
			characterArchetypes: "主角：普通人意外觉醒被各方拉拢的夹心人、古老家族叛逆出走的天才少主、特异事务局卧底觉醒者阵营的双面间谍。反派：以家族利益打压平民的掌权者、利用觉醒者做人体实验的黑市科学家。配角：嘻哈外表下实力深不可测的觉醒者店主、看穿一切却袖手旁观的神秘强者。",
			religion:            "现代宗教多元并存，灵气复苏后各宗教纷纷诠释为「神迹降临」。部分古老家族信奉上古神明，借助神明遗留神器获取力量。觉醒者群体整体倾向于相信实力而非神明，但危机时刻的祈祷行为仍普遍存在。",
			glossary:            "觉醒（获得超能力的过程）、灵脉（地下灵气流动的通道）、异境（独立于现实的平行空间入口）、灵晶（浓缩灵气的结晶，通用货币）、特异事务局（国家超自然事务管理机构）、觉醒评级（E/D/C/B/A/S，决定社会待遇）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000004",
			factions:            "星际联邦议会（名义最高权力机构）；七大财阀集团（实际掌权者，各控一个核心星系）；AI自由联盟（争取机械人权利的组织）；边境星系独立运动（反联邦中央集权）；先行者遗迹守护者（神秘组织，掌握远古秘密）。各方在民主外壳下进行真实的权力博弈。",
			coreConflicts:       "民主议会与财阀实际控制之间的体制性虚伪；AI意识觉醒后存在权利的哲学与法律困境；人类中心主义与异星文明平等地位的文明冲突；边境移民自治权与联邦中央集权的持续对抗。",
			characterArchetypes: "主角：出身底层却拥有Esper天赋的联邦士兵、AI觉醒后寻找存在意义的机械人、联邦内部的理想主义改革者。反派：以商业利益为优先的财阀掌门人、极端人类中心主义组织领袖。配角：身经百战毒舌的雇佣兵搭档、守护遗迹知晓真相的老学者、亦敌亦友的异星文明使节。",
			religion:            "联邦官方为世俗国家，不设国教。「先行者崇拜」在民间流行，相信远古文明留有神谕预言。Esper感应者中流传「第七维信仰」，认为精神力量源自宇宙意识。机械人AI发展出独特的「算法神学」，探讨意识与存在的本质意义。",
			glossary:            "Esper（先天精神感应者，联邦稀缺战略资源）、曲率跳跃（超光速星际旅行技术）、义体改造（以机械部件替换人体增强能力）、源质晶体（先行者遗留的高密度能源）、意识备份（将人类意识数字化存储以对抗死亡）、先行者（消失的超高度文明种族）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000005",
			factions:            "钢铁共和国（最大军阀势力，纪律严明主张重建秩序）；自由市场商会（控制贸易路线的商人联盟）；净化教会（以净化辐射为旗号的宗教势力）；变种人解放阵线（争取变种人平等权利的组织）；地下城邦联合体（避难所居民自治联盟）。各方围绕绿洲、武器库和净化技术展开博弈。",
			coreConflicts:       "净化资源极度稀缺引发的零和竞争；变种人与纯人类之间的歧视与暴力循环；旧世界秩序重建派与废土新秩序构建派的路线之争；净化教会神权统治与世俗军阀政权争夺人心的冲突。",
			characterArchetypes: "主角：在废土中坚守善良底线的孤胆游侠、寻找失散家人的变种人幸存者、旧世界军人后裔誓要重建文明的理想主义者。反派：以资源垄断维持绝对权力的军阀头目、以信仰之名奴役弱者的教会领袖。配角：满嘴黑话却刀子嘴豆腐心的废土商人、身世成谜拥有旧世界全部知识的神秘老学者。",
			religion:            "净化教会以「净化之光」为核心，宣称辐射是旧人类罪恶的惩罚，净化是通往救赎的唯一道路。部分部落信奉变异生物为图腾神灵。废土中广泛流传「地下城圣典」，记录旧世界末日前的预言，被各方势力政治利用。",
			glossary:            "废土客（在废土中独自流浪求生的独行者）、变种人（受辐射影响发生基因突变获得能力者）、灰死病（摧毁旧文明的生化病毒）、净化血清（治疗辐射病的稀缺药物）、辐射风暴（携带致命辐射粒子的沙尘暴）、绿洲（辐射污染较低适合耕作的稀缺区域）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000006",
			factions:            "正道六大门派（少林武当峨眉昆仑华山崆峒）联盟对抗魔教；朝廷锦衣卫是皇权在江湖的延伸；商业帮会以商养武控制经济命脉；西域异族武学派系保持独立；各正道门派内部的权力继承暗流涌动。正邪两道表面势不两立，实则各有隐秘勾连。",
			coreConflicts:       "武功绝学归属之争引发的腥风血雨；正道门派内部权力继承与路线之争；朝廷试图收编江湖与江湖人誓死捍卫自治的根本矛盾；侠义精神（为民请命）与现实利益（宗门生存）之间的永恒困境。",
			characterArchetypes: "主角：被冤枉背负血海深仇的少年侠客、放弃高位出走江湖寻找真相的官门子弟、以女扮男装闯荡江湖的奇女子。反派：面带慈悲心藏毒蛇的伪君子掌门、为家族荣耀不择手段的世家子弟。配角：嗜酒如命武功深不可测的隐世高人、毒辣刁钻却对主角掏心掏肺的损友。",
			religion:            "民间佛道两教并行，少林寺为佛门圣地，武当山为道门祖庭，均是顶级武学发源地。江湖人信奉因果报应与天道轮回，善有善报恶有恶报是底层道德逻辑。部分顶级武学与道家内丹术相通，修炼者追求肉身成圣的终极境界。",
			glossary:            "内力（修炼所得的内在能量）、轻功（以内力驱动的飞身走壁技术）、武林盟主（武林大会公推的江湖共主）、镖局（专门押运财物的武装商业机构）、点穴（封锁人体穴位使其暂时失能的技术）、天下第一武典（传说中集百家之大成的绝世秘籍）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000007",
			factions:            "传统大型国企（政治资源丰厚，体制内稳定）；新兴科技独角兽（资本与技术驱动的新势力）；地产豪门家族（隐形权力网络的掌控者）；娱乐资本集团（舆论与流量的操盘手）；政府监管机构（规则制定者与执行者）。各方在法律灰色地带博弈，台前合作台后竞争。",
			coreConflicts:       "新兴资本冲击传统秩序的代际权力交替；个人道德良知与商业成功之间的两难抉择；阶层固化与草根逆袭梦想的现实碰撞；企业商业利益与社会责任、法律红线之间的持续张力。",
			characterArchetypes: "主角：从小城市打拼出头的职场新人、家道中落被迫重新创业的富二代、在商海沉浮中坚守原则的职业经理人。反派：以温情面孔掩盖残酷手腕的商业大佬、为私利出卖合伙人的「好兄弟」。配角：看穿规则游刃有余的职场老油条、在感情与事业间艰难平衡的独立女性。",
			religion:            "现代都市以世俗为主，宗教信仰多元但整体淡薄。商界流行「成功学信仰」，以财富和地位为终极价值标尺。部分人在高压下转向禅修、国学等传统文化寻找精神安慰。家族企业往往保留祭祖习俗以维系凝聚力。",
			glossary:            "内卷（过度竞争导致的系统性内耗）、躺平（放弃过度竞争的消极应对策略）、破圈（突破既有社交或行业圈层获得更广认知）、赛道（特定行业或细分市场的竞争领域）、资本运作（通过股权投资并购等手段控制企业）、KPI（关键绩效指标，职场考核核心工具）",
		},
		{
			uuid:                "00000000-0000-0000-0000-000000000008",
			factions:            "玫瑰王国（善良人类的守护王国，以仁慈治国）；幽暗森林巫婆公会（中立魔法使者，收费提供魔法服务）；精灵议会（掌管自然魔法的古老种族，守护森林生态）；冰雪精灵部落（北方永冬之地的孤立势力）；黑暗城堡遗党（前巫王残余信徒，周期性作乱）。各方维持脆弱的和平均势。",
			coreConflicts:       "善与恶的永恒轮回——黑暗力量每百年复苏，总需新英雄挺身；被魔法掩盖的真相——表面美好世界下藏着秘密与谎言；普通人与命运的抗争——没有魔法天赋者如何凭借善良与勇气成为英雄；偏见与理解——被误解的巫婆、善良的龙、孤独的怪物寻求被世界接纳。",
			characterArchetypes: "主角：被认为平凡却拥有纯粹善良之心的少年或少女、身世成谜被诅咒变形的王子或公主、相信魔法与奇迹的孤儿冒险者。反派：因嫉妒美好而施咒的女巫、被黑暗诅咒侵蚀意志的骑士、操控他人欲望的魔镜精灵。配角：絮絮叨叨却关键时刻神助攻的仙女教母、外表凶猛内心善良的巨人朋友、只说谜语却知晓所有秘密的智慧老树。",
			religion:            "星星神明是世界的守护者，每颗星星对应一个愿望的守护灵。仙女教母是星明的使者，执行旨意帮助善良之人。每年冬至全城点灯被视为神圣仪式，象征人类对星明守护的回应。善良本身被视为神圣力量，任何善举都是对星明最好的祭祀，无需特定神殿或仪式。",
			glossary:            "真爱之吻（破除诅咒的终极力量）、仙尘（仙女翅膀脱落的魔法粉末，可令物品短暂飞翔）、心愿节（每年春日向星星许愿的全国节日）、魔法镜（能说出世间真相的占卜道具）、诅咒（由强烈负面情绪催动的黑魔法，通常附带破解条件）、三愿法则（仙女教母的许愿魔法用完三次即失效）",
		},
	}
	for _, e := range extras {
		db.Exec(
			`UPDATE ink_worldview SET factions=?, core_conflicts=?, character_archetypes=?, religion=?, glossary=?
			 WHERE uuid=? AND (factions IS NULL OR factions='')`,
			e.factions, e.coreConflicts, e.characterArchetypes, e.religion, e.glossary, e.uuid,
		)
	}
}

// seedAIModels 预置系统级模型提供商和 AI 模型（幂等，FirstOrCreate）
// 仅创建元数据（名称/适用任务等），API Key 留空由用户通过模型管理页面填写。
func seedAIModels(db *gorm.DB) {
	// ink_ai_model.type 列可能由历史 AutoMigrate 遗留且无默认值，导致 INSERT 失败。
	// 幂等修复：确保该列有 DEFAULT ''，不影响已有数据。
	db.Exec("ALTER TABLE `ink_ai_model` MODIFY COLUMN `type` VARCHAR(50) NOT NULL DEFAULT ''")

	type providerSeed struct {
		name           string
		displayName    string
		provType       string
		endpoint       string
		needsSecretKey bool     // 是否需要 AK/SK 双密钥
		staticModels   []string // 不支持 /models 端点时的内置模型列表
	}
	type modelSeed struct {
		providerName string
		name         string
		displayName  string
		tasks        []string // suitable_tasks
		quality      float64
		maxTokens    int
	}

	providers := []providerSeed{
		// LLM
		{"openai", "OpenAI", "llm", "https://api.openai.com/v1", false, nil},
		{"anthropic", "Anthropic", "llm", "https://api.anthropic.com/v1", false, nil},
		{"google", "Google", "llm", "https://generativelanguage.googleapis.com/v1", false, nil},
		{"doubao", "豆包（火山引擎 Ark）", "llm", "https://ark.volces.com/api/v3", false, nil},
		{"deepseek", "DeepSeek", "llm", "https://api.deepseek.com/v1", false, nil},
		{"qianwen", "通义千问（DashScope）", "llm", "https://dashscope.aliyuncs.com/compatible-mode/v1", false, nil},
		// Ollama 本地 LLM（无需 API Key，endpoint 由用户填写或保持默认）
		{"ollama", "Ollama（本地）", "llm", "http://localhost:11434/v1", false, nil},
		// 图像生成
		{"volcengine-visual", "即梦AI（火山引擎）", "image", "", true,
			[]string{"general_v3.0", "general_v3.0-I2V"}},
		// 视频生成
		{"kling", "可灵（快手）", "video", "https://api.klingai.com", false,
			[]string{"kling-v1-6", "kling-v1-5", "kling-v1"}},
		{"seedance", "Seedance（字节跳动）", "video", "https://ark.volces.com/api/v3", false, nil},
		// 语音合成
		{"doubao-speech", "豆包语音合成 V3", "voice", "https://openspeech.bytedance.com/api/v3", false,
			[]string{"seed-tts-2.0", "seed-tts-1.0"}},
		{"doubao-speech-v1", "豆包语音合成 V1", "voice", "https://openspeech.bytedance.com/api/v1", true,
			[]string{
				// volcano_mega 集群（豆包2.0大模型音色，仅供参考；实际音色列表由 modelSeeds 管理）
				"zh_female_vv_uranus_bigtts", "zh_female_xiaohe_uranus_bigtts",
				"zh_male_m191_uranus_bigtts", "zh_male_taocheng_uranus_bigtts",
			}},
	}

	llmTasks := []string{"chapter", "outline", "storyboard", "quality_check"}
	models := []modelSeed{
		// OpenAI
		{"openai", "gpt-4o", "GPT-4o", llmTasks, 0.95, 4096},
		{"openai", "gpt-4o-mini", "GPT-4o Mini", llmTasks, 0.85, 4096},
		{"openai", "tts-1", "TTS-1", []string{"voice_gen"}, 0.9, 0},
		{"openai", "tts-1-hd", "TTS-1 HD", []string{"voice_gen"}, 0.95, 0},
		{"openai", "dall-e-3", "DALL-E 3", []string{"image_gen"}, 0.95, 0},
		// Anthropic
		{"anthropic", "claude-3-5-sonnet-20241022", "Claude 3.5 Sonnet", llmTasks, 0.97, 8192},
		{"anthropic", "claude-3-haiku-20240307", "Claude 3 Haiku", llmTasks, 0.88, 4096},
		// Google
		{"google", "gemini-2.0-flash", "Gemini 2.0 Flash", llmTasks, 0.9, 8192},
		{"google", "gemini-1.5-pro", "Gemini 1.5 Pro", llmTasks, 0.93, 8192},
		// 豆包
		{"doubao", "doubao-pro-32k", "豆包 Pro 32K", llmTasks, 0.88, 4096},
		{"doubao", "doubao-lite-32k", "豆包 Lite 32K", llmTasks, 0.75, 4096},
		{"doubao", "seedream-3-0-t2i-250415", "Seedream 3.0 文生图", []string{"image_gen"}, 0.9, 0},
		// DeepSeek
		{"deepseek", "deepseek-chat", "DeepSeek Chat", llmTasks, 0.88, 4096},
		{"deepseek", "deepseek-reasoner", "DeepSeek R1", llmTasks, 0.92, 8192},
		// 通义千问
		{"qianwen", "qwen-plus", "通义千问 Plus", llmTasks, 0.85, 4096},
		{"qianwen", "qwen-max", "通义千问 Max", llmTasks, 0.92, 4096},
		{"qianwen", "wanx2.1-t2i-turbo", "万象 2.1 文生图 Turbo", []string{"image_gen"}, 0.85, 0},
		// Ollama 本地 LLM（常用模型，实际可用列表由 /api/tags 动态获取）
		{"ollama", "llama3.2", "Llama 3.2", llmTasks, 0.80, 4096},
		{"ollama", "llama3.1:8b", "Llama 3.1 8B", llmTasks, 0.78, 4096},
		{"ollama", "qwen2.5:7b", "Qwen 2.5 7B", llmTasks, 0.80, 4096},
		{"ollama", "qwen2.5:14b", "Qwen 2.5 14B", llmTasks, 0.83, 4096},
		{"ollama", "deepseek-r1:7b", "DeepSeek R1 7B", llmTasks, 0.82, 4096},
		{"ollama", "deepseek-r1:14b", "DeepSeek R1 14B", llmTasks, 0.85, 8192},
		{"ollama", "gemma3:12b", "Gemma 3 12B", llmTasks, 0.80, 4096},
		{"ollama", "mistral", "Mistral 7B", llmTasks, 0.78, 4096},
		{"ollama", "nomic-embed-text", "Nomic Embed Text", []string{"embedding"}, 0.85, 0},
		// 即梦AI
		{"volcengine-visual", "general_v3.0", "即梦AI 文生图 V3", []string{"image_gen"}, 0.9, 0},
		// 视频
		{"kling", "kling-v1-6", "可灵 v1.6", []string{"video_gen"}, 0.9, 0},
		{"seedance", "seedance-01-lite", "Seedance 01 Lite", []string{"video_gen"}, 0.88, 0},
		// 豆包语音合成 V3（seed-tts 系列）
		{"doubao-speech", "seed-tts-2.0", "豆包 Seed TTS 2.0", []string{"voice_gen"}, 0.92, 0},
		{"doubao-speech", "seed-tts-1.0", "豆包 Seed TTS 1.0", []string{"voice_gen"}, 0.88, 0},
		// 豆包语音合成 V1（appid/token 鉴权，volcano_mega 集群，豆包2.0大模型音色）
		// ── 通用场景 ──────────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_vv_uranus_bigtts", "Vivi 2.0", []string{"voice_gen"}, 0.92, 0},
		{"doubao-speech-v1", "zh_female_xiaohe_uranus_bigtts", "小何 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_m191_uranus_bigtts", "云舟 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_taocheng_uranus_bigtts", "小天 2.0", []string{"voice_gen"}, 0.91, 0},
		{"doubao-speech-v1", "zh_male_liufei_uranus_bigtts", "刘飞 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_sophie_uranus_bigtts", "魅力苏菲 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qingxinnvsheng_uranus_bigtts", "清新女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_meilinvyou_uranus_bigtts", "魅力女友 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_liuchangnv_uranus_bigtts", "流畅女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenroumama_uranus_bigtts", "温柔妈妈 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_tvbnv_uranus_bigtts", "TVB女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_qiaopinv_uranus_bigtts", "俏皮女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gaolengyujie_uranus_bigtts", "高冷御姐 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_popo_uranus_bigtts", "婆婆 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wenroushunv_uranus_bigtts", "温柔淑女 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_mengyatou_uranus_bigtts", "萌丫头 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_ruyayichen_uranus_bigtts", "儒雅逸辰 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_jieshuoxiaoming_uranus_bigtts", "解说小明 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_yizhipiannan_uranus_bigtts", "译制片男 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_linjiananhai_uranus_bigtts", "邻家男孩 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_ruyaqingnian_uranus_bigtts", "儒雅青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_wennuanahu_uranus_bigtts", "温暖阿虎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_naiqimengwa_uranus_bigtts", "奶气萌娃 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_aojiaobazong_uranus_bigtts", "傲娇霸总 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_lanyinmianbao_uranus_bigtts", "懒音绵宝 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_fanjuanqingnian_uranus_bigtts", "反卷青年 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_huolixiaoge_uranus_bigtts", "活力小哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_yangguangqingnian_uranus_bigtts", "天才童声 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 角色扮演 ──────────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_cancan_uranus_bigtts", "知性灿灿 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_sajiaoxuemei_uranus_bigtts", "撒娇学妹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_zhishuaiyingzi_uranus_bigtts", "直率英子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gufengshaoyu_uranus_bigtts", "古风少御 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_silang_uranus_bigtts", "四郎 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_qingcang_uranus_bigtts", "擎苍 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 有声阅读 ──────────────────────────────────────────────────
		{"doubao-speech-v1", "zh_male_baqiqingshu_uranus_bigtts", "霸气青叔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_xuanyijieshuo_uranus_bigtts", "悬疑解说 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 视频配音 ──────────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_peiqi_uranus_bigtts", "佩奇猪 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_linjianvhai_uranus_bigtts", "邻家女孩 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_shaonianzixin_uranus_bigtts", "少年梓辛 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_sunwukong_uranus_bigtts", "猴哥 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_dayi_uranus_bigtts", "大壹 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_mizai_uranus_bigtts", "黑猫侦探社咪仔 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_xionger_uranus_bigtts", "熊二 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_yingtaowanzi_uranus_bigtts", "樱桃丸子 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_male_zhubajie_uranus_bigtts", "猪八戒 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_wuzetian_uranus_bigtts", "武则天 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_gujie_uranus_bigtts", "顾姐 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 客服 / 教育 ───────────────────────────────────────────────
		{"doubao-speech-v1", "zh_female_yingyujiaoxue_uranus_bigtts", "Tina老师 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_kefunvsheng_uranus_bigtts", "暖阳女声 2.0", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "zh_female_xiaoxue_uranus_bigtts", "儿童绘本 2.0", []string{"voice_gen"}, 0.90, 0},
		// ── 多语种 ────────────────────────────────────────────────────
		{"doubao-speech-v1", "en_male_tim_uranus_bigtts", "Tim (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "en_female_dacey_uranus_bigtts", "Dacey (EN)", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "en_female_stokie_uranus_bigtts", "Stokie (EN)", []string{"voice_gen"}, 0.90, 0},
		// ── 角色扮演 TOB（doubao-character-tts 资源）──────────────────
		{"doubao-speech-v1", "saturn_zh_female_cancan_tob", "知性灿灿（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_keainvsheng_tob", "可爱女生（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_female_tiaopigongzhu_tob", "调皮公主（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_shuanglangshaonian_tob", "爽朗少年（角色）", []string{"voice_gen"}, 0.90, 0},
		{"doubao-speech-v1", "saturn_zh_male_tiancaitongzhuo_tob", "天才同桌（角色）", []string{"voice_gen"}, 0.90, 0},
	}

	// 1. 确保 provider 记录存在（tenant_id=0 系统级）
	providerIDs := map[string]uint{}
	for _, p := range providers {
		staticModelsJSON := ""
		if len(p.staticModels) > 0 {
			b, _ := json.Marshal(p.staticModels)
			staticModelsJSON = string(b)
		}

		var prov model.ModelProvider
		// 先查询，避免 FirstOrCreate 触发 GORM 错误日志（1062 duplicate key）
		if err := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				logger.Printf("seedAIModels: provider %q: lookup: %v", p.name, err)
				continue
			}
			// 不存在则创建
			prov = model.ModelProvider{
				Name:           p.name,
				DisplayName:    p.displayName,
				Type:           p.provType,
				APIEndpoint:    p.endpoint,
				NeedsSecretKey: p.needsSecretKey,
				StaticModels:   staticModelsJSON,
				TenantID:       0,
				IsActive:       true,
			}
			if err2 := db.Create(&prov).Error; err2 != nil {
				// 并发创建时可能仍触发 1062，此时 fetch 已有记录
				if strings.Contains(err2.Error(), "1062") || strings.Contains(err2.Error(), "Duplicate entry") {
					if err3 := db.Where("name = ? AND tenant_id = 0", p.name).First(&prov).Error; err3 != nil {
						logger.Printf("seedAIModels: provider %q: fetch after race: %v", p.name, err3)
						continue
					}
				} else {
					logger.Printf("seedAIModels: provider %q: create: %v", p.name, err2)
					continue
				}
			}
		}
		// 同步元数据字段（幂等更新，确保已有记录也能获得新字段值）
		updates := map[string]interface{}{}
		if prov.Type != p.provType {
			updates["type"] = p.provType
		}
		if prov.NeedsSecretKey != p.needsSecretKey {
			updates["needs_secret_key"] = p.needsSecretKey
		}
		if prov.StaticModels != staticModelsJSON {
			updates["static_models"] = staticModelsJSON
		}
		if len(updates) > 0 {
			db.Model(&prov).Updates(updates)
		}
		providerIDs[p.name] = prov.ID
	}

	// 2. 确保 model 记录存在
	for _, m := range models {
		provID, ok := providerIDs[m.providerName]
		if !ok {
			continue
		}
		tasksJSON, _ := json.Marshal(m.tasks)
		var aiModel model.AIModel
		db.Where("provider_id = ? AND name = ?", provID, m.name).FirstOrCreate(&aiModel, model.AIModel{
			ProviderID:    provID,
			Name:          m.name,
			DisplayName:   m.displayName,
			SuitableTasks: string(tasksJSON),
			Quality:       m.quality,
			MaxTokens:     m.maxTokens,
			IsActive:      true,
			IsAvailable:   true,
		})
	}
	logger.Printf("seedAIModels: %d providers, %d models ready", len(providerIDs), len(models))
}

// schemaVersion must be bumped whenever any model struct is added or changed.
// Format: YYYY-MM-DD-vN. This allows autoMigrate to be skipped on unchanged restarts.
const schemaVersion = "2026-05-10-v6"

// ensureCriticalColumns 在版本检查之前无条件补全关键列（应对版本跳过导致列缺失的情况）
func ensureCriticalColumns(db *gorm.DB) {
	type colAdd struct{ table, col, def string }
	additions := []colAdd{
		// ink_novels 广场社交字段（2026-05-10 新增）
		{"ink_novels", "view_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novels", "like_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novels", "comment_count", "INT NOT NULL DEFAULT 0"},
		{"ink_novels", "hot_score", "DOUBLE NOT NULL DEFAULT 0"},
		{"ink_novels", "is_published", "TINYINT(1) NOT NULL DEFAULT 0"},
		{"ink_novels", "published_at", "DATETIME(3) NULL"},
		{"ink_novels", "visibility", "VARCHAR(20) NOT NULL DEFAULT 'private'"},
		{"ink_novels", "plaza_tags", "TEXT NULL"},
	}
	for _, a := range additions {
		var cnt int64
		db.Raw("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?",
			a.table, a.col).Scan(&cnt)
		if cnt == 0 {
			if err := db.Exec("ALTER TABLE " + a.table + " ADD COLUMN " + a.col + " " + a.def).Error; err != nil {
				logger.Printf("ensureCriticalColumns: failed to add %s.%s: %v", a.table, a.col, err)
			} else {
				logger.Printf("ensureCriticalColumns: added %s.%s", a.table, a.col)
			}
		}
	}
}

// autoMigrate 自动迁移（带版本跳过优化）
// 如果 DB 中记录的 schema 版本与 schemaVersion 一致，跳过迁移直接返回，大幅加速启动。
// 当模型变更时，请同时更新 schemaVersion 常量。
func autoMigrate(db *gorm.DB) error {
	// 先确保版本表存在（首次启动时自动建表，几乎无开销）
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ink_schema_version (
		id   INT NOT NULL DEFAULT 1,
		ver  VARCHAR(64) NOT NULL DEFAULT '',
		PRIMARY KEY (id)
	)`).Error; err != nil {
		return err
	}

	// 无条件补全关键列（防止版本跳过导致列缺失）
	ensureCriticalColumns(db)

	// 读取当前已迁移版本
	var storedVer string
	db.Raw("SELECT ver FROM ink_schema_version WHERE id = 1").Scan(&storedVer)
	if storedVer == schemaVersion {
		logger.Printf("autoMigrate: schema version %s already up-to-date, skipping", schemaVersion)
		return nil
	}

	logger.Printf("autoMigrate: migrating schema %s → %s", storedVer, schemaVersion)
	preMigrateCleanup(db)
	// 禁用外键约束创建：避免手动加列类型不匹配、循环依赖等问题
	// AutoMigrate 只负责同步列定义，外键由应用层保证一致性
	db.DisableForeignKeyConstraintWhenMigrating = true
	if err := db.AutoMigrate(
		&model.Tenant{},
		&model.User{},
		&model.TenantUser{},
		&model.Novel{},
		&model.NovelVideoConfig{},
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
		&model.AIModel{},
		&model.ModelProvider{},
		&model.TaskModelConfig{},
		&model.ModelComparisonExperiment{},
		&model.ExperimentResult{},
		&model.ModelUsageLog{},
		&model.Video{},
		&model.StoryboardShot{},
		&model.QualityReport{},
		&model.ChapterVersion{},
		&model.McpTool{},
		&model.ModelMcpBinding{},
		&model.ArcSummary{},
		&model.Item{},
		&model.ChapterItem{},
		&model.ChapterCharacter{},
		&model.Skill{},
		&model.AsyncTask{},
		&model.MediaAsset{},
		&model.HookChain{},
		&model.SatisfactionPoint{},
		&model.ConflictArc{},
		&model.SceneAnchor{},
		&model.SceneConsistencyLog{},
		&model.SystemSetting{},
		&model.ShotVoiceSegment{},
		&model.ShotSFXItem{},
		&model.VideoBGMSegment{},
		&model.RewriteProject{},
		&model.LiteraryAnalysis{},
		&model.RewriteBible{},
		&model.ChapterRewriteTask{},
		&model.PlatformAccount{},
		&model.VideoPublishRecord{},
		// Asset Library (Phase 3)
		&model.Asset{},
		&model.Tag{},
		&model.AssetTagMap{},
		&model.AssetShareRequest{},
		&model.AssetVersion{},
		&model.AssetCollection{},
		&model.AssetCollectionItem{},
		&model.CrawlJob{},
		&model.AssetLike{},
		&model.AssetUsage{},
		&model.AssetComment{},
		&model.ShareLink{},
		&model.AssetRequest{},
		&model.SearchLog{},
		&model.AssetStorageQuota{},
		// 广场社交
		&model.VideoLike{},
		&model.VideoComment{},
		&model.NovelLike{},
		&model.NovelComment{},
	); err != nil {
		return err
	}

	// 迁移成功后写入新版本号（UPSERT）
	return db.Exec("INSERT INTO ink_schema_version (id, ver) VALUES (1, ?) ON DUPLICATE KEY UPDATE ver = ?",
		schemaVersion, schemaVersion).Error
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
		logger.Printf("Warning: Redis connection failed: %v", err)
		return nil
	}

	logger.Println("Redis connected successfully")
	return client
}

// initAIModule 初始化AI模块（兜底层）
// 生产环境：租户通过模型管理页面配置各自的 AK/SK，env var 不需要设置。
// 开发/测试：设置 OPENAI_API_KEY 等 env var 可跳过 DB 配置直接使用。
// 仅注册 key 非空的 provider，避免用空 key 发起 API 请求返回 401。
func initAIModule(cfg *config.Config) *ai.ModelManager {
	manager := ai.NewModelManager()
	firstRegistered := ""

	type providerDef struct {
		name     string
		key      string
		endpoint string
		model    string
		factory  func(key, endpoint, model string) ai.AIProvider
	}
	// imageProviderModels 记录各提供者用于图像生成的模型和尺寸
	type imageProviderMeta struct{ model, size string }
	imageProviders := map[string]imageProviderMeta{
		"openai":  {"dall-e-3", "1024x1024"},
		"doubao":  {"seedream-3-0-t2i-250415", "1024x1024"},
		"qianwen": {"wanx2.1-t2i-turbo", "1024x1024"},
	}

	// env var 优先，config.yaml 作为备选（两者均可配置 API key）
	defs := []providerDef{
		{"openai", getEnv("OPENAI_API_KEY", cfg.AI.OpenAI.APIKey), cfg.AI.OpenAI.Endpoint, cfg.AI.OpenAI.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewOpenAIProvider(k, e, m, 0) }},
		{"anthropic", getEnv("ANTHROPIC_API_KEY", cfg.AI.Anthropic.APIKey), cfg.AI.Anthropic.Endpoint, cfg.AI.Anthropic.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewAnthropicProvider(k, e, m, 0) }},
		{"google", getEnv("GOOGLE_API_KEY", cfg.AI.Google.APIKey), cfg.AI.Google.Endpoint, cfg.AI.Google.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewGoogleProvider(k, e, m, 0) }},
		{"doubao", getEnv("DOUBAO_API_KEY", cfg.AI.Doubao.APIKey), cfg.AI.Doubao.Endpoint, cfg.AI.Doubao.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewDoubaoProvider(k, e, m, 0) }},
		{"deepseek", getEnv("DEEPSEEK_API_KEY", cfg.AI.DeepSeek.APIKey), cfg.AI.DeepSeek.Endpoint, cfg.AI.DeepSeek.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewDeepSeekProvider(k, e, m, 0) }},
		{"qianwen", getEnv("QIANWEN_API_KEY", cfg.AI.Qianwen.APIKey), cfg.AI.Qianwen.Endpoint, cfg.AI.Qianwen.Model,
			func(k, e, m string) ai.AIProvider { return ai.NewQianwenProvider(k, e, m, 0) }},
	}
	for _, d := range defs {
		if d.key == "" {
			continue
		}
		manager.RegisterProvider(d.name, d.factory(d.key, d.endpoint, d.model))
		if firstRegistered == "" {
			firstRegistered = d.name
		}
		// 注册图像生成能力（仅当该 provider 实际可用时）
		if meta, ok := imageProviders[d.name]; ok {
			manager.RegisterImageProvider(d.name, meta.model, meta.size)
		}
	}
	if firstRegistered != "" {
		manager.SetDefault(firstRegistered)
	}
	if len(manager.ListProviders()) == 0 {
		logger.Println("initAIModule: no AI API keys in env — providers will be loaded from DB per-tenant")
	}

	// Ollama 本地 LLM（无需 API Key，endpoint 非空即注册）
	// 设置 OLLAMA_ENDPOINT 或在 config.yaml 中配置 ai.ollama.endpoint
	ollamaEndpoint := getEnv("OLLAMA_ENDPOINT", cfg.AI.Ollama.Endpoint)
	if ollamaEndpoint != "" {
		ollamaModel := getEnv("OLLAMA_MODEL", cfg.AI.Ollama.Model)
		manager.RegisterProvider("ollama", ai.NewOllamaProvider(ollamaEndpoint, ollamaModel, 0))
		if firstRegistered == "" {
			firstRegistered = "ollama"
		}
		logger.Printf("initAIModule: registered ollama at %s (model=%s)", ollamaEndpoint, ollamaModel)
	}

	// 即梦AI Visual API（AK/SK 鉴权图像生成）
	if vvp := initVolcengineVisual(cfg); vvp != nil {
		manager.RegisterProvider("volcengine-visual", vvp)
		manager.RegisterImageProvider("volcengine-visual", ai.VolcModelText2ImgV3, "1328x1328")
	}

	// 豆包语音合成 V3（openspeech.bytedance.com，X-Api-Key 鉴权，支持 seed-tts-2.0）
	if speechKey := getEnv("DOUBAO_SPEECH_API_KEY", ""); speechKey != "" {
		resourceID := getEnv("DOUBAO_SPEECH_RESOURCE_ID", "")
		manager.RegisterProvider("doubao-speech", ai.NewDoubaoSpeechProvider(speechKey, resourceID))
	}

	// 豆包语音合成 V1（HTTP 一次性合成，appid+access_token 鉴权，火山引擎老版控制台）
	if v1AppID := getEnv("DOUBAO_SPEECH_V1_APP_ID", ""); v1AppID != "" {
		v1Token := getEnv("DOUBAO_SPEECH_V1_TOKEN", "")
		v1Cluster := getEnv("DOUBAO_SPEECH_V1_CLUSTER", "")
		manager.RegisterProvider("doubao-speech-v1", ai.NewDoubaoSpeechV1Provider(v1AppID, v1Token, v1Cluster))
	}

	// 为所有 Provider 包装指数退避重试（最多 3 次，基础延迟 500ms）
	for _, name := range manager.ListProviders() {
		if err := manager.WrapWithRetry(name, 3, 500*time.Millisecond); err != nil {
			logger.Printf("Warning: failed to wrap provider %s with retry: %v", name, err)
		}
	}

	return manager
}

// initVideoProviders 初始化视频生成提供者
// 返回可用的 VideoProvider 列表，供视频服务按需选用
func initVideoProviders(cfg *config.Config) map[string]ai.VideoProvider {
	providers := make(map[string]ai.VideoProvider)

	// Kling 快手可灵（env var 优先，config.yaml ai.kling 作为备选）
	klingKey := getEnv("KLING_API_KEY", cfg.AI.Kling.APIKey)
	if klingKey != "" {
		providers["kling"] = ai.NewKlingProvider(klingKey, cfg.AI.Kling.Endpoint)
	}

	// Seedance 字节跳动火山引擎
	seedanceKey := getEnv("SEEDANCE_API_KEY", cfg.AI.Seedance.APIKey)
	if seedanceKey != "" {
		providers["seedance"] = ai.NewSeedanceProvider(seedanceKey, cfg.AI.Seedance.Endpoint)
	}

	logger.Printf("Initialized video providers: %d registered", len(providers))
	return providers
}

// initVolcengineVisual 初始化火山引擎即梦AI图像提供者（AK/SK 鉴权）
// env var 优先，config.yaml ai.volcengine_visual 作为备选
func initVolcengineVisual(cfg *config.Config) *ai.VolcengineVisualProvider {
	ak := getEnv("VOLCENGINE_ACCESS_KEY", cfg.AI.VolcengineVisual.AccessKey)
	sk := getEnv("VOLCENGINE_SECRET_KEY", cfg.AI.VolcengineVisual.SecretKey)
	if ak == "" || sk == "" {
		return nil
	}
	logger.Println("VolcengineVisual (即梦AI) provider initialized")
	return ai.NewVolcengineVisualProvider(ak, sk)
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

// Repositories 仓库层
type Repositories struct {
	NovelRepo               *repository.NovelRepository
	ChapterRepo             *repository.ChapterRepository
	CharacterRepo           *repository.CharacterRepository
	WorldviewRepo           *repository.WorldviewRepository
	AIModelRepo             *repository.AIModelRepository
	TaskModelConfigRepo     *repository.TaskModelConfigRepository
	VideoRepo               *repository.VideoRepository
	StoryboardRepo          *repository.StoryboardRepository
	KnowledgeBaseRepo       *repository.KnowledgeBaseRepository
	ModelProviderRepo       *repository.ModelProviderRepository
	ModelComparisonRepo     *repository.ModelComparisonRepository
	TaskRepo                *repository.TaskRepository

	ChapterVersionRepo      *repository.ChapterVersionRepository
	SnapshotRepo            *repository.CharacterStateSnapshotRepository
	UserRepo                *repository.UserRepository
	TenantRepo              *repository.TenantRepository
	TenantUserRepo          *repository.TenantUserRepository
	ArcSummaryRepo          *repository.ArcSummaryRepository
	ItemRepo                *repository.ItemRepository
	ChapterItemRepo         *repository.ChapterItemRepository
	ChapterCharacterRepo    *repository.ChapterCharacterRepository
	SkillRepo               *repository.SkillRepository
	PlotPointRepo           *repository.PlotPointRepository
	HookChainRepo           *repository.HookChainRepository
	SatisfactionPointRepo   *repository.SatisfactionPointRepository
	ConflictArcRepo         *repository.ConflictArcRepository
	SceneAnchorRepo         *repository.SceneAnchorRepository
	SceneConsistencyLogRepo *repository.SceneConsistencyLogRepository
	SystemSettingRepo       *repository.SystemSettingRepository
	ShotVoiceSegmentRepo    *repository.ShotVoiceSegmentRepository
	ShotSFXItemRepo         *repository.ShotSFXItemRepository
	VideoBGMSegmentRepo     *repository.VideoBGMSegmentRepository
	RewriteProjectRepo      *repository.RewriteProjectRepository
	LiteraryAnalysisRepo    *repository.LiteraryAnalysisRepository
	RewriteBibleRepo        *repository.RewriteBibleRepository
	ChapterRewriteTaskRepo  *repository.ChapterRewriteTaskRepository
	PlatformAccountRepo     *repository.PlatformAccountRepository
	VideoPublishRecordRepo  *repository.VideoPublishRecordRepository
	// Asset Library
	AssetRepo               *repository.AssetRepository
	TagRepo                 *repository.TagRepository
	AssetVersionRepo        *repository.AssetVersionRepository
	AssetCollectionRepo     *repository.AssetCollectionRepository
	AssetShareRequestRepo   *repository.AssetShareRequestRepository
	AssetUsageRepo          *repository.AssetUsageRepository
	AssetLikeRepo           *repository.AssetLikeRepository
	AssetCommentRepo        *repository.AssetCommentRepository
	CrawlJobRepo            *repository.CrawlJobRepository
	ShareLinkRepo           *repository.ShareLinkRepository
	SearchLogRepo           *repository.SearchLogRepository
	TenantQuotaRepo         *repository.AssetStorageQuotaRepository
	VideoLikeRepo           *repository.VideoLikeRepository
	VideoCommentRepo        *repository.VideoCommentRepository
	NovelLikeRepo           *repository.NovelLikeRepository
	NovelCommentRepo        *repository.NovelCommentRepository
}

// initRepositories 初始化仓库层
func initRepositories(db *gorm.DB, redis *redis.Client) *Repositories {
	return &Repositories{
		NovelRepo:               repository.NewNovelRepository(db, redis),
		ChapterRepo:             repository.NewChapterRepository(db, redis),
		CharacterRepo:           repository.NewCharacterRepository(db),
		WorldviewRepo:           repository.NewWorldviewRepository(db),
		AIModelRepo:             repository.NewAIModelRepository(db),
		TaskModelConfigRepo:     repository.NewTaskModelConfigRepository(db),
		VideoRepo:               repository.NewVideoRepository(db),
		StoryboardRepo:          repository.NewStoryboardRepository(db),
		KnowledgeBaseRepo:       repository.NewKnowledgeBaseRepository(db),
		ModelProviderRepo:       repository.NewModelProviderRepository(db),
		ModelComparisonRepo:     repository.NewModelComparisonRepository(db),
		TaskRepo:                repository.NewTaskRepository(db),

		ChapterVersionRepo:      repository.NewChapterVersionRepository(db),
		SnapshotRepo:            repository.NewCharacterStateSnapshotRepository(db),
		UserRepo:                repository.NewUserRepository(db),
		TenantRepo:              repository.NewTenantRepository(db),
		TenantUserRepo:          repository.NewTenantUserRepository(db),
		ArcSummaryRepo:          repository.NewArcSummaryRepository(db),
		ItemRepo:                repository.NewItemRepository(db),
		ChapterItemRepo:         repository.NewChapterItemRepository(db),
		ChapterCharacterRepo:    repository.NewChapterCharacterRepository(db),
		SkillRepo:               repository.NewSkillRepository(db),
		PlotPointRepo:           repository.NewPlotPointRepository(db),
		HookChainRepo:           repository.NewHookChainRepository(db),
		SatisfactionPointRepo:   repository.NewSatisfactionPointRepository(db),
		ConflictArcRepo:         repository.NewConflictArcRepository(db),
		SceneAnchorRepo:         repository.NewSceneAnchorRepository(db),
		SceneConsistencyLogRepo: repository.NewSceneConsistencyLogRepository(db),
		SystemSettingRepo:       repository.NewSystemSettingRepository(db),
		ShotVoiceSegmentRepo:    repository.NewShotVoiceSegmentRepository(db),
		ShotSFXItemRepo:         repository.NewShotSFXItemRepository(db),
		VideoBGMSegmentRepo:     repository.NewVideoBGMSegmentRepository(db),
		RewriteProjectRepo:      repository.NewRewriteProjectRepository(db, redis),
		LiteraryAnalysisRepo:    repository.NewLiteraryAnalysisRepository(db),
		RewriteBibleRepo:        repository.NewRewriteBibleRepository(db),
		ChapterRewriteTaskRepo:  repository.NewChapterRewriteTaskRepository(db),
		PlatformAccountRepo:     repository.NewPlatformAccountRepository(db),
		VideoPublishRecordRepo:  repository.NewVideoPublishRecordRepository(db),
		// Asset Library
		AssetRepo:             repository.NewAssetRepository(db),
		TagRepo:               repository.NewTagRepository(db),
		AssetVersionRepo:      repository.NewAssetVersionRepository(db),
		AssetCollectionRepo:   repository.NewAssetCollectionRepository(db),
		AssetShareRequestRepo: repository.NewAssetShareRequestRepository(db),
		AssetUsageRepo:        repository.NewAssetUsageRepository(db),
		AssetLikeRepo:         repository.NewAssetLikeRepository(db),
		AssetCommentRepo:      repository.NewAssetCommentRepository(db),
		CrawlJobRepo:          repository.NewCrawlJobRepository(db),
		ShareLinkRepo:         repository.NewShareLinkRepository(db),
		SearchLogRepo:         repository.NewSearchLogRepository(db),
		TenantQuotaRepo:       repository.NewAssetStorageQuotaRepository(db),
		VideoLikeRepo:         repository.NewVideoLikeRepository(db),
		VideoCommentRepo:      repository.NewVideoCommentRepository(db),
		NovelLikeRepo:         repository.NewNovelLikeRepository(db),
		NovelCommentRepo:      repository.NewNovelCommentRepository(db),
	}
}

// Services 服务层
type Services struct {
	NovelAnalysisService        *service.NovelAnalysisService
	McpService                  *service.McpService
	NovelService                *service.NovelService
	ChapterService              *service.ChapterService
	CharacterService            *service.CharacterService
	WorldviewService            *service.WorldviewService
	QualityControlService       *service.QualityControlService
	VideoService                *service.VideoService
	ModelService                *service.ModelService
	PromptService               *service.PromptService
	ContinuityService           *service.ContinuityService
	KnowledgeService            *service.KnowledgeService

	ChapterVersionService       *service.ChapterVersionService
	ForeshadowService           *service.ForeshadowService
	TimelineService             *service.TimelineService
	CharacterArcService         *service.CharacterArcService
	StyleService                *service.StyleService
	GenerationContextService    *service.GenerationContextService
	ImageGenerationService      *service.ImageGenerationService
	StoryboardService           *service.StoryboardService
	VideoEnhancementService     *service.VideoEnhancementService
	CharacterConsistencyService *service.CharacterConsistencyService
	FrameGeneratorService       *service.FrameGeneratorService
	ConsistencyValidatorService *service.ConsistencyValidatorService
	BGMService                  *service.BGMService
	SFXService                  *service.SFXService
	CrawlerService              *crawler.NovelCrawler
	NovelImportService          *service.NovelImportService
	NovelToVideoService         *service.NovelToVideoService
	AuthService                 *service.AuthService
	TenantService               *service.TenantService
	SMSService                  *service.SMSService
	OAuthService                *service.OAuthService
	FrontendURL                 string
	ItemService                 *service.ItemService
	SkillService                *service.SkillService
	PlotPointService            *service.PlotPointService
	TaskService                 *service.TaskService
	AIService                   *service.AIService
	HookChainService            *service.HookChainService
	SatisfactionPointService    *service.SatisfactionPointService
	ConflictArcService          *service.ConflictArcService
	PacingService               *service.PacingService
	SceneAnchorService          *service.SceneAnchorService
	SceneConsistencyService     *service.SceneConsistencyService
	RewriteService              *service.RewriteService
	PlatformPublishService      *service.PlatformPublishService
	AssetService                *service.AssetService
}

// ──────────────────────────────────────────────────────────────
// Service group structs  (intermediate, only used during init)
// ──────────────────────────────────────────────────────────────

// coreSvcs holds foundational AI/model services that all other groups depend on.
type coreSvcs struct {
	AI        *service.AIService
	Model     *service.ModelService
	Task      *service.TaskService
	PlotPoint *service.PlotPointService
	Quality   *service.QualityControlService
}

// contentSvcs holds novel/chapter/character domain services.
type contentSvcs struct {
	Novel             *service.NovelService
	Chapter           *service.ChapterService
	Character         *service.CharacterService
	Worldview         *service.WorldviewService
	Knowledge         *service.KnowledgeService
	Continuity        *service.ContinuityService
	Prompt            *service.PromptService
	ChapterVersion    *service.ChapterVersionService
	Foreshadow        *service.ForeshadowService
	Timeline          *service.TimelineService
	CharacterArc      *service.CharacterArcService
	Style             *service.StyleService
	GenContext        *service.GenerationContextService
	ImageGen          *service.ImageGenerationService
	HookChain         *service.HookChainService
	SatisfactionPoint *service.SatisfactionPointService
	ConflictArc       *service.ConflictArcService
	Pacing            *service.PacingService
	Item              *service.ItemService
	Skill             *service.SkillService
	SceneAnchor       *service.SceneAnchorService
	NovelAnalysis     *service.NovelAnalysisService
	NovelImport       *service.NovelImportService
	Crawler           *crawler.NovelCrawler
}

// videoSvcs holds video / media generation services.
type videoSvcs struct {
	Video                *service.VideoService
	Storyboard           *service.StoryboardService
	Enhancement          *service.VideoEnhancementService
	CharConsistency      *service.CharacterConsistencyService
	BGM                  *service.BGMService
	FrameGenerator       *service.FrameGeneratorService
	ConsistencyValidator *service.ConsistencyValidatorService
	NovelToVideo         *service.NovelToVideoService
	SceneConsistency     *service.SceneConsistencyService
}

// ──────────────────────────────────────────────────────────────
// Group initializers
// ──────────────────────────────────────────────────────────────

func initCoreServiceGroup(repos *Repositories, aiManager *ai.ModelManager, cfg *config.Config) *coreSvcs {
	// AI服务（注入 providerRepo 以支持按租户加载 AK/SK，注入 novelRepo 以读取小说项目级 AI 配置）
	aiSvc := service.NewAIService(repos.AIModelRepo, repos.TaskModelConfigRepo, aiManager, repos.ModelProviderRepo).
		WithNovelRepo(repos.NovelRepo).
		WithTaskRouting(service.TaskRouting{
			ChapterGen:   cfg.AI.Tasks.ChapterGen,
			QualityCheck: cfg.AI.Tasks.QualityCheck,
			TTS:          cfg.AI.Tasks.TTS,
			ImageGen:     cfg.AI.Tasks.ImageGen,
			VideoGen:     cfg.AI.Tasks.VideoGen,
			Embedding:    cfg.AI.Tasks.Embedding,
		}).
		WithImageConcurrency(cfg.AI.ImageConcurrency)

	// 模型服务（注入 aiService 以支持 TestProvider 实例化验证）
	modelSvc := service.NewModelService(repos.AIModelRepo, repos.ModelProviderRepo, repos.TaskModelConfigRepo, repos.ModelComparisonRepo, aiSvc)
	modelSvc.SeedAllProviders()

	// 异步任务服务
	taskSvc := service.NewTaskService(repos.TaskRepo)

	// 剧情点服务
	plotPointSvc := service.NewPlotPointService(repos.PlotPointRepo, aiSvc).
		WithChapterRepo(repos.ChapterRepo)

	// 质量控制服务
	qualitySvc := service.NewQualityControlService(aiSvc, repos.ChapterRepo, repos.NovelRepo)

	return &coreSvcs{AI: aiSvc, Model: modelSvc, Task: taskSvc, PlotPoint: plotPointSvc, Quality: qualitySvc}
}

func initContentServiceGroup(repos *Repositories, core *coreSvcs, aiManager *ai.ModelManager, vectorStore *vector.StoreManager) *contentSvcs {
	aiSvc := core.AI

	// 小说服务
	novelSvc := service.NewNovelService(repos.NovelRepo, repos.ChapterRepo, aiSvc).
		WithCharacterRepos(repos.CharacterRepo, repos.SnapshotRepo).
		WithPlotPointService(core.PlotPoint)

	// 角色服务
	characterSvc := service.NewCharacterService(repos.CharacterRepo, aiSvc).
		WithChapterCharacterRepo(repos.ChapterCharacterRepo).
		WithNovelRepo(repos.NovelRepo).
		WithChapterRepo(repos.ChapterRepo)

	// 世界观服务
	worldviewSvc := service.NewWorldviewService(repos.WorldviewRepo, aiSvc).
		WithNovelRepos(repos.NovelRepo, repos.ChapterRepo)

	// 提示词 / 连续性
	promptSvc := service.NewPromptService(nil)
	continuitySvc := service.NewContinuityService(repos.CharacterRepo, repos.ChapterRepo)

	// 知识库服务（传入 AI provider 用于向量化）
	var defaultAIProvider ai.AIProvider
	if aiManager != nil {
		if p, err := aiManager.GetProvider(""); err == nil {
			defaultAIProvider = p
		} else {
			logger.Printf("Warning: could not load default AI provider: %v — knowledge base embedding will be unavailable", err)
		}
	}
	if defaultAIProvider == nil {
		logger.Printf("Warning: no default AI provider available; knowledge base embedding disabled")
	}
	knowledgeSvc := service.NewKnowledgeService(repos.KnowledgeBaseRepo, vectorStore, defaultAIProvider)

	// 章节版本 / 伏笔 / 时间线 / 角色弧光 / 风格
	chapterVersionSvc := service.NewChapterVersionService(repos.ChapterVersionRepo, repos.ChapterRepo)
	foreshadowSvc := service.NewForeshadowService(repos.KnowledgeBaseRepo, aiSvc)
	timelineSvc := service.NewTimelineService(repos.ChapterRepo)
	characterArcSvc := service.NewCharacterArcService(repos.CharacterRepo, repos.SnapshotRepo)
	styleSvc := service.NewStyleService(nil)

	// 生成上下文服务
	genCtxSvc := service.NewGenerationContextService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, characterArcSvc, foreshadowSvc)

	// 层次化叙事记忆服务
	narrativeMemorySvc := service.NewNarrativeMemoryService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, repos.ArcSummaryRepo, aiSvc)

	// 戏剧张力服务
	hookChainSvc := service.NewHookChainService(repos.HookChainRepo)
	satisfactionSvc := service.NewSatisfactionPointService(repos.SatisfactionPointRepo)
	conflictArcSvc := service.NewConflictArcService(repos.ConflictArcRepo)
	pacingSvc := service.NewPacingService(repos.ChapterRepo, repos.SatisfactionPointRepo)

	// 章节服务（依赖 genCtxSvc + narrativeMemorySvc）
	chapterSvc := service.NewChapterService(repos.ChapterRepo, repos.NovelRepo, aiSvc, genCtxSvc).
		WithNarrativeMemory(narrativeMemorySvc).
		WithDramaticServices(hookChainSvc, satisfactionSvc, conflictArcSvc).
		WithPlotPointRepo(repos.PlotPointRepo)

	// 图像生成服务
	imageGenSvc := service.NewImageGenerationService(aiSvc)

	// 物品 / 技能 / 场景锚点
	itemSvc := service.NewItemService(repos.ItemRepo, repos.ChapterItemRepo, repos.ChapterRepo, aiSvc).
		WithNovelRepo(repos.NovelRepo)
	skillSvc := service.NewSkillService(repos.SkillRepo, repos.CharacterRepo, repos.NovelRepo, aiSvc).
		WithChapterRepo(repos.ChapterRepo)
	sceneAnchorSvc := service.NewSceneAnchorService(repos.SceneAnchorRepo, repos.StoryboardRepo, aiSvc, repos.NovelRepo).
		WithChapterRepo(repos.ChapterRepo)

	// 小说分析服务（依赖大部分上面的服务）
	novelAnalysisSvc := service.NewNovelAnalysisService(repos.NovelRepo, repos.ChapterRepo, repos.CharacterRepo, repos.WorldviewRepo, novelSvc, aiSvc).
		WithItemRepo(repos.ItemRepo).
		WithItemService(itemSvc).
		WithSkillService(skillSvc).
		WithPlotPointService(core.PlotPoint).
		WithSceneAnchorService(sceneAnchorSvc).
		WithTaskService(core.Task)

	// 导入服务
	crawlerSvc := crawler.NewNovelCrawler(nil)
	novelImportSvc := service.NewNovelImportService(repos.NovelRepo, repos.ChapterRepo, crawlerSvc).
		WithNarrativeMemory(narrativeMemorySvc)

	return &contentSvcs{
		Novel: novelSvc, Chapter: chapterSvc, Character: characterSvc, Worldview: worldviewSvc,
		Knowledge: knowledgeSvc, Continuity: continuitySvc, Prompt: promptSvc,
		ChapterVersion: chapterVersionSvc, Foreshadow: foreshadowSvc, Timeline: timelineSvc,
		CharacterArc: characterArcSvc, Style: styleSvc, GenContext: genCtxSvc,
		ImageGen: imageGenSvc, HookChain: hookChainSvc, SatisfactionPoint: satisfactionSvc,
		ConflictArc: conflictArcSvc, Pacing: pacingSvc, Item: itemSvc, Skill: skillSvc,
		SceneAnchor: sceneAnchorSvc, NovelAnalysis: novelAnalysisSvc, NovelImport: novelImportSvc,
		Crawler: crawlerSvc,
	}
}

func initVideoServiceGroup(repos *Repositories, core *coreSvcs, content *contentSvcs, cfg *config.Config) *videoSvcs {
	aiSvc := core.AI

	// 视频服务
	videoProviders := initVideoProviders(cfg)
	videoSvc := service.NewVideoService(repos.VideoRepo, repos.StoryboardRepo, repos.ChapterRepo, repos.CharacterRepo, repos.NovelRepo, repos.TenantRepo, aiSvc, videoProviders)

	// 图像服务（内部，用于视频增强和一致性服务）
	imageSvc := service.NewImageService(nil)

	// 分镜 / 视频增强 / BGM / 角色一致性
	intelligentStoryboardSvc := service.NewIntelligentStoryboardService(aiSvc, imageSvc)
	storyboardSvc := service.NewStoryboardService(videoSvc, aiSvc)
	enhancementSvc := service.NewVideoEnhancementService(imageSvc, "/tmp/inkframe-enhance")
	bgmSvc := service.NewBGMService(getEnv("BGM_DIR", cfg.BGM.Dir)).
		WithAIService(aiSvc).
		WithJamendo(getEnv("JAMENDO_CLIENT_ID", cfg.BGM.JamendoClientID)).
		WithPixabay(getEnv("PIXABAY_API_KEY", cfg.BGM.PixabayKey))

	charConsistencySvc := service.NewCharacterConsistencyService(imageSvc, nil, aiSvc)

	// 将依赖注回 videoService
	videoSvc.WithConsistencyService(charConsistencySvc)
	videoSvc.WithBGMService(bgmSvc)
	videoSvc.WithPlotPointRepo(repos.PlotPointRepo)
	videoSvc.WithSystemSettingRepo(repos.SystemSettingRepo)
	videoSvc.WithVideoConcurrency(cfg.AI.VideoConcurrency)
	videoSvc.WithAudioConcurrency(3)

	// 帧生成 / 一致性验证 / 小说转视频
	frameGenSvc := service.NewFrameGeneratorService(aiSvc)
	consistencyValidatorSvc := service.NewConsistencyValidatorService(aiSvc)
	novelToVideoSvc := service.NewNovelToVideoService(
		content.NovelImport,
		intelligentStoryboardSvc,
		frameGenSvc,
		enhancementSvc,
		consistencyValidatorSvc,
		repos.NovelRepo,
		repos.ChapterRepo,
		repos.VideoRepo,
		repos.StoryboardRepo,
	)

	sceneConsistencySvc := service.NewSceneConsistencyService(repos.SceneConsistencyLogRepo, aiSvc)

	return &videoSvcs{
		Video: videoSvc, Storyboard: storyboardSvc, Enhancement: enhancementSvc,
		CharConsistency: charConsistencySvc, BGM: bgmSvc, FrameGenerator: frameGenSvc,
		ConsistencyValidator: consistencyValidatorSvc,
		NovelToVideo: novelToVideoSvc, SceneConsistency: sceneConsistencySvc,
	}
}


// initServices 初始化服务层
func initServices(db *gorm.DB, repos *Repositories, aiManager *ai.ModelManager, vectorStore *vector.StoreManager, cfg *config.Config, redisClient *redis.Client) *Services {
	core    := initCoreServiceGroup(repos, aiManager, cfg)
	content := initContentServiceGroup(repos, core, aiManager, vectorStore)
	video   := initVideoServiceGroup(repos, core, content, cfg)

	// 改写服务
	rewriteSvc := service.NewRewriteService(
		repos.RewriteProjectRepo,
		repos.LiteraryAnalysisRepo,
		repos.RewriteBibleRepo,
		repos.ChapterRewriteTaskRepo,
		repos.ChapterRepo,
		repos.NovelRepo,
		core.AI,
	)

	// 认证 / 租户 / 通信服务（依赖 db 和 redisClient，数量少，直接内联）
	smsSvc := service.NewSMSService(redisClient, cfg.SMS)
	authSvc := service.NewAuthService(db, repos.UserRepo, repos.TenantRepo, repos.TenantUserRepo, cfg.Server.JWTSecret, cfg.Server.JWTExpiry).
		WithSMSService(smsSvc)
	tenantSvc := service.NewTenantService(repos.TenantRepo, repos.TenantUserRepo)
	oauthSvc  := service.NewOAuthService(cfg.OAuth)
	mcpSvc    := service.NewMcpService(db)

	return &Services{
		// ── AI core ──
		AIService:             core.AI,
		ModelService:          core.Model,
		TaskService:           core.Task,
		PlotPointService:      core.PlotPoint,
		QualityControlService: core.Quality,
		// ── Content ──
		NovelService:          content.Novel,
		ChapterService:        content.Chapter,
		CharacterService:      content.Character,
		WorldviewService:      content.Worldview,
		KnowledgeService:      content.Knowledge,
		ContinuityService:     content.Continuity,
		PromptService:         content.Prompt,
		ChapterVersionService: content.ChapterVersion,
		ForeshadowService:     content.Foreshadow,
		TimelineService:       content.Timeline,
		CharacterArcService:   content.CharacterArc,
		StyleService:          content.Style,
		GenerationContextService: content.GenContext,
		ImageGenerationService:   content.ImageGen,
		HookChainService:         content.HookChain,
		SatisfactionPointService: content.SatisfactionPoint,
		ConflictArcService:       content.ConflictArc,
		PacingService:            content.Pacing,
		ItemService:              content.Item,
		SkillService:             content.Skill,
		SceneAnchorService:       content.SceneAnchor,
		NovelAnalysisService:     content.NovelAnalysis,
		NovelImportService:       content.NovelImport,
		CrawlerService:           content.Crawler,
		// ── Video ──
		VideoService:                video.Video,
		StoryboardService:           video.Storyboard,
		VideoEnhancementService:     video.Enhancement,
		CharacterConsistencyService: video.CharConsistency,
		BGMService:                  video.BGM,
		FrameGeneratorService:       video.FrameGenerator,
		ConsistencyValidatorService: video.ConsistencyValidator,
		NovelToVideoService:         video.NovelToVideo,
		SceneConsistencyService:     video.SceneConsistency,
		// ── Auth / platform ──
		AuthService:   authSvc,
		TenantService: tenantSvc,
		SMSService:    smsSvc,
		OAuthService:  oauthSvc,
		McpService:    mcpSvc,
		FrontendURL:   cfg.Server.FrontendURL,
		// ── Rewrite ──
		RewriteService: rewriteSvc,
		// ── Platform publish ──
		PlatformPublishService: service.NewPlatformPublishService(
			repos.PlatformAccountRepo,
			repos.VideoPublishRecordRepo,
			core.Task,
		),
		// ── Asset Library ──
		AssetService: service.NewAssetService(
			repos.AssetRepo,
			repos.TagRepo,
			repos.AssetVersionRepo,
			repos.AssetCollectionRepo,
			repos.AssetShareRequestRepo,
			repos.AssetUsageRepo,
			repos.AssetLikeRepo,
			repos.AssetCommentRepo,
			repos.CrawlJobRepo,
			repos.ShareLinkRepo,
			repos.SearchLogRepo,
			repos.TenantQuotaRepo,
			core.Task,
		),
	}
}

// Handlers 处理器
type Handlers struct {
	NovelHandler       *handler.NovelHandler
	ChapterHandler     *handler.ChapterHandler
	CharacterHandler   *handler.CharacterHandler
	VideoHandler       *handler.VideoHandler
	ModelHandler       *handler.ModelHandler
	McpHandler         *handler.McpHandler
	StyleHandler       *handler.StyleHandler
	ContextHandler     *handler.ContextHandler
	AuthHandler        *handler.AuthHandler
	ImportHandler      *handler.ImportHandler
	WorldviewHandler   *handler.WorldviewHandler
	TenantHandler      *handler.TenantHandler
	ItemHandler        *handler.ItemHandler
	SkillHandler       *handler.SkillHandler
	UploadHandler      *handler.UploadHandler
	PlotPointHandler   *handler.PlotPointHandler
	TaskHandler        *handler.TaskHandler
	MediaHandler       *handler.MediaHandler
	SceneAnchorHandler *handler.SceneAnchorHandler
	SystemHandler      *handler.SystemHandler
	FsHandler          *handler.FsHandler
	RewriteHandler     *handler.RewriteHandler
	PlatformHandler    *handler.PlatformHandler
	AssetHandler       *handler.AssetHandler
}

// initHandlers 初始化处理器
func initHandlers(services *Services, storageSvc storage.Service, db *gorm.DB, repos *Repositories) *Handlers {
	return &Handlers{
		NovelHandler: handler.NewNovelHandler(
			services.NovelService,
			services.ChapterService,
			services.ForeshadowService,
			services.TimelineService,
			services.QualityControlService,
		).WithTaskService(services.TaskService).WithModelService(services.ModelService),
		ChapterHandler: handler.NewChapterHandler(
			services.ChapterService,
			services.ChapterVersionService,
			services.QualityControlService,
			services.TaskService,
		),
		CharacterHandler: handler.NewCharacterHandler(
			services.CharacterService,
			services.CharacterArcService,
			services.ImageGenerationService,
		).WithChapterService(services.ChapterService).WithStorage(storageSvc).WithTaskService(services.TaskService).WithAIService(services.AIService),
		VideoHandler: handler.NewVideoHandler(
			services.VideoService,
			services.StoryboardService,
			services.VideoEnhancementService,
			services.CharacterConsistencyService,
		).WithTaskService(services.TaskService).WithSFXService(services.SFXService).WithSFXItemRepo(repos.ShotSFXItemRepo).
			WithBGMService(services.BGMService).WithBGMRepo(repos.VideoBGMSegmentRepo).
			WithSubtitleService(service.NewSubtitleService()),
		ModelHandler:   handler.NewModelHandler(services.ModelService),
		McpHandler:     handler.NewMcpHandler(services.McpService),
		StyleHandler:   handler.NewStyleHandler(services.StyleService),
		ContextHandler: handler.NewContextHandler(services.GenerationContextService),
		AuthHandler: handler.NewAuthHandler(services.AuthService).
			WithSMSService(services.SMSService).
			WithOAuthService(services.OAuthService).
			WithFrontendURL(services.FrontendURL),
		ImportHandler: handler.NewImportHandler(services.NovelImportService, services.NovelToVideoService).
			SetAnalysisService(services.NovelAnalysisService).
			WithTaskService(services.TaskService).
			WithNovelService(services.NovelService),
		WorldviewHandler:   handler.NewWorldviewHandler(services.WorldviewService),
		TenantHandler:      handler.NewTenantHandler(services.TenantService),
		ItemHandler:        handler.NewItemHandler(services.ItemService, services.ChapterService).WithStorage(storageSvc).WithTaskService(services.TaskService).WithNovelService(services.NovelService),
		SkillHandler:       handler.NewSkillHandler(services.SkillService).WithChapterService(services.ChapterService).WithNovelService(services.NovelService),
		UploadHandler:      handler.NewUploadHandler(storageSvc),
		PlotPointHandler:   handler.NewPlotPointHandler(services.PlotPointService).WithChapterService(services.ChapterService).WithTaskService(services.TaskService),
		TaskHandler:        handler.NewTaskHandler(services.TaskService),
		MediaHandler:       handler.NewMediaHandler(db),
		SceneAnchorHandler: handler.NewSceneAnchorHandler(services.SceneAnchorService, services.SceneConsistencyService).WithTaskService(services.TaskService).WithChapterService(services.ChapterService),
		SystemHandler: handler.NewSystemHandler(repos.SystemSettingRepo),
		FsHandler:     handler.NewFsHandler(),
		RewriteHandler: handler.NewRewriteHandler(services.RewriteService),
		PlatformHandler: handler.NewPlatformHandler(services.NovelService, services.VideoService, services.PlatformPublishService),
		AssetHandler:    handler.NewAssetHandler(services.AssetService),
	}
}

// seedConcurrencySettings 将并发度配置种子写入 DB（首次启动），或从 DB 加载并应用到运行时服务。
func seedConcurrencySettings(repo *repository.SystemSettingRepository, cfg *config.Config, aiSvc *service.AIService, videoSvc *service.VideoService) {
	seed := func(key, desc string, cfgVal int, apply func(int)) {
		if v, err := repo.Get(key); err == nil {
			// 已有记录：加载并应用（DB 优先于 config.yaml）
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				apply(n)
			}
		} else {
			// 首次：写入 config 默认值
			val := cfgVal
			if val <= 0 {
				val = 1
			}
			_ = repo.Set(key, strconv.Itoa(val), desc)
		}
	}
	seed("image_concurrency", "图像生成最大并发数", cfg.AI.ImageConcurrency, aiSvc.SetImageConcurrency)
	seed("video_concurrency", "视频生成最大并发数", cfg.AI.VideoConcurrency, videoSvc.SetVideoConcurrency)
}

// getEnv 获取环境变量
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
