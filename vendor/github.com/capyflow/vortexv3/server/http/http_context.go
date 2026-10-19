package vhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	vpkg "github.com/capyflow/vortexv3/pkg"
	"github.com/gin-gonic/gin"
)

type VContextKey string

func (k VContextKey) String() string {
	return string(k)
}

func (k VContextKey) VString() string {
	return "V-" + k.String()
}

var VContextKeys = struct {
	Session   VContextKey
	TraceId   VContextKey
	JwtVerify VContextKey
}{
	Session:   "Session",
	TraceId:   "Trace-Id",
	JwtVerify: "JwtVerify",
}

type handleFunc func(*Context) error

// WrapHandleFunc 包装处理函数，将其转换为gin.HandlerFunc
func WrapHandleFunc(handle handleFunc) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		// 生成这次请求的traceId
		vctx := &Context{gctx: ctx}
		vctx.gctx.Set(VContextKeys.TraceId.VString(), vpkg.GenerateTraceId())
		if err := handle(vctx); err != nil {
			ctx.Error(err)
			ctx.Abort()
		}
	}
}

// TODO 实现http的上下文
type Context struct {
	gctx *gin.Context
}

// Context 返回请求的上下文
func (ctx *Context) Context() context.Context {
	return ctx.gctx.Request.Context()
}

// GinContext 返回gin的上下文
func (ctx *Context) GinContext() *gin.Context {
	return ctx.gctx
}

// Get 获取上下文值
func (ctx *Context) Get(key string) (any, bool) {
	return ctx.gctx.Get(key)
}

// Set 设置上下文值
func (ctx *Context) Set(key string, value any) {
	ctx.gctx.Set(key, value)
}

// GetHeader 获取请求头
func (ctx *Context) GetHeader(key string) string {
	return ctx.gctx.Request.Header.Get(key)
}

func (ctx *Context) Lang() string {
	return ctx.GetHeader(HttpHeaderEnums.AcceptLanguage)
}

// SetHeader 设置响应头
func (ctx *Context) SetHeader(key string, value string) {
	ctx.gctx.Writer.Header().Set(key, value)
}

// 获取到TraceId
func (ctx *Context) GetTraceId() string {
	traceId, _ := ctx.gctx.Get(VContextKeys.TraceId.VString())
	return traceId.(string)
}

// 从Context中获取到Session
func (ctx *Context) GetVSession() *vpkg.VSession {
	session, ok := ctx.gctx.Get(VContextKeys.Session.VString())
	if !ok {
		return &vpkg.VSession{
			UID: "guest",
			IP:  ctx.GinContext().ClientIP(),
		}
	}
	vsession, ok := session.(vpkg.VSession)
	if !ok {
		return &vpkg.VSession{
			UID: "guest",
			IP:  ctx.GinContext().ClientIP(),
		}
	}
	return &vsession
}

// 从包装好的数据中解析出请求体
func (ctx *Context) UnmarshalBodyWithVStruct(v any) error {
	var vstruct vpkg.VStruct
	if err := ctx.UnmarshalBody(&vstruct); err != nil {
		return err
	}
	body, err := json.Marshal(vstruct.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return err
	}
	return nil
}

// 直接从请求头中解析数据,这个方法只可以调用一次
func (ctx *Context) UnmarshalBody(v any) error {
	return json.NewDecoder(ctx.gctx.Request.Body).Decode(v)
}

func (ctx *Context) applyResponseOptions(head vpkg.VHead, opts []VHttpResponseOption) {
	if len(opts) == 0 {
		return
	}

	vopt := &VHttpOption{}
	for _, opt := range opts {
		opt(vopt)
	}

	if vopt.Header != nil {
		for key, values := range vopt.Header {
			if len(values) == 0 {
				continue
			}
			ctx.gctx.Writer.Header().Set(key, values[0])
			for _, value := range values[1:] {
				ctx.gctx.Writer.Header().Add(key, value)
			}
		}
	}

	if head != nil && vopt.VHead != nil {
		for k, v := range vopt.VHead {
			head[k] = v
		}
	}
}

// json响应
func (ctx *Context) JsonResponse(code Code, body any, opts ...VHttpResponseOption) error {
	lang := strings.ToLower(ctx.Lang())
	if len(lang) == 0 {
		lang = "zh-cn"
	}

	// 构建响应
	ec := code.Code
	msgKey := code.MsgKey
	if code.SubCode.Code != 0 {
		ec = code.SubCode.Code
		msgKey = code.SubCode.MsgKey + "." + lang
	}
	vstruct := vpkg.VStruct{
		Body: body,
		Path: ctx.gctx.Request.URL.Path,
		Head: func() vpkg.VHead {
			head := vpkg.VHead{}
			head.SetTraceId(ctx.GetTraceId())
			head.SetTs(time.Now().Unix())
			head.SetEc(ec)
			head.SetEm(vpkg.GetI18n(msgKey))
			return head
		}(),
	}
	ctx.applyResponseOptions(vstruct.Head, opts)
	ctx.gctx.JSON(code.Code, vstruct)
	ctx.gctx.Abort()
	return nil
}

// 没有vstruct的响应
func (ctx *Context) JSON(code Code, body any, opts ...VHttpResponseOption) error {
	ctx.applyResponseOptions(nil, opts)
	ctx.gctx.JSON(code.Code, body)
	return nil
}

// StreamResponse 流式响应
func (ctx *Context) StreamResponse(r io.Reader, opts ...VHttpResponseOption) error {
	ctx.applyResponseOptions(nil, opts)
	contentType := ctx.GetHeader(HttpHeaderEnums.ContentType)
	if len(contentType) == 0 {
		contentType = "application/octet-stream"
	}
	_, err := io.Copy(ctx.gctx.Writer, r)
	return err
}

// sse 响应
func (ctx *Context) SSEResponse(event string, data any, opts ...VHttpResponseOption) error {
	vstruct := vpkg.VStruct{
		Body: data,
		Path: ctx.gctx.Request.URL.Path,
		Head: func() vpkg.VHead {
			head := vpkg.VHead{}
			head.SetTraceId(ctx.GetTraceId())
			head.SetTs(time.Now().Unix())
			head.SetEc(Codes.Success.Code)
			head.SetEm(vpkg.GetI18n(Codes.Success.MsgKey))
			return head
		}(),
	}
	ctx.applyResponseOptions(vstruct.Head, opts)
	ctx.gctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.gctx.SSEvent(event, vstruct)
	ctx.gctx.Writer.Flush()
	return nil
}

func (ctx *Context) HTMLResp(html string, data any, opts ...VHttpResponseOption) error {
	ctx.applyResponseOptions(nil, opts)
	ctx.gctx.HTML(http.StatusOK, html, data)
	return nil
}
