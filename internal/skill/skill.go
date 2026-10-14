// Package skill 实现 Friday 的技能层：直接复用 vortexagent 的 SKILL.md 机制
// （frontmatter 的 name/description/model/allowed-tools/temperature/argument-hint
// 与正文指令），并在同一份文件里补上 Friday 需要的启用与触发元数据。
//
// 框架负责执行期的一切：<skill-instruction> 注入系统提示词、model/temperature 覆盖、
// allowed-tools 在"发给模型的工具声明"与"实际执行"两处过滤。本包只解决框架没有的
// 三件事：目录发现与持久化、按消息选择技能、后台可见的错误解释。
package skill

import (
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"gopkg.in/yaml.v3"
)

// 技能作用域。
const (
	ScopeProject = "project" // <dataDir>/skills/<id>/SKILL.md，后台可写
	ScopeUser    = "user"    // $HOME/.vortex/skills，与 vortexagent CLI 共享，只读
)

// maxBodyRunes 限制技能正文长度：正文会作为 <skill-instruction> 进系统提示词，
// 过长的技能会让每轮对话都变贵。
const maxBodyRunes = 8000

// Extra 是 Friday 写在同一份 frontmatter 里的扩展键。框架的 SkillMetadata 未开启
// yaml.KnownFields，未知键天然被忽略，因此两种解释器共用一个文件不冲突。
type Extra struct {
	// Enabled 缺省（未写）视为启用——"文件在就生效"与记忆页/任务文件一致。
	Enabled *bool `yaml:"enabled"`
	// Auto 控制该技能是否参与自动匹配。缺省为 false：自动触发是 opt-in。
	Auto *bool `yaml:"auto"`
	// Triggers 精确触发词：消息等于它，或以"它 + 空格"开头（同自定义指令的形状）。
	Triggers []string `yaml:"triggers"`
	// Keywords 自动匹配的加权词（比 name/description 命中权重更高）。
	Keywords []string `yaml:"keywords"`
	// Priority 同分时靠后排序用（大者优先）。
	Priority int `yaml:"priority"`
}

// EnabledOrDefault 返回是否启用（未写视为启用）。
func (e Extra) EnabledOrDefault() bool { return e.Enabled == nil || *e.Enabled }

// AutoOrDefault 返回是否参与自动匹配（未写视为不自动）。
func (e Extra) AutoOrDefault() bool { return e.Auto != nil && *e.Auto }

// Entry 是一个已加载的技能。ParseErr 非空表示该文件无效：保留原文件、
// 后台展示原因、不参与选择（框架的 Discover 会静默跳过坏文件，这里不学它）。
type Entry struct {
	ID       string
	Scope    string
	Path     string
	Skill    *vxagent.Skill
	Extra    Extra
	ParseErr string
	// Note 是非致命的排除原因（如"同名技能已被项目目录版本覆盖"）：
	// 后台要能解释这条为什么不生效，而不是静默消失。
	Note string

	// MatchedBy 记录本次命中的依据（触发词/命中 token），供轨迹与"思考过程"展示。
	MatchedBy []string
}

// Name 返回技能名（未成功解析时为空串）。
func (e *Entry) Name() string {
	if e == nil || e.Skill == nil {
		return ""
	}
	return strings.TrimSpace(e.Skill.Metadata.Name)
}

// Usable 表示该技能可被注入本轮对话。
func (e *Entry) Usable() bool {
	if e == nil || e.ParseErr != "" || e.Note != "" || e.Skill == nil {
		return false
	}
	return e.Name() != "" && e.Extra.EnabledOrDefault()
}

// Candidate 表示该技能可被自动匹配命中（还需 Usable 且显式 opt-in）。
func (e *Entry) Candidate() bool {
	return e.Usable() && e.Extra.AutoOrDefault()
}

// Summary 是后台列表用的一行视图。
type Summary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Scope        string   `json:"scope"`
	Model        string   `json:"model,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
	ArgumentHint string   `json:"argumentHint,omitempty"`
	Enabled      bool     `json:"enabled"`
	Auto         bool     `json:"auto"`
	Triggers     []string `json:"triggers,omitempty"`
	Keywords     []string `json:"keywords,omitempty"`
	Priority     int      `json:"priority"`
	ParseErr     string   `json:"parseError,omitempty"`
	Note         string   `json:"note,omitempty"`
	ReadOnly     bool     `json:"readOnly"`
}

// Summarize 生成后台视图。
func (e *Entry) Summarize() Summary {
	s := Summary{ID: e.ID, Scope: e.Scope, Enabled: true, ParseErr: e.ParseErr,
		Note: e.Note, ReadOnly: e.Scope == ScopeUser}
	if e.Skill != nil {
		m := e.Skill.Metadata
		s.Name, s.Description = m.Name, m.Description
		s.Model, s.ArgumentHint = m.Model, m.ArgumentHint
		s.AllowedTools = append([]string(nil), m.AllowedTools...)
	}
	s.Enabled = e.Extra.EnabledOrDefault()
	s.Auto = e.Extra.AutoOrDefault()
	s.Triggers = append([]string(nil), e.Extra.Triggers...)
	s.Keywords = append([]string(nil), e.Extra.Keywords...)
	s.Priority = e.Extra.Priority
	return s
}

// frontmatterParts 把 SKILL.md 拆成 (frontmatter YAML, 正文)。
//
// 定界按行判定（起止各是一行独立的 ---），比框架那套 SplitN("---", 3) 更严：
// 值里带 "---"（如 note: a---b）会被 SplitN 误当结尾而静默截断正文，这里让
// Validate 明确拒绝它，保证两个解释器看到的是同一份内容。
func frontmatterParts(content string) (meta string, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", content, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"),
				strings.TrimSpace(strings.Join(lines[i+1:], "\n")), true
		}
	}
	return "", content, false // 未闭合
}

// Validate 检查一份 SKILL.md 是否可作为项目技能落盘。
// 错误信息面向后台用户，说清"哪里不对、怎么改"。
func Validate(raw string) error {
	metaText, body, ok := frontmatterParts(raw)
	if !ok {
		return fmt.Errorf("技能文件需要以 --- 开头、并以单独一行 --- 结束的 frontmatter")
	}
	if strings.Contains(metaText, "---") {
		// 框架用 SplitN(content,"---",3) 切分，frontmatter 内出现 --- 会被当成结尾截断。
		return fmt.Errorf("frontmatter 里不能出现 --- 分隔线，会被当成结尾")
	}
	var framework map[string]any
	if err := yaml.Unmarshal([]byte(metaText), &framework); err != nil {
		return fmt.Errorf("frontmatter 不是合法 YAML: %w", err)
	}
	name, _ := framework["name"].(string)
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("frontmatter 缺少 name（技能名，对话里用它点名）")
	}
	if len([]rune(body)) > maxBodyRunes {
		return fmt.Errorf("技能正文 %d 字，超过 %d 字上限——正文会整段进系统提示词，太长会让每轮对话都变贵",
			len([]rune(body)), maxBodyRunes)
	}
	var extra Extra
	if err := yaml.Unmarshal([]byte(metaText), &extra); err != nil {
		return fmt.Errorf("frontmatter 的 Friday 扩展键（enabled/auto/triggers/keywords/priority）格式不对: %w", err)
	}
	return nil
}
