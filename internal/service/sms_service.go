package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/redis/go-redis/v9"
)

const (
	smsCodeKeyPrefix  = "sms:code:"
	smsLimitKeyPrefix = "sms:limit:"
	smsCountKeyPrefix = "sms:count:"
	smsCodeTTL        = 5 * time.Minute
	smsLimitTTL       = 60 * time.Second
	smsMaxVerifyCount = 3
)

// SMSService 短信服务
type SMSService struct {
	redis *redis.Client
	cfg   config.SMSConfig
}

// NewSMSService 创建短信服务
func NewSMSService(redisClient *redis.Client, cfg config.SMSConfig) *SMSService {
	return &SMSService{redis: redisClient, cfg: cfg}
}

// SendCode 发送验证码
func (s *SMSService) SendCode(phone string) error {
	if s.redis == nil {
		return errors.New("redis not available")
	}
	ctx := context.Background()

	// 频率限制：60秒内不可重复发送
	limitKey := smsLimitKeyPrefix + phone
	exists, err := s.redis.Exists(ctx, limitKey).Result()
	if err != nil {
		return fmt.Errorf("rate limit check failed: %w", err)
	}
	if exists > 0 {
		return errors.New("请勿频繁发送验证码，请60秒后重试")
	}

	// 生成6位随机验证码
	code, err := generateSMSCode(6)
	if err != nil {
		return fmt.Errorf("failed to generate code: %w", err)
	}

	// 调用阿里云短信API
	if err := s.sendAliyunSMS(phone, code); err != nil {
		return fmt.Errorf("failed to send SMS: %w", err)
	}

	// 存入Redis（5分钟TTL）
	codeKey := smsCodeKeyPrefix + phone
	if err := s.redis.Set(ctx, codeKey, code, smsCodeTTL).Err(); err != nil {
		return fmt.Errorf("failed to store code: %w", err)
	}

	// 重置验证次数
	countKey := smsCountKeyPrefix + phone
	s.redis.Set(ctx, countKey, 0, smsCodeTTL)

	// 设置发送频率限制（60秒）
	s.redis.Set(ctx, limitKey, 1, smsLimitTTL)

	return nil
}

// VerifyCode 验证验证码
func (s *SMSService) VerifyCode(phone, code string) error {
	if s.redis == nil {
		return errors.New("redis not available")
	}
	ctx := context.Background()

	countKey := smsCountKeyPrefix + phone
	count, _ := s.redis.Get(ctx, countKey).Int()
	if count >= smsMaxVerifyCount {
		s.redis.Del(ctx, smsCodeKeyPrefix+phone, countKey)
		return errors.New("验证码已失效，请重新获取")
	}

	codeKey := smsCodeKeyPrefix + phone
	stored, err := s.redis.Get(ctx, codeKey).Result()
	if err != nil {
		return errors.New("验证码不存在或已过期")
	}

	// 先递增计数
	s.redis.Incr(ctx, countKey)

	if stored != code {
		remaining := smsMaxVerifyCount - count - 1
		if remaining <= 0 {
			s.redis.Del(ctx, codeKey, countKey)
			return errors.New("验证码错误次数过多，请重新获取")
		}
		return fmt.Errorf("验证码错误，还剩 %d 次机会", remaining)
	}

	// 验证成功，清除
	s.redis.Del(ctx, codeKey, countKey)
	return nil
}

// sendAliyunSMS 调用阿里云短信API（RPC签名方式）
func (s *SMSService) sendAliyunSMS(phone, code string) error {
	if s.cfg.AccessKeyID == "" {
		// 开发模式：未配置AK则跳过真实发送
		return nil
	}

	params := map[string]string{
		"AccessKeyId":      s.cfg.AccessKeyID,
		"Action":           "SendSms",
		"Format":           "JSON",
		"PhoneNumbers":     phone,
		"RegionId":         "cn-hangzhou",
		"SignName":         s.cfg.SignName,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   smsGenerateNonce(),
		"SignatureVersion": "1.0",
		"TemplateCode":     s.cfg.TemplateCode,
		"TemplateParam":    fmt.Sprintf(`{"code":"%s"}`, code),
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          "2017-05-25",
	}

	signature := aliyunRPCSign("POST", params, s.cfg.AccessKeySecret)
	params["Signature"] = signature

	body := url.Values{}
	for k, v := range params {
		body.Set(k, v)
	}

	resp, err := http.PostForm("https://dysmsapi.aliyuncs.com", body)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if result.Code != "OK" {
		return fmt.Errorf("SMS API error: %s", result.Message)
	}
	return nil
}

// aliyunRPCSign 阿里云RPC API HMAC-SHA1签名
func aliyunRPCSign(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(params))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(params[k]))
	}
	queryString := strings.Join(parts, "&")

	strToSign := method + "&" + url.QueryEscape("/") + "&" + url.QueryEscape(queryString)

	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(strToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// generateSMSCode 生成n位随机数字验证码
func generateSMSCode(n int) (string, error) {
	max := new(big.Int)
	max.Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	num, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0"+strconv.Itoa(n)+"d", num.Int64()), nil
}

// smsGenerateNonce 生成随机字符串
func smsGenerateNonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
