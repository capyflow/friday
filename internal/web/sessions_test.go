// sessions_test.go 覆盖工作模式会话登记：新建/改名/删除对外部文件的写入、
// 会话 key 的放行边界、列表的归属与排序，以及重启后能从文件恢复。
package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
)

// newSessionChat 造一个带会话登记表的 Chat（会话池按测试临时目录落盘，
// 每个用例一份独立路径，避免用例之间互相看到对方的会话）。
func newSessionChat(t *testing.T) (*Chat, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "work-sessions.json")
	return New(agent.NewBrain(newTestAgent(t), newTestPool(t, dir), nil), dir,
		filepath.Join(dir, "paused.json"), file), file
}

// readWork 读登记表原文（断言落盘内容，而不是只看内存态）。
func readWork(t *testing.T, file string) map[string]sessionMeta {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var items map[string]sessionMeta
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("登记表不是合法 JSON: %v (%s)", err, data)
	}
	return items
}

// newTestAgent / newTestPool 起一套最小可用的对话底座：模型是 noop，
// 会话历史落测试临时目录（登记表用例要真的看到消息数与活跃时刻）。
func newTestAgent(t *testing.T) *vxagent.Agent {
	t.Helper()
	return vxagent.New(vxagent.Options{Provider: noopProvider{}, Model: "m"})
}

func newTestPool(t *testing.T, dir string) *agent.SessionPool {
	t.Helper()
	store, err := sessionstore.NewJSON(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatalf("会话存储初始化失败: %v", err)
	}
	return agent.NewSessionPool(store)
}

func say(t *testing.T, ctx context.Context, c *Chat, key, text string) {
	t.Helper()
	ctx = agent.WithSessionKey(ctx, key)
	sess := c.brain.Pool().Get(ctx, key)
	sess.Add(llm.NewTextMessage(llm.RoleUser, text))
	sess.Add(llm.NewTextMessage(llm.RoleAssistant, "收到"))
	if err := c.brain.Pool().Save(ctx, sess); err != nil {
		t.Fatalf("会话落盘失败: %v", err)
	}
}

func findSession(list []ChatSession, key string) *ChatSession {
	for i := range list {
		if list[i].Key == key {
			return &list[i]
		}
	}
	return nil
}

// TestCreateWorkSessionPersists 新建会话写进登记表，标题回落到「新会话」。
func TestCreateWorkSessionPersists(t *testing.T) {
	c, file := newSessionChat(t)
	ctx := context.Background()

	session, err := c.CreateWorkSession(ctx, "")
	if err != nil {
		t.Fatalf("新建会话失败: %v", err)
	}
	if !strings.HasPrefix(session.Key, workKeyPrefix) {
		t.Fatalf("key 应带 %s 前缀，实际 %q", workKeyPrefix, session.Key)
	}
	if session.Kind != SessionKindWork || session.Title != newSessionTitle {
		t.Fatalf("新会话应是未起名的工作会话: %+v", session)
	}
	items := readWork(t, file)
	if items[session.Key].CreatedAt.IsZero() {
		t.Fatalf("登记表应记下创建时刻: %+v", items)
	}
	// 还没说过话就没有"最后活跃"，列表必须回落到创建时刻，
	// 不能拿零值时间（0001-01-01）去排顺序或算"多久之前"。
	if fresh := findSession(c.ListSessions(ctx), session.Key); fresh == nil ||
		fresh.UpdatedAt.IsZero() || !fresh.UpdatedAt.Equal(fresh.CreatedAt) {
		t.Fatalf("空会话的 updatedAt 应等于创建时刻: %+v", fresh)
	}

	// 发过话之后，未起名的会话按首条提问自动取名。
	say(t, ctx, c, session.Key, "帮我排一下这周的健身计划\n第二行不该进标题")
	if got := findSession(c.ListSessions(ctx), session.Key); got == nil || got.Title != "帮我排一下这周的健身计划" {
		t.Fatalf("标题应取首行提问: %+v", got)
	} else if got.Messages != 2 || got.UpdatedAt.IsZero() {
		t.Fatalf("列表应带上消息数与活跃时刻: %+v", got)
	}
}

