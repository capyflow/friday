// sessions.go 管网页会话这份「清单」：生活模式只有唯一的主会话（key web，
// 网页/钉钉/飞书/心跳都接在它上面），工作模式允许用户自己新建多条彼此独立的
// 会话，各聊各的、上下文互不串。
//
// 为什么还要一个注册表文件：会话本体（消息历史）由框架的 sessionstore 落在
// session.json 里，框架的 Session 结构没有放元数据的位置——显示名和创建时间是
// Friday 自己的概念，另存一份 chat-sessions.json（数据目录，与 paused-turns.json
// 同级）比改别人的存储格式干净。表里只有工作会话，键是可读明文
// work-<日期时间>-<随机>，肉眼对得上会话栏里的那一条。
//
// 生命周期三条：新建进表、改名写表、删除连表项带历史与调用轨迹一起清。
// 不设条数上限，也不做启动 GC——会话数就是用户建的数量，工作会话跟主会话一样
// 长期存在，由用户自己删。表里有、池里没有的条目不算脏数据（新建还没发过消息
// 的会话就是这样），照旧列出来。
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/trace"
)

// 会话种类。聊天页的会话栏只列 life 与 work；internal（心跳/计划/目标这些后台
// 入口的会话）不进聊天页，只在设置页的会话列表里可见——列出来是为了能清上下文，
// 不是为了让用户去那儿对话。
const (
	SessionKindLife     = "life"
	SessionKindWork     = "work"
	SessionKindInternal = "internal"
)

const (
	// workKeyPrefix 是工作会话 key 的前缀，也是"这条会话可以删"的判据。
	workKeyPrefix = "work-"
	// lifeSessionTitle 是主会话在界面上的名字（生活模式没有第二条，标题是常量）。
	lifeSessionTitle = "主会话"
	// sessionTitleMax 是显示名的 rune 上限：会话栏一行的宽度就那么多，超了截断。
	sessionTitleMax = 24
	// newSessionTitle 是既没名字又还没内容的会话显示什么（新建后还没发过消息）。
	newSessionTitle = "新会话"
)

// sessionMeta 是一条工作会话在注册表里的元数据。
type sessionMeta struct {
	// Title 是用户手起的名字（空 = 没起过，列表里回落到首条提问自动取名）。
	Title string `json:"title,omitempty"`
	// CreatedAt 是建会话的时刻：会话还没说过话时没有活跃时间，列表靠它排序。
	CreatedAt time.Time `json:"createdAt"`
}

// ChatSession 是会话列表接口对外返回的一条会话。
type ChatSession struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Messages  int       `json:"messages"`
	// CreatedAt 只有工作会话带（注册表里记的建立时刻）。
	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// HasTitle 标记"用户起过名字"——前端据此决定改名框里预填什么、
	// 清空是否等于"回到自动取名"。
	HasTitle bool `json:"hasTitle,omitempty"`
}

// resolveSessionKey 把请求里的 session 字段翻成可用的会话 key。
//
// 只放行两种：主会话 web，和注册表里已建的工作会话。这里不能像历史接口那样
// 任传一个 key——那等于让任何一次请求都能凭空造出一条会话（会话栏会出现用户
// 没建过的条目，删除入口也找不到它）。
func (c *Chat) resolveSessionKey(raw string) (string, error) {
	key := agent.CurrentKey(strings.TrimSpace(raw))
	if key == "" || key == agent.SessionWeb {
		return agent.SessionWeb, nil
	}
	if !strings.HasPrefix(key, workKeyPrefix) {
		return "", fmt.Errorf("会话 %q 不存在：工作会话由「新建会话」创建，日常对话请直接发（不传 session 即主会话）", key)
	}
	c.mu.Lock()
	_, ok := c.work[key]
	c.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("会话 %q 已被删除或不属于本机，先新建一条再发", key)
	}
	return key, nil
}

// loadWork 恢复工作会话注册表。文件缺失按"还没有工作会话"处理；损坏时保留
// 原文件并放弃加载（不用空表覆盖用户起过的名字，重启修好后还能恢复）。
func (c *Chat) loadWork() {
	if c.sessionsFile == "" {
		return
	}
	data, err := os.ReadFile(c.sessionsFile)
	if err != nil {
		if !os.IsNotExist(err) && !errors.Is(err, os.ErrPermission) {
			log.Printf("[网页] 读取工作会话表 %q 失败，已保留原文件: %v", c.sessionsFile, err)
		}
		return
	}
	var items map[string]sessionMeta
	if err := json.Unmarshal(data, &items); err != nil {
		log.Printf("[网页] 工作会话表 %q 损坏，已保留原文件；修复后重启可恢复", c.sessionsFile)
		return
	}
	c.mu.Lock()
	for key, meta := range items {
		if key == "" || !strings.HasPrefix(key, workKeyPrefix) {
			continue
		}
		c.work[key] = meta
	}
	c.mu.Unlock()
}

