// options.go 是会话能力的归一化层：不同 agent 用不同机制回报可选配置——
// codex/zcode 走 session/new（load/resume 同）响应里的 configOptions，cc 只报
// 协议原生的 models 与 modes，zcode 的模式还双轨重复。归一化后统一成 Capability
// 清单透出：kind 是语义分类（model/thought_level/approval_mode/other），source
// 记录数据来源，切换时按 source 路由到 set_config_option / set_model / set_mode。
// 这是 ACP 的可选能力——agent 什么都不报时清单为空，前端按「无选择器」降级。
package acp

import (
	acp "github.com/coder/acp-go-sdk"
)

// 能力的语义分类（kind）。稳定词汇表，前端据此选图标与中文名。
const (
	CapKindModel        = "model"
	CapKindThoughtLevel = "thought_level"
	CapKindApprovalMode = "approval_mode"
	CapKindOther        = "other"
)

// 能力的数据来源（source），决定切换时的 SDK 调用路由。
const (
	CapSourceConfigOption = "config-option" // session/set_config_option
	CapSourceSessionModel = "session-model" // session/set_model
	CapSourceSessionMode  = "session-mode"  // session/set_mode
)

// SelectValue 是下拉项的一个取值。value 是切回 agent 时要原样回传的标识
// （zcode 桥编码为 provider\model），name 是给人看的展示名。
type SelectValue struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

// Capability 是归一化后透出给管理接口的一项会话能力。agent 不支持的能力
// 不出现在清单里（而非占位），前端据此隐藏对应 chip。
type Capability struct {
	ID           string        `json:"id"` // 稳定 id：已知 kind 用 kind 本身，other 用 "other:"+原始id
	Kind         string        `json:"kind"`
	Name         string        `json:"name"` // agent 原始名（other 类直接作为前端展示名）
	CurrentValue string        `json:"currentValue"`
	Values       []SelectValue `json:"values"`
	Source       string        `json:"source"`
}

// classifyConfigOption 按语义归类一个 config option：category 是 SDK 的官方
// 词汇（model/mode/thought_level）；id 兜底（有的 agent 只在 id 上表意）。
func classifyConfigOption(id string, category string) string {
	switch {
	case category == "model" || (category == "" && id == "model"):
		return CapKindModel
	case category == "thought_level":
		return CapKindThoughtLevel
	case category == "mode":
		return CapKindApprovalMode
	default:
		return CapKindOther
	}
}

// stableCapID 生成能力的稳定 id：已知 kind 全局唯一，直接用 kind；other 类
// 可能多项并存，拼上原始 id 防撞。
func stableCapID(kind, origID string) string {
	if kind == CapKindOther {
		return CapKindOther + ":" + origID
	}
	return kind
}

// convertConfigOption 把一个 select 形态的 config option 收敛成平铺 DTO。
// 分组形态（grouped）拍平，组名并进展示名——目前实际 agent（zcode 桥）只报平铺列表。
func convertConfigOption(sel *acp.SessionConfigOptionSelect) (string, Capability, bool) {
	if sel == nil {
		return "", Capability{}, false
	}
	cat := ""
	if sel.Category != nil {
		cat = string(*sel.Category)
	}
	kind := classifyConfigOption(string(sel.Id), cat)
	vals := make([]SelectValue, 0)
	if g := sel.Options.Grouped; g != nil {
		for _, grp := range *g {
			for _, v := range grp.Options {
				vals = append(vals, SelectValue{Value: string(v.Value), Name: grp.Name + " / " + v.Name})
			}
		}
	}
	if flat := sel.Options.Ungrouped; flat != nil {
		for _, v := range *flat {
			vals = append(vals, SelectValue{Value: string(v.Value), Name: v.Name})
		}
	}
	cap := Capability{
		ID:           stableCapID(kind, string(sel.Id)),
		Kind:         kind,
		Name:         sel.Name,
		CurrentValue: string(sel.CurrentValue),
		Values:       vals,
		Source:       CapSourceConfigOption,
	}
	return string(sel.Id), cap, true
}

// normalizeCapabilities 把三份原始状态归一成统一能力清单。去重规则：同一
// kind 先到先得（config-option 优先于协议原生，因为它已验证可热切且各 agent
// 词汇不同）；thought_level 只有 config-option 一个来源，agent 不报即缺失。
func normalizeCapabilities(opts []acp.SessionConfigOption,
	models *acp.SessionModelState, modeState *acp.SessionModeState) []Capability {

	out := make([]Capability, 0)
	seen := map[string]bool{}
	for _, o := range opts {
		_, cap, ok := convertConfigOption(o.Select)
		if !ok {
			continue // boolean 等非 select 形态不透出（SDK 里还是 UNSTABLE 且无 UI 需求）
		}
		if cap.Kind != CapKindOther && seen[cap.Kind] {
			continue
		}
		if cap.Kind != CapKindOther {
			seen[cap.Kind] = true
		}
		out = append(out, cap)
	}
	if !seen[CapKindModel] && models != nil && len(models.AvailableModels) > 0 {
		vals := make([]SelectValue, 0, len(models.AvailableModels))
		for _, m := range models.AvailableModels {
			vals = append(vals, SelectValue{Value: string(m.Id), Name: m.Name})
		}
		out = append(out, Capability{
			ID:           CapKindModel,
			Kind:         CapKindModel,
			Name:         "模型",
			CurrentValue: string(models.CurrentModelId),
			Values:       vals,
			Source:       CapSourceSessionModel,
		})
	}
	if !seen[CapKindApprovalMode] && modeState != nil && len(modeState.AvailableModes) > 0 {
		vals := make([]SelectValue, 0, len(modeState.AvailableModes))
		for _, m := range modeState.AvailableModes {
			vals = append(vals, SelectValue{Value: string(m.Id), Name: m.Name})
		}
		out = append(out, Capability{
			ID:           CapKindApprovalMode,
			Kind:         CapKindApprovalMode,
			Name:         "审批模式",
			CurrentValue: string(modeState.CurrentModeId),
			Values:       vals,
			Source:       CapSourceSessionMode,
		})
	}
	return out
}
