package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
)

// AuthHandler 认证处理器
type AuthHandler struct {
	authService  *service.AuthService
	smsService   *service.SMSService
	oauthService *service.OAuthService
	frontendURL  string
	userRepo     interface {
		GetByID(id uint) (interface{}, error)
	}
}

func NewAuthHandler(authService *service.AuthService) *AuthHandler {
	return &AuthHandler{authService: authService, frontendURL: "http://localhost:3000"}
}

// WithSMSService 注入短信服务
func (h *AuthHandler) WithSMSService(sms *service.SMSService) *AuthHandler {
	h.smsService = sms
	return h
}

// WithOAuthService 注入OAuth服务
func (h *AuthHandler) WithOAuthService(oauth *service.OAuthService) *AuthHandler {
	h.oauthService = oauth
	return h
}

// WithFrontendURL 设置前端URL（OAuth回调重定向用）
func (h *AuthHandler) WithFrontendURL(u string) *AuthHandler {
	h.frontendURL = u
	return h
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
	userIDVal, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}

	tenantID, _ := c.Get("tenant_id")
	role, _ := c.Get("user_role")
	userID := userIDVal.(uint)

	user, err := h.authService.GetUserByID(userID)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, "failed to fetch user")
		return
	}

	respondOK(c, gin.H{
		"user_id":   userID,
		"tenant_id": tenantID,
		"role":      role,
		"username":  user.Username,
		"nickname":  user.Nickname,
		"avatar":    user.Avatar,
		"email":     user.Email,
	})
}

// SendSMSCode 发送短信验证码
// POST /api/v1/auth/sms/send
func (h *AuthHandler) SendSMSCode(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	if h.smsService == nil {
		respondErr(c, http.StatusServiceUnavailable, "SMS service not configured")
		return
	}
	if err := h.smsService.SendCode(req.Phone); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "验证码已发送"})
}

// PhoneRegister 手机号注册
// POST /api/v1/auth/phone/register
func (h *AuthHandler) PhoneRegister(c *gin.Context) {
	var req struct {
		Phone      string `json:"phone" binding:"required"`
		Code       string `json:"code" binding:"required"`
		Nickname   string `json:"nickname"`
		TenantName string `json:"tenant_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	resp, err := h.authService.RegisterWithPhone(req.Phone, req.Code, req.Nickname, req.TenantName)
	if err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	respondCreated(c, resp)
}

// PhoneLogin 手机号验证码登录
// POST /api/v1/auth/phone/login
func (h *AuthHandler) PhoneLogin(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
		Code  string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	resp, err := h.authService.LoginWithPhone(req.Phone, req.Code)
	if err != nil {
		respondErr(c, http.StatusUnauthorized, err.Error())
		return
	}
	respondOK(c, resp)
}

// OAuthURL 获取OAuth授权URL
// GET /api/v1/auth/oauth/:provider/url?state=xxx
func (h *AuthHandler) OAuthURL(c *gin.Context) {
	provider := c.Param("provider")
	state := c.Query("state")
	if state == "" {
		state = "default"
	}
	if h.oauthService == nil {
		respondErr(c, http.StatusServiceUnavailable, "OAuth service not configured")
		return
	}
	authURL, err := h.oauthService.GetAuthURL(provider, state)
	if err != nil {
		respondBadRequest(c, err.Error())
		return
	}
	respondOK(c, gin.H{"url": authURL})
}

// OAuthCallback OAuth回调
// GET /api/v1/auth/oauth/:provider/callback?code=xxx&state=xxx
func (h *AuthHandler) OAuthCallback(c *gin.Context) {
	provider := c.Param("provider")
	code := c.Query("code")
	if code == "" {
		c.Redirect(http.StatusFound, h.frontendURL+"/auth/login?error=oauth_failed")
		return
	}
	if h.oauthService == nil {
		c.Redirect(http.StatusFound, h.frontendURL+"/auth/login?error=oauth_not_configured")
		return
	}

	userInfo, err := h.oauthService.ExchangeUserInfo(provider, code)
	if err != nil {
		c.Redirect(http.StatusFound, h.frontendURL+"/auth/login?error=oauth_exchange_failed")
		return
	}

	authResp, err := h.authService.LoginWithOAuth(userInfo)
	if err != nil {
		c.Redirect(http.StatusFound, h.frontendURL+"/auth/login?error=login_failed")
		return
	}

	redirectURL := fmt.Sprintf("%s/auth/oauth-callback?token=%s&expires_at=%s",
		h.frontendURL,
		authResp.Token,
		authResp.ExpiresAt.Format("2006-01-02T15:04:05Z"),
	)
	c.Redirect(http.StatusFound, redirectURL)
}
