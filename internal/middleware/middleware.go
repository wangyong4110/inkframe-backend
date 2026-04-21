package middleware

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// tokenBucket 令牌桶
type tokenBucket struct {
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	lastTime time.Time
	mu       sync.Mutex
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastTime).Seconds()
	b.lastTime = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// rateLimitStore IP -> tokenBucket
var (
	rateLimitStore   sync.Map
	rateLimitCleanup sync.Once
)

func getBucket(ip string, capacity, rate float64) *tokenBucket {
	// 启动周期清理（每 10 分钟清除 30 分钟内未访问的桶，防止内存无限增长）
	rateLimitCleanup.Do(func() {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			for range ticker.C {
				cutoff := time.Now().Add(-30 * time.Minute)
				rateLimitStore.Range(func(k, v interface{}) bool {
					b := v.(*tokenBucket)
					b.mu.Lock()
					stale := b.lastTime.Before(cutoff)
					b.mu.Unlock()
					if stale {
						rateLimitStore.Delete(k)
					}
					return true
				})
			}
		}()
	})

	v, ok := rateLimitStore.Load(ip)
	if ok {
		return v.(*tokenBucket)
	}
	b := &tokenBucket{
		tokens:   capacity,
		capacity: capacity,
		rate:     rate,
		lastTime: time.Now(),
	}
	actual, _ := rateLimitStore.LoadOrStore(ip, b)
	return actual.(*tokenBucket)
}

// Logger 日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		log.Printf("[%s] %s %s %d %v",
			time.Now().Format("2006-01-02 15:04:05"),
			method,
			path,
			status,
			latency,
		)
	}
}

// Recovery 恢复中间件
func Recovery() gin.HandlerFunc {
	return gin.Recovery()
}

// CORS 跨域中间件
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-Requested-With")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers")
		c.Header("Access-Control-Max-Age", "172800")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// JWTClaims JWT声明
type JWTClaims struct {
	UserID   uint   `json:"user_id"`
	TenantID uint   `json:"tenant_id"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// NewAuth 创建JWT认证中间件
func NewAuth(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
			return
		}

		tokenStr := parts[1]
		claims := &JWTClaims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set("user_id", claims.UserID)
		c.Set("tenant_id", claims.TenantID)
		c.Set("user_role", claims.Role)
		c.Next()
	}
}

// Auth 认证中间件（保留空实现以兼容旧代码）
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}

// RateLimit 限流中间件
// capacity: 令牌桶容量（突发请求数）, rate: 每秒补充速率
func RateLimit(capacity float64, rate float64) gin.HandlerFunc {
	if capacity <= 0 {
		capacity = 60 // default: 60 burst
	}
	if rate <= 0 {
		rate = 10 // default: 10 req/s
	}
	return func(c *gin.Context) {
		ip := c.ClientIP()
		bucket := getBucket(ip, capacity, rate)
		if !bucket.allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, please slow down",
			})
			return
		}
		c.Next()
	}
}
