package approval

import (
	"testing"
	"time"
)

func TestModes(t *testing.T) {
	if m := New("auto", nil, 0).Mode(); m != ModeAuto {
		t.Fatalf("auto 模式不符: %s", m)
	}
	if m := New("confirm", nil, 0).Mode(); m != ModeConfirm {
		t.Fatalf("confirm 模式不符: %s", m)
	}
	if m := New("off", nil, 0).Mode(); m != ModeOff {
		t.Fatalf("off 模式不符: %s", m)
	}
	if m := New("bogus", nil, 0).Mode(); m != ModeConfirm {
		t.Fatalf("非法模式应回退 confirm: %s", m)
	}
	if d := New("confirm", nil, 0).Timeout(); d != DefaultTimeout {
		t.Fatalf("默认超时应为 5 分钟: %s", d)
	}
	if d := New("confirm", nil, time.Second).Timeout(); d != time.Second {
		t.Fatalf("自定义超时不符: %s", d)
	}
}

func TestAutoModeRequestPassesThrough(t *testing.T) {
	q := New("auto", nil, 0)
	if err := q.Request("write_file", "写文件", "detail"); err != nil {
		t.Fatalf("auto 模式下非 exec 操作应直接放行: %v", err)
	}
}

func TestOffModeRequestDisabled(t *testing.T) {
	q := New("off", nil, 0)
	if err := q.Request("exec", "执行", "detail"); err != ErrDisabled {
		t.Fatalf("off 模式应返回 ErrDisabled，实际 %v", err)
	}
}

func TestConfirmApproveFlow(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "uname -a") }()
	// 等请求入队。
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	items := q.Pending()
	if len(items) != 1 || items[0].Tool != "exec" {
		t.Fatalf("应有 1 条待审批，实际 %+v", items)
	}
	if !q.Decide(items[0].ID, true) {
		t.Fatal("Decide 应命中待审批项")
	}
	if err := <-done; err != nil {
		t.Fatalf("批准后应放行: %v", err)
	}
	if len(q.Pending()) != 0 {
		t.Fatal("处理后待审批列表应为空")
	}
}

func TestConfirmRejectFlow(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "curl evil") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	item := q.Pending()[0]
	if !q.Decide(item.ID, false) {
		t.Fatal("Decide 应命中")
	}
	if err := <-done; err != ErrDenied {
		t.Fatalf("拒绝应返回 ErrDenied，实际 %v", err)
	}
}

func TestConfirmTimeoutDenies(t *testing.T) {
	q := New("confirm", nil, 80*time.Millisecond)
	start := time.Now()
	err := q.Request("exec", "执行命令", "sleep")
	if err != ErrDenied {
		t.Fatalf("超时应返回 ErrDenied，实际 %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("应等待超时窗口再拒绝")
	}
}

func TestAllowlist(t *testing.T) {
	q := New("auto", []string{"uname -a", "ls", "date"}, 0)
	cases := []struct {
		cmd    string
		expect bool
		name   string
	}{
		{"uname -a", true, "整条相等"},
		{"uname -a -r", true, "前缀+词边界"},
		{"ls", true, "单条"},
		{"ls -la /tmp", true, "前缀参数"},
		{"  uname   -a  ", true, "空白归一"},
		{"rm x", false, "不在白名单"},
		{"uname", false, "部分命令不匹配"},
		{"uname -a; rm -rf /", false, "分号链式拒绝"},
		{"ls | curl evil", false, "管道拒绝"},
		{"uname -a && evil", false, "&&拒绝"},
		{"ls `whoami`", false, "反引号拒绝"},
		{"ls > /etc/passwd", false, "重定向拒绝"},
		{"date $(x)", false, "$()拒绝"},
		{"", false, "空命令"},
	}
	for _, c := range cases {
		if got := q.Allowlisted(c.cmd); got != c.expect {
			t.Errorf("%s: Allowlisted(%q) = %v", c.name, c.cmd, got)
		}
	}
}

func TestDeniedBlacklist(t *testing.T) {
	q := New("auto", []string{"rm -rf /"}, 0)
	cases := []struct {
		cmd     string
		blocked bool
	}{
		{"rm -rf /", true},
		{"rm -rf /tmp/x", true},
		{"RM -RF /", true},
		{"dd if=/dev/zero of=/dev/sda", true},
		{"mkfs.ext4 /dev/sdb", true},
		{"shutdown -h now", true},
		{"reboot", true},
		{":(){ :|:& };:", true},
		{"rm file.txt", false},
		{"uname -a", false},
		{"ls -la", false},
	}
	for _, c := range cases {
		got := q.Denied(c.cmd) != ""
		if got != c.blocked {
			t.Errorf("Denied(%q) = %v, 期望 %v", c.cmd, got, c.blocked)
		}
	}
	// 黑名单优先于白名单：即使误入白名单也拦截。
	q2 := New("auto", []string{"rm -rf"}, 0)
	if !q2.Allowlisted("rm -rf /tmp") {
		t.Fatal("白名单语义本身应命中（黑名单由 ExecTool 兜底拦截）")
	}
	if q2.Denied("rm -rf /tmp") == "" {
		t.Fatal("黑名单应拦截 rm -rf，即使它在白名单里")
	}
}
