package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/inkframe/inkframe-backend/internal/config"
)

// OAuthUserInfo OAuth统一用户信息
type OAuthUserInfo struct {
	Provider string
	OpenID   string
	Nickname string
	Avatar   string
	Phone    string
}

// OAuthService OAuth服务
type OAuthService struct {
	cfg config.OAuthConfig
}

// NewOAuthService 创建OAuth服务
func NewOAuthService(cfg config.OAuthConfig) *OAuthService {
	return &OAuthService{cfg: cfg}
}

// GetAuthURL 生成各平台授权URL（前端跳转用）
func (s *OAuthService) GetAuthURL(provider, state string) (string, error) {
	switch provider {
	case "wechat":
		return fmt.Sprintf(
			"https://open.weixin.qq.com/connect/oauth2/authorize?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_userinfo&state=%s#wechat_redirect",
			url.QueryEscape(s.cfg.Wechat.AppID),
			url.QueryEscape(s.cfg.Wechat.RedirectURI),
			url.QueryEscape(state),
		), nil
	case "alipay":
		return fmt.Sprintf(
			"https://openauth.alipay.com/oauth2/publicAppAuthorize.htm?app_id=%s&scope=auth_user&redirect_uri=%s&state=%s",
			url.QueryEscape(s.cfg.Alipay.AppID),
			url.QueryEscape(s.cfg.Alipay.RedirectURI),
			url.QueryEscape(state),
		), nil
	case "douyin":
		return fmt.Sprintf(
			"https://open.douyin.com/platform/oauth/connect/?client_key=%s&response_type=code&scope=user_info&redirect_uri=%s&state=%s",
			url.QueryEscape(s.cfg.Douyin.AppID),
			url.QueryEscape(s.cfg.Douyin.RedirectURI),
			url.QueryEscape(state),
		), nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider: %s", provider)
	}
}

// ExchangeUserInfo 用code换取用户信息
func (s *OAuthService) ExchangeUserInfo(provider, code string) (*OAuthUserInfo, error) {
	switch provider {
	case "wechat":
		return s.wechatExchange(code)
	case "alipay":
		return s.alipayExchange(code)
	case "douyin":
		return s.douyinExchange(code)
	default:
		return nil, fmt.Errorf("unsupported OAuth provider: %s", provider)
	}
}

// wechatExchange 微信公众号OAuth换取用户信息
func (s *OAuthService) wechatExchange(code string) (*OAuthUserInfo, error) {
	// Step 1: 获取access_token
	tokenURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/oauth2/access_token?appid=%s&secret=%s&code=%s&grant_type=authorization_code",
		s.cfg.Wechat.AppID, s.cfg.Wechat.AppSecret, code,
	)
	resp, err := http.Get(tokenURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("wechat token request failed: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		OpenID      string `json:"openid"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode wechat token response: %w", err)
	}
	if tokenResp.ErrCode != 0 {
		return nil, fmt.Errorf("wechat token error %d: %s", tokenResp.ErrCode, tokenResp.ErrMsg)
	}

	// Step 2: 获取用户信息
	userURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/userinfo?access_token=%s&openid=%s&lang=zh_CN",
		tokenResp.AccessToken, tokenResp.OpenID,
	)
	userResp, err := http.Get(userURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("wechat userinfo request failed: %w", err)
	}
	defer userResp.Body.Close()

	var userInfo struct {
		OpenID     string `json:"openid"`
		Nickname   string `json:"nickname"`
		HeadImgURL string `json:"headimgurl"`
		ErrCode    int    `json:"errcode"`
		ErrMsg     string `json:"errmsg"`
	}
	if err := json.NewDecoder(userResp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode wechat userinfo: %w", err)
	}
	if userInfo.ErrCode != 0 {
		return nil, fmt.Errorf("wechat userinfo error %d: %s", userInfo.ErrCode, userInfo.ErrMsg)
	}

	return &OAuthUserInfo{
		Provider: "wechat",
		OpenID:   userInfo.OpenID,
		Nickname: userInfo.Nickname,
		Avatar:   userInfo.HeadImgURL,
	}, nil
}

// alipayExchange 支付宝OAuth换取用户信息
// 完整实现需使用RSA2对参数签名，尚未实现，暂不开放。
func (s *OAuthService) alipayExchange(_ string) (*OAuthUserInfo, error) {
	return nil, fmt.Errorf("alipay oauth not yet implemented")
}

// douyinExchange 抖音OAuth换取用户信息
func (s *OAuthService) douyinExchange(code string) (*OAuthUserInfo, error) {
	// Step 1: 换取access_token
	resp, err := http.PostForm("https://open.douyin.com/oauth/access_token/", url.Values{
		"client_key":    {s.cfg.Douyin.AppID},
		"client_secret": {s.cfg.Douyin.AppSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return nil, fmt.Errorf("douyin token request failed: %w", err)
	}
	defer resp.Body.Close()

	var tokenResult struct {
		Data struct {
			AccessToken string `json:"access_token"`
			OpenID      string `json:"open_id"`
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResult); err != nil {
		return nil, fmt.Errorf("failed to decode douyin token response: %w", err)
	}
	if tokenResult.Data.ErrorCode != 0 {
		return nil, fmt.Errorf("douyin token error %d: %s", tokenResult.Data.ErrorCode, tokenResult.Data.Description)
	}

	// Step 2: 获取用户信息
	req, _ := http.NewRequest("GET", "https://open.douyin.com/oauth/userinfo/", nil)
	req.Header.Set("access-token", tokenResult.Data.AccessToken)
	q := req.URL.Query()
	q.Set("client_key", s.cfg.Douyin.AppID)
	q.Set("open_id", tokenResult.Data.OpenID)
	req.URL.RawQuery = q.Encode()

	client := &http.Client{}
	userResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("douyin userinfo request failed: %w", err)
	}
	defer userResp.Body.Close()

	var userResult struct {
		Data struct {
			OpenID      string `json:"open_id"`
			Nickname    string `json:"nickname"`
			Avatar      string `json:"avatar"`
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.NewDecoder(userResp.Body).Decode(&userResult); err != nil {
		return nil, fmt.Errorf("failed to decode douyin userinfo: %w", err)
	}
	if userResult.Data.ErrorCode != 0 {
		return nil, fmt.Errorf("douyin userinfo error %d: %s", userResult.Data.ErrorCode, userResult.Data.Description)
	}

	return &OAuthUserInfo{
		Provider: "douyin",
		OpenID:   userResult.Data.OpenID,
		Nickname: userResult.Data.Nickname,
		Avatar:   userResult.Data.Avatar,
	}, nil
}
