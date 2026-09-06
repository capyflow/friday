// Package config 负责加载机器人的 YAML 配置：LLM 供应商、语音链路（ASR/唤醒词/TTS）、
// 显示输出等。核心 agent 只依赖 Provider 与 SystemPrompt 字段，其余为语音/显示层配置。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是机器人整体配置。
type Config struct {
	Provider     ProviderConfig `yaml:"provider"`
	Audio        AudioConfig    `yaml:"audio"`
	ASR          ASRConfig      `yaml:"asr"`
	Wake         WakeConfig     `yaml:"wake"`
	TTS          TTSConfig      `yaml:"tts"`
	VAD          VADConfig      `yaml:"vad"`
	Display      DisplayConfig  `yaml:"display"`
	Admin        AdminConfig    `yaml:"admin"`
	Memory       MemoryConfig   `yaml:"memory"`
	SystemPrompt string         `yaml:"systemPrompt"`
	// TaskFile 定时任务持久化 JSON 文件路径（默认 tasks.json，重启自动恢复）。
	TaskFile string `yaml:"taskFile"`
}

// AdminConfig 描述管理后台（vortexv3 HTTP 服务）。
type AdminConfig struct {
	// Enabled 默认开启；配置里写 enabled: false 显式关闭。
	Enabled *bool `yaml:"enabled"`
	// Port 监听端口（默认 8080）。
	Port int `yaml:"port"`
	// AccountFile 管理员账号文件（默认 admin-account.json）。
	// 内置默认账号 admin/admin，首次登录强制重新设置用户名与密码；
	// 账号以 bcrypt 哈希落盘，重启后以设置的账号为准。
	AccountFile string `yaml:"accountFile"`
}

