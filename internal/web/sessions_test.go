// sessions_test.go 覆盖网页会话清单与放行边界：发消息不能凭空造会话（主会话
// 之外一律拒），列表只分主会话与后台入口会话两类。
package web

import (
	"context"
	"path/filepath"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
)

// newSessionChat 造一个带独立数据目录的 Chat（会话历史落临时目录，
// 用例之间互不串）。
func newSessionChat(t *testing.T) *Chat {
	t.Helper()
	return newSessionChatIn(t, t.TempDir())
}

// newSessionChatIn 在给定数据目录里起一个 Chat。
func newSessionChatIn(t *testing.T, dir string) *Chat {
	t.Helper()
	return New(agent.NewBrain(newTestAgent(t), newTestPool(t, dir), nil), dir,
		filepath.Join(dir, "paused.json"))
}

// newTestAgent / newTestPool 起一套最小可用的对话底座：模型是 noop，
// 会话历史落测试临时目录（清单用例要真的看到消息数与活跃时刻）。
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

// TestResolveSessionKey 不带 session 与显式 web 都落到主会话；后台入口的 key
// 与任意字符串（含曾经的工作会话 key 形状）都不能通过发消息造出会话。
func TestResolveSessionKey(t *testing.T) {
	if key, err := resolveSessionKey(""); err != nil || key != agent.SessionWeb {
		t.Fatalf("不带 session 应是主会话: %q %v", key, err)
	}
	if key, err := resolveSessionKey(" web "); err != nil || key != agent.SessionWeb {
		t.Fatalf("显式 web 应是主会话: %q %v", key, err)
	}
	for _, bad := range []string{"task", "goal-1", "work-20260101-000000-dead"} {
		if _, err := resolveSessionKey(bad); err == nil {
			t.Fatalf("%q 不该被放行", bad)
		}
	}
}

// TestListSessionsKinds 列表分主会话/后台会话两类：主会话在最前，后台入口
// （心跳/任务）垫底并标 internal。
func TestListSessionsKinds(t *testing.T) {
	c := newSessionChat(t)
	ctx := context.Background()

	say(t, ctx, c, agent.SessionWeb, "日常说一句")
	// 后台入口的会话（心跳/任务）也活在池里，应标成 internal。
	say(t, ctx, c, agent.SessionTask, "心跳巡检")

	list := c.ListSessions(ctx)
	if len(list) != 2 {
		t.Fatalf("应列出 2 条会话: %+v", list)
	}
	if list[0].Key != agent.SessionWeb || list[0].Kind != SessionKindLife || list[0].Title != lifeSessionTitle {
		t.Fatalf("第一条应是主会话: %+v", list[0])
	}
	if list[0].Messages != 2 {
		t.Fatalf("主会话应带消息数: %+v", list[0])
	}
	if list[1].Key != agent.SessionTask || list[1].Kind != SessionKindInternal {
		t.Fatalf("后台会话应垫底并标 internal: %+v", list[1])
	}
}
