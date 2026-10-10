package agent

import (
	"fmt"
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
