package handler

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/inkframe/inkframe-backend/internal/service"
	"gorm.io/gorm"
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
	if !bindJSON(c, &req) {
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
	if !bindJSON(c, &req) {
		return
	}

	resp, err := h.authService.Login(&req)
	if err != nil {
		respondErr(c, http.StatusUnauthorized, err.Error())
		return
	}

	respondOK(c, resp)
}

// Logout 退出登录，将当前 Token 加入黑名单使其立即失效
// POST /api/v1/auth/logout
func (h *AuthHandler) Logout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	tokenStr := ""
	if parts := strings.SplitN(authHeader, " ", 2); len(parts) == 2 {
		tokenStr = parts[1]
	}
	if err := h.authService.Logout(tokenStr); err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "logged out"})
}

// RefreshToken 刷新令牌
// POST /api/v1/auth/refresh
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if !bindJSON(c, &req) {
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
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// User not in DB (e.g. dev bypass mode with user_id=1 but no seed).
			// Return partial info from JWT context so the frontend can proceed.
			respondOK(c, gin.H{
				"user_id":   userID,
				"tenant_id": tenantID,
				"role":      role,
				"username":  "dev",
				"nickname":  "Dev User",
				"avatar":    "",
				"email":     "",
			})
			return
		}
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

// UpdateProfile 更新当前用户资料
// PUT /api/v1/auth/me
func (h *AuthHandler) UpdateProfile(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	userID := userIDVal.(uint)

	var req struct {
		Nickname string `json:"nickname"`
		Email    string `json:"email"`
		Avatar   string `json:"avatar"`
	}
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.authService.UpdateProfile(userID, req.Nickname, req.Email, req.Avatar)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Dev bypass mode: no actual user record in DB; echo back the requested values.
			respondOK(c, gin.H{
				"user_id":  userID,
				"username": "dev",
				"nickname": req.Nickname,
				"avatar":   req.Avatar,
				"email":    req.Email,
			})
			return
		}
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{
		"user_id":  user.ID,
		"username": user.Username,
		"nickname": user.Nickname,
		"avatar":   user.Avatar,
		"email":    user.Email,
	})
}

// ChangePassword 修改当前用户密码
// PUT /api/v1/auth/me/password
func (h *AuthHandler) ChangePassword(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	userID := userIDVal.(uint)

	var req struct {
		OldPassword string `json:"old_password" binding:"required"`
		NewPassword string `json:"new_password" binding:"required,min=8"`
	}
	if !bindJSON(c, &req) {
		return
	}

	if err := h.authService.ChangePassword(userID, req.OldPassword, req.NewPassword); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Dev bypass mode: no actual user record in DB.
			respondOK(c, gin.H{"changed": true})
			return
		}
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"changed": true})
}

// DeleteAccount 注销当前账号
// DELETE /api/v1/auth/me
func (h *AuthHandler) DeleteAccount(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	userID := userIDVal.(uint)

	var req struct {
		Password string `json:"password"`
	}
	// Body is optional — OAuth-only accounts may have no password.
	_ = c.ShouldBindJSON(&req)

	if err := h.authService.DeleteAccount(userID, req.Password); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"deleted": true})
}

// SendSMSCode 发送短信验证码
// POST /api/v1/auth/sms/send
func (h *AuthHandler) SendSMSCode(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if !bindJSON(c, &req) {
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
	if !bindJSON(c, &req) {
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
	if !bindJSON(c, &req) {
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
		b := make([]byte, 16)
		if _, err := rand.Read(b); err == nil {
			state = hex.EncodeToString(b)
		} else {
			state = fmt.Sprintf("s%d", len(state))
		}
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
	// Store state in an HttpOnly cookie so OAuthCallback can verify it (CSRF protection).
	c.SetCookie("oauth_state", state, 600, "/", "", false, true)
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

	// Validate state parameter against the cookie to prevent CSRF.
	cookieState, err := c.Cookie("oauth_state")
	if err != nil || cookieState == "" || cookieState != c.Query("state") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid oauth state"})
		return
	}
	// Consume the state cookie immediately after validation.
	c.SetCookie("oauth_state", "", -1, "/", "", false, true)

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

	// Use URL fragment (#) instead of query string to prevent token leakage via
	// Referer headers and server access logs.
	redirectURL := fmt.Sprintf("%s/auth/oauth-callback#token=%s&expires_at=%s",
		h.frontendURL,
		authResp.Token,
		authResp.ExpiresAt.Format("2006-01-02T15:04:05Z"),
	)
	c.Redirect(http.StatusFound, redirectURL)
}

// ─────────────────────────────────────────────────────────────────────
// 密码重置 handlers
// ─────────────────────────────────────────────────────────────────────

// RequestPasswordReset 发起密码重置（无论邮箱是否存在，均返回相同响应，防枚举）
// POST /api/v1/auth/password-reset/request
func (h *AuthHandler) RequestPasswordReset(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if !bindJSON(c, &req) {
		return
	}
	// 静默处理错误：无论用户是否存在，前端都收到相同的成功响应
	_, _ = h.authService.RequestPasswordReset(req.Email)
	respondOK(c, gin.H{"message": "if the email exists, a reset link has been sent"})
}

// ResetPassword 使用重置 token 设置新密码
// POST /api/v1/auth/password-reset/confirm
func (h *AuthHandler) ResetPassword(c *gin.Context) {
	var req struct {
		Token       string `json:"token" binding:"required"`
		NewPassword string `json:"new_password" binding:"required,min=8"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.authService.ResetPassword(req.Token, req.NewPassword); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "password has been reset"})
}

// ─────────────────────────────────────────────────────────────────────
// 邮箱验证 handlers
// ─────────────────────────────────────────────────────────────────────

// SendEmailVerification 为当前登录用户发送邮箱验证令牌（需要 JWT）
// POST /api/v1/auth/email-verification/send
func (h *AuthHandler) SendEmailVerification(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		respondErr(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	uid := userIDVal.(uint)
	_, err := h.authService.SendEmailVerification(uid)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "verification email sent"})
}

// VerifyEmail 通过 token 验证邮箱（无需登录）
// GET /api/v1/auth/email-verification/verify?token=xxx
func (h *AuthHandler) VerifyEmail(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		respondErr(c, http.StatusBadRequest, "token is required")
		return
	}
	if err := h.authService.VerifyEmail(token); err != nil {
		respondErr(c, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(c, gin.H{"message": "email verified"})
}
