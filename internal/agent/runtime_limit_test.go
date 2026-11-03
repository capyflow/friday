package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
)

// toolLoopServer 起一个假 OpenAI 兼容服务：每次请求都回一个工具调用，
// 模型自己永远不收尾——框架只能转到轮数上限才停。用它数"模型被叫了几次"
// 就是这一轮真实跑了多少工具轮。
func toolLoopServer(t *testing.T, toolName string, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":null,`+
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}]}`, toolName)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// limitBrain 按配置搭一个跑在假服务上的 Brain（真实 provider + 真实框架循环）。
func limitBrain(t *testing.T, cfgBody string) (*Brain, *int32) {
	t.Helper()
	var calls int32
	srv := toolLoopServer(t, "noop", &calls)

	path := filepath.Join(t.TempDir(), "config.yaml")
	// noop 放进直通名单：confirm 模式下未放行的工具会进审批队列等人点，
	// 测试里没人审批，会一直挂着。
	body := fmt.Sprintf("provider:\n  name: openai\n  baseURL: %s/v1\n  model: m\n  apiKey: sk-test\n"+
		"permissions:\n  allow: [\"noop\"]\n%s", srv.URL, cfgBody)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reg := vxagent.NewRegistry()
	if err := reg.Add(&stubTool{name: "noop"}); err != nil {
		t.Fatal(err)
	}
	ag, err := Build(cfg, reg, NewSessionPool(nil),
		approval.New(string(approval.ModeConfirm), nil, 0), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return NewBrain(ag, NewSessionPool(nil), nil), &calls
}

// TestBuildRespectsMaxIterations 配置的轮数上限真的生效：模型被叫满上限次数后报错，
// 报错里带的是配置值而不是框架缺省的 10。
func TestBuildRespectsMaxIterations(t *testing.T) {
	brain, calls := limitBrain(t, "agent:\n  maxIterations: 3\n")
	ctx := logx.WithID(WithSessionKey(context.Background(), SessionWeb), "limit-test")

	_, err := brain.Ask(ctx, "干活", nil, nil)
	if err == nil {
		t.Fatal("模型一直调工具时应撞轮数上限报错")
	}
	if !strings.Contains(err.Error(), "超过 3 轮") {
		t.Fatalf("报错应带上配置的上限 3，实际 %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("应正好跑满 3 轮工具调用，实际 %d", got)
	}
}

// TestBuildDefaultMaxIterations 未配置时默认 100 轮（框架缺省只有 10 轮，
// 多工具活儿半路就断——"工具调用超过 10 轮仍未结束"就是这么来的）。
func TestBuildDefaultMaxIterations(t *testing.T) {
	brain, calls := limitBrain(t, "")
	ctx := logx.WithID(WithSessionKey(context.Background(), SessionWeb), "limit-default")

	_, err := brain.Ask(ctx, "干活", nil, nil)
	if err == nil {
		t.Fatal("模型一直调工具时应撞轮数上限报错")
	}
	if !strings.Contains(err.Error(), "超过 100 轮") {
		t.Fatalf("默认上限应为 100，实际 %v", err)
	}
	if got := atomic.LoadInt32(calls); got != config.DefaultAgentMaxIterations {
		t.Fatalf("应跑满 %d 轮，实际 %d", config.DefaultAgentMaxIterations, got)
	}
}
