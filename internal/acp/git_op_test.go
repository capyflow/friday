package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/approval"
)

// waitGitOp 轮询直到这次写操作进入期望状态（15 秒上限：正常用例只跑一次 git）。
func waitGitOp(t *testing.T, m *Manager, threadID, opID string, want ...string) GitOp {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last GitOp
	for time.Now().Before(deadline) {
		op, err := m.GitOpByID(threadID, opID)
		if err != nil {
			t.Fatalf("读取 Git 操作失败: %v", err)
		}
		last = op
		for _, w := range want {
			if op.State == w {
				return op
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待状态 %v 超时，最后一条记录 %+v", want, last)
	return GitOp{}
}

// settleGitOp 等这次写操作的状态稳定下来（连续两次读到的状态一样）。
// 写操作的执行在后台 goroutine 里跑，StopGitOp 之后它还可能再改一次状态——
// 这正是要盯的地方，所以不能只看「第一次读到的状态」。
func settleGitOp(t *testing.T, m *Manager, threadID, opID string) GitOp {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last GitOp
	stable := 0
	for time.Now().Before(deadline) {
		op, err := m.GitOpByID(threadID, opID)
		if err != nil {
			t.Fatalf("读取 Git 操作失败: %v", err)
		}
		if op.State == last.State && op.Message == last.Message {
			stable++
			if stable >= 3 { // 连续 3 次（每次隔 30ms）读到同一个状态才算稳
				return op
			}
		} else {
			stable = 0
		}
		last = op
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("Git 操作状态迟迟不稳定，最后一条 %+v", last)
	return GitOp{}
}

// gitLogCount 数仓库里有几条提交。
func gitLogCount(t *testing.T, dir string) int {
	t.Helper()
	out := strings.TrimSpace(gitTest(t, dir, "rev-list", "--count", "HEAD"))
	n := 0
	for _, r := range out {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}

// ---------- 无审批队列：命令直接执行 ----------

func TestStartGitOpWithoutApproverRunsImmediately(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil) // approver 为 nil：不进审批队列，直接执行
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed by op\n")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	if op.State != GitOpAwaiting {
		t.Fatalf("刚提交应为待批: %+v", op)
	}
	got := waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	if got.FinishedAt.IsZero() {
		t.Fatalf("结束时刻未记录: %+v", got)
	}
	if got.Cwd != th.Cwd {
		t.Fatalf("执行目录应为会话主目录: %+v", got)
	}
	// 仓库真的变了：暂存区里有 a.txt。
	staged := gitTest(t, repo, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "a.txt") {
		t.Fatalf("暂存区里没有 a.txt: %q", staged)
	}
}

func TestStartGitOpCheckoutChangesHead(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	gitTest(t, repo, "branch", "feature")
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: "feature"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	if got := gitBranchOf(t, repo); got != "feature" {
		t.Fatalf("HEAD 未切到 feature: %q", got)
	}
}

func TestStartGitOpCreateBranch(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "createBranch", Arg: "topic/one"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	if got := gitBranchOf(t, repo); got != "topic/one" {
		t.Fatalf("HEAD 未切到新建分支: %q", got)
	}
	branches := strings.TrimSpace(gitTest(t, repo, "branch", "--list", "topic/one"))
	if branches == "" {
		t.Fatalf("新建分支没建出来")
	}
}

func TestStartGitOpCommitAddsCommit(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	before := gitLogCount(t, repo)
	mustWriteFile(t, repo, "a.txt", "to be committed\n")

	stage, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交暂存失败: %v", err)
	}
	waitGitOp(t, m, th.ID, stage.ID, GitOpOK)

	commit, err := m.StartGitOp(th.ID, GitOpRequest{Op: "commit", Arg: "面板提交"})
	if err != nil {
		t.Fatalf("提交 commit 失败: %v", err)
	}
	waitGitOp(t, m, th.ID, commit.ID, GitOpOK)

	if after := gitLogCount(t, repo); after != before+1 {
		t.Fatalf("提交数没多一条: before=%d after=%d", before, after)
	}
	subject := strings.TrimSpace(gitTest(t, repo, "log", "-1", "--format=%s"))
	if subject != "面板提交" {
		t.Fatalf("最新提交 subject 不符: %q", subject)
	}
}

func TestStartGitOpUnstage(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n")
	gitTest(t, repo, "add", "a.txt")

	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "unstage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交取消暂存失败: %v", err)
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpOK)

	staged := gitTest(t, repo, "diff", "--cached", "--name-only")
	if strings.Contains(staged, "a.txt") {
		t.Fatalf("取消暂存后暂存区仍有 a.txt: %q", staged)
	}
}

