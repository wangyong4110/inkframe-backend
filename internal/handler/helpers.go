package handler

import "github.com/gin-gonic/gin"

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
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}
