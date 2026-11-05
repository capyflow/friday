// session_record.go 是 Friday 在工作区里的唯一落盘产物：每条会话一份「会话记录」，
// 以及基于它的「总结并新开」交接。
//
// 为什么每条会话都要有记录文件：ACP 的会话状态存在 agent 自己那边，Friday 只看得见
// 流出来的文本，上下文一满或一重连就回不来。投影（acp-threads.json）里那份时间线是
// 权威，但 agent 读不到它；把同一份内容再落成工作区里的一个文件，agent 就能自己读
// 全量历史，交接也不再依赖那份截断摘要。文件是投影的**导出物**，每轮结束整份重写，
// 不做增量追加——重写永远等于当前时间线，不会出现两份真相互相漂移。
//
// 位置与污染：`.friday/sessions/<会话id>.md`，目录自带 `.friday/.gitignore`
// （内容 `*` + `!/.gitignore`），实测 `git ls-files --others --exclude-standard`
// 只看得见那一个 ignore 文件；非 git 项目没有 .gitignore 可依赖，快照遍历直接跳过
// `.friday`。两种情况下都不会进 git、也不会混进「本轮改动」。
//
// 生命周期与会话一一对应，规则只剩两条：删会话就删它的记录；启动时 GC 掉投影里
// 已经没有对应会话的孤儿文件。不需要数量上限——文件数就是会话数，会话本来就该由用户
// 自己删。
package acp

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	fridayBaseRel   = ".friday"                   // Friday 在工作区里唯一的落盘目录（自我忽略）
	sessionDirRel   = fridayBaseRel + "/sessions" // 每条会话一份记录
	fridayGitignore = "*\n!/.gitignore\n"         // 让 .friday 里的东西不进 git
	recordFileMax   = 200 << 10                   // 单个记录文件上限，超出截断并在文里说明
	// 随首轮发的精简摘要要短（怕占爆 agent 输入）；记录文件是给 agent 按需读的，
	// 预算可以宽得多。两套预算分开，别拿一份的口径去要求另一份。
	inlineEntries = 12
	inlineTotal   = 4000
	recordUser    = 2000
	recordAgent   = 4000
	recordNote    = 1000
	recordTotal   = 120000
)

// sessionRecordPath 返回某条会话记录的绝对路径与相对主目录的路径。路径完全由
// cwd + 会话 id 推出，不存进投影——少一个字段就少一处可能不一致。
func sessionRecordPath(cwd, threadID string) (abs, rel string) {
	if strings.TrimSpace(cwd) == "" || strings.TrimSpace(threadID) == "" {
		return "", ""
	}
	rel = filepath.ToSlash(filepath.Join(sessionDirRel, threadID+".md"))
	return filepath.Join(cwd, filepath.FromSlash(rel)), rel
}

// ensureFridayIgnore 写 .friday/.gitignore（只在缺失时写，用户改过就不再动）。
func ensureFridayIgnore(cwd string) {
	dir := filepath.Join(cwd, fridayBaseRel)
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); err == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[智能体] .friday 目录创建失败: %v", err)
		return
	}
	if err := os.WriteFile(ignore, []byte(fridayGitignore), 0o644); err != nil {
		log.Printf("[智能体] .friday/.gitignore 写入失败: %v", err)
	}
}

type recordArgs struct {
	Thread    *Thread
	WsName    string
	Messages  []Entry
	FromTitle string // 交接来的那条来源会话的标题（FromThread 为空时忽略）
	FromCwd   string // 来源会话的主目录（跨工作区/换主目录时算相对路径要用）
}

