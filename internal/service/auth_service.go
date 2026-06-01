package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// AuthService 认证服务
type AuthService struct {
	db                  *gorm.DB
	userRepo            *repository.UserRepository
	tenantRepo          *repository.TenantRepository
	tuRepo              *repository.TenantUserRepository
	jwtSecret           string
	jwtExpiry           time.Duration
	smsService          *SMSService
	tokenRepo           *repository.UserTokenRepository
	rdb                 *redis.Client
	emailSender         EmailSender
	appBaseURL          string
	appName             string        // 邮件中展示的应用名称，默认"简影"
	emailVerifyTTL      time.Duration // 验证邮件链接有效时长，默认 1h
	requireVerification bool          // 邮箱注册是否需要验证后才能登录
}

func NewAuthService(
	db *gorm.DB,
	userRepo *repository.UserRepository,
	tenantRepo *repository.TenantRepository,
	tuRepo *repository.TenantUserRepository,
	jwtSecret string,
	jwtExpiry time.Duration,
) *AuthService {
	return &AuthService{
		db:         db,
		userRepo:   userRepo,
		tenantRepo: tenantRepo,
		tuRepo:     tuRepo,
		jwtSecret:  jwtSecret,
		jwtExpiry:  jwtExpiry,
	}
}

// WithSMSService 注入短信服务（可选）
func (s *AuthService) WithSMSService(sms *SMSService) *AuthService {
	s.smsService = sms
	return s
}

// WithTokenRepo 注入 UserToken 仓库（密码重置 & 邮箱验证）
func (s *AuthService) WithTokenRepo(r *repository.UserTokenRepository) *AuthService {
	s.tokenRepo = r
	return s
}

// WithRedis 注入 Redis 客户端（可选，用于 JWT 黑名单）
func (s *AuthService) WithRedis(rdb *redis.Client) *AuthService {
	s.rdb = rdb
	return s
}

// WithEmailSender 注入邮件发送服务（可选）
func (s *AuthService) WithEmailSender(sender EmailSender) *AuthService {
	s.emailSender = sender
	return s
}

// WithAppBaseURL 设置应用前端基础 URL（用于生成重置/验证链接）
func (s *AuthService) WithAppBaseURL(url string) *AuthService {
	s.appBaseURL = url
	return s
}

// WithAppName 设置邮件中显示的应用名称（默认"简影"）
func (s *AuthService) WithAppName(name string) *AuthService {
	s.appName = name
	return s
}

// WithEmailVerifyTTL 设置验证邮件链接有效时长（默认 1h）
func (s *AuthService) WithEmailVerifyTTL(ttl time.Duration) *AuthService {
	s.emailVerifyTTL = ttl
	return s
}

// emailAppName 返回配置的应用名称，未配置时回退到"简影"
func (s *AuthService) emailAppName() string {
	if s.appName != "" {
		return s.appName
	}
	return "简影"
}

// verifyTTL 返回验证链接有效时长，未配置时默认 1h
func (s *AuthService) verifyTTL() time.Duration {
	if s.emailVerifyTTL > 0 {
		return s.emailVerifyTTL
	}
	return time.Hour
}

// formatDuration 将 time.Duration 格式化为中文可读字符串
func formatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d 天", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}

// WithRequireVerification 设置邮箱注册是否必须验证后才能登录
func (s *AuthService) WithRequireVerification(v bool) *AuthService {
	s.requireVerification = v
	return s
}

// RegisterRequest 注册请求
type RegisterRequest struct {
	TenantName string `json:"tenant_name"` // 可选，为空时自动使用用户名
	Username   string `json:"username" binding:"required,min=3,max=50"`
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required,min=8"`
	Nickname   string `json:"nickname"`
}

// RegisterResponse 注册响应（邮箱验证模式下不返回 token，需先完成邮箱验证才可登录）
type RegisterResponse struct {
	Message   string `json:"message"`
	Email     string `json:"email"`
	ExpiresIn string `json:"expires_in"` // 验证链接有效时长，如"1 小时"
}

