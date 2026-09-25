package task

import (
	"context"

	"github.com/capyflow/vortexagent/autonomous"
)

// directExecutor 是交给框架的执行器：只负责"把目标变成一段结果"，不做任何
// 实际投递——普通提醒返回原文（零 token），智能任务同样只返回指令占位。
// 真正的投递与 LLM 执行统一延迟到框架的 OnGoalExecute 回调（task.go 的
// onFire）中异步进行：框架在该回调前已完成状态推进与落盘（先落盘后回调），
// 且 Execute 立即返回，框架主循环永不阻塞。
type directExecutor struct{}

func (directExecutor) Execute(_ context.Context, g *autonomous.Goal) (string, error) {
	return g.Snapshot().Description, nil
}
