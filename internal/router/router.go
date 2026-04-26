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
}

// SetupRouter 配置路由
func SetupRouter(cfg *Config) *gin.Engine {
	r := gin.Default()

	// 全局中间件
	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())

	// 健康检查（公开）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

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

		// 导入
		importGroup := v1.Group("/import")
		{
			importGroup.POST("/novel", cfg.ImportHandler.ImportNovel)
			importGroup.POST("/novel/file", cfg.ImportHandler.ImportFromFile)
			importGroup.POST("/novel/url", cfg.ImportHandler.ImportFromURL)
			importGroup.POST("/novel/crawl", cfg.ImportHandler.ImportFromCrawl)
			importGroup.POST("/novel/video", cfg.ImportHandler.ImportAndGenerate)
			importGroup.GET("/status/:task_id", cfg.ImportHandler.GetImportStatus)
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
			novels.GET("/:id/chapters/:chapter_no", cfg.ChapterHandler.GetChapterByNo)
			novels.PUT("/:id/chapters/:chapter_no", cfg.ChapterHandler.UpdateChapterByNo)
			novels.DELETE("/:id/chapters/:chapter_no", cfg.ChapterHandler.DeleteChapterByNo)
			novels.POST("/:id/chapters/:chapter_no/character-snapshots", cfg.NovelHandler.SyncCharacterSnapshots)

			// 大纲
			novels.POST("/:id/outline", cfg.NovelHandler.GenerateOutline)

			// 角色
			novels.GET("/:id/characters", cfg.CharacterHandler.ListCharacters)
			novels.POST("/:id/characters", cfg.CharacterHandler.CreateCharacter)
			novels.POST("/:id/characters/generate", cfg.CharacterHandler.GenerateCharacterProfile)

			// 角色弧光
			novels.GET("/:id/character-arcs", cfg.CharacterHandler.GetAllCharacterArcs)
			novels.GET("/:id/character-arcs/:character_id", cfg.CharacterHandler.GetCharacterArc)
			novels.PUT("/:id/character-arcs/:character_id", cfg.CharacterHandler.UpdateCharacterArc)

			// 视频
			novels.GET("/:id/videos", cfg.VideoHandler.ListVideos)
			novels.POST("/:id/videos", cfg.VideoHandler.CreateVideo)

			// 从已有小说生成视频
			novels.POST("/:id/generate-video", cfg.ImportHandler.GenerateVideoFromNovel)

			// 分析导入的小说
			novels.POST("/:id/analyze", cfg.ImportHandler.StartAnalysis)
			novels.GET("/:id/analyze/status", cfg.ImportHandler.GetAnalysisStatus)

			// 爬取进度
			novels.GET("/:id/crawl/status", cfg.ImportHandler.GetCrawlStatus)
			novels.POST("/:id/crawl/resume", cfg.ImportHandler.ResumeCrawl)

			// 物品（项目级）
			if cfg.ItemHandler != nil {
				novels.GET("/:id/items", cfg.ItemHandler.ListItems)
				novels.POST("/:id/items", cfg.ItemHandler.CreateItem)
				// 章节级物品（有效列表 + 覆盖）
				novels.GET("/:id/chapters/:chapter_no/items", cfg.ItemHandler.ListEffectiveItems)
				novels.POST("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.UpsertChapterItem)
				novels.DELETE("/:id/chapters/:chapter_no/items/:item_id", cfg.ItemHandler.DeleteChapterItem)
			}

			// 章节级角色（有效列表 + 覆盖）
			novels.GET("/:id/chapters/:chapter_no/characters", cfg.CharacterHandler.ListEffectiveCharacters)
			novels.POST("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.UpsertChapterCharacter)
			novels.DELETE("/:id/chapters/:chapter_no/characters/:character_id", cfg.CharacterHandler.DeleteChapterCharacter)

			// 技能管理
			if cfg.SkillHandler != nil {
				novels.GET("/:id/skills", cfg.SkillHandler.ListSkills)
				novels.POST("/:id/skills", cfg.SkillHandler.CreateSkill)
				novels.POST("/:id/skills/generate", cfg.SkillHandler.GenerateSkills)
			}
		}

		// 技能（单个技能操作）
		if cfg.SkillHandler != nil {
			skills := v1.Group("/skills")
			{
				skills.GET("/:skillId", cfg.SkillHandler.GetSkill)
				skills.PUT("/:skillId", cfg.SkillHandler.UpdateSkill)
				skills.DELETE("/:skillId", cfg.SkillHandler.DeleteSkill)
			}
		}

		// 物品（单个物品操作）
		if cfg.ItemHandler != nil {
			items := v1.Group("/items")
			{
				items.GET("/:id", cfg.ItemHandler.GetItem)
				items.PUT("/:id", cfg.ItemHandler.UpdateItem)
				items.DELETE("/:id", cfg.ItemHandler.DeleteItem)
				items.POST("/:id/images", cfg.ItemHandler.GenerateItemImage)
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
			chapters.POST("/:id/approve", cfg.ChapterHandler.ApproveChapter)
			chapters.POST("/:id/reject", cfg.ChapterHandler.RejectChapter)
		}

		// 角色相关
		characters := v1.Group("/characters")
		{
			characters.GET("/:id", cfg.CharacterHandler.GetCharacter)
			characters.PUT("/:id", cfg.CharacterHandler.UpdateCharacter)
			characters.DELETE("/:id", cfg.CharacterHandler.DeleteCharacter)
			characters.POST("/:id/images", cfg.CharacterHandler.GenerateCharacterImage)
			characters.POST("/:id/analyze-consistency", cfg.CharacterHandler.AnalyzeCharacterConsistency)
		}

		// 视频相关
		videos := v1.Group("/videos")
		{
			videos.GET("", cfg.VideoHandler.ListVideos)
			videos.GET("/:id", cfg.VideoHandler.GetVideo)
			videos.PUT("/:id", cfg.VideoHandler.UpdateVideo)
			videos.DELETE("/:id", cfg.VideoHandler.DeleteVideo)
			videos.GET("/:id/storyboard", cfg.VideoHandler.GetStoryboard)
			videos.PUT("/:id/storyboard/:shot_id", cfg.VideoHandler.UpdateStoryboardShot)
			videos.POST("/:id/storyboard/generate", cfg.VideoHandler.GenerateStoryboard)
			videos.POST("/:id/generate", cfg.VideoHandler.StartVideoGeneration)
			videos.GET("/:id/status", cfg.VideoHandler.GetVideoStatus)
			videos.GET("/:id/shots", cfg.VideoHandler.ListShots)
			videos.POST("/:id/shots/generate", cfg.VideoHandler.GenerateShotVideos)
			videos.POST("/:id/shots/batch-generate", cfg.VideoHandler.BatchGenerateShots)
			videos.POST("/:id/shots/:shot_id/generate", cfg.VideoHandler.GenerateSingleShot)
			videos.POST("/:id/stitch", cfg.VideoHandler.StitchVideoHandler)
			videos.GET("/:id/export/capcut", cfg.VideoHandler.ExportCapCutDraft)
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
	}

	return r
}
