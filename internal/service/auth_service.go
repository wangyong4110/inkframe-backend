package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/inkframe/inkframe-backend/internal/middleware"
	"github.com/inkframe/inkframe-backend/internal/model"
	"github.com/inkframe/inkframe-backend/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

// AuthService 认证服务
type AuthService struct {
	userRepo   *repository.UserRepository
	tenantRepo *repository.TenantRepository
	tuRepo     *repository.TenantUserRepository
	jwtSecret  string
	jwtExpiry  time.Duration
}

func NewAuthService(
	userRepo *repository.UserRepository,
	tenantRepo *repository.TenantRepository,
	tuRepo *repository.TenantUserRepository,
	jwtSecret string,
	jwtExpiry time.Duration,
) *AuthService {
	return &AuthService{
		userRepo:   userRepo,
		tenantRepo: tenantRepo,
		tuRepo:     tuRepo,
		jwtSecret:  jwtSecret,
		jwtExpiry:  jwtExpiry,
	}
}

// RegisterRequest 注册请求
type RegisterRequest struct {
	TenantName string `json:"tenant_name" binding:"required"`
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
	// 检查邮箱是否已存在
	if _, err := s.userRepo.GetByEmail(req.Email); err == nil {
		return nil, errors.New("email already registered")
	}

	// 哈希密码
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	// 创建用户
	user := &model.User{
		UUID:     uuid.New().String(),
		Username: req.Username,
		Email:    req.Email,
		Password: string(hashed),
		Nickname: req.Nickname,
		Status:   "active",
		Role:     "user",
	}
	if err := s.userRepo.Create(user); err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	// 创建租户
	tenant := &model.Tenant{
		Name:   req.TenantName,
		Code:   uuid.New().String()[:8],
		Plan:   "free",
		Status: "active",
	}
	if err := s.tenantRepo.Create(tenant); err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}

	// 创建租户-用户关联（owner角色）
	tu := &model.TenantUser{
		TenantID: tenant.ID,
		UserID:   user.ID,
		Role:     "owner",
		Status:   "active",
	}
	if err := s.tuRepo.Create(tu); err != nil {
		return nil, fmt.Errorf("failed to create tenant user: %w", err)
	}

	// 更新租户已用用户数
	_ = s.tenantRepo.IncrUsedUsers(tenant.ID)

	return s.signToken(user.ID, tenant.ID, "owner")
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
