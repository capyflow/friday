package agent

import (
	"fmt"
	"slices"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/ctxengine"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// ReloadProvider 热更新 LLM 供应商配置（baseURL/model/apiKey）：
//  1. 在串行配置事务中构建独立快照与新 Agent；
//  2. 等待在途请求结束，先写回 config.yaml（保留注释）；
//  3. 全部成功后发布配置与 Agent，写回失败保留原运行状态。
//
// apiKey 为空串表示不改已有密钥；非空时写盘（数据目录用户私有）。
func ReloadProvider(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue, baseURL, model, apiKey string) error {
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || model == "" {
		return fmt.Errorf("baseURL 与 model 不能为空")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return fmt.Errorf("baseURL 必须以 http:// 或 https:// 开头")
	}

	return cfg.Apply(func(next *config.Config) error {
		previous := next.Snapshot()
		if apiKey == "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(previous.Provider.BaseURL, "/") && previous.ProviderAPIKey() != "" {
			return fmt.Errorf("更换 API 地址时请填写该服务的 API Key")
		}
		next.Provider.BaseURL, next.Provider.Model = baseURL, model
		if apiKey != "" {
			next.Provider.APIKey = apiKey
		}
		return brain.Reconfigure(func() (*vxagent.Agent, error) {
			var engine *ctxengine.Engine
			mgr := brain.CtxMgr()
			if mgr != nil {
				if err := mgr.Rebuild(next); err != nil {
					return nil, fmt.Errorf("重建上下文引擎失败: %w", err)
				}
				engine = mgr.Current()
			}
			newAg, err := Build(next, registry, brain.Pool(), approver, engine)
			if err == nil {
				err = config.UpdateProvider(configPath, baseURL, model, apiKey)
			}
			if err != nil {
				if mgr != nil {
					if restoreErr := mgr.Rebuild(previous); restoreErr != nil {
						return nil, fmt.Errorf("更新失败: %v；恢复上下文引擎失败: %w", err, restoreErr)
					}
				}
				return nil, err
			}
			return newAg, nil
		})
	})
}

