package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/acp"
	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/memory"
)

// dirsBody 发一次 GET /api/acp/dirs 并解出 { body: ... } 信封。
func dirsBody(t *testing.T, handler http.Handler, token string, rawQuery string) (int, map[string]any) {
	t.Helper()
	rec := setupTestRequest(handler, http.MethodGet, "/api/acp/dirs?"+rawQuery, nil, "127.0.0.1:1234", "localhost", "", token)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 dirs 响应 %s: %v", rec.Body.String(), err)
	}
	return rec.Code, envelope.Body
}

func TestAcpUpgradeUpdatesLaunchConfiguration(t *testing.T) {
	// 默认 acp 基目录是相对路径 <数据目录>/acp：chdir 到临时目录隔离真实数据目录。
	tmp := t.TempDir()
	t.Chdir(tmp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\necho upgraded\n"))
	}))
	defer srv.Close()
	base := filepath.Join(tmp, "acp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := map[string]any{
		"fetchedAt": time.Now(),
		"index": map[string]any{"version": "1", "agents": []any{map[string]any{
			"id": "upgrade-test", "name": "Upgrade Test", "version": "2.0.0",
			"distribution": map[string]any{"binary": map[string]any{
				"darwin-aarch64": map[string]any{"archive": srv.URL + "/bin", "cmd": "agent", "args": []string{"--new"}},
				"linux-x86_64":   map[string]any{"archive": srv.URL + "/bin", "cmd": "agent", "args": []string{"--new"}},
			}},
		}}},
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "registry-cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, token, _, _ := manageTestServer(t)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(s.configPath, []byte("accounts: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Acp = config.AcpConfig{Enabled: true, Agents: []config.AcpAgentConfig{{
		Name: "upgrade-test", Command: "/old/agent", Args: []string{"--old"}, Version: "1.0.0",
		Env: map[string]string{"CUSTOM": "kept"}, Workspace: "/custom/workspace",
	}}}
	m := acp.NewManager(func() config.AcpConfig { return s.cfg.Snapshot().Acp }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	rec := setupTestRequest(s.Handler(), http.MethodPost, "/api/acp/install", []byte(`{"id":"upgrade-test"}`), "127.0.0.1:1234", "localhost", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("升级失败: %d %s", rec.Code, rec.Body.String())
	}
	got, ok := s.cfg.Snapshot().Acp.Agent("upgrade-test")
	if !ok {
		t.Fatalf("升级后清单里找不到 upgrade-test: %+v", s.cfg.Snapshot().Acp.Agents)
	}
	want := filepath.Join("acp", "agents", "upgrade-test", "2.0.0", "agent")
	if got.Command != want || len(got.Args) != 1 || got.Args[0] != "--new" || got.Version != "2.0.0" {
		t.Fatalf("升级后启动配置未更新: %+v", got)
	}
	if got.Env["CUSTOM"] != "kept" || got.Workspace != "/custom/workspace" {
		t.Fatalf("升级覆盖了用户配置: %+v", got)
	}
	// 写回的 config.yaml 里不该出现内置条目（friday 是代码注入的，用户没配过它）
	data, err = os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "builtin") || strings.Contains(string(data), "friday") {
		t.Fatalf("内置条目被写进 config.yaml:\n%s", data)
	}
}

