package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
)

// waitCheck 轮询直到这次运行进入 want 里的状态（15 秒上限：正常用例最长只跑 2 秒超时）。
func waitCheck(t *testing.T, m *Manager, threadID, runID string, want ...string) CheckRun {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last CheckRun
	for time.Now().Before(deadline) {
		r, err := m.Check(threadID, runID)
		if err != nil {
			t.Fatalf("读取验证运行失败: %v", err)
		}
		last = r
		for _, w := range want {
			if r.State == w {
				return r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待状态 %v 超时，最后一条记录 %+v", want, last)
	return CheckRun{}
}

// waitCard 等审批卡出现（StartCheck 与入队之间是异步的）。
func waitCard(t *testing.T, q *approval.Queue) approval.Item {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if items := q.Pending(); len(items) > 0 {
			return items[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("审批卡没出现")
	return approval.Item{}
}

// waitPendingLen 等审批列表到达期望长度：撤卡（Cancel）由等待方 goroutine 出队，
// StopCheck 返回时可能还差一次调度。
func waitPendingLen(t *testing.T, q *approval.Queue, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(q.Pending()) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("审批列表长度应为 %d，实际 %+v", want, q.Pending())
}

func TestCheckRunApprovesAndPasses(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "echo 验证通过", 0)
	if err != nil {
		t.Fatalf("提交验证命令失败: %v", err)
	}
	if run.State != CheckAwaiting {
		t.Fatalf("刚提交应为待批: %+v", run)
	}
	card := waitCard(t, q)
	if card.Tool != "验证命令" {
		t.Fatalf("审批卡工具名不符: %+v", card)
	}
	if !strings.Contains(card.Detail, run.Cwd) || !strings.Contains(card.Detail, "echo 验证通过") {
		t.Fatalf("审批卡没写清目录与命令: %+v", card)
	}
	if !q.Decide(card.ID, true) {
		t.Fatalf("批准失败")
	}
	got := waitCheck(t, m, th.ID, run.ID, CheckPassed)
	if got.ExitCode != 0 || !strings.Contains(got.Output, "验证通过") {
		t.Fatalf("通过态记录不符: %+v", got)
	}
	if got.FinishedAt.IsZero() || got.TimeoutSec != int(checkDefaultTimeout/time.Second) {
		t.Fatalf("结束时刻/默认超时未记录: %+v", got)
	}
	if len(m.Checks(th.ID)) != 1 {
		t.Fatalf("记录数不符: %+v", m.Checks(th.ID))
	}
}

func TestCheckRunReportsFailureExitCode(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "echo 用例炸了 >&2; exit 3", 0)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	q.Decide(waitCard(t, q).ID, true)
	got := waitCheck(t, m, th.ID, run.ID, CheckFailed)
	if got.ExitCode != 3 || !strings.Contains(got.Note, "退出码 3") {
		t.Fatalf("失败态应带退出码: %+v", got)
	}
	if !strings.Contains(got.Output, "用例炸了") {
		t.Fatalf("失败输出要能看到（前端据此高亮失败用例）: %q", got.Output)
	}
}

func TestCheckDeniedSkipsExecution(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "touch ran.txt", 0)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if !q.Decide(waitCard(t, q).ID, false) {
		t.Fatalf("拒绝失败")
	}
	got := waitCheck(t, m, th.ID, run.ID, CheckDenied)
	if got.ExitCode != -1 {
		t.Fatalf("未执行不该有退出码: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(th.Cwd, "ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("审批被拒却执行了命令: %v", err)
	}
	waitPendingLen(t, q, 0)
}

func TestCheckRejectsDangerousCommandBeforeApproval(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	if _, err := m.StartCheck(th.ID, "rm -rf /tmp/whatever", 0); err == nil {
		t.Fatalf("毁灭性命令应被拒绝")
	} else if !strings.Contains(err.Error(), "rm -rf") {
		t.Fatalf("错误应点明命中的片段: %v", err)
	}
	if len(q.Pending()) != 0 || len(m.Checks(th.ID)) != 0 {
		t.Fatalf("被本地黑名单拦下时不该入队或留记录")
	}
}

func TestCheckStopsWhenTimeout(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "sleep 30", 2*time.Second)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	q.Decide(waitCard(t, q).ID, true)
	got := waitCheck(t, m, th.ID, run.ID, CheckTimeout)
	if got.TimeoutSec != 2 || !strings.Contains(got.Note, "超过 2 秒") {
		t.Fatalf("超时说明不符: %+v", got)
	}
}

func TestCheckStopWhileRunning(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "sleep 30", time.Minute)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	q.Decide(waitCard(t, q).ID, true)
	waitCheck(t, m, th.ID, run.ID, CheckRunning)
	if err := m.StopCheck(th.ID, run.ID); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	got := waitCheck(t, m, th.ID, run.ID, CheckKilled)
	if !strings.Contains(got.Note, "已手动停止") {
		t.Fatalf("停止说明不符: %+v", got)
	}
	// 已结束的运行不能再停。
	if err := m.StopCheck(th.ID, run.ID); err == nil {
		t.Fatalf("重复停止应报错")
	}
}

func TestCheckStopWhileAwaitingKeepsApprovalClean(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, "echo 不该跑", 0)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitCard(t, q) // 确认卡已经挂上，再点停止
	if err := m.StopCheck(th.ID, run.ID); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	got := waitCheck(t, m, th.ID, run.ID, CheckKilled)
	if !strings.Contains(got.Note, "进程未启动") {
		t.Fatalf("待批时停止应说明进程未启动: %+v", got)
	}
	waitPendingLen(t, q, 0)
}

