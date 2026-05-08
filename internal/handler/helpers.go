package handler

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/model"
)

// getTenantID extracts tenant_id injected by JWT middleware.
// Returns 0 if not present (falls back to system-level providers).
func getTenantID(c *gin.Context) uint {
	if v, ok := c.Get("tenant_id"); ok {
		if id, ok := v.(uint); ok {
			return id
		}
	}
	return 0
}

// maskAPIKey masks an API key for safe display in responses.
// e.g. "sk-abcdefgh1234" → "sk-a****1234"
// providerHasCredentialsRaw checks raw (unmasked) credentials.
func providerHasCredentialsRaw(p *model.ModelProvider) bool {
	switch p.Name {
	case "volcengine-visual", "doubao-speech-v1":
		return strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.APISecretKey) != ""
	case "ollama":
		return true // Ollama 无需 API Key，本地服务始终视为已配置
	}
	return strings.TrimSpace(p.APIKey) != ""
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}
