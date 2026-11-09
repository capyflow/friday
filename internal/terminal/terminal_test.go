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

func TestNoSessionCapByDefault(t *testing.T) {
	cfg := testConfig() // MaxSessions 取默认值：不限
	m := managerOf(t, cfg)
	if got := cfg.Max(); got != 0 {
		t.Fatalf("默认上限应为 0（不限），实际 %d", got)
	}
	// 旧默认是 4，这里开 5 个证明默认不再拦
	for i := 0; i < 5; i++ {
		if _, err := m.Start(80, 24, ""); err != nil {
			t.Fatalf("第 %d 个终端不该被拦: %v", i+1, err)
		}
	}
	if m.Count() != 5 {
		t.Fatalf("应有 5 个活跃终端，实际 %d", m.Count())
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

// TestEnsureUTF8 覆盖"继承的字符集不是 UTF-8 就补一个"这条规则：已经是 UTF-8 的
// 一律不动；要补时把 LANG/LC_ALL/LC_CTYPE 三个旧值一并换掉（留着 LC_CTYPE=C 会压住
// LANG，所以旧的必须消失）。
func TestEnsureUTF8(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		mut  bool // 期望改动环境
	}{
		{name: "LC_ALL 已是 UTF-8", env: []string{"LC_ALL=en_US.UTF-8", "LANG=C"}},
		{name: "LANG 已是小写 utf8", env: []string{"LANG=zh_CN.utf8"}},
		{name: "LC_CTYPE 是 UTF-8 也算", env: []string{"LC_CTYPE=C.UTF-8", "LANG=C"}},
		{name: "GUI 启动没有 locale 变量", env: []string{"PATH=/bin"}, mut: true},
		{name: "LC_ALL=C 压着一切", env: []string{"LANG=en_US.UTF-8", "LC_ALL=C"}, mut: true},
		{name: "只剩非 UTF-8 的 LC_CTYPE", env: []string{"LC_CTYPE=C"}, mut: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, loc := ensureUTF8(c.env)
			if !c.mut {
				if len(got) != len(c.env) || loc != "" {
					t.Fatalf("已是 UTF-8 不该改动: %v（补了 %q）", got, loc)
				}
				return
			}
			if loc == "" {
				t.Skipf("本机没有可用的 UTF-8 locale，这条规则在此机上无从验证（env=%v）", c.env)
			}
			if !isUTF8(effectiveLocale(got)) {
				t.Fatalf("补完仍不是 UTF-8: %v（挑到 %q）", got, loc)
			}
			// 旧的 LANG/LC_ALL/LC_CTYPE 只能剩下我们补的那两个，别留一个非 UTF-8 的在旁边压着
			for _, kv := range got {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "LANG", "LC_ALL":
					if v != loc {
						t.Fatalf("%s 应是被补的 %q，实际 %q（全量 %v）", k, loc, v, got)
					}
				case "LC_CTYPE":
					t.Fatalf("旧的 LC_CTYPE 没换掉: %q（全量 %v）", kv, got)
				}
			}
		})
	}
}

// TestPickUTF8KeepsLanguage 现有 locale 只换编码不换语言：zh_CN.GBK 应先看 zh_CN.UTF-8。
// 本机没装那个 locale 时挑不到，属正常，只验证"挑到的一定是 UTF-8 且语言没跑偏"。
func TestPickUTF8KeepsLanguage(t *testing.T) {
	loc := pickUTF8("zh_CN.GBK")
	if loc == "" {
		t.Skip("本机没有任何候选 UTF-8 locale")
	}
	if !isUTF8(loc) {
		t.Fatalf("挑出来的不是 UTF-8: %q", loc)
	}
	if avail := availableLocales(); !avail[strings.ToLower(loc)] {
		t.Fatalf("挑出来的 locale 本机不存在: %q", loc)
	}
}

// TestShellRunsInUTF8Locale 是真 PTY 上的回归闸：把测试进程的 LC_ALL 按成 C
// （GUI 启动的外壳就是这个处境），起终端后 shell 里 `locale charmap` 必须回 UTF-8。
// 少了 ensureUTF8 这一步，zsh 的行编辑器按字节数宽度，界面里的中文和提示符箭头会
// 画成一片 <0080> 方框——这条用例就是把那个缺陷按住的。
func TestShellRunsInUTF8Locale(t *testing.T) {
	if pickUTF8("C") == "" {
		t.Skip("本机没有可用的 UTF-8 locale")
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "")
	t.Setenv("LC_CTYPE", "")

	m := managerOf(t, testConfig())
	s, err := m.Start(80, 24, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("locale charmap\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "UTF-8", 10*time.Second)
	if !strings.Contains(out, "UTF-8") {
		t.Fatalf("shell 的字符集应是 UTF-8，locale charmap 实际回: %q", out)
	}
}
