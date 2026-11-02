// workspace.go 是「工作区」这一层：一组目录（第一个是主目录）+ 挂在它下面的会话。
//
// 为什么要有这一层：ACP 的一个 session 只有一个 cwd（`session/new { cwd }`），
// 但真实活往往是「一个仓库 + 几个参考目录」。协议为此留了
// `additionalDirectories`（绝对路径，扩展文件可见范围、不改 cwd），并由 agent 在
// initialize 里用 `sessionCapabilities.additionalDirectories` 声明吃不吃这个字段
// ——所以工作区的多目录语义是「主目录当 cwd，其余目录做扩展根」，对不声明的 agent
// 就只用主目录（前端据此明说，不假装多根生效）。
//
// 会话仍然锚定一个主目录（原 D1 的纪律没变）：cwd 记在 thread 上，重接会话时
// 必须与建会话时一致；扩展根则跟着工作区当前的目录走（协议允许 load 时给出
// 完整的新增根列表）。
package acp

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Workspace 是一个工作区：名字 + 一组绝对路径目录 + 归属它的会话。
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Dirs      []string  `json:"dirs"` // 绝对路径；第一个 = 主目录（新建会话时的默认 cwd）
	Pinned    bool      `json:"pinned,omitempty"`
	Policy    string    `json:"policy,omitempty"` // ask | readonly | auto；空值按 ask
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// 工作区审批档位。它只是“新会话/重连时的默认值”，不是硬闸门：
// agent 不支持审批模式时只能记录下来并靠轮前快照兜底。
const (
	WorkspacePolicyAsk      = "ask"
	WorkspacePolicyReadOnly = "readonly"
	WorkspacePolicyAuto     = "auto"
)

// NormalizeWorkspacePolicy 把空值/未知值收成 ask（默认每次都问）。
func NormalizeWorkspacePolicy(p string) string {
	switch strings.TrimSpace(p) {
	case WorkspacePolicyReadOnly:
		return WorkspacePolicyReadOnly
	case WorkspacePolicyAuto:
		return WorkspacePolicyAuto
	default:
		return WorkspacePolicyAsk
	}
}

func validWorkspacePolicy(p string) bool {
	switch strings.TrimSpace(p) {
	case WorkspacePolicyAsk, WorkspacePolicyReadOnly, WorkspacePolicyAuto:
		return true
	}
	return false
}

// WorkspaceListItem 是列表接口回给前端的形态：工作区本体 + 已经不存在的目录。
// missingDirs 是读时算的（用户在 Finder 里删了目录，UI 要能说清"这个源文件夹没了"，
// 而不是让会话主目录显示成一条无效路径），不写进投影文件。
type WorkspaceListItem struct {
	Workspace
	MissingDirs []string `json:"missingDirs,omitempty"`
}

// WorkspaceList 返回全部工作区（列表接口用）并逐个标注缺失目录。
func (m *Manager) WorkspaceList() []WorkspaceListItem {
	out := make([]WorkspaceListItem, 0)
	m.mu.Lock()
	for _, id := range m.wsOrder {
		ws := m.workspaces[id]
		if ws == nil {
			continue
		}
		item := WorkspaceListItem{Workspace: copyWorkspace(ws)}
		for _, d := range ws.Dirs {
			if info, err := os.Stat(d); err != nil || !info.IsDir() {
				item.MissingDirs = append(item.MissingDirs, d)
			}
		}
		out = append(out, item)
	}
	m.mu.Unlock()
	return out
}

// ListWorkspaces 返回全部工作区（元数据，不含缺失标注；按创建顺序，
// 置顶与最近活动排序是展示层的事，见 web/src/components/WorkspaceView.vue）。
func (m *Manager) ListWorkspaces() []Workspace {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Workspace, 0, len(m.wsOrder))
	for _, id := range m.wsOrder {
		if ws := m.workspaces[id]; ws != nil {
			out = append(out, copyWorkspace(ws))
		}
	}
	return out
}

// GetWorkspace 返回单个工作区。
func (m *Manager) GetWorkspace(id string) (Workspace, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return Workspace{}, false
	}
	return copyWorkspace(ws), true
}

