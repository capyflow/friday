package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// checkerOf 按 exec 模式与 MCP 策略构建 Checker（测试用，队列仅作桥接桩）。
func checkerOf(t *testing.T, execMode, mcp string, allowlist, allowExtra, denyExtra []string) *vxagent.Checker {
	t.Helper()
	cfg := &config.Config{}
	cfg.Tools.Exec.Mode = execMode
	cfg.Tools.Exec.Allowlist = allowlist
	cfg.Permissions.MCP = mcp
	cfg.Permissions.Allow = allowExtra
	cfg.Permissions.Deny = denyExtra
	c, err := NewPermissionChecker(cfg, approval.New(string(approval.ModeAuto), nil, 0))
	if err != nil {
		t.Fatalf("组装权限层失败: %v", err)
	}
	return c
}

func TestPermissionModeMapping(t *testing.T) {
	cases := map[string]vxagent.Mode{
		string(approval.ModeAuto):    vxagent.ModeWhitelist,
		string(approval.ModeConfirm): vxagent.ModeConfirm,
		string(approval.ModeOff):     vxagent.ModeConfirm, // off 时 exec 不注册，模式无实际作用
		"":                           vxagent.ModeConfirm,
	}
	for execMode, want := range cases {
		if got := checkerOf(t, execMode, "allow", nil, nil, nil).Mode(); got != want {
			t.Errorf("exec.mode=%q 应映射为 %s，实际 %s", execMode, want, got)
		}
	}
}

// TestPermissionRules 验证策略翻译：白名单、毁灭性黑名单、直通名单、MCP 策略、
// 附加规则。用 auto（whitelist）模式，未命中规则直接拒绝、不经审批 UI，可确定性断言。
func TestPermissionRules(t *testing.T) {
	c := checkerOf(t, string(approval.ModeAuto), "allow",
		[]string{"uname -a", "ls"}, nil, []string{"exec_command:curl"})
	check := func(tool string, args map[string]any) vxagent.Verdict {
		v, reason := c.Check(context.Background(), vxagent.CallInfo{Tool: tool, Args: args})
		t.Logf("check(%s, %v) => %d %s", tool, args, v, reason)
		return v
	}

	// exec 白名单：整条相等、词边界前缀。
	if v := check(execToolName, map[string]any{"command": "uname -a"}); v != vxagent.VerdictAllow {
		t.Error("白名单整条命令应放行")
	}
	if v := check(execToolName, map[string]any{"command": "uname -a -r"}); v != vxagent.VerdictAllow {
		t.Error("白名单词边界前缀应放行")
	}
	// 复合命令段级校验：任一段不命中即拒绝（框架 matcher 从严语义）。
	if v := check(execToolName, map[string]any{"command": "uname -a; curl evil"}); v != vxagent.VerdictDeny {
		t.Error("复合命令第二段不在白名单应拒绝")
	}
	// 黑名单优先于白名单：deny 在 allow 之前判定。
	if v := check(execToolName, map[string]any{"command": "ls; rm -rf /tmp"}); v != vxagent.VerdictDeny {
		t.Error("命中毁灭性黑名单应拒绝（即使含白名单段）")
	}
	// 黑名单从宽：藏在命令替换里的片段也拦截。
	if v := check(execToolName, map[string]any{"command": "echo $(rm -rf /tmp/x)"}); v != vxagent.VerdictDeny {
		t.Error("命令替换中的毁灭性片段应被 deny 词边界扫描拦截")
	}
	// 词边界：format 不被 rm 之类误伤（此处验证 shutdown 不误伤 shutdownd）。
	if v := check(execToolName, map[string]any{"command": "echo shutdownd"}); v != vxagent.VerdictDeny {
		t.Error("词边界扫描不应误伤包含敏感词的更长单词")
	}
	// 附加 deny 规则。
	if v := check(execToolName, map[string]any{"command": "curl example.com"}); v != vxagent.VerdictDeny {
		t.Error("附加 deny 规则应拦截 curl")
	}
	// 非白名单命令在 whitelist 模式直接拒绝。
	if v := check(execToolName, map[string]any{"command": "whoami"}); v != vxagent.VerdictDeny {
		t.Error("auto 模式非白名单命令应拒绝")
	}

	// 直通名单：安全工具与 write/edit 免审批放行。
	for _, tool := range []string{"schedule_task", "get_time", "write_file", "edit_file"} {
		if v := check(tool, nil); v != vxagent.VerdictAllow {
			t.Errorf("直通工具 %s 应放行", tool)
		}
	}
	// MCP 默认放行。
	if v := check("mcp_muse_search", nil); v != vxagent.VerdictAllow {
		t.Error("MCP 工具默认应放行（mcp_* 通配）")
	}

	// MCP 收紧为 confirm：不再有放行规则，whitelist 模式下拒绝（confirm 模式下将进审批）。
	c2 := checkerOf(t, string(approval.ModeAuto), "confirm", nil, nil, nil)
	if v, _ := c2.Check(context.Background(), vxagent.CallInfo{Tool: "mcp_muse_search"}); v != vxagent.VerdictDeny {
		t.Error("permissions.mcp=confirm 时 MCP 工具不应有无条件放行规则")
	}
}