// sessionRecordText 生成一条会话的完整记录：元信息、交接链、可见时间线（含每轮改动
// 文件清单），末尾明说"这份里没有什么"，让 agent 拿它当线索而不是当完整真相。
func sessionRecordText(a recordArgs) string {
	th := a.Thread
	var b strings.Builder
	b.WriteString("# Friday 会话记录\n\n")
	fmt.Fprintf(&b, "- 会话：%s（id=%s）\n", orText(th.Title, "未命名"), th.ID)
	fmt.Fprintf(&b, "- 智能体：%s，主目录：%s\n", th.Agent, th.Cwd)
	fmt.Fprintf(&b, "- 工作区：%s\n", orText(a.WsName, "未命名"))
	fmt.Fprintf(&b, "- 创建：%s　更新：%s\n",
		th.CreatedAt.Format("2006-01-02 15:04:05"), time.Now().Format("2006-01-02 15:04:05"))
	if th.HandoffFrom != "" {
		_, fromRel := sessionRecordPath(a.FromCwd, th.HandoffFrom)
		fmt.Fprintf(&b, "- 交接自：会话 %s", th.HandoffFrom)
		if a.FromTitle != "" {
			fmt.Fprintf(&b, "（%s）", a.FromTitle)
		}
		if fromRel != "" {
			fmt.Fprintf(&b, "，它的完整记录：%s", fromRel)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n> 本文件由 Friday 每轮结束时从会话投影整份重写，是投影的导出物；\n"+
		"> 权威内容以 Friday 里的会话为准，改这个文件不会影响会话。\n\n")

	images := 0
	for _, e := range a.Messages {
		if e.Role == "user" && e.Images > 0 {
			images += e.Images
		}
	}
	b.WriteString("## 对话\n")
	for _, e := range a.Messages {
		text := strings.TrimSpace(e.Text)
		switch e.Role {
		case "user":
			if text == "" && e.Images == 0 {
				continue
			}
			head := "用户"
			if e.Images > 0 {
				head = fmt.Sprintf("用户（附 %d 张图）", e.Images)
			}
			fmt.Fprintf(&b, "\n**%s**：%s\n", head, truncateRunes(text, recordUser))
		case "assistant":
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "\n**agent**：%s\n", truncateRunes(text, recordAgent))
			if e.Changes != nil && len(e.Changes.Files) > 0 {
				names := make([]string, 0, len(e.Changes.Files))
				for _, f := range e.Changes.Files {
					names = append(names, fmt.Sprintf("%s（%s +%d/-%d）",
						f.Path, f.Status, f.Additions, f.Deletions))
				}
				fmt.Fprintf(&b, "\n本轮改动：%s\n", strings.Join(names, "、"))
			}
		case "note":
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "\n> %s\n", truncateRunes(text, recordNote))
		}
	}

	b.WriteString("\n## 这份记录里没有什么\n")
	b.WriteString("- agent 在自己进程里读过的文件内容、跑过的命令与输出、思考过程：ACP 不会把这些流给 Friday，一条都没记。\n")
	if images > 0 {
		fmt.Fprintf(&b, "- 图片附件的像素内容：只记了哪几条消息带了几张图，图本身在 Friday 的上传目录里。\n")
	}
	b.WriteString("- 结论与约定如果重要，请写进工作区的「项目记忆」页——它比这份记录可靠，也不受这里的字数上限约束。\n")
	b.WriteString("\n请把它当线索：需要事实细节时直接读盘确认，不要当成完整上下文。\n")

	out := b.String()
	if len(out) > recordTotal {
		out = truncateRunes(out, recordTotal) + "\n…（记录已达上限被截断，更早的内容仍在 Friday 会话里）\n"
	}
	return out
}

// writeSessionRecord 重写某条会话的记录文件，返回相对路径（写不成返回空串）。
// 失败只记日志并让调用方降级：记录是加分项，会话本身照旧能跑。
func writeSessionRecord(a recordArgs) string {
	abs, rel := sessionRecordPath(a.Thread.Cwd, a.Thread.ID)
	if abs == "" {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		log.Printf("[智能体] 会话记录目录创建失败: %v", err)
		return ""
	}
	ensureFridayIgnore(a.Thread.Cwd)
	text := sessionRecordText(a)
	if len(text) > recordFileMax {
		// 裸字节截断可能落在多字节字符中间，切出非法 UTF-8：回退到完整 rune 边界。
		// 截断提示放在裁剪之后追加——上一道的「已达上限」提示可能正好被这次裁剪切掉。
		text = text[:recordFileMax]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "\n\n…（记录文件已达大小上限被截断，更早的内容仍在 Friday 会话里）\n"
	}
	if err := os.WriteFile(abs, []byte(text), 0o600); err != nil {
		log.Printf("[智能体] 会话记录写入失败 %s: %v", abs, err)
		return ""
	}
	return rel
}

