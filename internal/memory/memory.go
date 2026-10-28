// Package memory 实现机器人长期记忆的 wiki 形态：记忆按主题组织为页面
// （标题 + markdown 正文），agent 通过 remember/recall/forget 工具检索与
// 维护页面（先查相关页，合并内容后整体保存），管理后台可像浏览 wiki 文档
// 一样查看与编辑。每页持久化为一个 <id>.md 文件（YAML frontmatter +
// markdown 正文，tmp 文件 + rename 原子替换），重启不丢；旧版扁平
// memories.json 在 wiki 目录为空时自动一次性迁入《用户档案》页。
package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Page 是一页长期记忆（wiki 条目）。
type Page struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`             // markdown 正文
	Source    string    `json:"source,omitempty"` // user=用户要求 / agent=机器人主动 / admin=后台 / migrate=旧记忆迁移
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PageMeta 是页面列表项（不含正文；搜索命中正文时附上下文片段）。
type PageMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Snippet   string    `json:"snippet,omitempty"`
}

// frontMatter 是页面文件的元数据段（文件头 YAML frontmatter）。
type frontMatter struct {
	ID        string    `yaml:"id"`
	Title     string    `yaml:"title"`
	Source    string    `yaml:"source,omitempty"`
	CreatedAt time.Time `yaml:"created"`
	UpdatedAt time.Time `yaml:"updated"`
}

// WikiStore 是长期记忆 wiki 仓库：内存索引 + 每页一个 <id>.md 文件。
// dir 为空表示不持久化（测试用）；legacyFile 为旧版扁平记忆 JSON 路径
// （一次性迁移来源，可为空）。
type WikiStore struct {
	mu         sync.Mutex
	dir        string
	legacyFile string
	pages      map[string]*Page
	// lastNow 是最近一次经 nextNowLocked 发放的墙钟。Windows 的系统时钟粗粒度
	// （同 tick 读数全等）且可能被 NTP 回拨，页面 ID 与「最近更新排前」都依赖
	// 「后写的更大」——统一从守卫钟发放（与 internal/todo 的 lastCreated 同纪律）。
	lastNow time.Time
}

// NewWikiStore 创建 wiki 记忆仓库：加载 dir 下的页面，并在目录为空且
// legacyFile 存在时把旧版记忆一次性迁入《用户档案》页。
func NewWikiStore(dir, legacyFile string) *WikiStore {
	s := &WikiStore{dir: dir, legacyFile: legacyFile, pages: map[string]*Page{}}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[memory] 创建记忆目录失败: %v", err)
		}
	}
	s.load()
	s.migrateLegacy()
	return s
}

// pagePath 返回页面对应的磁盘路径；id 非法（路径穿越）时返回空串。
func (s *WikiStore) pagePath(id string) string {
	if !validPageID(id) {
		return ""
	}
	return filepath.Join(s.dir, id+".md")
}

// validPageID 限制 ID 字符集（字母/数字/下划线/连字符），防止路径穿越。
func validPageID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// load 从目录加载全部页面；损坏的页面跳过（与任务落盘相同的容错策略）。
func (s *WikiStore) load() {
	if s.dir == "" {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return // 目录不存在：空启动
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		p, err := readPageFile(path)
		if err != nil {
			log.Printf("[memory] 页面 %s 解析失败，已跳过: %v", e.Name(), err)
			continue
		}
		if p.ID == "" {
			p.ID = strings.TrimSuffix(e.Name(), ".md")
		}
		if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
			if fi, err := e.Info(); err == nil {
				if p.CreatedAt.IsZero() {
					p.CreatedAt = fi.ModTime()
				}
				if p.UpdatedAt.IsZero() {
					p.UpdatedAt = fi.ModTime()
				}
			}
		}
		s.pages[p.ID] = p
	}
}

// readPageFile 解析一个页面文件：首行 --- 到下一个 --- 为 YAML frontmatter，
// 其余为 markdown 正文。
func readPageFile(path string) (*Page, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	const open = "---\n"
	if !strings.HasPrefix(text, open) {
		return nil, fmt.Errorf("缺少 frontmatter")
	}
	rest := text[len(open):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, fmt.Errorf("frontmatter 未闭合")
	}
	var fm frontMatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return nil, fmt.Errorf("frontmatter 解析: %w", err)
	}
	body := rest[end+4:]                  // 跳过 "\n---"，剩余 "\n[空行]正文"
	body = strings.TrimPrefix(body, "\n") // "---" 行尾换行
	body = strings.TrimPrefix(body, "\n") // frontmatter 与正文间的空行（写方约定）
	return &Page{
		ID:        fm.ID,
		Title:     strings.TrimSpace(fm.Title),
		Body:      body,
		Source:    fm.Source,
		CreatedAt: fm.CreatedAt,
		UpdatedAt: fm.UpdatedAt,
	}, nil
}

// writePageLocked 原子写回页面文件（调用方须持有 mu）。
func (s *WikiStore) writePageLocked(p *Page) error {
	if s.dir == "" {
		return nil
	}
	path := s.pagePath(p.ID)
	if path == "" {
		return fmt.Errorf("非法页面 ID %q", p.ID)
	}
	fmRaw, err := yaml.Marshal(frontMatter{
		ID: p.ID, Title: p.Title, Source: p.Source,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fmRaw)
	b.WriteString("---\n\n")
	b.WriteString(p.Body)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nextNowLocked 返回严格递增的墙钟（Round(0) 剥离单调钟）。同 tick 内的两次
// time.Now() 读数全等会让新页面拿到与上一页相同的 ID/UpdatedAt——原 newIDLocked
// 靠撞ID重试，实际是空转到时钟跳格；这里每次调用至少推进 1ns，循环必然终止。
func (s *WikiStore) nextNowLocked() time.Time {
	now := time.Now().Round(0)
	if !now.After(s.lastNow) {
		now = s.lastNow.Add(time.Nanosecond)
	}
	s.lastNow = now
	return now
}

// newIDLocked 生成未占用的新页面 ID（调用方须持有 mu）。ID 取自守卫钟，
// 与已加载页面撞 ID 时逐纳秒推进重试（有限步，含时钟回拨后重启的情形）。
func (s *WikiStore) newIDLocked() string {
	for {
		id := "m" + strconv.FormatInt(s.nextNowLocked().UnixNano(), 10)
		if _, ok := s.pages[id]; !ok {
			return id
		}
	}
}

// Save 新建或更新一页记忆并落盘，返回页面与是否新建。
// id 非空：更新指定页面（标题可改，页面不存在时报错）；
// id 为空：按标题精确匹配更新已有页面（agent 合并写入的幂等入口），无匹配则新建。
func (s *WikiStore) Save(id, title, body, source string) (Page, bool, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" {
		return Page{}, false, fmt.Errorf("页面标题不能为空")
	}
	if body == "" {
		return Page{}, false, fmt.Errorf("页面正文不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nextNowLocked()
	var p *Page
	created := false
	if trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), "#")); trimmed != "" {
		cur, ok := s.pages[trimmed]
		if !ok {
			return Page{}, false, fmt.Errorf("未找到记忆页 #%s", trimmed)
		}
		p = cur
	} else {
		for _, c := range s.pages {
			if c.Title == title {
				p = c
				break
			}
		}
		if p == nil {
			p = &Page{ID: s.newIDLocked(), CreatedAt: now}
			s.pages[p.ID] = p
			created = true
		}
	}
	oldBody := p.Body
	p.Title, p.Body, p.Source, p.UpdatedAt = title, body, source, now
	if err := s.writePageLocked(p); err != nil {
		if created { // 新建落盘失败：移除内存态，避免内存与磁盘不一致
			delete(s.pages, p.ID)
		} else {
			p.Body = oldBody
		}
		return Page{}, false, err
	}
	return *p, created, nil
}

// Read 返回一页完整内容。
func (s *WikiStore) Read(id string) (Page, bool) {
	id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), "#"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pages[id]; ok {
		return *p, true
	}
	return Page{}, false
}

// Delete 删除一页记忆（整页删除），返回是否删除了。
func (s *WikiStore) Delete(id string) bool {
	id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), "#"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pages[id]; !ok {
		return false
	}
	delete(s.pages, id)
	if s.dir != "" {
		if path := s.pagePath(id); path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("[memory] 删除页面文件失败: %v", err)
			}
		}
	}
	return true
}

// Pages 返回全部页面元数据，按更新时间倒序。
func (s *WikiStore) Pages() []PageMeta {
	return s.Search("")
}

// Search 按关键词匹配标题与正文（不区分大小写）；keyword 为空返回全部。
// 命中正文时附上下文片段。
func (s *WikiStore) Search(keyword string) []PageMeta {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	s.mu.Lock()
	out := make([]PageMeta, 0, len(s.pages))
	for _, p := range s.pages {
		if keyword != "" {
			if strings.Contains(strings.ToLower(p.Title), keyword) {
				out = append(out, metaOf(p, ""))
				continue
			}
			i := strings.Index(strings.ToLower(p.Body), keyword)
			if i < 0 {
				continue
			}
			out = append(out, metaOf(p, snippet(p.Body, i, len(keyword))))
			continue
		}
		out = append(out, metaOf(p, ""))
	}
	s.mu.Unlock()
	// 更新时间倒序；UpdatedAt 相同（磁盘加载的页面可同 tick）以 ID 兜底——
	// ID 取自守卫钟、同长数字串，字典序即时间序，保证两次调用结果一致。
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Round(0).Equal(out[j].UpdatedAt.Round(0)) {
			return out[i].UpdatedAt.Round(0).After(out[j].UpdatedAt.Round(0))
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Count 返回页面数。
func (s *WikiStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pages)
}

// metaOf 投影出页面元数据。
func metaOf(p *Page, snip string) PageMeta {
	return PageMeta{
		ID: p.ID, Title: p.Title, Source: p.Source,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Snippet: snip,
	}
}

// snippet 截取正文中命中位置前后的上下文（前后各约 30 字符）。
func snippet(body string, idx, kwLen int) string {
	const around = 30
	start, end := idx-around, idx+kwLen+around
	if start < 0 {
		start = 0
	}
	if end > len(body) {
		end = len(body)
	}
	// 对齐 UTF-8 字符边界，避免截断多字节字符
	for start > 0 && !utf8.RuneStart(body[start]) {
		start--
	}
	for end < len(body) && !utf8.RuneStart(body[end]) {
		end++
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(body) {
		suffix = "…"
	}
	return prefix + strings.Join(strings.Fields(body[start:end]), " ") + suffix
}

// migrateLegacy 一次性迁移旧版扁平记忆（JSON 列表）：仅当 wiki 目录为空且
// 旧文件存在时执行，全部条目按时间顺序汇入《用户档案》一页；旧文件保留不删。
func (s *WikiStore) migrateLegacy() {
	if s.dir == "" || s.legacyFile == "" || len(s.pages) > 0 {
		return
	}
	b, err := os.ReadFile(s.legacyFile)
	if err != nil {
		return // 旧文件不存在：无需迁移
	}
	var ms []struct {
		Content   string    `json:"content"`
		Source    string    `json:"source,omitempty"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(b, &ms); err != nil {
		log.Printf("[memory] 旧记忆文件解析失败，跳过迁移: %v", err)
		return
	}
	if len(ms) == 0 {
		return
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].CreatedAt.Before(ms[j].CreatedAt) })
	var body strings.Builder
	body.WriteString("由旧版扁平记忆自动迁移，逐条格式：（日期 · 来源）内容。\n\n")
	for _, m := range ms {
		body.WriteString(fmt.Sprintf("- （%s · %s）%s\n",
			m.CreatedAt.Format("2006-01-02"), sourceLabel(m.Source), m.Content))
	}
	now := s.nextNowLocked()
	p := &Page{ID: s.newIDLocked(), Title: "用户档案", Body: body.String(), Source: "migrate", CreatedAt: now, UpdatedAt: now}
	s.pages[p.ID] = p
	if err := s.writePageLocked(p); err != nil {
		delete(s.pages, p.ID)
		log.Printf("[memory] 旧记忆迁移落盘失败: %v", err)
		return
	}
	log.Printf("[memory] 已从 %s 迁移 %d 条旧记忆 → 页面《用户档案》（原文件保留）", s.legacyFile, len(ms))
}

// sourceLabel 记忆来源的中文标签（工具展示与迁移共用）。
func sourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case "agent":
		return "主动"
	case "admin":
		return "后台"
	case "migrate":
		return "迁移"
	default:
		return "用户"
	}
}
