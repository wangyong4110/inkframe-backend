package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/service"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Config 路由配置
type Config struct {
	JWTSecret        string
	AllowedOrigins   []string      // CORS 允许的来源列表；留空表示允许所有（开发模式）
	TrustedProxies   []string      // 受信任的反向代理 IP；留空时默认 ["127.0.0.1", "::1"]
	RedisClient      *redis.Client  // 可选，用于 JWT 黑名单检查
	DB               *gorm.DB       // 可选，用于 health check 和邮箱验证中间件
	AIService        *service.AIService // 可选，用于 /health AI 健康状态
	NovelHandler     *handler.NovelHandler
	ChapterHandler   *handler.ChapterHandler
	CharacterHandler *handler.CharacterHandler
	VideoHandler     *handler.VideoHandler
	ModelHandler     *handler.ModelHandler
	McpHandler       *handler.McpHandler
	StyleHandler     *handler.StyleHandler
	ContextHandler   *handler.ContextHandler
	AuthHandler      *handler.AuthHandler
	ImportHandler    *handler.ImportHandler
	WorldviewHandler *handler.WorldviewHandler
	TenantHandler    *handler.TenantHandler
	ItemHandler      *handler.ItemHandler
	SkillHandler     *handler.SkillHandler
	UploadHandler    *handler.UploadHandler
	PlotPointHandler *handler.PlotPointHandler
	TaskHandler        *handler.TaskHandler
	MediaHandler       *handler.MediaHandler
	SceneAnchorHandler *handler.SceneAnchorHandler
	SystemHandler      *handler.SystemHandler
	FsHandler          *handler.FsHandler
	RewriteHandler     *handler.RewriteHandler
	PlatformHandler    *handler.PlatformHandler
	AssetHandler       *handler.AssetHandler
	ImageHandler       *handler.ImageHandler
	WebSearchHandler       *handler.WebSearchHandler
	WikiSearchHandler      *handler.WikiSearchHandler
	StoryPatternHandler    *handler.StoryPatternHandler
	ImageRefSearchHandler  *handler.ImageRefSearchHandler
	ColorPaletteHandler    *handler.ColorPaletteHandler
	NotificationHandler    *handler.NotificationHandler
	KnowledgeHandler       *handler.KnowledgeHandler
	DramaticHandler        *handler.DramaticHandler
	DashboardHandler       *handler.DashboardHandler
	ForeshadowHandler      *handler.ForeshadowHandler
	WebhookHandler         *handler.WebhookHandler
	AuditHandler           *handler.AuditHandler
	OutlineReviewHandler   *handler.OutlineReviewHandler
	CollabHandler          *handler.CollabHandler
	SysAdminHandler        *handler.SysAdminHandler
	SensitiveWordHandler   *handler.SensitiveWordHandler
}

