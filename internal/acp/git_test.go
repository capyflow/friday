package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 测试用的 git 仓库夹具 ----------

// requireGitCLI 环境里没有 git 时跳过（而不是让用例失败）：这套用例要真的
// 跑 git 命令，不能靠 mock 蒙混过关。
func requireGitCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("环境里没有 git，跳过：%v", err)
	}
}

// gitFixtureRepo 造一个真实的 git 仓库：本地 config 设身份（不依赖全局 git
// config），提交两个文件。返回仓库目录。
//
// 分支名不假设：git 的默认分支名可能是 master 也可能是 main，取决于安装的
// git 版本与 init.defaultBranch，所以用例一律用 rev-parse 读实际值。
func gitFixtureRepo(t *testing.T) string {
	t.Helper()
	requireGitCLI(t)
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "friday-test")
	gitTest(t, dir, "config", "user.email", "friday-test@example.com")
	// 兜底：别让机器上的 gpg 签名配置把提交卡住。
	gitTest(t, dir, "config", "commit.gpgsign", "false")
	mustWriteFile(t, dir, "a.txt", "one\n")
	mustWriteFile(t, dir, "b.txt", "two\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-qm", "init")
	return dir
}

// gitBranchOf 读仓库当前分支名（不假设 master/main）。
func gitBranchOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
}

// gitThreadIn 建一个锚定在 dir 上的会话（走工作区 → 会话的正规路径，
// 不改生产代码）。
func gitThreadIn(t *testing.T, m *Manager, dir string) Thread {
	t.Helper()
	ws := mustWorkspace(t, m, dir)
	return mustThreadIn(t, m, ws)
}

// gitStatusFiles 把状态里的文件列表转成 map，方便按路径断言。
func gitStatusFiles(st *GitStatus) map[string]GitFileStatus {
	out := map[string]GitFileStatus{}
	for _, f := range st.Files {
		out[f.Path] = f
	}
	return out
}

// ---------- 只读查询 ----------

func TestGitStatusOnNonRepoDirectory(t *testing.T) {
	requireGitCLI(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, t.TempDir()) // 普通目录，不是仓库

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("非仓库目录不该报错: %v", err)
	}
	if st == nil {
		t.Fatal("状态不该是 nil")
	}
	if st.IsRepo {
		t.Fatalf("普通目录不该被当成仓库: %+v", st)
	}
	if strings.TrimSpace(st.Note) == "" {
		t.Fatalf("非仓库目录要有一句说明（前端据此告诉用户这里不是 git 仓库）: %+v", st)
	}
}

func TestGitStatusReportsCurrentBranch(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if !st.IsRepo {
		t.Fatalf("应是仓库: %+v", st)
	}
	if want := gitBranchOf(t, repo); st.Branch != want {
		t.Fatalf("分支名不符: got %q want %q", st.Branch, want)
	}
	if st.Detached {
		t.Fatalf("不该是 detached: %+v", st)
	}
	if st.Staged != 0 || st.Modified != 0 || st.Untracked != 0 || st.Deleted != 0 {
		t.Fatalf("干净的仓库不该有计数: %+v", st)
	}
	if len(st.Files) != 0 {
		t.Fatalf("干净的仓库不该有文件列表: %+v", st.Files)
	}
}

func TestGitStatusCountsModifiedAndUntracked(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n") // 已跟踪文件被改
	mustWriteFile(t, repo, "new.txt", "brand new\n")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Modified != 1 {
		t.Fatalf("Modified 应为 1: %+v", st)
	}
	if st.Untracked != 1 {
		t.Fatalf("Untracked 应为 1: %+v", st)
	}
	if st.Staged != 0 {
		t.Fatalf("Staged 应为 0: %+v", st)
	}
	files := gitStatusFiles(st)
	if f, ok := files["a.txt"]; !ok || f.Stage != "unstaged" {
		t.Fatalf("a.txt 应是未暂存的修改: %+v", st.Files)
	}
	if _, ok := files["new.txt"]; !ok {
		t.Fatalf("new.txt 应出现在文件列表里: %+v", st.Files)
	}
}

func TestGitStatusCountsStagedChanges(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "changed\n")
	gitTest(t, repo, "add", "a.txt")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Staged != 1 {
		t.Fatalf("Staged 应为 1: %+v", st)
	}
	if st.Modified != 0 {
		t.Fatalf("已暂存的文件不该再算工作区修改: %+v", st)
	}
	files := gitStatusFiles(st)
	if f, ok := files["a.txt"]; !ok || f.Stage != "staged" {
		t.Fatalf("a.txt 应是已暂存: %+v", st.Files)
	}
}

