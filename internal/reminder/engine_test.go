package reminder

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduleFires verifies 核心场景：注册提醒 → 到点触发回调。
func TestScheduleFires(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(r Reminder) { fired.Add(1) })
	defer eng.Close()

	id, err := eng.Schedule("", time.Now().Add(40*time.Millisecond), "该敷面膜了")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if id == "" {
		t.Fatal("id 为空")
	}

	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatalf("回调未触发，fired=%d", fired.Load())
	}
	if eng.Len() != 0 {
		t.Fatalf("触发后引擎应清空，Len=%d", eng.Len())
	}
}

// TestScheduleImmediate fires 已过期提醒应立刻触发（delay=0）。
func TestScheduleImmediate(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(r Reminder) { fired.Add(1) })
	defer eng.Close()

	if _, err := eng.Schedule("", time.Now().Add(-time.Second), "过期提醒"); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatalf("过期提醒应立刻触发，fired=%d", fired.Load())
	}
}

// TestCancelPreventsFire verifies 取消后不再触发。
func TestCancelPreventsFire(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(r Reminder) { fired.Add(1) })
	defer eng.Close()

	id, _ := eng.Schedule("", time.Now().Add(30*time.Millisecond), "不该触发")
	if !eng.Cancel(id) {
		t.Fatal("Cancel 应返回 true")
	}
	if eng.Cancel(id) {
		t.Fatal("重复 Cancel 应返回 false")
	}
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("已取消的提醒不应触发，fired=%d", fired.Load())
	}
}

// TestListAndSort verifies List 返回未触发提醒且按时间升序。
func TestListAndSort(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	eng.Schedule("b", time.Now().Add(2*time.Minute), "B")
	eng.Schedule("a", time.Now().Add(1*time.Minute), "A")

	rs := eng.List()
	if len(rs) != 2 {
		t.Fatalf("List 应返回 2 条，实际 %d", len(rs))
	}
	if rs[0].ID != "a" || rs[1].ID != "b" {
		t.Fatalf("应按时间升序，got %s,%s", rs[0].ID, rs[1].ID)
	}
}

// TestDuplicateID 重名 ID 应报错。
func TestDuplicateID(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()
	when := time.Now().Add(time.Minute)
	if _, err := eng.Schedule("x", when, "1"); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if _, err := eng.Schedule("x", when, "2"); err != ErrDuplicateID {
		t.Fatalf("重复注册应返回 ErrDuplicateID，got %v", err)
	}
}

// TestCloseRejects 关闭后 Schedule 应报错。
func TestCloseRejects(t *testing.T) {
	eng := NewEngine(nil)
	eng.Close()
	if _, err := eng.Schedule("", time.Now().Add(time.Minute), "x"); err != ErrClosed {
		t.Fatalf("关闭后注册应返回 ErrClosed，got %v", err)
	}
}

func TestParseHumanDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"10分钟", 10 * time.Minute, true},
		{"10 分钟", 10 * time.Minute, true},
		{"30秒", 30 * time.Second, true},
		{"2小时", 2 * time.Hour, true},
		{"1小时30分钟", 90 * time.Minute, true},
		{"1天", 24 * time.Hour, true},
		{"1h30m", 90 * time.Minute, true},
		{"90s", 90 * time.Second, true},
		{"5 minutes", 5 * time.Minute, true},
		{"乱七八糟", 0, false},
	}
	for _, c := range cases {
		got, err := ParseHumanDuration(c.in)
		if c.ok && err != nil {
			t.Errorf("%q 应解析成功，got err=%v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%q 应解析失败，got %v", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%q = %v, want %v", c.in, got, c.want)
		}
	}
}
