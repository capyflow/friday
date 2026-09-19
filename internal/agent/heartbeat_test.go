package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/task"
)

// fakeDialogue 记录 prompt 并返回脚本化回答。
type fakeDialogue struct {
	mu      sync.Mutex
	prompts []string
	session []string // 每次提问的会话 key
	replies []string // 依次返回（越界取最后一个）
	err     error
}

func (f *fakeDialogue) Ask(ctx context.Context, text string, _ []llm.Content, _ func(llm.Delta)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	f.session = append(f.session, SessionKeyFrom(ctx))
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return "", nil
	}
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return r, nil
}

func (f *fakeDialogue) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

// fakeNotifier 捕获投递的消息。
type fakeNotifier struct {
	name  string
	mu    sync.Mutex
	sent  []string
	delay time.Duration
}

func (n *fakeNotifier) Name() string { return n.name }

func (n *fakeNotifier) Notify(_ context.Context, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, text)
	return nil
}

func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

func TestParseAgentOutput(t *testing.T) {
	cases := []struct {
		in   string
		body string
		push bool
		name string
	}{
		{"[推送]\n今天要吃药", "今天要吃药", true, "推送+换行正文"},
		{"[推送] 今天要吃药", "今天要吃药", true, "推送同行正文"},
		{"[静默]\n一切正常", "", false, "静默"},
		{"[静默]", "", false, "静默无正文"},
		{"直接说的话", "直接说的话", true, "缺协议按推送处理"},
		{"[推送]\n第一段\n第二段", "第一段\n第二段", true, "多行正文"},
	}
	for _, c := range cases {
		body, push := parseAgentOutput(c.in)
		if push != c.push || body != c.body {
			t.Errorf("%s: parseAgentOutput(%q) = (%q,%v), 期望 (%q,%v)",
				c.name, c.in, body, push, c.body, c.push)
		}
	}
}

func TestTaskRunnerOutputProtocol(t *testing.T) {
	d := &fakeDialogue{replies: []string{"[推送]\n该喝水了"}}
	console := &fakeNotifier{name: "console"}
	feishuN := &fakeNotifier{name: "feishu"}
	r := &TaskRunner{
		Ask:       d,
		Notifiers: []Notifier{console, feishuN},
		Now:       func() time.Time { return time.Date(2026, 9, 19, 10, 0, 0, 0, time.Local) },
	}
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "巡检"})
	if console.count() != 1 || feishuN.count() != 1 {
		t.Fatalf("[推送] 应投递全部通道（console=%d feishu=%d）", console.count(), feishuN.count())
	}
	if console.sent[0] != "该喝水了" {
		t.Fatalf("投递正文不符: %q", console.sent[0])
	}
	if got := d.lastPrompt(); !strings.Contains(got, "任务指令：巡检") {
		t.Fatalf("prompt 应包含任务指令: %q", got)
	}
	if got := d.session[0]; got != SessionTask {
		t.Fatalf("智能任务会话 key 应为 task，实际 %q", got)
	}

	// [静默]：不投递。
	d2 := &fakeDialogue{replies: []string{"[静默]\n一切正常"}}
	r2 := &TaskRunner{Ask: d2, Notifiers: []Notifier{console}, Now: r.Now}
	r2.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "巡检"})
	if console.count() != 1 { // 上例的 1 次 + 本例 0 次 = 1
		t.Fatalf("[静默] 不应投递")
	}
}

func TestTaskRunnerQuietHours(t *testing.T) {
	d := &fakeDialogue{replies: []string{"[推送]\n早上好"}}
	n := &fakeNotifier{name: "console"}
	r := &TaskRunner{
		Ask:        d,
		QuietHours: "23:00-07:00",
		Notifiers:  []Notifier{n},
		// 03:00 落在跨零点静默时段内
		Now: func() time.Time { return time.Date(2026, 9, 19, 3, 0, 0, 0, time.Local) },
	}
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "心跳"})
	if n.count() != 0 {
		t.Fatal("静默时段内不应推送")
	}

	// 非静默时段正常推送。
	r.Now = func() time.Time { return time.Date(2026, 9, 19, 9, 0, 0, 0, time.Local) }
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "心跳"})
	if n.count() != 1 {
		t.Fatal("非静默时段应推送")
	}
}

func TestTaskRunnerChannelFilter(t *testing.T) {
	d := &fakeDialogue{replies: []string{"[推送]\n定向消息"}}
	console := &fakeNotifier{name: "console"}
	feishuN := &fakeNotifier{name: "feishu"}
	r := &TaskRunner{Ask: d, Notifiers: []Notifier{console, feishuN}, Now: func() time.Time { return time.Now() }}
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "心跳", Channel: "feishu"})
	if console.count() != 0 || feishuN.count() != 1 {
		t.Fatalf("通道过滤不符（console=%d feishu=%d）", console.count(), feishuN.count())
	}
}

func TestTaskRunnerHeartbeatChecklist(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "HEARTBEAT.md")
	if err := os.WriteFile(file, []byte("- 检查服药提醒\n- 检查天气"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &fakeDialogue{replies: []string{"[静默]"}}
	r := &TaskRunner{Ask: d, HeartbeatFile: file, Now: func() time.Time { return time.Now() }}
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Heartbeat: true, Text: "心跳"})
	if got := d.lastPrompt(); !strings.Contains(got, "检查服药提醒") {
		t.Fatalf("心跳 prompt 应包含 HEARTBEAT.md 清单内容: %q", got)
	}
}

func TestTaskRunnerAskError(t *testing.T) {
	d := &fakeDialogue{err: errors.New("LLM 挂了")}
	n := &fakeNotifier{name: "console"}
	r := &TaskRunner{Ask: d, Notifiers: []Notifier{n}, Now: func() time.Time { return time.Now() }}
	r.Run(context.Background(), task.Task{Kind: task.KindAgent, Text: "心跳"})
	if n.count() != 0 {
		t.Fatal("执行失败时不应投递")
	}
}

func TestQuietHoursParse(t *testing.T) {
	r := &TaskRunner{}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 19, h, m, 0, 0, time.Local) }
	cases := []struct {
		spec   string
		t      time.Time
		expect bool
	}{
		{"23:00-07:00", at(3, 0), true},   // 跨零点：凌晨在内
		{"23:00-07:00", at(23, 30), true}, // 跨零点：深夜在内
		{"23:00-07:00", at(12, 0), false},
		{"12:00-14:00", at(13, 0), true}, // 不跨零点
		{"12:00-14:00", at(15, 0), false},
		{"bad", at(3, 0), false}, // 解析失败不静默
		{"", at(3, 0), false},    // 关闭
	}
	for _, c := range cases {
		r.QuietHours = c.spec
		if got := r.inQuiet(c.t); got != c.expect {
			t.Errorf("inQuiet(%q @ %02d:%02d) = %v, 期望 %v", c.spec, c.t.Hour(), c.t.Minute(), got, c.expect)
		}
	}
}
