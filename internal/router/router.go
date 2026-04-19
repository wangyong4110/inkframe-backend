package router

import (
	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/handler"
	"github.com/inkframe/inkframe-backend/internal/middleware"
)

// Config 路由配置
type Config struct {
	NovelHandler       *handler.NovelHandler
	ChapterHandler    *handler.ChapterHandler
	CharacterHandler  *handler.CharacterHandler
	VideoHandler      *handler.VideoHandler
	ModelHandler      *handler.ModelHandler
	StyleHandler      *handler.StyleHandler
	ContextHandler    *handler.ContextHandler
	ImportHandler     *handler.ImportHandler
}

// SetupRouter 配置路由
func SetupRouter(cfg *Config) *gin.Engine {
	r := gin.Default()

	// 中间件
	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// API v1
	v1 := r.Group("/api/v1")
	{
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

			// 从小说生成视频
			novels.POST("/:id/generate-video", cfg.ImportHandler.GenerateVideoFromNovel)

			// 章节
			novels.GET("/:novel_id/chapters", cfg.ChapterHandler.ListChapters)
			novels.POST("/:novel_id/chapters", cfg.ChapterHandler.CreateChapter)

			// 角色
			novels.GET("/:novel_id/characters", cfg.CharacterHandler.ListCharacters)
			novels.POST("/:novel_id/characters", cfg.CharacterHandler.CreateCharacter)

			// 角色弧光
			novels.GET("/:novel_id/character-arcs", cfg.CharacterHandler.GetAllCharacterArcs)
			novels.GET("/:novel_id/character-arcs/:character_id", cfg.CharacterHandler.GetCharacterArc)
			novels.PUT("/:novel_id/character-arcs/:character_id", cfg.CharacterHandler.UpdateCharacterArc)

			// 视频
			novels.GET("/:novel_id/videos", cfg.VideoHandler.ListVideos)
			novels.POST("/:novel_id/videos", cfg.VideoHandler.CreateVideo)
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
			videos.POST("/:id/storyboard/generate", cfg.VideoHandler.GenerateStoryboard)
			videos.POST("/:id/generate", cfg.VideoHandler.StartVideoGeneration)
			videos.GET("/:id/status", cfg.VideoHandler.GetVideoStatus)
		}

		// 分镜
		storyboard := v1.Group("/storyboard")
		{
			storyboard.POST("/generate", func(c *gin.Context) {
				// 转发到 video handler
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

		// 模型管理
		modelProviders := v1.Group("/model-providers")
		{
			modelProviders.GET("", cfg.ModelHandler.ListProviders)
			modelProviders.POST("", cfg.ModelHandler.CreateProvider)
			modelProviders.GET("/:id", func(c *gin.Context) {
				cfg.ModelHandler.ListProviders(c)
			})
			modelProviders.PUT("/:id", cfg.ModelHandler.UpdateProvider)
			modelProviders.DELETE("/:id", cfg.ModelHandler.DeleteProvider)
			modelProviders.POST("/:id/test", cfg.ModelHandler.TestProvider)
		}

		models := v1.Group("/models")
		{
			models.GET("", cfg.ModelHandler.ListModels)
			models.POST("", cfg.ModelHandler.CreateModel)
			models.PUT("/:id", cfg.ModelHandler.UpdateModel)
			models.DELETE("/:id", cfg.ModelHandler.DeleteModel)
			models.POST("/:id/test", cfg.ModelHandler.TestModel)
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

		// 风格控制
		styles := v1.Group("/styles")
		{
			styles.GET("/default", cfg.StyleHandler.GetDefaultStyle)
			styles.POST("/prompt", cfg.StyleHandler.BuildStylePrompt)
			styles.GET("/presets", cfg.StyleHandler.GetStylePresets)
			styles.POST("/presets/:name/apply", cfg.StyleHandler.ApplyStylePreset)
		}

		// 导入相关
		import := v1.Group("/import")
		{
			import.POST("/novel", cfg.ImportHandler.ImportNovel)
			import.POST("/novel/file", cfg.ImportHandler.ImportFromFile)
			import.POST("/novel/url", cfg.ImportHandler.ImportFromURL)
			import.POST("/novel/crawl", cfg.ImportHandler.ImportFromCrawl)
			import.POST("/novel/video", cfg.ImportHandler.ImportAndGenerate)
			import.GET("/status/:task_id", cfg.ImportHandler.GetImportStatus)
		}
	}

	return r
}
