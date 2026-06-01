package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Config 应用配置
type Config struct {
	// 服务器配置
	Server ServerConfig `mapstructure:"server"`

	// 数据库配置
	Database DatabaseConfig `mapstructure:"database"`

	// Redis配置
	Redis RedisConfig `mapstructure:"redis"`

	// 文件存储配置
	Storage StorageConfig `mapstructure:"storage"`

	// 向量数据库配置
	VectorDB VectorDBConfig `mapstructure:"vector_db"`

	// 日志配置
	Logger LoggerConfig `mapstructure:"logger"`

	// 短信配置
	SMS SMSConfig `mapstructure:"sms"`

	// OAuth配置
	OAuth OAuthConfig `mapstructure:"oauth"`

	// 联网搜索配置
	WebSearch WebSearchConfig `mapstructure:"web_search"`

	// 爬取配置
	Crawl CrawlConfig `mapstructure:"crawl"`

	// 邮件配置
	Email EmailConfig `mapstructure:"email"`
}

// EmailConfig 邮件配置
// 支持两种发送方式（二选一）：
//   - SMTP：填 host/port/username/password/from/use_tls
//   - HTTP Webhook：填 webhook_url（+ 可选 webhook_token），SMTP 字段留空
type EmailConfig struct {
	// SMTP 配置
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	From     string `mapstructure:"from"`
	UseTLS   bool   `mapstructure:"use_tls"`

	// HTTP Webhook 配置（SMTP 不可用时使用）
	// POST {"to":"...","subject":"...","body":"..."} 到 webhook_url
	// webhook_token 非空时附加 Authorization: Bearer <token> 请求头
	WebhookURL   string `mapstructure:"webhook_url"`
	WebhookToken string `mapstructure:"webhook_token"`

	// RequireVerification 为 true 时，邮箱注册必须完成验证才能登录。
	// 默认 false：注册即激活，无需验证邮件。
	RequireVerification bool `mapstructure:"require_verification"`

	// EmailVerifyTTL 验证邮件链接的有效时长，默认 1h。
	// 支持 Go duration 格式：30m、1h、24h 等。
	EmailVerifyTTL time.Duration `mapstructure:"email_verify_ttl"`
}

// CrawlConfig 资产爬取配置
type CrawlConfig struct {
	// ProxyURL 用于访问国内外受限站点的 HTTP/HTTPS 代理。
	// 留空则使用系统默认（HTTPS_PROXY 环境变量）。
	// 示例：http://127.0.0.1:7890 或 socks5://127.0.0.1:1080
	// 对应环境变量：CRAWL_PROXY_URL
	ProxyURL    string `mapstructure:"proxy_url"`
	UnsplashKey string `mapstructure:"unsplash_key"` // 对应环境变量 UNSPLASH_ACCESS_KEY
}

// WebSearchConfig 联网搜索配置
type WebSearchConfig struct {
	Provider string `mapstructure:"provider"` // "bing"|"searxng"|"tavily"|""
	APIKey   string `mapstructure:"api_key"`  // Bing/Tavily key（也可用 WEB_SEARCH_API_KEY 环境变量）
	Endpoint string `mapstructure:"endpoint"` // SearXNG: http://your-host
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	Mode            string        `mapstructure:"mode"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	MaxHeaderBytes  int           `mapstructure:"max_header_bytes"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	JWTSecret       string        `mapstructure:"jwt_secret"`
	JWTExpiry       time.Duration `mapstructure:"jwt_expiry"`
	FrontendURL     string        `mapstructure:"frontend_url"`
	// AppName 应用名称，用于邮件主题、正文等对外展示。留空默认"简影"。
	AppName         string        `mapstructure:"app_name"`
	// AllowedOrigins 允许的 CORS 来源列表。留空表示允许所有来源（开发模式兼容）。
	// 生产环境应设置为前端 URL，如 ["https://app.example.com"]。
	AllowedOrigins []string `mapstructure:"allowed_origins"`
	// EncryptionKey 用于 AES-256-GCM 加密 DB 中存储的 API Key。
	// 32 字节以内的字符串（不足则补零）。留空时禁用加密（开发兼容）。
	EncryptionKey string `mapstructure:"encryption_key"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Driver          string        `mapstructure:"driver"`
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	Database        string        `mapstructure:"database"`
	Username        string        `mapstructure:"username"`
	Password        string        `mapstructure:"password"`
	Charset         string        `mapstructure:"charset"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	TablePrefix     string        `mapstructure:"table_prefix"`
}

// RedisConfig Redis配置
type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