// LoginRequest 登录请求
type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse 认证响应
type AuthResponse struct {
	Token     string    `json:"token"`
	UserID    uint      `json:"user_id"`
	TenantID  uint      `json:"tenant_id"`
	Username  string    `json:"username"`
	Nickname  string    `json:"nickname"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Register 注册新用户并创建租户。
// 若 requireVerification=true：EmailVerifiedAt 留 nil，异步发送验证邮件，
//   用户须点击邮件链接后才能登录，返回 RegisterResponse（无 token）。
// 若 requireVerification=false（默认）：注册即激活，直接返回 JWT token。
func (s *AuthService) Register(req *RegisterRequest) (interface{}, error) {
	if req.TenantName == "" {
		req.TenantName = req.Username
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if existingUser, err := s.userRepo.GetByEmail(req.Email); err == nil {
		// 邮箱已存在：若开启验证且用户尚未验证，视为"重新发送验证邮件"而非报错
		if s.requireVerification && existingUser.EmailVerifiedAt == nil {
			go func() {
				if _, err := s.SendEmailVerification(existingUser.ID); err != nil {
					logger.Printf("[Register] resend verification for unverified user %d: %v", existingUser.ID, err)
				}
			}()
			return &RegisterResponse{
				Message:   "验证邮件已重新发送，请查收邮箱并点击链接完成验证后再登录",
				Email:     req.Email,
				ExpiresIn: formatDuration(s.verifyTTL()),
			}, nil
		}
		return nil, errors.New("email already registered")
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	var user *model.User
	var tenantID uint

	err = s.db.Transaction(func(tx *gorm.DB) error {
		u := &model.User{
			UUID:     uuid.New().String(),
			Username: req.Username,
			Email:    req.Email,
			Password: string(hashed),
			Nickname: req.Nickname,
			Status:   "active",
			Role:     "user",
		}
		// 不开启验证时，注册即标记为已验证
		if !s.requireVerification {
			now := time.Now()
			u.EmailVerifiedAt = &now
		}
		if err := tx.Create(u).Error; err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
		user = u

		tenant := &model.Tenant{
			Name:   req.TenantName,
			Code:   uuid.New().String()[:8],
			Plan:   "free",
			Status: "active",
		}
		if err := tx.Create(tenant).Error; err != nil {
			return fmt.Errorf("failed to create tenant: %w", err)
		}
		tenantID = tenant.ID

		tu := &model.TenantUser{
			TenantID: tenant.ID,
			UserID:   u.ID,
			Role:     "owner",
			Status:   "active",
		}
		if err := tx.Create(tu).Error; err != nil {
			return fmt.Errorf("failed to create tenant user: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 事务提交后再递增，避免在同一连接池内对未提交行加锁导致 50s 超时
	_ = s.tenantRepo.IncrUsedUsers(tenantID)

	if s.requireVerification {
		// 异步发送验证邮件，不阻塞注册响应
		go func() {
			if _, err := s.SendEmailVerification(user.ID); err != nil {
				logger.Printf("[Register] send verification email failed for user %d: %v", user.ID, err)
			}
		}()
		return &RegisterResponse{
			Message:   "注册成功，请查收验证邮件并点击链接完成验证后再登录",
			Email:     req.Email,
			ExpiresIn: formatDuration(s.verifyTTL()),
		}, nil
	}

	return s.signToken(user.ID, tenantID, "owner")
}

// Login 登录
const maxFailedLoginAttempts = 10
const loginLockDuration = 15 * time.Minute

func (s *AuthService) Login(req *LoginRequest) (*AuthResponse, error) {
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	user, err := s.userRepo.GetByEmail(req.Email)
	if err != nil {
		return nil, errors.New("invalid email or password")
	}

	// 账号锁定检查
	if user.LockUntil != nil && time.Now().Before(*user.LockUntil) {
		return nil, fmt.Errorf("account locked due to too many failed attempts, try again after %s",
			user.LockUntil.Format("15:04:05"))
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		// 密码错误：记录失败次数
		user.FailedLoginCount++
		if user.FailedLoginCount >= maxFailedLoginAttempts {
			lockUntil := time.Now().Add(loginLockDuration)
			user.LockUntil = &lockUntil
			user.FailedLoginCount = 0
		}
		_ = s.db.Model(user).Select("failed_login_count", "lock_until").Updates(user).Error
		return nil, errors.New("invalid email or password")
	}

	if user.Status != "active" {
		return nil, errors.New("account is not active")
	}

	// 邮箱验证检查（仅在 requireVerification=true 时生效；OAuth/手机号账号不受此约束）
	if s.requireVerification && user.OAuthProvider == "" && user.Phone == "" {
		if user.EmailVerifiedAt == nil || user.EmailVerifiedAt.IsZero() {
			return nil, errors.New("email not verified, please check your inbox")
		}
	}

	// 登录成功：重置失败计数
	if user.FailedLoginCount > 0 || user.LockUntil != nil {
		_ = s.db.Model(user).Updates(map[string]interface{}{
			"failed_login_count": 0,
			"lock_until":         nil,
		}).Error
	}

	// 获取用户的租户信息（取第一个租户）
	tu, err := s.getDefaultTenantUser(user.ID)
	if err != nil {
		return nil, errors.New("no tenant associated with this account")
	}

	if err := s.userRepo.UpdateLastLogin(user.ID); err != nil {
		logger.Printf("[AuthService] failed to update last login for user %d: %v", user.ID, err)
	}

	return s.signToken(user.ID, tu.TenantID, tu.Role)
}

// RefreshToken 刷新令牌
func (s *AuthService) RefreshToken(tokenStr string) (*AuthResponse, error) {
	claims := &middleware.JWTClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(s.jwtSecret), nil
	})
	if err != nil || !token.Valid {
		return nil, errors.New("invalid or expired token")
	}

	// Check if this JTI is blacklisted (logged out)
	if s.rdb != nil && claims.JTI != "" {
		blacklistKey := "jwt:blacklist:" + claims.JTI
		ctx := context.Background()
		exists, err := s.rdb.Exists(ctx, blacklistKey).Result()
		if err == nil && exists > 0 {
			return nil, fmt.Errorf("token has been revoked")
		}
	}

	oldJTI := claims.JTI
	if oldJTI == "" {
		oldJTI = claims.RegisteredClaims.ID
	}

	// Atomically mark this refresh token as "in-use" using SETNX to prevent concurrent reuse.
	// If two concurrent requests try to refresh the same token, only one wins the SETNX lock;
	// the other gets rejected immediately.
	// TTL matches jwtExpiry (the access token lifetime) to prevent token replay for the full
	// token lifetime; minimum 15 minutes to avoid premature expiry on short-lived tokens.
	if oldJTI != "" && s.rdb != nil {
		usedKey := "jwt:refresh_used:" + oldJTI
		usedTTL := s.jwtExpiry
		if usedTTL < 15*time.Minute {
			usedTTL = 15 * time.Minute
		}
		set, err := s.rdb.SetNX(context.Background(), usedKey, "1", usedTTL).Result()
		if err == nil && !set {
			// Another request already used this refresh token
			return nil, errors.New("refresh token has already been used")
		}
	}

	// Issue new token first, then blacklist the old JTI so it cannot be used again.
	resp, err := s.signToken(claims.UserID, claims.TenantID, claims.Role)
	if err != nil {
		return nil, err
	}

	// Blacklist the old JTI to prevent replay (e.g. if SETNX key expires before token does).
	if oldJTI != "" && s.rdb != nil {
		ttl := s.jwtExpiry
		if claims.ExpiresAt != nil {
			remaining := time.Until(claims.ExpiresAt.Time)
			if remaining > 0 {
				ttl = remaining
			}
		}
		s.rdb.Set(context.Background(), "jwt:blacklist:"+oldJTI, "1", ttl)
	}

	return resp, nil
}

// signToken 生成JWT令牌
func (s *AuthService) signToken(userID, tenantID uint, role string) (*AuthResponse, error) {
	expiresAt := time.Now().Add(s.jwtExpiry)
	jti := uuid.New().String()
	claims := &middleware.JWTClaims{
		UserID:   userID,
		TenantID: tenantID,
		Role:     role,
		JTI:      jti,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        jti,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(s.jwtSecret))
	if err != nil {
		return nil, fmt.Errorf("failed to sign token: %w", err)
	}

	user, err := s.userRepo.GetByID(userID)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token:     signed,
		UserID:    userID,
		TenantID:  tenantID,
		Username:  user.Username,
		Nickname:  user.Nickname,
		Email:     user.Email,
		Role:      role,
		ExpiresAt: expiresAt,
	}, nil
}

// Logout 使 Token 立即失效（将 jti 写入 Redis 黑名单，TTL = token 剩余有效期）
// 若 Redis 未配置或 token 无 jti，则安全静默（不返回错误，客户端清除 token 即可）。
func (s *AuthService) Logout(tokenStr string) error {
	if tokenStr == "" {
		return nil
	}
	claims := &middleware.JWTClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(s.jwtSecret), nil
	})
	// 即使 token 已过期，ParseWithClaims 也会填充 claims；只要能解出 jti 即可加黑名单
	if err != nil && claims.RegisteredClaims.ID == "" && claims.JTI == "" {
		// 无法解析，直接忽略（过期 token 或格式错误）
		return nil
	}

	jti := claims.JTI
	if jti == "" {
		jti = claims.RegisteredClaims.ID
	}
	if jti == "" {
		return nil
	}

	if s.rdb == nil {
		return nil
	}

	ttl := time.Duration(0)
	if claims.ExpiresAt != nil {
		remaining := time.Until(claims.ExpiresAt.Time)
		if remaining > 0 {
			ttl = remaining
		}
	}

	ctx := context.Background()
	if ttl > 0 {
		s.rdb.Set(ctx, "jwt:blacklist:"+jti, "1", ttl)
	} else {
		// token 已过期，无需写入黑名单
	}
	return nil
}

// getDefaultTenantUser 获取用户默认租户关联
func (s *AuthService) getDefaultTenantUser(userID uint) (*model.TenantUser, error) {
	return s.tuRepo.GetFirstByUser(userID)
}

// GetUserByID 获取用户信息
func (s *AuthService) GetUserByID(id uint) (*model.User, error) {
	return s.userRepo.GetByID(id)
}

// UpdateProfile 更新用户资料（昵称、邮箱、头像）
func (s *AuthService) UpdateProfile(userID uint, nickname, email, avatar string) (*model.User, error) {
	updates := map[string]interface{}{}
	if nickname != "" {
		updates["nickname"] = nickname
	}
	if email != "" {
		// Ensure the new email is not already taken by another user.
		if existing, err := s.userRepo.GetByEmail(email); err == nil && existing.ID != userID {
			return nil, errors.New("email already in use by another account")
		}
		updates["email"] = email
	}
	if avatar != "" {
		updates["avatar"] = avatar
	}
	if len(updates) == 0 {
		return s.userRepo.GetByID(userID)
	}
	if err := s.userRepo.UpdateProfile(userID, updates); err != nil {
		return nil, err
	}
	return s.userRepo.GetByID(userID)
}

// DeleteAccount 注销账号：验证密码后匿名化唯一字段再软删除。
// 必须先匿名化 email/username/phone，否则软删除后这些值仍占着唯一索引，
// 导致用同一邮箱重新注册时触发 MySQL Duplicate entry 1062。
func (s *AuthService) DeleteAccount(userID uint, password string) error {
	user, err := s.userRepo.GetByID(userID)
	if err != nil {
		return err
	}
	if user.Password != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
			return errors.New("password is incorrect")
		}
	}
	// 先匿名化唯一字段，释放唯一索引槽位
	ghost := fmt.Sprintf("deleted_%d", userID)
	if err := s.db.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"email":    ghost + "@deleted.local",
		"username": ghost,
		"phone":    "",
	}).Error; err != nil {
		return fmt.Errorf("failed to anonymize user before deletion: %w", err)
	}
	return s.userRepo.Delete(userID)
}

// ChangePassword 修改密码
func (s *AuthService) ChangePassword(userID uint, oldPwd, newPwd string) error {
	user, err := s.userRepo.GetByID(userID)
	if err != nil {
		return err // preserve gorm.ErrRecordNotFound for dev-bypass detection
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(oldPwd)); err != nil {
		return errors.New("current password is incorrect")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPwd), 12)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	if err := s.userRepo.UpdatePassword(userID, string(hashed)); err != nil {
		return err
	}
	// Password has been updated in DB. Now invalidate existing sessions in Redis.
	// If Redis fails, return an error so the caller knows old sessions may still be valid,
	// but the password change itself is not lost.
	if err := s.invalidateUserSessions(userID); err != nil {
		logger.Printf("[AuthService] invalidateUserSessions failed for user %d: %v (old sessions may still be valid)", userID, err)
		return fmt.Errorf("password changed but session invalidation failed, please log out manually: %w", err)
	}
	return nil
}

// RegisterWithPhone 手机号注册
func (s *AuthService) RegisterWithPhone(phone, code, nickname, tenantName string) (*AuthResponse, error) {
	if s.smsService == nil {
		return nil, errors.New("SMS service not configured")
	}
	if err := s.smsService.VerifyCode(phone, code); err != nil {
		return nil, err
	}

	// 检查手机号是否已注册
	if _, err := s.userRepo.GetByPhone(phone); err == nil {
		return nil, errors.New("phone number already registered")
	}

	// 随机密码（用户不会直接使用，可通过手机验证码登录）
	rawPwd := uuid.New().String()
	hashed, err := bcrypt.GenerateFromPassword([]byte(rawPwd), 12)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	if nickname == "" {
		nickname = "用户" + phone[len(phone)-4:]
	}
	username := "phone_" + phone

	if tenantName == "" {
		tenantName = nickname + "的空间"
	}

	var user *model.User
	var tenantID uint
	err = s.db.Transaction(func(tx *gorm.DB) error {
		phoneVerifiedAt := time.Now()
		u := &model.User{
			UUID:            uuid.New().String(),
			Username:        username,
			Email:           phone + "@phone.local",
			Phone:           phone,
			Password:        string(hashed),
			Nickname:        nickname,
			Status:          "active",
			Role:            "user",
			EmailVerifiedAt: &phoneVerifiedAt, // 手机验证码注册，视为已验证
		}
		if err := tx.Create(u).Error; err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}
		user = u

		t := &model.Tenant{
			Name:   tenantName,
			Code:   uuid.New().String()[:8],
			Plan:   "free",
			Status: "active",
		}
		if err := tx.Create(t).Error; err != nil {
			return fmt.Errorf("failed to create tenant: %w", err)
		}
		tenantID = t.ID

		tu := &model.TenantUser{
			TenantID: t.ID,
			UserID:   u.ID,
			Role:     "owner",
			Status:   "active",
		}
		if err := tx.Create(tu).Error; err != nil {
			return fmt.Errorf("failed to create tenant user: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.tenantRepo.IncrUsedUsers(tenantID)

	return s.signToken(user.ID, tenantID, "owner")
}

// LoginWithPhone 手机号验证码登录
func (s *AuthService) LoginWithPhone(phone, code string) (*AuthResponse, error) {
	if s.smsService == nil {
		return nil, errors.New("SMS service not configured")
	}
	if err := s.smsService.VerifyCode(phone, code); err != nil {
		return nil, err
	}

	user, err := s.userRepo.GetByPhone(phone)
	if err != nil {
		return nil, errors.New("phone number not registered")
	}
	if user.Status != "active" {
		return nil, errors.New("account is not active")
	}

	tu, err := s.getDefaultTenantUser(user.ID)
	if err != nil {
		return nil, errors.New("no tenant associated with this account")
	}

	if err := s.userRepo.UpdateLastLogin(user.ID); err != nil {
		logger.Printf("[AuthService] failed to update last login for user %d: %v", user.ID, err)
	}
	return s.signToken(user.ID, tu.TenantID, tu.Role)
}

// LoginWithOAuth OAuth登录/注册（找到绑定用户则登录，否则自动注册）
func (s *AuthService) LoginWithOAuth(info *OAuthUserInfo) (*AuthResponse, error) {
	// 查找已绑定该OAuth的用户
	user, err := s.userRepo.GetByOAuth(info.Provider, info.OpenID)
	if err == nil {
		// 已有绑定用户，直接登录
		if user.Status != "active" {
			return nil, errors.New("account is not active")
		}
		tu, err := s.getDefaultTenantUser(user.ID)
		if err != nil {
			return nil, errors.New("no tenant associated with this account")
		}
		if err := s.userRepo.UpdateLastLogin(user.ID); err != nil {
			logger.Printf("[AuthService] failed to update last login for user %d: %v", user.ID, err)
		}
		return s.signToken(user.ID, tu.TenantID, tu.Role)
	}

	// 未找到，自动注册新用户
	openIDShort := info.OpenID
	if len(openIDShort) > 8 {
		openIDShort = openIDShort[:8]
	}
	username := info.Provider + "_" + openIDShort
	// 避免用户名冲突：追加随机后缀
	username = username + "_" + strings.ToLower(uuid.New().String()[:4])

	rawPwd := uuid.New().String()
	hashed, err := bcrypt.GenerateFromPassword([]byte(rawPwd), 12)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	nickname := info.Nickname
	if nickname == "" {
		nickname = username
	}

	oauthVerifiedAt := time.Now()
	newUser := &model.User{
		UUID:            uuid.New().String(),
		Username:        username,
		Email:           username + "@oauth.local",
		Password:        string(hashed),
		Nickname:        nickname,
		Avatar:          info.Avatar,
		Status:          "active",
		Role:            "user",
		OAuthProvider:   info.Provider,
		OAuthID:         info.OpenID,
		EmailVerifiedAt: &oauthVerifiedAt, // OAuth 身份已由第三方验证
	}
	if info.Phone != "" {
		newUser.Phone = info.Phone
	}
	if err := s.userRepo.Create(newUser); err != nil {
		if isDuplicateKeyError(err) {
			// Concurrent OAuth login created the user between our GetByOAuth check and Create.
			// Re-fetch the existing user and log in instead of returning an error.
			existing, err2 := s.userRepo.GetByOAuth(info.Provider, info.OpenID)
			if err2 != nil {
				return nil, fmt.Errorf("failed to create user: %w", err)
			}
			if existing.Status != "active" {
				return nil, errors.New("account is not active")
			}
			tu, err2 := s.getDefaultTenantUser(existing.ID)
			if err2 != nil {
				return nil, errors.New("no tenant associated with this account")
			}
			_ = s.userRepo.UpdateLastLogin(existing.ID)
			return s.signToken(existing.ID, tu.TenantID, tu.Role)
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	tenant := &model.Tenant{
		Name:   nickname + "的空间",
		Code:   uuid.New().String()[:8],
		Plan:   "free",
		Status: "active",
	}
	if err := s.tenantRepo.Create(tenant); err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}

	tu := &model.TenantUser{
		TenantID: tenant.ID,
		UserID:   newUser.ID,
		Role:     "owner",
		Status:   "active",
	}
	if err := s.tuRepo.Create(tu); err != nil {
		return nil, fmt.Errorf("failed to create tenant user: %w", err)
	}
	_ = s.tenantRepo.IncrUsedUsers(tenant.ID)

	return s.signToken(newUser.ID, tenant.ID, "owner")
}

// ─────────────────────────────────────────────────────────────────────
// 密码重置流程
// ─────────────────────────────────────────────────────────────────────

// RequestPasswordReset 请求密码重置，生成 30 分钟有效的一次性 token。
// 无论邮箱是否存在，均返回 nil 错误（防止用户枚举）。
// 生产环境应在此处发送重置邮件；当前仅返回 token 供调试使用。
func (s *AuthService) RequestPasswordReset(email string) (token string, err error) {
	email = strings.ToLower(strings.TrimSpace(email))

	// per-email 频率限制：每 5 分钟最多发送 1 封重置邮件
	if s.rdb != nil {
		rateLimitKey := "pwd_reset_rate:" + email
		if cnt, _ := s.rdb.Incr(context.Background(), rateLimitKey).Result(); cnt == 1 {
			s.rdb.Expire(context.Background(), rateLimitKey, 5*time.Minute)
		} else if cnt > 1 {
			return "", nil // 静默返回，防枚举同时限速
		}
	}

	user, err := s.userRepo.GetByEmail(email)
	if err != nil {
		// 用户不存在：静默返回（防枚举）
		return "", nil
	}
	if s.tokenRepo == nil {
		return "", fmt.Errorf("token repository not configured")
	}
	// 先删除该用户之前的同类 token
	_ = s.tokenRepo.DeleteByUser(user.ID, "reset_password")

	rawToken := uuid.New().String()
	t := &model.UserToken{
		UserID:    user.ID,
		Token:     rawToken,
		TokenType: "reset_password",
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if err := s.tokenRepo.Create(t); err != nil {
		return "", err
	}
	baseURL := s.appBaseURL
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", baseURL, rawToken)
	appName := s.emailAppName()
	subject := fmt.Sprintf("【%s】密码重置链接", appName)
	body := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:sans-serif;background:#f4f4f4;margin:0;padding:32px">
<div style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:40px;box-shadow:0 2px 8px rgba(0,0,0,.08)">
  <h2 style="color:#1a1a1a;margin:0 0 8px">密码重置</h2>
  <p style="color:#555;line-height:1.6">您申请了 %s 账号的密码重置，请点击下方按钮设置新密码。链接 <strong>30 分钟</strong>内有效。</p>
  <div style="text-align:center;margin:32px 0">
    <a href="%s" style="display:inline-block;background:#6366f1;color:#fff;text-decoration:none;padding:12px 32px;border-radius:8px;font-weight:600;font-size:15px">重置密码</a>
  </div>
  <p style="color:#999;font-size:13px;line-height:1.6">如无法点击按钮，请复制以下链接到浏览器：<br><a href="%s" style="color:#6366f1;word-break:break-all">%s</a></p>
  <hr style="border:none;border-top:1px solid #eee;margin:24px 0">
  <p style="color:#bbb;font-size:12px;margin:0">如非本人操作，请忽略此邮件，您的账号安全不受影响。</p>
</div>
</body></html>`, appName, resetURL, resetURL, resetURL)
	if s.emailSender != nil {
		email, subj, bd := user.Email, subject, body
		go func() {
			if err := s.emailSender.SendEmail(email, subj, bd); err != nil {
				logger.Printf("RequestPasswordReset: send email failed: %v", err)
			}
		}()
	}
	return rawToken, nil
}

