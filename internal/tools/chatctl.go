package tools

import (
	"context"
	"sync/atomic"
)

// ChatController 承载"结束连续对话"信号：stop_listening 工具置位，
// 语音循环每轮回答后取走（读即清除），不影响下一次对话。
type ChatController struct {
	stop atomic.Bool
}

// Stop 置位结束信号。
func (c *ChatController) Stop() { c.stop.Store(true) }

// Taken 取走结束信号：返回当前是否置位并同时清除。
func (c *ChatController) Taken() bool { return c.stop.Swap(false) }

// StopChatTool 由 LLM 显式结束本轮连续对话。
type StopChatTool struct {
	Ctrl *ChatController
}

func (t *StopChatTool) Name() string { return "stop_listening" }

func (t *StopChatTool) Description() string {
	return "结束本轮连续对话，回到待机唤醒状态。仅当用户明确表示不需要继续时调用" +
		"（如\"不用了\"\"别说了\"\"就这样吧\"\"再见\"）；普通聊天或提出新请求时不要调用。"
}

func (t *StopChatTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}

func (t *StopChatTool) Call(ctx context.Context, _ map[string]any) (string, error) {
	t.Ctrl.Stop()
	return marshalJSON(map[string]string{"ok": "true", "stopped": "true"}), nil
}
