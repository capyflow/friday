package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduleOnceFires 核心场景：注册单次任务 → 到点触发回调。
func TestScheduleOnceFires(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(tk Task) { fired.Add(1) })
	defer eng.Close()

	tsk, err := eng.Schedule(Task{When: time.Now().Add(40 * time.Millisecond), Text: "该敷面膜了"})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if tsk.ID == "" {
		t.Fatal("id 为空")
	}
	if tsk.Kind != KindOnce {
		t.Fatalf("填 When 应推断为 once，got %s", tsk.Kind)
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

// TestScheduleImmediate 已过期任务应立刻触发（delay=0）。
func TestScheduleImmediate(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(tk Task) { fired.Add(1) })
	defer eng.Close()

	if _, err := eng.Schedule(Task{When: time.Now().Add(-time.Second), Text: "过期任务"}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatalf("过期任务应立刻触发，fired=%d", fired.Load())
	}
}

// TestCancelPreventsFire 取消后不再触发，重复取消返回 false。
func TestCancelPreventsFire(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(tk Task) { fired.Add(1) })
	defer eng.Close()

	tsk, _ := eng.Schedule(Task{When: time.Now().Add(30 * time.Millisecond), Text: "不该触发"})
	if !eng.Cancel(tsk.ID) {
		t.Fatal("Cancel 应返回 true")
	}
	if eng.Cancel(tsk.ID) {
		t.Fatal("重复 Cancel 应返回 false")
	}
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("已取消的任务不应触发，fired=%d", fired.Load())
	}
}

// TestRepeatFiresNTimes 多次任务应恰好触发 Times 次后自删，回调中剩余次数递减。
func TestRepeatFiresNTimes(t *testing.T) {
	var mu sync.Mutex
	var remaining []int
	eng := NewEngine(func(tk Task) {
		mu.Lock()
		remaining = append(remaining, tk.Remaining)
		mu.Unlock()
	})
	defer eng.Close()

	tsk, err := eng.Schedule(Task{Kind: KindRepeat, Interval: 40 * time.Millisecond, Times: 3, Text: "喝水"})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if tsk.Times != 3 || tsk.Remaining != 3 {
		t.Fatalf("Schedule 返回值应填 Times/Remaining=3，got %+v", tsk)
	}
	if tsk.Next.IsZero() {
		t.Fatal("Schedule 返回值应填 Next")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(remaining)
		mu.Unlock()
		if n == 3 && eng.Len() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	copyRem := append([]int(nil), remaining...)
	mu.Unlock()
	if len(copyRem) != 3 {
		t.Fatalf("应恰好触发 3 次，实际 %d 次（%v）", len(copyRem), copyRem)
	}
	if copyRem[0] != 2 || copyRem[1] != 1 || copyRem[2] != 0 {
		t.Fatalf("剩余次数应递减 2,1,0，got %v", copyRem)
	}

	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(remaining) != 3 {
		t.Fatalf("次数用尽后不应再触发，实际 %d 次", len(remaining))
	}
}

// TestRepeatCancelStopsFires 取消多次任务后不再继续触发。
func TestRepeatCancelStopsFires(t *testing.T) {
	var fired atomic.Int32
	eng := NewEngine(func(tk Task) { fired.Add(1) })
	defer eng.Close()

	tsk, err := eng.Schedule(Task{Kind: KindRepeat, Interval: 40 * time.Millisecond, Times: 100})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("repeat 任务未触发")
	}
	eng.Cancel(tsk.ID)
	// 取消后应停止触发：两个时间窗计数不变才算停（允许取消瞬间在途的最后一次）。
	time.Sleep(200 * time.Millisecond)
	n1 := fired.Load()
	time.Sleep(200 * time.Millisecond)
	if n2 := fired.Load(); n2 != n1 {
		t.Fatalf("取消后仍在触发：%d -> %d", n1, n2)
	}
}

