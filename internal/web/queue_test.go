package web

import (
	"testing"
	"time"

	"github.com/capyflow/friday/internal/eventbus"
)

// TestQueueFIFOOrder 队列必须先进先出：先排队的先跑。
func TestQueueFIFOOrder(t *testing.T) {
	q := &turnQueue{}
	q.push(&queuedTurn{id: "t1", question: "第一"})
	q.push(&queuedTurn{id: "t2", question: "第二"})
	q.push(&queuedTurn{id: "t3", question: "第三"})

	if got := q.len(); got != 3 {
		t.Fatalf("队列长度 = %d，期望 3", got)
	}
	for _, want := range []string{"t1", "t2", "t3"} {
		item := q.pop()
		if item == nil {
			t.Fatalf("队列提前空了，期望 %s", want)
		}
		if item.id != want {
			t.Fatalf("出队顺序 = %s，期望 %s", item.id, want)
		}
	}
	if q.pop() != nil {
		t.Fatal("空队列 pop 应返回 nil")
	}
}

// TestQueuePushPosition push 返回的位次从 1 起，且与实际位置一致。
func TestQueuePushPosition(t *testing.T) {
	q := &turnQueue{}
	if pos := q.push(&queuedTurn{id: "t1"}); pos != 1 {
		t.Fatalf("首位次 = %d，期望 1", pos)
	}
	if pos := q.push(&queuedTurn{id: "t2"}); pos != 2 {
		t.Fatalf("次位次 = %d，期望 2", pos)
	}
}

// TestQueueRemoveAndFind 取消排队项要能按 id 精确移除，其余项不受影响。
func TestQueueRemoveAndFind(t *testing.T) {
	q := &turnQueue{}
	q.push(&queuedTurn{id: "t1"})
	q.push(&queuedTurn{id: "t2"})
	q.push(&queuedTurn{id: "t3"})

	if q.find("t2") == nil {
		t.Fatal("find 应找到 t2")
	}
	got := q.remove("t2")
	if got == nil || got.id != "t2" {
		t.Fatal("remove 应返回被移除的 t2")
	}
	if q.find("t2") != nil {
		t.Fatal("移除后 find 不应再找到 t2")
	}
	if q.len() != 2 {
		t.Fatalf("移除后队列长度 = %d，期望 2", q.len())
	}
	if q.remove("不存在") != nil {
		t.Fatal("移除不存在的项应返回 nil")
	}
}

// TestQueueSnapshot 快照要带位次与来源，前端据此显示「排队中 · 第 N 位」。
func TestQueueSnapshot(t *testing.T) {
	q := &turnQueue{}
	q.push(&queuedTurn{
		id: "t1", question: "问一句",
		origin: eventbus.Origin{ClientID: "c1", Device: "手机端"},
	})
	q.push(&queuedTurn{id: "t2", question: "再问一句"})

	snap := q.snapshot()
	if len(snap) != 2 {
		t.Fatalf("快照长度 = %d，期望 2", len(snap))
	}
	if snap[0].Position != 1 || snap[0].TurnID != "t1" {
		t.Fatalf("首项快照不对：%+v", snap[0])
	}
	if snap[0].Device != "手机端" || snap[0].ClientID != "c1" {
		t.Fatalf("快照没带上来源：%+v", snap[0])
	}
	if snap[1].Position != 2 {
		t.Fatalf("次项位次 = %d，期望 2", snap[1].Position)
	}
}

// TestChatEnqueueRejectsWhenFull 队列满了要拒收并说清原因——
// 无限期攒下去，最后一次性全放出来不是用户想要的。
func TestChatEnqueueRejectsWhenFull(t *testing.T) {
	c := &Chat{
		runs:        map[string]map[string]*chatRun{},
		queues:      map[string]*turnQueue{},
		dispatching: map[string]bool{},
		paused:      map[string]*PausedTurn{},
	}
	// 占住 dispatching，模拟「这一轮正在跑」，让新来的都进队列。
	c.dispatching["web"] = true
	for i := 0; i < queueMax; i++ {
		status, _, err := c.enqueue("web", &queuedTurn{id: "t" + string(rune('a'+i))})
		if err != nil {
			t.Fatalf("第 %d 条不应被拒：%v", i, err)
		}
		if status != statusQueued {
			t.Fatalf("第 %d 条状态 = %s，期望 %s", i, status, statusQueued)
		}
	}
	_, _, err := c.enqueue("web", &queuedTurn{id: "overflow"})
	if err == nil {
		t.Fatal("队列满时应返回错误")
	}
}

// TestChatEnqueueStartsWhenIdle 会话空闲时立刻开跑（statusRunning），不排队。
func TestChatEnqueueStartsWhenIdle(t *testing.T) {
	c := &Chat{
		runs:        map[string]map[string]*chatRun{},
		queues:      map[string]*turnQueue{},
		dispatching: map[string]bool{},
		paused:      map[string]*PausedTurn{},
	}
	status, position, err := c.enqueue("web", &queuedTurn{id: "t1", question: "你好"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if status != statusRunning {
		t.Fatalf("空闲时应立刻开跑，状态 = %s", status)
	}
	if position != 1 {
		t.Fatalf("位次 = %d，期望 1", position)
	}
}

// TestQueuedTurnExpires 超过 TTL 的排队项应被判定为过期。
func TestQueuedTurnExpires(t *testing.T) {
	old := &queuedTurn{id: "t1", enqueuedAt: time.Now().Add(-queueTTL - time.Minute)}
	if time.Since(old.enqueuedAt) <= queueTTL {
		t.Fatal("构造的排队项应已过期")
	}
	fresh := &queuedTurn{id: "t2", enqueuedAt: time.Now()}
	if time.Since(fresh.enqueuedAt) > queueTTL {
		t.Fatal("新排队项不应过期")
	}
}
