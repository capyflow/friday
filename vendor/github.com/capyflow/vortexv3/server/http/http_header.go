package vhttp

import (
	"strconv"
)

var HttpHeaderEnums = struct {
	AcceptLanguage           string
	Accept                   string
	Authorization            string
	CacheControl             string
	Connection               string
	ContentDisposition       string
	ContentLength            string
	ContentType              string
	Cookie                   string
	Host                     string
	Origin                   string
	Referer                  string
	SetCookie                string
	UserAgent                string
	AccessControlAllowOrigin string
	XForwardedFor            string
	XRequestedWith           string
	Range                    string
}{
	Accept:                   "Accept",
	AcceptLanguage:           "Accept-Language",
	Authorization:            "Authorization",
	CacheControl:             "Cache-Control",
	Connection:               "Connection",
	ContentDisposition:       "Content-Disposition",
	ContentLength:            "Content-Length",
	ContentType:              "Content-Type",
	Cookie:                   "Cookie",
	Host:                     "Host",
	Origin:                   "Origin",
	Referer:                  "Referer",
	SetCookie:                "Set-Cookie",
	UserAgent:                "User-Agent",
	AccessControlAllowOrigin: "Access-Control-Allow-Origin",
	XForwardedFor:            "X-Forwarded-For",
	XRequestedWith:           "X-Requested-With",
	Range:                    "Range",
}

// 设置contentLength
func WithContentLength(length int) VHttpResponseOption {
	return WithHttpResponseHeader(HttpHeaderEnums.ContentLength, strconv.FormatInt(int64(length), 10))
}

// 设置contentType
func WithContentType(contentType string) VHttpResponseOption {
	return WithHttpResponseHeader(HttpHeaderEnums.ContentType, contentType)
}
