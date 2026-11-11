package acp

// proc_test.go 覆盖进程拉起的纯函数面：init 错误分类与 stderr 尾部截取。
// env 合并测试随 buildEnv 下沉到 config.MergeEnv（internal/config/npmcache_test.go）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/config"
)

func TestStderrTailAfterKill(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent.stderr.log")
	content := "line-1: something went wrong\nline-2: not logged in\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &agentProc{stderrFile: file}
	if got := p.stderrTail(2); !strings.Contains(got, "not logged in") {
		t.Fatalf("kill 前应能读到文件尾部: %q", got)
	}
	// 模拟 kill：缓存尾部 + 删文件，报错路径仍可引用。
	p.stderrMu.Lock()
	p.stderrMemo = readTail(file, 64*1024)
	p.stderrMu.Unlock()
	os.Remove(file)
	if got := p.stderrTail(2); !strings.Contains(got, "not logged in") {
		t.Fatalf("kill 后报错仍应引用缓存的 stderr: %q", got)
	}
	if got := p.stderrTail(1); strings.Contains(got, "line-1") {
		t.Fatalf("n=1 应只留最后一行: %q", got)
	}
}

// expiredCtx 返回已过期的上下文：classifyInitError 靠它识别 SDK 包装过的超时。
func expiredCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	return ctx
}

func TestClassifyInitError(t *testing.T) {
	cfg := config.AcpAgentConfig{Name: "codex", Command: "npx"}

	// 进程秒退 + stderr 命中登录关键词 → 指引（含具体命令）。
	exited := make(chan struct{})
	close(exited)
	got := classifyInitError(context.Background(), errors.New("rpc closed"), "Error: Not logged in · run `codex login`", exited, cfg)
	if !strings.Contains(got, "疑似未登录") || !strings.Contains(got, "codex login") {
		t.Fatalf("秒退+登录关键词应给出登录指引，实际 %q", got)
	}
	// 秒退 + 无关键词 → 命令问题。
	got = classifyInitError(context.Background(), errors.New("rpc closed"), "boom: config broken", exited, cfg)
	if !strings.Contains(got, "秒退") {
		t.Fatalf("秒退+无关键词应归为命令问题，实际 %q", got)
	}
	// 活着但超时（C7 的 gemini 形态）。
	alive := make(chan struct{})
	got = classifyInitError(expiredCtx(), context.DeadlineExceeded, "", alive, cfg)
	if !strings.Contains(got, "握手超时") {
		t.Fatalf("超时应归为握手超时，实际 %q", got)
	}
	// SDK 把超时包成不带 Unwrap 链的 *RequestError，errors.Is 认不出，
	// 只能靠 ictx 判断（proc.go 的回归点：曾被误报成「协议层错误」）。
	got = classifyInitError(expiredCtx(), errors.New("internal error: context deadline exceeded"), "", alive, cfg)
	if !strings.Contains(got, "握手超时") {
		t.Fatalf("SDK 包装的超时应归为握手超时，实际 %q", got)
	}
	// 活着且报错文本带凭据关键词 → 指引。
	got = classifyInitError(context.Background(), errors.New("request failed: 401 unauthorized"), "", alive, cfg)
	if !strings.Contains(got, "疑似未登录") {
		t.Fatalf("401 应归为未登录，实际 %q", got)
	}
	// 未知名目 + 未知名 stderr → 通用协议层错误。
	got = classifyInitError(context.Background(), errors.New("rpc rejected"), "weird payload", alive, cfg)
	if !strings.Contains(got, "协议层错误") {
		t.Fatalf("未知名目应归为协议层错误，实际 %q", got)
	}
	// 指引表按命令基名兜底：配置名不认识时看命令。
	cfgG := config.AcpAgentConfig{Name: "我的助手", Command: "/usr/local/bin/gemini"}
	got = classifyInitError(context.Background(), errors.New("x"), "please log in first", exited, cfgG)
	if !strings.Contains(got, "Google 登录") {
		t.Fatalf("命令基名应能命中指引表，实际 %q", got)
	}
}
