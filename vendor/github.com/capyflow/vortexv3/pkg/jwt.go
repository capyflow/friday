package vpkg

import (
	"strings"

	"github.com/gin-gonic/gin"
)

type JwtOption struct {
	SecretKey string `json:"secret_key" toml:"secretKey"` // JWT密钥
	Expire    int64  `json:"expire" toml:"expire"`        // 过期时间

	// "header:Authorization:Bearer"
	// "query:token"
	// "cookie:token"
	TokenLookUp       []string `json:"token_look_up" toml:"tokenLookUp"`             // 从请求头中查找token的key
	SkipExpiredVerfiy bool     `json:"skip_expired_verify" toml:"skipExpiredVerify"` // 跳过过期验证
}

// LookUpToken 从请求中查找token
// 支持从 query 参数读取（用于浏览器 WebSocket）：?token=xxx 或 ?access_token=xxx
func LookUpToken(ctx *gin.Context, opt *JwtOption) string {
	lookUps := opt.TokenLookUp
	if len(lookUps) == 0 {
		lookUps = []string{"header:Authorization:Bearer", "query:token", "query:access_token"}
	}
	for _, key := range lookUps {
		split := strings.Split(key, ":")
		if len(split) < 2 {
			continue
		}
		switch split[0] {
		case "header":
			token := ctx.GetHeader(split[1])
			if len(split) == 3 {
				token = strings.TrimPrefix(token, strings.TrimSpace(split[2])+" ")
			}
			if len(token) > 0 {
				return token
			}
		case "query":
			token := ctx.Query(split[1])
			if len(token) > 0 {
				return token
			}
		case "cookie":
			token, err := ctx.Cookie(split[1])
			if err == nil && len(token) > 0 {
				return token
			}
		}
	}
	return ""
}