func TestGitStatusCountsDeletedFile(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	if err := os.Remove(filepath.Join(repo, "b.txt")); err != nil {
		t.Fatalf("删文件失败: %v", err)
	}

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Deleted != 1 {
		t.Fatalf("Deleted 应为 1: %+v", st)
	}
	if _, ok := gitStatusFiles(st)["b.txt"]; !ok {
		t.Fatalf("b.txt 应出现在文件列表里: %+v", st.Files)
	}
}

func TestGitStatusDoesNotReportConflictForStagedNewFile(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "new.txt", "brand new\n")
	gitTest(t, repo, "add", "new.txt")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Conflicted != 0 {
		t.Fatalf("普通的新增文件不是冲突: %+v", st)
	}
	if st.Staged != 1 {
		t.Fatalf("Staged 应为 1: %+v", st)
	}
}

func TestGitStatusCountsStagedAndModifiedSeparately(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "staged\n")
	gitTest(t, repo, "add", "a.txt")
	mustWriteFile(t, repo, "a.txt", "staged then modified again\n")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Staged != 1 {
		t.Fatalf("Staged 应为 1: %+v", st)
	}
	if st.Modified != 1 {
		t.Fatalf("既暂存又改了的工作区改动也要算 Modified: %+v", st)
	}
}

func TestGitStatusMarksStagedAndModifiedAsBoth(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	mustWriteFile(t, repo, "a.txt", "staged\n")
	gitTest(t, repo, "add", "a.txt")
	mustWriteFile(t, repo, "a.txt", "staged then edited again\n")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	f, ok := gitStatusFiles(st)["a.txt"]
	if !ok {
		t.Fatalf("a.txt 应出现在文件列表里: %+v", st.Files)
	}
	if f.Stage != "both" {
		t.Fatalf("既暂存又改了，Stage 应为 both: %+v", f)
	}
}

func TestGitStatusListsRenamedFileOnce(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	gitTest(t, repo, "mv", "a.txt", "renamed.txt")

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	files := gitStatusFiles(st)
	if _, ok := files["renamed.txt"]; !ok {
		t.Fatalf("renamed.txt 应出现在文件列表里: %+v", st.Files)
	}
	if len(st.Files) != 1 {
		t.Fatalf("改名不该多出条目（旧路径那条要跳过）: %+v", st.Files)
	}
}

func TestGitLogReturnsHistory(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	gitTest(t, repo, "commit", "-q", "--allow-empty", "-m", "second commit")

	commits, err := m.GitLog(th.ID, 10)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("应有 2 条历史: %+v", commits)
	}
	latest := commits[0]
	if latest.Subject != "second commit" {
		t.Fatalf("最新一条的 subject 不符: %+v", latest)
	}
	if latest.Author != "friday-test" {
		t.Fatalf("作者不符: %+v", latest)
	}
	if latest.Email != "friday-test@example.com" {
		t.Fatalf("邮箱不符: %+v", latest)
	}
	if strings.TrimSpace(latest.Hash) == "" || strings.TrimSpace(latest.Short) == "" {
		t.Fatalf("hash 不该为空: %+v", latest)
	}
	if latest.Date.IsZero() {
		t.Fatalf("日期不该为空: %+v", latest)
	}
	if commits[1].Subject != "init" {
		t.Fatalf("第二条应是 init: %+v", commits[1])
	}
}

func TestGitLogRespectsLimit(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	for i := 0; i < 3; i++ {
		gitTest(t, repo, "commit", "-q", "--allow-empty", "-m", "extra")
	}

	if got, err := m.GitLog(th.ID, 2); err != nil {
		t.Fatalf("读历史失败: %v", err)
	} else if len(got) != 2 {
		t.Fatalf("limit=2 应返回 2 条: %+v", got)
	}
	if got, err := m.GitLog(th.ID, 0); err != nil {
		t.Fatalf("读历史失败: %v", err)
	} else if len(got) != 4 {
		t.Fatalf("limit=0 应走默认上限（够用即可）: %+v", got)
	}
	if got, err := m.GitLog(th.ID, 100000); err != nil {
		t.Fatalf("读历史失败: %v", err)
	} else if len(got) != 4 {
		t.Fatalf("超大的 limit 应被夹住: %+v", got)
	}
}

