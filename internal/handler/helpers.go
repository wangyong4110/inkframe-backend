package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// parseID parses a uint route parameter, writing a 400 response on failure.
// Returns (id, true) on success; (0, false) on failure.
// Rejects zero values — all entity IDs must be positive integers.
func parseID(c *gin.Context, param string) (uint, bool) {
	v, err := strconv.ParseUint(c.Param(param), 10, 32)
	if err != nil || v == 0 {
		respondBadRequest(c, "invalid "+param+": must be a positive integer")
		return 0, false
	}
	return uint(v), true
}

// bindJSON deserialises the JSON request body into v.
// Returns true on success; writes a 400 response and returns false on failure.
func bindJSON(c *gin.Context, v interface{}) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		respondBadRequest(c, fmt.Sprintf("invalid request: %v", err))
		return false
	}
	return true
}

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

// isAdminOrOwner returns true if the requesting user holds the system "admin"
// role or the tenant-scoped "owner" or "admin" role.
// This is a lightweight check based solely on the JWT claim — it does not hit
// the database and is therefore safe to call on every mutating request.
func isAdminOrOwner(c *gin.Context) bool {
	role := c.GetString("user_role")
	return role == "admin" || role == "owner"
}

// maskAPIKey masks an API key for safe display in responses.
// e.g. "sk-abcdefgh1234" → "sk-a****1234"
// providerHasCredentialsRaw checks raw (unmasked) credentials.
func providerHasCredentialsRaw(p *model.ModelProvider) bool {
	switch p.Name {
	case "volcengine-visual", "doubao-speech-v1",
		"kling", "kling-sfx", "kling-tts", "kling-image":
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

// requireNovelEditorRole checks that the user has at least "editor" role for the given novel.
// Returns false and writes 403 if the check fails. Skips check when novelSvc is nil.
func requireNovelEditorRole(c *gin.Context, novelSvc *service.NovelService, novelID uint) bool {
	if novelSvc == nil {
		return true
	}
	role := novelSvc.GetRoleForUser(novelID, getTenantID(c), getUserIDFromCtx(c))
	if role != "editor" && role != "owner" {
		respondErr(c, http.StatusForbidden, "需要编辑权限")
		return false
	}
	return true
}

// taskFail 标记任务失败并记录日志；若 taskSvc.Fail 本身出错也打印。
func taskFail(svc interface{ Fail(string, string) error }, taskID, reason string) {
	if err := svc.Fail(taskID, reason); err != nil {
		logger.Errorf("[task] Fail(%s) error: %v", taskID, err)
	}
}

// taskComplete 标记任务完成并记录日志；若 taskSvc.Complete 本身出错也打印。
func taskComplete(svc interface{ Complete(string, interface{}) error }, taskID string, data interface{}) {
	if err := svc.Complete(taskID, data); err != nil {
		logger.Errorf("[task] Complete(%s) error: %v", taskID, err)
	}
}
