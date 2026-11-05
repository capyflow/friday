// terminal_test.go 用真 PTY + /bin/sh 跑一遍：起终端、送键盘、读输出、改尺寸、
// 断开收尾。Windows 上这套（ConPTY）还没做，整个文件跳过。
package terminal

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{Enabled: true, MaxSessions: DefaultMaxSessions, Shell: "/bin/sh"}
}

func managerOf(t *testing.T, cfg Config) *Manager {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 PTY（ConPTY）还没做")
	}
	m := NewManager(func() Config { return cfg })
	t.Cleanup(m.CloseAll)
	return m
}

// readUntil 从输出通道里读到出现 want 为止（超时即失败）：终端输出是分块来的，
// 不能假设一次 Read 就拿到整行。
func readUntil(t *testing.T, s *Session, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got strings.Builder
	for time.Now().Before(deadline) {
		select {
		case chunk, ok := <-s.Output():
			if !ok {
				return got.String()
			}
			got.Write(chunk)
			if strings.Contains(got.String(), want) {
				return got.String()
			}
		case <-time.After(timeout / 4):
		}
	}
	t.Fatalf("终端输出里没等到 %q，实际: %q", want, got.String())
	return ""
}

func TestStartEchoAndClose(t *testing.T) {
	m := managerOf(t, testConfig())
	dir := t.TempDir()

	s, err := m.Start(80, 24, dir)
	if err != nil {
		t.Fatalf("起终端失败: %v", err)
	}
	if s.Shell != "/bin/sh" || s.Cwd != dir {
		t.Fatalf("会话信息不对: %+v", s.Info())
	}
	// 注意 PTY 会把敲进去的命令原样回显，所以等的必须是"算出来的结果"而不是
	// 命令里出现的字面量——否则回显自己就能把断言喂饱。
	if _, err := s.Write([]byte("pwd\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out := readUntil(t, s, filepath.Base(dir), 10*time.Second)
	// 起始目录生效才算"这个终端在这条会话的目录里开着"
	if !strings.Contains(out, filepath.Base(dir)) {
		t.Fatalf("pwd 应回显起始目录 %q，实际输出: %q", dir, out)
	}
	if _, err := s.Write([]byte("echo ok-$((1+1))\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if out = readUntil(t, s, "ok-2", 10*time.Second); !strings.Contains(out, "ok-2") {
		t.Fatalf("命令没跑出来: %q", out)
	}

	s.Close()
	select {
	case ex := <-s.Done():
		_ = ex // 关闭即收进程组：Done 必须在有限时间内落地
	case <-time.After(3 * time.Second):
		t.Fatal("Close 后进程组没被收掉")
	}
	if got := m.Count(); got != 0 {
		t.Fatalf("结束的终端应让出名额，实际还占 %d", got)
	}
}

func TestResizeReachesShell(t *testing.T) {
	m := managerOf(t, testConfig())
	s, err := m.Start(80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(40, 12); err != nil {
		t.Fatalf("改尺寸失败: %v", err)
	}
	if _, err := s.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "12 40", 10*time.Second)
	if !strings.Contains(out, "12 40") {
		t.Fatalf("PTY 尺寸没同步给 shell，stty size 实际回: %q", out)
	}
	if err := s.Resize(0, 0); err == nil {
		t.Fatal("非法尺寸应报错")
	}
}

func TestMaxSessionsCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxSessions = 1
	m := managerOf(t, cfg)

	first, err := m.Start(80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(80, 24, "")
	if err == nil || !strings.Contains(err.Error(), "上限 1") {
		t.Fatalf("第二个终端应被上限拦住并说清上限: %v", err)
	}
	first.Close()
	// 名额要还得回来：等进程真收掉再开第二个
	deadline := time.Now().Add(3 * time.Second)
	for m.Count() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Start(80, 24, ""); err != nil {
		t.Fatalf("关掉一个后应能再开: %v", err)
	}
}

func TestConfiguredShellMissingIsError(t *testing.T) {
	cfg := testConfig()
	cfg.Shell = filepath.Join(t.TempDir(), "没这个 shell")
	m := managerOf(t, cfg)
	_, err := m.Start(80, 24, "")
	if err == nil || !strings.Contains(err.Error(), "不可执行") {
		t.Fatalf("配置里的 shell 不存在应直说: %v", err)
	}
}

func TestDisabledAndBadCwd(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	m := managerOf(t, cfg)
	if _, err := m.Start(80, 24, ""); err == nil || !strings.Contains(err.Error(), "未开启") {
		t.Fatalf("关掉后应拒绝并指路设置: %v", err)
	}

	m2 := managerOf(t, testConfig())
	if _, err := m2.Start(80, 24, filepath.Join(t.TempDir(), "friday-没这个目录")); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("起始目录不存在应报错: %v", err)
	}
	if _, err := m2.Start(0, 0, ""); err == nil || !strings.Contains(err.Error(), "尺寸非法") {
		t.Fatalf("尺寸没量到应报错并提示刷新: %v", err)
	}
}

// TestResolveShellFallback 没配 shell 时要按 $SHELL → zsh → bash → sh 落到一个
// 真能执行的；配置里写了个本机没有的名字则明确拒绝，不悄悄换一个。
func TestResolveShellFallback(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	if got, err := resolveShell(""); err != nil || got != "/bin/sh" {
		t.Fatalf("$SHELL 应优先: %q %v", got, err)
	}
	t.Setenv("SHELL", "")
	got, err := resolveShell("")
	if err != nil {
		t.Fatalf("没有 $SHELL 也要落到常见 shell: %v", err)
	}
	if !strings.HasSuffix(got, "/sh") && !strings.HasSuffix(got, "/bash") && !strings.HasSuffix(got, "/zsh") {
		t.Fatalf("回落结果不像 shell: %q", got)
	}
	if _, err := resolveShell("sh"); err == nil {
		t.Fatal("配置里写了本机没有的 shell 名应报错，不该悄悄回落")
	}
}
