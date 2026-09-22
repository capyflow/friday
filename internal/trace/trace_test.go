package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/logx"
)

// TestPersistenceRoundTrip 验证轨迹落盘与重启恢复：
// 记录若干调用 → 等防抖落盘 → 用同一文件重新 Init → 数据完整恢复。
func TestPersistenceRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "traces.json")

	Init(file)
	ctx := logx.WithID(context.Background(), "req-1")
	Begin("req-1", "web", "web", "帮我看看天气")
	RecordLLMCall(ctx, 1, `{"messages":[]}`, 100, 20, 500)
	RecordLLMMessage(ctx, `{"text":"今天晴"}`)
	Record(ctx, "answer", "今天天气晴朗")
	End("req-1", "")

	Begin("req-2", "task", "task", "心跳巡检")
	End("req-2", "超时失败")

	waitFlush(t, file)

	// 模拟重启：同一路径重新 Init。
	Init(file)
	got, ok := Get("req-1")
	if !ok {
		t.Fatal("重启后应能恢复 req-1")
	}
	if got.Source != "web" || got.Session != "web" || got.Input != "帮我看看天气" {
		t.Fatalf("req-1 基本字段不符: %+v", got)
	}
	if got.Error != "" || got.Answer != "今天天气晴朗" {
		t.Fatalf("req-1 answer/error 不符: %+v", got)
	}
	if got.In != 100 || got.Out != 20 {
		t.Fatalf("req-1 token 用量不符: %d/%d", got.In, got.Out)
	}
	if len(got.Entries) != 3 { // llm_in / llm_out / answer
		t.Fatalf("req-1 应有 3 条事件，实际 %d", len(got.Entries))
	}
	for _, e := range got.Entries {
		if e.Kind == "" || e.Time.IsZero() {
			t.Fatalf("事件字段不完整: %+v", e)
		}
	}
	if got.Entries[1].Ms != 500 || got.Entries[1].In != 100 || got.Entries[1].Out != 20 {
		t.Fatalf("llm_out 事件应带回 token 用量与耗时: %+v", got.Entries[1])
	}

	got2, ok := Get("req-2")
	if !ok {
		t.Fatal("重启后应能恢复 req-2")
	}
	if got2.Error != "超时失败" {
		t.Fatalf("req-2 错误信息不符: %+v", got2)
	}

	list := List()
	if len(list) != 2 {
		t.Fatalf("重启后 List 应有 2 条，实际 %d", len(list))
	}
	if list[0].ID != "req-2" || list[1].ID != "req-1" {
		t.Fatalf("List 应按最新在前: %s, %s", list[0].ID, list[1].ID)
	}
}

// TestLoadSkipsBrokenEntries 损坏文件应从空开始，不 panic。
func TestLoadSkipsBrokenFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(file, []byte("{不是JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	Init(file)
	Begin("x", "web", "web", "hi")
	if _, ok := Get("x"); !ok {
		t.Fatal("损坏文件后应能正常记录新轨迹")
	}
}

// TestNoAutoEviction 轨迹跟随对话历史：不再自动淘汰，超出旧上限仍保留。
func TestNoAutoEviction(t *testing.T) {
	Init("")
	for i := 0; i < 210; i++ { // 超过旧的 200 条上限
		Begin("boot-"+string(rune('a'+i%26))+"-"+time.Now().Format("150405.000000000"), "web", "web", "x")
	}
	if n := len(List()); n < 210 {
		t.Fatalf("轨迹应跟随对话历史不自动淘汰，实际仅 %d 条", n)
	}
	// 清除联动：ClearSession 只清指定会话。
	if n := ClearSession("web"); n < 210 {
		t.Fatalf("ClearSession 应清除全部 web 轨迹，实际 %d", n)
	}
	if left := len(List()); left != 0 {
		t.Fatalf("清除后应为空，实际 %d", left)
	}
}

// waitFlush 等待防抖落盘完成（文件出现且大小稳定）。
func waitFlush(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(file); err == nil && st.Size() > 0 {
			time.Sleep(flushDelay) // 再等一个窗口确保重命名完成
			if _, err := os.Stat(file); err == nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("落盘超时：文件未生成")
}