func TestGitStatusReportsAheadBehind(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	// 本地裸仓库当远端（纯本地文件路径，不联网）。
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatalf("建远端目录失败: %v", err)
	}
	gitTest(t, origin, "init", "-q", "--bare")
	gitTest(t, repo, "remote", "add", "origin", origin)
	gitTest(t, repo, "push", "-q", "-u", "origin", "HEAD")

	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Upstream == "" {
		t.Fatalf("有上游时 Upstream 不该为空: %+v", st)
	}
	if st.Ahead != 0 || st.Behind != 0 {
		t.Fatalf("刚推上去不该有领先/落后: %+v", st)
	}

	// 本地多一个提交 → 领先 1。
	mustWriteFile(t, repo, "a.txt", "local change\n")
	gitTest(t, repo, "add", "a.txt")
	gitTest(t, repo, "commit", "-qm", "local only")

	st, err = m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if st.Ahead != 1 {
		t.Fatalf("Ahead 应为 1: %+v", st)
	}
	if st.Behind != 0 {
		t.Fatalf("Behind 应为 0: %+v", st)
	}
}

func TestGitStatusOnDetachedHead(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	head := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	gitTest(t, repo, "checkout", "-q", head)

	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	st, err := m.GitStatus(th.ID)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if !st.Detached {
		t.Fatalf("游离 HEAD 应标 Detached: %+v", st)
	}
}

func TestGitLogHandlesMultiLineBody(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	gitTest(t, repo, "commit", "-q", "--allow-empty", "-m", "有正文", "-m", "正文第一行\n正文第二行")

	commits, err := m.GitLog(th.ID, 5)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(commits) == 0 {
		t.Fatal("应有历史")
	}
	latest := commits[0]
	if latest.Subject != "有正文" {
		t.Fatalf("subject 不符: %+v", latest)
	}
	if !strings.Contains(latest.Body, "正文第一行") {
		t.Fatalf("正文丢了: %+v", latest)
	}
}

func TestGitBranchesListsCurrentAndNewBranch(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	gitTest(t, repo, "branch", "feature")

	branches, err := m.GitBranches(th.ID)
	if err != nil {
		t.Fatalf("读分支失败: %v", err)
	}
	current := gitBranchOf(t, repo)
	var sawCurrent bool
	sawFeature := false
	for _, b := range branches {
		if b.Name == current {
			sawCurrent = true
			if !b.Current {
				t.Fatalf("当前分支 %q 的 Current 应为 true: %+v", current, b)
			}
		}
		if b.Name == "feature" {
			sawFeature = true
			if b.Current {
				t.Fatalf("feature 不是当前分支: %+v", b)
			}
		}
	}
	if !sawCurrent {
		t.Fatalf("分支列表里应有当前分支 %q: %+v", current, branches)
	}
	if !sawFeature {
		t.Fatalf("新建的分支应列出来: %+v", branches)
	}
}

func TestGitBranchesMarksRemoteBranches(t *testing.T) {
	requireGitCLI(t)
	repo := gitFixtureRepo(t)
	// 造一个远端跟踪引用（纯本地，不联网）：git branch -a 才会列它。
	gitTest(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, repo)

	branches, err := m.GitBranches(th.ID)
	if err != nil {
		t.Fatalf("读分支失败: %v", err)
	}
	var remote *GitBranch
	for i := range branches {
		if branches[i].Name == "origin/main" {
			remote = &branches[i]
		}
	}
	if remote == nil {
		t.Fatalf("远端分支应列出来: %+v", branches)
	}
	if !remote.Remote {
		t.Fatalf("远端分支的 Remote 应为 true: %+v", remote)
	}
}

func TestGitQueriesOnNonRepoDirectory(t *testing.T) {
	requireGitCLI(t)
	m := newTestManager(t, nil, nil, nil)
	th := gitThreadIn(t, m, t.TempDir())

	commits, err := m.GitLog(th.ID, 10)
	if err != nil {
		t.Fatalf("非仓库读历史不该报错: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("非仓库不该有历史: %+v", commits)
	}
	branches, err := m.GitBranches(th.ID)
	if err != nil {
		t.Fatalf("非仓库读分支不该报错: %v", err)
	}
	if len(branches) != 0 {
		t.Fatalf("非仓库不该有分支: %+v", branches)
	}
}