// deleteSessionRecord 删掉某条会话的记录文件。路径由 cwd + 会话 id 推出，
// 不接受外部输入，因此不存在越界删除的口子。
func deleteSessionRecord(cwd, threadID string) {
	abs, _ := sessionRecordPath(cwd, threadID)
	if abs == "" {
		return
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		log.Printf("[智能体] 会话记录清理失败 %s: %v", abs, err)
	}
}

// pruneSessionRecords 启动时收一次尾：投影里已经没有对应会话的记录是孤儿（旧版本
// 删会话没删干净、或删在中途），直接删。同一主目录里"会话还在"的记录一律不动——
// 记录数就是会话数，该由用户删会话来减，不是由淘汰策略来减。
func (m *Manager) pruneSessionRecords() {
	m.mu.Lock()
	type ref struct {
		cwd string
		ids map[string]bool
	}
	byCwd := map[string]*ref{}
	for _, th := range m.threads {
		if th.Cwd == "" {
			continue
		}
		r := byCwd[th.Cwd]
		if r == nil {
			r = &ref{cwd: th.Cwd, ids: map[string]bool{}}
			byCwd[th.Cwd] = r
		}
		r.ids[th.ID] = true
	}
	m.mu.Unlock()

	for _, r := range byCwd {
		dir := filepath.Join(r.cwd, sessionDirRel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 还没写过记录，正常
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if r.ids[strings.TrimSuffix(e.Name(), ".md")] {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				log.Printf("[智能体] 孤儿会话记录清理失败: %v", err)
			}
		}
	}
}

// summarizeEntries 从会话投影整理精简摘要：只使用 Friday 看得见的消息（用户原话、
// agent 最终回答与系统备注），不做模型调用。取最近 maxEntries 条，单条按角色截断，
// 整段封顶 maxTotal 字。随首轮发出去的就是这一份。
func summarizeEntries(msgs []Entry, maxEntries, maxTotal int) string {
	start := len(msgs) - maxEntries
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "以下是上一个会话的交接摘要（由 Friday 从可见时间线整理，共 %d 条消息，"+
		"这里只含最近 %d 条；agent 未上报的细节、工具输出与图片附件不在里面）。\n",
		len(msgs), len(msgs)-start)
	for _, e := range msgs[start:] {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		switch e.Role {
		case "user":
			b.WriteString("\n用户：" + truncateRunes(text, 500))
		case "assistant":
			b.WriteString("\nagent：" + truncateRunes(text, 800))
		case "note":
			b.WriteString("\n备注：" + truncateRunes(text, 300))
		}
	}
	out := b.String()
	if len([]rune(out)) > maxTotal {
		out = truncateRunes(out, maxTotal) + "\n…（已达摘要上限，更早的内容见上一个会话的记录文件）"
	}
	return out
}

