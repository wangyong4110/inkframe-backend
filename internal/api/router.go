package api

import (
	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/api/handler"
	"github.com/inkframe/inkframe-backend/internal/api/middleware"
)

// SetupRouter 设置路由
func SetupRouter(
	novelHandler *handler.NovelHandler,
	chapterHandler *handler.ChapterHandler,
	modelHandler *handler.ModelHandler,
	videoHandler *handler.VideoHandler,
	healthHandler *handler.HealthHandler,
) *gin.Engine {
	r := gin.New()

	// 全局中间件
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.CORS())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger())

	// 健康检查
	r.GET("/health", healthHandler.Check)

	// API v1 路由组
	v1 := r.Group("/api/v1")
	{
		// 小说路由
		novels := v1.Group("/novels")
		{
			novels.GET("", novelHandler.List)
			novels.POST("", novelHandler.Create)
			novels.GET("/:id", novelHandler.Get)
			novels.PUT("/:id", novelHandler.Update)
			novels.DELETE("/:id", novelHandler.Delete)
			novels.POST("/:id/outline", novelHandler.GenerateOutline)

			// 章节路由
			novels.GET("/:novel_id/chapters", chapterHandler.List)
			novels.POST("/:novel_id/chapters", chapterHandler.Generate)
		}

		// 模型路由
		models := v1.Group("/models")
		{
			models.GET("", modelHandler.ListModels)
			models.GET("/providers", modelHandler.ListProviders)
			models.GET("/available/:task_type", modelHandler.ListModels)
		}

		// 模型选择路由
		model := v1.Group("/model")
		{
			model.POST("/select", modelHandler.SelectModel)
		}

		// 视频路由
		videos := v1.Group("/videos")
		{
			videos.POST("", videoHandler.Create)
			videos.GET("/:id", func(c *gin.Context) {
				c.JSON(200, gin.H{"message": "TODO"})
			})
			videos.POST("/:id/storyboard", videoHandler.GenerateStoryboard)
		}
	}

	return r
}
