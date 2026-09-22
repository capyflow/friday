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

func TestRequestRememberDecideWith(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	type result struct {
		remember bool
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "whoami")
		ch <- result{remember, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !q.DecideWith(q.Pending()[0].ID, true, true) {
		t.Fatal("DecideWith 应命中待审批项")
	}
	r := <-ch
	if r.err != nil || !r.remember {
		t.Fatalf("批准并记住应放行且 remember=true: %+v", r)
	}

	// 不记住的批准：remember=false。
	ch2 := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "date")
		ch2 <- result{remember, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !q.DecideWith(q.Pending()[0].ID, true, false) {
		t.Fatal("DecideWith 应命中待审批项")
	}
	r = <-ch2
	if r.err != nil || r.remember {
		t.Fatalf("普通批准应 remember=false: %+v", r)
	}

	// 拒绝：ErrDenied。
	ch3 := make(chan result, 1)
	go func() {
		remember, err := q.RequestRemember("exec", "执行命令", "curl evil")
		ch3 <- result{remember, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.DecideWith(q.Pending()[0].ID, false, true)
	r = <-ch3
	if r.err != ErrDenied {
		t.Fatalf("拒绝应返回 ErrDenied，实际 %v", r.err)
	}
}

// Request 是 RequestRemember 的薄包装：批准返回 nil，拒绝返回 ErrDenied。
func TestRequestWrapsRequestRemember(t *testing.T) {
	q := New("confirm", nil, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- q.Request("exec", "执行命令", "uname -a") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.Decide(q.Pending()[0].ID, true)
	if err := <-done; err != nil {
		t.Fatalf("批准后应放行: %v", err)
	}
}

// DenyPatterns 返回黑名单副本，供权限层翻译成 exec_command deny 规则。
func TestDenyPatterns(t *testing.T) {
	pats := DenyPatterns()
	if len(pats) == 0 {
		t.Fatal("黑名单不应为空")
	}
	pats[0] = "tampered"
	if DenyPatterns()[0] == "tampered" {
		t.Fatal("DenyPatterns 应返回副本，避免调用方污染黑名单")
	}
	found := false
	for _, p := range DenyPatterns() {
		if p == "rm -rf" {
			found = true
		}
	}
	if !found {
		t.Fatal("黑名单应包含 rm -rf")
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