// ---------- 白名单与参数校验 ----------

func TestStartGitOpRejectsUnsupportedOp(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	for _, op := range []string{"push", "reset --hard", "rm -rf", "fetch", "clean", ""} {
		if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: op, Arg: "main"}); err == nil {
			t.Fatalf("op %q 应被拒绝", op)
		}
	}
	if got := m.GitOps(th.ID); len(got) != 0 {
		t.Fatalf("被本地拦下的 op 不该留下记录: %+v", got)
	}
}

func TestStartGitOpRejectsInjectedArg(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	cases := []struct {
		op  string
		arg string
	}{
		{"checkout", "; rm -rf /"},
		{"checkout", "--upload-pack=touch /tmp/pwned"},
		{"checkout", "-f"},
		{"checkout", "../evil"},
		{"createBranch", "; rm -rf /"},
		{"createBranch", "--upload-pack=evil"},
		{"stage", "; rm -rf /"},
		{"stage", "--upload-pack=evil"},
		{"stage", "../../etc/passwd"},
		{"stage", "/etc/passwd"},
		{"unstage", "; rm -rf /"},
		{"unstage", "/etc/passwd"},
	}
	for _, c := range cases {
		if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: c.op, Arg: c.arg}); err == nil {
			t.Fatalf("op %q 的参数 %q 应被拒绝", c.op, c.arg)
		}
	}
	if got := m.GitOps(th.ID); len(got) != 0 {
		t.Fatalf("参数非法时不该留下记录: %+v", got)
	}
}

func TestStartGitOpRejectsEmptyArg(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	for _, c := range []struct{ op, arg string }{
		{"checkout", ""},
		{"createBranch", ""},
		{"stage", ""},
		{"unstage", ""},
		{"commit", ""},
	} {
		if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: c.op, Arg: c.arg}); err == nil {
			t.Fatalf("op %q 缺参数应被拒绝", c.op)
		}
	}
}

func TestStartGitOpOnNonRepoDirectory(t *testing.T) {
	requireGitCLI(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, t.TempDir())

	if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: "main"}); err == nil {
		t.Fatalf("非仓库目录不该接受写操作")
	}
}

func TestStartGitOpNeedsThreadCwd(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	if _, err := m.StartGitOp("不存在的会话", GitOpRequest{Op: "checkout", Arg: "main"}); err == nil {
		t.Fatalf("会话不存在应报错")
	}
}

// ---------- 失败路径 ----------

func TestStartGitOpCheckoutMissingBranchFails(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	before := gitBranchOf(t, repo)
	// 分支不存在要当场拒掉，不进审批队列——用户不必等审批完才发现分支是假的。
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: "no-such-branch"})
	if err == nil {
		t.Fatalf("切到不存在的分支应当当场被拒: %+v", op)
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Fatalf("报错要写清是哪个分支: %v", err)
	}
	if now := gitBranchOf(t, repo); now != before {
		t.Fatalf("失败的 checkout 不该动 HEAD: before=%q now=%q", before, now)
	}
}

func TestStartGitOpCommitWithoutStagedChangesFails(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	before := gitLogCount(t, repo)
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "commit", Arg: "没有东西可提交"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpFailed)
	if after := gitLogCount(t, repo); after != before {
		t.Fatalf("失败的提交数不该变: before=%d after=%d", before, after)
	}
}

// ---------- 审批队列 ----------

