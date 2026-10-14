// store.go 负责技能目录的扫描、加载与写入：项目目录可写、用户目录只读，
// 一份 SKILL.md 就是全部真相（不另存 sidecar、不写 config.yaml）。
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"gopkg.in/yaml.v3"
)

// Store 是技能仓库。零值不可用，请用 NewStore。
type Store struct {
	mu      sync.RWMutex
	dir     string // 可写：默认 <数据目录>/skills
	userDir string // 只读：$HOME/.vortex/skills
	entries []*Entry
	byName  map[string]*Entry
	byID    map[string]*Entry
	loadErr error
}

// NewStore 创建仓库并首次扫描。dir 为空表示不持久化（测试用纯内存）。
func NewStore(dir string) *Store {
	s := &Store{dir: dir, byName: map[string]*Entry{}, byID: map[string]*Entry{}}
	if home, err := os.UserHomeDir(); err == nil {
		s.userDir = filepath.Join(home, ".vortex", "skills")
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.loadErr = fmt.Errorf("创建技能目录 %q 失败: %w", dir, err)
		}
	}
	s.reloadLocked()
	return s
}

// Err 返回目录级错误（技能目录不可读等）；单个文件的错误在各自的 ParseErr 里。
func (s *Store) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// Dir 返回可写技能目录。
func (s *Store) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

// Reload 重新扫描两个目录，返回最新列表摘要。
func (s *Store) Reload() []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.summariesLocked()
}

// reloadLocked 重新扫描；调用方须持写锁。
func (s *Store) reloadLocked() {
	entries := make([]*Entry, 0, 8)
	// 项目目录先扫：同名技能项目覆盖用户（与框架 SkillManager 的优先级一致）。
	for _, scan := range []struct {
		dir, scope string
	}{
		{s.dir, ScopeProject},
		{s.userDir, ScopeUser},
	} {
		if scan.dir == "" {
			continue
		}
		dirs, err := os.ReadDir(scan.dir)
		if err != nil {
			if !os.IsNotExist(err) && scan.scope == ScopeProject {
				s.loadErr = fmt.Errorf("读取技能目录 %q 失败: %w", scan.dir, err)
			}
			continue
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			path := filepath.Join(scan.dir, d.Name(), "SKILL.md")
			if _, err := os.Stat(path); err != nil {
				continue // 目录里没有 SKILL.md：不是技能包
			}
			entries = append(entries, loadEntry(d.Name(), path, scan.scope))
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Scope != entries[j].Scope {
			return entries[i].Scope == ScopeProject // 项目在前
		}
		if entries[i].Extra.Priority != entries[j].Extra.Priority {
			return entries[i].Extra.Priority > entries[j].Extra.Priority
		}
		return entries[i].ID < entries[j].ID
	})

	byName := map[string]*Entry{}
	byID := map[string]*Entry{}
	for _, e := range entries {
		byID[e.ID] = e
		if e.ParseErr != "" || e.Name() == "" {
			continue
		}
		if prev, dup := byName[e.Name()]; dup {
			// 重名不静默丢弃：后来者标记原因，让用户在后台看得见改哪个。
			if prev.Scope == e.Scope {
				e.Note = fmt.Sprintf("技能名与 %q 重复，本条不参与选择——请改 name 或删除其一", prev.ID)
			} else {
				e.Note = fmt.Sprintf("已被 %s 目录的同名技能 %q 覆盖", e.Scope, prev.ID)
				byID[e.ID] = e
				continue
			}
			continue
		}
		byName[e.Name()] = e
	}
	s.entries, s.byName, s.byID = entries, byName, byID
}

func (s *Store) summariesLocked() []Summary {
	out := make([]Summary, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Summarize())
	}
	return out
}

// List 返回全部技能摘要（含解析失败的，后台要能解释）。
func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.summariesLocked()
}

// Candidates 返回可参与本轮选择的技能（启用、有效、未被覆盖）。
func (s *Store) Candidates() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Usable() {
			out = append(out, e)
		}
	}
	return out
}