func TestAcpDirsListsSubdirectoriesOnly(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	m := acp.NewManager(func() config.AcpConfig { return config.AcpConfig{} }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	handler := s.Handler()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b-dir", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := dirsBody(t, handler, token, "path="+url.QueryEscape(root))
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %v", code, body)
	}
	if body["path"] != root {
		t.Fatalf("应回显绝对路径 %s，实际 %v", root, body["path"])
	}
	if parent := body["parent"]; parent != filepath.Dir(root) {
		t.Fatalf("parent 应为 %s，实际 %v", filepath.Dir(root), parent)
	}
	dirs, ok := body["dirs"].([]any)
	if !ok || len(dirs) != 2 {
		t.Fatalf("应只列 2 个子目录（不含文件、不递归），实际 %v", body["dirs"])
	}
	first := dirs[0].(map[string]any)
	second := dirs[1].(map[string]any)
	if first["name"] != "a-dir" || second["name"] != "b-dir" {
		t.Fatalf("应按名称排序：a-dir, b-dir；实际 %v, %v", first, second)
	}
	if first["path"] != filepath.Join(root, "a-dir") {
		t.Fatalf("子目录 path 应为绝对路径，实际 %v", first["path"])
	}

	// 缺省 path = HOME（只要求可用，不假设内容）。
	code, body = dirsBody(t, handler, token, "")
	home, _ := os.UserHomeDir()
	if code != http.StatusOK || body["path"] != home {
		t.Fatalf("缺省 path 应列 HOME %s，状态 %d，实际 %v", home, code, body["path"])
	}

	// 不存在的目录 / 文件路径报 400。
	code, _ = dirsBody(t, handler, token, "path="+url.QueryEscape(filepath.Join(root, "nope")))
	if code != http.StatusBadRequest {
		t.Fatalf("不存在的目录应报 400，实际 %d", code)
	}
	code, _ = dirsBody(t, handler, token, "path="+url.QueryEscape(filepath.Join(root, "file.txt")))
	if code != http.StatusBadRequest {
		t.Fatalf("文件路径应报 400，实际 %d", code)
	}
}

// acpCall 发一次带 JSON 体的 ACP 请求，回状态码与 { body: ... } 信封。
func acpCall(t *testing.T, handler http.Handler, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	rec := setupTestRequest(handler, method, path, raw, "127.0.0.1:1234", "localhost", "", token)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if len(rec.Body.Bytes()) > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	}
	return rec.Code, envelope.Body
}

func firstString(v any) string {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	s, _ := list[0].(string)
	return s
}

