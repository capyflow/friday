package plan

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/capyflow/friday/internal/workdir"
)

// create_plan 工具：让 agent 自己列一份步骤清单并按步执行。
//
// 为什么 agent 需要这个：网页那条计划链路是"用户点计划 → 规划轮 → 网页确认 →
// 执行"，适合用户自己想好的活儿。工作区里的活儿常常是 agent 自己判断出来的
// （"这活儿得分几步做"），再让它绕回网页点一次纯属绕路——所以这里给 agent 一个
// 直接立计划的入口：列步骤 → 立刻按步执行，每步仍然是一条独立轨迹、一份独立成本，
// 用户在「自动化 → 计划」页随时能看进度、暂停、取消。
//
// 执行仍然受既有的安全边界管：每一步里的写文件/命令照旧走审批队列与权限层，
// 计划本身不会绕过任何一道闸。
type CreatePlanTool struct {
	mu    sync.RWMutex
	store *Store
	start func(id string) error
}

// NewCreatePlanTool 创建 create_plan 工具。仓库与执行器在启动装配的后段才有，
// 所以接线用 SetStore / SetStarter 补（与内置 agent 接线同一套做法）。
func NewCreatePlanTool() *CreatePlanTool { return &CreatePlanTool{} }

// SetStore 接上计划仓库。
func (t *CreatePlanTool) SetStore(s *Store) {
	t.mu.Lock()
	t.store = s
	t.mu.Unlock()
}

// SetStarter 接上执行器（Runner.Start）。
func (t *CreatePlanTool) SetStarter(fn func(id string) error) {
	t.mu.Lock()
	t.start = fn
	t.mu.Unlock()
}

func (t *CreatePlanTool) Name() string { return "create_plan" }

func (t *CreatePlanTool) Description() string {
	return "把一个复杂任务拆成步骤清单并按步执行：每步是一次独立提问、一条独立轨迹，" +
		"步骤之间可以暂停/重试/跳过。适合「要改好几处、每处都得验证」这种多步活儿——" +
		"一次性说完容易漏，中途断了也没法接上。步骤要写清每步做什么、做完的标准。" +
		"默认立完就开始执行（auto_start=true）；传 auto_start=false 则只立计划，等你确认。" +
		"执行期间在「自动化 → 计划」页能看进度、暂停或取消。"
}

func (t *CreatePlanTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "这次要完成的事（一句话，用户视角的目标）",
			},
			"steps": map[string]any{
				"type":        "array",
				"description": "步骤清单，按执行顺序",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":    map[string]any{"type": "string", "description": "这一步做什么（一句话）"},
						"detail":   map[string]any{"type": "string", "description": "这一步的具体做法与注意事项"},
						"doneWhen": map[string]any{"type": "string", "description": "做完的标准（怎么算完成）"},
					},
					"required": []string{"title"},
				},
			},
			"auto_start": map[string]any{
				"type":        "boolean",
				"description": "立完计划是否立刻开始执行，默认 true",
			},
		},
		"required": []string{"goal", "steps"},
	}
}

func (t *CreatePlanTool) Call(ctx context.Context, args map[string]any) (string, error) {
	t.mu.RLock()
	store, starter := t.store, t.start
	t.mu.RUnlock()
	if store == nil {
		return "", fmt.Errorf("计划功能未启用（内部装配缺失，需重启 Friday）")
	}
	goal, _ := args["goal"].(string)
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", fmt.Errorf("缺少参数 goal（这次要完成的事）")
	}
	rawSteps, _ := args["steps"].([]any)
	if len(rawSteps) == 0 {
		return "", fmt.Errorf("缺少参数 steps（步骤清单，至少一条）")
	}
	if len(rawSteps) > maxSteps {
		return "", fmt.Errorf("步骤 %d 条，超过上限 %d 条；拆成多份计划", len(rawSteps), maxSteps)
	}

	steps := make([]Step, 0, len(rawSteps))
	for i, item := range rawSteps {
		m, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("steps[%d] 不是对象（应为 {title, detail, doneWhen}）", i)
		}
		title, _ := m["title"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			return "", fmt.Errorf("steps[%d] 缺少 title", i)
		}
		detail, _ := m["detail"].(string)
		doneWhen, _ := m["doneWhen"].(string)
		steps = append(steps, Step{
			N: i + 1, Title: title,
			Detail: strings.TrimSpace(detail), DoneWhen: strings.TrimSpace(doneWhen),
			State: StepPending,
		})
	}

	draft, err := store.Create(goal, "agent", "")
	if err != nil {
		return "", fmt.Errorf("创建计划失败: %w", err)
	}
	dir := workdir.From(ctx)
	updated, err := store.Update(draft.ID, func(q *Plan) error {
		q.Steps = steps
		q.Title = clip(goal, 60)
		q.Dir = dir
		q.State = StateAwaiting
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("写入计划步骤失败: %w", err)
	}

	autoStart := true
	if v, ok := args["auto_start"].(bool); ok {
		autoStart = v
	}
	if autoStart {
		if starter == nil {
			return "", fmt.Errorf("计划执行器未装配（内部装配缺失，需重启 Friday）")
		}
		if err := starter(updated.ID); err != nil {
			return "", fmt.Errorf("计划已创建（%s）但启动失败: %w（可在「自动化 → 计划」页手动开始）", updated.ID, err)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "已创建计划 %s（%d 步）并开始执行：\n", updated.ID, len(updated.Steps))
		for _, st := range updated.Steps {
			fmt.Fprintf(&b, "%d. %s\n", st.N, st.Title)
		}
		b.WriteString("进度可在「自动化 → 计划」页查看，随时能暂停或取消。")
		return b.String(), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "已创建计划 %s（%d 步，等你确认后执行）：\n", updated.ID, len(updated.Steps))
	for _, st := range updated.Steps {
		fmt.Fprintf(&b, "%d. %s\n", st.N, st.Title)
	}
	b.WriteString("在「自动化 → 计划」页确认后开始执行。")
	return b.String(), nil
}
