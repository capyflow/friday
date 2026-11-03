// tools_search.go 是设置 → 扩展 → 工具 里「联网搜索」的后端：读取/保存
// 供应商与密钥（写 config.yaml 的 tools.search 段，热生效——工具实例经
// SearchFn 每次调用读运行时快照，无需重建 agent），附一个真搜一次的测试端点。
package admin

import (
	"context"
	"strings"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/tools"
)

// validSearchProviders 是可配置的搜索供应商（与 tools.WebSearchTool 的路由一致）。
var validSearchProviders = map[string]bool{"bing": true, "tavily": true, "brave": true, "bocha": true}

// handleToolsSearchGet 返回当前联网搜索配置。
func (s *Server) handleToolsSearchGet(ctx *vhttp.Context) error {
	sc := s.cfg.Snapshot().Tools.Search
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"provider": sc.ProviderOrDefault(), "apiKey": sc.APIKey,
	})
}

// handleToolsSearchSet 保存联网搜索配置（provider + apiKey），原子写回
// config.yaml 并热生效。provider 必须在支持清单内；apiKey 允许为空（bing 恒
// 免密钥，付费供应商空 key 由工具侧回落 bing 保底）。
func (s *Server) handleToolsSearchSet(ctx *vhttp.Context) error {
	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"apiKey"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	provider := strings.TrimSpace(strings.ToLower(req.Provider))
	if !validSearchProviders[provider] {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": "不支持的供应商 " + provider + "（可选 bing/tavily/brave/bocha）"})
	}
	if s.cfg == nil || s.configPath == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	next := config.SearchConfig{Provider: provider, APIKey: strings.TrimSpace(req.APIKey)}
	if err := s.cfg.Apply(func(n *config.Config) error {
		n.Tools.Search = next
		return config.UpdateToolsSearch(s.configPath, next)
	}); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "保存失败: " + err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "联网搜索配置已保存并生效"})
}

// handleToolsSearchTest 用当前配置真搜一次（设置页「测试」按钮）：返回工具的
// 原始输出（截断展示），配置错 key 错都会在这里现形。
func (s *Server) handleToolsSearchTest(ctx *vhttp.Context) error {
	var req struct {
		Query string `json:"query"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = "今日新闻"
	}
	if s.cfg == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "配置不可用"})
	}
	tr := &tools.WebSearchTool{SearchFn: func() (string, string) {
		sc := s.cfg.Snapshot().Tools.Search
		return sc.ProviderOrDefault(), sc.APIKey
	}}
	tctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := tr.Call(tctx, map[string]any{"query": query, "count": float64(3)})
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": out})
}
