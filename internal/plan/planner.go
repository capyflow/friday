// planner.go 跑"规划轮"：把一句诉求变成可审阅的步骤清单，写回仓库等待确认。
package plan

import (
	"context"
	"fmt"
	"log"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
)

// Asker 抽象对话能力（*agent.Brain 实现；测试可注入假实现）。
type Asker interface {
	AskWith(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta), opts ...agent.CallOpt) (string, error)
}

// plannerAllowedTools 是规划轮允许的工具：只读类，够模型查证事实，
// 不足以下手改数据——规划阶段就该只做规划。
var plannerAllowedTools = []string{
	"get_time", "list_todos", "get_weather", "list_files", "read_file", "recall", "search_history",
}

// draftInstruction 是内置"规划技能"的正文：定义产出格式与拆解要求。
const draftInstruction = `你现在处于"制定计划"阶段，只负责把用户诉求拆成可逐步执行的清单，不动手执行。

只输出一个 JSON 对象，不要代码围栏、不要任何解释文字，结构如下：
{
  "title": "一句话概括这件事（30 字内）",
  "summary": "整体思路与预期结果（可选，80 字内）",
  "steps": [
    {
      "title": "这一步做什么（20 字内）",
      "detail": "怎么做、需要哪些信息；这一步将独立执行，看不到当前聊天记录，请把必要的日期、城市、对象名等写清楚",
      "doneWhen": "满足什么条件算这一步完成",
      "tools": ["预计会用到的工具名，可选"]
    }
  ]
}

要求：
1. 步骤 1–%d 条，按执行顺序排列，每条都能独立判断成败；
2. 需要用户决策的地方写成明确的一步（例如"向用户确认预算区间"），不要自作主张；
3. 不要为了凑数拆分细碎动作，也不要在这一步真的去写文件、加待办、发通知。`

// Planner 把诉求变成待确认的计划。
type Planner struct {
	Ask           Asker
	Store         *Store
	MaxIterations int // 规划轮的工具轮数上限（0 = 用 Agent 默认）
}

// plannerSkill 是规划轮的内置技能：正文限定产出格式，allowed-tools 收窄工具面。
func plannerSkill() *vxagent.Skill {
	body := fmt.Sprintf(draftInstruction, maxSteps)
	return &vxagent.Skill{
		Metadata: vxagent.SkillMetadata{
			Name:         "__planning__",
			Description:  "制定计划（系统内置）",
			AllowedTools: plannerAllowedTools,
			Temperature:  temp(0.3),
		},
		Content: body,
		Scope:   "system",
	}
}

func temp(v float64) *float64 { return &v }

// Draft 跑规划轮。返回 (待确认的计划, 可直接回给用户的话, 错误)。
//
// 两次都解析不出合法清单时不制造"死卡片"：把模型说的话原样当普通回答返回，
// 同时留一条 failed 计划说明原因，用户知道发生了什么、也能在计划页看到。
func (p *Planner) Draft(ctx context.Context, goal, origin, skill string) (*Plan, string, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, "", fmt.Errorf("计划目标不能为空")
	}
	draft, err := p.Store.Create(goal, origin, skill)
	if err != nil {
		return nil, "", err
	}

	pctx := agent.WithSessionKey(ctx, draft.Session)
	pctx = agent.WithRun(pctx, "plan:"+draft.ID)
	opts := []agent.CallOpt{agent.WithJSONMode(), agent.WithInlineSkill(plannerSkill()), agent.WithoutAutoSkill()}
	if p.MaxIterations > 0 {
		opts = append(opts, agent.WithMaxIterations(p.MaxIterations))
	}

	answer, err := p.Ask.AskWith(pctx, goal, nil, nil, opts...)
	if err != nil {
		p.markFailed(draft.ID, "规划调用失败: "+err.Error())
		return nil, "", err
	}

	parsed, perr := Parse(goal, answer)
	if perr != nil {
		// 一次纠正重试：带上解析失败原因，让模型重新输出（上一轮的坏输出已在会话里）。
		corrected, aerr := p.Ask.AskWith(pctx, goal+CorrectionHint(perr), nil, nil, opts...)
		switch {
		case aerr != nil:
			perr = fmt.Errorf("%v（纠正重试也失败: %v）", perr, aerr)
		default:
			// 无论这次能否解析，都把模型最后说的话作为降级回答返回。
			answer = corrected
			if again, err2 := Parse(goal, corrected); err2 == nil {
				parsed, perr = again, nil
			} else {
				perr = err2
			}
		}
	}

	if parsed == nil {
		p.markFailed(draft.ID, "规划未产出可解析的步骤清单: "+perr.Error())
		return nil, answer, nil
	}

	updated, err := p.Store.Update(draft.ID, func(q *Plan) error {
		MergeInto(q, parsed)
		q.Origin = origin
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return updated, "", nil
}

func (p *Planner) markFailed(id, reason string) {
	if _, err := p.Store.Update(id, func(q *Plan) error {
		q.State = StateFailed
		q.Error = reason
		return nil
	}); err != nil {
		// 状态没落成不影响用户看到的降级回复，但必须留痕。
		log.Printf("[计划] %s 状态回写失败: %v", id, err)
	}
}