// TestListSortedByNext List 应按下次触发时间升序，含混合任务类型。
func TestListSortedByNext(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	eng.Schedule(Task{ID: "b", Kind: KindOnce, When: time.Now().Add(2 * time.Minute), Text: "B"})
	eng.Schedule(Task{ID: "a", Kind: KindOnce, When: time.Now().Add(1 * time.Minute), Text: "A"})
	eng.Schedule(Task{ID: "c", Kind: KindRepeat, Interval: time.Hour, Times: 3, Text: "C"})

	tasks := eng.List()
	if len(tasks) != 3 {
		t.Fatalf("List 应返回 3 条，实际 %d", len(tasks))
	}
	if tasks[0].ID != "a" || tasks[1].ID != "b" || tasks[2].ID != "c" {
		t.Fatalf("应按 Next 升序，got %s,%s,%s", tasks[0].ID, tasks[1].ID, tasks[2].ID)
	}
	if tasks[2].Kind != KindRepeat || tasks[2].Remaining != 3 {
		t.Fatalf("repeat 任务应带 Remaining，got %+v", tasks[2])
	}
}

// TestCronRegistered 循环任务注册后应算出下一次触发时间。
func TestCronRegistered(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	// 不填 Kind，验证按 Cron 字段推断。
	tsk, err := eng.Schedule(Task{Cron: "0 8 * * *", Text: "提醒喝水"})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if tsk.Kind != KindCron {
		t.Fatalf("填 Cron 应推断为 cron，got %s", tsk.Kind)
	}
	now := time.Now()
	if tsk.Next.IsZero() || !tsk.Next.After(now) || tsk.Next.After(now.Add(24*time.Hour+time.Minute)) {
		t.Fatalf("Next 应为 24 小时内的下一个 8 点，got %v", tsk.Next)
	}
	if eng.Len() != 1 {
		t.Fatalf("应注册 1 条，实际 %d", eng.Len())
	}
	if !eng.Cancel(tsk.ID) {
		t.Fatal("循环任务应可取消")
	}
	if eng.Len() != 0 {
		t.Fatalf("取消后应为 0，实际 %d", eng.Len())
	}
}

// TestFireCronDispatch 循环任务触发路径：回调收到任务、条目常驻且 Next 推进。
func TestFireCronDispatch(t *testing.T) {
	var mu sync.Mutex
	fired := make([]Task, 0, 2)
	eng := NewEngine(func(tk Task) {
		mu.Lock()
		fired = append(fired, tk)
		mu.Unlock()
	})
	defer eng.Close()

	tsk, err := eng.Schedule(Task{Kind: KindCron, Cron: "@every 150ms", Text: "提醒喝水"})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) < 2 {
		t.Fatalf("循环任务应至少触发 2 次，实际 %d 次", len(fired))
	}
	first := fired[0]
	if first.Kind != KindCron || first.Text != "提醒喝水" || first.When.IsZero() {
		t.Fatalf("回调任务字段错误，got %+v", first)
	}
	listed := eng.List()
	if len(listed) != 1 {
		t.Fatalf("cron 任务触发后应常驻，实际 %d 条", len(listed))
	}
	if listed[0].Next.Before(tsk.Next) {
		t.Fatalf("触发后 Next 不应回退：before=%v after=%v", tsk.Next, listed[0].Next)
	}
}

// TestScheduleValidation 各类非法参数应返回对应错误。
func TestScheduleValidation(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()

	if _, err := eng.Schedule(Task{Kind: KindOnce}); err != ErrNoWhen {
		t.Fatalf("once 缺 When 应 ErrNoWhen，got %v", err)
	}
	if _, err := eng.Schedule(Task{Kind: KindRepeat, Times: 3}); err != ErrNoInterval {
		t.Fatalf("repeat 缺 Interval 应 ErrNoInterval，got %v", err)
	}
	if _, err := eng.Schedule(Task{Kind: KindRepeat, Interval: time.Minute, Times: 1}); err != ErrBadTimes {
		t.Fatalf("repeat times=1 应 ErrBadTimes，got %v", err)
	}
	if _, err := eng.Schedule(Task{Kind: KindCron, Cron: "不是 cron"}); err == nil {
		t.Fatal("非法 cron 应报错")
	}
	if _, err := eng.Schedule(Task{}); err == nil {
		t.Fatal("空任务应报错")
	}
}

