package agent

import (
	"fmt"
	"strings"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/config"
)

// ReloadProvider 热更新 LLM 供应商配置（baseURL/model）：
//  1. 构建新 Agent（复用工具注册表与会话池），构建失败则原配置不动；
//  2. 原子替换 Brain 内的 Agent（等待所有会话进行中的请求完成后生效）；
//  3. 把新配置写回 config.yaml（yaml.Node 编辑，保留注释），
//     写回失败不影响已生效的运行时配置，返回提示性错误。
func ReloadProvider(cfg *config.Config, configPath string, registry *vxagent.Registry, brain *Brain, baseURL, model string) error {
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
	newAg, err := Build(cfg, registry, brain.Pool())
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
