package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/metrics"
)

// PrometheusMiddleware 记录每个 HTTP 请求的请求数、延迟和在途数。
// 路由标签使用 c.FullPath()（如 /api/v1/chapters/:id）避免高基数。
// 应在 Router.Use() 最前端注册，覆盖所有路由。
func PrometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		metrics.HTTPRequestsInFlight.Inc()
		defer metrics.HTTPRequestsInFlight.Dec()

		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		method := c.Request.Method
		status := strconv.Itoa(c.Writer.Status())
		elapsed := time.Since(start).Seconds()

		metrics.HTTPRequestsTotal.WithLabelValues(method, path, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(method, path, status).Observe(elapsed)
	}
}
