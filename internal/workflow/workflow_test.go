package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// T0 竖切测试：静态预检（check）、跨界协议（run 的帧往返）、执行器（world.run / files.*）
// 与进程可牺牲性（超时杀宿主）。见 docs/WORKFLOW.md 第十一节。

func mustHost(t *testing.T) *Host {
	t.Helper()
	h, err := Detect("", "")
	if err != nil {
		t.Skipf("本机没有可用的脚本宿主：%v", err)
	}
	h.MaxOldSpaceMB = 512
	if _, err := h.Version(context.Background()); err != nil {
		t.Skipf("宿主 version 失败：%v", err)
	}
	return h
}

func TestDetectFindsHostDir(t *testing.T) {
	h, err := Detect("", "")
	if err != nil {
		t.Fatalf("从包目录向上找 wfhost 应当成功：%v", err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, hostEntry)); err != nil {
		t.Fatalf("定位到的目录里没有入口文件：%v", err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, "facade.d.ts")); err != nil {
		t.Fatalf("定位到的目录里没有 facade.d.ts：%v", err)
	}
}

func TestCheckInventory(t *testing.T) {
	h := mustHost(t)
	res, err := h.Check(context.Background(), filepath.Join("testdata", "inventory.wf.ts"))
	if err != nil {
		t.Fatalf("check 调用失败：%v", err)
	}
	if !res.OK {
		t.Fatalf("fixture 应当通过预检，诊断：\n%s", DiagnosticsText(res.Diagnostics))
	}
	if want := []string{"摸清目录并逐目录阅读", "用构建作为确定性门"}; !equalStrings(res.Phases, want) {
		t.Errorf("阶段图 = %v，期望 %v", res.Phases, want)
	}
	if !contains(res.Commands, "go") {
		t.Errorf("命令全集应含 go（字面量收集），得到 %v", res.Commands)
	}
	if res.Meta == nil || res.Meta.Args["max_dirs"].Type != "number" {
		t.Fatalf("元数据入参未解析出来：%+v", res.Meta)
	}
	if len(res.AskSites) != 1 {
		t.Fatalf("ask 站点应为 1，得到 %d", len(res.AskSites))
	}
	site := res.AskSites[0]
	if site.Raw {
		t.Errorf("该 ask 带类型参数，不应是 raw")
	}
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(site.Schema, &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v", err)
	}
	if schema.Type != "object" || !contains(schema.Required, "hasEntry") {
		t.Errorf("schema 形态不对：%s", site.Schema)
	}
	if got := schema.Properties["purpose"].Description; got == "" {
		t.Errorf("JSDoc 应当变成字段说明（这是最便宜的提质手段），得到空")
	}
}

func TestCheckRejects(t *testing.T) {
	h := mustHost(t)
	cases := []struct {
		file     string
		wantSrc  string
		wantRule string
	}{
		{"bad_type.wf.ts", "tsc", ""},
		{"bad_command.wf.ts", "lint", "R1"},
		{"bad_phase.wf.ts", "lint", "R3"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			res, err := h.Check(context.Background(), filepath.Join("testdata", c.file))
			if err != nil {
				t.Fatalf("check 调用失败：%v", err)
			}
			if res.OK {
				t.Fatalf("%s 应当被预检拦下", c.file)
			}
			if len(res.Phases) != 0 || len(res.Commands) != 0 {
				t.Errorf("有诊断时不应给出阶段图/命令集：%v %v", res.Phases, res.Commands)
			}
			if c.wantSrc == "tsc" && !hasSource(res, "tsc") {
				t.Errorf("期望 tsc 诊断，得到：\n%s", DiagnosticsText(res.Diagnostics))
			}
			if c.wantRule != "" && !hasRule(res, c.wantRule) {
				t.Errorf("期望规则 %s，得到：\n%s", c.wantRule, DiagnosticsText(res.Diagnostics))
			}
		})
	}
}

// bad_phase 同时应命中 R2（空阶段）与 R8（决定论）。
func TestCheckFlagsEmptyPhaseAndNondeterminism(t *testing.T) {
	h := mustHost(t)
	res, err := h.Check(context.Background(), filepath.Join("testdata", "bad_phase.wf.ts"))
	if err != nil {
		t.Fatalf("check 失败：%v", err)
	}
	for _, rule := range []string{"R2", "R8"} {
		if !hasRule(res, rule) {
			t.Errorf("缺少 %s 诊断：\n%s", rule, DiagnosticsText(res.Diagnostics))
		}
	}
}

func TestGuardDenyPatterns(t *testing.T) {
	if err := GuardDenyPatterns("echo hi"); err != nil {
		t.Errorf("普通命令不应被拦：%v", err)
	}
	if err := GuardDenyPatterns("rm -rf /"); err == nil {
		t.Error("毁灭性命令必须被拦")
	}
	if err := GuardDenyPatterns("formatters"); err != nil {
		t.Errorf("词边界不该误伤：formatters → %v", err)
	}
}

