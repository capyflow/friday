package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/task"
)

func TestTaskManagementToolsFlow(t *testing.T) {
	e := task.NewReliableEngineFile(nil, filepath.Join(t.TempDir(), "tasks.json"))
	defer e.Close()
	ctx := context.Background()
	add := &ScheduleTaskTool{Engine: e}
	if _, err := add.Call(ctx, map[string]any{"text": "检查天气", "in": "1h", "times": float64(3), "agent": true, "channel": "web", "status": "paused"}); err != nil {
		t.Fatal(err)
	}
	old := e.List()[0]
	list, err := (&ListTasksTool{Engine: e}).Call(ctx, nil)
	if err != nil || !strings.Contains(list, "已暂停") || !strings.Contains(list, "agent") {
		t.Fatalf("列表状态或智能任务遗漏: %q %v", list, err)
	}
	res, err := (&UpdateTaskTool{Engine: e}).Call(ctx, map[string]any{"id": "#" + old.ID, "text": "新指令", "every": "0 8 * * *", "agent": true, "channel": "feishu", "todoId": "forged"})
	if err != nil || !strings.Contains(res, "已入队执行保持原记录") {
		t.Fatalf("更新工具: %q %v", res, err)
	}
	got, _ := e.Get(old.ID)
	if got.Status != task.StatusPaused || got.TodoID != "" || got.Kind != task.KindAgent || got.Channel != "feishu" || got.Cron != "0 8 * * *" {
		t.Fatalf("定义错误: %+v", got)
	}
	if _, err := (&ResumeTaskTool{Engine: e}).Call(ctx, map[string]any{"id": old.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(old.ID); got.Status != task.StatusActive {
		t.Fatalf("恢复失败: %+v", got)
	}
	if _, err := (&PauseTaskTool{Engine: e}).Call(ctx, map[string]any{"id": old.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(old.ID); got.Status != task.StatusPaused {
		t.Fatalf("暂停失败: %+v", got)
	}
}

func TestParseTaskArgsRejectsAmbiguousTimesAndAllowsAllAgentSchedules(t *testing.T) {
	for _, args := range []map[string]any{
		{"text": "x", "in": "1h", "when": time.Now().Format(time.RFC3339)},
		{"text": "x", "in": "1h", "times": 2.5},
		{"text": "x", "in": "1h", "channel": "unknown"},
		{"text": "x", "in": "1h", "status": "invalid"},
	} {
		if _, err := ParseTaskArgs(args); err == nil {
			t.Fatalf("非法定义应拒绝: %+v", args)
		}
	}
	for _, args := range []map[string]any{
		{"text": "x", "when": "2026-10-11T08:00:00.123456789+08:00", "agent": true},
		{"text": "x", "in": "1h", "times": float64(3), "agent": true},
		{"text": "x", "every": "0 8 * * *", "agent": true},
	} {
		got, err := ParseTaskArgs(args)
		if err != nil || got.Kind != task.KindAgent {
			t.Fatalf("智能定时路径: %+v %v", got, err)
		}
	}
}