// TestDuplicateID 重名 ID 应报错。
func TestDuplicateID(t *testing.T) {
	eng := NewEngine(nil)
	defer eng.Close()
	when := time.Now().Add(time.Minute)
	if _, err := eng.Schedule(Task{ID: "x", Kind: KindOnce, When: when, Text: "1"}); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if _, err := eng.Schedule(Task{ID: "x", Kind: KindOnce, When: when, Text: "2"}); err != ErrDuplicateID {
		t.Fatalf("重复注册应返回 ErrDuplicateID，got %v", err)
	}
}

// TestCloseRejects 关闭后 Schedule 应报错。
func TestCloseRejects(t *testing.T) {
	eng := NewEngine(nil)
	eng.Close()
	if _, err := eng.Schedule(Task{When: time.Now().Add(time.Minute), Text: "x"}); err != ErrClosed {
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

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "10分钟"},
		{90 * time.Minute, "1小时30分钟"},
		{24 * time.Hour, "1天"},
		{45 * time.Second, "45秒"},
		{0, "0s"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPersistRoundTrip 落盘 → 重启恢复：ID/类型/触发规则/剩余次数/创建时间均保留。
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	eng1 := NewEngineFile(nil, path)

	o, err := eng1.Schedule(Task{ID: "o", When: time.Now().Add(time.Hour), Text: "单次"})
	if err != nil {
		t.Fatalf("Schedule once: %v", err)
	}
	r, err := eng1.Schedule(Task{ID: "r", Kind: KindRepeat, Interval: time.Hour, Times: 5, Text: "多次"})
	if err != nil {
		t.Fatalf("Schedule repeat: %v", err)
	}
	if _, err := eng1.Schedule(Task{ID: "c", Kind: KindCron, Cron: "0 8 * * *", Text: "循环"}); err != nil {
		t.Fatalf("Schedule cron: %v", err)
	}
	eng1.Close()

	eng2 := NewEngineFile(nil, path)
	defer eng2.Close()

	byID := map[string]Task{}
	for _, tk := range eng2.List() {
		byID[tk.ID] = tk
	}
	if len(byID) != 3 {
		t.Fatalf("应恢复 3 条任务，实际 %d：%v", len(byID), byID)
	}

	if got := byID["o"]; got.Kind != KindOnce || !got.When.Equal(o.When) || !got.CreatedAt.Equal(o.CreatedAt) {
		t.Fatalf("单次任务恢复不符，got %+v want %+v", got, o)
	}
	if got := byID["r"]; got.Kind != KindRepeat || got.Interval != r.Interval ||
		got.Times != 5 || got.Remaining != 5 || !got.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("多次任务恢复不符，got %+v want %+v", got, r)
	}
	if got := byID["c"]; got.Kind != KindCron || got.Cron != "0 8 * * *" || got.Next.IsZero() {
		t.Fatalf("循环任务恢复不符，got %+v", got)
	}
}

// TestPersistFireUpdatesFile 触发后落盘同步更新：重启不会重复触发已触发的任务。
func TestPersistFireUpdatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	var fired atomic.Int32
	eng := NewEngineFile(func(tk Task) { fired.Add(1) }, path)

	if _, err := eng.Schedule(Task{ID: "once", When: time.Now().Add(30 * time.Millisecond), Text: "x"}); err != nil {
		t.Fatalf("Schedule once: %v", err)
	}
	if _, err := eng.Schedule(Task{ID: "rep", Kind: KindRepeat, Interval: 30 * time.Millisecond, Times: 2, Text: "y"}); err != nil {
		t.Fatalf("Schedule repeat: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for fired.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fired.Load(); n != 3 {
		t.Fatalf("应触发 1+2 次，实际 %d", n)
	}
	if eng.Len() != 0 {
		t.Fatalf("触发完毕引擎应清空，Len=%d", eng.Len())
	}

	// 落盘先于回调，回调计数到齐时文件必已写完：应为空列表。
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var stored []storedTask
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("解析文件: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("触发完毕后文件应为空，got %s", b)
	}

	// 重启后不应重复触发。
	fired.Store(0)
	eng2 := NewEngineFile(func(tk Task) { fired.Add(1) }, path)
	defer eng2.Close()
	time.Sleep(150 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Fatalf("重启后不应重复触发，实际 %d", n)
	}
}