// IsEnabled 返回后台是否启用（未配置时默认开启）。
func (a AdminConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// MemoryConfig 描述长期记忆。
type MemoryConfig struct {
	// File 记忆持久化 JSON 文件路径（默认 memories.json）。
	File string `yaml:"file"`
}

// ProviderConfig 描述 LLM 供应商。
type ProviderConfig struct {
	// Name 固定为 "openai"（vortexagent 的 OpenAI 兼容层，DeepSeek/Qwen/智谱等通用）。
	Name string `yaml:"name"`
	// BaseURL OpenAI 兼容服务地址，如 https://api.deepseek.com/v1。
	BaseURL string `yaml:"baseURL"`
	// Model 模型名，如 deepseek-chat。
	Model string `yaml:"model"`
	// APIKeyEnv 环境变量名，运行时从该变量读取 API 密钥（不落盘）。
	APIKeyEnv string `yaml:"apiKeyEnv"`
	// Temperature 采样温度（可选，默认 0.7）。
	Temperature float64 `yaml:"temperature"`
	// MaxTokens 单次输出最大 token（可选，默认 1024）。
	MaxTokens int `yaml:"maxTokens"`
}

// AudioConfig 描述麦克风/喇叭设备。
type AudioConfig struct {
	InputDevice  string `yaml:"inputDevice"`  // 留空自动选择
	OutputDevice string `yaml:"outputDevice"` // 留空自动选择
	SampleRate   int    `yaml:"sampleRate"`   // 默认 16000
	Channels     int    `yaml:"channels"`     // 默认 1
}

// ASRConfig 描述语音识别。
type ASRConfig struct {
	// Engine: "none"(文本模式) 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir sherpa-onnx 模型目录（离线识别模型）。
	ModelDir string `yaml:"modelDir"`
}

// WakeConfig 描述唤醒词。
type WakeConfig struct {
	// Engine: "none" 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir 唤醒词模型目录（KWS 模型）。
	ModelDir string `yaml:"modelDir"`
	// Keywords 唤醒词列表，如 ["小助手"]。
	Keywords []string `yaml:"keywords"`
}

// TTSConfig 描述语音合成。
type TTSConfig struct {
	// Engine: "none" 或 "sherpa-onnx"。
	Engine string `yaml:"engine"`
	// ModelDir TTS 模型目录。
	ModelDir string `yaml:"modelDir"`
	// Voice 说话人/音色名（留空用模型默认）。
	Voice string `yaml:"voice"`
	// Speed 语速倍率（1.0 正常，越大越快；留空/0 视为 1.0）。
	Speed float64 `yaml:"speed"`
}

// VADConfig 描述录音断句（silero VAD）：说完自动停止录音，替代固定时长等待。
type VADConfig struct {
	// ModelPath silero_vad.onnx 路径；为空或文件不存在时回退为固定时长录音。
	ModelPath string `yaml:"modelPath"`
	// Threshold 语音概率阈值（0~1，越大越不敏感，默认 0.5）。
	Threshold float32 `yaml:"threshold"`
	// MinSilence 静音持续多少秒判定"说完"（默认 0.6）。
	MinSilence float32 `yaml:"minSilence"`
	// MinSpeech 语音持续多少秒才算"说过话"（默认 0.25，滤除短噪声）。
	MinSpeech float32 `yaml:"minSpeech"`
}

// DisplayConfig 描述屏幕。
type DisplayConfig struct {
	// Driver: "console"(开发) 或后续 "oled"/"st7789" 等。
	Driver string `yaml:"driver"`
}

// Load 从 path 读取 YAML 配置，并对缺失字段填充默认值。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ProviderAPIKey 从配置指定的环境变量读取 API 密钥。
func (c *Config) ProviderAPIKey() string {
	return os.Getenv(c.Provider.APIKeyEnv)
}

// applyDefaults 填充未配置字段的合理默认值。
func (c *Config) applyDefaults() {
	if c.Provider.Name == "" {
		c.Provider.Name = "openai"
	}
	if c.Provider.Temperature == 0 {
		c.Provider.Temperature = 0.7
	}
	if c.Provider.MaxTokens == 0 {
		c.Provider.MaxTokens = 1024
	}
	if c.Audio.SampleRate == 0 {
		c.Audio.SampleRate = 16000
	}
	if c.Audio.Channels == 0 {
		c.Audio.Channels = 1
	}
	if c.ASR.Engine == "" {
		c.ASR.Engine = "none"
	}
	if c.Wake.Engine == "" {
		c.Wake.Engine = "none"
	}
	if c.TTS.Engine == "" {
		c.TTS.Engine = "none"
	}
	if c.VAD.Threshold == 0 {
		c.VAD.Threshold = 0.5
	}
	if c.VAD.MinSilence == 0 {
		c.VAD.MinSilence = 0.6
	}
	if c.VAD.MinSpeech == 0 {
		c.VAD.MinSpeech = 0.25
	}
	if c.Display.Driver == "" {
		c.Display.Driver = "console"
	}
	if c.TaskFile == "" {
		c.TaskFile = "tasks.json"
	}
	if c.Admin.Port == 0 {
		c.Admin.Port = 8080
	}
	if c.Admin.AccountFile == "" {
		c.Admin.AccountFile = "admin-account.json"
	}
	if c.Memory.File == "" {
		c.Memory.File = "memories.json"
	}
}

// Validate 校验必填项。
func (c *Config) Validate() error {
	if c.Provider.APIKeyEnv == "" {
		return fmt.Errorf("config: provider.apiKeyEnv 不能为空（指定存放 API 密钥的环境变量名）")
	}
	if os.Getenv(c.Provider.APIKeyEnv) == "" {
		return fmt.Errorf("config: 环境变量 %s 未设置", c.Provider.APIKeyEnv)
	}
	if c.Provider.BaseURL == "" {
		return fmt.Errorf("config: provider.baseURL 不能为空（如 https://api.deepseek.com/v1）")
	}
	if c.Provider.Model == "" {
		return fmt.Errorf("config: provider.model 不能为空（如 deepseek-chat）")
	}
	if c.Provider.Name != "openai" {
		return fmt.Errorf("config: provider.name 仅支持 openai（OpenAI 兼容层），当前为 %q", c.Provider.Name)
	}
	return nil
}