// Get 按 ID 取技能副本摘要。
func (s *Store) Get(id string) (Summary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	if !ok {
		return Summary{}, false
	}
	return e.Summarize(), true
}

// FindName 按技能名取条目（显式 /skill 名 用）。先精确匹配，再大小写折叠匹配，
// 让 "/skill Morning" 也能命中名为 "morning" 的技能。
func (s *Store) FindName(name string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	if e, ok := s.byName[name]; ok {
		return e, true
	}
	for _, e := range s.byName {
		if strings.EqualFold(e.Name(), name) {
			return e, true
		}
	}
	return nil, false
}

// Raw 返回技能文件原文（后台编辑器用）。
func (s *Store) Raw(id string) (string, error) {
	s.mu.RLock()
	e, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("技能不存在: %s", id)
	}
	data, err := os.ReadFile(e.Path)
	if err != nil {
		return "", fmt.Errorf("读取技能文件失败: %w", err)
	}
	return string(data), nil
}

// Save 写入/覆盖一个项目技能（校验后原子落盘并热生效）。id 为空时按技能名生成。
func (s *Store) Save(id, raw string) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return Summary{}, fmt.Errorf("未配置技能目录，无法保存")
	}
	if err := Validate(raw); err != nil {
		return Summary{}, err
	}
	proposed, err := parseEntry("probe", filepath.Join(s.dir, "probe", "SKILL.md"), ScopeProject, raw)
	if err != nil {
		return Summary{}, err
	}
	name := proposed.Name()
	newID := strings.TrimSpace(id)
	if newID == "" {
		base := suggestID(name)
		newID = base
		// suggestID 的时间戳回落在同一毫秒内会重复，顺延到未被占用的目录名
		for seq := 2; ; seq++ {
			if _, taken := s.byID[newID]; !taken {
				break
			}
			newID = fmt.Sprintf("%s-%d", base, seq)
		}
	}
	if !validSkillID(newID) {
		return Summary{}, fmt.Errorf("技能目录名只能包含字母、数字、下划线和连字符：%q", newID)
	}
	// 重名检查（排除自身 id）
	if other, ok := s.byName[name]; ok && other.ID != newID {
		return Summary{}, fmt.Errorf("技能名 %q 已被 %q 使用，请换个名字或先删除那条", name, other.ID)
	}

	path := filepath.Join(s.dir, newID, "SKILL.md")
	if err := writeFileAtomic(path, raw); err != nil {
		return Summary{}, err
	}
	s.reloadLocked()
	if e, ok := s.byID[newID]; ok {
		return e.Summarize(), nil
	}
	return Summary{}, fmt.Errorf("技能已写入但未能加载：%s", path)
}

// SetEnabled 只改写 frontmatter 的 enabled 键，保留正文与其余注释。
func (s *Store) SetEnabled(id string, enabled bool) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return Summary{}, fmt.Errorf("技能不存在: %s", id)
	}
	if e.Scope == ScopeUser {
		return Summary{}, fmt.Errorf("用户目录技能（%s）在本机 $HOME/.vortex/skills 下，后台只读", id)
	}
	raw, err := os.ReadFile(e.Path)
	if err != nil {
		return Summary{}, fmt.Errorf("读取技能文件失败: %w", err)
	}
	updated, err := setFrontmatterKey(string(raw), "enabled", enabled)
	if err != nil {
		return Summary{}, err
	}
	if err := Validate(updated); err != nil {
		return Summary{}, err
	}
	if err := writeFileAtomic(e.Path, updated); err != nil {
		return Summary{}, err
	}
	s.reloadLocked()
	if again, ok := s.byID[id]; ok {
		return again.Summarize(), nil
	}
	return Summary{}, fmt.Errorf("已保存但未能重新加载技能 %s", id)
}