// TestAcpWorkspaceAndSessionEndpoints 覆盖「工作区 → 会话」两层的 REST 面：
// 建工作区、加/移目录、改名、在工作区里建会话、列表与详情回带目录视图、
// 删工作区连带会话。
func TestAcpWorkspaceAndSessionEndpoints(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	cfg := config.AcpConfig{Enabled: true,
		Agents: []config.AcpAgentConfig{{Name: "fake", Command: "fake-bin"}}}
	m := acp.NewManager(func() config.AcpConfig { return cfg }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	handler := s.Handler()

	dirA, dirB := t.TempDir(), t.TempDir()
	code, body := acpCall(t, handler, token, "POST", "/api/acp/workspaces",
		map[string]any{"name": "proj-a", "dirs": []string{dirA}})
	if code != http.StatusOK {
		t.Fatalf("建工作区应 200，实际 %d: %v", code, body)
	}
	ws, _ := body["workspace"].(map[string]any)
	wsID, _ := ws["id"].(string)
	if wsID == "" || ws["name"] != "proj-a" || firstString(ws["dirs"]) != dirA {
		t.Fatalf("工作区回包不符: %v", body)
	}

	// 加目录
	code, body = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID+"/dirs",
		map[string]any{"path": dirB})
	ws, _ = body["workspace"].(map[string]any)
	dirs, _ := ws["dirs"].([]any)
	if code != http.StatusOK || len(dirs) != 2 {
		t.Fatalf("加目录后应 2 个目录，实际 %d / %v", code, ws)
	}

	// 在工作区里建会话：不给 cwd 就用主目录
	code, body = acpCall(t, handler, token, "POST", "/api/acp/threads",
		map[string]any{"workspaceId": wsID, "agent": "fake"})
	th, _ := body["thread"].(map[string]any)
	thID, _ := th["id"].(string)
	if code != http.StatusOK || thID == "" {
		t.Fatalf("建会话应 200，实际 %d: %v", code, body)
	}
	if th["workspaceId"] != wsID || th["cwd"] != dirA {
		t.Fatalf("会话应锚定工作区与主目录: %v", th)
	}

	// 缺 workspaceId → 400 并给下一步
	code, body = acpCall(t, handler, token, "POST", "/api/acp/threads",
		map[string]any{"agent": "fake"})
	errText, _ := body["error"].(string)
	if code != http.StatusBadRequest || errText == "" {
		t.Fatalf("缺 workspaceId 应 400 并说明下一步，实际 %d: %v", code, body)
	}

	// 列表一次拉齐骨架
	code, body = acpCall(t, handler, token, "GET", "/api/acp/threads", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", code)
	}
	if list, _ := body["workspaces"].([]any); len(list) != 1 {
		t.Fatalf("应回带 1 个工作区，实际 %v", body["workspaces"])
	}
	if list, _ := body["threads"].([]any); len(list) != 1 {
		t.Fatalf("应回带 1 个会话，实际 %v", body["threads"])
	}

	// 详情带目录视图（进程不在场 → known=false，不宣称扩展根生效）
	code, body = acpCall(t, handler, token, "GET", "/api/acp/threads/"+thID, nil)
	sess, _ := body["session"].(map[string]any)
	if code != http.StatusOK || sess == nil {
		t.Fatalf("详情应 200 且带 session，实际 %d: %v", code, body)
	}
	if sess["cwd"] != dirA || sess["known"] != false {
		t.Fatalf("session 视图不符: %v", sess)
	}
	if got, _ := sess["dirs"].([]any); len(got) != 2 {
		t.Fatalf("session.dirs 应是工作区全部目录，实际 %v", sess["dirs"])
	}

	// 改名
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID,
		map[string]any{"name": "改名后的项目"}); code != http.StatusOK {
		t.Fatalf("改名应 200，实际 %d", code)
	}

	// 默认审批档位：新建就是 ask，可改成 readonly，非法值 400
	if ws["policy"] != "ask" {
		t.Fatalf("新工作区默认档位应为 ask: %v", ws)
	}
	code, body = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID,
		map[string]any{"policy": "readonly"})
	ws, _ = body["workspace"].(map[string]any)
	if code != http.StatusOK || ws["policy"] != "readonly" {
		t.Fatalf("改档位应 200 且回 readonly，实际 %d: %v", code, body)
	}
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID,
		map[string]any{"policy": "yolo"}); code != http.StatusBadRequest {
		t.Fatalf("非法档位应 400，实际 %d", code)
	}

	// 工作区文件树：只列直接子项，路径越界 400
	if err := os.WriteFile(filepath.Join(dirA, "notes.md"), []byte("# hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirA, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, body = acpCall(t, handler, token, "GET", "/api/acp/threads/"+thID+"/files", nil)
	files, _ := body["entries"].([]any)
	if code != http.StatusOK || len(files) != 2 {
		t.Fatalf("文件树应列 2 项，实际 %d: %v", code, body)
	}
	if files[0].(map[string]any)["name"] != "src" {
		t.Fatalf("目录应排在文件前: %v", files)
	}
	if code, _ = acpCall(t, handler, token, "GET", "/api/acp/threads/"+thID+"/files?path=../", nil); code != http.StatusBadRequest {
		t.Fatalf("越界路径应 400，实际 %d", code)
	}
	if code, _ = acpCall(t, handler, token, "GET", "/api/acp/threads/"+thID+"/changes?turn=../x", nil); code != http.StatusBadRequest {
		t.Fatalf("非法 turn 应 400，实际 %d", code)
	}
	if ws, ok := m.GetWorkspace(wsID); !ok || ws.Name != "改名后的项目" {
		t.Fatalf("改名未生效: %+v", ws)
	}

	// 三个字段都可选、给哪个改哪个：空对象应报错而不是"全清空"
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("空请求体应 400，实际 %d", code)
	}
	// 置顶不该顺手改名字
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID, map[string]any{"pinned": true}); code != http.StatusOK {
		t.Fatalf("置顶应 200，实际 %d", code)
	}
	if ws, ok := m.GetWorkspace(wsID); !ok || !ws.Pinned || ws.Name != "改名后的项目" {
		t.Fatalf("置顶后名字该保住: %+v", ws)
	}
	// 换主目录：目录挪到首位，会话仍锚在自己的 cwd
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID,
		map[string]any{"primaryPath": dirB}); code != http.StatusOK {
		t.Fatalf("换主目录应 200，实际 %d", code)
	}
	wsNow, _ := m.GetWorkspace(wsID)
	if wsNow.Dirs[0] != dirB {
		t.Fatalf("主目录应变成 %s，实际 %v", dirB, wsNow.Dirs)
	}
	if th, ok := m.Get(thID); !ok || th.Cwd != dirA {
		t.Fatalf("换主目录不该动既有会话的 cwd: %+v", th)
	}
	// 不存在的工作区 → 400（不是静默成功）
	if code, _ = acpCall(t, handler, token, "POST", "/api/acp/workspaces/nope", map[string]any{"pinned": true}); code != http.StatusBadRequest {
		t.Fatalf("不存在的工作区应 400，实际 %d", code)
	}
	// 列表回包带 missingDirs（用户在别处删了目录时 UI 能明说）
	if _, err := os.Stat(dirA); err == nil {
		code, body = acpCall(t, handler, token, "GET", "/api/acp/threads", nil)
		list, _ := body["workspaces"].([]any)
		if code != http.StatusOK || len(list) != 1 {
			t.Fatalf("列表应回带 1 个工作区: %d %v", code, body["workspaces"])
		}
		item, _ := list[0].(map[string]any)
		if item["pinned"] != true {
			t.Fatalf("列表应带上置顶状态: %v", item)
		}
		if _, ok := item["missingDirs"]; ok {
			t.Fatalf("目录都在时不该有 missingDirs: %v", item)
		}
	}

	// 移目录：移掉扩展目录可以，移掉最后一个目录要报错
	if code, body = acpCall(t, handler, token, "DELETE",
		"/api/acp/workspaces/"+wsID+"/dirs?path="+url.QueryEscape(dirB), nil); code != http.StatusOK {
		t.Fatalf("移目录应 200，实际 %d: %v", code, body)
	}
	if code, _ = acpCall(t, handler, token, "DELETE",
		"/api/acp/workspaces/"+wsID+"/dirs?path="+url.QueryEscape(dirA), nil); code == http.StatusOK {
		t.Fatal("移掉最后一个目录应报错")
	}

	// 删工作区：连带会话，并回带会话 id 让前端收起缓冲
	code, body = acpCall(t, handler, token, "DELETE", "/api/acp/workspaces/"+wsID, nil)
	ids, _ := body["threadIds"].([]any)
	if code != http.StatusOK || len(ids) != 1 || ids[0] != thID {
		t.Fatalf("删工作区应回带被删会话，实际 %d: %v", code, body)
	}
	if _, ok := m.Get(thID); ok {
		t.Fatal("工作区删除后会话应已移除")
	}
	if _, ok := m.GetWorkspace(wsID); ok {
		t.Fatal("工作区应已移除")
	}
}

