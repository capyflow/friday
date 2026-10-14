// store.go 是计划的磁盘仓库：一份 plans.json，原子写回，重启自动恢复。
// 沿用仓库既有约定——损坏文件保留并报错（绝不用空数据覆盖）、终态记录限量淘汰、
// 进行中的记录永不淘汰。
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxPlans 是磁盘上保留的计划数上限（终态从最旧开始淘汰）。
const maxPlans = 50

// Store 持有计划集合。零值不可用，请用 NewStore。
type Store struct {
	mu      sync.Mutex
	path    string
	plans   map[string]*Plan
	order   []string // 创建顺序（id）
	loadErr error
}

type snapshot struct {
	Plans []*Plan `json:"plans"`
}

// NewStore 加载磁盘计划。文件缺失按空仓库；损坏时保留原文件并返回错误
// （内存里从空开始，但不会再写回，避免覆盖）。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, plans: map[string]*Plan{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			return s, nil
		}
		s.loadErr = fmt.Errorf("读取计划文件 %q 失败，已保留原文件；修复文件后重启: %w", path, err)
		return s, s.loadErr
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		s.loadErr = fmt.Errorf("计划文件 %q 损坏，已保留原文件；修复文件后重启: %w", path, err)
		return s, s.loadErr
	}
	now := time.Now()
	for i, p := range snap.Plans {
		if p == nil {
			continue
		}
		if err := p.Validate(); err != nil {
			s.loadErr = fmt.Errorf("计划文件 %q 第 %d 条无效，已保留原文件；修复后重启: %v", path, i+1, err)
			s.plans = map[string]*Plan{}
			return s, s.loadErr
		}
		p.Normalize(now)
		// 进程重启时正在跑的步骤不可知（工具副作用可能已发生），一律降为暂停，
		// 由用户决定继续——与通知中心把 processing 退回 pending 同一套语义。
		if p.State == StateRunning {
			p.State = StatePaused
			p.Error = "进程重启时正在执行，已暂停；确认后可从当前步骤继续"
			for j := range p.Steps {
				if p.Steps[j].State == StepRunning {
					p.Steps[j].State = StepPending
				}
			}
		}
		if p.State == StateDraft {
			// 草稿意味着规划轮没走完就退了：没有步骤的计划无从继续。
			p.State = StateFailed
			if p.Error == "" {
				p.Error = "上次规划未完成（没有步骤记录）"
			}
		}
		s.plans[p.ID] = p
		s.order = append(s.order, p.ID)
	}
	return s, nil
}

// uniqueIDLocked 生成不与现有计划冲突的 id（同一微秒内连续创建会撞Nano除数）。
func (s *Store) uniqueIDLocked(now time.Time) string {
	id := newID(now)
	for i := 1; ; i++ {
		if _, exists := s.plans[id]; !exists {
			return id
		}
		id = fmt.Sprintf("%s-%d", newID(now.Add(time.Microsecond*time.Duration(i))), i)
	}
}

// Err 返回目录/文件级错误（单个计划的无效内容在加载时即整体报错）。
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// Create 新建一个草稿计划（步骤待规划轮填入）。
func (s *Store) Create(goal, origin, skill string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	p := &Plan{
		ID: s.uniqueIDLocked(now), Goal: strings.TrimSpace(goal), Origin: origin, Skill: strings.TrimSpace(skill),
		State: StateDraft, CreatedAt: now, UpdatedAt: now,
	}
	p.Normalize(now)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	s.plans[p.ID] = p
	s.order = append(s.order, p.ID)
	if err := s.saveLocked(); err != nil {
		delete(s.plans, p.ID)
		for i := len(s.order) - 1; i >= 0; i-- {
			if s.order[i] == p.ID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
		return nil, err
	}
	copyOf := *p
	return &copyOf, nil
}

// Get 返回计划的深拷贝。
func (s *Store) Get(id string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, fmt.Errorf("计划不存在: %s", id)
	}
	return clone(p), nil
}

// List 返回计划摘要（新建在前）；status 为 active 时只给未终态的，
// awaiting 只给待确认的，其余值给全部。
func (s *Store) List(status string) []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.plans))
	for _, p := range s.plans {
		switch status {
		case "active":
			if !p.Active() {
				continue
			}
		case "awaiting":
			if p.State != StateAwaiting {
				continue
			}
		}
		out = append(out, *clone(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Update 在仓库锁内改一条计划并落盘；fn 返回错误则不改动、不落盘。
// fn 拿到的是真实指针（不要跨调用保存），返回的是拷贝。
func (s *Store) Update(id string, fn func(*Plan) error) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return nil, fmt.Errorf("计划不存在: %s", id)
	}
	before := clone(p)
	if err := fn(p); err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Now()
	if err := p.Validate(); err != nil {
		*p = *before
		return nil, fmt.Errorf("计划更新后无效，已回退: %w", err)
	}
	if err := s.saveLocked(); err != nil {
		*p = *before
		return nil, err
	}
	return clone(p), nil
}

// saveLocked 落盘（调用方持锁）。未配置路径时为空操作；
// 加载阶段就出过错（文件损坏/不可读）时拒绝写回——绝不用内存里的空仓库覆盖原文件。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if s.loadErr != nil {
		return fmt.Errorf("计划文件先前已损坏（%v），已停止写回；修复文件后重启", s.loadErr)
	}
	s.trimLocked()
	list := make([]*Plan, 0, len(s.plans))
	for _, id := range s.order {
		if p, ok := s.plans[id]; ok {
			list = append(list, p)
		}
	}
	data, err := json.MarshalIndent(snapshot{Plans: list}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建计划目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建计划临时文件失败: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("写入计划临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("同步计划临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭计划临时文件失败: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		return fmt.Errorf("设置计划文件权限失败: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("替换计划文件失败: %w", err)
	}
	return nil
}

// trimLocked 淘汰最旧的终态记录到上限，进行中的永不淘汰。
func (s *Store) trimLocked() {
	if len(s.order) <= maxPlans {
		return
	}
	keep := make([]string, 0, len(s.order))
	removable := len(s.order) - maxPlans
	for _, id := range s.order {
		p, ok := s.plans[id]
		if !ok {
			continue
		}
		if removable > 0 && !p.Active() {
			delete(s.plans, id)
			removable--
			continue
		}
		keep = append(keep, id)
	}
	s.order = keep
}

// clone 深拷贝，避免调用方改到内存仓库。
func clone(p *Plan) *Plan {
	out := *p
	out.Steps = append([]Step(nil), p.Steps...)
	for i := range out.Steps {
		out.Steps[i].Tools = append([]string(nil), p.Steps[i].Tools...)
	}
	if p.ConfirmedAt != nil {
		ca := *p.ConfirmedAt
		out.ConfirmedAt = &ca
	}
	if p.Confirmed != nil {
		c := *p.Confirmed
		out.Confirmed = &c
	}
	return &out
}