// CreateWorkspace 新建工作区：dirs 为绝对路径或 ~ 前缀（逐个解析并确保存在），
// 一个都不给就回退到数据目录下的临时会话目录 acp-sessions/<工作区ID>——
// 「还没想好目录也能开聊」的默认留在这一层。
func (m *Manager) CreateWorkspace(name string, dirs []string) (Workspace, error) {
	c := m.cfgFn().Normalized()
	if !c.Enabled {
		return Workspace{}, errors.New("智能体功能未开启（设置 → 智能体）")
	}
	ws := &Workspace{
		ID:        randHex(8),
		Name:      strings.TrimSpace(name),
		Policy:    WorkspacePolicyAsk,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	resolved := make([]string, 0, len(dirs))
	for _, d := range dirs {
		p := strings.TrimSpace(d)
		if p == "" {
			continue
		}
		abs, err := resolveDir(p, "")
		if err != nil {
			return Workspace{}, err
		}
		resolved = appendUnique(resolved, abs)
	}
	if len(resolved) == 0 {
		base, err := sessionsBaseDirFn()
		if err != nil {
			return Workspace{}, err
		}
		abs, err := resolveDir(filepath.Join(base, ws.ID), "")
		if err != nil {
			return Workspace{}, err
		}
		resolved = append(resolved, abs)
	}
	ws.Dirs = resolved
	if ws.Name == "" {
		ws.Name = filepath.Base(resolved[0])
	}
	m.mu.Lock()
	m.workspaces[ws.ID] = ws
	m.wsOrder = append(m.wsOrder, ws.ID)
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		log.Printf("[智能体] 工作区落盘失败: %v", err)
	}
	log.Printf("[智能体] 新建工作区 %s（%s，%d 个目录）", ws.ID, ws.Name, len(ws.Dirs))
	return copyWorkspace(ws), nil
}

// RenameWorkspace 改名（只影响展示，不动目录与会话）；name 留空回到主目录名。
func (m *Manager) RenameWorkspace(id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return fmt.Errorf("未找到工作区 %s", id)
	}
	if name = strings.TrimSpace(name); name == "" {
		name = filepath.Base(ws.Dirs[0])
	}
	ws.Name, ws.UpdatedAt = name, time.Now()
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 工作区改名落盘失败: %v", err)
	}
	return nil
}

// SetPinned 置顶/取消置顶工作区（只影响列表排序，不动目录与会话）。
func (m *Manager) SetPinned(id string, pinned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return fmt.Errorf("未找到工作区 %s", id)
	}
	ws.Pinned, ws.UpdatedAt = pinned, time.Now()
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 工作区置顶落盘失败: %v", err)
	}
	return nil
}

// SetPolicy 设置工作区的默认审批档位（新会话/重连时尝试下发，手动选择优先）。
func (m *Manager) SetPolicy(id, policy string) error {
	policy = strings.TrimSpace(policy)
	if !validWorkspacePolicy(policy) {
		return fmt.Errorf("审批档位只支持 ask / readonly / auto，收到 %q", policy)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return fmt.Errorf("未找到工作区 %s", id)
	}
	ws.Policy, ws.UpdatedAt = policy, time.Now()
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 工作区审批档位落盘失败: %v", err)
	}
	return nil
}

