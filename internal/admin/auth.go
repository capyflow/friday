// auth.go 是网页入口的登录凭据：账号密码换 HMAC-SHA256 JWT（HS256），之后
// 每个请求带 Authorization: Bearer <token>。token 自包含用户名、过期时间与
// 密码版本（账号密码变更后旧 token 立即失效），无需服务端会话表；签名密钥
// 落盘在数据目录，重启后仍有效。退出登录把 token 记入内存吊销表（重启丢失，
// 但账号密码校验与有效期仍在）。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

const (
	tokenTTL     = 7 * 24 * time.Hour
	authKeyBytes = 32
)

// sessionClaims 是 JWT 载荷：标准 subject/iat/exp + 密码版本。
type sessionClaims struct {
	jwt.RegisteredClaims
	PV string `json:"pv"`
}

// loadOrCreateAuthKey 读取或生成签名密钥；路径为空时用进程内随机密钥（测试）。
func loadOrCreateAuthKey(path string) []byte {
	if strings.TrimSpace(path) == "" {
		return randomAuthKey()
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= authKeyBytes {
		return b
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[管理后台] 读取登录签名密钥失败（将重新生成，旧 token 失效）: %v", err)
	}
	key := randomAuthKey()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("[管理后台] 创建登录签名密钥目录失败（重启后需重新登录）: %v", err)
		return key
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		log.Printf("[管理后台] 保存登录签名密钥失败（重启后需重新登录）: %v", err)
	}
	return key
}

func randomAuthKey() []byte {
	b := make([]byte, authKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic("admin: 生成登录签名密钥失败: " + err.Error())
	}
	return b
}

// passwordVersion 把账号密码哈希折成短串：换密码即换版本，旧 token 全部失效。
func passwordVersion(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:8])
}

// issueToken 为当前账号签发一个 JWT。
func (s *Server) issueToken(username string) (string, error) {
	if s.accounts == nil {
		return "", errors.New("账号存储不可用")
	}
	pv := s.accounts.CredentialVersion()
	now := time.Now()
	claims := sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
		PV: pv,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.authKey)
}

// parseToken 校验签名与有效期，返回载荷（不含吊销）。
func (s *Server) parseToken(raw string) (*sessionClaims, bool) {
	if raw == "" {
		return nil, false
	}
	claims := &sessionClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return s.authKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid || claims.Subject == "" {
		return nil, false
	}
	return claims, true
}

// authenticated 判断 token 是否属于当前账号且密码版本一致、未被吊销。
func (s *Server) authenticated(raw string) bool {
	claims, ok := s.parseToken(raw)
	if !ok || s.accounts == nil {
		return false
	}
	acc := s.accounts.Current()
	if acc.SetupRequired || claims.Subject != acc.Username {
		return false
	}
	if claims.PV != s.accounts.CredentialVersion() {
		return false
	}
	return !s.isRevoked(raw)
}

func (s *Server) isRevoked(raw string) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	exp, ok := s.revoked[raw]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.revoked, raw)
		return false
	}
	return true
}

// revokeToken 把已签发的 token 记入内存吊销表（退出登录）。
func (s *Server) revokeToken(raw string) {
	if raw == "" {
		return
	}
	claims := &sessionClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return s.authKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil {
		return
	}
	exp := time.Now().Add(tokenTTL)
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time
	}
	s.authMu.Lock()
	for tok, at := range s.revoked { // 顺带清掉已过期的吊销记录
		if time.Now().After(at) {
			delete(s.revoked, tok)
		}
	}
	s.revoked[raw] = exp
	s.authMu.Unlock()
}

// bearerToken 从 Authorization 头取 Bearer token。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// forbidden 是统一的登录失效响应：403 + auth 标记，前端据此清 token 回登录页。
func forbidden(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Code{Code: http.StatusForbidden, MsgKey: "unauthorized"},
		map[string]any{"error": "登录已失效，请重新登录", "auth": true})
}
