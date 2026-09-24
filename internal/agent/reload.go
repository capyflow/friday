package agent

import (
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// ReloadProvider 热更新 LLM 供应商配置（baseURL/model）：
//  1. 构建新 Agent（复用工具注册表与会话池；approver 支撑权限层 Checker），
//     构建失败则原配置不动；
//  2. 原子替换 Brain 内的 Agent（等待所有会话进行中的请求完成后生效）；
//  3. 把新配置写回 config.yaml（yaml.Node 编辑，保留注释），
//     写回失败不影响已生效的运行时配置，返回提示性错误。
func ReloadProvider(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, approver *approval.Queue, baseURL, model string) error {
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	if baseURL == "" || model == "" {
		return fmt.Errorf("baseURL 与 model 不能为空")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return fmt.Errorf("baseURL 必须以 http:// 或 https:// 开头")
	}

	oldURL, oldModel := cfg.Provider.BaseURL, cfg.Provider.Model
	cfg.Provider.BaseURL, cfg.Provider.Model = baseURL, model
	newAg, err := Build(cfg, registry, brain.Pool(), approver)
	if err != nil {
		cfg.Provider.BaseURL, cfg.Provider.Model = oldURL, oldModel
		return err
	}
	brain.SetAgent(newAg)
	if err := config.UpdateProvider(configPath, baseURL, model); err != nil {
		return fmt.Errorf("已热生效，但写回 config.yaml 失败: %w", err)
	}
	return nil
}

// ReloadPersona 切换人设并保存自定义人设预设：
//  1. 校验 active 必须命中（内置预设 + 自定义 presets 合集），防止切到不存在的人设；
//  2. 更新运行时配置并重建 Agent（新系统提示词随 Agent 热生效，会话保持连续）；
//  3. 写回 config.yaml（persona.active + persona.presets，yaml.Node 编辑保留注释），
//     写回失败不影响已生效的运行时配置，返回提示性错误。
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
	// 合并校验：active 必须命中内置 + 自定义合集（自定义正文允许为空——空正文
	// 视为覆盖失败回退内置，见 resolvePersona；这里只挡"目标人设根本不存在"）。
	known := map[string]bool{}
	for _, p := range BuiltinPersonas() {
		known[p.ID] = true
	}
	for _, p := range cfg.Persona.Presets {
		known[p.ID] = true
	}
	if presets != nil {
		for _, p := range presets {
			known[p.ID] = true
		}
	}
	if !known[active] {
		return fmt.Errorf("未找到人设 %s", active)
	}

	oldActive, oldPresets := cfg.Persona.Active, cfg.Persona.Presets
	cfg.Persona.Active = active
	if presets != nil {
		cfg.Persona.Presets = presets
	}
	newAg, err := Build(cfg, registry, brain.Pool(), approver)
	if err != nil {
		cfg.Persona.Active, cfg.Persona.Presets = oldActive, oldPresets
		return err
	}
	brain.SetAgent(newAg)
	if err := config.UpdatePersona(configPath, active, cfg.Persona.Presets); err != nil {
		return fmt.Errorf("已热生效，但写回 config.yaml 失败: %w", err)
	}
	return nil
}
