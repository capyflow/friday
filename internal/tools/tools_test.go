package tools

import (
	"context"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

// fakeSpeaker 用于测试 speak/display 工具。
type fakeSpeaker struct {
	texts []string
}

func (f *fakeSpeaker) Speak(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}

// TestScheduleTaskOnce 验证相对时长路径：in → 单次任务。
func TestScheduleTaskOnce(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleTaskTool{Engine: eng}
	res, err := tool.Call(context.Background(), map[string]any{
		"text": "该敷面膜了",
		"in":   "50ms",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("返回为空")
	}
	if eng.Len() != 1 {
		t.Fatalf("应注册 1 条任务，实际 %d", eng.Len())
	}
	tsk := eng.List()[0]
	if tsk.Kind != task.KindOnce {
		t.Fatalf("应为 once，got %s", tsk.Kind)
	}
}

// TestScheduleTaskRepeat in + times → 多次任务。
func TestScheduleTaskRepeat(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleTaskTool{Engine: eng}
	if _, err := tool.Call(context.Background(), map[string]any{
		"text":  "提醒喝水",
		"in":    "10分钟",
		"times": float64(3),
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	tasks := eng.List()
	if len(tasks) != 1 || tasks[0].Kind != task.KindRepeat || tasks[0].Times != 3 {
		t.Fatalf("应注册一条 3 次的 repeat 任务，got %+v", tasks)
	}
	if tasks[0].Interval != 10*time.Minute {
		t.Fatalf("间隔应为 10 分钟，got %v", tasks[0].Interval)
	}
}

// TestScheduleTaskCron every → 循环任务。
func TestScheduleTaskCron(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleTaskTool{Engine: eng}
	res, err := tool.Call(context.Background(), map[string]any{
		"text":  "提醒喝水",
		"every": "每天早上8点",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("返回为空")
	}
	tasks := eng.List()
	if len(tasks) != 1 || tasks[0].Kind != task.KindCron || tasks[0].Cron != "0 8 * * *" {
		t.Fatalf("应注册一条 cron=0 8 * * * 任务，got %+v", tasks)
	}
	if tasks[0].Next.IsZero() {
		t.Fatal("循环任务应算出下次触发时间")
	}
}

// TestScheduleTaskWhen 绝对时间路径：when → 单次任务。
func TestScheduleTaskWhen(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleTaskTool{Engine: eng}
	if _, err := tool.Call(context.Background(), map[string]any{
		"text": "叫我起床",
		"when": time.Now().Add(time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if eng.Len() != 1 {
		t.Fatalf("应注册 1 条任务，实际 %d", eng.Len())
	}
}

// TestScheduleTaskInvalid 各类非法参数应报错且不注册。
func TestScheduleTaskInvalid(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()
	tool := &ScheduleTaskTool{Engine: eng}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"缺少 text", map[string]any{"in": "10分钟"}},
		{"非法时长", map[string]any{"text": "x", "in": "完全不是时长"}},
		{"缺少时间参数", map[string]any{"text": "x"}},
		{"非法 RFC3339", map[string]any{"text": "x", "when": "明天早上8点"}},
		{"times 缺少 in", map[string]any{"text": "x", "times": float64(3)}},
		{"非法 every", map[string]any{"text": "x", "every": "每小时"}},
	}
	for _, c := range cases {
		if _, err := tool.Call(context.Background(), c.args); err == nil {
			t.Errorf("%s：应报错", c.name)
		}
	}
	if eng.Len() != 0 {
		t.Fatalf("非法参数不应注册任务，实际 %d", eng.Len())
	}
}

// TestListAndCancel 验证列出与取消。
func TestListAndCancel(t *testing.T) {
	eng := task.NewEngine(nil)
	defer eng.Close()

	tsk, err := eng.Schedule(task.Task{When: time.Now().Add(time.Hour), Text: "测试"})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	list := &ListTasksTool{Engine: eng}
	res, err := list.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res) == 0 || len(res) > 500 {
		t.Fatalf("List 返回异常: %q", res)
	}

	cancel := &CancelTaskTool{Engine: eng}
	out, err := cancel.Call(context.Background(), map[string]any{"id": tsk.ID})
	if err != nil || len(out) == 0 {
		t.Fatalf("Cancel: %v %q", err, out)
	}
	if eng.Len() != 0 {
		t.Fatalf("取消后应为 0，实际 %d", eng.Len())
	}

	missing, err := cancel.Call(context.Background(), map[string]any{"id": "nope"})
	if err != nil || len(missing) == 0 {
		t.Fatalf("取消不存在的任务应友好返回: %v %q", err, missing)
	}
}

// TestGetTime 验证时间工具返回可解析的 RFC3339。
func TestGetTime(t *testing.T) {
	tool := &GetTimeTool{}
	res, err := tool.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, res); err != nil {
		t.Fatalf("应返回 RFC3339，got %q: %v", res, err)
	}
}

// TestSpeakAndDisplay 验证播报/显示工具调用对应输出。
func TestSpeakAndDisplay(t *testing.T) {
	sp := &fakeSpeaker{}
	st := &SpeakNowTool{Speaker: sp}
	if _, err := st.Call(context.Background(), map[string]any{"text": "该敷面膜了"}); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if len(sp.texts) != 1 || sp.texts[0] != "该敷面膜了" {
		t.Fatalf("Speak 未调用底层，got %v", sp.texts)
	}

	dis := &fakeDisplayer{}
	dt := &DisplayNowTool{Displayer: dis}
	if _, err := dt.Call(context.Background(), map[string]any{"text": "该敷面膜了"}); err != nil {
		t.Fatalf("Display: %v", err)
	}
	if len(dis.texts) != 1 || dis.texts[0] != "该敷面膜了" {
		t.Fatalf("Display 未调用底层，got %v", dis.texts)
	}
}

type fakeDisplayer struct {
	texts []string
}

func (f *fakeDisplayer) Display(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}

func TestParseHumanCron(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"每天早上8点", "0 8 * * *", true},
		{"每天早上8:30", "30 8 * * *", true},
		{"每周一九点", "0 9 * * 1", true},
		{"每周一三五9点", "0 9 * * 1,3,5", true},
		{"每天晚上十一点", "0 23 * * *", true},
		{"每天十五点", "0 15 * * *", true},
		{"0 8 * * *", "0 8 * * *", true},
		{"每天23点59分", "59 23 * * *", true},
		{"随便说点什么", "", false},
		{"每小时", "", false},
	}
	for _, c := range cases {
		got, err := ParseHumanCron(c.in)
		if c.ok && err != nil {
			t.Errorf("%q 应解析成功，got err=%v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%q 应解析失败，got %q", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%q = %q, want %q", c.in, got, c.want)
		}
	}
}