// ReloadProviderModels 保存模型库（provider.models）并激活指定条目：
//  1. 校验条目完备性（name 唯一、baseURL/model 非空、inputs 合法、active 命中），
//     并做密钥继承：前端传空 + 旧库同名条目有 key → 沿用旧 key（"留空=保留"）；
//  2. 把激活条目套用到扁平字段，在串行配置事务里按变化范围决定重建量：
//     地址/模型/生效密钥/窗口任一变了 → 重建上下文引擎与 Agent（窗口固化在
//     engine Config，一并重建）；只编辑了非激活条目或输入格式声明 → 只写盘；
//  3. 先写回 config.yaml（保留注释），全部成功后发布；失败保留原运行状态。
//
// 与 ReloadProvider 不同，库内切换不强制重填密钥——每条档案自带密钥（或继承）。
func ReloadProviderModels(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue, active string, models []config.ModelProfile) error {
	active = strings.TrimSpace(active)
	return cfg.Apply(func(next *config.Config) error {
		previous := next.Snapshot()
		oldByName := map[string]config.ModelProfile{}
		for _, m := range previous.Provider.Models {
			oldByName[m.Name] = m
		}

		cleaned := make([]config.ModelProfile, 0, len(models))
		seen := map[string]bool{}
		for _, m := range models {
			m.Name = strings.TrimSpace(m.Name)
			m.BaseURL = strings.TrimSpace(m.BaseURL)
			m.Model = strings.TrimSpace(m.Model)
			m.APIKey = strings.TrimSpace(m.APIKey)
			m.APIKeyEnv = strings.TrimSpace(m.APIKeyEnv)
			if m.Name == "" || m.BaseURL == "" || m.Model == "" {
				return fmt.Errorf("模型条目的名称、API 地址与模型名都不能为空")
			}
			if !strings.HasPrefix(m.BaseURL, "http://") && !strings.HasPrefix(m.BaseURL, "https://") {
				return fmt.Errorf("模型 %s 的 API 地址必须以 http:// 或 https:// 开头", m.Name)
			}
			if seen[m.Name] {
				return fmt.Errorf("模型名称重复: %s", m.Name)
			}
			seen[m.Name] = true
			inputs := make([]string, 0, len(m.Inputs))
			for _, in := range m.Inputs {
				in = strings.TrimSpace(in)
				if in == "" {
					continue
				}
				if in != config.InputText && in != config.InputImage {
					return fmt.Errorf("模型 %s 的输入格式 %q 不支持（text/image）", m.Name, in)
				}
				if !slices.Contains(inputs, in) {
					inputs = append(inputs, in)
				}
			}
			m.Inputs = inputs
			if m.ContextWindow < 0 {
				m.ContextWindow = 0
			}
			if m.MaxTokens < 0 {
				m.MaxTokens = 0
			}
			if m.APIKey == "" { // 留空 = 保留该条目已存密钥
				if old, ok := oldByName[m.Name]; ok {
					m.APIKey = old.APIKey
					if m.APIKeyEnv == "" {
						m.APIKeyEnv = old.APIKeyEnv
					}
				}
			}
			cleaned = append(cleaned, m)
		}
		if !seen[active] {
			return fmt.Errorf("激活的模型 %q 不在列表里", active)
		}
		var entry config.ModelProfile
		for _, m := range cleaned {
			if m.Name == active {
				entry = m
				break
			}
		}

		// 激活条目套用到扁平字段（投影）：从现值出发，条目未覆盖的维度沿用现状。
		prev := previous.Provider
		projected := prev
		projected.BaseURL = entry.BaseURL
		projected.Model = entry.Model
		projected.ContextWindow = entry.ContextWindow
		// 输出预算：条目填了才覆盖扁平值——条目留空时沿用现状（现状缺省即
		// config.DefaultMaxTokens），避免保存模型库把用户手写的 provider.maxTokens 打回默认。
		if entry.MaxTokens > 0 {
			projected.MaxTokens = entry.MaxTokens
		}
		projected.Active = active
		switch { // 密钥套用：条目直填 > 条目环境变量 > 沿用全局现状
		case entry.APIKey != "":
			projected.APIKey = entry.APIKey
		case entry.APIKeyEnv != "":
			projected.APIKeyEnv = entry.APIKeyEnv
			projected.APIKey = ""
		}
		projected.Models = cleaned

		rebuild := strings.TrimRight(projected.BaseURL, "/") != strings.TrimRight(prev.BaseURL, "/") ||
			projected.Model != prev.Model ||
			projected.ResolveAPIKey() != prev.ResolveAPIKey() ||
			projected.ContextWindow != prev.ContextWindow ||
			projected.MaxTokens != prev.MaxTokens

		next.Provider = projected

		if !rebuild { // 非激活条目/输入格式声明变化：与运行态无关，落盘即可
			return config.UpdateProviderModels(configPath, next.Provider)
		}
		return brain.Reconfigure(func() (*vxagent.Agent, error) {
			var engine *ctxengine.Engine
			mgr := brain.CtxMgr()
			if mgr != nil {
				if err := mgr.Rebuild(next); err != nil {
					return nil, fmt.Errorf("重建上下文引擎失败: %w", err)
				}
				engine = mgr.Current()
			}
			newAg, err := Build(next, registry, brain.Pool(), approver, engine)
			if err == nil {
				err = config.UpdateProviderModels(configPath, next.Provider)
			}
			if err != nil {
				if mgr != nil {
					if restoreErr := mgr.Rebuild(previous); restoreErr != nil {
						return nil, fmt.Errorf("更新失败: %v；恢复上下文引擎失败: %w", err, restoreErr)
					}
				}
				return nil, err
			}
			return newAg, nil
		})
	})
}