// TestRenameWorkSession 改名写盘；清空名字回到自动取名。
func TestRenameWorkSession(t *testing.T) {
	c, file := newSessionChat(t)
	ctx := context.Background()
	session, err := c.CreateWorkSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	say(t, ctx, c, session.Key, "整理季度 OKR")

	view, err := c.RenameWorkSession(ctx, session.Key, "  OKR 复盘 ")
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if view.Title != "OKR 复盘" || !view.HasTitle {
		t.Fatalf("改名应去空白并标记有名: %+v", view)
	}
	if readWork(t, file)[session.Key].Title != "OKR 复盘" {
		t.Fatalf("改名没落盘: %+v", readWork(t, file))
	}

	view, err = c.RenameWorkSession(ctx, session.Key, "   ")
	if err != nil {
		t.Fatalf("清空名字应成功: %v", err)
	}
	if view.Title != "整理季度 OKR" || view.HasTitle {
		t.Fatalf("清空后应回到首条提问自动取名: %+v", view)
	}

	if _, err := c.RenameWorkSession(ctx, agent.SessionWeb, "别的名字"); err == nil {
		t.Fatal("主会话不该能改名")
	}
	if _, err := c.RenameWorkSession(ctx, "work-不存在", "x"); err == nil {
		t.Fatal("未登记的会话不该能改名")
	}
}

// TestDeleteWorkSessionClearsEverything 删除会话把登记表、历史与暂停现场一起带走。
func TestDeleteWorkSessionClearsEverything(t *testing.T) {
	c, file := newSessionChat(t)
	ctx := context.Background()
	session, err := c.CreateWorkSession(ctx, "删我")
	if err != nil {
		t.Fatal(err)
	}
	say(t, ctx, c, session.Key, "先说点什么")
	c.mu.Lock()
	c.paused[session.Key] = &PausedTurn{Question: "先说点什么", Partial: "好的"}
	c.savePausedLocked()
	c.mu.Unlock()

	if err := c.DeleteWorkSession(session.Key); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if items := readWork(t, file); items[session.Key] != (sessionMeta{}) {
		t.Fatalf("登记表里还留着这条: %+v", items)
	}
	if msgs := c.brain.Pool().History(ctx, session.Key); len(msgs) != 0 {
		t.Fatalf("历史没清掉: %d 条", len(msgs))
	}
	c.mu.Lock()
	_, paused := c.paused[session.Key]
	c.mu.Unlock()
	if paused {
		t.Fatal("暂停现场应随会话删除")
	}
	if findSession(c.ListSessions(ctx), session.Key) != nil {
		t.Fatal("列表里不该再有这条会话")
	}

	if err := c.DeleteWorkSession(agent.SessionWeb); err == nil {
		t.Fatal("主会话不能删")
	}
	if err := c.DeleteWorkSession("work-没建过"); err == nil {
		t.Fatal("未登记的会话不该能删")
	}
}

// TestDeleteRejectsRunningSession 正在回复的会话要你先停，不能删到一半。
func TestDeleteRejectsRunningSession(t *testing.T) {
	c, _ := newSessionChat(t)
	session, err := c.CreateWorkSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.runs[session.Key] = &chatRun{cancel: func() {}}
	c.mu.Unlock()

	err = c.DeleteWorkSession(session.Key)
	if err == nil || !strings.Contains(err.Error(), "正在回复中") {
		t.Fatalf("进行中的会话应拒绝删除并说明下一步: %v", err)
	}
}

// TestResolveSessionKey 发消息只放行主会话与登记过的会话。
func TestResolveSessionKey(t *testing.T) {
	c, _ := newSessionChat(t)
	ctx := context.Background()

	if key, err := c.resolveSessionKey(""); err != nil || key != agent.SessionWeb {
		t.Fatalf("不带 session 应是主会话: %q %v", key, err)
	}
	if key, err := c.resolveSessionKey(" web "); err != nil || key != agent.SessionWeb {
		t.Fatalf("显式 web 应是主会话: %q %v", key, err)
	}
	// 后台入口的 key 与任意字符串都不能通过发消息造出会话。
	for _, bad := range []string{"task", "goal-1", "work-20260101-000000-dead"} {
		if _, err := c.resolveSessionKey(bad); err == nil {
			t.Fatalf("%q 不该被放行", bad)
		}
	}

	session, err := c.CreateWorkSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if key, err := c.resolveSessionKey(session.Key); err != nil || key != session.Key {
		t.Fatalf("登记过的会话应放行: %q %v", key, err)
	}
}