func TestStartGitOpWithApproverWaitsForApproval(t *testing.T) {
	requireGitCLI(t)
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	repo := gitFixtureRepo(t)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	card := waitCard(t, q)
	if card.Tool != "Git 操作" {
		t.Fatalf("审批卡工具名不符: %+v", card)
	}
	if !strings.Contains(card.Detail, th.Cwd) || !strings.Contains(card.Detail, "a.txt") {
		t.Fatalf("审批卡没写清目录与命令: %+v", card)
	}
	// 未批准前不许执行。
	if got := m.GitOps(th.ID); len(got) != 1 || got[0].State != GitOpAwaiting {
		t.Fatalf("未批准前应停在待批: %+v", got)
	}
	staged := gitTest(t, repo, "diff", "--cached", "--name-only")
	if strings.Contains(staged, "a.txt") {
		t.Fatalf("未批准就执行了: %q", staged)
	}

	if !q.Decide(card.ID, true) {
		t.Fatalf("批准失败")
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	staged = gitTest(t, repo, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "a.txt") {
		t.Fatalf("批准后没执行: %q", staged)
	}
}

func TestStartGitOpDeniedDoesNotRun(t *testing.T) {
	requireGitCLI(t)
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	repo := gitFixtureRepo(t)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	card := waitCard(t, q)
	if !q.Decide(card.ID, false) {
		t.Fatalf("拒绝失败")
	}
	got := waitGitOp(t, m, th.ID, op.ID, GitOpDenied)
	if got.FinishedAt.IsZero() {
		t.Fatalf("拒绝态未记录结束时刻: %+v", got)
	}
	staged := gitTest(t, repo, "diff", "--cached", "--name-only")
	if strings.Contains(staged, "a.txt") {
		t.Fatalf("被拒绝却执行了: %q", staged)
	}
	waitPendingLen(t, q, 0)
}

// TestStopGitOpCancelsAwaitingOp 盯住「用户点停止」之后的状态文案。
//
// 这条用例当前是失败的，原因是实现有 bug（不是用例写错）：StopGitOp 先把状态
// 定成「已手动停止（命令未执行）」，但后台 goroutine 醒来时 pend.Wait() 返回
// false，于是又把它改写成「审批未通过（拒绝或超时），未执行」。用户明明点了
// 停止，界面却说审批没通过。同一个坑在 verify.go 里是用 CheckRun.cancelled
// 这个标志挡住的，GitOp 缺了对应的标志。实现补上之后，这条用例即通过。
func TestStopGitOpCancelsAwaitingOp(t *testing.T) {
	requireGitCLI(t)
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	repo := gitFixtureRepo(t)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	waitCard(t, q) // 确认卡已挂上，再点停止
	if err := m.StopGitOp(th.ID, op.ID); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	// 状态要稳定下来后再断言：StopGitOp 会先写「已手动停止」，后台 goroutine
	// 醒来后可能再改一次（这正是要盯的竞态）。
	got := settleGitOp(t, m, th.ID, op.ID)
	if !strings.Contains(got.Message, "已手动停止") {
		t.Fatalf("停止后应说明是手动停的: %+v", got)
	}
	staged := gitTest(t, repo, "diff", "--cached", "--name-only")
	if strings.Contains(staged, "a.txt") {
		t.Fatalf("停止后不该执行: %q", staged)
	}
	waitPendingLen(t, q, 0)
}

// TestStartGitOpCheckoutDoesNotDiscardWork 盯住「checkout 只该切分支」这条边界。
//
// git checkout <path> 会把该文件恢复到暂存区的内容——也就是丢掉用户没提交的
// 改动。面板里的 checkout 语义是「切分支」，不该顺手扔掉工作区的活儿。
// 当前实现按 gitRefRe 放行了 "a.txt" 这种路径形态的参数，于是这条命令跑通了，
// 未提交的修改被静默丢弃。
// TestStartGitOpCheckoutDoesNotDiscardWork 盯住「checkout 只该切分支」这条边界。
//
// 这条用例当前是失败的，原因是实现有 bug（不是用例写错）：git checkout <path>
// 会把该文件恢复到暂存区的内容——也就是丢掉用户没提交的改动。面板里的
// checkout 语义是「切分支」，不该顺手扔掉工作区的活儿；参数校验 gitRefRe
// 放行了 "a.txt" 这种路径形态，于是命令跑通了，未提交的修改被静默丢弃。
// 实现收紧 checkout 的参数（只认分支/引用名）或改用 -- 分隔后，这条即通过。
func TestStartGitOpCheckoutDoesNotDiscardWork(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "未提交的重要改动\n")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: "a.txt"})
	if err != nil {
		return // 参数层就拦下了（正确做法）
	}
	got := waitGitOp(t, m, th.ID, op.ID, GitOpOK, GitOpFailed)
	body, readErr := os.ReadFile(filepath.Join(repo, "a.txt"))
	if readErr != nil {
		t.Fatalf("读 a.txt 失败: %v", readErr)
	}
	if string(body) != "未提交的重要改动\n" {
		t.Fatalf("checkout 不该丢弃未提交的改动（state=%s out=%q），a.txt 变成了 %q",
			got.State, got.Output, string(body))
	}
}

