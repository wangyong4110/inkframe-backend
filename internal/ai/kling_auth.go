package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// normalizeKlingEndpoint 规范化可灵 API 端点，去除尾部 "/v1" 和 "/"，
// 使得所有 Kling provider 内部路径（如 "/v1/audio/text-to-audio"）能正确拼接。
func normalizeKlingEndpoint(endpoint, defaultEndpoint string) string {
	if endpoint == "" {
		return defaultEndpoint
	}
	ep := strings.TrimRight(endpoint, "/")
	ep = strings.TrimSuffix(ep, "/v1")
	ep = strings.TrimRight(ep, "/")
	return ep
}

// klingDoRequest 是所有 Kling 系列 provider 共用的 HTTP 请求实现。
// 使用 accessKey+secretKey 生成 HS256 JWT，发送 JSON 请求，返回响应体和状态码。
func klingDoRequest(ctx context.Context, accessKey, secretKey, endpoint string, client *http.Client, method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(b)
	}
	url := endpoint + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}
	token, err := klingJWT(accessKey, secretKey)
	if err != nil {
		return nil, 0, fmt.Errorf("kling: JWT generation failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// klingJWT 生成可灵 API 所需的 JWT Bearer 令牌。
//
// 鉴权方式：HS256 JWT，payload 为：
//
//	{"iss": "<accessKey>", "exp": now+1800, "nbf": now-5}
//
// signed with secretKey.
// 令牌有效期 30 分钟，每次请求前实时生成即可（无需缓存）。
func klingJWT(accessKey, secretKey string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": accessKey,
		"exp": now.Add(30 * time.Minute).Unix(),
		"nbf": now.Add(-5 * time.Second).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secretKey))
}
