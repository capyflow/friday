package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

// stringsOf 把 fake 记录的 params（JSON 解出来的 []any）折成字符串切片。
func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("解析 params: %v", err)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 additionalDirectories: %v", err)
	}
	return out
}

// 工作区的多目录语义：主目录当 cwd，其余目录作为 additionalDirectories 下发——
// 前提是 agent 在 initialize 里声明了 sessionCapabilities.additionalDirectories。
func TestWorkspaceExtraDirsFollowCapability(t *testing.T) {
	f := &fakeAgent{extraRoots: true, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	dirA, dirB := t.TempDir(), t.TempDir()
	ws := mustWorkspace(t, m, dirA, dirB)
	th := mustThreadIn(t, m, ws)
	if th.Cwd != dirA {
		t.Fatalf("会话主目录应是工作区第一个目录，实际 %s", th.Cwd)
	}

	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	p := f.sessionNewParams()
	if p == nil || p["cwd"] != dirA {
		t.Fatalf("session/new 的 cwd 应是主目录: %v", p)
	}
	if got := stringsOf(t, p["additionalDirectories"]); len(got) != 1 || got[0] != dirB {
		t.Fatalf("声明支持时应把其余目录作扩展根下发，实际 %v", got)
	}

	// 目录视图：进程在场 → 能力已知、扩展根就是刚下发的那份
	cwd, dirs, extra, known, supported := m.SessionDirs(th.ID)
	if cwd != dirA || len(dirs) != 2 || !known || !supported {
		t.Fatalf("SessionDirs 视图不符: cwd=%s dirs=%v known=%v supported=%v", cwd, dirs, known, supported)
	}
	if len(extra) != 1 || extra[0] != dirB {
		t.Fatalf("扩展根应回带 %s，实际 %v", dirB, extra)
	}
}

// agent 没声明该能力时不硬塞字段（协议外的字段可能直接被拒），多出的目录只是
// Friday 侧的分组——前端据 known/supported 明确说"只用主目录"。
func TestWorkspaceExtraDirsSkippedWhenUnsupported(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript} // extraRoots=false
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, t.TempDir(), t.TempDir())
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	p := f.sessionNewParams()
	if p == nil {
		t.Fatal("未记录 session/new params")
	}
	if v, ok := p["additionalDirectories"]; ok {
		t.Fatalf("未声明能力不应下发该字段，实际 %v", v)
	}
	if _, _, _, known, supported := m.SessionDirs(th.ID); !known || supported {
		t.Fatalf("能力应已知为不支持: known=%v supported=%v", known, supported)
	}
}