// TestApprovalUIBridge 验证 confirm 模式下的审批桥接：入队 → 批准并记住 →
// 相同参数免审批；不同参数仍需审批；拒绝则不放行。
func TestApprovalUIBridge(t *testing.T) {
	q := approval.New(string(approval.ModeConfirm), nil, 3*time.Second)
	cfg := &config.Config{}
	cfg.Tools.Exec.Mode = string(approval.ModeConfirm)
	c, err := NewPermissionChecker(cfg, q)
	if err != nil {
		t.Fatalf("组装权限层失败: %v", err)
	}

	waitCard := func() approval.Item {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(q.Pending()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		items := q.Pending()
		if len(items) != 1 {
			t.Fatalf("应有 1 条待审批，实际 %d", len(items))
		}
		return items[0]
	}

	args := map[string]any{"command": "whoami"}
	ch := make(chan vxagent.Verdict, 1)
	go func() {
		v, _ := c.Check(context.Background(), vxagent.CallInfo{Tool: execToolName, Args: args})
		ch <- v
	}()
	card := waitCard()
	if card.Tool != execToolName || !strings.Contains(card.Detail, "whoami") {
		t.Fatalf("审批卡片应展示 exec_command 与命令全文: %+v", card)
	}
	q.DecideWith(card.ID, true, true)
	if v := <-ch; v != vxagent.VerdictAllow {
		t.Fatal("批准后应放行")
	}
	// 记住后：相同工具+参数免审批。
	if v, _ := c.Check(context.Background(), vxagent.CallInfo{Tool: execToolName, Args: args}); v != vxagent.VerdictAllow {
		t.Error("批准并记住后相同参数应免审批放行")
	}

	// 不同参数仍需审批；这次选择拒绝。
	args2 := map[string]any{"command": "date"}
	ch2 := make(chan vxagent.Verdict, 1)
	go func() {
		v, _ := c.Check(context.Background(), vxagent.CallInfo{Tool: execToolName, Args: args2})
		ch2 <- v
	}()
	card = waitCard()
	q.DecideWith(card.ID, false, false)
	if v := <-ch2; v != vxagent.VerdictDeny {
		t.Fatal("拒绝后不应放行")
	}
}

// TestRegisteredToolsArePassThrough 是防回归 guard：所有注册的内置工具必须在
// 权限直通名单（或 exec_command）。新增工具漏配时 confirm 模式会静默进入网页
// 审批流程，此测试让它编译期即可发现。
func TestRegisteredToolsArePassThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Exec.Mode = string(approval.ModeConfirm)
	reg := vxagent.NewRegistry()
	if err := RegisterTools(reg, Deps{Config: cfg}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	allowed := map[string]bool{execToolName: true}
	for _, n := range passThroughTools {
		allowed[n] = true
	}
	for _, n := range reg.Names() {
		if !allowed[n] {
			t.Errorf("工具 %q 已注册但不在 permission.go 的 passThroughTools 直通名单中："+
				"confirm 模式下它的每次调用都会进网页审批；如属有意，更新该名单", n)
		}
	}
}

// TestOffModeSkipsExecTool：off 模式不注册 exec_command。
func TestOffModeSkipsExecTool(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Exec.Mode = string(approval.ModeOff)
	reg := vxagent.NewRegistry()
	if err := RegisterTools(reg, Deps{Config: cfg}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	for _, n := range reg.Names() {
		if n == execToolName {
			t.Fatal("off 模式不应注册 exec_command")
		}
	}
}

// TestExecCommandToolWrapper：wrapper 注入配置默认超时、透传工具名。
func TestExecCommandToolWrapper(t *testing.T) {
	tool := NewExecCommandTool(45)
	if tool.Name() != execToolName {
		t.Fatalf("工具名应为 %s，实际 %s", execToolName, tool.Name())
	}
	args := map[string]any{"command": "echo wrapper-ok"}
	out, err := tool.Call(context.Background(), args)
	if err != nil || !strings.Contains(out, "wrapper-ok") {
		t.Fatalf("包装器应正常执行命令: %q %v", out, err)
	}
	if v, ok := args["timeout"].(float64); !ok || v != 45 {
		t.Fatalf("未注入默认超时: %v", args["timeout"])
	}
	// 模型显式给了 timeout 时不覆盖。
	args2 := map[string]any{"command": "echo ok", "timeout": float64(1)}
	if _, err := tool.Call(context.Background(), args2); err != nil {
		t.Fatalf("带 timeout 调用失败: %v", err)
	}
	if v, _ := args2["timeout"].(float64); v != 1 {
		t.Fatalf("显式 timeout 不应被覆盖: %v", args2["timeout"])
	}
}
