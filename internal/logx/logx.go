// Package logx 提供带请求 ID 的链路日志：一次用户输入或提醒触发分配一个请求 ID
// 挂进 context，沿途各环节（LLM、工具、TTS 等）的日志自动带上 "[req-N] " 前缀，
// 终端里按前缀过滤即可看到一次交互的完整调用链。
package logx

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
)

var seq atomic.Int64

// NextID 生成递增的请求 ID，形如 "req-1"。
func NextID() string {
	return fmt.Sprintf("req-%d", seq.Add(1))
}

type idKey struct{}

// WithID 返回挂上了请求 ID 的 ctx。
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, idKey{}, id)
}

// IDFrom 取出 ctx 中的请求 ID；未设置时返回空串。
func IDFrom(ctx context.Context) string {
	id, _ := ctx.Value(idKey{}).(string)
	return id
}

// Printf 打印日志；ctx 带请求 ID 时自动加 "[req-N] " 前缀。
func Printf(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if id := IDFrom(ctx); id != "" {
		msg = "[" + id + "] " + msg
	}
	log.Print(msg)
}

// Truncate 按 rune 截断字符串，超长以 "…" 结尾（中文安全）。
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
