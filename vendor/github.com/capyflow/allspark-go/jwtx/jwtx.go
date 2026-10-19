package jwtx

import (
	"github.com/golang-jwt/jwt/v5"
)

// 生成jwt签名
func SignJwt(secret string, claims jwt.MapClaims) (string, error) {
	token := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		claims,
	)
	tokenString, err := token.SignedString([]byte(secret))
	if nil != err {
		return "", err
	}
	return tokenString, nil
}

// 解析token
func ParseToken(secret string, token string) (jwt.MapClaims, error) {
	tokenClaims, err := jwt.Parse(token, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if nil != err {
		return nil, err
	}
	return tokenClaims.Claims.(jwt.MapClaims), nil
}
