package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/web"
)

// runtimeTestServer 装配一个能改运行上限的后台：真实 config.yaml + 真实
// Agent/Brain（重建 Agent 不联网，provider 只在构造时读配置）。
func runtimeTestServer(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()
	s, _, _ := setupTestServer(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "provider:\n  name: openai\n  baseURL: https://api.example.com/v1\n  model: m\n  apiKey: sk-test\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reg := vxagent.NewRegistry()
	brain := agent.NewBrain(nil, agent.NewSessionPool(nil), nil)

	s.cfg = cfg
	s.configPath = path
	s.regFn = func() *vxagent.Registry { return reg }
	s.approver = approval.New(string(approval.ModeConfirm), nil, 0)
	s.chat = web.New(brain, filepath.Join(dir, "uploads"), filepath.Join(dir, "paused.json"),
		filepath.Join(dir, "work-sessions.json"))
	// 路由在 Handler() 构建时按已装配的依赖挂载，故装配完成后才取 handler。
	return s, s.Handler(), testAuthToken(t, s), path
}

// runtimeBody 解出 { body: ... } 信封内容。
func runtimeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
	}
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应 %s: %v", rec.Body, err)
	}
	if envelope.Body == nil {
		t.Fatalf("响应缺少 body: %s", rec.Body)
	}
	return envelope.Body
}

func runtimeRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	return setupTestRequest(handler, method, path, []byte(body),
		"127.0.0.1:1234", "localhost", "", token)
}

// TestRuntimeHandlersGetAndSave 运行上限接口：读当前值与可填区间，保存后热生效
// 并写回 config.yaml。
func TestRuntimeHandlersGetAndSave(t *testing.T) {
	s, handler, token, path := runtimeTestServer(t)

	body := runtimeBody(t, runtimeRequest(handler, "GET", "/api/runtime", "", token))
	if body["maxIterations"] != float64(config.DefaultAgentMaxIterations) {
		t.Fatalf("未配置时应回默认 %d，实际 %v", config.DefaultAgentMaxIterations, body["maxIterations"])
	}
	if body["defaultMaxIterations"] != float64(config.DefaultAgentMaxIterations) ||
		body["minIterations"] != float64(config.MinAgentMaxIterations) ||
		body["maxIterationsLimit"] != float64(config.MaxAgentMaxIterations) {
		t.Fatalf("应带出默认值与可填区间: %v", body)
	}

	saved := runtimeBody(t, runtimeRequest(handler, "POST", "/api/runtime", `{"maxIterations": 60}`, token))
	if saved["maxIterations"] != float64(60) {
		t.Fatalf("保存后应回 60，实际 %v", saved["maxIterations"])
	}
	if got := s.cfg.Snapshot().Agent.MaxIterations; got != 60 {
		t.Fatalf("运行态配置应更新为 60，实际 %d", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "maxIterations: 60") {
		t.Fatalf("应写回 config.yaml:\n%s", data)
	}

	// 填 0 = 恢复默认。
	restored := runtimeBody(t, runtimeRequest(handler, "POST", "/api/runtime", `{"maxIterations": 0}`, token))
	if restored["maxIterations"] != float64(config.DefaultAgentMaxIterations) {
		t.Fatalf("填 0 应恢复默认 %d，实际 %v", config.DefaultAgentMaxIterations, restored["maxIterations"])
	}
}

// TestRuntimeHandlerRejectsBadInput 越界与缺字段都拒绝，且不动运行态。
func TestRuntimeHandlerRejectsBadInput(t *testing.T) {
	s, handler, token, _ := runtimeTestServer(t)
	before := s.cfg.Snapshot().Agent.MaxIterations

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"越界", `{"maxIterations": 999}`, "1-500"},
		{"负数", `{"maxIterations": -3}`, "1-500"},
		{"缺字段", `{}`, "maxIterations"},
	} {
		rec := runtimeRequest(handler, "POST", "/api/runtime", tc.body, token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d %s", tc.name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: 报错应带出 %q，实际 %s", tc.name, tc.want, rec.Body)
		}
	}
	if got := s.cfg.Snapshot().Agent.MaxIterations; got != before {
		t.Fatalf("拒绝后配置不应变化: %d → %d", before, got)
	}
}