func TestAcpWorkspaceMemoryEndpoints(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	cfg := config.AcpConfig{Enabled: true,
		Agents: []config.AcpAgentConfig{{Name: "fake", Command: "fake-bin"}}}
	m := acp.NewManager(func() config.AcpConfig { return cfg }, nil, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	s.memory = memory.NewWikiStore("", "")
	t.Cleanup(m.Close)
	handler := s.Handler()

	code, body := acpCall(t, handler, token, "POST", "/api/acp/workspaces",
		map[string]any{"name": "proj-a", "dirs": []string{t.TempDir()}})
	if code != http.StatusOK {
		t.Fatalf("建工作区失败: %d %v", code, body)
	}
	ws := body["workspace"].(map[string]any)
	wsID := ws["id"].(string)

	code, body = acpCall(t, handler, token, "POST", "/api/acp/workspaces/"+wsID+"/memory",
		map[string]any{"title": "项目约定", "body": "这个仓库用 pnpm，不要用 npm"})
	if code != http.StatusOK {
		t.Fatalf("保存项目记忆失败: %d %v", code, body)
	}
	page := body["page"].(map[string]any)
	pageID := page["id"].(string)
	if page["scope"] != wsID {
		t.Fatalf("页面 scope 应为工作区 id: %v", page)
	}
	if global := s.memory.Pages(); len(global) != 0 {
		t.Fatalf("项目记忆不应混进全局记忆列表: %+v", global)
	}

	code, body = acpCall(t, handler, token, "GET", "/api/acp/workspaces/"+wsID+"/memory", nil)
	pages := body["pages"].([]any)
	if code != http.StatusOK || len(pages) != 1 {
		t.Fatalf("项目记忆列表不符: %d %v", code, body)
	}
	code, body = acpCall(t, handler, token, "GET", "/api/acp/workspaces/"+wsID+"/memory/"+pageID, nil)
	page = body["page"].(map[string]any)
	if code != http.StatusOK || !strings.Contains(page["body"].(string), "pnpm") {
		t.Fatalf("读取项目记忆失败: %d %v", code, body)
	}
	code, _ = acpCall(t, handler, token, "DELETE", "/api/acp/workspaces/"+wsID+"/memory/"+pageID, nil)
	if code != http.StatusOK {
		t.Fatalf("删除项目记忆失败: %d", code)
	}
	code, body = acpCall(t, handler, token, "GET", "/api/acp/workspaces/"+wsID+"/memory", nil)
	if code != http.StatusOK || len(body["pages"].([]any)) != 0 {
		t.Fatalf("删除后列表应为空: %d %v", code, body)
	}
}

// TestAcpCheckEndpoints 覆盖验证面板的 REST 面：建议命令、提交后进审批队列、
// 批准后跑到结束、停止进行中的运行、非法命令与空命令的报错。
func TestAcpCheckEndpoints(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	cfg := config.AcpConfig{Enabled: true,
		Agents: []config.AcpAgentConfig{{Name: "fake", Command: "fake-bin"}}}
	q := approval.New("confirm", nil, 0)
	m := acp.NewManager(func() config.AcpConfig { return cfg }, q, filepath.Join(t.TempDir(), "threads.json"))
	s.acp = m
	t.Cleanup(m.Close)
	handler := s.Handler()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := acpCall(t, handler, token, "POST", "/api/acp/workspaces",
		map[string]any{"name": "proj-a", "dirs": []string{dir}})
	ws, _ := body["workspace"].(map[string]any)
	wsID, _ := ws["id"].(string)
	if code != http.StatusOK || wsID == "" {
		t.Fatalf("建工作区失败: %d %v", code, body)
	}
	code, body = acpCall(t, handler, token, "POST", "/api/acp/threads",
		map[string]any{"workspaceId": wsID, "agent": "fake"})
	th, _ := body["thread"].(map[string]any)
	thID, _ := th["id"].(string)
	if code != http.StatusOK || thID == "" {
		t.Fatalf("建会话失败: %d %v", code, body)
	}
	base := "/api/acp/threads/" + thID + "/checks"

	// 空记录 + 按标志文件推断的建议命令
	code, body = acpCall(t, handler, token, "GET", base, nil)
	if code != http.StatusOK || len(body["runs"].([]any)) != 0 {
		t.Fatalf("初始记录应为空: %d %v", code, body)
	}
	if !strings.Contains(fmt.Sprint(body["suggestions"]), "go test ./...") {
		t.Fatalf("go.mod 应推断出 go test: %v", body["suggestions"])
	}

	// 空命令与毁灭性命令：400 且不入队
	code, body = acpCall(t, handler, token, "POST", base, map[string]any{"command": "  "})
	if code != http.StatusBadRequest {
		t.Fatalf("空命令应 400: %d %v", code, body)
	}
	code, body = acpCall(t, handler, token, "POST", base, map[string]any{"command": "rm -rf /tmp/whatever"})
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body["error"]), "毁灭性") {
		t.Fatalf("毁灭性命令应 400 并说明原因: %d %v", code, body)
	}
	if len(q.Pending()) != 0 {
		t.Fatalf("被拒的命令不该挂审批卡: %v", q.Pending())
	}

	// 提交 → 待批 → 批准 → 通过
	code, body = acpCall(t, handler, token, "POST", base,
		map[string]any{"command": "echo 面板可用", "timeoutSec": 30})
	run, _ := body["run"].(map[string]any)
	runID, _ := run["id"].(string)
	if code != http.StatusOK || runID == "" || run["state"] != "awaiting" {
		t.Fatalf("提交后应为待批: %d %v", code, body)
	}
	if len(q.Pending()) != 1 || q.Pending()[0].Tool != "验证命令" {
		t.Fatalf("应挂一张验证命令审批卡: %v", q.Pending())
	}
	q.Decide(q.Pending()[0].ID, true)
	got := waitCheckRun(t, handler, token, base+"/"+runID, "passed")
	if !strings.Contains(fmt.Sprint(got["output"]), "面板可用") || got["exitCode"] != float64(0) {
		t.Fatalf("通过记录不符: %v", got)
	}
	if got["timeoutSec"] != float64(30) {
		t.Fatalf("超时应回显: %v", got)
	}

	// 进行中的运行可以停
	code, body = acpCall(t, handler, token, "POST", base,
		map[string]any{"command": "sleep 20", "timeoutSec": 60})
	run, _ = body["run"].(map[string]any)
	longID, _ := run["id"].(string)
	if code != http.StatusOK || longID == "" {
		t.Fatalf("提交长命令失败: %d %v", code, body)
	}
	q.Decide(q.Pending()[0].ID, true)
	waitCheckRun(t, handler, token, base+"/"+longID, "running")
	code, body = acpCall(t, handler, token, "POST", base+"/"+longID+"/stop", nil)
	if code != http.StatusOK {
		t.Fatalf("停止失败: %d %v", code, body)
	}
	got = waitCheckRun(t, handler, token, base+"/"+longID, "killed")
	if !strings.Contains(fmt.Sprint(got["note"]), "已手动停止") {
		t.Fatalf("停止说明不符: %v", got)
	}

	// 单飞：上一条已结束，新的能提交；重复提交进行中的人会看到明确错误
	code, body = acpCall(t, handler, token, "POST", base, map[string]any{"command": "exit 7"})
	run, _ = body["run"].(map[string]any)
	failID, _ := run["id"].(string)
	if code != http.StatusOK || failID == "" {
		t.Fatalf("再提交一条应成功: %d %v", code, body)
	}
	if code, body = acpCall(t, handler, token, "POST", base, map[string]any{"command": "echo 排队"}); code != http.StatusBadRequest {
		t.Fatalf("进行中重复提交应 400: %d %v", code, body)
	}
	q.Decide(q.Pending()[0].ID, true)
	got = waitCheckRun(t, handler, token, base+"/"+failID, "failed")
	if got["exitCode"] != float64(7) || !strings.Contains(fmt.Sprint(got["note"]), "退出码 7") {
		t.Fatalf("失败记录应带退出码: %v", got)
	}

	// 未知运行 → 400
	if code, _ = acpCall(t, handler, token, "GET", base+"/不存在", nil); code != http.StatusBadRequest {
		t.Fatalf("未知运行应 400，实际 %d", code)
	}
	if code, _ = acpCall(t, handler, token, "POST", base+"/不存在/stop", nil); code != http.StatusBadRequest {
		t.Fatalf("停止未知运行应 400，实际 %d", code)
	}
}

// waitCheckRun 轮询单次验证运行直到目标状态（10 秒上限）。
func waitCheckRun(t *testing.T, handler http.Handler, token, path, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, body := acpCall(t, handler, token, "GET", path, nil)
		run, _ := body["run"].(map[string]any)
		if code == http.StatusOK && run != nil {
			last = run
			if run["state"] == want {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待验证状态 %s 超时，最后记录 %+v", want, last)
	return nil
}