// SetupRouter 配置路由
func SetupRouter(cfg *Config) *gin.Engine {
	r := gin.New()

	// Configure trusted proxies so c.ClientIP() resolves the real client IP from
	// X-Forwarded-For only when the request originates from a trusted proxy.
	// This prevents clients from spoofing X-Forwarded-For to bypass rate limiting.
	trustedProxies := cfg.TrustedProxies
	if len(trustedProxies) == 0 {
		trustedProxies = []string{"127.0.0.1", "::1"}
	}
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		logger.Errorf("[Router] SetTrustedProxies: %v", err)
	}

	// 全局中间件
	r.Use(middleware.PrometheusMiddleware()) // 最前注册，覆盖所有路由
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.CORSMiddleware(cfg.AllowedOrigins))
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.MaxBodySize(1 * 1024 * 1024)) // 1MB for JSON; multipart/upload routes are excluded by middleware

	// Prometheus 指标端点（不经过 JWT 认证，供 Prometheus Scraper 抓取）
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// 健康检查（公开）
	r.GET("/health", func(c *gin.Context) {
		status := gin.H{"status": "ok", "db": "ok", "redis": "ok", "ai": "unknown"}
		httpStatus := http.StatusOK

		if cfg.DB != nil {
			sqlDB, err := cfg.DB.DB()
			if err != nil || sqlDB.Ping() != nil {
				status["db"] = "error"
				httpStatus = http.StatusServiceUnavailable
			}
		}
		if cfg.RedisClient != nil {
			if err := cfg.RedisClient.Ping(c.Request.Context()).Err(); err != nil {
				status["redis"] = "error"
				// Redis failure is non-critical, don't set 503
			}
		}
		if cfg.AIService != nil {
			aiStatus := cfg.AIService.GetOverallHealthStatus()
			status["ai"] = aiStatus
			if aiStatus == "down" {
				httpStatus = http.StatusServiceUnavailable
			}
		}
		c.JSON(httpStatus, status)
	})

	// 本地上传文件静态服务
	r.Static("/uploads", "./uploads")

	// 媒体素材下载（DB 存储后端；无需登录，可嵌入 <audio>/<img>）
	if cfg.MediaHandler != nil {
		r.GET("/api/v1/media/:id", cfg.MediaHandler.ServeMedia)
	}

	// 音频流端点（无需 JWT，供 <audio src="..."> 直接访问）
	if cfg.VideoHandler != nil {
		r.GET("/api/v1/sfx-items/:item_id/audio", cfg.VideoHandler.ServeSFXItemAudio)
		r.GET("/api/v1/videos/:id/shots/:shot_id/segments/:seg_id/audio", cfg.VideoHandler.ServeSegmentAudio)
		r.GET("/api/v1/videos/:id/storyboard/:shot_id/audio", cfg.VideoHandler.ServeAudio)
	}

	// 公开分享页（无需登录）
	if cfg.AssetHandler != nil {
		r.GET("/api/v1/share/:token", cfg.AssetHandler.PublicSharePage)
	}

	// 公开平台广场路由（无需 JWT，只限速）
	if cfg.PlatformHandler != nil {
		public := r.Group("/api/v1")
		public.Use(middleware.RateLimitWithRedis(cfg.RedisClient, 60, 10))
		public.GET("/platform/videos", cfg.PlatformHandler.GetPlatformFeed)
		public.GET("/platform/videos/:id", cfg.PlatformHandler.GetPlatformVideo)
		public.POST("/platform/videos/:id/view", cfg.PlatformHandler.RecordView)
		public.GET("/platform/videos/:id/comments", cfg.PlatformHandler.ListComments)
		public.GET("/platform/novels/ranking", cfg.PlatformHandler.GetNovelRanking)
		public.GET("/platform/novels", cfg.PlatformHandler.GetPlatformNovels)
		public.GET("/platform/novels/:id", cfg.PlatformHandler.GetPlatformNovel)
		public.POST("/platform/novels/:id/view", cfg.PlatformHandler.RecordNovelView)
		public.GET("/platform/novels/:id/chapters", cfg.PlatformHandler.GetNovelChapters)
		public.GET("/platform/novels/:id/chapters/:chapter_no", cfg.PlatformHandler.GetPublishedChapter)
		public.GET("/platform/novels/:id/chapters/:chapter_no/comments", cfg.PlatformHandler.ListChapterComments)
		public.GET("/platform/novels/:id/comments", cfg.PlatformHandler.ListNovelComments)
	}

	// 公开认证路由（不需要JWT，但需要限速防暴力破解）
	auth := r.Group("/api/v1/auth")
	auth.Use(middleware.RateLimitWithRedis(cfg.RedisClient, 10, 0.2)) // 10 burst, ~12 req/min
	{
		// Auth endpoints: stricter rate limit — 5 req/min per IP
		authRL := middleware.RateLimitAuth()
		auth.POST("/register", authRL, cfg.AuthHandler.Register)
		auth.POST("/login", authRL, cfg.AuthHandler.Login)
		auth.POST("/refresh", cfg.AuthHandler.RefreshToken)
		auth.POST("/sms/send", cfg.AuthHandler.SendSMSCode)
		auth.POST("/phone/register", cfg.AuthHandler.PhoneRegister)
		auth.POST("/phone/login", cfg.AuthHandler.PhoneLogin)
		auth.GET("/oauth/:provider/url", cfg.AuthHandler.OAuthURL)
		auth.GET("/oauth/:provider/callback", cfg.AuthHandler.OAuthCallback)
		// 密码重置（公开，无需登录）
		auth.POST("/password-reset/request", cfg.AuthHandler.RequestPasswordReset)
		auth.POST("/password-reset/confirm", cfg.AuthHandler.ResetPassword)
		// 邮箱验证回调（公开，链接中携带 token）
		auth.GET("/email-verification/verify", cfg.AuthHandler.VerifyEmail)
		// 重发验证邮件（公开，防枚举静默处理）
		auth.POST("/email-verification/resend", cfg.AuthHandler.ResendEmailVerification)
	}

	// 受保护路由（需要JWT）
	v1 := r.Group("/api/v1")
	v1.Use(middleware.RateLimitWithRedis(cfg.RedisClient, 240, 60))
	v1.Use(middleware.NewAuth(cfg.JWTSecret, cfg.RedisClient))
	v1.Use(middleware.CheckTenantSubscription(cfg.DB))
	{
		// 当前用户信息 & 资料管理
		v1.GET("/auth/me", cfg.AuthHandler.GetCurrentUser)
		v1.PUT("/auth/me", cfg.AuthHandler.UpdateProfile)
		v1.PUT("/auth/me/password", cfg.AuthHandler.ChangePassword)
		v1.DELETE("/auth/me", cfg.AuthHandler.DeleteAccount)
		// 邮箱验证发送（需要登录）
		v1.POST("/auth/email-verification/send", cfg.AuthHandler.SendEmailVerification)
		v1.POST("/auth/logout", cfg.AuthHandler.Logout)
		// 管理员解锁账号
		v1.POST("/auth/users/:id/unlock", cfg.AuthHandler.UnlockUser)

		// 统一异步任务
		if cfg.TaskHandler != nil {
			tasks := v1.Group("/tasks")
			{
				tasks.GET("", cfg.TaskHandler.ListTasks)
				tasks.GET("/:task_id", cfg.TaskHandler.GetTask)
				tasks.POST("/:task_id/cancel", cfg.TaskHandler.CancelTask)
			}
		}

		// 站内通知
		if cfg.NotificationHandler != nil {
			notifications := v1.Group("/notifications")
			{
				notifications.GET("", cfg.NotificationHandler.List)
				notifications.GET("/unread-count", cfg.NotificationHandler.UnreadCount)
				notifications.PUT("/read-all", cfg.NotificationHandler.MarkAllRead)
				notifications.PUT("/:id/read", cfg.NotificationHandler.MarkRead)
				notifications.DELETE("/:id", cfg.NotificationHandler.Delete)
			}
		}

		// 通用文件上传
		if cfg.UploadHandler != nil {
			v1.POST("/upload/image", cfg.UploadHandler.UploadImage)
			v1.POST("/upload/video", cfg.UploadHandler.UploadVideo)
		}

		// AI 对话式小说创建
		v1.POST("/ai/novel-chat", cfg.NovelHandler.NovelChat)
		v1.POST("/ai/novel-chat/stream", cfg.NovelHandler.NovelChatStream)

		// 导入
		importGroup := v1.Group("/import")
		{
			importGroup.POST("/novel", cfg.ImportHandler.ImportNovel)
			importGroup.POST("/novel/file", cfg.ImportHandler.ImportFromFile)
			importGroup.POST("/novel/file/init", cfg.ImportHandler.InitChunkedUpload)
			importGroup.PUT("/novel/file/chunk", cfg.ImportHandler.UploadChunk)
			importGroup.POST("/novel/file/complete", cfg.ImportHandler.CompleteChunkedUpload)
			importGroup.POST("/novel/url", cfg.ImportHandler.ImportFromURL)
			importGroup.POST("/novel/crawl", cfg.ImportHandler.ImportFromCrawl)
			importGroup.POST("/novel/video", cfg.ImportHandler.ImportAndGenerate)
		}

		// 小说相关
		novels := v1.Group("/novels")
		{
			novels.GET("", cfg.NovelHandler.ListNovels)
			novels.POST("", cfg.NovelHandler.CreateNovel)
			novels.GET("/:id", cfg.NovelHandler.GetNovel)
			novels.PUT("/:id", cfg.NovelHandler.UpdateNovel)
			novels.DELETE("/:id", cfg.NovelHandler.DeleteNovel)
			novels.GET("/:id/export", cfg.NovelHandler.ExportNovel)

			novels.POST("/:id/chapters/generate", cfg.NovelHandler.GenerateChapter)
			novels.POST("/:id/chapters/batch-generate", cfg.NovelHandler.BatchGenerateChapters)
			novels.POST("/:id/chapters/batch-review", cfg.ChapterHandler.BatchReviewChapters)
			novels.POST("/:id/outline", cfg.NovelHandler.GenerateOutline)

			// 大纲历史版本
			novels.GET("/:id/outline-versions", cfg.NovelHandler.ListOutlineVersions)

			// 时间线
			novels.GET("/:id/timeline", cfg.NovelHandler.GetTimeline)
			novels.POST("/:id/timeline/build", cfg.NovelHandler.BuildTimeline)

			// 上下文
			novels.GET("/:id/context", cfg.ContextHandler.GetContext)
			novels.GET("/:id/context/preview", cfg.ContextHandler.PreviewContext)
			novels.POST("/:id/prompt", cfg.ContextHandler.BuildPrompt)

			// 章节
			novels.GET("/:id/chapters", cfg.ChapterHandler.ListChapters)
			novels.POST("/:id/chapters", cfg.ChapterHandler.CreateChapter)
			novels.POST("/:id/chapters/insert", cfg.ChapterHandler.InsertChapter)
			novels.PUT("/:id/chapters/reorder", cfg.ChapterHandler.ReorderChapters)
			novels.DELETE("/:id/chapters", cfg.ChapterHandler.BatchDeleteChapters)
			novels.POST("/:id/chapters/batch-summarize", cfg.ChapterHandler.BatchSummarizeChapters)
			novels.POST("/:id/chapters/batch-publish", cfg.ChapterHandler.BatchPublishChapters)
			novels.GET("/:id/chapters/:chapter_no", cfg.ChapterHandler.GetChapterByNo)
			novels.PUT("/:id/chapters/:chapter_no", cfg.ChapterHandler.UpdateChapterByNo)
			novels.DELETE("/:id/chapters/:chapter_no", cfg.ChapterHandler.DeleteChapterByNo)
			novels.POST("/:id/chapters/:chapter_no/publish", cfg.ChapterHandler.PublishChapter)
			novels.POST("/:id/chapters/:chapter_no/unpublish", cfg.ChapterHandler.UnpublishChapter)
			novels.POST("/:id/chapters/:chapter_no/outline", cfg.ChapterHandler.GenerateChapterOutline)
			novels.POST("/:id/chapters/:chapter_no/character-snapshots", cfg.NovelHandler.SyncCharacterSnapshots)

			// 发布/取消发布/审核
			novels.POST("/:id/publish", cfg.NovelHandler.PublishNovel)
			novels.POST("/:id/unpublish", cfg.NovelHandler.UnpublishNovel)
			novels.PUT("/:id/review", cfg.NovelHandler.ReviewNovel)

			// 封面
			novels.POST("/:id/cover/generate", cfg.NovelHandler.GenerateCoverImage)

			// 角色
			novels.GET("/:id/characters", cfg.CharacterHandler.ListCharacters)
			novels.POST("/:id/characters", cfg.CharacterHandler.CreateCharacter)
			novels.DELETE("/:id/characters", cfg.CharacterHandler.BatchDeleteCharacters)
			novels.POST("/:id/characters/generate", cfg.CharacterHandler.GenerateCharacterProfile)
			novels.POST("/:id/characters/ai-batch", cfg.CharacterHandler.AIBatchGenerate)
			novels.POST("/:id/characters/batch-images", cfg.CharacterHandler.BatchGenerateImages)

			// 角色弧光
			novels.GET("/:id/character-arcs", cfg.CharacterHandler.GetAllCharacterArcs)
			novels.GET("/:id/character-arcs/:character_id", cfg.CharacterHandler.GetCharacterArc)
			novels.PUT("/:id/character-arcs/:character_id", cfg.CharacterHandler.UpdateCharacterArc)

			// 视频
			novels.GET("/:id/videos", cfg.VideoHandler.ListVideos)
			novels.POST("/:id/videos", cfg.VideoHandler.CreateVideo)

			// 从已有小说生成视频
			novels.POST("/:id/generate-video", cfg.ImportHandler.GenerateVideoFromNovel)

			// 分析导入的小说（状态通过统一端点 GET /api/v1/tasks/:task_id 查询）
			novels.POST("/:id/analyze", cfg.ImportHandler.StartAnalysis)
			// 分析进度查询（直接按 novel_id 查最新分析任务）
			novels.GET("/:id/analysis/status", cfg.NovelHandler.GetAnalysisStatus)

			// 爬取进度
			novels.GET("/:id/crawl/status", cfg.ImportHandler.GetCrawlStatus)
			novels.POST("/:id/crawl/resume", cfg.ImportHandler.ResumeCrawl)

			// 物品（项目级）
			if cfg.ItemHandler != nil {
				novels.GET("/:id/items", cfg.ItemHandler.ListItems)
				novels.POST("/:id/items", cfg.ItemHandler.CreateItem)
				novels.DELETE("/:id/items", cfg.ItemHandler.BatchDeleteItems)
				novels.POST("/:id/items/ai-extract", cfg.ItemHandler.AIExtractFromNovel)
				novels.POST("/:id/items/batch-images", cfg.ItemHandler.BatchGenerateImages)
				// 章节级物品（有效列表 + 覆盖 + AI提取）
				novels.GET("/:id/chapters/:chapter_no/items", cfg.ItemHandler.ListEffectiveItems)
				novels.POST("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.UpsertChapterItem)
				novels.DELETE("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.DeleteChapterItem)
				novels.POST("/:id/chapters/:chapter_no/items/ai-extract", cfg.ItemHandler.AIExtractChapterItems)
			}

			// 技能体系
			if cfg.SkillHandler != nil {
				novels.GET("/:id/skills", cfg.SkillHandler.ListSkills)
				novels.POST("/:id/skills", cfg.SkillHandler.CreateSkill)
				novels.DELETE("/:id/skills", cfg.SkillHandler.BatchDeleteSkills)
				novels.POST("/:id/skills/ai-generate", cfg.SkillHandler.GenerateSkills)
			}

			// 章节级角色（有效列表 + 覆盖 + AI提取次要角色 + 形象生成）
			novels.GET("/:id/chapters/:chapter_no/characters", cfg.CharacterHandler.ListEffectiveCharacters)
			novels.POST("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.UpsertChapterCharacter)
			novels.DELETE("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.DeleteChapterCharacter)
			novels.POST("/:id/chapters/:chapter_no/characters/ai-extract", cfg.CharacterHandler.AIExtractMinorCharacters)
			novels.POST("/:id/chapters/:chapter_no/characters/generate-images", cfg.CharacterHandler.GenerateChapterCharacterImages)

			// 剧情点（小说级）
			if cfg.PlotPointHandler != nil {
				novels.GET("/:id/plot-points", cfg.PlotPointHandler.ListByNovel)
				novels.POST("/:id/plot-points/ai-extract", cfg.PlotPointHandler.AIExtractFromNovel)
			}

	
			// 知识库
			if cfg.KnowledgeHandler != nil {
				novels.GET("/:id/knowledge/search", cfg.KnowledgeHandler.SearchKnowledge)
				novels.GET("/:id/knowledge", cfg.KnowledgeHandler.ListKnowledge)
				novels.POST("/:id/knowledge", cfg.KnowledgeHandler.CreateKnowledge)
				novels.POST("/:id/knowledge/bulk-import", cfg.KnowledgeHandler.BulkImport)
				novels.PUT("/:id/knowledge/:kb_id", cfg.KnowledgeHandler.UpdateKnowledge)
				novels.DELETE("/:id/knowledge/:kb_id", cfg.KnowledgeHandler.DeleteKnowledge)
			}

			// 戏剧张力管理
			if cfg.DramaticHandler != nil {
				// 节奏曲线
				novels.GET("/:id/pacing-curve", cfg.DramaticHandler.GetPacingCurve)
				novels.GET("/:id/pacing-health", cfg.DramaticHandler.GetPacingHealth)
				// 钩子链
				novels.GET("/:id/hooks", cfg.DramaticHandler.ListHooks)
				novels.POST("/:id/hooks", cfg.DramaticHandler.CreateHook)
				// 爽点
				novels.GET("/:id/satisfaction-points", cfg.DramaticHandler.ListSatisfactionPoints)
				novels.POST("/:id/satisfaction-points", cfg.DramaticHandler.CreateSatisfactionPoint)
				// 冲突弧
				novels.GET("/:id/conflict-arcs", cfg.DramaticHandler.ListConflictArcs)
				novels.POST("/:id/conflict-arcs", cfg.DramaticHandler.CreateConflictArc)
			}

			// 场景锚点（挂在 novel 下）
			if cfg.SceneAnchorHandler != nil {
				novels.GET("/:id/scene-anchors", cfg.SceneAnchorHandler.ListSceneAnchors)
				novels.POST("/:id/scene-anchors", cfg.SceneAnchorHandler.CreateSceneAnchor)
				novels.POST("/:id/scene-anchors/extract", cfg.SceneAnchorHandler.ExtractSceneAnchors)
				novels.POST("/:id/scene-anchors/ai-extract", cfg.SceneAnchorHandler.AIExtractFromNovel)
				novels.POST("/:id/scene-anchors/batch-ref-images", cfg.SceneAnchorHandler.BatchGenerateRefImages)
				novels.POST("/:id/chapters/:chapter_no/scene-anchors/ai-extract", cfg.SceneAnchorHandler.AIExtractChapterAnchors)
				novels.GET("/:id/chapters/:chapter_no/scene-anchors", cfg.SceneAnchorHandler.ListChapterAnchors)
				novels.PUT("/:id/chapters/:chapter_no/scene-anchors/:anchor_id", cfg.SceneAnchorHandler.BindChapterAnchor)
				novels.DELETE("/:id/chapters/:chapter_no/scene-anchors/:anchor_id", cfg.SceneAnchorHandler.UnbindChapterAnchor)
			}

			// 大纲审查（小说级别）
			if cfg.OutlineReviewHandler != nil {
				novels.POST("/:id/outline-review/batch", cfg.OutlineReviewHandler.BatchReviewNovel)
				novels.GET("/:id/outline-review", cfg.OutlineReviewHandler.ListNovelReviews)
				novels.GET("/:id/outline-review/synthesis", cfg.OutlineReviewHandler.GetNovelSynthesis)
			}
		}

		// 物品（单个物品操作）
		if cfg.ItemHandler != nil {
			items := v1.Group("/items")
			{
				items.GET("/:id", cfg.ItemHandler.GetItem)
				items.PUT("/:id", cfg.ItemHandler.UpdateItem)
				items.DELETE("/:id", cfg.ItemHandler.DeleteItem)
				items.POST("/:id/image/upload", cfg.ItemHandler.UploadItemImage)
				items.POST("/:id/images", cfg.ItemHandler.GenerateItemImage)
				items.POST("/:id/reference/upload", cfg.ItemHandler.UploadItemReference)
			}
		}

		// 技能单条操作
		if cfg.SkillHandler != nil {
			skills := v1.Group("/skills")
			{
				skills.GET("/:id", cfg.SkillHandler.GetSkill)
				skills.PUT("/:id", cfg.SkillHandler.UpdateSkill)
				skills.DELETE("/:id", cfg.SkillHandler.DeleteSkill)
				skills.POST("/:id/effect", cfg.SkillHandler.GenerateSkillEffect)
			}
		}

		// 章节相关
		chapters := v1.Group("/chapters")
		{
			chapters.GET("/:id", cfg.ChapterHandler.GetChapter)
			chapters.PUT("/:id", cfg.ChapterHandler.UpdateChapter)
			chapters.DELETE("/:id", cfg.ChapterHandler.DeleteChapter)
			chapters.POST("/:id/regenerate", cfg.ChapterHandler.RegenerateChapter)
			chapters.GET("/:id/versions", cfg.ChapterHandler.GetVersions)
			chapters.POST("/:id/versions/:version_no/restore", cfg.ChapterHandler.RestoreVersion)
			chapters.GET("/:id/versions/:version_id/content", cfg.ChapterHandler.GetVersionContent)
			chapters.POST("/:id/quality-check", cfg.ChapterHandler.QualityCheck)
			chapters.GET("/:id/quality-report", cfg.ChapterHandler.GetQualityReport)
			chapters.POST("/:id/improve", cfg.ChapterHandler.RefineChapter)
			chapters.POST("/:id/rewrite", cfg.ChapterHandler.RewriteChapterByInstruction)
			chapters.POST("/:id/refine-selection", cfg.ChapterHandler.RefineSelection)
			chapters.POST("/:id/chat/stream", cfg.ChapterHandler.ChapterChatStream)
			chapters.POST("/:id/approve", cfg.ChapterHandler.ApproveChapter)
			chapters.POST("/:id/reject", cfg.ChapterHandler.RejectChapter)

			// AI 审查
			chapters.POST("/:id/review", cfg.ChapterHandler.ReviewChapter)
			chapters.GET("/:id/reviews", cfg.ChapterHandler.ListChapterReviews)
			chapters.GET("/:id/reviews/:rid", cfg.ChapterHandler.GetChapterReview)
			chapters.POST("/:id/reviews/:rid/rollback", cfg.ChapterHandler.RollbackChapterReview)
			chapters.POST("/:id/review/apply-diffs", cfg.ChapterHandler.ApplyChapterReviewDiffs)
			chapters.GET("/:id/ignored-issues", cfg.ChapterHandler.ListChapterIgnoredIssues)
			chapters.POST("/:id/ignored-issues", cfg.ChapterHandler.IgnoreChapterIssue)
			chapters.DELETE("/:id/ignored-issues/:iid", cfg.ChapterHandler.UnignoreChapterIssue)

			// 大纲审查
			if cfg.OutlineReviewHandler != nil {
				chapters.POST("/:id/outline-review", cfg.OutlineReviewHandler.ReviewChapter)
				chapters.GET("/:id/outline-review", cfg.OutlineReviewHandler.GetChapterReview)
			}

			// 连续性检查记录
			chapters.GET("/:id/continuity-reports", cfg.ChapterHandler.ListContinuityReports)

			// 剧情点（章节级）
			if cfg.PlotPointHandler != nil {
				chapters.GET("/:id/plot-points", cfg.PlotPointHandler.ListByChapter)
				chapters.POST("/:id/plot-points", cfg.PlotPointHandler.Create)
				chapters.POST("/:id/plot-points/extract", cfg.PlotPointHandler.ExtractFromChapter)
			}
		}

		// 剧情点（单条操作）
		if cfg.PlotPointHandler != nil {
			plotPoints := v1.Group("/plot-points")
			{
				plotPoints.GET("/:id", cfg.PlotPointHandler.Get)
				plotPoints.PUT("/:id", cfg.PlotPointHandler.Update)
				plotPoints.PUT("/:id/resolve", cfg.PlotPointHandler.MarkResolved)
				plotPoints.DELETE("/:id", cfg.PlotPointHandler.Delete)
			}
		}

		// 戏剧张力单条操作
		if cfg.DramaticHandler != nil {
			hooks := v1.Group("/hooks")
			{
				hooks.PUT("/:id", cfg.DramaticHandler.UpdateHook)
				hooks.DELETE("/:id", cfg.DramaticHandler.DeleteHook)
				hooks.PUT("/:id/fulfill", cfg.DramaticHandler.FulfillHook)
				hooks.PUT("/:id/payoff-quality", cfg.DramaticHandler.RateHookPayoff)
			}
			sps := v1.Group("/satisfaction-points")
			{
				sps.PUT("/:id", cfg.DramaticHandler.UpdateSatisfactionPoint)
				sps.DELETE("/:id", cfg.DramaticHandler.DeleteSatisfactionPoint)
			}
			arcs := v1.Group("/conflict-arcs")
			{
				arcs.PUT("/:id", cfg.DramaticHandler.UpdateConflictArc)
				arcs.DELETE("/:id", cfg.DramaticHandler.DeleteConflictArc)
				arcs.PUT("/:id/advance-phase", cfg.DramaticHandler.AdvancePhase)
				arcs.PUT("/:id/tension", cfg.DramaticHandler.UpdateArcTension)
			}
		}

		// 伏笔（专用表 ink_foreshadow）
		if cfg.ForeshadowHandler != nil {
			foreshadows := v1.Group("/novels/:id/foreshadows")
			{
				foreshadows.GET("", cfg.ForeshadowHandler.ListForeshadows)
				foreshadows.POST("", cfg.ForeshadowHandler.CreateForeshadow)
				foreshadows.POST("/extract", cfg.ForeshadowHandler.AIExtractForeshadows)
				foreshadows.GET("/unfulfilled", cfg.ForeshadowHandler.ListUnfulfilledForeshadows)
				foreshadows.GET("/stats", cfg.ForeshadowHandler.GetForeshadowStats)
				foreshadows.PUT("/:foreshadow_id", cfg.ForeshadowHandler.UpdateForeshadow)
				foreshadows.DELETE("/:foreshadow_id", cfg.ForeshadowHandler.DeleteForeshadow)
				foreshadows.POST("/:foreshadow_id/reinforce", cfg.ForeshadowHandler.AddReinforcement)
				foreshadows.GET("/tree", cfg.ForeshadowHandler.GetForeshadowTree)
			}
		}

		// 场景锚点单条操作
		if cfg.SceneAnchorHandler != nil {
			sceneAnchors := v1.Group("/scene-anchors")
			{
				sceneAnchors.GET("/:id", cfg.SceneAnchorHandler.GetSceneAnchor)
				sceneAnchors.PUT("/:id", cfg.SceneAnchorHandler.UpdateSceneAnchor)
				sceneAnchors.DELETE("/:id", cfg.SceneAnchorHandler.DeleteSceneAnchor)
				sceneAnchors.PUT("/:id/ref-image", cfg.SceneAnchorHandler.LockRefImage)
				sceneAnchors.POST("/:id/ref-image/upload", cfg.SceneAnchorHandler.UploadRefImage)
				sceneAnchors.POST("/:id/generate-ref-image", cfg.SceneAnchorHandler.GenerateRefImage)
				sceneAnchors.POST("/:id/ai-analyze", cfg.SceneAnchorHandler.AIAnalyzeSceneAnchor)
				sceneAnchors.POST("/:id/edit-ref-image", cfg.SceneAnchorHandler.EditRefImage)
				sceneAnchors.GET("/:id/consistency-logs", cfg.SceneAnchorHandler.GetConsistencyLogs)
			}
		}


		// 角色相关
		characters := v1.Group("/characters")
		{
			characters.GET("/:id", cfg.CharacterHandler.GetCharacter)
			characters.PUT("/:id", cfg.CharacterHandler.UpdateCharacter)
			characters.DELETE("/:id", cfg.CharacterHandler.DeleteCharacter)
			characters.POST("/:id/images", cfg.CharacterHandler.GenerateCharacterImage)
			characters.POST("/:id/three-view", cfg.CharacterHandler.GenerateThreeView)
			characters.POST("/:id/image/upload", cfg.CharacterHandler.UploadCharacterImage)
			characters.POST("/:id/analyze-consistency", cfg.CharacterHandler.AnalyzeCharacterConsistency)
			characters.POST("/:id/voice/preview", cfg.CharacterHandler.PreviewVoice)
			characters.GET("/:id/voice/sample", cfg.CharacterHandler.ServeVoiceSample)
			characters.POST("/:id/reanalyze", cfg.CharacterHandler.ReanalyzeCharacter)
			characters.POST("/:id/extract-voice", cfg.CharacterHandler.ExtractCharacterVoice)
			characters.GET("/:id/snapshots", cfg.CharacterHandler.ListCharacterSnapshots)
			characters.POST("/:id/snapshots", cfg.CharacterHandler.CreateCharacterSnapshot)
			// 角色形象（不同时期外观管理）
			characters.GET("/:id/looks", cfg.CharacterHandler.ListCharacterLooks)
			characters.POST("/:id/looks", cfg.CharacterHandler.CreateCharacterLook)
			characters.GET("/:id/looks/active", cfg.CharacterHandler.GetActiveLook)
			characters.POST("/:id/looks/generate-prompt", cfg.CharacterHandler.GenerateLookVisualPrompt)
			characters.PUT("/:id/looks/:look_id", cfg.CharacterHandler.UpdateCharacterLook)
			characters.DELETE("/:id/looks/:look_id", cfg.CharacterHandler.DeleteCharacterLook)
			characters.POST("/:id/looks/:look_id/images", cfg.CharacterHandler.GenerateLookImages)
			characters.POST("/:id/looks/:look_id/upload", cfg.CharacterHandler.UploadCharacterLookImage)
		}

		// 视频相关
		videos := v1.Group("/videos")
		{
			videos.GET("", cfg.VideoHandler.ListVideos)
			videos.GET("/providers", cfg.VideoHandler.ListVideoProviders)
			videos.GET("/:id", cfg.VideoHandler.GetVideo)
			videos.GET("/:id/progress", cfg.VideoHandler.GetVideoProgress)
			videos.PUT("/:id", cfg.VideoHandler.UpdateVideo)
			videos.DELETE("/:id", cfg.VideoHandler.DeleteVideo)
			videos.GET("/:id/storyboard", cfg.VideoHandler.GetStoryboard)
			videos.PUT("/:id/storyboard/:shot_id", cfg.VideoHandler.UpdateStoryboardShot)
			videos.POST("/:id/storyboard/generate", cfg.VideoHandler.GenerateStoryboard)
			videos.POST("/:id/storyboard/review", cfg.VideoHandler.ReviewStoryboard)
			videos.GET("/:id/storyboard/reviews", cfg.VideoHandler.ListReviewRecords)
			videos.POST("/:id/storyboard/reviews/:record_id/rollback", cfg.VideoHandler.RollbackReview)
			videos.POST("/:id/storyboard/optimize-from-review", cfg.VideoHandler.OptimizeStoryboardFromReview)
			videos.POST("/:id/storyboard/optimize/apply", cfg.VideoHandler.ApplyStoryboardDiffs)
			videos.GET("/:id/storyboard/ignored-suggestions", cfg.VideoHandler.ListIgnoredSuggestions)
			videos.POST("/:id/storyboard/ignored-suggestions", cfg.VideoHandler.IgnoreSuggestion)
			videos.DELETE("/:id/storyboard/ignored-suggestions/:suggestion_id", cfg.VideoHandler.UnignoreSuggestion)
			videos.POST("/:id/storyboard/review/apply-inserts", cfg.VideoHandler.ApplyReviewInserts)
			videos.POST("/:id/storyboard/review/apply-deletes", cfg.VideoHandler.ApplyReviewDeletes)
			videos.POST("/:id/generate", cfg.VideoHandler.StartVideoGeneration)
			videos.GET("/:id/status", cfg.VideoHandler.GetVideoStatus)
			videos.GET("/:id/shots", cfg.VideoHandler.ListShots)
			videos.POST("/:id/shots/generate", cfg.VideoHandler.GenerateShotVideos)
			videos.POST("/:id/shots/batch-generate", cfg.VideoHandler.BatchGenerateShots)
			videos.POST("/:id/shots/batch-images", cfg.VideoHandler.BatchGenerateShotImages)
			videos.POST("/:id/shots/batch-clips", cfg.VideoHandler.BatchGenerateShotClips)
			videos.POST("/:id/shots/insert", cfg.VideoHandler.InsertShot)
			videos.POST("/:id/shots/reorder", cfg.VideoHandler.ReorderShots)
			videos.POST("/:id/shots/sfx", cfg.VideoHandler.BatchGenerateSFX)
			videos.POST("/:id/shots/sfx-tags", cfg.VideoHandler.AnalyzeSFXTags)
			videos.POST("/:id/shots/batch-voice", cfg.VideoHandler.BatchGenerateVoice)
			// BGM 背景音乐
			videos.GET("/:id/bgm/segments", cfg.VideoHandler.ListBGMSegments)
			videos.GET("/:id/bgm/search", cfg.VideoHandler.JamendoSearchBGM)
			videos.GET("/:id/bgm/proxy", cfg.VideoHandler.ProxyBGMAudio)
			videos.PUT("/:id/bgm/segments/:seg_id", cfg.VideoHandler.UpdateBGMSegment)
			videos.PATCH("/:id/bgm/segments/:seg_id/track", cfg.VideoHandler.ApplyBGMTrack)
			videos.PATCH("/:id/bgm/segments/:seg_id/disabled", cfg.VideoHandler.ToggleBGMSegment)
			videos.POST("/:id/bgm/analyze", cfg.VideoHandler.AnalyzeBGMSegments)
			videos.POST("/:id/bgm/generate", cfg.VideoHandler.GenerateBGM)
			videos.POST("/:id/shots/:shot_id/generate", cfg.VideoHandler.GenerateSingleShot)
			videos.POST("/:id/shots/:shot_id/copy", cfg.VideoHandler.CopyShot)
			videos.DELETE("/:id/shots/:shot_id", cfg.VideoHandler.DeleteShot)
			videos.POST("/:id/shots/:shot_id/refine-image", cfg.VideoHandler.RefineShotImage)
			videos.POST("/:id/shots/:shot_id/sfx", cfg.VideoHandler.GenerateShotSFX)
			videos.PUT("/:id/shots/:shot_id/sfx-tags", cfg.VideoHandler.UpdateShotSFXTags)
			// 音效条目（多条）
			videos.GET("/:id/shots/:shot_id/sfx-items", cfg.VideoHandler.ListShotSFXItems)
			videos.POST("/:id/shots/:shot_id/sfx-items", cfg.VideoHandler.ImportShotSFXItem)
			videos.PUT("/:id/shots/:shot_id/sfx-items/:item_id", cfg.VideoHandler.UpdateShotSFXItem)
			videos.PATCH("/:id/shots/:shot_id/sfx-items/:item_id/disabled", cfg.VideoHandler.ToggleShotSFXItem)
			videos.DELETE("/:id/shots/:shot_id/sfx-items/:item_id", cfg.VideoHandler.DeleteShotSFXItem)
			// 音效文件代理路由已移至公开区域（/api/v1/sfx-items/:item_id/audio）
			// 语音段落
			videos.GET("/:id/shots/:shot_id/segments", cfg.VideoHandler.ListVoiceSegments)
			videos.POST("/:id/shots/:shot_id/segments", cfg.VideoHandler.AppendVoiceSegment)
			videos.POST("/:id/shots/:shot_id/segments/insert", cfg.VideoHandler.InsertVoiceSegment)
			videos.PUT("/:id/shots/:shot_id/segments/:seg_id", cfg.VideoHandler.UpdateVoiceSegment)
			videos.DELETE("/:id/shots/:shot_id/segments/:seg_id", cfg.VideoHandler.DeleteVoiceSegment)
			videos.POST("/:id/shots/:shot_id/segments/:seg_id/voice", cfg.VideoHandler.GenerateSegmentVoice)
			videos.POST("/:id/shots/:shot_id/voice/merge", cfg.VideoHandler.MergeVoiceSegments)
			// ServeSegmentAudio 已移至公开路由区域
			videos.POST("/:id/storyboard/:shot_id/voice", cfg.VideoHandler.GenerateShotVoice)
			// ServeAudio 已移至公开路由区域
			videos.POST("/:id/stitch", cfg.VideoHandler.StitchVideoHandler)
			videos.GET("/:id/download", cfg.VideoHandler.DownloadVideo)
			videos.GET("/:id/export/:format", cfg.VideoHandler.Export)
			videos.POST("/:id/subtitles/export", cfg.VideoHandler.ExportSubtitles)
			videos.POST("/:id/synthesize", cfg.VideoHandler.SynthesizeVideo)
			videos.POST("/:id/publish", cfg.VideoHandler.PublishVideo)
			videos.POST("/:id/unpublish", cfg.VideoHandler.UnpublishVideo)
			if cfg.PlatformHandler != nil {
				videos.GET("/:id/publish-records", cfg.PlatformHandler.ListPublishRecords)
				videos.POST("/:id/publish-external", cfg.PlatformHandler.PublishToExternal)
			}
			// 分镜绑定场景锚点
			if cfg.SceneAnchorHandler != nil {
				videos.PUT("/:id/shots/:shot_id/anchor", cfg.SceneAnchorHandler.SetShotAnchor)
			}
			// 分镜绑定角色
			videos.PUT("/:id/shots/:shot_id/characters", cfg.VideoHandler.SetShotCharacters)
		}

		// 分镜
		storyboard := v1.Group("/storyboard")
		{
			storyboard.POST("/generate", func(c *gin.Context) {
				cfg.VideoHandler.GenerateStoryboard(c)
			})
			storyboard.POST("/analyze-emotions", cfg.VideoHandler.AnalyzeEmotions)
		}

		// 视频增强
		video := v1.Group("/video")
		{
			video.POST("/enhance", cfg.VideoHandler.EnhanceVideo)
			video.POST("/recommendations", cfg.VideoHandler.GetEnhancementRecommendations)
		}

		// 一致性检测
		consistency := v1.Group("/consistency")
		{
			consistency.GET("/default", cfg.VideoHandler.GetDefaultConsistencyConfig)
			consistency.POST("/score", cfg.VideoHandler.CalculateConsistencyScore)
		}

		// 模型管理
		modelProviders := v1.Group("/model-providers")
		{
			modelProviders.GET("", cfg.ModelHandler.ListProviders)
			modelProviders.POST("", cfg.ModelHandler.CreateProvider)
			modelProviders.GET("/templates", cfg.ModelHandler.ListProviderTemplates)
			modelProviders.POST("/sync-group", cfg.ModelHandler.SyncGroupProviders)
			modelProviders.GET("/capable", cfg.ModelHandler.ListCapableProviders)
			modelProviders.POST("/fetch-models", cfg.ModelHandler.FetchProviderModels)
			modelProviders.GET("/:id", cfg.ModelHandler.GetProvider)
			modelProviders.PUT("/:id", cfg.ModelHandler.UpdateProvider)
			modelProviders.DELETE("/:id", cfg.ModelHandler.DeleteProvider)
			modelProviders.POST("/:id/test", cfg.ModelHandler.TestProvider)
		}

		models := v1.Group("/models")
		{
			models.GET("", cfg.ModelHandler.ListModels)
			models.POST("", cfg.ModelHandler.CreateModel)
			models.GET("/available/:task_type", cfg.ModelHandler.GetAvailableModels)
			models.POST("/select", cfg.ModelHandler.SelectModel)
			models.POST("/test-prompt", cfg.ModelHandler.TestModelPrompt)
			models.GET("/task-mappings", cfg.ModelHandler.GetTaskMappings)
			models.PUT("/task-mappings", cfg.ModelHandler.UpdateTaskMapping)
			models.PUT("/:id", cfg.ModelHandler.UpdateModel)
			models.DELETE("/:id", cfg.ModelHandler.DeleteModel)
			models.POST("/:id/test", cfg.ModelHandler.TestModel)
			// MCP 工具绑定
			models.GET("/:id/mcp-tools", cfg.McpHandler.GetModelMcpTools)
			models.POST("/:id/mcp-tools", cfg.McpHandler.BindMcpTool)
			models.DELETE("/:id/mcp-tools/:tool_id", cfg.McpHandler.UnbindMcpTool)
		}

		// 敏感词管理（管理员）
		if cfg.SensitiveWordHandler != nil {
			sw := v1.Group("/admin/sensitive-words")
			{
				sw.GET("", cfg.SensitiveWordHandler.List)
				sw.POST("", cfg.SensitiveWordHandler.Create)
				sw.PUT("/:id", cfg.SensitiveWordHandler.Update)
				sw.DELETE("/:id", cfg.SensitiveWordHandler.Delete)
			}
		}

		// MCP 工具管理
		mcpTools := v1.Group("/mcp-tools")
		{
			mcpTools.GET("", cfg.McpHandler.ListMcpTools)
			mcpTools.POST("", cfg.McpHandler.CreateMcpTool)
			mcpTools.PUT("/:id", cfg.McpHandler.UpdateMcpTool)
			mcpTools.DELETE("/:id", cfg.McpHandler.DeleteMcpTool)
			mcpTools.POST("/:id/test", cfg.McpHandler.TestMcpTool)
			mcpTools.GET("/:id/models", cfg.McpHandler.GetMcpToolModels)
		}

		// 内置工具端点（系统 MCP 工具的 HTTP 后端）
		toolsGroup := v1.Group("/tools")
		if cfg.WebSearchHandler != nil {
			toolsGroup.POST("/web-search", cfg.WebSearchHandler.Search)
		}
		if cfg.WikiSearchHandler != nil {
			toolsGroup.POST("/wiki-search", cfg.WikiSearchHandler.Search)
		}
		if cfg.StoryPatternHandler != nil {
			toolsGroup.POST("/story-pattern", cfg.StoryPatternHandler.Search)
			toolsGroup.GET("/story-pattern/list", cfg.StoryPatternHandler.ListAll)

			// REST 风格端点（供前端直接浏览）
			v1.GET("/story-patterns", cfg.StoryPatternHandler.ListAll)
			v1.GET("/story-patterns/:id", cfg.StoryPatternHandler.GetPattern)
			novels.POST("/:id/story-patterns/suggest", cfg.StoryPatternHandler.SuggestForNovel)
		}
		if cfg.ImageRefSearchHandler != nil {
			toolsGroup.POST("/image-ref-search", cfg.ImageRefSearchHandler.Search)
		}
		if cfg.ColorPaletteHandler != nil {
			toolsGroup.POST("/color-palette", cfg.ColorPaletteHandler.Get)
			toolsGroup.GET("/color-palette/list", cfg.ColorPaletteHandler.ListAll)
		}

		taskConfigs := v1.Group("/task-configs")
		{
			taskConfigs.GET("/:task", cfg.ModelHandler.GetTaskConfig)
			taskConfigs.PUT("/:task", cfg.ModelHandler.UpdateTaskConfig)
		}

		experiments := v1.Group("/experiments")
		{
			experiments.GET("", cfg.ModelHandler.ListExperiments)
			experiments.POST("", cfg.ModelHandler.CreateExperiment)
			experiments.GET("/:id", cfg.ModelHandler.GetExperiment)
			experiments.POST("/:id/start", cfg.ModelHandler.StartExperiment)
		}

		// 世界观
		worldviews := v1.Group("/worldviews")
		{
			worldviews.GET("", cfg.WorldviewHandler.ListWorldviews)
			worldviews.POST("", cfg.WorldviewHandler.CreateWorldview)
			worldviews.POST("/generate", cfg.WorldviewHandler.GenerateWorldview)
			worldviews.GET("/:id", cfg.WorldviewHandler.GetWorldview)
			worldviews.PUT("/:id", cfg.WorldviewHandler.UpdateWorldview)
			worldviews.DELETE("/:id", cfg.WorldviewHandler.DeleteWorldview)
			// 单字段更新（不覆盖其他字段）
			worldviews.PUT("/:id/sections/:section_key", cfg.WorldviewHandler.UpdateSection)
			// 势力/实体 CRUD
			worldviews.GET("/:id/entities", cfg.WorldviewHandler.ListEntities)
			worldviews.POST("/:id/entities", cfg.WorldviewHandler.CreateEntity)
			worldviews.PUT("/:id/entities/:entity_id", cfg.WorldviewHandler.UpdateEntity)
			worldviews.DELETE("/:id/entities/:entity_id", cfg.WorldviewHandler.DeleteEntity)
		}

		// 租户管理
		tenants := v1.Group("/tenants")
		{
			tenants.GET("", cfg.TenantHandler.ListTenants)
			tenants.POST("", cfg.TenantHandler.CreateTenant)
			tenants.GET("/:id", cfg.TenantHandler.GetTenant)
			tenants.PUT("/:id", cfg.TenantHandler.UpdateTenant)
			tenants.DELETE("/:id", cfg.TenantHandler.DeleteTenant)
			tenants.GET("/:id/quota", cfg.TenantHandler.GetQuota)
			tenants.GET("/:id/members", cfg.TenantHandler.ListMembers)
			tenants.POST("/:id/members", cfg.TenantHandler.AddMember)
			tenants.DELETE("/:id/members/:user_id", cfg.TenantHandler.RemoveMember)
			tenants.PUT("/:id/members/:user_id/role", cfg.TenantHandler.UpdateMemberRole)
		}

		// 风格控制
		styles := v1.Group("/styles")
		{
			styles.GET("/default", cfg.StyleHandler.GetDefaultStyle)
			styles.POST("/prompt", cfg.StyleHandler.BuildStylePrompt)
			styles.GET("/presets", cfg.StyleHandler.GetStylePresets)
			styles.POST("/presets/:name/apply", cfg.StyleHandler.ApplyStylePreset)
		}

		system := v1.Group("/system")
		{
			system.GET("/settings", cfg.SystemHandler.ListSettings)
			system.PUT("/settings/:key", cfg.SystemHandler.UpdateSetting)
		}

		// 仪表盘统计
		if cfg.DashboardHandler != nil {
			v1.GET("/dashboard/stats", cfg.DashboardHandler.GetStats)
		}

		// 平台广场 + 外部账号（JWT 保护部分）
		if cfg.PlatformHandler != nil {
			// JWT 保护的平台路由
			platformR := v1.Group("/platform")
			{
				platformR.POST("/videos/:id/like", cfg.PlatformHandler.ToggleLike)
				platformR.POST("/videos/:id/comments", cfg.PlatformHandler.AddComment)
				platformR.DELETE("/videos/:id/comments/:cid", cfg.PlatformHandler.DeleteComment)
				platformR.POST("/novels/:id/like", cfg.PlatformHandler.ToggleNovelLike)
				platformR.POST("/novels/:id/comments", cfg.PlatformHandler.AddNovelComment)
				platformR.DELETE("/novels/:id/comments/:cid", cfg.PlatformHandler.DeleteNovelComment)
				// 章节社交
				platformR.POST("/novels/:id/chapters/:chapter_no/like", cfg.PlatformHandler.ToggleChapterLike)
				platformR.GET("/novels/:id/chapters/:chapter_no/like", cfg.PlatformHandler.GetChapterIsLiked)
				platformR.POST("/novels/:id/chapters/:chapter_no/comments", cfg.PlatformHandler.AddChapterComment)
				platformR.DELETE("/novels/:id/chapters/:chapter_no/comments/:cid", cfg.PlatformHandler.DeleteChapterComment)
				platformR.POST("/novels/:id/chapters/:chapter_no/read", cfg.PlatformHandler.MarkChapterRead)
				// 阅读进度
				platformR.PUT("/novels/:id/progress", cfg.PlatformHandler.SaveReadingProgress)
				platformR.GET("/novels/:id/progress", cfg.PlatformHandler.GetReadingProgress)
				platformR.GET("/novels/:id/read-chapters", cfg.PlatformHandler.GetReadChapters)
				// 阅读历史
				platformR.GET("/me/reading-history", cfg.PlatformHandler.GetReadingHistory)
				platformR.GET("/accounts", cfg.PlatformHandler.ListAccounts)
				platformR.GET("/accounts/oauth/:platform", cfg.PlatformHandler.ConnectAccount)
				platformR.GET("/accounts/oauth-url/:platform", cfg.PlatformHandler.GetOAuthURL)
				platformR.GET("/accounts/callback/:platform", cfg.PlatformHandler.OAuthCallback)
				platformR.DELETE("/accounts/:id", cfg.PlatformHandler.DisconnectAccount)
			}
		}

		// ── 素材库 ────────────────────────────────────────────────────
		if cfg.AssetHandler != nil {
			ah := cfg.AssetHandler

			// Tag dictionary (before /assets to avoid param conflict)
			v1.GET("/assets/tags", ah.ListTags)
			v1.GET("/assets/tags/suggest", ah.SuggestTags)
			v1.GET("/assets/trash", ah.ListTrash)
			v1.DELETE("/assets/trash", ah.EmptyTrash)
			v1.DELETE("/assets/clear-personal", ah.ClearPersonalAssets)
			v1.GET("/assets/stats/ranking", ah.GetValueRanking)
			v1.GET("/assets/stats/search-gaps", ah.GetSearchGaps)
			v1.POST("/assets/batch-delete", ah.BatchDelete)
			v1.POST("/assets/batch-share-request", ah.BatchShareRequest)

			// Asset CRUD
			v1.GET("/assets", ah.SearchAssets)
			v1.POST("/assets", ah.Upload)
			v1.GET("/assets/:id", ah.GetAsset)
			v1.PUT("/assets/:id", ah.UpdateAsset)
			v1.DELETE("/assets/:id", ah.SoftDelete)
			v1.POST("/assets/:id/restore", ah.Restore)
			v1.DELETE("/assets/:id/purge", ah.Purge)
			v1.GET("/assets/:id/stream", ah.Stream)

			// Share workflow
			v1.POST("/assets/:id/share-request", ah.RequestShare)
			v1.GET("/assets/:id/share-request", ah.GetShareRequest)
			v1.DELETE("/assets/:id/share-request", ah.CancelShareRequest)
			v1.POST("/assets/:id/withdraw", ah.WithdrawShare)

			// Tags on asset
			v1.POST("/assets/:id/tags", ah.AddTags)
			v1.DELETE("/assets/:id/tags/:tag_id", ah.RemoveTag)
			v1.POST("/assets/:id/auto-tag", ah.TriggerAutoTag)

			// Public interactions
			v1.POST("/assets/:id/like", ah.ToggleLike)
			v1.POST("/assets/:id/use", ah.UseAsset)

			// Versions
			v1.GET("/assets/:id/versions", ah.ListVersions)
			v1.POST("/assets/:id/versions", ah.CreateVersion)
			v1.POST("/assets/:id/versions/:v/restore", ah.RestoreVersion)

			// Comments
			v1.GET("/assets/:id/comments", ah.ListComments)
			v1.POST("/assets/:id/comments", ah.AddComment)
			v1.DELETE("/assets/:id/comments/:cid", ah.DeleteComment)

			// Collections
			v1.GET("/asset-collections", ah.ListCollections)
			v1.POST("/asset-collections", ah.CreateCollection)
			v1.GET("/asset-collections/:id/items", ah.ListCollectionItems)
			v1.POST("/asset-collections/:id/items", ah.AddToCollection)
			v1.DELETE("/asset-collections/:id/items", ah.RemoveFromCollection)

			// Share links
			v1.GET("/share-links", ah.ListShareLinks)
			v1.POST("/share-links", ah.CreateShareLink)
			v1.DELETE("/share-links/:token", ah.RevokeShareLink)

			// Analytics
			v1.GET("/account/quota", ah.GetQuota)

			// Crawl jobs
			v1.GET("/crawl-jobs", ah.ListCrawlJobs)
			v1.POST("/crawl-jobs", ah.CreateCrawlJob)
			v1.GET("/crawl-jobs/:id", ah.GetCrawlJob)
			v1.POST("/crawl-jobs/:id/cancel", ah.CancelCrawlJob)
			v1.POST("/crawl-jobs/:id/retry", ah.RetryCrawlJob)

			// Admin
			adminR := v1.Group("/admin")
			{
				adminR.GET("/share-requests", ah.ListPendingShareRequests)
				adminR.POST("/share-requests/:id/approve", ah.ApproveShareRequest)
				adminR.POST("/share-requests/:id/reject", ah.RejectShareRequest)
				adminR.POST("/assets/:id/remove", ah.AdminRemoveAsset)
			}
		}

		// Public share page (no auth required — placed outside v1 auth middleware)
		// NOTE: registered separately below on the root group

		// 通用图像编辑 / 高清放大
		if cfg.ImageHandler != nil {
			v1.POST("/images/edit", cfg.ImageHandler.EditImage)
			v1.POST("/images/upscale", cfg.ImageHandler.UpscaleImage)
		}

		// 本地文件系统浏览（本地部署工具专用）
		if cfg.FsHandler != nil {
			v1.GET("/fs/browse", cfg.FsHandler.Browse)
		}

		// 改写项目
		if cfg.RewriteHandler != nil {
			rewrite := v1.Group("/rewrite")
			{
				rewrite.GET("/projects", cfg.RewriteHandler.ListProjects)
				rewrite.POST("/projects", cfg.RewriteHandler.CreateProject)
				rewrite.GET("/projects/:id", cfg.RewriteHandler.GetProject)
				rewrite.DELETE("/projects/:id", cfg.RewriteHandler.DeleteProject)
				rewrite.POST("/projects/:id/analyze", cfg.RewriteHandler.StartAnalysis)
				rewrite.POST("/projects/:id/rewrite", cfg.RewriteHandler.StartRewriting)
				rewrite.GET("/projects/:id/analysis", cfg.RewriteHandler.GetAnalysis)
				rewrite.GET("/projects/:id/bible", cfg.RewriteHandler.GetBible)
				rewrite.GET("/projects/:id/chapters", cfg.RewriteHandler.ListChapterTasks)
				rewrite.GET("/projects/:id/chapters/:task_id", cfg.RewriteHandler.GetChapterTask)
				rewrite.POST("/projects/:id/chapters/:task_id/approve", cfg.RewriteHandler.ApproveChapter)
				rewrite.POST("/projects/:id/chapters/:task_id/apply", cfg.RewriteHandler.ApplyRewriteToChapter)
				rewrite.PUT("/projects/:id/bible", cfg.RewriteHandler.UpdateBible)
				rewrite.GET("/projects/:id/compliance-report", cfg.RewriteHandler.GetComplianceReport)
				rewrite.POST("/projects/:id/cancel", cfg.RewriteHandler.CancelRewrite)
			}
		}

		// Webhook 订阅管理
		if cfg.WebhookHandler != nil {
			webhooks := v1.Group("/webhooks")
			{
				webhooks.GET("", cfg.WebhookHandler.List)
				webhooks.POST("", cfg.WebhookHandler.Create)
				webhooks.DELETE("/:id", cfg.WebhookHandler.Delete)
				webhooks.POST("/:id/test", cfg.WebhookHandler.TestWebhook)
			}
		}

		// 审计日志查询
		if cfg.AuditHandler != nil {
			v1.GET("/audit-logs", cfg.AuditHandler.List)
		}

		// 协作
		if cfg.CollabHandler != nil {
			v1.GET("/novels/:id/events", cfg.CollabHandler.SSEStream)
			v1.GET("/novels/:id/members", cfg.CollabHandler.ListMembers)
			v1.POST("/novels/:id/members/invite", cfg.CollabHandler.InviteMember)
			v1.DELETE("/novels/:id/members/me", cfg.CollabHandler.LeaveNovel)
			v1.DELETE("/novels/:id/members/:uid", cfg.CollabHandler.RemoveMember)
			v1.PUT("/novels/:id/members/:uid", cfg.CollabHandler.UpdateMemberRole)
			v1.POST("/collab/accept", cfg.CollabHandler.AcceptInvite)
			v1.POST("/editing-locks", cfg.CollabHandler.AcquireLock)
			v1.DELETE("/editing-locks/:type/:entity_id", cfg.CollabHandler.ReleaseLock)
			v1.PUT("/editing-locks/:type/:entity_id/heartbeat", cfg.CollabHandler.HeartbeatLock)
			v1.GET("/editing-locks/:type/:entity_id", cfg.CollabHandler.GetLock)
		}
	}

	// System admin routes (require system_admin JWT) — registered outside v1 to use its own auth chain
	if cfg.SysAdminHandler != nil {
		sa := r.Group("/api/v1/sysadmin")
		sa.Use(middleware.NewAuth(cfg.JWTSecret, cfg.RedisClient))
		sa.Use(middleware.RequireSystemAdmin())
		{
			sa.GET("/overview", cfg.SysAdminHandler.GetOverview)
			sa.GET("/tenants", cfg.SysAdminHandler.ListTenants)
			sa.GET("/tenants/:id", cfg.SysAdminHandler.GetTenant)
			sa.PUT("/tenants/:id", cfg.SysAdminHandler.UpdateTenant)
			sa.DELETE("/tenants/:id", cfg.SysAdminHandler.DeleteTenant)
			sa.GET("/users", cfg.SysAdminHandler.ListUsers)
			sa.GET("/users/:id", cfg.SysAdminHandler.GetUser)
			sa.PUT("/users/:id", cfg.SysAdminHandler.UpdateUser)
			sa.POST("/users/:id/impersonate", cfg.SysAdminHandler.ImpersonateUser)
			sa.POST("/users/:id/reset-password", cfg.SysAdminHandler.ResetUserPassword)
			sa.GET("/tasks", cfg.SysAdminHandler.ListTasks)
			sa.POST("/tasks/:id/cancel", cfg.SysAdminHandler.CancelTask)
			sa.GET("/audit-logs", cfg.SysAdminHandler.ListAuditLogs)
			sa.GET("/settings", cfg.SysAdminHandler.ListSettings)
			sa.PUT("/settings", cfg.SysAdminHandler.UpdateSettings)
			sa.GET("/content-review/novels", cfg.SysAdminHandler.ListNovels)
			sa.GET("/asset-governance", cfg.SysAdminHandler.GetAssetGovernance)
			sa.GET("/ai-infra", cfg.SysAdminHandler.GetAIInfra)
			sa.POST("/notifications/broadcast", cfg.SysAdminHandler.BroadcastNotification)
			sa.POST("/notifications/tenant/:id", cfg.SysAdminHandler.NotifyTenant)
			sa.GET("/experiments", cfg.SysAdminHandler.ListExperiments)
			sa.POST("/change-password", cfg.SysAdminHandler.ChangePassword)
		}
	}

	return r
}
