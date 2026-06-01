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
	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/redis/go-redis/v9"
)

const (
	smsCodeKeyPrefix      = "sms:code:"
	smsLimitKeyPrefix     = "sms:limit:"
	smsCountKeyPrefix     = "sms:count:"
	smsDailyKeyPrefix     = "sms:daily:"
	smsVerifyDailyPrefix  = "sms:verify_daily:"
	smsCodeTTL            = 5 * time.Minute
	smsLimitTTL           = 60 * time.Second
	smsMaxVerifyCount     = 3
	smsDailyMax           = 5
	smsVerifyDailyMax     = 10
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

	// 每日发送次数限制
	dailyKey := smsDailyKeyPrefix + phone
	dailyCount, err := s.redis.Incr(ctx, dailyKey).Result()
	if err != nil {
		return fmt.Errorf("daily limit check failed: %w", err)
	}
	if dailyCount == 1 {
		// First send today — set 24h expiry
		s.redis.Expire(ctx, dailyKey, 24*time.Hour)
	}
	if dailyCount > smsDailyMax {
		return fmt.Errorf("今日发送验证码次数已达上限（%d次），请明日再试", smsDailyMax)
	}

	// 生成6位随机验证码
	code, err := generateSMSCode(6)
	if err != nil {
		return fmt.Errorf("failed to generate code: %w", err)
	}

	// 调用阿里云短信API（开发模式下跳过真实发送，但仍将验证码存入 Redis，
	// 验证逻辑与生产环境完全一致，不允许使用任意验证码）。
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
// Uses a Lua script to atomically check the code and manage attempt counting,
// preventing race conditions where concurrent requests could both pass verification
// using the same code before the delete completes.
func (s *SMSService) VerifyCode(phone, code string) error {
	if s.redis == nil {
		return errors.New("redis not available")
	}
	ctx := context.Background()

	countKey := smsCountKeyPrefix + phone
	codeKey := smsCodeKeyPrefix + phone

	// 每日验证次数限制（防暴力枚举）
	verifyDailyKey := smsVerifyDailyPrefix + phone
	verifyCount, verifyErr := s.redis.Incr(ctx, verifyDailyKey).Result()
	if verifyErr != nil {
		return errors.New("验证服务暂时不可用")
	}
	if verifyCount == 1 {
		s.redis.Expire(ctx, verifyDailyKey, 24*time.Hour)
	}
	if verifyCount > smsVerifyDailyMax {
		return errors.New("今日验证次数已达上限")
	}

	// Lua script: atomically verify code and manage attempt counter.
	// Returns:
	//   0  — success (code matched, both keys deleted)
	//  -1  — code does not exist or has expired
	//  -2  — too many wrong attempts (keys deleted)
	//   n  — wrong code, n attempts used so far (code key preserved for remaining attempts)
	luaScript := redis.NewScript(`
		local stored = redis.call('GET', KEYS[1])
		if stored == false then return -1 end
		local count_key = KEYS[2]
		local max = tonumber(ARGV[2])
		if stored ~= ARGV[1] then
			local n = redis.call('INCR', count_key)
			redis.call('EXPIRE', count_key, ARGV[3])
			if n >= max then
				redis.call('DEL', KEYS[1], count_key)
				return -2
			end
			return n
		end
		redis.call('DEL', KEYS[1], count_key)
		return 0
	`)

	result, err := luaScript.Run(ctx, s.redis,
		[]string{codeKey, countKey},
		code, smsMaxVerifyCount, int(smsCodeTTL.Seconds()),
	).Int()
	if err != nil {
		return errors.New("验证服务暂时不可用")
	}

	switch result {
	case 0:
		return nil // success
	case -1:
		return errors.New("验证码不存在或已过期")
	case -2:
		return errors.New("验证码错误次数过多，请重新获取")
	default:
		remaining := int64(smsMaxVerifyCount) - int64(result)
		if remaining <= 0 {
			return errors.New("验证码错误次数过多，请重新获取")
		}
		return fmt.Errorf("验证码错误，还剩 %d 次机会", remaining)
	}
}

// sendAliyunSMS 调用阿里云短信API（RPC签名方式，dysmsapi.aliyuncs.com）
// 开发模式（AccessKeyID 为空）时，跳过真实发送并打印验证码日志，但调用方仍会将
// 验证码存入 Redis；验证逻辑与生产环境完全相同，不存在"万能验证码"漏洞。
func (s *SMSService) sendAliyunSMS(phone, code string) error {
	if s.cfg.AccessKeyID == "" {
		// DEV MODE: SMS not sent; code will be stored in Redis by the caller for normal verification.
		logger.Printf("[SMS DEV MODE] verification code for %s: %s (not sent, dev mode only — AccessKeyID not configured)", phone, code)
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

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm("https://dysmsapi.aliyuncs.com", body)
	if err != nil {
		return fmt.Errorf("SMS HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code      string `json:"Code"`
		Message   string `json:"Message"`
		RequestID string `json:"RequestId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode SMS response: %w", err)
	}
	if result.Code != "OK" {
		return fmt.Errorf("阿里云短信发送失败 [%s]: %s", result.Code, result.Message)
	}
	logger.Printf("[SMS] sent to %s requestId=%s", phone, result.RequestID)
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
