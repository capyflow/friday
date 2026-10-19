package vhttp

import "net/http"

type VHttpOption struct {
	VHead  map[string]any
	Header http.Header
}

type VHttpResponseOption func(*VHttpOption)

// WithVHttpHead 设置响应头
func WithVHttpHead(head map[string]any) VHttpResponseOption {
	return func(opt *VHttpOption) {
		if opt.VHead == nil {
			opt.VHead = make(map[string]any)
		}
		for k, v := range head {
			opt.VHead[k] = v
		}
	}
}

// WithHttpResponseHeader 设置响应头
func WithHttpResponseHeader(key string, value string) VHttpResponseOption {
	return func(opt *VHttpOption) {
		if opt.Header == nil {
			opt.Header = make(http.Header)
		}
		opt.Header.Set(key, value)
	}
}