// StorageConfig 文件存储配置
type StorageConfig struct {
	Type      string `mapstructure:"type"`
	Endpoint  string `mapstructure:"endpoint"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	BaseURL   string `mapstructure:"base_url"`
}

// VectorDBConfig 向量数据库配置
type VectorDBConfig struct {
	Type      string `mapstructure:"type"`
	Endpoint  string `mapstructure:"endpoint"`
	APIKey    string `mapstructure:"api_key"`
	IndexName string `mapstructure:"index_name"`
}

// LoggerConfig 日志配置
type LoggerConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	OutputPath string `mapstructure:"output_path"`
}

// SMSConfig 阿里云短信配置
type SMSConfig struct {
	AccessKeyID     string `mapstructure:"access_key_id"`
	AccessKeySecret string `mapstructure:"access_key_secret"`
	SignName        string `mapstructure:"sign_name"`
	TemplateCode    string `mapstructure:"template_code"`
}

// OAuthProviderConfig OAuth提供商配置
type OAuthProviderConfig struct {
	AppID       string `mapstructure:"app_id"`
	AppSecret   string `mapstructure:"app_secret"`
	RedirectURI string `mapstructure:"redirect_uri"`
}

// AlipayConfig 支付宝OAuth配置
type AlipayConfig struct {
	AppID       string `mapstructure:"app_id"`
	PrivateKey  string `mapstructure:"private_key"`
	PublicKey   string `mapstructure:"public_key"`
	RedirectURI string `mapstructure:"redirect_uri"`
}

// OAuthConfig OAuth配置
type OAuthConfig struct {
	Wechat OAuthProviderConfig `mapstructure:"wechat"`
	Alipay AlipayConfig        `mapstructure:"alipay"`
	Douyin OAuthProviderConfig `mapstructure:"douyin"`
}

// Load 加载配置
func Load(configPath string) (*Config, error) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	// 设置默认值
	setDefaults()

	// 读取配置
	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &cfg, nil
}

// setDefaults 设置默认值
func setDefaults() {
	// 服务器默认值
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("server.mode", "debug")
	viper.SetDefault("server.read_timeout", 30*time.Second)
	viper.SetDefault("server.write_timeout", 30*time.Second)
	viper.SetDefault("server.max_header_bytes", 1<<20)
	viper.SetDefault("server.shutdown_timeout", 10*time.Second)
	viper.SetDefault("server.jwt_secret", "change-me-in-production")
	viper.SetDefault("server.jwt_expiry", 24*time.Hour)
	viper.SetDefault("server.frontend_url", "http://localhost:3000")

	// 数据库默认值
	viper.SetDefault("database.driver", "mysql")
	viper.SetDefault("database.charset", "utf8mb4")
	viper.SetDefault("database.max_idle_conns", 10)
	viper.SetDefault("database.max_open_conns", 100)
	viper.SetDefault("database.conn_max_lifetime", time.Hour)
	viper.SetDefault("database.table_prefix", "ink_")

	// Redis默认值
	viper.SetDefault("redis.db", 0)
	viper.SetDefault("redis.pool_size", 100)

	// 日志默认值
	viper.SetDefault("logger.level", "info")
	viper.SetDefault("logger.format", "json")
	viper.SetDefault("logger.output_path", "stdout")
}

// GetDSN 获取数据库DSN
func (c *DatabaseConfig) GetDSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=True&loc=Local",
		c.Username, c.Password, c.Host, c.Port, c.Database, c.Charset)
}

// GetAddr 获取服务器地址
func (c *ServerConfig) GetAddr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// GetRedisAddr 获取Redis地址
func (c *RedisConfig) GetRedisAddr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:            "0.0.0.0",
			Port:            8080,
			Mode:            "debug",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    300 * time.Second, // AI 图片生成等长耗时接口需要更长的写超时
			MaxHeaderBytes:  1 << 20,
			ShutdownTimeout: 10 * time.Second,
			JWTSecret:       "change-me-in-production",
			JWTExpiry:       24 * time.Hour,
		},
		Database: DatabaseConfig{
			Driver:          "mysql",
			Charset:         "utf8mb4",
			MaxIdleConns:    10,
			MaxOpenConns:    100,
			ConnMaxLifetime: time.Hour,
			TablePrefix:     "ink_",
		},
		Redis: RedisConfig{
			Host:     "localhost",
			Port:     6379,
			DB:       0,
			PoolSize: 100,
		},
		Logger: LoggerConfig{
			Level:      "info",
			Format:     "json",
			OutputPath: "stdout",
		},
	}
}
