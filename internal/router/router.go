package router

import (
	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/middleware"
)

// Config 路由配置
type Config struct {
	JWTSecret        string
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
}

// SetupRouter 配置路由
func SetupRouter(cfg *Config) *gin.Engine {
	r := gin.New()

	// 全局中间件
	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())

	// 健康检查（公开）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 本地上传文件静态服务
	r.Static("/uploads", "./uploads")

	// 媒体素材下载（DB 存储后端；无需登录，可嵌入 <audio>/<img>）
	if cfg.MediaHandler != nil {
		r.GET("/api/v1/media/:id", cfg.MediaHandler.ServeMedia)
	}

	// 公开分享页（无需登录）
	if cfg.AssetHandler != nil {
		r.GET("/api/v1/share/:token", cfg.AssetHandler.PublicSharePage)
	}

	// 公开平台广场路由（无需 JWT，只限速）
	if cfg.PlatformHandler != nil {
		public := r.Group("/api/v1")
		public.Use(middleware.RateLimit(60, 10))
		public.GET("/platform/videos", cfg.PlatformHandler.GetPlatformFeed)
		public.GET("/platform/videos/:id", cfg.PlatformHandler.GetPlatformVideo)
		public.POST("/platform/videos/:id/view", cfg.PlatformHandler.RecordView)
		public.GET("/platform/videos/:id/comments", cfg.PlatformHandler.ListComments)
		public.GET("/platform/novels", cfg.PlatformHandler.GetPlatformNovels)
		public.GET("/platform/novels/:id", cfg.PlatformHandler.GetPlatformNovel)
		public.POST("/platform/novels/:id/view", cfg.PlatformHandler.RecordNovelView)
		public.GET("/platform/novels/:id/comments", cfg.PlatformHandler.ListNovelComments)
	}

	// 公开认证路由（不需要JWT）
	auth := r.Group("/api/v1/auth")
	{
		auth.POST("/register", cfg.AuthHandler.Register)
		auth.POST("/login", cfg.AuthHandler.Login)
		auth.POST("/refresh", cfg.AuthHandler.RefreshToken)
		auth.POST("/sms/send", cfg.AuthHandler.SendSMSCode)
		auth.POST("/phone/register", cfg.AuthHandler.PhoneRegister)
		auth.POST("/phone/login", cfg.AuthHandler.PhoneLogin)
		auth.GET("/oauth/:provider/url", cfg.AuthHandler.OAuthURL)
		auth.GET("/oauth/:provider/callback", cfg.AuthHandler.OAuthCallback)
	}

	// 受保护路由（需要JWT）
	v1 := r.Group("/api/v1")
	v1.Use(middleware.RateLimit(60, 10))
	v1.Use(middleware.NewAuth(cfg.JWTSecret))
	{
		// 当前用户信息
		v1.GET("/auth/me", cfg.AuthHandler.GetCurrentUser)

		// 统一异步任务
		if cfg.TaskHandler != nil {
			tasks := v1.Group("/tasks")
			{
				tasks.GET("", cfg.TaskHandler.ListTasks)
				tasks.GET("/:task_id", cfg.TaskHandler.GetTask)
				tasks.POST("/:task_id/cancel", cfg.TaskHandler.CancelTask)
			}
		}

		// 通用图片上传
		if cfg.UploadHandler != nil {
			v1.POST("/upload/image", cfg.UploadHandler.UploadImage)
		}

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
			novels.POST("/:id/chapters/generate", cfg.NovelHandler.GenerateChapter)

			// 伏笔
			novels.GET("/:id/foreshadows", cfg.NovelHandler.GetForeshadows)
			novels.POST("/:id/foreshadows/:foreshadow_id/fulfill", cfg.NovelHandler.MarkForeshadowFulfilled)

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
			novels.POST("/:id/chapters/batch-summarize", cfg.ChapterHandler.BatchSummarizeChapters)
			novels.GET("/:id/chapters/:chapter_no", cfg.ChapterHandler.GetChapterByNo)
			novels.PUT("/:id/chapters/:chapter_no", cfg.ChapterHandler.UpdateChapterByNo)
			novels.DELETE("/:id/chapters/:chapter_no", cfg.ChapterHandler.DeleteChapterByNo)
			novels.POST("/:id/chapters/:chapter_no/outline", cfg.ChapterHandler.GenerateChapterOutline)
			novels.POST("/:id/chapters/:chapter_no/character-snapshots", cfg.NovelHandler.SyncCharacterSnapshots)

			// 大纲
			novels.POST("/:id/outline", cfg.NovelHandler.GenerateOutline)

			// 角色
			novels.GET("/:id/characters", cfg.CharacterHandler.ListCharacters)
			novels.POST("/:id/characters", cfg.CharacterHandler.CreateCharacter)
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

			// 爬取进度
			novels.GET("/:id/crawl/status", cfg.ImportHandler.GetCrawlStatus)
			novels.POST("/:id/crawl/resume", cfg.ImportHandler.ResumeCrawl)

			// 物品（项目级）
			if cfg.ItemHandler != nil {
				novels.GET("/:id/items", cfg.ItemHandler.ListItems)
				novels.POST("/:id/items", cfg.ItemHandler.CreateItem)
				novels.POST("/:id/items/ai-extract", cfg.ItemHandler.AIExtractFromNovel)
				novels.POST("/:id/items/batch-images", cfg.ItemHandler.BatchGenerateImages)
				// 章节级物品（有效列表 + 覆盖 + AI提取）
				novels.GET("/:id/chapters/:chapter_no/items", cfg.ItemHandler.ListEffectiveItems)
				novels.POST("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.UpsertChapterItem)
				novels.DELETE("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.DeleteChapterItem)
				novels.POST("/:id/chapters/:chapter_no/items/ai-extract", cfg.ItemHandler.AIExtractChapterItems)
			}

			// 章节级角色（有效列表 + 覆盖 + AI提取次要角色）
			novels.GET("/:id/chapters/:chapter_no/characters", cfg.CharacterHandler.ListEffectiveCharacters)
			novels.POST("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.UpsertChapterCharacter)
			novels.DELETE("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.DeleteChapterCharacter)
			novels.POST("/:id/chapters/:chapter_no/characters/ai-extract", cfg.CharacterHandler.AIExtractMinorCharacters)

			// 技能管理
			if cfg.SkillHandler != nil {
				novels.GET("/:id/skills", cfg.SkillHandler.ListSkills)
				novels.POST("/:id/skills", cfg.SkillHandler.CreateSkill)
				novels.POST("/:id/skills/generate", cfg.SkillHandler.GenerateSkills)
				novels.POST("/:id/chapters/:chapter_no/skills/ai-extract", cfg.SkillHandler.AIExtractChapterSkills)
			}

			// 剧情点（小说级）
			if cfg.PlotPointHandler != nil {
				novels.GET("/:id/plot-points", cfg.PlotPointHandler.ListByNovel)
				novels.POST("/:id/plot-points/ai-extract", cfg.PlotPointHandler.AIExtractFromNovel)
			}

	
			// 场景锚点（挂在 novel 下）
			if cfg.SceneAnchorHandler != nil {
				novels.GET("/:id/scene-anchors", cfg.SceneAnchorHandler.ListSceneAnchors)
				novels.POST("/:id/scene-anchors", cfg.SceneAnchorHandler.CreateSceneAnchor)
				novels.POST("/:id/scene-anchors/extract", cfg.SceneAnchorHandler.ExtractSceneAnchors)
				novels.POST("/:id/scene-anchors/ai-extract", cfg.SceneAnchorHandler.AIExtractFromNovel)
				novels.POST("/:id/scene-anchors/batch-ref-images", cfg.SceneAnchorHandler.BatchGenerateRefImages)
				novels.POST("/:id/chapters/:chapter_no/scene-anchors/ai-extract", cfg.SceneAnchorHandler.AIExtractChapterAnchors)
			}
		}

		// 技能（单个技能操作）
		if cfg.SkillHandler != nil {
			skills := v1.Group("/skills")
			{
				skills.GET("/:skillId", cfg.SkillHandler.GetSkill)
				skills.PUT("/:skillId", cfg.SkillHandler.UpdateSkill)
				skills.DELETE("/:skillId", cfg.SkillHandler.DeleteSkill)
				skills.POST("/:skillId/effect-image", cfg.SkillHandler.GenerateSkillEffect)
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

		// 章节相关
		chapters := v1.Group("/chapters")
		{
			chapters.GET("/:id", cfg.ChapterHandler.GetChapter)
			chapters.PUT("/:id", cfg.ChapterHandler.UpdateChapter)
			chapters.DELETE("/:id", cfg.ChapterHandler.DeleteChapter)
			chapters.POST("/:id/regenerate", cfg.ChapterHandler.RegenerateChapter)
			chapters.GET("/:id/versions", cfg.ChapterHandler.GetVersions)
			chapters.POST("/:id/versions/:version_no/restore", cfg.ChapterHandler.RestoreVersion)
			chapters.POST("/:id/quality-check", cfg.ChapterHandler.QualityCheck)
			chapters.GET("/:id/quality-report", cfg.ChapterHandler.GetQualityReport)
			chapters.POST("/:id/improve", cfg.ChapterHandler.RefineChapter)
			chapters.POST("/:id/approve", cfg.ChapterHandler.ApproveChapter)
			chapters.POST("/:id/reject", cfg.ChapterHandler.RejectChapter)

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
				plotPoints.PUT("/:id", cfg.PlotPointHandler.Update)
				plotPoints.PUT("/:id/resolve", cfg.PlotPointHandler.MarkResolved)
				plotPoints.DELETE("/:id", cfg.PlotPointHandler.Delete)
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
				sceneAnchors.POST("/:id/generate-ref-image", cfg.SceneAnchorHandler.GenerateRefImage)
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
			characters.POST("/:id/portrait/upload", cfg.CharacterHandler.UploadPortrait)
			characters.POST("/:id/analyze-consistency", cfg.CharacterHandler.AnalyzeCharacterConsistency)
			characters.POST("/:id/voice/preview", cfg.CharacterHandler.PreviewVoice)
			characters.GET("/:id/voice/sample", cfg.CharacterHandler.ServeVoiceSample)
		}

		// 视频相关
		videos := v1.Group("/videos")
		{
			videos.GET("", cfg.VideoHandler.ListVideos)
			videos.GET("/providers", cfg.VideoHandler.ListVideoProviders)
			videos.GET("/:id", cfg.VideoHandler.GetVideo)
			videos.PUT("/:id", cfg.VideoHandler.UpdateVideo)
			videos.DELETE("/:id", cfg.VideoHandler.DeleteVideo)
			videos.GET("/:id/storyboard", cfg.VideoHandler.GetStoryboard)
			videos.PUT("/:id/storyboard/:shot_id", cfg.VideoHandler.UpdateStoryboardShot)
			videos.POST("/:id/storyboard/generate", cfg.VideoHandler.GenerateStoryboard)
			videos.POST("/:id/storyboard/review", cfg.VideoHandler.ReviewStoryboard)
			videos.POST("/:id/storyboard/optimize-from-review", cfg.VideoHandler.OptimizeStoryboardFromReview)
			videos.POST("/:id/generate", cfg.VideoHandler.StartVideoGeneration)
			videos.GET("/:id/status", cfg.VideoHandler.GetVideoStatus)
			videos.GET("/:id/shots", cfg.VideoHandler.ListShots)
			videos.POST("/:id/shots/generate", cfg.VideoHandler.GenerateShotVideos)
			videos.POST("/:id/shots/batch-generate", cfg.VideoHandler.BatchGenerateShots)
			videos.POST("/:id/shots/batch-images", cfg.VideoHandler.BatchGenerateShotImages)
			videos.POST("/:id/shots/batch-clips", cfg.VideoHandler.BatchGenerateShotClips)
			videos.POST("/:id/shots/insert", cfg.VideoHandler.InsertShot)
			videos.POST("/:id/shots/sfx", cfg.VideoHandler.BatchGenerateSFX)
			videos.POST("/:id/shots/sfx-tags", cfg.VideoHandler.AnalyzeSFXTags)
			videos.POST("/:id/shots/batch-voice", cfg.VideoHandler.BatchGenerateVoice)
			// BGM 背景音乐
			videos.GET("/:id/bgm/segments", cfg.VideoHandler.ListBGMSegments)
			videos.GET("/:id/bgm/search", cfg.VideoHandler.JamendoSearchBGM)
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
			// 音效条目（多条）
			videos.GET("/:id/shots/:shot_id/sfx-items", cfg.VideoHandler.ListShotSFXItems)
			videos.PUT("/:id/shots/:shot_id/sfx-items/:item_id", cfg.VideoHandler.UpdateShotSFXItem)
			videos.PATCH("/:id/shots/:shot_id/sfx-items/:item_id/disabled", cfg.VideoHandler.ToggleShotSFXItem)
			videos.DELETE("/:id/shots/:shot_id/sfx-items/:item_id", cfg.VideoHandler.DeleteShotSFXItem)
			// 语音段落
			videos.GET("/:id/shots/:shot_id/segments", cfg.VideoHandler.ListVoiceSegments)
			videos.POST("/:id/shots/:shot_id/segments", cfg.VideoHandler.AppendVoiceSegment)
			videos.POST("/:id/shots/:shot_id/segments/insert", cfg.VideoHandler.InsertVoiceSegment)
			videos.PUT("/:id/shots/:shot_id/segments/:seg_id", cfg.VideoHandler.UpdateVoiceSegment)
			videos.DELETE("/:id/shots/:shot_id/segments/:seg_id", cfg.VideoHandler.DeleteVoiceSegment)
			videos.POST("/:id/shots/:shot_id/segments/:seg_id/voice", cfg.VideoHandler.GenerateSegmentVoice)
			videos.GET("/:id/shots/:shot_id/segments/:seg_id/audio", cfg.VideoHandler.ServeSegmentAudio)
			videos.POST("/:id/storyboard/:shot_id/voice", cfg.VideoHandler.GenerateShotVoice)
			videos.GET("/:id/storyboard/:shot_id/audio", cfg.VideoHandler.ServeAudio)
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
			models.PUT("/:id", cfg.ModelHandler.UpdateModel)
			models.DELETE("/:id", cfg.ModelHandler.DeleteModel)
			models.POST("/:id/test", cfg.ModelHandler.TestModel)
			// MCP 工具绑定
			models.GET("/:id/mcp-tools", cfg.McpHandler.GetModelMcpTools)
			models.POST("/:id/mcp-tools", cfg.McpHandler.BindMcpTool)
			models.DELETE("/:id/mcp-tools/:tool_id", cfg.McpHandler.UnbindMcpTool)
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
				platformR.GET("/accounts", cfg.PlatformHandler.ListAccounts)
				platformR.GET("/accounts/oauth/:platform", cfg.PlatformHandler.ConnectAccount)
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
			}
		}
	}

	return r
}
