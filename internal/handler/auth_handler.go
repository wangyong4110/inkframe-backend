package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// AuthHandler 认证处理器
type AuthHandler struct {
	authService *service.AuthService
	userRepo    interface {
		GetByID(id uint) (interface{}, error)
	}
}

func NewAuthHandler(authService *service.AuthService) *AuthHandler {
	return &AuthHandler{authService: authService}
}

// Register 注册
// POST /api/v1/auth/register
func (h *AuthHandler) Register(c *gin.Context) {
	var req service.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	resp, err := h.authService.Register(&req)
	if err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	respondCreated(c, resp)
}

// Login 登录
// POST /api/v1/auth/login
func (h *AuthHandler) Login(c *gin.Context) {
	var req service.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	resp, err := h.authService.Login(&req)
	if err != nil {
		respondErr(c, http.StatusUnauthorized, err.Error())
		return
	}

	respondOK(c, resp)
}

// RefreshToken 刷新令牌
// POST /api/v1/auth/refresh
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}

	resp, err := h.authService.RefreshToken(req.Token)
	if err != nil {
		respondErr(c, http.StatusUnauthorized, err.Error())
		return
	}

	respondOK(c, resp)
}

// GetCurrentUser 获取当前用户信息
// GET /api/v1/auth/me
func (h *AuthHandler) GetCurrentUser(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}

	tenantID, _ := c.Get("tenant_id")
	role, _ := c.Get("user_role")

	respondOK(c, gin.H{
		"user_id":   userID,
		"tenant_id": tenantID,
		"role":      role,
	})
}
