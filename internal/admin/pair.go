// 手机配对：桌面端生成一次性配对码的二维码，手机扫码自动换取登录 JWT。
//
// 信任链：配对码由已登录用户（桌面端）索取、内存单条、5 分钟过期、兑换即作废；
// redeem 是公开路由但只有持有「刚生成的码」的人能用，泄露窗口与 CSRF 面都
// 收敛在 5 分钟内。配对 URL 走 TLS 局域网门（https://<LAN-IP>:<port>/mobile?pair=…）。
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"
	"github.com/skip2/go-qrcode"
)

// pairTTL 配对码有效期。
const pairTTL = 5 * time.Minute

// pairCodeAlphabet 去掉易混字符（0/O、1/I/L）的码表。
const pairCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

type pairState struct {
	code    string
	expires time.Time
}

// newPairCode 生成（或刷新）当前唯一有效的配对码。
func (s *Server) newPairCode() (string, time.Time) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见；退化为时间熵也不致卡死功能。
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> uint(i%8))
		}
	}
	code := make([]byte, 8)
	for i, b := range buf {
		code[i] = pairCodeAlphabet[int(b)%len(pairCodeAlphabet)]
	}
	expires := time.Now().Add(pairTTL)
	s.pair = &pairState{code: string(code), expires: expires}
	return string(code), expires
}

// redeemPairCode 校验并消费配对码（一次性）。
func (s *Server) redeemPairCode(code string) bool {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pair == nil || time.Now().After(s.pair.expires) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(s.pair.code)) != 1 {
		return false
	}
	s.pair = nil
	return true
}

// handlePairInfo 生成配对码与二维码（需登录）。
func (s *Server) handlePairInfo(ctx *vhttp.Context) error {
	if !s.tlsReady() {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "未开启局域网门（admin.tls.enabled），无法生成手机配对链接",
		})
	}
	ip := LANIPv4()
	if ip == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "本机没有局域网 IP（无网卡或纯回环环境），手机无法直连",
		})
	}
	code, expires := s.newPairCode()
	url := fmt.Sprintf("https://%s:%d/mobile?pair=%s", ip, s.tlsPort, code)
	png, err := qrcode.Encode(url, qrcode.Medium, 512)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "生成二维码失败"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"url":       url,
		"qr":        base64.StdEncoding.EncodeToString(png),
		"expiresAt": expires.UTC().Format(time.RFC3339),
		"caURL":     "/friday-ca.crt",
	})
}

// handlePairRedeem 用配对码换登录 JWT（公开路由；码一次性、5 分钟过期）。
func (s *Server) handlePairRedeem(ctx *vhttp.Context) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if !s.redeemPairCode(req.Code) {
		return ctx.JsonResponse(vhttp.Codes.Unauthorized, map[string]string{"error": "配对码无效或已过期，请在电脑端重新生成"})
	}
	acc := s.accounts.Current()
	token, err := s.issueToken(acc.Username)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "签发登录凭据失败，请重试"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{"token": token, "username": acc.Username})
}

// handleCACert 下发自签 CA 证书（公开静态；手机首次信任用）。TLS 未启用时 404。
func (s *Server) handleCACert(ctx *vhttp.Context) error {
	if len(s.caPEM) == 0 {
		ctx.GinContext().JSON(http.StatusNotFound, map[string]string{"error": "未开启局域网门（admin.tls.enabled）"})
		return nil
	}
	g := ctx.GinContext()
	g.Header("Content-Type", "application/x-x509-ca-cert")
	g.Header("Content-Disposition", `attachment; filename="friday-ca.crt"`)
	g.Data(http.StatusOK, "application/x-x509-ca-cert", s.caPEM)
	return nil
}

// tlsReady 局域网门是否已就绪（Start 时挂载成功）。
func (s *Server) tlsReady() bool { return s.tlsPort > 0 && len(s.caPEM) > 0 }
