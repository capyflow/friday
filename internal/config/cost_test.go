package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// validBase 返回一个能通过 Validate 最小要求的配置（只提供 provider 必填项）。
func validBase() *Config {
	return &Config{Provider: ProviderConfig{
		Name: "openai", BaseURL: "https://api.example.com/v1", Model: "test-model",
	}}
}

// TestCostDefaults 成本配置缺省：币种、价格表文件名、预警线、目标串行、软限默认开启。
func TestCostDefaults(t *testing.T) {
	c := validBase()
	c.applyDefaults()
	if c.Cost.Currency != "CNY" {
		t.Errorf("币种缺省 = %q", c.Cost.Currency)
	}
	if c.Cost.PricingFile != "pricing.json" {
		t.Errorf("价格表缺省 = %q", c.Cost.PricingFile)
	}
	if c.Cost.Budget.SoftRatio != 0.8 {
		t.Errorf("预警线缺省 = %v", c.Cost.Budget.SoftRatio)
	}
	if c.Cost.Goals.MaxConcurrent != 1 {
		t.Errorf("目标并发缺省 = %d", c.Cost.Goals.MaxConcurrent)
	}
	if c.UsageFile != "token-usage.json" {
		t.Errorf("用量文件缺省 = %q", c.UsageFile)
	}
	if !c.Cost.PauseProactiveEnabled() {
		t.Error("pauseProactive 未配置应为开启")
	}
	off := false
	c.Cost.PauseProactive = &off
	if c.Cost.PauseProactiveEnabled() {
		t.Error("显式 false 应关闭暂停开关")
	}
}

// TestValidateRejectsBadCostConfig 非法阈值与负额度启动即报错（0 表示不限，合法）。
func TestValidateRejectsBadCostConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		ok     bool
	}{
		{"默认零值即不限", func(c *Config) {}, true},
		{"预警线为 0", func(c *Config) { c.Cost.Budget.SoftRatio = 0 }, false},
		{"预警线超过 1", func(c *Config) { c.Cost.Budget.SoftRatio = 1.5 }, false},
		{"预警线为负", func(c *Config) { c.Cost.Budget.SoftRatio = -0.2 }, false},
		{"日额度为负", func(c *Config) { c.Cost.Budget.DailyTokens = -1 }, false},
		{"日金额为负", func(c *Config) { c.Cost.Budget.DailyCost = -0.5 }, false},
		{"目标 token 预算为负", func(c *Config) { c.Cost.Budget.PerGoalTokens = -1 }, false},
		{"目标步数为负", func(c *Config) { c.Cost.Budget.PerGoalSteps = -1 }, false},
		{"预警线恰好 1", func(c *Config) { c.Cost.Budget.SoftRatio = 1 }, true},
	}
	for _, tc := range cases {
		c := validBase()
		c.applyDefaults()
		tc.mutate(c)
		err := c.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: 应通过校验，得到 %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: 应被拒绝", tc.name)
		} else if !tc.ok && err != nil && !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("%s: 错误信息应带 config: 前缀，实际 %v", tc.name, err)
		}
	}
}

// TestValidateHeartbeatIntervalFloor 心跳间隔下限：每轮都调模型，过短会失控烧钱。
func TestValidateHeartbeatIntervalFloor(t *testing.T) {
	cases := []struct {
		enabled  bool
		schedule string
		interval string
		wantErr  bool
		why      string
	}{
		{true, "", "30s", true, "30 秒心跳应被拒绝"},
		{true, "", "1m", false, "1 分钟是下限"},
		{true, "", "30m", false, "常规间隔"},
		{true, "0 8 * * *", "5s", false, "配了 cron 时不校验 interval"},
		{false, "", "5s", false, "未启用心跳不拦"},
		{true, "", "not-a-duration", true, "无法解析应报错"},
		{true, "", "  10s ", true, "前后空格不应绕过下限校验"},
	}
	for _, tc := range cases {
		c := validBase()
		c.applyDefaults()
		c.Heartbeat.Enabled = tc.enabled
		c.Heartbeat.Schedule = tc.schedule
		c.Heartbeat.Interval = tc.interval
		err := c.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("%s: 应报错（enabled=%v schedule=%q interval=%q）", tc.why, tc.enabled, tc.schedule, tc.interval)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: 不应报错: %v", tc.why, err)
		}
	}
}

// TestCostYAMLRoundTrip 配置键名与 yaml tag 对齐（写错 tag 会让用户配了不生效）。
func TestCostYAMLRoundTrip(t *testing.T) {
	src := `
cost:
  currency: USD
  pricingFile: data/pricing.json
  pauseProactive: false
  budget:
    dailyTokens: 2000000
    monthlyTokens: 30000000
    dailyCost: 12.5
    softRatio: 0.9
    perPlanTokens: 60000
    perGoalTokens: 120000
    perGoalSteps: 12
  goals:
    maxConcurrent: 2
usageFile: my-usage.json
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("解析 cost 段: %v", err)
	}
	if c.Cost.Currency != "USD" || c.Cost.PricingFile != "data/pricing.json" {
		t.Fatalf("顶层键解析不符: %+v", c.Cost)
	}
	if c.Cost.PauseProactive == nil || *c.Cost.PauseProactive {
		t.Fatal("pauseProactive: false 应解析为显式关闭")
	}
	b := c.Cost.Budget
	if b.DailyTokens != 2000000 || b.MonthlyTokens != 30000000 || b.DailyCost != 12.5 ||
		b.SoftRatio != 0.9 || b.PerPlanTokens != 60000 || b.PerGoalTokens != 120000 || b.PerGoalSteps != 12 {
		t.Fatalf("budget 键解析不符: %+v", b)
	}
	if c.Cost.Goals.MaxConcurrent != 2 {
		t.Fatalf("goals.maxConcurrent = %d", c.Cost.Goals.MaxConcurrent)
	}
	if c.UsageFile != "my-usage.json" {
		t.Fatalf("usageFile = %q", c.UsageFile)
	}

	out, err := yaml.Marshal(&c.Cost)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	for _, key := range []string{"currency", "pricingFile", "pauseProactive", "dailyTokens", "monthlyTokens", "dailyCost", "softRatio", "perPlanTokens", "perGoalTokens", "perGoalSteps", "maxConcurrent"} {
		if !strings.Contains(string(out), key) {
			t.Errorf("序列化缺少键 %s: %s", key, out)
		}
	}
}
