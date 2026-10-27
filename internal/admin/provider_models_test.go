package admin

import (
	"testing"

	"github.com/capyflow/friday/internal/config"
)

// TestEffectiveProviderModelsHidesBootstrapPlaceholder 首次启动的示例占位不能
// 作为模型条目下发给前端；真实扁平配置与模型库仍按原逻辑展示。
func TestEffectiveProviderModelsHidesBootstrapPlaceholder(t *testing.T) {
	cases := []struct {
		name       string
		in         config.ProviderConfig
		wantActive string
		wantModels int
	}{
		{
			name: "扁平占位",
			in: config.ProviderConfig{
				BaseURL: config.PlaceholderBaseURL, Model: config.PlaceholderModel,
			},
			wantActive: "", wantModels: 0,
		},
		{
			name: "单条占位库",
			in: config.ProviderConfig{
				Active: config.PlaceholderModel,
				Models: []config.ModelProfile{{
					Name: config.PlaceholderModel, BaseURL: config.PlaceholderBaseURL, Model: config.PlaceholderModel,
				}},
			},
			wantActive: "", wantModels: 0,
		},
		{
			name:       "扁平真实配置",
			in:         config.ProviderConfig{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
			wantActive: "deepseek-chat", wantModels: 1,
		},
		{
			name: "模型库",
			in: config.ProviderConfig{
				Active: "a",
				Models: []config.ModelProfile{{Name: "a", BaseURL: "https://api.a.example/v1", Model: "m-a"}},
			},
			wantActive: "a", wantModels: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active, models := effectiveProviderModels(tc.in)
			if active != tc.wantActive || len(models) != tc.wantModels {
				t.Fatalf("effectiveProviderModels = %q / %d 条，期望 %q / %d 条",
					active, len(models), tc.wantActive, tc.wantModels)
			}
			if tc.wantModels == 1 && len(models) == 1 && models[0].Model == "" {
				t.Fatalf("合成条目缺少模型名: %+v", models[0])
			}
		})
	}
}
