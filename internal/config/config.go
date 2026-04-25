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

	// AI模型配置
	AI AIConfig `mapstructure:"ai"`

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
	ConnMaxLifetime  time.Duration `mapstructure:"conn_max_lifetime"`
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

// AIConfig AI模型配置
type AIConfig struct {
	// OpenAI配置
	OpenAI OpenAIConfig `mapstructure:"openai"`

	// Anthropic配置
	Anthropic AnthropicConfig `mapstructure:"anthropic"`

	// Google配置
	Google GoogleConfig `mapstructure:"google"`

	// 豆包配置（字节跳动火山引擎 Ark，含 Seedream 图像）
	Doubao DoubaoConfig `mapstructure:"doubao"`

	// DeepSeek配置
	DeepSeek DeepSeekConfig `mapstructure:"deepseek"`

	// 通义千问配置（阿里云百炼/DashScope，含 Wan 图像）
	Qianwen QianwenConfig `mapstructure:"qianwen"`

	// Seedance视频配置（字节跳动火山引擎 Ark）
	Seedance SeedanceConfig `mapstructure:"seedance"`

	// 默认配置
	Default DefaultAIConfig `mapstructure:"default"`
}

// OpenAIConfig OpenAI配置
type OpenAIConfig struct {
	APIKey    string `mapstructure:"api_key"`
	Endpoint  string `mapstructure:"endpoint"`
	Model     string `mapstructure:"model"`
	MaxTokens int    `mapstructure:"max_tokens"`
}

// AnthropicConfig Anthropic配置
type AnthropicConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
	Model    string `mapstructure:"model"`
}

// GoogleConfig Google配置
type GoogleConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
	Model    string `mapstructure:"model"`
}

// DoubaoConfig 豆包配置
type DoubaoConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
	Model    string `mapstructure:"model"`
}

// DeepSeekConfig DeepSeek配置
type DeepSeekConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
	Model    string `mapstructure:"model"`
}

// QianwenConfig 通义千问配置
type QianwenConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
	Model    string `mapstructure:"model"`
}

// SeedanceConfig Seedance视频配置
type SeedanceConfig struct {
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"`
}

// DefaultAIConfig 默认AI配置
type DefaultAIConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top_p"`
	TopK        int     `mapstructure:"top_k"`
}

// StorageConfig 文件存储配置
type StorageConfig struct {
	Type       string `mapstructure:"type"`
	Endpoint   string `mapstructure:"endpoint"`
	AccessKey  string `mapstructure:"access_key"`
	SecretKey  string `mapstructure:"secret_key"`
	Bucket     string `mapstructure:"bucket"`
	Region     string `mapstructure:"region"`
	BaseURL    string `mapstructure:"base_url"`
}

// VectorDBConfig 向量数据库配置
type VectorDBConfig struct {
	Type       string `mapstructure:"type"`
	Endpoint   string `mapstructure:"endpoint"`
	APIKey     string `mapstructure:"api_key"`
	IndexName  string `mapstructure:"index_name"`
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

	// AI默认值
	viper.SetDefault("ai.default.temperature", 0.7)
	viper.SetDefault("ai.default.top_p", 0.9)
	viper.SetDefault("ai.default.top_k", 40)

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
			WriteTimeout:    30 * time.Second,
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
		AI: AIConfig{
			Default: DefaultAIConfig{
				Temperature: 0.7,
				TopP:        0.9,
				TopK:        40,
			},
		},
		Logger: LoggerConfig{
			Level:      "info",
			Format:     "json",
			OutputPath: "stdout",
		},
	}
}