// ResetPassword 使用有效 token 设置新密码。
func (s *AuthService) ResetPassword(token, newPassword string) error {
	if s.tokenRepo == nil {
		return fmt.Errorf("token repository not configured")
	}
	t, err := s.tokenRepo.FindValid(token, "reset_password")
	if err != nil {
		return fmt.Errorf("invalid or expired reset token")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
	if err != nil {
		return err
	}
	if err := s.userRepo.UpdatePassword(t.UserID, string(hashed)); err != nil {
		return err
	}
	if err := s.tokenRepo.MarkUsed(t.ID); err != nil {
		return err
	}
	// Fix 9: Synchronous call with non-fatal error logging.
	if err := s.invalidateUserSessions(t.UserID); err != nil {
		logger.Printf("[AuthService] invalidateUserSessions failed for user %d: %v (old sessions may still be valid)", t.UserID, err)
	}
	return nil
}

// invalidateUserSessions 将用户当前时间戳写入 Redis，使所有颁发时间早于此时的 JWT 失效。
// TTL 与配置的 JWT 有效期（jwtExpiry）一致，避免黑名单键永久留存或在 token 仍有效时提前过期。
// 若 Redis 不可用则返回 nil（非致命）。
func (s *AuthService) invalidateUserSessions(userID uint) error {
	if s.rdb == nil {
		return nil
	}
	// Use the configured JWT expiry as the blacklist TTL so the key is kept at least as long
	// as any token that could have been issued. Fall back to 90 days if expiry is not set.
	ttl := s.jwtExpiry
	if ttl <= 0 {
		ttl = 90 * 24 * time.Hour
	}
	key := fmt.Sprintf("jwt:user_invalidate:%d", userID)
	return s.rdb.Set(context.Background(), key, time.Now().Unix(), ttl).Err()
}

// isDuplicateKeyError returns true if the error originates from a database UNIQUE constraint violation.
// Handles both MySQL ("Duplicate entry") and SQLite ("UNIQUE constraint failed") error messages.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "UNIQUE constraint failed")
}