// saveWorkLocked 原子落盘工作会话表。调用方须持 c.mu。
func (c *Chat) saveWorkLocked() error {
	if c.sessionsFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.work, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化工作会话表失败: %w", err)
	}
	dir := filepath.Dir(c.sessionsFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建工作会话表目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(c.sessionsFile)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建工作会话表临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("写入工作会话表失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步工作会话表失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭工作会话表失败: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("设置工作会话表权限失败: %w", err)
	}
	if err := os.Rename(tmp, c.sessionsFile); err != nil {
		return fmt.Errorf("替换工作会话表失败: %w", err)
	}
	return nil
}

// newWorkSessionKey 生成一条工作会话的 key：work-日期时间-随机尾巴。
// 可读（在日志和 JSON 里能认出是哪条），尾巴防同一秒内建两条撞车。
func newWorkSessionKey(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%s-%x", workKeyPrefix, now.Format("20060102-150405"), now.UnixNano())
	}
	return fmt.Sprintf("%s%s-%s", workKeyPrefix, now.Format("20060102-150405"), hex.EncodeToString(b[:]))
}

// CreateWorkSession 新建一条工作会话：只登记，不预先拉模型。
// 会话真正的内容从用户发的第一条消息开始。
func (c *Chat) CreateWorkSession(ctx context.Context, title string) (ChatSession, error) {
	key := newWorkSessionKey(time.Now())
	c.mu.Lock()
	c.work[key] = sessionMeta{Title: strings.TrimSpace(title), CreatedAt: time.Now()}
	if err := c.saveWorkLocked(); err != nil {
		delete(c.work, key)
		c.mu.Unlock()
		return ChatSession{}, err
	}
	c.mu.Unlock()
	log.Printf("[网页] 新建工作会话 %s", key)
	return c.sessionView(ctx, key), nil
}

// RenameWorkSession 改工作会话的名字；title 传空表示回到自动取名
// （自动名取首条提问，跟会话栏里没起名时看到的一致）。
func (c *Chat) RenameWorkSession(ctx context.Context, key, title string) (ChatSession, error) {
	key = agent.CurrentKey(strings.TrimSpace(key))
	c.mu.Lock()
	meta, ok := c.work[key]
	if !ok {
		c.mu.Unlock()
		return ChatSession{}, fmt.Errorf("没有叫 %q 的工作会话（主会话不用改名，它一直叫「%s」）", key, lifeSessionTitle)
	}
	meta.Title = strings.TrimSpace(title)
	c.work[key] = meta
	if err := c.saveWorkLocked(); err != nil {
		c.mu.Unlock()
		return ChatSession{}, err
	}
	c.mu.Unlock()
	return c.sessionView(ctx, key), nil
}

// DeleteWorkSession 删除一条工作会话：表项、历史消息、调用轨迹与暂停现场一起清。
// 主会话不在此列——它是生活模式那条唯一的对话，删了等于把管家清空，
// 要清内容用「清除上下文」。
func (c *Chat) DeleteWorkSession(key string) error {
	key = agent.CurrentKey(strings.TrimSpace(key))
	if key == agent.SessionWeb {
		return fmt.Errorf("主会话不能删除（要清掉它的内容用「清除上下文」）")
	}
	c.mu.Lock()
	if _, ok := c.work[key]; !ok {
		c.mu.Unlock()
		return fmt.Errorf("没有叫 %q 的工作会话", key)
	}
	if run := c.runs[key]; run != nil {
		c.mu.Unlock()
		return fmt.Errorf("%q 正在回复中，先点停止再删（删一半留个断掉的连接没意义）", key)
	}
	c.mu.Unlock()

	if c.brain != nil {
		if err := c.brain.Reset(key); err != nil {
			return fmt.Errorf("清除会话历史失败: %w", err)
		}
	}
	trace.ClearSession(key)

	c.mu.Lock()
	delete(c.work, key)
	if _, ok := c.paused[key]; ok {
		delete(c.paused, key)
		c.savePausedLocked()
	}
	if err := c.saveWorkLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	log.Printf("[网页] 删除工作会话 %s", key)
	return nil
}