// ReloadPermissions 切换全局权限模式（confirm / acceptEdits / fullAccess）并热生效。
//
// 走的是「层级生效」而不是整体重建 Agent：重建要等本轮对话释放读锁（brain.rw），
// 对话中点切换会把 HTTP 请求一直挂到本轮跑完，而本轮用的还是旧档位。现在的次序：
//  1. 校验模式并先写回 config.yaml（写不进去就不动运行态）；
//  2. 更新配置快照（GET /api/permissions 立刻读到新值）；
//  3. 编辑免审标记当场翻转——acceptEdits/fullAccess 下 write/edit/mcp_edit 的内部
//     审批立刻放行，正在进行的这一轮也算数；
//  4. 命令审批档位对活着的权限 Checker 做 SetMode（后台一次，见
//     ApplyPermissionModeNow）；只有查不到活 Checker 时才退回重建 Agent。
func ReloadPermissions(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue, mode string) error {
	mode, err := config.NormalizePermissionMode(mode)
	if err != nil {
		return err
	}
	if cfg.Snapshot().Permissions.EffectiveMode() == mode {
		return nil // 同模式重复切换：无变化
	}
	if err := config.UpdatePermissionMode(configPath, mode); err != nil {
		return err
	}
	if err := cfg.Apply(func(next *config.Config) error {
		next.Permissions.Mode = mode
		return nil
	}); err != nil {
		return err
	}
	if approver != nil {
		approver.SetEditsFree(cfg.Snapshot().Permissions.EditsAutoApproved())
	}
	if ApplyPermissionModeNow(cfg, approver) {
		return nil
	}
	// 兜底：没记录过活 Checker（例如测试里直接拼了个队列）才重建 Agent。
	return rebuildForPermissions(cfg, configPath, registry, brain, approver)
}

// rebuildForPermissions 是切换权限模式的旧路径：重建 Agent 让权限 Checker 跟着换。
// 它会等本轮对话读锁，只在 ApplyPermissionModeNow 落不到时使用。
func rebuildForPermissions(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue) error {
	mode := cfg.Snapshot().Permissions.EffectiveMode()
	return brain.Reconfigure(func() (*vxagent.Agent, error) {
		var engine *ctxengine.Engine
		if mgr := brain.CtxMgr(); mgr != nil {
			engine = mgr.Current()
		}
		newAg, err := Build(cfg.Snapshot(), registry, brain.Pool(), approver, engine)
		if err != nil {
			return nil, err
		}
		if err := config.UpdatePermissionMode(configPath, mode); err != nil {
			return nil, err
		}
		return newAg, nil
	})
}

// ReloadPersona 切换人设并保存自定义人设预设：
//  1. 校验 active 必须命中（内置预设 + 自定义 presets 合集），防止切到不存在的人设；
//  2. 用独立配置快照构建 Agent，先写回 config.yaml（保留注释）；
//  3. 成功后发布人设与 Agent，会话保持连续；失败保留原运行状态。
//
// presets 传 nil 表示仅切换人设、保留 config.yaml 中已有的自定义预设。
func ReloadPersona(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue, active string, presets []config.Persona) error {
	active = strings.TrimSpace(active)
	if active == "" {
		return fmt.Errorf("人设 ID 不能为空")
	}
	if presets != nil {
		for _, p := range presets {
			if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
				return fmt.Errorf("自定义人设的 id 与 name 不能为空")
			}
			if p.ID == active && strings.TrimSpace(p.Prompt) == "" {
				return fmt.Errorf("人设 %s 的提示词正文不能为空", p.ID)
			}
		}
	}
	return cfg.Apply(func(next *config.Config) error {
		next.Persona.Active = active
		if presets != nil {
			next.Persona.Presets = append([]config.Persona{}, presets...)
		}
		known := map[string]bool{}
		for _, p := range BuiltinPersonas() {
			known[p.ID] = true
		}
		for _, p := range next.Persona.Presets {
			known[p.ID] = true
		}
		if !known[active] {
			return fmt.Errorf("未找到人设 %s", active)
		}
		return brain.Reconfigure(func() (*vxagent.Agent, error) {
			var engine *ctxengine.Engine
			if mgr := brain.CtxMgr(); mgr != nil {
				engine = mgr.Current()
			}
			newAg, err := Build(next, registry, brain.Pool(), approver, engine)
			if err != nil {
				return nil, err
			}
			if err := config.UpdatePersona(configPath, active, next.Persona.Presets); err != nil {
				return nil, err
			}
			return newAg, nil
		})
	})
}
