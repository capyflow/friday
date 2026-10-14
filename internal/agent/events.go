// events.go 是对话中间过程事件（思考轮次 / 工具调用）的按请求订阅 hub：
// 生命周期钩子（robot.go buildHooks）在每次轮次与工具调用时发布事件，
// 网页 SSE 处理器按请求 ID 订阅，把"思考过程"实时推给前端展示（zcode 风）。
// 事件只进内存，无人订阅即丢弃，不影响对话主链路。
package agent

import (
	"context"
	"sync"

	"github.com/capyflow/friday/internal/logx"
)

// Step 是一条中间过程事件。
type Step struct {
	Kind   string `json:"kind"`             // round（LLM 轮次）| tool（工具调用）
	Name   string `json:"name,omitempty"`   // 工具名（kind=tool）
	Detail string `json:"detail,omitempty"` // 摘要（命令/参数/结果片段）
	Status string `json:"status,omitempty"` // run | ok | fail（kind=tool）
}

// stepTaps 按请求 ID 保存订阅者；发布为非阻塞广播（订阅端缓冲满即丢弃该条）。
var (
	stepMu   sync.Mutex
	stepTaps = map[string][]chan Step{}
)

// SubscribeSteps 订阅指定请求 ID 的过程事件。返回停止函数（移除订阅；
// 通道不会关闭，停止后不再有写入）。
func SubscribeSteps(reqID string) (<-chan Step, func()) {
	ch := make(chan Step, 64)
	stepMu.Lock()
	stepTaps[reqID] = append(stepTaps[reqID], ch)
	stepMu.Unlock()
	stop := func() {
		stepMu.Lock()
		taps := stepTaps[reqID]
		for i, c := range taps {
			if c == ch {
				stepTaps[reqID] = append(taps[:i], taps[i+1:]...)
				break
			}
		}
		if len(stepTaps[reqID]) == 0 {
			delete(stepTaps, reqID)
		}
		stepMu.Unlock()
	}
	return ch, stop
}

// PublishStep 发布一条过程事件（按 ctx 中的请求 ID 路由；无 ID 或无人订阅时丢弃）。
func PublishStep(ctx context.Context, s Step) {
	id := logx.IDFrom(ctx)
	if id == "" {
		return
	}
	stepMu.Lock()
	defer stepMu.Unlock()
	for _, ch := range stepTaps[id] {
		select {
		case ch <- s:
		default: // 订阅端消费不过来就丢事件，绝不阻塞对话
		}
	}
}