// ─────────────────────────────────────────────────────────────────────
// 邮箱验证流程
// ─────────────────────────────────────────────────────────────────────

// SendEmailVerification 为指定用户生成 24 小时有效的邮箱验证 token。
// 生产环境应在此处发送验证邮件；当前仅返回 token 供调试使用。
func (s *AuthService) SendEmailVerification(userID uint) (token string, err error) {
	if s.tokenRepo == nil {
		return "", fmt.Errorf("token repository not configured")
	}
	// 先删除该用户之前的同类 token
	_ = s.tokenRepo.DeleteByUser(userID, "verify_email")

	rawToken := uuid.New().String()
	ttl := s.verifyTTL()
	t := &model.UserToken{
		UserID:    userID,
		Token:     rawToken,
		TokenType: "verify_email",
		ExpiresAt: time.Now().Add(ttl),
	}
	if err := s.tokenRepo.Create(t); err != nil {
		return "", err
	}
	user, err := s.userRepo.GetByID(userID)
	if err != nil {
		logger.Printf("SendEmailVerification: get user %d failed: %v", userID, err)
	} else {
		baseURL := s.appBaseURL
		if baseURL == "" {
			baseURL = "http://localhost:3000"
		}
		verifyURL := fmt.Sprintf("%s/auth/verify-email?token=%s", baseURL, rawToken)
		appName := s.emailAppName()
		subject := fmt.Sprintf("【%s】邮箱验证", appName)
		body := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:sans-serif;background:#f4f4f4;margin:0;padding:32px">
<div style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:40px;box-shadow:0 2px 8px rgba(0,0,0,.08)">
  <h2 style="color:#1a1a1a;margin:0 0 8px">验证您的邮箱</h2>
  <p style="color:#555;line-height:1.6">感谢注册 %s！请点击下方按钮完成邮箱验证，链接 <strong>%s</strong>内有效。</p>
  <div style="text-align:center;margin:32px 0">
    <a href="%s" style="display:inline-block;background:#6366f1;color:#fff;text-decoration:none;padding:12px 32px;border-radius:8px;font-weight:600;font-size:15px">验证邮箱</a>
  </div>
  <p style="color:#999;font-size:13px;line-height:1.6">如无法点击按钮，请复制以下链接到浏览器：<br><a href="%s" style="color:#6366f1;word-break:break-all">%s</a></p>
  <hr style="border:none;border-top:1px solid #eee;margin:24px 0">
  <p style="color:#bbb;font-size:12px;margin:0">如非本人操作，请忽略此邮件。</p>
</div>
</body></html>`, appName, formatDuration(ttl), verifyURL, verifyURL, verifyURL)
		if s.emailSender != nil {
			email, subj, bd := user.Email, subject, body
			go func() {
				if err := s.emailSender.SendEmail(email, subj, bd); err != nil {
					logger.Printf("SendEmailVerification: send email failed: %v", err)
				}
			}()
		}
	}
	return rawToken, nil
}

// ResendEmailVerification 为未验证的邮箱账号重新发送验证邮件（无需登录）。
// 无论邮箱是否存在、是否已验证，均静默返回 nil，防止用户枚举。
// 每个邮箱 5 分钟内最多触发一次（Redis 限速，Redis 不可用时跳过限速）。
func (s *AuthService) ResendEmailVerification(email string) error {
	email = strings.ToLower(strings.TrimSpace(email))

	if s.rdb != nil {
		rateLimitKey := "email_verify_resend_rate:" + email
		if cnt, _ := s.rdb.Incr(context.Background(), rateLimitKey).Result(); cnt == 1 {
			s.rdb.Expire(context.Background(), rateLimitKey, 5*time.Minute)
		} else if cnt > 1 {
			return nil // 静默限速
		}
	}

	user, err := s.userRepo.GetByEmail(email)
	if err != nil {
		return nil // 邮箱不存在，静默返回
	}
	if user.EmailVerifiedAt != nil && !user.EmailVerifiedAt.IsZero() {
		return nil // 已验证，静默返回
	}
	uid := user.ID
	go func() {
		if _, err := s.SendEmailVerification(uid); err != nil {
			logger.Printf("[ResendEmailVerification] failed for user %d: %v", uid, err)
		}
	}()
	return nil
}

// UnlockUser 管理员手动解除账号锁定
func (s *AuthService) UnlockUser(userID uint) error {
	return s.db.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"failed_login_count": 0,
		"lock_until":         nil,
	}).Error
}

// VerifyEmail 使用有效 token 将用户邮箱标记为已验证。
func (s *AuthService) VerifyEmail(token string) error {
	if s.tokenRepo == nil {
		return fmt.Errorf("token repository not configured")
	}
	t, err := s.tokenRepo.FindValid(token, "verify_email")
	if err != nil {
		return fmt.Errorf("invalid or expired verification token")
	}
	now := time.Now()
	if err := s.db.Model(&model.User{}).Where("id = ?", t.UserID).
		Update("email_verified_at", now).Error; err != nil {
		return err
	}
	return s.tokenRepo.MarkUsed(t.ID)
}