// ListSessions 返回全部网页会话：主会话在最前，工作会话按最近活动倒序，
// 后台入口的会话（心跳/计划/目标）垫底且只带 key。
// 只读——不因为列一次清单就把空会话写进池里。
func (c *Chat) ListSessions(ctx context.Context) []ChatSession {
	c.mu.Lock()
	work := make(map[string]sessionMeta, len(c.work))
	for key, meta := range c.work {
		work[key] = meta
	}
	c.mu.Unlock()

	poolIdx := map[string]agent.SessionInfo{}
	if c.brain != nil {
		for _, s := range c.brain.Pool().List() {
			poolIdx[s.Key] = s
		}
	}

	out := make([]ChatSession, 0, len(work)+len(poolIdx)+1)
	life := ChatSession{Key: agent.SessionWeb, Title: lifeSessionTitle, Kind: SessionKindLife}
	if info, ok := poolIdx[agent.SessionWeb]; ok {
		life.Messages, life.UpdatedAt = info.Messages, info.UpdatedAt
	}
	out = append(out, life)

	known := map[string]bool{agent.SessionWeb: true}
	for key := range work {
		known[key] = true
	}
	for _, info := range poolIdx {
		if known[info.Key] {
			continue
		}
		out = append(out, ChatSession{Key: info.Key, Kind: SessionKindInternal,
			Messages: info.Messages, UpdatedAt: info.UpdatedAt})
	}

	for key, meta := range work {
		view := ChatSession{Key: key, Kind: SessionKindWork, CreatedAt: meta.CreatedAt,
			HasTitle: meta.Title != ""}
		view.Title = meta.Title
		if info, ok := poolIdx[key]; ok {
			view.Messages, view.UpdatedAt = info.Messages, info.UpdatedAt
		}
		// 还没说过话的会话没有"最后活跃"，用创建时刻顶上：列表按活动排序、
		// 界面上写"多久之前"，两边都不能拿零值时间（0001-01-01）去算。
		if view.UpdatedAt.IsZero() {
			view.UpdatedAt = meta.CreatedAt
		}
		if view.Title == "" {
			view.Title = c.autoTitle(ctx, key)
		}
		out = append(out, view)
	}

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := kindRank(out[i].Kind), kindRank(out[j].Kind)
		if ri != rj {
			return ri < rj
		}
		switch out[i].Kind {
		case SessionKindWork:
			return sessionActivity(out[i]).After(sessionActivity(out[j]))
		case SessionKindInternal:
			return out[i].Key < out[j].Key
		default:
			return false
		}
	})
	return out
}

// sessionView 组装单条工作会话的对外视图（新建/改名后立即回给前端，省一次列表）。
func (c *Chat) sessionView(ctx context.Context, key string) ChatSession {
	c.mu.Lock()
	meta, ok := c.work[key]
	c.mu.Unlock()
	if !ok {
		return ChatSession{Key: key, Title: newSessionTitle, Kind: SessionKindWork}
	}
	view := ChatSession{Key: key, Kind: SessionKindWork, CreatedAt: meta.CreatedAt, HasTitle: meta.Title != ""}
	view.Title = meta.Title
	if c.brain != nil {
		for _, s := range c.brain.Pool().List() {
			if s.Key == key {
				view.Messages, view.UpdatedAt = s.Messages, s.UpdatedAt
				break
			}
		}
	}
	if view.UpdatedAt.IsZero() {
		view.UpdatedAt = meta.CreatedAt // 同 ListSessions：没活跃过就用创建时刻
	}
	if view.Title == "" {
		view.Title = c.autoTitle(ctx, key)
	}
	return view
}

// autoTitle 用会话里的第一条提问当名字（取首行、按会话栏宽度截断）。
// 没内容就回「新会话」——用户刚建完还没发话时，会话栏总得有东西可指。
func (c *Chat) autoTitle(ctx context.Context, key string) string {
	if c.brain == nil {
		return newSessionTitle
	}
	for _, m := range c.brain.Pool().History(ctx, key) {
		if m.Role != llm.RoleUser {
			continue
		}
		title := firstLineText(m)
		if title == "" {
			continue
		}
		if r := []rune(title); len(r) > sessionTitleMax {
			return string(r[:sessionTitleMax]) + "…"
		}
		return title
	}
	return newSessionTitle
}

// firstLineText 取一条消息的首行文本（图片消息没有文本时返回空串）。
func firstLineText(m llm.Message) string {
	var b strings.Builder
	for _, part := range m.Content {
		if part.Type == llm.ContentText {
			b.WriteString(part.Text)
		}
	}
	line, _, _ := strings.Cut(strings.TrimSpace(b.String()), "\n")
	return strings.TrimSpace(line)
}

// kindRank 决定三类会话在列表里的先后。
func kindRank(kind string) int {
	switch kind {
	case SessionKindLife:
		return 0
	case SessionKindWork:
		return 1
	default:
		return 2
	}
}

// sessionActivity 取会话的排序时刻：发过话用最后活跃，否则用创建时刻。
func sessionActivity(s ChatSession) time.Time {
	if !s.UpdatedAt.IsZero() {
		return s.UpdatedAt
	}
	return s.CreatedAt
}
