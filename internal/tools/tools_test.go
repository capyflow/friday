package tools

import (
	"context"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/reminder"
)

// fakeSpeaker 用于测试 speak/display 工具。
type fakeSpeaker struct {
	texts []string
}

func (f *fakeSpeaker) Speak(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}

// TestScheduleReminderCall 验证相对时长路径：in=50ms → 引擎注册一条提醒。
func TestScheduleReminderCall(t *testing.T) {
	eng := reminder.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleReminderTool{Engine: eng}
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
		t.Fatalf("应注册 1 条提醒，实际 %d", eng.Len())
	}
}

// TestScheduleReminderCallInvalidIn 非法时长应报错且不注册。
func TestScheduleReminderCallInvalidIn(t *testing.T) {
	eng := reminder.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleReminderTool{Engine: eng}
	if _, err := tool.Call(context.Background(), map[string]any{
		"text": "x",
		"in":   "完全不是时长",
	}); err == nil {
		t.Fatal("非法时长应报错")
	}
	if eng.Len() != 0 {
		t.Fatalf("不应注册提醒，实际 %d", eng.Len())
	}
}

// TestScheduleReminderCallMissing 缺少 in/when 应报错。
func TestScheduleReminderCallMissing(t *testing.T) {
	eng := reminder.NewEngine(nil)
	defer eng.Close()
	tool := &ScheduleReminderTool{Engine: eng}
	if _, err := tool.Call(context.Background(), map[string]any{"text": "x"}); err == nil {
		t.Fatal("缺少 in/when 应报错")
	}
}

// TestListAndCancel 验证列出与取消。
func TestListAndCancel(t *testing.T) {
	eng := reminder.NewEngine(nil)
	defer eng.Close()

	sched := &ScheduleReminderTool{Engine: eng}
	id, err := sched.Engine.Schedule("", time.Now().Add(time.Hour), "测试")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	list := &ListRemindersTool{Engine: eng}
	res, err := list.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res) == 0 || len(res) > 500 {
		t.Fatalf("List 返回异常: %q", res)
	}

	cancel := &CancelReminderTool{Engine: eng}
	out, err := cancel.Call(context.Background(), map[string]any{"id": id})
	if err != nil || len(out) == 0 {
		t.Fatalf("Cancel: %v %q", err, out)
	}
	if eng.Len() != 0 {
		t.Fatalf("取消后应为 0，实际 %d", eng.Len())
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

// TestScheduleRecurring 验证循环任务注册与列表。
func TestScheduleRecurring(t *testing.T) {
	eng := reminder.NewEngine(nil)
	defer eng.Close()

	tool := &ScheduleRecurringTool{Engine: eng}
	res, err := tool.Call(context.Background(), map[string]any{
		"text": "提醒喝水",
		"when": "每天早上8点",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("返回为空")
	}

	list := &ListRecurringTool{Engine: eng}
	out, err := list.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("List 返回为空")
	}
}
