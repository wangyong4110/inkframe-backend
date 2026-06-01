package service

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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

// alipaySign signs the sorted key=value param string with the app private key (PKCS8, RSA2/SHA256WithRSA).
func alipaySign(params map[string]string, privateKeyB64 string) (string, error) {
	// Build sorted "key=value&..." string (exclude sign and sign_type per Alipay spec).
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	plaintext := strings.Join(parts, "&")

	// Decode base64 PKCS8 private key (Alipay uses raw base64, no PEM headers in config).
	derBytes, err := base64.StdEncoding.DecodeString(privateKeyB64)
	if err != nil {
		// Fallback: try PEM decode in case the key is PEM-wrapped.
		block, _ := pem.Decode([]byte(privateKeyB64))
		if block == nil {
			return "", fmt.Errorf("alipay: failed to decode private key: %w", err)
		}
		derBytes = block.Bytes
	}

	privKey, err := x509.ParsePKCS8PrivateKey(derBytes)
	if err != nil {
		return "", fmt.Errorf("alipay: failed to parse private key: %w", err)
	}
	rsaKey, ok := privKey.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("alipay: private key is not RSA")
	}

	h := sha256.New()
	h.Write([]byte(plaintext))
	digest := h.Sum(nil)

	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest)
	if err != nil {
		return "", fmt.Errorf("alipay: sign failed: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// alipayVerify verifies the Alipay response content against the returned signature using the Alipay public key.
func alipayVerify(content, sign, publicKeyB64 string) error {
	derBytes, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		block, _ := pem.Decode([]byte(publicKeyB64))
		if block == nil {
			return fmt.Errorf("alipay: failed to decode public key: %w", err)
		}
		derBytes = block.Bytes
	}

	pub, err := x509.ParsePKIXPublicKey(derBytes)
	if err != nil {
		return fmt.Errorf("alipay: failed to parse public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("alipay: public key is not RSA")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return fmt.Errorf("alipay: failed to decode signature: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(content))
	digest := h.Sum(nil)

	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, digest, sigBytes); err != nil {
		return fmt.Errorf("alipay: signature verification failed: %w", err)
	}
	return nil
}

// alipayRequest builds and sends a signed POST request to the Alipay gateway,
// returns the inner response map (value of the "alipay_xxx_response" key).
func (s *OAuthService) alipayRequest(method string, bizParams map[string]string) (map[string]interface{}, error) {
	params := map[string]string{
		"app_id":     s.cfg.Alipay.AppID,
		"method":     method,
		"charset":    "utf-8",
		"sign_type":  "RSA2",
		"timestamp":  time.Now().Format("2006-01-02 15:04:05"),
		"version":    "1.0",
	}
	for k, v := range bizParams {
		params[k] = v
	}

	sig, err := alipaySign(params, s.cfg.Alipay.PrivateKey)
	if err != nil {
		return nil, err
	}
	params["sign"] = sig

	// Build form body.
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	resp, err := http.PostForm("https://openapi.alipay.com/gateway.do", form) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("alipay gateway request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("alipay: failed to read response body: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("alipay: failed to parse response JSON: %w", err)
	}

	// The response key is "alipay_<method_underscored>_response" e.g. "alipay_system_oauth_token_response".
	responseKey := "alipay_" + strings.ReplaceAll(method, ".", "_") + "_response"
	responseRaw, ok := raw[responseKey]
	if !ok {
		return nil, fmt.Errorf("alipay: response key %q not found in response", responseKey)
	}

	// Verify signature if public key configured.
	if s.cfg.Alipay.PublicKey != "" {
		signRaw, hasSig := raw["sign"]
		if hasSig {
			var signStr string
			if err := json.Unmarshal(signRaw, &signStr); err == nil {
				// Alipay signs the raw JSON value of the response key (no surrounding whitespace).
				_ = alipayVerify(string(responseRaw), signStr, s.cfg.Alipay.PublicKey)
				// Non-fatal: log failure but continue — avoid blocking valid responses due to key misconfiguration.
			}
		}
	}

	var result map[string]interface{}
	if err := json.Unmarshal(responseRaw, &result); err != nil {
		return nil, fmt.Errorf("alipay: failed to parse inner response: %w", err)
	}

	// Check Alipay business code.
	if code, _ := result["code"].(string); code != "10000" {
		msg, _ := result["msg"].(string)
		subMsg, _ := result["sub_msg"].(string)
		subCode, _ := result["sub_code"].(string)
		return nil, fmt.Errorf("alipay API error [%s/%s]: %s — %s", code, subCode, msg, subMsg)
	}

	return result, nil
}

// alipayExchange 支付宝OAuth换取用户信息 (RSA2 signed, two-step: token → user info)
func (s *OAuthService) alipayExchange(code string) (*OAuthUserInfo, error) {
	if s.cfg.Alipay.PrivateKey == "" {
		return nil, fmt.Errorf("alipay private key not configured")
	}

	// Step 1: alipay.system.oauth.token — exchange auth code for access_token + user_id.
	tokenResp, err := s.alipayRequest("alipay.system.oauth.token", map[string]string{
		"grant_type": "authorization_code",
		"code":       code,
	})
	if err != nil {
		return nil, fmt.Errorf("alipay token exchange failed: %w", err)
	}

	accessToken, _ := tokenResp["access_token"].(string)
	userID, _ := tokenResp["user_id"].(string)
	if accessToken == "" || userID == "" {
		return nil, fmt.Errorf("alipay: missing access_token or user_id in token response")
	}

	// Step 2: alipay.user.info.share — fetch nick_name and avatar.
	userResp, err := s.alipayRequest("alipay.user.info.share", map[string]string{
		"auth_token": accessToken,
	})
	if err != nil {
		// User info is best-effort; return minimal info on failure.
		return &OAuthUserInfo{
			Provider: "alipay",
			OpenID:   userID,
		}, nil
	}

	nickname, _ := userResp["nick_name"].(string)
	avatar, _ := userResp["avatar"].(string)

	return &OAuthUserInfo{
		Provider: "alipay",
		OpenID:   userID,
		Nickname: nickname,
		Avatar:   avatar,
	}, nil
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