// TestPersistRepeatRemaining 重启后多次任务应从剩余次数继续。
func TestPersistRepeatRemaining(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	var fired atomic.Int32
	eng := NewEngineFile(func(tk Task) { fired.Add(1) }, path)
	if _, err := eng.Schedule(Task{ID: "rep", Kind: KindRepeat, Interval: 40 * time.Millisecond, Times: 5, Text: "y"}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	eng.Close() // 剩余 3 次，快照落盘

	var stored []storedTask
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("解析文件: %v", err)
	}
	if len(stored) != 1 || stored[0].Remaining != 3 {
		t.Fatalf("文件应记录剩余 3 次，got %s", b)
	}

	eng2 := NewEngineFile(func(tk Task) { fired.Add(1) }, path)
	defer eng2.Close()
	deadline = time.Now().Add(3 * time.Second)
	for fired.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fired.Load(); n != 5 {
		t.Fatalf("重启后应再触发 3 次（共 5），实际 %d", n)
	}
	time.Sleep(150 * time.Millisecond)
	if n := fired.Load(); n != 5 {
		t.Fatalf("次数用尽后不应再触发，实际 %d", n)
	}
}

// TestPersistOverdueOnceFiresImmediately 恢复时已过期的单次任务立即补触发一次。
func TestPersistOverdueOnceFiresImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	raw := fmt.Sprintf(`[{"id":"late","text":"补触发","kind":"once","when":%q,"createdAt":%q}]`,
		time.Now().Add(-time.Minute).Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	var fired atomic.Int32
	eng := NewEngineFile(func(tk Task) { fired.Add(1) }, path)
	defer eng.Close()

	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fired.Load(); n != 1 {
		t.Fatalf("过期任务应立即补触发一次，fired=%d", n)
	}
}

// TestPersistCorruptFile 文件损坏时保留原文件，拒绝注册避免覆盖用户数据。
func TestPersistCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte("{{{不是 JSON"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	eng := NewEngineFile(nil, path)
	defer eng.Close()
	if eng.Len() != 0 {
		t.Fatalf("损坏文件应空启动，实际 %d 条", eng.Len())
	}
	if _, err := eng.Schedule(Task{When: time.Now().Add(time.Minute), Text: "x"}); err == nil {
		t.Fatal("损坏文件必须拒绝注册")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{{{不是 JSON" {
		t.Fatalf("必须保留原文件: bytes=%q err=%v", b, err)
	}
}

// TestPersistConcurrentSaves 并发注册/取消下的落盘必须串行化：注册、取消与
// 框架推进可能同时写盘，未串行化时两个"写 tmp + rename"会互踩导致 rename
// 失败（回归测试）。
func TestPersistConcurrentSaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	eng := NewEngineFile(nil, path)
	defer eng.Close()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			if _, err := eng.Schedule(Task{ID: id, When: time.Now().Add(time.Hour), Text: "x"}); err != nil {
				t.Errorf("Schedule %s: %v", id, err)
			}
			if i%2 == 0 {
				eng.Cancel(id)
			}
		}(i)
	}
	wg.Wait()

	if s := buf.String(); strings.Contains(s, "持久化失败") {
		t.Fatalf("并发落盘不应失败：%s", s)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var stored []storedTask
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("文件应为合法 JSON: %v (%s)", err, b)
	}
	if len(stored) != 25 {
		t.Fatalf("取消一半后应剩 25 条，实际 %d (%s)", len(stored), b)
	}
}
