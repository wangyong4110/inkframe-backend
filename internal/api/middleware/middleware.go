package middleware

import (
	"bytes"
	"io"
	"math/rand"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// CORS 跨域中间件
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-Request-ID")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Type, X-Request-ID")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// RequestID 请求ID中间件
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// Logger 日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		// 读取请求体
		if c.Request.Body != nil && raw == "" {
			bodyBytes, _ := io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			if len(bodyBytes) > 0 && len(bodyBytes) < 1000 {
				raw = string(bodyBytes)
			}
		}

		// 处理请求
		c.Next()

		// 记录日志
		latency := time.Since(start)
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()

		if raw != "" {
			path = path + "?" + raw
		}

		gin.DefaultWriter.Write([]byte(
			time.Now().Format("2006/01/02 - 15:04:05") +
				" | " + statusCode2String(statusCode) +
				" | " + latency.String() +
				" | " + clientIP +
				" | " + method +
				" | " + path + "\n",
		)
	}
}

// statusCode2String 状态码转字符串
func statusCode2String(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Recovery 恢复中间件
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				gin.DefaultWriter.Write([]byte(
					"[PANIC RECOVERED] " +
						time.Now().Format("2006/01/02 15:04:05") +
						" | " + c.Request.Method +
						" | " + c.Request.URL.Path +
						" | " + interface2String(err) + "\n",
				))
				c.AbortWithStatusJSON(500, gin.H{
					"error": "Internal Server Error",
				})
			}
		}()
		c.Next()
	}
}

// interface2String 接口转字符串
func interface2String(i interface{}) string {
	switch v := i.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return "unknown error"
	}
}

// RateLimiter 限流中间件（简单实现）
func RateLimiter(limit int, window time.Duration) gin.HandlerFunc {
	tokens := make(chan struct{}, limit)
	for i := 0; i < limit; i++ {
		tokens <- struct{}{}
	}

	go func() {
		ticker := time.NewTicker(window)
		for range ticker.C {
			for i := 0; i < limit; i++ {
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}()

	return func(c *gin.Context) {
		select {
		case <-tokens:
			c.Next()
		default:
			c.AbortWithStatusJSON(429, gin.H{
				"error": "Too Many Requests",
			})
		}
	}
}

// Timeout 超时中间件
func Timeout(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		done := make(chan struct{})

		go func() {
			c.Next()
			close(done)
		}()

		select {
		case <-done:
			return
		case <-time.After(timeout):
			c.AbortWithStatusJSON(408, gin.H{
				"error": "Request Timeout",
			})
		}
	}
}

// RandomDelay 随机延迟中间件（用于测试）
func RandomDelay(min, max time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		delay := time.Duration(rand.Int63n(int64(max-min))) + min
		time.Sleep(delay)
		c.Next()
	}
}