func TestRunBasic(t *testing.T) {
	h := mustHost(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("周五的备忘"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		calls []AskRequest
	)
	ex := &Local{Root: root, Asker: func(_ context.Context, req AskRequest) (AskReply, error) {
		mu.Lock()
		calls = append(calls, req)
		mu.Unlock()
		if req.WantsSchema() {
			return AskReply{Value: json.RawMessage(`{"file":"a.txt","what":"备忘"}`)}, nil
		}
		return AskReply{Text: "memo"}, nil
	}}

	var eventCount atomic.Int32
	out, err := Run(context.Background(), h, filepath.Join("testdata", "run_basic.wf.ts"),
		map[string]any{"greeting": "hello"}, ex, RunOpts{OnEvent: func(Event) { eventCount.Add(1) }})
	if err != nil {
		t.Fatalf("run 失败：%v", err)
	}

	if !strings.Contains(string(out.Value), "hello from world.run") {
		t.Errorf("脚本 return 的值里没有命令输出：%s", out.Value)
	}
	if got := out.PhasesSeen(); len(got) != 2 || got[0] != "读一个文件并跑一条命令" {
		t.Errorf("阶段事件顺序不对：%v", got)
	}
	if n := len(out.Reports()); n != 2 {
		t.Errorf("report 应有 2 条（边跑边报），得到 %d", n)
	}
	if int(eventCount.Load()) < 4 {
		t.Errorf("OnEvent 回调应覆盖全部上行事件，得到 %d", eventCount.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("应有两次 ask，得到 %d", len(calls))
	}
	var typed *AskRequest
	typedCount := 0
	for i := range calls {
		if calls[i].WantsSchema() {
			typed = &calls[i]
			typedCount++
		}
	}
	if typedCount != 1 {
		t.Fatalf("两路 ask 里只应有带类型参数的那一路有 schema，得到 %d（raw ask 不应带 schema 字段）", typedCount)
	}
	if typed == nil {
		t.Fatal("带类型参数的 ask 没把 schema 送回 Go")
	}
	if typed.Agent == "" || typed.Instructions == "" {
		t.Errorf("ask 缺少子代理身份或任务全文：%+v", typed)
	}
	if !strings.Contains(string(typed.Schema), `"file"`) {
		t.Errorf("schema 里应有字段 file：%s", typed.Schema)
	}
}

// 一期把「node 进程可牺牲」当成不变量：超时必须能杀掉宿主并带出已上报的产出。
func TestRunTimeoutKillsHost(t *testing.T) {
	h := mustHost(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Local{Root: root, Asker: func(ctx context.Context, _ AskRequest) (AskReply, error) {
		<-ctx.Done() // 永不返回，逼出超时路径
		return AskReply{}, ctx.Err()
	}}

	start := time.Now()
	_, err := Run(context.Background(), h, filepath.Join("testdata", "run_basic.wf.ts"), nil, ex,
		RunOpts{Timeout: 3 * time.Second})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误里应能看出是超时：%v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("超时后没能及时收摊，等了 %s", d)
	}
}

// files.* 的路径边界：脚本给的路径不可信。
func TestFilesPathGuard(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("本机不支持符号链接：%v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := &Local{Root: root}
	ctx := context.Background()

	for _, p := range []string{"../escape.txt", "/etc/hosts", "a/../../x", "link.txt"} {
		if _, err := ex.FilesRead(ctx, p); err == nil {
			t.Errorf("%q 应当被拒绝", p)
		}
	}
	if _, err := ex.FilesRead(ctx, "note.txt"); err == nil {
		t.Error("不存在的文件应报错")
	}
}

func TestGlobAndGrep(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.go", "package a\n// 说明\n")
	write(t, root, "src/nested/b.go", "package nested\n")
	write(t, root, "src/notes.txt", "package not-go\n")
	ex := &Local{Root: root}
	ctx := context.Background()

	got, err := ex.FilesGlob(ctx, "src/**/*.go")
	if err != nil {
		t.Fatalf("glob 失败：%v", err)
	}
	if !equalStrings(got, []string{"src/a.go", "src/nested/b.go"}) {
		t.Errorf("glob 结果 = %v", got)
	}

	hits, err := ex.FilesGrep(ctx, "^package ", "**/*.go")
	if err != nil {
		t.Fatalf("grep 失败：%v", err)
	}
	if len(hits) != 2 {
		t.Errorf("grep 命中 = %d，期望 2：%+v", len(hits), hits)
	}
	if _, err := ex.FilesGrep(ctx, "(a)(b)\\1", "**/*"); err == nil {
		t.Error("RE2 不支持的后向引用应报错而不是静默")
	}
}

// 不在仓库里时 git.* 必须可捕获地失败（脚本要 catch）。
func TestGitOutsideRepo(t *testing.T) {
	ex := &Local{Root: t.TempDir()}
	if _, err := ex.GitStatus(context.Background()); err == nil {
		t.Error("非仓库目录里 git.status 应报错")
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasSource(res *CheckResult, src string) bool {
	for _, d := range res.Diagnostics {
		if d.Source == src {
			return true
		}
	}
	return false
}

func hasRule(res *CheckResult, rule string) bool {
	for _, d := range res.Diagnostics {
		if d.Rule == rule {
			return true
		}
	}
	return false
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
