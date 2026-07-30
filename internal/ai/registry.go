package ai

import (
	"fmt"
	"sync"
	"time"
)

// ProviderConfig 包含构造 Provider 所需的全部凭据和配置字段。
// 各工厂函数按需取用所需字段，忽略不相关的字段。
type ProviderConfig struct {
	APIKey       string
	APISecretKey string
	APIEndpoint  string
	ModelName    string
	APIVersion   string // 模型级 APIVersion（如 Doubao resourceID、Azure apiVersion）
	Timeout      time.Duration
	ModelType    string // "text" / "voice" / "image" / "video" 等
}

// ProviderFactory 是供应商工厂函数签名。
// 工厂根据 ProviderConfig 中的字段构造具体的 Provider 实例。
type ProviderFactory func(cfg ProviderConfig) (ProviderMeta, error)

var (
	registryMu sync.RWMutex
	registry   = make(map[string]ProviderFactory)
)

// Register 注册一个供应商工厂函数。
// 通常在供应商包的 init() 中调用。
// 重复注册同一名称会 panic（应在 init 阶段尽早暴露配置错误）。
func Register(name string, factory ProviderFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("ai: duplicate provider factory %q", name))
	}
	registry[name] = factory
}

// LookupFactory 按名称查找已注册的供应商工厂。
func LookupFactory(name string) (ProviderFactory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}