func orText(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// Handoff 从一条会话生成交接摘要，并在同一工作区/主目录下新开一条"续"会话。
//
// agent 留空 = 交给原来那台；给了别的名字 = 换一台接着干。摘要只是文本，跨 agent
// 带得动；但模型与审批档位的取值各家不同，换人时不沿用，新会话回到工作区默认档。
//
// 交接不复制文件：新会话首轮拿到的是「精简摘要 + 来源会话记录文件的路径」，完整
// 历史留在来源会话自己的记录里。两条会话的记录文件各自随自己的轮次重写。
func (m *Manager) Handoff(id, agent string) (Thread, error) {
	mu := m.threadLock(id)
	mu.Lock()
	defer mu.Unlock()

	m.mu.Lock()
	th, ok := m.threads[id]
	if !ok {
		m.mu.Unlock()
		return Thread{}, fmt.Errorf("未找到会话 %s", id)
	}
	if th.State == StateRunning {
		m.mu.Unlock()
		return Thread{}, errors.New("这一轮还在跑，等它结束再交接")
	}
	if len(th.Messages) == 0 {
		m.mu.Unlock()
		return Thread{}, errors.New("这条会话还没有可交接的对话内容")
	}
	title := th.Title
	if title == "" {
		title = "上一个会话"
	}
	workspaceID, cwd, fromAgent := th.WorkspaceID, th.Cwd, th.Agent
	toAgent := strings.TrimSpace(agent)
	if toAgent == "" {
		toAgent = fromAgent
	}
	sameAgent := toAgent == fromAgent
	choices := make(map[string]string, len(th.CapChoices))
	for k, v := range th.CapChoices {
		choices[k] = v
	}
	msgs := append([]Entry{}, th.Messages...)
	fromID := th.ID
	m.mu.Unlock()

	// 先保证来源会话自己有记录（老会话可能建在本功能之前，那时还没写过文件）：
	// 整份重写一次，拿到的就是"确实存在"的路径——写不成返回空，首轮块里就不能指。
	fromRel := m.rewriteRecordFor(fromID)

	nt, err := m.CreateSession(toAgent, workspaceID, cwd, title+" · 续")
	if err != nil {
		return Thread{}, err
	}

	inline := summarizeEntries(msgs, inlineEntries, inlineTotal)
	loc := "上一个会话的记录文件（本次没能生成，以下面这份为准）"
	if fromRel != "" {
		loc = "工作区文件 " + fromRel
	}
	block := fmt.Sprintf("[Friday 交接] 上一个会话（%s · %s）的精简摘要如下。"+
		"更早的对话、每轮改动过的文件清单在%s，需要细节请读它；"+
		"读不到就以这份为准，不要假设未列出的细节。\n\n%s",
		fromAgent, title, loc, inline)

	note := "已从上一个会话交接上下文；摘要会随下一轮发给 agent。"
	if !sameAgent {
		note = fmt.Sprintf("已从上一个会话交接上下文（上一个由 %s 跑，这次交给 %s）；"+
			"模型与审批档位不跨 agent 沿用，本次按工作区默认档。摘要会随下一轮发给 agent。",
			fromAgent, toAgent)
	}

	m.mu.Lock()
	cur := m.threads[nt.ID]
	if cur != nil {
		cur.Handoff = block
		cur.HandoffFrom = fromID
		if sameAgent && len(choices) > 0 {
			cur.CapChoices = choices
		} else if !sameAgent {
			// CreateSession 会按「新会话沿用这台 agent 上次的选择」给新会话播种目标
			// agent 的旧档位；交接换人明说「按工作区默认档」，这里显式清掉，别让文案撒谎。
			cur.CapChoices = nil
		}
	}
	m.mu.Unlock()

	// 新会话立刻有自己的记录（里面已经带上交接链），路径再写进时间线的 note 里。
	newRel := ""
	if cur != nil {
		newRel = m.rewriteRecordFor(nt.ID)
	}
	if newRel != "" {
		note += fmt.Sprintf("本会话的完整记录：%s（每轮结束重写，删会话一起清理）。", newRel)
	}

	m.mu.Lock()
	if again := m.threads[nt.ID]; again != nil {
		again.Messages = append(again.Messages, Entry{Role: "note", At: time.Now(),
			Text: note + "\n\n" + truncateRunes(inline, 800)})
		if err := m.saveLocked(); err != nil {
			log.Printf("[智能体] 交接结果落盘失败: %v", err)
		}
	}
	m.mu.Unlock()

	log.Printf("[智能体] 会话 %s 已交接为新会话 %s（%s → %s，摘要 %d 字，本会话记录=%q）",
		id, nt.ID, fromAgent, toAgent, len([]rune(inline)), newRel)
	got, ok := m.Get(nt.ID)
	if !ok {
		return Thread{}, errors.New("续会话创建后读取失败")
	}
	return got, nil
}

// rewriteRecordFor 按投影当前内容重写某条会话的记录文件，返回相对路径（失败为空）。
// 轮末与交接都走这里，保证"文件 = 投影的导出物"这一条不变。
func (m *Manager) rewriteRecordFor(id string) string {
	m.mu.Lock()
	th, ok := m.threads[id]
	if !ok || th.Cwd == "" {
		m.mu.Unlock()
		return ""
	}
	cp := *th
	cp.Messages = append([]Entry{}, th.Messages...)
	msgs := cp.Messages
	ws := m.workspaces[th.WorkspaceID]
	wsName := ""
	if ws != nil {
		wsName = ws.Name
	}
	fromID, fromCwd, fromTitle := th.HandoffFrom, "", ""
	if src, ok := m.threads[fromID]; ok {
		fromCwd, fromTitle = src.Cwd, src.Title
	}
	m.mu.Unlock()
	return writeSessionRecord(recordArgs{
		Thread: &cp, WsName: wsName, Messages: msgs,
		FromTitle: fromTitle, FromCwd: fromCwd,
	})
}