// Delete 移除一个项目技能。先把目录改名成 .disabled-<ts> 再删除，
// 避免"删一半"留下没有 SKILL.md 的空壳或被误当技能的残骸。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("技能不存在: %s", id)
	}
	if e.Scope == ScopeUser {
		return fmt.Errorf("用户目录技能（%s）由本机 $HOME/.vortex/skills 管理，后台不删", id)
	}
	dir := filepath.Dir(e.Path)
	stash := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".disabled-"+
		fmt.Sprint(time.Now().UnixNano()))
	if err := os.Rename(dir, stash); err != nil {
		return fmt.Errorf("移除技能目录失败: %w", err)
	}
	if err := os.RemoveAll(stash); err != nil {
		return fmt.Errorf("技能目录已移出但清理失败（不影响生效）: %w", err)
	}
	s.reloadLocked()
	return nil
}

// loadEntry 读取一个 SKILL.md 并生成条目；任何失败都写进 ParseErr（保留原文件）。
func loadEntry(id, path, scope string) *Entry {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Entry{ID: id, Path: path, Scope: scope,
			ParseErr: fmt.Sprintf("读取失败：%v", err)}
	}
	e, err := parseEntry(id, path, scope, string(data))
	if err != nil {
		return &Entry{ID: id, Path: path, Scope: scope, ParseErr: err.Error()}
	}
	return e
}

// parseEntry 解析一份 SKILL.md 文本。框架字段用框架自己的导出类型
// （vxagent.SkillMetadata，字段名与 yaml tag 由上游定义），Friday 扩展键用本包的
// Extra；不读磁盘，因此后台保存前就能校验同一套规则。
func parseEntry(id, path, scope, raw string) (*Entry, error) {
	if scope == ScopeProject && !validSkillID(id) {
		return nil, fmt.Errorf("技能目录名 %q 非法（只能含字母/数字/下划线/连字符）", id)
	}
	if err := Validate(raw); err != nil {
		return nil, err
	}
	metaText, body, _ := frontmatterParts(raw)
	var meta vxagent.SkillMetadata
	if err := yaml.Unmarshal([]byte(metaText), &meta); err != nil {
		return nil, fmt.Errorf("框架字段解析失败：%v", err)
	}
	var extra Extra
	if err := yaml.Unmarshal([]byte(metaText), &extra); err != nil {
		return nil, fmt.Errorf("扩展键解析失败：%v", err)
	}
	meta.Name = strings.TrimSpace(meta.Name)
	return &Entry{
		ID: id, Path: path, Scope: scope, Extra: extra,
		Skill: &vxagent.Skill{Metadata: meta, Content: body, Path: path, Scope: scope},
	}, nil
}

// validSkillID 限制目录名字符集，防路径穿越（与记忆页 ID 同一套约束）。
func validSkillID(id string) bool {
	if id == "" || id == "." || id == ".." {
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

// suggestID 由技能名生成目录名：ASCII 名直接规范化，中文名回落到 skill-<时间戳>。
func suggestID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 3 {
		return slug
	}
	return fmt.Sprintf("skill-%d", time.Now().UnixMilli())
}

// writeFileAtomic 原子写（临时文件 + Sync + Rename），先建父目录。
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建技能目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建技能临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("写入技能临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步技能临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭技能临时文件失败: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		return fmt.Errorf("设置技能文件权限失败: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("替换技能文件失败: %w", err)
	}
	return nil
}

// setFrontmatterKey 改一个 frontmatter 键，保留其它键、顺序与注释。
func setFrontmatterKey(raw, key string, value bool) (string, error) {
	metaText, body, ok := frontmatterParts(raw)
	if !ok {
		return "", fmt.Errorf("技能文件缺少 --- 包裹的 frontmatter，无法改 %s", key)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(metaText), &doc); err != nil {
		return "", fmt.Errorf("frontmatter 不是合法 YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 ||
		doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("frontmatter 顶层须是键值映射")
	}
	mapNode := doc.Content[0]
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(value)}
	found := false
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			mapNode.Content[i+1] = valNode
			found = true
			break
		}
	}
	if !found {
		mapNode.Content = append(mapNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, valNode)
	}
	out, err := yaml.Marshal(mapNode)
	if err != nil {
		return "", fmt.Errorf("序列化 frontmatter 失败: %w", err)
	}
	return "---\n" + string(out) + "---\n\n" + body + "\n", nil
}
