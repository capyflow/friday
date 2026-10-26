package acp

// proc_test.go 覆盖进程拉起的纯函数面：env 合并（同名 key 覆盖生效、结果无
// 重复——直接 append 会让 libc 子进程读到旧值）与 stderr 尾部截取。

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

func envKeys(env []string) map[string][]string {
	out := map[string][]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			out[kv] = append(out[kv], "")
			continue
		}
		out[kv[:i]] = append(out[kv[:i]], kv[i+1:])
	}
	return out
}

func TestBuildEnvOverride(t *testing.T) {
	inherit := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"LANG=zh_CN.UTF-8",
		"EXISTING=old",
	}
	got := buildEnv(inherit, map[string]string{
		"HOME":     "/tmp/agent-home",
		"EXISTING": "new",
		"FRESH":    "added",
	})

	keys := envKeys(got)
	for name, want := range map[string]string{
		"PATH":     "/usr/bin:/bin", // 未覆盖的 key 原样保留
		"HOME":     "/tmp/agent-home",
		"EXISTING": "new",
		"FRESH":    "added",
		"LANG":     "zh_CN.UTF-8",
	} {
		if len(keys[name]) != 1 {
			t.Fatalf("%s 应恰好出现一次，实际 %d 次: %v", name, len(keys[name]), keys[name])
		}
		if keys[name][0] != want {
			t.Fatalf("%s = %q，期望 %q", name, keys[name][0], want)
		}
	}
}

func TestBuildEnvDuplicateInheritKeepsFirst(t *testing.T) {
	// 继承环境自身有重复时保留首条（与 libc getenv 同语义），配置覆盖仍生效。
	inherit := []string{"FOO=first", "FOO=second", "BAR=x"}
	got := buildEnv(inherit, map[string]string{"BAR": "y"})
	keys := envKeys(got)
	if len(keys["FOO"]) != 1 || keys["FOO"][0] != "first" {
		t.Fatalf("继承重复应保留首条，实际 %v", keys["FOO"])
	}
	if len(keys["BAR"]) != 1 || keys["BAR"][0] != "y" {
		t.Fatalf("配置覆盖应生效，实际 %v", keys["BAR"])
	}
}

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
