package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/capyflow/friday/internal/config"
)

// 一轮跑完，投影里那条 assistant 消息要带上时间线留档（思考/工具/正文）。
// 思考与工具调用只经事件总线（内存环形缓冲）推给前端，进程一重启就全没了——
// 不留档的话重启后回放出来只剩正文，过程行整段消失。
func TestTurnRecordsTimelineBlocks(t *testing.T) {
	f := &fakeAgent{promptScript: defaultScript}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	got, ok := m.Get(th.ID)
	if !ok {
		t.Fatal("thread 丢失")
	}
	if len(got.Messages) != 2 {
		t.Fatalf("投影消息数不符: %+v", got.Messages)
	}
	blocks := got.Messages[1].Blocks
	if len(blocks) != 3 {
		t.Fatalf("留档块数不符（应为 思考+正文+工具）: %+v", blocks)
	}
	if blocks[0].Kind != BlockThink || blocks[0].Text != "思考：先看看目录" {
		t.Fatalf("思考块不符: %+v", blocks[0])
	}
	if blocks[1].Kind != BlockText || blocks[1].Text != "你好，这是回答。" {
		t.Fatalf("正文块不符: %+v", blocks[1])
	}
	if blocks[2].Kind != BlockTool || blocks[2].Name != "运行 ls" || blocks[2].Status != "ok" {
		t.Fatalf("工具块不符: %+v", blocks[2])
	}
	if blocks[2].Text == "" {
		t.Fatalf("工具块应有参数摘要: %+v", blocks[2])
	}
}

// 重启（重新读投影）后留档仍在——这是这次修复的落点：进程重启、回放历史，
// 过程行与工具明细都还在，不是只剩一段纯正文。
func TestTimelineBlocksSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	cfg := testConfig(t)

	f := &fakeAgent{promptScript: defaultScript}
	m := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	m.spawn = spawnFake(f)
	th := mustThread(t, m)
	c := &collector{}
	if _, err := m.Send(context.Background(), th.ID, "打个招呼", nil, c.hooks()); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	before, ok := m.Get(th.ID)
	if !ok || len(before.Messages[1].Blocks) == 0 {
		t.Fatalf("轮末没留下时间线块: %+v", before)
	}
	m.Close()

	// 重开：等价于 Friday 重启后读回投影。
	m2 := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	defer m2.Close()
	after, ok := m2.Get(th.ID)
	if !ok {
		t.Fatal("重启后会话丢失")
	}
	if len(after.Messages) != 2 {
		t.Fatalf("重启后投影消息数不符: %+v", after.Messages)
	}
	got := after.Messages[1].Blocks
	if len(got) != len(before.Messages[1].Blocks) {
		t.Fatalf("重启后留档块数变了: %+v", got)
	}
	if got[0].Kind != BlockThink || got[0].Text != "思考：先看看目录" {
		t.Fatalf("重启后思考块丢失: %+v", got)
	}
	if got[2].Kind != BlockTool || got[2].Name != "运行 ls" {
		t.Fatalf("重启后工具块丢失: %+v", got)
	}
}

// 旧投影（没有 blocks 字段）照旧能读：不能因为加了留档就让老会话打不开。
func TestOldProjectionWithoutBlocksStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	old := `{"workspaces":[],"threads":[{"id":"t1","agent":"fake","cwd":"/tmp","title":"老会话",
"createdAt":"2026-11-01T10:00:00Z","updatedAt":"2026-11-01T10:00:00Z","state":"idle",
"messages":[{"role":"user","text":"你好","at":"2026-11-01T10:00:00Z"},
{"role":"assistant","text":"在的","at":"2026-11-01T10:00:01Z"}]}],
"version":1}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("写旧投影失败: %v", err)
	}
	cfg := testConfig(t)
	m := NewManager(func() config.AcpConfig { return cfg }, nil, path)
	defer m.Close()
	th, ok := m.Get("t1")
	if !ok {
		t.Fatal("旧投影没读出来")
	}
	if len(th.Messages) != 2 || th.Messages[1].Text != "在的" {
		t.Fatalf("旧投影内容不符: %+v", th.Messages)
	}
	if len(th.Messages[1].Blocks) != 0 {
		t.Fatalf("旧投影不该有留档块: %+v", th.Messages[1].Blocks)
	}
}
