// Package memory 实现机器人长期记忆：agent 通过 remember/recall/forget 工具
// 记住与回忆用户相关的事实（偏好、习惯、重要信息），管理后台可查看与增删。
// 记忆持久化为 JSON 文件（临时文件 + rename 原子替换），重启不丢。
package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory 是一条长期记忆。
type Memory struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Source    string    `json:"source,omitempty"` // user=用户要求记住 / agent=机器人主动记 / admin=后台手工添加
	CreatedAt time.Time `json:"createdAt"`
}

// Store 是长期记忆仓库。
type Store struct {
	mu       sync.Mutex
	path     string // 为空表示不持久化（测试用）
	memories []Memory
}

// NewStore 创建记忆仓库并从 path 加载；文件不存在或损坏时空启动。
func NewStore(path string) *Store {
	s := &Store{path: path}
	s.load()
	return s
}

// load 从文件加载记忆；损坏时忽略（与任务落盘相同的容错策略）。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var ms []Memory
	if err := json.Unmarshal(b, &ms); err != nil {
		log.Printf("[memory] 记忆文件损坏，已忽略: %v", err)
		return
	}
	s.memories = ms
}

// saveLocked 原子写回（调用方须持有 mu）。
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.memories, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[memory] 持久化失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[memory] 持久化失败: %v", err)
	}
}

// Add 新增一条记忆并落盘，返回新记忆。
func (s *Store) Add(content, source string) (Memory, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Memory{}, fmt.Errorf("记忆内容不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Memory{
		ID:        "m" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Content:   content,
		Source:    source,
		CreatedAt: time.Now(),
	}
	s.memories = append(s.memories, m)
	s.saveLocked()
	return m, nil
}

// List 返回全部记忆，按创建时间倒序（新的在前）。
func (s *Store) List() []Memory {
	return s.Search("")
}

// Search 按关键词子串匹配（不区分大小写）；keyword 为空返回全部。
func (s *Store) Search(keyword string) []Memory {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	s.mu.Lock()
	out := make([]Memory, 0, len(s.memories))
	for _, m := range s.memories {
		if keyword == "" || strings.Contains(strings.ToLower(m.Content), keyword) {
			out = append(out, m)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Recent 返回最近 n 条记忆（按创建时间倒序）。
func (s *Store) Recent(n int) []Memory {
	all := s.List()
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// Delete 按 ID 删除一条记忆，返回是否删除了。
// ID 容忍 "#m..." 形式（recall 展示时带 # 前缀，模型会原样传回）。
func (s *Store) Delete(id string) bool {
	id = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), "#"))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.memories {
		if m.ID == id {
			s.memories = append(s.memories[:i], s.memories[i+1:]...)
			s.saveLocked()
			return true
		}
	}
	return false
}

// Count 返回记忆条数。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.memories)
}
