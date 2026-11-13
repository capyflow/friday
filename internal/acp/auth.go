// auth.go 是 agent 的登录态探测与认证执行（设置 → 智能体 → 认证）。
//
// 为什么要有这一层：agent 装好之后能不能干活，取决于它自己的登录态（ChatGPT
// 账号、Google 账号、API key…）。这份状态既不在 config.yaml 里，也不在 Friday
// 的会话层里——以前只有「测试」按钮失败时，报错末尾才附一句"去终端登录"的
// 指引，属于事后诸葛亮：人要先踩一次坑，才知道这台 agent 还没登录。
//
// 这里把登录态做成可主动查询的状态，并按 ACP 规范（initialize 响应里的
// authMethods）把认证真正做掉。三种类型分别对应三种做法：
//
//   - agent 型：调 agent 自己的 authenticate 方法（它内部弹登录页或校验凭据）；
//   - env_var 型：用户填 key，写进该 agent 的环境变量（落 config.yaml 的
//     acp.agents[].env），随进程注入；
//   - terminal 型：需要交互式 TUI 登录，给出可执行的命令让人在终端里跑。
//
// 另有一条兜底：CLI 类 agent 往往不声明 authMethods（ACP 不管凭据），登录态
// 只体现在 HOME 下的凭据文件里（codex 的 auth.json、gemini 的
// oauth_creds.json…）。这类按经验路径探测，配合已知的登录命令表。
package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

// 登录态三态：已登录 / 未登录 / 判不出来。
// 「判不出来」是正经结论，不是失败：agent 不声明认证方式、凭据也不在已知
// 路径上时，硬说"未登录"会让人去折腾本来正常的配置。
const (
	AuthStateLoggedIn  = "logged_in"
	AuthStateLoggedOut = "logged_out"
	AuthStateUnknown   = "unknown"
)

const (
	// maxAuthProbeMethods 是探测时最多试几个认证方式。声明了多个方式的 agent
	// （codebuddy 有四个）逐个试一遍既慢又多半失败，试前几个够定性了。
	maxAuthProbeMethods = 3
	// sessionProbeTimeout 是「建一次会话」试探的等待上限。
	sessionProbeTimeout = 25 * time.Second
	// authActionTimeout 是用户主动点「认证」时的等待上限。ACP 的 authenticate
	// 通常是同步的（agent 自己开浏览器，URL 放在 _meta 里，客户端去开），
	// 等更久没有意义——真要等用户操作的，agent 也不会吭声，那种情况留给
	// 「去终端登录」这条路。
	authActionTimeout = 15 * time.Second
)

// authProbeTimeout 是探测时 authenticate 的等待上限：只是问一句"有登录态吗"。
// 做成变量是为了让测试不用真的等这么久。
var authProbeTimeout = 5 * time.Second

// AuthVarView 是环境变量型认证要用户填的一个变量。
type AuthVarView struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
}

