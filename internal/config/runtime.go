package config

import (
	"maps"
	"slices"
	"sync"
)

var runtimeInitMu sync.Mutex
var configFileMu sync.Mutex

func (c *Config) runtimeMutex() *sync.RWMutex {
	runtimeInitMu.Lock()
	defer runtimeInitMu.Unlock()
	if c.runtimeMu == nil {
		c.runtimeMu = &sync.RWMutex{}
	}
	return c.runtimeMu
}

// Snapshot 返回独立的运行时配置。热更新字段的切片/映射不与原配置共享。
func (c *Config) Snapshot() *Config {
	mu := c.runtimeMutex()
	mu.RLock()
	defer mu.RUnlock()
	return c.snapshotLocked()
}

func (c *Config) snapshotLocked() *Config {
	next := *c
	next.runtimeMu = nil
	next.Persona.Presets = slices.Clone(c.Persona.Presets)
	next.Provider.Models = slices.Clone(c.Provider.Models)
	for i := range next.Provider.Models {
		next.Provider.Models[i].Inputs = slices.Clone(c.Provider.Models[i].Inputs)
	}
	next.Mcp.Servers = slices.Clone(c.Mcp.Servers)
	for i := range next.Mcp.Servers {
		next.Mcp.Servers[i].Args = slices.Clone(c.Mcp.Servers[i].Args)
		next.Mcp.Servers[i].Env = maps.Clone(c.Mcp.Servers[i].Env)
		next.Mcp.Servers[i].Headers = maps.Clone(c.Mcp.Servers[i].Headers)
	}
	next.Acp.Agents = slices.Clone(c.Acp.Agents)
	for i := range next.Acp.Agents {
		next.Acp.Agents[i].Args = slices.Clone(c.Acp.Agents[i].Args)
		next.Acp.Agents[i].Env = maps.Clone(c.Acp.Agents[i].Env)
	}
	return &next
}

// Apply 串行构建并持久化热更新，所有步骤成功才发布配置。
// fn 使用独立快照，不得递归访问原 Config 的 Snapshot/Apply。
func (c *Config) Apply(fn func(next *Config) error) error {
	mu := c.runtimeMutex()
	mu.Lock()
	defer mu.Unlock()
	next := c.snapshotLocked()
	if err := fn(next); err != nil {
		return err
	}
	c.Provider = next.Provider
	// Language 也要跟着发布：设置页改完语言立刻生效（提示词与外部 agent 的
	// prompt 前置块都读这里的值），不发的话只有 config.yaml 变了、内存里还是旧的。
	c.Language = next.Language
	c.Persona = next.Persona
	c.Permissions = next.Permissions
	c.Agent = next.Agent
	c.Mcp = next.Mcp
	c.Acp = next.Acp
	return nil
}
