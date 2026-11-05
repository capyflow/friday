package acp

// session_record_test.go 守每条会话一份记录这套：什么时候写、写的内容对不对、
// git 看不看得见、会话删掉与启动收尾时怎么清、交接怎么指向来源会话的记录。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordSetup(t *testing.T, dir string) (*Manager, Thread) {
	t.Helper()
	m := newTestManager(t, nil, nil, &fakeAgent{promptScript: defaultScript})
	ws := mustWorkspace(t, m, dir)
	return m, mustThreadIn(t, m, ws)
}

func readRecord(t *testing.T, dir string, th Thread) string {
	t.Helper()
	abs, rel := sessionRecordPath(dir, th.ID)
	if rel == "" {
		t.Fatal("会话记录路径推导失败")
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("读会话记录 %s: %v", rel, err)
	}
	return string(b)
}

// 每条会话都有记录，且每轮结束整份重写（不是增量追加，避免和投影漂移）。
func TestSessionRecordRewrittenEachTurn(t *testing.T) {
	dir := t.TempDir()
	m, th := recordSetup(t, dir)

	if _, err := m.Send(context.Background(), th.ID, "第一轮的问题", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	text := readRecord(t, dir, th)
	for _, want := range []string{"# Friday 会话记录", th.ID, "第一轮的问题", "## 这份记录里没有什么"} {
		if !strings.Contains(text, want) {
			t.Errorf("记录缺少 %q:\n%s", want, text)
		}
	}

	if _, err := m.Send(context.Background(), th.ID, "第二轮的问题", nil, TurnHooks{}); err != nil {
		t.Fatalf("次轮: %v", err)
	}
	text = readRecord(t, dir, th)
	if !strings.Contains(text, "第二轮的问题") || !strings.Contains(text, "第一轮的问题") {
		t.Errorf("重写后的记录应含两轮内容:\n%s", text)
	}
	if strings.Contains(text, "handoff") {
		t.Errorf("不该再有交接目录的说法:\n%s", text)
	}
}

// 不污染仓库这条是真跑 git 验的：快照算未跟踪用的那条命令里只有自忽略 .gitignore。
func TestSessionRecordInvisibleToGit(t *testing.T) {
	dir := initTestRepo(t)
	m, th := recordSetup(t, dir)
	if _, err := m.Send(context.Background(), th.ID, "改点东西", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	others := strings.Fields(gitTest(t, dir, "ls-files", "--others", "--exclude-standard"))
	sawIgnore := false
	for _, o := range others {
		if strings.HasPrefix(o, sessionDirRel) {
			t.Errorf("会话记录被当成未跟踪改动: %v", others)
		}
		if o == fridayBaseRel+"/.gitignore" {
			sawIgnore = true
		}
	}
	if !sawIgnore {
		t.Errorf("自忽略 .gitignore 应作为一次性新增可见，实际: %v", others)
	}
}

func TestDeleteThreadRemovesSessionRecord(t *testing.T) {
	dir := t.TempDir()
	m, th := recordSetup(t, dir)
	if _, err := m.Send(context.Background(), th.ID, "留个记录", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	abs, _ := sessionRecordPath(dir, th.ID)
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("记录应存在: %v", err)
	}
	if !m.Delete(th.ID) {
		t.Fatal("删除会话失败")
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("删会话应连带删掉它的记录，实际仍在: %v", err)
	}
}

// 启动收尾只清孤儿：会话还在就不动，记录数就是会话数，不该有淘汰策略替用户删。
func TestPruneSessionRecordsOnlyRemovesOrphans(t *testing.T) {
	dir := t.TempDir()
	m, th := recordSetup(t, dir)
	if _, err := m.Send(context.Background(), th.ID, "留个记录", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	recordDir := filepath.Join(dir, sessionDirRel)
	if err := os.WriteFile(filepath.Join(recordDir, "ghost.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // 再灌几条活会话：会话还在就不该被动到
		ws := mustWorkspace(t, m, dir)
		extra := mustThreadIn(t, m, ws)
		if err := os.WriteFile(filepath.Join(recordDir, extra.ID+".md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m.pruneSessionRecords()

	if _, err := os.Stat(filepath.Join(recordDir, "ghost.md")); !os.IsNotExist(err) {
		t.Error("孤儿记录没被清掉")
	}
	abs, _ := sessionRecordPath(dir, th.ID)
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("会话还在，记录不该被删: %v", err)
	}
}

// 交接不复制文件：新会话首轮块指向来源会话自己的记录，并记下交接链。
func TestHandoffPointsToSourceSessionRecord(t *testing.T) {
	dir := t.TempDir()
	m, th := recordSetup(t, dir)
	if _, err := m.Send(context.Background(), th.ID, "先看下这个项目", nil, TurnHooks{}); err != nil {
		t.Fatalf("首轮: %v", err)
	}
	// 来源记录被删掉（模拟老会话还没写过文件）：交接前应补写回来
	absFrom, relFrom := sessionRecordPath(dir, th.ID)
	if err := os.Remove(absFrom); err != nil {
		t.Fatal(err)
	}

	nt, err := m.Handoff(th.ID, "")
	if err != nil {
		t.Fatalf("交接失败: %v", err)
	}
	if _, err := os.Stat(absFrom); err != nil {
		t.Errorf("交接应补写来源会话的记录: %v", err)
	}
	if nt.HandoffFrom != th.ID {
		t.Errorf("续会话应记下交接来源: %q", nt.HandoffFrom)
	}
	if !strings.Contains(nt.Handoff, relFrom) {
		t.Errorf("首轮块应指向来源会话记录 %s，实际 %q", relFrom, nt.Handoff)
	}
	_, relNew := sessionRecordPath(dir, nt.ID)
	if !strings.Contains(nt.Messages[0].Text, relNew) {
		t.Errorf("note 要说出本会话记录在哪: %q", nt.Messages[0].Text)
	}

	// 新会话自己的记录里要能顺着交接链找到来源
	text := readRecord(t, dir, nt)
	for _, want := range []string{"交接自", th.ID, relFrom, "先看下这个项目"} {
		if !strings.Contains(text, want) {
			t.Errorf("续会话记录缺少 %q:\n%s", want, text)
		}
	}
}

// 记录正文要带上每轮改动清单与附件数量——这两样正是"精简摘要"最容易丢的东西。
func TestSessionRecordListsChangesAndImages(t *testing.T) {
	th := &Thread{ID: "new1", Agent: "codex", Cwd: "/home/u/p", Title: "加密改造", HandoffFrom: "old1"}
	text := sessionRecordText(recordArgs{
		Thread: th, WsName: "codis", FromCwd: "/home/u/p", FromTitle: "上一段",
		Messages: []Entry{
			{Role: "user", Text: "把加密补上", Images: 2},
			{Role: "assistant", Text: "已改完。", Changes: &TurnChanges{Files: []FileChange{
				{Path: "core/enc.go", Status: "modified", Additions: 3, Deletions: 1},
				{Path: "core/enc_test.go", Status: "added"},
			}}},
		},
	})
	for _, want := range []string{
		"本轮改动：core/enc.go（modified +3/-1）、core/enc_test.go（added +0/-0）",
		"附 2 张图", "图片附件的像素内容", "codis", "上一段",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("记录缺少 %q:\n%s", want, text)
		}
	}
}

// .friday 被占成普通文件时写不出记录，但交接与对话本身不能因此失败。
func TestSessionRecordWriteFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fridayBaseRel), []byte("挡住"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, th := recordSetup(t, dir)
	if _, err := m.Send(context.Background(), th.ID, "照样跑", nil, TurnHooks{}); err != nil {
		t.Fatalf("写不出记录不该影响对话: %v", err)
	}
	nt, err := m.Handoff(th.ID, "")
	if err != nil {
		t.Fatalf("写不出记录也不该影响交接: %v", err)
	}
	if !strings.Contains(nt.Handoff, "没能生成") {
		t.Errorf("写不出记录时要明说，别让 agent 去找不存在的路径: %q", nt.Handoff)
	}
	if !strings.Contains(nt.Handoff, "照样跑") {
		t.Errorf("精简摘要仍要随首轮发出去: %q", nt.Handoff)
	}
	if strings.Contains(nt.Messages[0].Text, "本会话的完整记录：") {
		t.Errorf("没写成文件就不该报路径: %q", nt.Messages[0].Text)
	}
}
