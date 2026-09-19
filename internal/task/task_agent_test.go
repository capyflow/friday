package task

import (
	"sync"
	"testing"
	"time"
)

// TestScheduleAgentCron 验收 M3：智能任务（KindAgent）按 cron 触发，回调里 Kind 保持 agent。
func TestScheduleAgentCron(t *testing.T) {
	var mu sync.Mutex
	fired := make([]Task, 0, 1)
	eng := NewEngine(func(t Task) {
		mu.Lock()
		fired = append(fired, t)
		mu.Unlock()
	})
	defer eng.Close()

	// @every 是 robfig/cron 描述符：心跳 interval 即用它实现。
	spec := "@every 1s"
	if _, err := cronParser.Parse(spec); err != nil {
		t.Fatalf("@every 应被 cron 解析器支持: %v", err)
	}
	tsk, err := eng.Schedule(Task{Kind: KindAgent, Heartbeat: true, Cron: spec, Text: "心跳巡检"})
	if err != nil {
		t.Fatalf("安排智能任务失败: %v", err)
	}
	if tsk.Kind != KindAgent {
		t.Fatalf("Kind 应保持 agent: %s", tsk.Kind)
	}
	if tsk.Next.IsZero() {
		t.Fatal("Next 应已填充")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) == 0 {
		t.Fatal("智能任务应在 1 秒内首次触发")
	}
	if fired[0].Kind != KindAgent || !fired[0].Heartbeat {
		t.Fatalf("回调收到的任务应为心跳智能任务: %+v", fired[0])
	}
}

// TestScheduleAgentOnce 验证智能任务单次触发（When 语义）。
func TestScheduleAgentOnce(t *testing.T) {
	var mu sync.Mutex
	fired := make([]Task, 0, 1)
	eng := NewEngine(func(t Task) {
		mu.Lock()
		fired = append(fired, t)
		mu.Unlock()
	})
	defer eng.Close()

	if _, err := eng.Schedule(Task{Kind: KindAgent, Text: "1 秒后查天气"}); err == nil {
		t.Fatal("agent 任务缺时间字段应报错")
	}
	when := time.Now().Add(200 * time.Millisecond)
	if _, err := eng.Schedule(Task{Kind: KindAgent, Text: "1 秒后查天气", When: when}); err != nil {
		t.Fatalf("安排失败: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0].Kind != KindAgent {
		t.Fatalf("应触发一次 agent 任务: %+v", fired)
	}
	if eng.Len() != 0 {
		t.Fatal("单次任务触发后应移除")
	}
}

// TestScheduleAgentPersistenceRoundTrip 验证 agent 任务落盘恢复后 Kind 不变。
func TestScheduleAgentPersistenceRoundTrip(t *testing.T) {
	path := t.TempDir() + "/tasks.json"
	eng := NewEngineFile(func(Task) {}, path)
	if _, err := eng.Schedule(Task{ID: "hb", Kind: KindAgent, Heartbeat: true, Cron: "0 8 * * *", Text: "心跳"}); err != nil {
		t.Fatalf("安排失败: %v", err)
	}
	eng.Close()

	eng2 := NewEngineFile(func(Task) {}, path)
	defer eng2.Close()
	list := eng2.List()
	if len(list) != 1 {
		t.Fatalf("恢复任务数不符: %+v", list)
	}
	got := list[0]
	if got.Kind != KindAgent || !got.Heartbeat || got.Cron != "0 8 * * *" {
		t.Fatalf("恢复后的任务字段不符: %+v", got)
	}
}
