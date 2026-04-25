package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// AuthService 认证服务
type AuthService struct {
	db         *gorm.DB
	userRepo   *repository.UserRepository
	tenantRepo *repository.TenantRepository
	tuRepo     *repository.TenantUserRepository
	jwtSecret  string
	jwtExpiry  time.Duration
	smsService *SMSService
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

// RegisterRequest 注册请求
type RegisterRequest struct {
	TenantName string `json:"tenant_name"` // 可选，为空时自动使用用户名
	Username   string `json:"username" binding:"required"`
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required,min=8"`
	Nickname   string `json:"nickname"`
}

// LoginRequest 登录请求
type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse 认证响应
type AuthResponse struct {
	Token    string      `json:"token"`
	UserID   uint        `json:"user_id"`
	TenantID uint        `json:"tenant_id"`
	Username string      `json:"username"`
	Role     string      `json:"role"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// Register 注册新用户并创建租户
func (s *AuthService) Register(req *RegisterRequest) (*AuthResponse, error) {
	if req.TenantName == "" {
		req.TenantName = req.Username
	}
	// 检查邮箱是否已存在
	if _, err := s.userRepo.GetByEmail(req.Email); err == nil {
		return nil, errors.New("email already registered")
	}

	// 哈希密码
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	var user *model.User
	var tenantID uint

	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 创建用户
		user = &model.User{
			UUID:     uuid.New().String(),
			Username: req.Username,
			Email:    req.Email,
			Password: string(hashed),
			Nickname: req.Nickname,
			Status:   "active",
			Role:     "user",
		}
		if err := tx.Create(user).Error; err != nil {
			return fmt.Errorf("failed to create user: %w", err)
		}

		// 创建租户
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

		// 创建租户-用户关联（owner角色）
		tu := &model.TenantUser{
			TenantID: tenant.ID,
			UserID:   user.ID,
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

// Login 登录
func (s *AuthService) Login(req *LoginRequest) (*AuthResponse, error) {
	user, err := s.userRepo.GetByEmail(req.Email)
	if err != nil {
		return nil, errors.New("invalid email or password")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		return nil, errors.New("invalid email or password")
	}

	if user.Status != "active" {
		return nil, errors.New("account is not active")
	}

	// 获取用户的租户信息（取第一个租户）
	// In a real system, allow selecting tenant if user belongs to multiple
	tu, err := s.getDefaultTenantUser(user.ID)
	if err != nil {
		return nil, errors.New("no tenant associated with this account")
	}

	_ = s.userRepo.UpdateLastLogin(user.ID)

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

	return s.signToken(claims.UserID, claims.TenantID, claims.Role)
}

// signToken 生成JWT令牌
func (s *AuthService) signToken(userID, tenantID uint, role string) (*AuthResponse, error) {
	expiresAt := time.Now().Add(s.jwtExpiry)
	claims := &middleware.JWTClaims{
		UserID:   userID,
		TenantID: tenantID,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
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
		Role:      role,
		ExpiresAt: expiresAt,
	}, nil
}

// getDefaultTenantUser 获取用户默认租户关联
func (s *AuthService) getDefaultTenantUser(userID uint) (*model.TenantUser, error) {
	return s.tuRepo.GetFirstByUser(userID)
}

// GetUserByID 获取用户信息
func (s *AuthService) GetUserByID(id uint) (*model.User, error) {
	return s.userRepo.GetByID(id)
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
	hashed, err := bcrypt.GenerateFromPassword([]byte(rawPwd), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	if nickname == "" {
		nickname = "用户" + phone[len(phone)-4:]
	}
	username := "phone_" + phone

	user := &model.User{
		UUID:     uuid.New().String(),
		Username: username,
		Email:    phone + "@phone.local",
		Phone:    phone,
		Password: string(hashed),
		Nickname: nickname,
		Status:   "active",
		Role:     "user",
	}
	if err := s.userRepo.Create(user); err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	if tenantName == "" {
		tenantName = nickname + "的空间"
	}
	tenant := &model.Tenant{
		Name:   tenantName,
		Code:   uuid.New().String()[:8],
		Plan:   "free",
		Status: "active",
	}
	if err := s.tenantRepo.Create(tenant); err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}

	tu := &model.TenantUser{
		TenantID: tenant.ID,
		UserID:   user.ID,
		Role:     "owner",
		Status:   "active",
	}
	if err := s.tuRepo.Create(tu); err != nil {
		return nil, fmt.Errorf("failed to create tenant user: %w", err)
	}
	_ = s.tenantRepo.IncrUsedUsers(tenant.ID)

	return s.signToken(user.ID, tenant.ID, "owner")
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

	_ = s.userRepo.UpdateLastLogin(user.ID)
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
		_ = s.userRepo.UpdateLastLogin(user.ID)
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
	hashed, err := bcrypt.GenerateFromPassword([]byte(rawPwd), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	nickname := info.Nickname
	if nickname == "" {
		nickname = username
	}

	newUser := &model.User{
		UUID:          uuid.New().String(),
		Username:      username,
		Email:         username + "@oauth.local",
		Password:      string(hashed),
		Nickname:      nickname,
		Avatar:        info.Avatar,
		Status:        "active",
		Role:          "user",
		OAuthProvider: info.Provider,
		OAuthID:       info.OpenID,
	}
	if info.Phone != "" {
		newUser.Phone = info.Phone
	}
	if err := s.userRepo.Create(newUser); err != nil {
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
