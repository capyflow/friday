package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// 子任务派发（delegate_task）：把一个独立的小任务派给一条隔离子会话。
//
// 为什么需要它：内置 agent 单线程串行地干所有事——一个大需求往往是"摸清 A、
// 再看 B、顺手确认 C"，串着做就是三次等待。派子任务让这些调查并行发生，主会话
// 只收结论，上下文也不会被中间过程塞满。
//
// 边界写死三条：
//  1. 子会话看不到主对话，只有你给的那一条指令（外加项目上下文）——所以指令要自足；
//  2. 子任务默认只读（检索、读文件、跑只读命令），改文件的事留给主会话做，避免
//     多个子任务同时改一个项目时互相覆盖；
//  3. 子任务不能再派子任务（嵌套上限 1）——否则递归派发能把预算烧光。

// maxDelegateDepth 是子任务嵌套层数上限：主会话 0 层，子任务 1 层，子任务里再派就超限。
const maxDelegateDepth = 1

// delegateSeq 给每次派发一个自增序号：子会话 key 要唯一（否则两次派发会串上下文），
// 但也不能用时间戳（可读性差且持久层文件名不友好）。
var delegateSeq atomic.Int64

// ctxDelegateDepth 是「当前处在第几层子任务」的 ctx 标记。
type ctxDelegateDepth struct{}

// WithDelegateDepth 把子任务嵌套层数挂进 ctx。
func WithDelegateDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, ctxDelegateDepth{}, depth)
}

// DelegateDepth 取出当前嵌套层数（没挂过就是 0，即主会话）。
func DelegateDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if d, ok := ctx.Value(ctxDelegateDepth{}).(int); ok {
		return d
	}
	return 0
}

// DelegateRunner 是"能派子任务"的能力接口（*Brain 满足它）。
// 工具只认这个接口，方便测试注入假实现。
type DelegateRunner interface {
	Delegate(ctx context.Context, task string) (string, error)
}

// DelegateTool 是 delegate_task 工具：把一段自足的任务描述派给子会话，拿回结论。
type DelegateTool struct {
	mu     sync.RWMutex
	runner DelegateRunner
}

// NewDelegateTool 创建 delegate_task 工具。接线在 Brain 建好之后用 SetRunner 补上
// （Agent 必须先有工具注册表，Brain 又得包着 Agent——顺序上只能后接）。
func NewDelegateTool() *DelegateTool { return &DelegateTool{} }

// SetRunner 接上真正的派发实现（通常是 *Brain.Delegate）。
func (t *DelegateTool) SetRunner(r DelegateRunner) {
	t.mu.Lock()
	t.runner = r
	t.mu.Unlock()
}

func (t *DelegateTool) Name() string { return "delegate_task" }

func (t *DelegateTool) Description() string {
	return "派一个子任务出去：在独立上下文里做调查/检索/读码这类子活儿，把结论带回来。" +
		"适合\"摸清某块代码\"\"同时查几处\"这类能并行的事。子任务看不到主对话，" +
		"只有你写的这条指令（要写自足，别写\"刚才那个文件\"）；它默认只读" +
		"（可检索、可读文件、可跑只读命令），不会改文件——要改由你自己动手。" +
		"子任务不能再派子任务。"
}

func (t *DelegateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "交给子任务的具体活儿，要自足（子任务看不到主对话）",
			},
		},
		"required": []string{"task"},
	}
}

func (t *DelegateTool) Call(ctx context.Context, args map[string]any) (string, error) {
	task, _ := args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("缺少参数 task（要派给子任务的具体活儿）")
	}
	if depth := DelegateDepth(ctx); depth >= maxDelegateDepth {
		return "", fmt.Errorf("子任务不能再派子任务（嵌套上限 %d 层）", maxDelegateDepth)
	}
	t.mu.RLock()
	runner := t.runner
	t.mu.RUnlock()
	if runner == nil {
		return "", fmt.Errorf("子任务派发未接入对话大脑（内部装配缺失，需重启 Friday）")
	}
	answer, err := runner.Delegate(ctx, task)
	if err != nil {
		return "", fmt.Errorf("子任务失败: %w", err)
	}
	if strings.TrimSpace(answer) == "" {
		return "子任务跑完了，但没给出结论（多半是它什么都没找到）", nil
	}
	return answer, nil
}

// delegatePrompt 给子会话的首条指令：交代身份、边界与交付格式。
// 语气与边界都在这里写死——子会话看不到主对话，只能靠这段话知道自己该怎么干。
func delegatePrompt(task string) string {
	return fmt.Sprintf("【子任务】%s\n\n"+
		"你是一个子任务执行者：这是别人派给你的一个独立小任务，你看不到主对话，只有这一条指令与项目上下文。\n"+
		"- 只做被要求的事，做完用一段话给结论（关键信息、文件路径、行号、命令的真实输出），不要寒暄、不要复述这条指令。\n"+
		"- 默认只读：找代码用 search_code，看文件用 read_file，看仓库现状用 git_status / git_diff；除非任务明确要求改动，否则不要写文件、也不要跑有副作用的命令。\n"+
		"- 找不到就说找不到，不要编；不确定的地方写明不确定。", task)
}

// Delegate 在一条隔离子会话里跑一个子任务，把最终结论交回调用方。
//
// 子会话 key 与主会话隔离（sub-<父key>-<序号>）：上下文不互相污染，两次派发也不串。
// 跑完立刻清掉这条子会话——它是一次性的，留在 session.json 里只会越积越多。
func (b *Brain) Delegate(ctx context.Context, task string) (string, error) {
	parent := CurrentKey(SessionKeyFrom(ctx))
	key := fmt.Sprintf("sub-%s-%d", parent, delegateSeq.Add(1))
	dctx := WithSessionKey(ctx, key)
	dctx = WithDelegateDepth(dctx, DelegateDepth(ctx)+1)

	answer, err := b.AskWith(dctx, delegatePrompt(task), nil, nil)

	// 子会话是一次性的：跑完清掉（会话 + 上下文引擎归档）。失败也要清，
	// 否则半路崩掉的子会话会一直挂在持久层里。
	if rerr := b.Reset(key); rerr != nil {
		log.Printf("[子任务] 清理子会话 %s 失败: %v", key, rerr)
	}
	return answer, err
}