// 会话的 cwd 一经创建就锚定（D1）：工作区后面加目录只扩扩展根，改主目录归属
// 也不动既有会话；下一次重接会话按当前工作区目录重新给出扩展根。
func TestWorkspaceDirChangesFollowNextResume(t *testing.T) {
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	f := &fakeAgent{extraRoots: true, loadSession: true, loadOK: true, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, dirA)
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "第一轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}

	// 工作区扩到三个目录，并把原来的主目录移出（会话仍锚在它上面）
	if _, err := m.AddWorkspaceDir(ws.ID, dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddWorkspaceDir(ws.ID, dirC); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RemoveWorkspaceDir(ws.ID, dirA); err != nil {
		t.Fatalf("移出主目录应允许（会话自己记着 cwd）: %v", err)
	}
	// 进程回收一次，模拟空闲回收后的重接
	m.killLive(th.ID, "测试：回收进程")
	if _, err := m.Send(context.Background(), th.ID, "第二轮", nil, TurnHooks{}); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	p := f.sessionLoadParams()
	if p == nil || p["cwd"] != dirA {
		t.Fatalf("重接会话必须用原来的 cwd（协议要求与建会话时一致）: %v", p)
	}
	got := stringsOf(t, p["additionalDirectories"])
	if len(got) != 2 || got[0] != dirB || got[1] != dirC {
		t.Fatalf("扩展根应跟着工作区当前目录，实际 %v", got)
	}
}

// 已经不存在的扩展目录不发给 agent（它会直接拒绝建会话），但仍是工作区的一部分。
func TestMissingExtraDirIsNotSent(t *testing.T) {
	keep, gone := t.TempDir(), filepath.Join(t.TempDir(), "not-created")
	f := &fakeAgent{extraRoots: true, promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	ws := mustWorkspace(t, m, keep)
	if _, err := m.AddWorkspaceDir(ws.ID, gone); err != nil {
		t.Fatalf("加目录（不存在时自动建）应成功: %v", err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	if got := stringsOf(t, f.sessionNewParams()["additionalDirectories"]); len(got) != 0 {
		t.Fatalf("已消失的目录不该下发，实际 %v", got)
	}
}

// 删除工作区：连带它的会话（投影 + 存活进程 + 并发槽位），磁盘目录不碰。
func TestDeleteWorkspaceRemovesSessionsAndProcesses(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	dir := t.TempDir()
	ws := mustWorkspace(t, m, dir)
	th := mustThreadIn(t, m, ws)
	if _, err := m.Send(context.Background(), th.ID, "开工", nil, TurnHooks{}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	alive := m.live[th.ID] != nil
	m.mu.Unlock()
	if !alive {
		t.Fatal("turn 后进程应在场（空闲回收计时内）")
	}
	if ids := m.ThreadsInWorkspace(ws.ID); len(ids) != 1 || ids[0] != th.ID {
		t.Fatalf("工作区里应有这 1 个会话，实际 %v", ids)
	}

	if !m.DeleteWorkspace(ws.ID) {
		t.Fatal("删除工作区失败")
	}
	if _, ok := m.Get(th.ID); ok {
		t.Fatal("会话应随工作区一起删除")
	}
	m.mu.Lock()
	stillLive := m.live[th.ID]
	m.mu.Unlock()
	if stillLive != nil {
		t.Fatal("会话进程应已回收")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("删除工作区不应碰磁盘目录: %v", err)
	}

	// 槽位归还后可以继续开新工作区并跑对话（不会因没归还槽而排队）
	ws2 := mustWorkspace(t, m, t.TempDir())
	th2 := mustThreadIn(t, m, ws2)
	if _, err := m.Send(context.Background(), th2.ID, "再来", nil, TurnHooks{}); err != nil {
		t.Fatalf("删除后仍应能对话: %v", err)
	}
}

// 改名与去重：名字留空回落主目录名；同目录不重复加。
func TestWorkspaceRenameAndDirDedup(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, nil, nil, nil)
	ws, err := m.CreateWorkspace("  ", []string{dir, dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Dirs) != 1 {
		t.Fatalf("同一目录不应重复: %v", ws.Dirs)
	}
	if ws.Name != filepath.Base(dir) {
		t.Fatalf("没起名应回落目录名，实际 %q", ws.Name)
	}
	if _, err := m.AddWorkspaceDir(ws.ID, dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := m.GetWorkspace(ws.ID); len(after.Dirs) != 1 {
		t.Fatalf("重复加同一目录应无变化: %v", after.Dirs)
	}
	if err := m.RenameWorkspace(ws.ID, "  "); err != nil {
		t.Fatal(err)
	}
	if after, _ := m.GetWorkspace(ws.ID); after.Name != filepath.Base(dir) {
		t.Fatalf("空名字应回落目录名，实际 %q", after.Name)
	}
	if err := m.RenameWorkspace("nope", "x"); err == nil {
		t.Fatal("不存在的工作区改名应报错")
	}
}

// 置顶与换主目录：都是工作区这一层的展示/组织属性，不动任何会话的 cwd（D1 的锚）。
func TestWorkspacePinAndPrimaryDir(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	m := newTestManager(t, nil, nil, nil)
	ws, err := m.CreateWorkspace("三目录", []string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	th := mustThreadIn(t, m, ws) // 会话锚在 a（当时的主目录）

	if err := m.SetPinned(ws.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetWorkspace(ws.ID)
	if !got.Pinned {
		t.Fatal("置顶未生效")
	}

	if _, err := m.SetPrimaryDir(ws.ID, b); err != nil {
		t.Fatalf("设主目录失败: %v", err)
	}
	got, _ = m.GetWorkspace(ws.ID)
	want := []string{b, a, c}
	for i := range want {
		if got.Dirs[i] != want[i] {
			t.Fatalf("主目录应挪到首位 %v，实际 %v", want, got.Dirs)
		}
	}
	if th2, ok := m.Get(th.ID); !ok || th2.Cwd != a {
		t.Fatalf("换主目录不该动既有会话的 cwd: %+v", th2)
	}
	// 重复设同一个主目录是无操作（不该抖动顺序）
	if _, err := m.SetPrimaryDir(ws.ID, b); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.GetWorkspace(ws.ID); again.Dirs[0] != b || again.Dirs[1] != a {
		t.Fatalf("重复设置不该改变顺序: %v", again.Dirs)
	}
	// 不在工作区里的目录 → 报错并给下一步
	other := t.TempDir()
	if _, err := m.SetPrimaryDir(ws.ID, other); err == nil {
		t.Fatal("不在工作区里的目录应报错")
	} else if !strings.Contains(err.Error(), "添加目录") {
		t.Fatalf("错误应给下一步，实际: %v", err)
	}
	if err := m.SetPinned("nope", true); err == nil {
		t.Fatal("不存在的工作区置顶应报错")
	}
}

// 列表接口标注已经不存在的目录：用户在别处删了源文件夹，UI 要能明说而不是显示死路径。
func TestWorkspaceListMarksMissingDirs(t *testing.T) {
	live := t.TempDir()
	gone := filepath.Join(t.TempDir(), "deleted")
	m := newTestManager(t, nil, nil, nil)
	ws, err := m.CreateWorkspace("有缺失", []string{live})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddWorkspaceDir(ws.ID, gone); err != nil {
		t.Fatal(err) // 加目录时会自动建出来
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	list := m.WorkspaceList()
	if len(list) != 1 {
		t.Fatalf("应列出 1 个工作区，实际 %d", len(list))
	}
	item := list[0]
	if len(item.MissingDirs) != 1 || item.MissingDirs[0] != gone {
		t.Fatalf("缺失目录应标注 %s，实际 %v", gone, item.MissingDirs)
	}
	if len(item.Dirs) != 2 {
		t.Fatalf("缺失不该从列表里掉目录: %v", item.Dirs)
	}
	// 缺失标注是读时算的，不写进投影文件
	data, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "missingDirs") {
		t.Fatal("missingDirs 不该落盘")
	}
}

// 会话必须落在工作区的目录里：既不在列表也不是可解析路径时报错并给下一步。
func TestSessionCwdMustBelongToWorkspace(t *testing.T) {
	inside, outside := t.TempDir(), t.TempDir()
	m := newTestManager(t, nil, nil, nil)
	ws := mustWorkspace(t, m, inside)
	if _, err := m.CreateSession("fake", ws.ID, outside, ""); err == nil {
		t.Fatal("目录不在工作区里应报错")
	} else if got := err.Error(); got == "" {
		t.Fatal("错误信息不应为空")
	}
	if _, err := m.CreateSession("fake", "nope", "", ""); err == nil {
		t.Fatal("不存在的工作区应报错")
	}
}

// 投影重载：工作区与会话一起落盘、一起读回。
func TestWorkspacesPersistWithThreads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	cfg := testConfig(t)
	m := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	ws := mustWorkspace(t, m, dir)
	if _, err := m.CreateSession("fake", ws.ID, "", "记一笔"); err != nil {
		t.Fatal(err)
	}
	m.Close()

	m2 := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	defer m2.Close()
	list := m2.ListWorkspaces()
	if len(list) != 1 || list[0].ID != ws.ID || len(list[0].Dirs) != 1 {
		t.Fatalf("工作区应原样读回: %+v", list)
	}
	threads := m2.List()
	if len(threads) != 1 || threads[0].WorkspaceID != ws.ID || threads[0].Title != "记一笔" {
		t.Fatalf("会话应带工作区归属读回: %+v", threads)
	}
}

// 标题从首条消息派生：第一个非空行、去 markdown 前缀、折叠空白、截 24 个 rune；
// 没有可读文本（只发图）就留空，等后面某一轮再定。
func TestTitleFromMessage(t *testing.T) {
	cases := []struct{ in, want string }{
		{"帮我看下目录", "帮我看下目录"},
		{"## 修一下登录跳转\n后面还有一堆细节", "修一下登录跳转"},
		{"\n\n  > - 第三条才是要点\n", "第三条才是要点"},
		{"   \n\t\n", ""},
		{"", ""},
		{"第一个    行   空白要折叠", "第一个 行 空白要折叠"},
		{"# ", ""},
	}
	for _, c := range cases {
		if got := titleFromMessage(c.in); got != c.want {
			t.Fatalf("titleFromMessage(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("很", 40)
	got := titleFromMessage(long)
	if n := len([]rune(got)); n != 25 { // 24 个字符 + 省略号
		t.Fatalf("超长标题应截到 24 个 rune（带省略号），实际 %d: %q", n, got)
	}
}

// codex 与 claude 风格的真实取值集合：词汇完全不同，映射表必须两边都讲得通。
var codexModes = []SelectValue{
	{"read-only", "Read Only"},
	{"workspace-write", "Workspace Write"},
	{"agent", "Auto review"},
	{"agent-full-access", "Full access"},
}
var claudeModes = []SelectValue{
	{"default", "Default"},
	{"acceptEdits", "Accept edits"},
	{"plan", "Plan Mode"},
}

func TestPickApprovalValue(t *testing.T) {
	cases := []struct {
		name      string
		policy    string
		values    []SelectValue
		want      string
		wantKind  string
		wantExact bool
	}{
		{"有 default 就是精确档", WorkspacePolicyAsk, []SelectValue{{"default", "默认"}, {"auto", "自动"}}, "default", "exact", true},
		{"ask 直接命中", WorkspacePolicyAsk, []SelectValue{{"ask", "每次询问"}}, "ask", "exact", true},
		{"codex 的 ask 退到 workspace-write", WorkspacePolicyAsk, codexModes, "workspace-write", "limited", false},
		{"codex 的 readonly 命中 read-only", WorkspacePolicyReadOnly, codexModes, "read-only", "readonly", true},
		{"codex 的 auto 命中 full-access", WorkspacePolicyAuto, codexModes, "agent-full-access", "exact", true},
		{"claude 的 ask 命中 default", WorkspacePolicyAsk, claudeModes, "default", "exact", true},
		{"claude 的 auto 没有全自动，退到 acceptEdits", WorkspacePolicyAuto, claudeModes, "acceptEdits", "auto-limited", false},
		{"claude 的 readonly 命中 plan", WorkspacePolicyReadOnly, claudeModes, "plan", "readonly", true},
		{"只有只读与全自动时，ask 宁可不下发也不碰这两档", WorkspacePolicyAsk,
			[]SelectValue{{"read-only", "只读"}, {"agent-full-access", "全自动"}}, "", "", false},
		{"没有可用取值", WorkspacePolicyAuto, []SelectValue{{"model", "模型"}}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pickApproval(c.policy, c.values)
			if c.want == "" {
				if ok {
					t.Fatalf("不该命中任何取值，实际 %+v", got)
				}
				return
			}
			if !ok || got.Value != c.want || got.Kind != c.wantKind || got.Exact != c.wantExact {
				t.Fatalf("pickApproval(%s) = %+v，期望 %s（kind=%s exact=%v）",
					c.policy, got, c.want, c.wantKind, c.wantExact)
			}
			note := policyFallbackNote(c.policy, got)
			if c.wantExact != (note == "") {
				t.Fatalf("精确档不该有退让说明，退让档必须有：%+v note=%q", got, note)
			}
			if !c.wantExact && !strings.Contains(note, got.Value) {
				t.Fatalf("退让说明要点名实际落点取值：%q", note)
			}
		})
	}
}

func TestJoinSelectValuesTruncates(t *testing.T) {
	vals := make([]SelectValue, 0, 9)
	for i := 0; i < 9; i++ {
		vals = append(vals, SelectValue{Value: fmt.Sprintf("v%d", i), Name: fmt.Sprintf("档 %d", i)})
	}
	got := joinSelectValues(vals, 6)
	if !strings.Contains(got, "档 0(v0)") || !strings.Contains(got, "共 9 项") {
		t.Fatalf("拼接不符: %q", got)
	}
	if strings.Contains(got, "v8") {
		t.Fatalf("超出上限的取值不该出现: %q", got)
	}
}

func TestWorkspacePolicyDefaultsAndValidation(t *testing.T) {
	if got := NormalizeWorkspacePolicy(""); got != WorkspacePolicyAsk {
		t.Fatalf("空档位应归一为 ask，实际 %q", got)
	}
	if !validWorkspacePolicy("readonly") || validWorkspacePolicy("yolo") {
		t.Fatal("档位校验不符")
	}
}
