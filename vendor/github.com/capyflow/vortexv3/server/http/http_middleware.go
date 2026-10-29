package vhttp

import (
	"encoding/json"
	"errors"

	"github.com/capyflow/allspark-go/jwtx"
	vpkg "github.com/capyflow/vortexv3/pkg"
	"github.com/golang-jwt/jwt/v5"
)

func JwtMiddleWareFunc(opt *vpkg.JwtOption, skipExpireCheck bool) VortexHttpMiddleware {
	return func(ctx *Context) error {
		token := vpkg.LookUpToken(ctx.GinContext(), opt)
		claims, err := jwtx.ParseToken(opt.SecretKey, token)
		if err != nil {
			if errors.Is(err, jwt.ErrTokenExpired) && !skipExpireCheck {
				return ctx.JsonResponse(Codes.Unauthorized, vpkg.VMsgResponse{
					"error": "token expired",
				})
			}
			return ctx.JsonResponse(Codes.Unauthorized, vpkg.VMsgResponse{
				"error": "token parse error," + err.Error(),
			})
		}
		raw, err := json.Marshal(claims)
		if nil != err {
			return ctx.JsonResponse(Codes.Unauthorized, vpkg.VMsgResponse{
				"error": "token marshal failed",
			})
		}
		var session vpkg.VSession
		err = json.Unmarshal(raw, &session)
		if nil != err {
			return ctx.JsonResponse(Codes.Unauthorized, vpkg.VMsgResponse{
				"error": "token unmarshal failed",
			})
		}
		session.IP = ctx.GinContext().ClientIP()
		ctx.Set(VContextKeys.Session.VString(), session)
		return nil
	}
}
