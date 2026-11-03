// Package skill 实现 Friday 的技能层：直接复用 vortexagent 的 SKILL.md 机制
// （frontmatter 的 name/description/model/allowed-tools/temperature/argument-hint
// 与正文指令），并在同一份文件里补上 Friday 需要的启用与触发元数据。
//
// 框架负责执行期的一切：<skill-instruction> 注入系统提示词、model/temperature 覆盖、
// allowed-tools 在"发给模型的工具声明"与"实际执行"两处过滤。本包解决框架没有的
// 三件事：目录发现与持久化、按消息选择技能、后台可见的错误解释，外加一项渲染决策——
// 正文超过 InlineBudget 时只给概览，由 read_skill 工具按需取全文（见 Overview）。
// 长度不再判无效：长技能照样可用，只是走概览通道。
package skill

import (
	"fmt"
	"path/filepath"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"gopkg.in/yaml.v3"
)

// 技能作用域（后台分组与文案用；谁生效由发现根的顺序决定，不看这个）。
const (
	ScopeProject = "project" // 项目级：Friday 数据目录的技能 + 工作目录里的 .agents/skills
	ScopeUser    = "user"    // 个人级：~/.agents/skills 与 <dataDir>/skills-user
)

// Root 是一个技能发现根。Store 按传入顺序扫描，越靠前优先级越高：同名技能里
// 靠后的那条不参与选择，并在后台写明"被谁覆盖"。
type Root struct {
	// Dir 是 <dir>/<技能目录名>/SKILL.md 的父目录；空串表示这个根不启用。
	Dir string `json:"dir"`
	// Scope 是后台分组标签（ScopeProject / ScopeUser）。
	Scope string `json:"scope"`
	// Source 是给人看的来源名（如「项目 .agents/skills」），标在后台每一行上。
	Source string `json:"source"`
	// Writable 表示这一根由 Friday 后台管理（可新建/可改/可删）。约定地址
	// （.agents/skills）属于外部工具与团队共享的文件，一律只读——不替用户改别人的仓库，
	// 也就不创建、不写 frontmatter。
	Writable bool `json:"writable"`
}

// UserScopeDir 是 user 级技能在数据目录下的目录名（内核 chdir 到数据目录后按相对路径使用）。
const UserScopeDir = "skills-user"

// InlineBudget 是"命中技能时整段注入正文"的字数上限。超过它的技能不整段塞进
// 系统提示词，而是只给概览（description + 技能目录 + 开头一小段），正文与引用
// 资料由模型用 read_skill 按需取——与 Anthropic Agent Skills、Cursor Rules
// agent-requested 那类渐进披露同一套思路：常驻只留索引，细节按需加载。
const InlineBudget = 4000

// overviewHeadRunes 是概览里附带的正文开头长度：够模型判断"要不要用它"即可。
const overviewHeadRunes = 600

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
	Source   string // 来自哪个发现根（后台展示）
	RootIdx  int    // 发现根的顺序：越小优先级越高，同名时靠后者被覆盖
	Writable bool   // 该根是否由后台管理（否则只读）
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

// Dir 返回技能目录（SKILL.md 的父目录）：read_skill 读引用资料时以它为根。
func (e *Entry) Dir() string {
	if e == nil || e.Path == "" {
		return ""
	}
	return filepath.Dir(e.Path)
}

// Overview 生成超预算技能的概要：说清它是谁、正文多长、怎么取全文与引用资料，
// 再附开头一段让模型判断要不要用。它替代整段正文进入系统提示词。
func (e *Entry) Overview() string {
	body := ""
	if e.Skill != nil {
		body = strings.TrimSpace(e.Skill.Content)
	}
	var b strings.Builder
	name := e.Name()
	b.WriteString("技能：" + name + "\n")
	if e.Skill != nil && strings.TrimSpace(e.Skill.Metadata.Description) != "" {
		b.WriteString(strings.TrimSpace(e.Skill.Metadata.Description) + "\n")
	}
	fmt.Fprintf(&b, "\n正文 %d 字，超过一次注入的预算（%d 字），这里只给开头一段。要按这份技能做事时：\n",
		len([]rune(body)), InlineBudget)
	fmt.Fprintf(&b, "- 取全文：read_skill({\"name\":\"%s\"})\n", name)
	fmt.Fprintf(&b, "- 引用资料：先 read_skill({\"name\":\"%s\",\"path\":\".\"}) 列目录，再读具体文件\n", name)
	b.WriteString("不要凭这段概览猜细节。\n\n开头一段：\n")
	if r := []rune(body); len(r) > overviewHeadRunes {
		b.WriteString(string(r[:overviewHeadRunes]) + "…（已截断）")
	} else {
		b.WriteString(body)
	}
	return b.String()
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
	Source       string   `json:"source,omitempty"` // 来自哪个发现根
	Path         string   `json:"path,omitempty"`   // 文件位置：多根之后"哪一条"要说得清
}

// Summarize 生成后台视图。
func (e *Entry) Summarize() Summary {
	s := Summary{ID: e.ID, Scope: e.Scope, Enabled: true, ParseErr: e.ParseErr,
		Note: e.Note, ReadOnly: !e.Writable, Source: e.Source, Path: e.Path}
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

// Validate 检查一份 SKILL.md 是否可作为后台技能落盘。
// 错误信息面向后台用户，说清"哪里不对、怎么改"。正文长度不设限：超预算的技能
// 走概览 + read_skill 通道，不再因为长而失效。
func Validate(raw string) error {
	metaText, _, ok := frontmatterParts(raw)
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
	var extra Extra
	if err := yaml.Unmarshal([]byte(metaText), &extra); err != nil {
		return fmt.Errorf("frontmatter 的 Friday 扩展键（enabled/auto/triggers/keywords/priority）格式不对: %w", err)
	}
	return nil
}