// AuthMethodView 是一种认证方式（按 ACP 的三类归一）。
type AuthMethodView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"` // agent | env_var | terminal
	Description string            `json:"description,omitempty"`
	Link        string            `json:"link,omitempty"`
	Vars        []AuthVarView     `json:"vars,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// AuthStatus 是某个 agent 的登录态快照。
type AuthStatus struct {
	Agent   string           `json:"agent"`
	State   string           `json:"state"`
	Message string           `json:"message,omitempty"`
	Methods []AuthMethodView `json:"methods,omitempty"`
	Hint    string           `json:"hint,omitempty"`
	Command string           `json:"command,omitempty"`
	Creds   []string         `json:"creds,omitempty"`
	// SuggestedVars 是这台 agent 推荐的环境变量名（agent 没声明 env_var
	// 认证方式时的兜底入口：填进去落到 acp.agents[].env，随进程注入）。
	SuggestedVars []string `json:"suggestedVars,omitempty"`
	// Link 是取凭据的地址（API Key 管理页等），面板里给一个直达链接。
	Link string `json:"link,omitempty"`
	// Follow 是登录时启动命令之后还要补敲的行（如 codebuddy 的 /login）。
	Follow []string `json:"follow,omitempty"`
}

// AuthResult 是一次认证动作的结果。
type AuthResult struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

// loginGuide 是某个 agent 的登录指引：一句人话 + 一条可直接执行的命令 +
// 推荐的环境变量名。
type loginGuide struct {
	hint    string
	command string
	// vars 是这台 agent 认的环境变量名（agent 没声明 env_var 认证方式时的
	// 兜底入口：填进去落到 acp.agents[].env，随进程注入）。
	vars []string
	// link 是取凭据的地址（如 API Key 管理页），面板里给一个直达链接。
	link string
	// follow 是启动交互式命令后还要补敲的行（如 codebuddy 的 /login）。
	// 参数形式 `codebuddy /login` 不认斜杠命令（实测仍报未登录），只有进入
	// 交互界面后再敲 /login 才有效——所以这里带上 follow，让终端页依次敲。
	follow []string
}

// loginGuides 是按 agent 名的登录指引（命令优先用配置名，其次用命令基名兜底）。
// 凭据都在 HOME 下（C13），无 TTY 的服务端最容易断在这里。
var loginGuides = map[string]loginGuide{
	"codex":    {hint: "在终端运行 `codex login` 完成 ChatGPT 登录", command: "codex login", link: "https://chatgpt.com/codex/settings"},
	"claude":   {hint: "在终端运行 `claude` 并执行 /login，或在环境变量里配 ANTHROPIC_API_KEY", command: "claude", vars: []string{"ANTHROPIC_API_KEY"}, link: "https://console.anthropic.com/settings/keys"},
	"gemini":   {hint: "在终端运行一次 `gemini` 完成 Google 登录，或在环境变量里配 GEMINI_API_KEY", command: "gemini", vars: []string{"GEMINI_API_KEY"}, link: "https://aistudio.google.com/apikey"},
	"opencode": {hint: "在终端运行 `opencode auth login` 配置模型渠道", command: "opencode auth login"},
	"zcode":    {hint: "复用 ~/.zcode 登录态；缺失时在终端运行 `zcode` 登录一次", command: "zcode"},
	// codebuddy 声明了四种登录方式（iOA / Google-Github / 微信 / 企业域），
	// 由 agent 自己的 authenticate 处理；但它只认三种凭据：环境变量里的
	// CODEBUDDY_API_KEY / CODEBUDDY_AUTH_TOKEN、settings.json 里的
	// apiKeyHelper，或 CLI 交互登录（凭据进 macOS 钥匙串）。**网页端登录它
	// 读不到**——网页的登录态不是 CLI 的凭据。所以这里把两种环境变量直接
	// 推荐出来，让人填进 agent 的环境变量里。
	"codebuddy":      {hint: "网页版的登录态不与 CLI 共享：CLI 要单独登录一次——在终端里跑 `codebuddy` 后敲 /login，走浏览器授权（凭据进 macOS 钥匙串，之后 Friday 拉起 agent 就能用到）；或把 API Key 填在下面", command: "codebuddy", follow: []string{"/login"}, vars: []string{"CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN"}, link: "https://tencent.sso.copilot.tencent.com/profile/keys"},
	"codebuddy-code": {hint: "网页版的登录态不与 CLI 共享：CLI 要单独登录一次——在终端里跑 `codebuddy` 后敲 /login，走浏览器授权（凭据进 macOS 钥匙串，之后 Friday 拉起 agent 就能用到）；或把 API Key 填在下面", command: "codebuddy", follow: []string{"/login"}, vars: []string{"CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN"}, link: "https://tencent.sso.copilot.tencent.com/profile/keys"},
}

// loginGuideFor 给出该 agent 的登录指引：配置名优先，命令基名兜底。
func loginGuideFor(cfg config.AcpAgentConfig) loginGuide {
	for _, key := range []string{strings.ToLower(cfg.Name), strings.ToLower(filepath.Base(cfg.Command))} {
		if g, ok := loginGuides[key]; ok {
			return g
		}
	}
	return loginGuide{
		hint:    "在该 agent 的官方 CLI 完成一次登录（凭据通常落在 HOME 下）",
		command: filepath.Base(cfg.Command),
	}
}

// credFiles 是 CLI 类 agent 的登录凭据落点（相对 HOME）。文件存在即认为登录过。
var credFiles = map[string][]string{
	"codex":    {".codex/auth.json"},
	"claude":   {".claude/.credentials.json", ".claude.json"},
	"gemini":   {".gemini/oauth_creds.json"},
	"opencode": {".local/share/opencode/auth.json", ".config/opencode/auth.json"},
	"zcode":    {".zcode/auth.json"},
}

// probeCredFiles 列出本机存在的登录凭据文件（显示成 ~/.codex/auth.json 这样）。
// 找不到 HOME 或文件不在，返回空——判不出就判不出，不编。
func probeCredFiles(cfg config.AcpAgentConfig) []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var rels []string
	for _, key := range []string{strings.ToLower(cfg.Name), strings.ToLower(filepath.Base(cfg.Command))} {
		if files, ok := credFiles[key]; ok {
			rels = files
			break
		}
	}
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		if st, err := os.Stat(filepath.Join(home, rel)); err == nil && !st.IsDir() {
			out = append(out, "~/"+rel)
		}
	}
	return out
}

// AuthStatus 取某个 agent 的登录态快照（设置页「认证」面板）。
func (m *Manager) AuthStatus(ctx context.Context, name string) (AuthStatus, error) {
	c := m.cfgFn().Normalized()
	agentCfg, ok := c.Agent(name)
	if !ok {
		return AuthStatus{}, fmt.Errorf("没有叫 %q 的智能体", name)
	}
	st := AuthStatus{Agent: name, State: AuthStateUnknown, Methods: []AuthMethodView{}}
	if agentCfg.Builtin {
		st.State = AuthStateLoggedIn
		st.Message = "内置 Friday 不需要单独登录：模型密钥在「设置 → 模型服务」里配置。"
		return st, nil
	}
	guide := loginGuideFor(agentCfg)
	st.Hint = guide.hint
	st.Command = guide.command
	st.SuggestedVars = guide.vars
	st.Link = guide.link
	st.Follow = guide.follow
	st.Creds = probeCredFiles(agentCfg)

	err := m.withProbe(agentCfg, func(proc *agentProc) error {
		st.Methods = authMethodViews(proc.authMethods)
		st.State, st.Message = probeLoginState(ctx, proc, agentCfg, st.Creds, guide.vars)
		return nil
	})
	if err != nil {
		// 拉不起来时不能一口咬定"未登录"：命令写错、包没装、PATH 变了都会走到这里。
		// 只有报错本身就指向登录态（stderr 命中登录/401 关键词）时才判未登录，
		// 其余说"判不出来"并把原因与凭据线索摆出来。
		if strings.Contains(err.Error(), "疑似未登录") {
			st.State = AuthStateLoggedOut
			st.Message = err.Error()
			return st, nil
		}
		st.State = AuthStateUnknown
		st.Message = "拉起失败，暂时判不出登录态：" + err.Error()
		if len(st.Creds) > 0 {
			st.Message += "（检测到登录凭据 " + strings.Join(st.Creds, "、") + "，登录态本身可能没问题）"
		}
		return st, nil
	}
	return st, nil
}

// withProbe 起一条短命连接做探测用：拉起 → initialize → 用完即回收。
// 探测用的工作目录是临时目录，用完就删（不在数据目录里留痕）。
func (m *Manager) withProbe(agentCfg config.AcpAgentConfig, fn func(*agentProc) error) error {
	ws, err := os.MkdirTemp("", "friday-acp-auth-*")
	if err != nil {
		return err
	}
	proc, err := m.ctor(agentCfg)(agentCfg, ws, m.handshakeBudget(agentCfg), nil, nil)
	if err != nil {
		os.RemoveAll(ws)
		return err
	}
	defer func() {
		proc.kill()
		os.RemoveAll(ws)
	}()
	return fn(proc)
}

// probeLoginState 判定登录态。
//
// 顺序是有讲究的：先用本地的、瞬时的信号（agent 声明要的环境变量填了没、
// HOME 下有没有凭据文件），再问 agent 自己。为什么不反过来——authenticate
// 可能阻塞（等浏览器里点确认）甚至当场拉起登录流程，探测阶段不该有副作用；
// 而且声明了多个方式的 agent（codebuddy 有 iOA / Google / 微信 / 企业域四个）
// 逐个调一遍既慢又多半失败。最后才用 authenticate 兜底判定。
func probeLoginState(ctx context.Context, proc *agentProc, agentCfg config.AcpAgentConfig, creds []string, suggested []string) (string, string) {
	methods := normalizeAuthMethods(proc.authMethods)
	// 环境变量型：配置里已经填了这些变量就算配好了。
	for _, am := range methods {
		if am.view.Type != "env_var" || am.raw.EnvVar == nil {
			continue
		}
		if len(missingEnvVars(agentCfg, am.raw.EnvVar.Vars)) == 0 {
			return AuthStateLoggedIn, "已配置 " + envVarNames(am.raw.EnvVar.Vars)
		}
	}
	// 推荐的环境变量填了就算配好了（codebuddy 这类只认环境变量的 agent）。
	if names := filledEnvVars(agentCfg, suggested); len(names) > 0 {
		return AuthStateLoggedIn, "已配置 " + strings.Join(names, "、")
	}
	// 没声明认证方式的老 CLI：凭据落在 HOME 下（ACP 不管凭据这件事）。
	if len(creds) > 0 {
		return AuthStateLoggedIn, "检测到登录凭据：" + strings.Join(creds, "、")
	}
	// 配置里手填过密钥也算已配好。
	if names := configuredKeyVars(agentCfg); len(names) > 0 {
		return AuthStateLoggedIn, "已配置密钥：" + strings.Join(names, "、")
	}
	// 前面都没命中，才问 agent 自己：逐个试它声明的方式（最多试几个）。
	var lastErr string
	sawTimeout := false
	tried := 0
	for _, am := range methods {
		if am.view.Type != "agent" {
			continue
		}
		if tried >= maxAuthProbeMethods {
			break
		}
		tried++
		actx, cancel := context.WithTimeout(ctx, authProbeTimeout)
		_, err := proc.conn.Authenticate(actx, acp.AuthenticateRequest{MethodId: am.view.ID})
		timedOut := actx.Err() != nil
		cancel()
		if err == nil {
			return AuthStateLoggedIn, "认证通过（" + am.view.Name + "）"
		}
		if timedOut {
			// agent 没回应认证请求：多半是它正等着人完成登录（codebuddy 就是
			// 这样——authenticate 收下不吭声）。这不是"未登录"的证据，别硬判。
			sawTimeout = true
			continue
		}
		lastErr = err.Error()
	}
	// authenticate 没给出结论，就用「建一次会话」再问一遍——这是最硬的证据：
	// 能建出会话说明凭据真的可用。codebuddy 这类把凭据放钥匙串的 agent 读不到
	// 文件，但建会话时它自己会用（实测未登录时它回 Authentication required）。
	if state, msg, ok := probeSession(ctx, proc); ok {
		return state, msg
	}
	if lastErr != "" {
		return AuthStateLoggedOut, "未登录：" + lastErr
	}
	if sawTimeout {
		return AuthStateUnknown, "agent 没有回应认证请求（它可能在等你完成登录）——按下方方式登录后重新检测一次"
	}
	if len(proc.authMethods) == 0 {
		return AuthStateUnknown, "该 agent 没有声明认证方式，无法自动判定登录态（可按下方指引在终端里登录一次）"
	}
	return AuthStateLoggedOut, "尚未登录"
}

// probeSession 用「建一次会话」问 agent 自己登录态：能建出会话 = 凭据可用；
// 报鉴权错 = 没登录；其它错误（建会话本身的问题）不算结论。
//
// 这是最可靠的一招：凭据放钥匙串里的 agent（codebuddy）读不到文件，但真要
// 建会话它必须自己有凭据——所以 agent 的回答就是权威答案。
func probeSession(ctx context.Context, proc *agentProc) (string, string, bool) {
	sctx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	_, err := proc.conn.NewSession(sctx, acp.NewSessionRequest{
		Cwd:        os.TempDir(),
		McpServers: []acp.McpServer{},
	})
	if err == nil {
		return AuthStateLoggedIn, "已登录：agent 能正常建立会话（凭据可用）", true
	}
	if looksLikeAuthProblem(err.Error()) {
		return AuthStateLoggedOut, "未登录：" + err.Error(), true
	}
	return "", "", false
}

// Authenticate 按 agent 声明的方式做一次认证。env 是本次注入的环境变量
// （env_var 型：调用方已把它写进 config.yaml，这里再随进程带上）。
func (m *Manager) Authenticate(ctx context.Context, name, methodID string, env map[string]string) (AuthResult, error) {
	c := m.cfgFn().Normalized()
	agentCfg, ok := c.Agent(name)
	if !ok {
		return AuthResult{}, fmt.Errorf("没有叫 %q 的智能体", name)
	}
	if agentCfg.Builtin {
		return AuthResult{}, errors.New("内置 Friday 不需要单独登录：模型密钥在「设置 → 模型服务」里配置")
	}
	if len(env) > 0 {
		merged := make(map[string]string, len(agentCfg.Env)+len(env))
		for k, v := range agentCfg.Env {
			merged[k] = v
		}
		for k, v := range env {
			merged[k] = v
		}
		agentCfg.Env = merged
	}
	var out AuthResult
	err := m.withProbe(agentCfg, func(proc *agentProc) error {
		am, ok := findAuthMethod(proc.authMethods, methodID)
		if !ok {
			return fmt.Errorf("该 agent 没有 id 为 %q 的认证方式（它声明的是：%s）",
				methodID, authMethodIDs(proc.authMethods))
		}
		if am.view.Type == "terminal" {
			// terminal 型要的是交互式 TUI：进程内跑不出人机交互，只能给命令。
			return fmt.Errorf("这个认证方式需要交互式登录，请在终端里运行：%s",
				terminalAuthCommand(agentCfg, am.raw))
		}
		actx, cancel := context.WithTimeout(ctx, authActionTimeout)
		defer cancel()
		if _, err := proc.conn.Authenticate(actx, acp.AuthenticateRequest{MethodId: methodID}); err != nil {
			out.State = AuthStateLoggedOut
			msg := err.Error()
			switch {
			case actx.Err() != nil:
				// agent 收下请求却不吭声（codebuddy 就是这样：它把登录界面
				// 甩给人，不走这个 RPC）。这时候干等没意义，把路指到终端。
				out.State = AuthStateUnknown
				msg = "agent 没有回应认证请求（它多半是让你在它自己的界面里登录）——请按登录指引在终端里登录一次"
			case strings.Contains(msg, "Method not implemented"):
				// 有的 agent 只在 initialize 里声明认证方式，却没实现这个方法
				// （claude-code-acp 的 claude-login 就是这样）。别让用户以为
				// 认证坏了——按登录指引在终端里登录一次才是正路。
				msg = "这个 agent 声明了认证方式但没实现它（Method not implemented）——按登录指引在终端里登录一次"
			}
			out.Message = "认证失败：" + msg
			if guide := loginGuideFor(agentCfg); guide.command != "" {
				out.Message += "（命令：" + guide.command + "）"
			}
			if tail := proc.stderrTail(6); tail != "" {
				out.Message += "（agent 输出：" + tail + "）"
			}
			return nil
		}
		out.State = AuthStateLoggedIn
		out.Message = "认证成功：" + am.view.Name
		return nil
	})
	if err != nil {
		return AuthResult{}, err
	}
	return out, nil
}

// authMethod 是一种认证方式的归一结果：view 给界面用，raw 留着取协议字段。
type authMethod struct {
	view AuthMethodView
	raw  acp.AuthMethod
}

// normalizeAuthMethods 把 ACP 的认证方式联合体归一成三种类型（agent / env_var /
// terminal）。
//
// 这里必须纠正 SDK 的一处行为：acp-go-sdk 的 AuthMethod 联合体只按"报文里有没有
// type 字段"挑分支——有 type（哪怕值是 "agent"）又没带 vars 时会被判成 terminal，
// 没有 type 反而判成 agent。ACP 规范说"没有 type 即视为 agent"，所以这里按
// 规范归一：既没有 args 也没有 env 的 terminal 变体一律当成 agent 型。
func normalizeAuthMethods(methods []acp.AuthMethod) []authMethod {
	out := make([]authMethod, 0, len(methods))
	for _, mv := range methods {
		switch {
		case mv.EnvVar != nil:
			v := AuthMethodView{
				ID: mv.EnvVar.Id, Name: mv.EnvVar.Name, Type: "env_var",
				Description: derefStr(mv.EnvVar.Description), Link: derefStr(mv.EnvVar.Link),
			}
			for _, ev := range mv.EnvVar.Vars {
				v.Vars = append(v.Vars, AuthVarView{
					Name: ev.Name, Label: derefStr(ev.Label),
					Optional: ev.Optional, Secret: ev.Secret,
				})
			}
			out = append(out, authMethod{view: v, raw: mv})
		case mv.Terminal != nil && (len(mv.Terminal.Args) > 0 || len(mv.Terminal.Env) > 0):
			out = append(out, authMethod{
				view: AuthMethodView{
					ID: mv.Terminal.Id, Name: mv.Terminal.Name, Type: "terminal",
					Description: derefStr(mv.Terminal.Description),
					Args:        mv.Terminal.Args, Env: anyMapToStrings(mv.Terminal.Env),
				},
				raw: mv,
			})
		case mv.Terminal != nil:
			out = append(out, authMethod{
				view: AuthMethodView{
					ID: mv.Terminal.Id, Name: mv.Terminal.Name, Type: "agent",
					Description: derefStr(mv.Terminal.Description),
				},
				raw: mv,
			})
		case mv.Agent != nil:
			out = append(out, authMethod{
				view: AuthMethodView{
					ID: mv.Agent.Id, Name: mv.Agent.Name, Type: "agent",
					Description: derefStr(mv.Agent.Description),
				},
				raw: mv,
			})
		}
	}
	return out
}

// authMethodViews 把 ACP 的认证方式摊平成三种类型给界面用。
func authMethodViews(methods []acp.AuthMethod) []AuthMethodView {
	norm := normalizeAuthMethods(methods)
	out := make([]AuthMethodView, 0, len(norm))
	for _, am := range norm {
		out = append(out, am.view)
	}
	return out
}

// findAuthMethod 按 id 找一种认证方式（归一后的）。
func findAuthMethod(methods []acp.AuthMethod, id string) (authMethod, bool) {
	for _, am := range normalizeAuthMethods(methods) {
		if am.view.ID == id {
			return am, true
		}
	}
	return authMethod{}, false
}

// authMethodIDs 把 agent 声明的认证方式 id 列出来（报错时告诉用户有哪些可选）。
func authMethodIDs(methods []acp.AuthMethod) string {
	ids := make([]string, 0, len(methods))
	for _, v := range authMethodViews(methods) {
		ids = append(ids, v.ID)
	}
	if len(ids) == 0 {
		return "（无）"
	}
	return strings.Join(ids, "、")
}

// terminalAuthCommand 拼出终端里该跑的命令：agent 自己的启动命令 + 认证参数。
func terminalAuthCommand(cfg config.AcpAgentConfig, mv acp.AuthMethod) string {
	parts := []string{cfg.Command}
	parts = append(parts, cfg.Args...)
	if mv.Terminal != nil {
		parts = append(parts, mv.Terminal.Args...)
	}
	return strings.Join(parts, " ")
}

// missingEnvVars 列出还没填的环境变量（空 = 都填了）。
func missingEnvVars(cfg config.AcpAgentConfig, vars []acp.AuthEnvVar) []string {
	var missing []string
	for _, v := range vars {
		if strings.TrimSpace(cfg.Env[v.Name]) == "" {
			missing = append(missing, v.Name)
		}
	}
	return missing
}

// envVarNames 把变量名连成一句（"OPENAI_API_KEY、GEMINI_API_KEY"）。
func envVarNames(vars []acp.AuthEnvVar) string {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}
	return strings.Join(names, "、")
}

// filledEnvVars 列出这些环境变量里已经填了值的（空的当没填）。
func filledEnvVars(cfg config.AcpAgentConfig, names []string) []string {
	var out []string
	for _, n := range names {
		if strings.TrimSpace(cfg.Env[n]) != "" {
			out = append(out, n)
		}
	}
	return out
}

// configuredKeyVars 列出配置里已填的密钥类环境变量（结尾是 _API_KEY / _TOKEN）。
func configuredKeyVars(cfg config.AcpAgentConfig) []string {
	var out []string
	for k, v := range cfg.Env {
		if strings.TrimSpace(v) == "" {
			continue
		}
		u := strings.ToUpper(k)
		if strings.HasSuffix(u, "_API_KEY") || strings.HasSuffix(u, "_TOKEN") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// anyMapToStrings 把 ACP 报文里的 map[string]any 转成 map[string]string
// （非字符串的值只取其字面量，不猜语义）。
func anyMapToStrings(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// derefStr 取 *string 的值（空指针给空串）。
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