// SetPrimaryDir 把工作区里的某个目录提为主目录（挪到 Dirs 首位）。
// 已建会话仍锚在自己的 cwd 上（D1），换主目录只影响"以后新建会话默认落在哪"
// 与扩展根的计算，不会把既有会话的上下文换脑子。
func (m *Manager) SetPrimaryDir(id, path string) (Workspace, error) {
	abs, err := resolveDir(strings.TrimSpace(path), "")
	if err != nil {
		return Workspace{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return Workspace{}, fmt.Errorf("未找到工作区 %s", id)
	}
	if !containsStr(ws.Dirs, abs) {
		return Workspace{}, fmt.Errorf("目录 %s 不在该工作区里（先「添加目录」再加进来）", abs)
	}
	if ws.Dirs[0] != abs {
		rest := make([]string, 0, len(ws.Dirs)-1)
		for _, d := range ws.Dirs {
			if d != abs {
				rest = append(rest, d)
			}
		}
		ws.Dirs = append([]string{abs}, rest...)
		ws.UpdatedAt = time.Now()
		if err := m.saveLocked(); err != nil {
			log.Printf("[智能体] 工作区换主目录落盘失败: %v", err)
		}
	}
	return copyWorkspace(ws), nil
}

// AddWorkspaceDir 往工作区追加一个目录（已在里面就原样返回，不重复）。
func (m *Manager) AddWorkspaceDir(id, path string) (Workspace, error) {
	abs, err := resolveDir(strings.TrimSpace(path), "")
	if err != nil {
		return Workspace{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return Workspace{}, fmt.Errorf("未找到工作区 %s", id)
	}
	if !containsStr(ws.Dirs, abs) {
		ws.Dirs = append(ws.Dirs, abs)
		ws.UpdatedAt = time.Now()
		if err := m.saveLocked(); err != nil {
			log.Printf("[智能体] 工作区加目录落盘失败: %v", err)
		}
	}
	return copyWorkspace(ws), nil
}

// RemoveWorkspaceDir 从工作区移出一个目录（不碰磁盘上的目录本身）。
// 会话锚定的主目录被移出也允许：cwd 记在会话上，历史与上下文都不受影响，
// 只是这个目录不再是该会话的扩展根之一。
func (m *Manager) RemoveWorkspaceDir(id, path string) (Workspace, error) {
	abs, err := resolveDir(strings.TrimSpace(path), "")
	if err != nil {
		return Workspace{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return Workspace{}, fmt.Errorf("未找到工作区 %s", id)
	}
	kept := make([]string, 0, len(ws.Dirs))
	for _, d := range ws.Dirs {
		if d != abs {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return Workspace{}, errors.New("工作区至少要留一个目录（会话的主目录从这些目录里选）")
	}
	ws.Dirs, ws.UpdatedAt = kept, time.Now()
	if err := m.saveLocked(); err != nil {
		log.Printf("[智能体] 工作区移目录落盘失败: %v", err)
	}
	return copyWorkspace(ws), nil
}

// DeleteWorkspace 删除工作区：连带删掉它下面的会话（投影 + 存活进程）。
// 磁盘上的目录与文件一律不碰——工作区只是 Friday 侧的分组。
func (m *Manager) DeleteWorkspace(id string) bool {
	m.mu.Lock()
	ws := m.workspaces[id]
	if ws == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.workspaces, id)
	for i, v := range m.wsOrder {
		if v == id {
			m.wsOrder = append(m.wsOrder[:i], m.wsOrder[i+1:]...)
			break
		}
	}
	var doomed []*Thread
	var touched []*procState // 这条工作区里的会话所在连接（摘完会话后空的才回收）
	for thID, th := range m.threads {
		if th.WorkspaceID != id {
			continue
		}
		doomed = append(doomed, th)
		delete(m.threads, thID)
		delete(m.locks, thID)
		if lv := m.live[thID]; lv != nil {
			if lv.ps != nil {
				lv.ps.dropThread(thID)
				if !containsProc(touched, lv.ps) {
					touched = append(touched, lv.ps)
				}
			}
			delete(m.live, thID)
		}
	}
	for _, th := range doomed {
		for i, v := range m.order {
			if v == th.ID {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	err := m.saveLocked()
	m.mu.Unlock()
	// 连接是按 agent+目录共享的：只有上面一个会话都不剩的连接才回收
	for _, ps := range touched {
		ps.mu.Lock()
		left := len(ps.owners)
		ps.mu.Unlock()
		if left == 0 {
			m.killProc(ps.key, "删除工作区")
		}
	}
	if err != nil {
		log.Printf("[智能体] 删除工作区落盘失败: %v", err)
	}
	log.Printf("[智能体] 已删除工作区 %s（连带 %d 个会话）", id, len(doomed))
	return true
}

// ThreadsInWorkspace 返回某工作区里的会话 id（删除工作区时前端要各自的落点）。
func (m *Manager) ThreadsInWorkspace(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, thID := range m.order {
		if th := m.threads[thID]; th != nil && th.WorkspaceID == id {
			out = append(out, thID)
		}
	}
	return out
}

func containsProc(list []*procState, want *procState) bool {
	for _, ps := range list {
		if ps == want {
			return true
		}
	}
	return false
}

// workspaceDirs 返回该工作区当前的目录（绝对路径，副本）。
func (m *Manager) workspaceDirs(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.workspaces[id]
	if ws == nil {
		return nil
	}
	return append([]string{}, ws.Dirs...)
}

// extraDirs 算出该会话除主目录以外的扩展根：工作区当前目录去掉 cwd，
// 再把已经不存在的目录剔掉（agent 收到不存在的路径会直接拒绝建会话）。
func (m *Manager) extraDirs(th *Thread) []string {
	if th.WorkspaceID == "" {
		return nil
	}
	var out []string
	for _, d := range m.workspaceDirs(th.WorkspaceID) {
		if d == "" || d == th.Cwd {
			continue
		}
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, d)
	}
	return out
}

// extraDirsFor 只在 agent 声明支持 additionalDirectories 时给出扩展根：不吃这个
// 字段的 agent 就只拿 cwd，多出的目录不硬塞（也不在前端假装生效）。
func (m *Manager) extraDirsFor(proc *agentProc, th *Thread) []string {
	if proc == nil || !proc.extraRoots {
		return nil
	}
	return m.extraDirs(th)
}

// SessionDirs 汇总某会话的目录视图：主目录、工作区全部目录、实际下发的扩展根，
// 以及这个答案是否来自一次真实握手（进程不在场时 capability 未知——它来自
// initialize，没拉起就确实不知道，前端据此既不宣称也不否定多目录生效）。
func (m *Manager) SessionDirs(id string) (cwd string, dirs, extra []string, known, supported bool) {
	m.mu.Lock()
	th := m.threads[id]
	if th == nil {
		m.mu.Unlock()
		return "", nil, nil, false, false
	}
	cwd = th.Cwd
	if ws := m.workspaces[th.WorkspaceID]; ws != nil {
		dirs = append([]string{}, ws.Dirs...)
	}
	if lv := m.live[id]; lv != nil && lv.proc != nil {
		known, supported = true, lv.proc.extraRoots
		anchor := *th // extraDirs 只看 Cwd/WorkspaceID，拿副本避免跨锁持有指针
		m.mu.Unlock()
		if supported {
			extra = m.extraDirs(&anchor)
		}
		return
	}
	m.mu.Unlock()
	return cwd, dirs, nil, false, false
}

// workspaceForDir 迁移用：同目录的旧 thread 归到同一个工作区，找不到就建一个。
// 调用方须持 m.mu。
func (m *Manager) workspaceForDirLocked(dir string) *Workspace {
	for _, id := range m.wsOrder {
		ws := m.workspaces[id]
		if ws != nil && containsStr(ws.Dirs, dir) {
			return ws
		}
	}
	now := time.Now()
	name := filepath.Base(dir)
	if name == "" || name == string(filepath.Separator) {
		name = "工作区"
	}
	ws := &Workspace{ID: randHex(8), Name: name, Dirs: []string{dir},
		CreatedAt: now, UpdatedAt: now}
	m.workspaces[ws.ID] = ws
	m.wsOrder = append(m.wsOrder, ws.ID)
	return ws
}

// resolveDir 展开 ~、取绝对路径、必要时建目录（与旧版选目录的行为一致）。
// fallback 非空且 path 为空时用 fallback；两者都空则报错（错误文案带下一步）。
func resolveDir(path, fallback string) (string, error) {
	ws := strings.TrimSpace(path)
	if ws == "" {
		ws = strings.TrimSpace(fallback)
	}
	if ws == "" {
		return "", errors.New("请填写目录（agent 在这里干活，会写入文件）")
	}
	if ws == "~" || strings.HasPrefix(ws, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 HOME 失败: %w", err)
		}
		ws = filepath.Join(home, strings.TrimPrefix(ws, "~"))
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	if info, err := os.Stat(abs); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("检查目录失败: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", fmt.Errorf("创建目录失败: %w", err)
		}
	} else if !info.IsDir() {
		return "", fmt.Errorf("目录 %s 不是目录", abs)
	}
	return abs, nil
}

func copyWorkspace(ws *Workspace) Workspace {
	cp := *ws
	cp.Dirs = append([]string{}, ws.Dirs...)
	return cp
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func appendUnique(list []string, want string) []string {
	if containsStr(list, want) {
		return list
	}
	return append(list, want)
}