// TestListSessionsKinds 列表分主会话/工作会话/后台会话，工作会话按最近活动倒序。
func TestListSessionsKinds(t *testing.T) {
	c, _ := newSessionChat(t)
	ctx := context.Background()

	first, err := c.CreateWorkSession(ctx, "第一个")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateWorkSession(ctx, "第二个")
	if err != nil {
		t.Fatal(err)
	}
	say(t, ctx, c, agent.SessionWeb, "日常说一句")
	say(t, ctx, c, first.Key, "干活一")
	say(t, ctx, c, second.Key, "干活二")
	// 后台入口的会话（心跳/任务）也活在池里，应标成 internal。
	say(t, ctx, c, agent.SessionTask, "心跳巡检")

	list := c.ListSessions(ctx)
	if len(list) != 4 {
		t.Fatalf("应列出 4 条会话: %+v", list)
	}
	if list[0].Key != agent.SessionWeb || list[0].Kind != SessionKindLife || list[0].Title != lifeSessionTitle {
		t.Fatalf("第一条应是主会话: %+v", list[0])
	}
	if list[1].Key != second.Key || list[2].Key != first.Key {
		t.Fatalf("工作会话应按最近活动倒序: %+v %+v", list[1], list[2])
	}
	if list[3].Key != agent.SessionTask || list[3].Kind != SessionKindInternal {
		t.Fatalf("后台会话应垫底并标 internal: %+v", list[3])
	}
}

// TestWorkSessionsSurviveRestart 重启后登记表仍在（新建但还没发过话的会话也在），
// 登记表损坏时保留原文件、不覆盖成空表。
func TestWorkSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "work-sessions.json")
	ag, pool := newTestAgent(t), newTestPool(t, dir)
	ctx := context.Background()

	c := New(agent.NewBrain(ag, pool, nil), dir, filepath.Join(dir, "paused.json"), file)
	session, err := c.CreateWorkSession(ctx, "重启还要在")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateWorkSession(ctx, ""); err != nil {
		t.Fatal(err)
	}

	restarted := New(agent.NewBrain(ag, pool, nil), dir, filepath.Join(dir, "paused.json"), file)
	got := findSession(restarted.ListSessions(ctx), session.Key)
	if got == nil || got.Title != "重启还要在" {
		t.Fatalf("重启后应恢复登记与会话名: %+v", got)
	}
	if _, err := restarted.resolveSessionKey(session.Key); err != nil {
		t.Fatalf("重启后这条会话仍应可对话: %v", err)
	}

	if err := os.WriteFile(file, []byte("{坏掉的 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := New(agent.NewBrain(ag, pool, nil), dir, filepath.Join(dir, "paused.json"), file)
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "坏掉的") {
		t.Fatalf("损坏的登记表应原样保留: %v %s", err, data)
	}
	if len(broken.ListSessions(ctx)) != 1 {
		t.Fatalf("读不到登记表演示为没有工作会话: %+v", broken.ListSessions(ctx))
	}
}

// TestSessionTitleTruncated 首条提问太长时标题截断，会话栏一行放得下。
func TestSessionTitleTruncated(t *testing.T) {
	c, _ := newSessionChat(t)
	ctx := context.Background()
	session, err := c.CreateWorkSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("要", sessionTitleMax+10) + "做的事"
	say(t, ctx, c, session.Key, long)

	got := findSession(c.ListSessions(ctx), session.Key)
	runes := []rune(got.Title)
	if len(runes) != sessionTitleMax+1 || !strings.HasSuffix(got.Title, "…") {
		t.Fatalf("标题应截到 %d 字加省略号: %q", sessionTitleMax, got.Title)
	}
}