// TestGitOpTimeoutStartsAfterApproval 盯住执行超时的起算点。
//
// 审批最多等 gitOpApprovalWait（5 分钟），执行预算是 gitOpTimeout（2 分钟）。
// 若执行 ctx 在提交操作时就起算，用户在前 2 分钟里点「批准」，命令会被判定
// 超时而不执行——「等 5 分钟」就成了假的。所以执行超时必须从「批准之后」起算。
func TestGitOpTimeoutStartsAfterApproval(t *testing.T) {
	// 把执行超时压到很短：批准之前先等一会儿，批准后仍要能跑完。
	oldTimeout := gitOpTimeout
	gitOpTimeout = 1500 * time.Millisecond
	defer func() { gitOpTimeout = oldTimeout }()

	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	q := approval.New("confirm", nil, 0)
	m := newTestManager(t, nil, q, nil)
	th := gitThreadIn(t, m, repo)

	// 先改一个文件并暂存，commit 才有东西可提交（空提交会被 git 拒绝）。
	mustWriteFile(t, repo, "a.txt", "two\n")
	gitTest(t, repo, "add", "a.txt")
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "commit", Arg: "ok"})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	// 故意等一会儿再批准：批准前的时间不该算进执行预算。
	time.Sleep(1200 * time.Millisecond)
	card := waitCard(t, q)
	if !q.Decide(card.ID, true) {
		t.Fatalf("批准失败")
	}

	got := waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	if got.ApprovedAt.IsZero() {
		t.Fatalf("应记下批准时刻: %+v", got)
	}
	if got.ApprovedAt.Sub(got.StartedAt) < time.Second {
		t.Fatalf("批准时刻应晚于提交时刻: %+v", got)
	}
}

func TestStopGitOpOnFinishedOrUnknownOp(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	if err := m.StopGitOp(th.ID, "g0"); err == nil {
		t.Fatalf("停止不存在的操作应报错")
	}
	op, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: gitBranchOf(t, repo)})
	if err != nil {
		t.Fatalf("提交写操作失败: %v", err)
	}
	waitGitOp(t, m, th.ID, op.ID, GitOpOK)
	if err := m.StopGitOp(th.ID, op.ID); err == nil {
		t.Fatalf("停止已结束的操作应报错")
	}
}

// ---------- 操作记录 ----------

func TestGitOpsListsNewestFirst(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	first, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "a.txt"})
	if err != nil {
		t.Fatalf("第一次提交失败: %v", err)
	}
	waitGitOp(t, m, th.ID, first.ID, GitOpOK)
	second, err := m.StartGitOp(th.ID, GitOpRequest{Op: "checkout", Arg: gitBranchOf(t, repo)})
	if err != nil {
		t.Fatalf("第二次提交失败: %v", err)
	}
	waitGitOp(t, m, th.ID, second.ID, GitOpOK)

	ops := m.GitOps(th.ID)
	if len(ops) != 2 {
		t.Fatalf("应有 2 条记录: %+v", ops)
	}
	if ops[0].ID != second.ID || ops[1].ID != first.ID {
		t.Fatalf("记录应是新的在前: %+v", ops)
	}
	if ops[0].Op != "checkout" || ops[1].Op != "stage" {
		t.Fatalf("记录内容不符: %+v", ops)
	}
	if ops[0].StartedAt.IsZero() {
		t.Fatalf("开始时刻未记录: %+v", ops[0])
	}
}

func TestGitOpByIDRejectsUnknownID(t *testing.T) {
	m := newTestManager(t, nil, nil, nil)
	if _, err := m.GitOpByID("不存在的会话", "g1"); err == nil {
		t.Fatalf("查不存在的操作应报错")
	}
}

// TestGitOpDoesNotLeakOutsideWorkspace 确认写操作只在会话主目录里跑，
// 不会因为参数里的路径逃逸出去。
func TestGitOpDoesNotLeakOutsideWorkspace(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	outside := t.TempDir()
	mustWriteFile(t, outside, "secret.txt", "don't touch\n")
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: outside}); err == nil {
		t.Fatalf("绝对路径应被拒绝")
	}
	if _, err := m.StartGitOp(th.ID, GitOpRequest{Op: "stage", Arg: "../" + filepath.Base(outside) + "/secret.txt"}); err == nil {
		t.Fatalf("逃逸路径应被拒绝")
	}
	b, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if err != nil || string(b) != "don't touch\n" {
		t.Fatalf("外部文件被动过: %q err=%v", b, err)
	}
}
