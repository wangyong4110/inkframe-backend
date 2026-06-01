package middleware

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
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

// Logger 日志中间件（跳过健康检查及任务轮询路径）
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		// 跳过高频无噪声路径：健康检查、任务状态轮询
		if path == "/health" || (c.Request.Method == "GET" && strings.HasPrefix(path, "/api/v1/tasks/")) {
			c.Next()
			return
		}

		start := time.Now()
		method := c.Request.Method

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		logger.Printf("[%s] %s %s %d %v",
			time.Now().Format("2006-01-02 15:04:05"),
			method,
			path,
			status,
			latency,
		)
	}
}

// Recovery 恢复中间件：捕获 panic，记录请求上下文 + 完整堆栈，返回 JSON 500。
// http.ErrAbortHandler 是 net/http 内部用于中断响应的哨兵值，不应拦截，直接 re-panic。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// net/http 用此值提前中断连接，不属于业务 panic，透传给上层
			if r == http.ErrAbortHandler {
				panic(r)
			}

			// 捕获最多 64 KB 的当前 goroutine 调用栈
			buf := make([]byte, 64<<10)
			n := runtime.Stack(buf, false)
			stack := buf[:n]

			logger.Printf("[PANIC] %v | %s %s | client=%s\n%s",
				r,
				c.Request.Method,
				c.Request.URL.RequestURI(),
				c.ClientIP(),
				stack,
			)

			// 若响应头尚未写出，返回标准 JSON 500；否则只能中止连接
			if c.Writer.Written() {
				c.Abort()
			} else {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"message": "internal server error",
				})
			}
		}()
		c.Next()
	}
}

// CORSMiddleware 跨域中间件。
// allowedOrigins 为空时允许所有来源（开发模式兼容，等同于旧的通配符行为）。
// 生产环境应传入前端 URL 列表，如 []string{"https://app.example.com"}。
func CORSMiddleware(allowedOrigins []string) gin.HandlerFunc {
	originSet := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = true
	}
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if len(allowedOrigins) == 0 || originSet[origin] {
			// 允许该来源：返回实际 origin（而非通配符），以兼容带凭据的请求
			c.Header("Access-Control-Allow-Origin", origin)
		}
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

// CORS 跨域中间件（向后兼容包装，允许所有来源）。
// 新代码请使用 CORSMiddleware(cfg.Server.AllowedOrigins)。
func CORS() gin.HandlerFunc {
	return CORSMiddleware(nil)
}

// JWTClaims JWT声明
type JWTClaims struct {
	UserID   uint   `json:"user_id"`
	TenantID uint   `json:"tenant_id"`
	Role     string `json:"role"`
	JTI      string `json:"jti"`
	jwt.RegisteredClaims
}

// NewAuth 创建JWT认证中间件。
// 当 jwtSecret 为空字符串时进入开发绕过模式：跳过 token 校验，
// 并将 tenant_id=1 / user_id=1 / user_role="admin" 注入上下文，
// 方便本地开发测试。生产环境务必配置非空的 jwt_secret。
//
// 安全保障：开发旁路模式仅在 GIN_MODE != release 且 APP_ENV != production 时生效，
// 防止生产配置缺失 jwt_secret 时意外暴露 API。
//
// rdb 为可选的 Redis 客户端，用于检查 JWT 黑名单（Logout 使 Token 立即失效）。
// 传 nil 则跳过黑名单检查（不影响正常认证）。
func NewAuth(jwtSecret string, rdb *redis.Client) gin.HandlerFunc {
	// Fix 2: dev-bypass is only allowed when NEITHER GIN_MODE=release NOR APP_ENV=production.
	isDevBypass := jwtSecret == ""
	if isDevBypass {
		if gin.Mode() == gin.ReleaseMode || os.Getenv("APP_ENV") == "production" {
			panic("FATAL: jwt_secret is empty in production mode. Set server.jwt_secret in config.yaml")
		}
		logger.Printf("[Auth] WARNING: jwt_secret empty — dev-bypass active, all requests granted (tenant=1)")
	} else if len(jwtSecret) < 32 {
		if gin.Mode() == gin.ReleaseMode || os.Getenv("APP_ENV") == "production" {
			panic("FATAL: jwt_secret is too short (must be at least 32 characters). Set a strong secret in config.yaml")
		}
		logger.Printf("[Auth] WARNING: jwt_secret is shorter than 32 characters — use a stronger secret in production")
	}
	return func(c *gin.Context) {
		// ── 开发绕过模式（jwt_secret 为空且非生产） ────────────────────────
		if isDevBypass {
			c.Set("user_id", uint(1))
			c.Set("tenant_id", uint(1))
			c.Set("user_role", "admin")
			c.Next()
			return
		}

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

		// ── 黑名单检查（若 Redis 可用且 JTI 不为空） ──────────────────────
		jti := claims.JTI
		if jti == "" {
			jti = claims.RegisteredClaims.ID
		}
		if rdb != nil && jti != "" {
			blacklistKey := "jwt:blacklist:" + jti
			exists, redisErr := rdb.Exists(context.Background(), blacklistKey).Result()
			if redisErr == nil && exists > 0 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
				return
			}
		}

		// ── 密码变更失效检查（若 Redis 可用） ─────────────────────────────
		if rdb != nil && claims.UserID > 0 {
			invalidateKey := fmt.Sprintf("jwt:user_invalidate:%d", claims.UserID)
			if val, redisErr := rdb.Get(context.Background(), invalidateKey).Result(); redisErr == nil {
				if ts, parseErr := strconv.ParseInt(val, 10, 64); parseErr == nil {
					if claims.RegisteredClaims.IssuedAt != nil && claims.RegisteredClaims.IssuedAt.Unix() < ts {
						c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
						return
					}
				}
			}
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

// MaxBodySize 请求体大小限制中间件。
// maxBytes: 最大允许的请求体字节数（超出时返回 413）。
// multipart/form-data 请求（文件上传）自动跳过，由各自处理器负责大小校验。
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
			c.Next()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// RequireEmailVerified rejects requests from users whose email is not verified.
// Pass the DB so it can fetch user.EmailVerifiedAt.
func RequireEmailVerified(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.Next()
			return
		}
		var user model.User
		if err := db.Select("email_verified_at").First(&user, userID).Error; err != nil {
			c.Next()
			return
		}
		// Fix 8: Also reject zero-value timestamps (default time.Time{}).
		if user.EmailVerifiedAt == nil || user.EmailVerifiedAt.IsZero() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code": 403, "message": "email not verified",
			})
			return
		}
		c.Next()
	}
}

// SecurityHeaders adds common security response headers to every request.
// HSTS is intentionally omitted — it should only be set when TLS is terminated
// by this server directly, not behind a reverse proxy.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
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
