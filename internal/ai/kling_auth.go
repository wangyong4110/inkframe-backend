package ai

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

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