func TestCheckSingleFlightPerThread(t *testing.T) {
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := mustThread(t, m)

	if _, err := m.StartCheck(th.ID, "sleep 5", 0); err != nil {
		t.Fatalf("第一条提交失败: %v", err)
	}
	if _, err := m.StartCheck(th.ID, "echo 第二条", 0); err == nil {
		t.Fatalf("同会话并发提交应被拒绝")
	} else if !strings.Contains(err.Error(), "已有验证命令") {
		t.Fatalf("错误文案不符: %v", err)
	}
	if len(q.Pending()) != 1 {
		t.Fatalf("被拒的第二条不该再入一张卡: %+v", q.Pending())
	}
}

func TestCheckTrimsLongOutput(t *testing.T) {
	m := newTestManager(t, nil, nil, nil) // 无审批队列：直接执行（仅测试环境）
	th := mustThread(t, m)

	run, err := m.StartCheck(th.ID, `awk 'BEGIN{for(i=0;i<20000;i++)print "line-"i}'`, 0)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	got := waitCheck(t, m, th.ID, run.ID, CheckPassed)
	if !got.Truncated {
		t.Fatalf("20 万行输出应标截断: %+v", got)
	}
	lines := strings.Split(got.Output, "\n")
	if len(lines) > 400 {
		t.Fatalf("输出尾部没限行数: %d 行", len(lines))
	}
	if !strings.Contains(got.Output, "line-19999") {
		t.Fatalf("应保留最后几行: %s", lines[len(lines)-1])
	}
}

func TestCheckHistoryKeepsRecentRuns(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	th := mustThread(t, m)

	for i := 0; i < checkHistoryKeep+3; i++ {
		run, err := m.StartCheck(th.ID, "echo ok", 0)
		if err != nil {
			t.Fatalf("第 %d 条提交失败: %v", i, err)
		}
		waitCheck(t, m, th.ID, run.ID, CheckPassed)
	}
	if got := m.Checks(th.ID); len(got) != checkHistoryKeep {
		t.Fatalf("记录应裁剪到 %d 条，实际 %d", checkHistoryKeep, len(got))
	}
}

func TestCheckNeedsThreadCwd(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	if _, err := m.StartCheck("不存在", "echo hi", 0); err == nil {
		t.Fatalf("会话不存在应报错")
	}
	if _, err := m.CheckSuggestions("不存在"); err == nil {
		t.Fatalf("读取建议也应报错")
	}
}

func TestSuggestChecksFromProjectMarkers(t *testing.T) {
	t.Run("pnpm 仓库按 packageManager 给命令", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, root, "package.json", `{"packageManager":"pnpm@9.1.0","scripts":{"test":"vitest run","lint":"eslint .","dev":"vite"}}`)
		mustWriteFile(t, root, "pnpm-lock.yaml", "")
		got := commandsOf(suggestChecks(root))
		want := []string{"pnpm test", "pnpm run lint"} // dev 不是检查命令，不该出现
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("建议不符: %+v", got)
		}
	})
	t.Run("npm 仓库默认 npm", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, root, "package.json", `{"scripts":{"test":"vitest run"}}`)
		got := commandsOf(suggestChecks(root))
		if len(got) != 1 || got[0] != "npm test" {
			t.Fatalf("建议不符: %+v", got)
		}
	})
	t.Run("Go 与 Python 并存都给", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, root, "go.mod", "module example\n")
		mustWriteFile(t, root, "pytest.ini", "[pytest]\n")
		got := commandsOf(suggestChecks(root))
		if len(got) != 2 || got[0] != "go test ./..." || got[1] != "pytest" {
			t.Fatalf("建议不符: %+v", got)
		}
	})
	t.Run("只有 Makefile 时兜底", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, root, "Makefile", "build:\n\t@true\n\ntest:\n\t@true\n")
		got := commandsOf(suggestChecks(root))
		if len(got) != 1 || got[0] != "make test" {
			t.Fatalf("建议不符: %+v", got)
		}
	})
	t.Run("空目录没有建议", func(t *testing.T) {
		if got := suggestChecks(t.TempDir()); len(got) != 0 {
			t.Fatalf("不该有建议: %+v", got)
		}
	})
}

func TestCleanCheckOutputStripsAnsiAndCarriageReturns(t *testing.T) {
	raw := "\x1b[32m通过\x1b[0m\r\n仍在跑\x1b[K\r最后一行\r\n\r\n"
	got := cleanCheckOutput(raw)
	if got != "通过\n仍在跑\n最后一行" {
		t.Fatalf("清洗结果不符: %q", got)
	}
}

func commandsOf(items []CheckSuggestion) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Command)
	}
	return out
}

func mustWriteFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatalf("写 %s 失败: %v", name, err)
	}
}
