package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/skill"
)

const testSkill = `---
name: 晨会准备
description: 汇总今日待办与天气
auto: true
triggers: ["晨会"]
allowed-tools: [list_todos]
---
先报日期，再报天气，最后汇报待办。
`

func skillsTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, _, token, _, _ := manageTestServer(t)
	s.skills = skill.NewStore(filepath.Join(t.TempDir(), "skills"), "")
	return s, s.Handler(), token
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// skillBody 发一次请求并解出 { body: ... } 信封 + HTTP 状态码。
func skillBody(t *testing.T, handler http.Handler, token string, method, path string, body []byte) map[string]any {
	t.Helper()
	rec := setupTestRequest(handler, method, path, body, "127.0.0.1:1234", "localhost", "", token)
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 %s %s 响应 %s: %v", method, path, rec.Body, err)
	}
	if envelope.Body == nil {
		envelope.Body = map[string]any{}
	}
	envelope.Body["__status"] = rec.Code
	return envelope.Body
}

func skillID(t *testing.T, summary map[string]any) string {
	t.Helper()
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("响应缺少技能 id: %+v", summary)
	}
	return id
}

// TestSkillsHandlerRoundTrip 新建 → 列表 → 读原文 → 编辑 → 停用 → 删除，全程无需重启。
func TestSkillsHandlerRoundTrip(t *testing.T) {
	s, handler, token := skillsTestServer(t)

	created := skillBody(t, handler, token, "POST", "/api/skills", mustJSON(t, map[string]any{"raw": testSkill}))
	if created["__status"] != http.StatusOK {
		t.Fatalf("新建失败: %+v", created)
	}
	summary, _ := created["skill"].(map[string]any)
	if summary["name"] != "晨会准备" {
		t.Fatalf("新建响应不符: %+v", created)
	}
	id := skillID(t, summary)
	if len(s.skills.List()) != 1 {
		t.Fatalf("保存即应生效（无需重启），实际 %d 条", len(s.skills.List()))
	}

	list := skillBody(t, handler, token, "GET", "/api/skills", nil)
	if items, _ := list["skills"].([]any); len(items) != 1 {
		t.Fatalf("列表应有 1 条: %+v", list)
	}
	if list["dir"] == "" || list["dir"] == nil {
		t.Error("列表应带上技能目录，方便用户手改文件")
	}

	got := skillBody(t, handler, token, "GET", "/api/skills/"+id, nil)
	rawText, _ := got["raw"].(string)
	if got["__status"] != http.StatusOK || !strings.Contains(rawText, "晨会准备") {
		t.Fatalf("读原文失败: %+v", got)
	}

	edited := strings.Replace(testSkill, "name: 晨会准备", "name: 晚间复盘", 1)
	updated := skillBody(t, handler, token, "PUT", "/api/skills/"+id, mustJSON(t, map[string]any{"raw": edited}))
	if updated["__status"] != http.StatusOK {
		t.Fatalf("编辑失败: %+v", updated)
	}
	if _, ok := s.skills.FindName("晚间复盘"); !ok {
		t.Error("编辑后仓库应按新名字命中")
	}

	off := skillBody(t, handler, token, "POST", "/api/skills/"+id+"/enabled", mustJSON(t, map[string]any{"enabled": false}))
	if off["__status"] != http.StatusOK {
		t.Fatalf("停用失败: %+v", off)
	}
	if item, ok := s.skills.Get(id); !ok || item.Enabled {
		t.Errorf("停用后状态不符: %+v", item)
	}

	deleted := skillBody(t, handler, token, "DELETE", "/api/skills/"+id, nil)
	if deleted["__status"] != http.StatusOK {
		t.Fatalf("删除失败: %+v", deleted)
	}
	if len(s.skills.List()) != 0 {
		t.Errorf("删除后仓库应为空: %+v", s.skills.List())
	}
}

// TestSkillsHandlerRejectsInvalidFile 坏文件给出原因且不落盘；重名指出冲突对象。
func TestSkillsHandlerRejectsInvalidFile(t *testing.T) {
	s, handler, token := skillsTestServer(t)

	res := skillBody(t, handler, token, "POST", "/api/skills", mustJSON(t, map[string]any{"raw": "没有 frontmatter\n"}))
	if res["__status"] == http.StatusOK {
		t.Fatalf("无 frontmatter 的技能应被拒绝: %+v", res)
	}
	if errText, _ := res["error"].(string); !strings.Contains(errText, "frontmatter") {
		t.Errorf("错误应说明原因，实际 %q", errText)
	}
	if len(s.skills.List()) != 0 {
		t.Errorf("被拒绝的写入不该落盘: %+v", s.skills.List())
	}

	first := skillBody(t, handler, token, "POST", "/api/skills", mustJSON(t, map[string]any{"raw": testSkill}))
	if first["__status"] != http.StatusOK {
		t.Fatalf("首条应能建: %+v", first)
	}
	second := skillBody(t, handler, token, "POST", "/api/skills", mustJSON(t, map[string]any{"raw": testSkill}))
	if second["__status"] == http.StatusOK {
		t.Fatalf("重名应被拒绝: %+v", second)
	}
	if errText, _ := second["error"].(string); !strings.Contains(errText, "已被") {
		t.Errorf("重名错误应指出冲突对象，实际 %q", errText)
	}
}

// TestSkillsHandlerMissingIDAndBadBody 不存在的 id 与畸形请求体都得可读报错。
func TestSkillsHandlerMissingIDAndBadBody(t *testing.T) {
	_, handler, token := skillsTestServer(t)

	if res := skillBody(t, handler, token, "GET", "/api/skills/ghost", nil); res["__status"] == http.StatusOK {
		t.Errorf("不存在的技能应报错: %+v", res)
	}
	if res := skillBody(t, handler, token, "POST", "/api/skills", []byte("not json")); res["__status"] == http.StatusOK {
		t.Errorf("畸形请求体应报错: %+v", res)
	}
	if res := skillBody(t, handler, token, "POST", "/api/skills/ghost/enabled", mustJSON(t, map[string]any{"enabled": true})); res["__status"] == http.StatusOK {
		t.Errorf("给不存在的技能改开关应报错: %+v", res)
	}
}

// TestSkillsReloadHandlerPicksUpManualEdit 手改磁盘文件后 reload 接口生效。
func TestSkillsReloadHandlerPicksUpManualEdit(t *testing.T) {
	s, handler, token := skillsTestServer(t)
	created := skillBody(t, handler, token, "POST", "/api/skills", mustJSON(t, map[string]any{"raw": testSkill}))
	id := skillID(t, created["skill"].(map[string]any))

	edited := strings.Replace(testSkill, "name: 晨会准备", "name: 手改的名字", 1)
	if err := os.WriteFile(filepath.Join(s.skills.Dir(), id, "SKILL.md"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	res := skillBody(t, handler, token, "POST", "/api/skills/reload", mustJSON(t, map[string]any{}))
	if res["__status"] != http.StatusOK {
		t.Fatalf("reload 失败: %+v", res)
	}
	if _, ok := s.skills.FindName("手改的名字"); !ok {
		t.Error("reload 后应看到手改的新名字")
	}
}

// TestSkillsRoutesAbsentWhenDisabled 技能层关闭（store 为 nil）时不挂载接口。
func TestSkillsRoutesAbsentWhenDisabled(t *testing.T) {
	s, _, token, _, _ := manageTestServer(t)
	s.skills = nil
	handler := s.Handler()

	rec := setupTestRequest(handler, "GET", "/api/skills", nil, "127.0.0.1:1234", "localhost", "", token)
	if rec.Code == http.StatusOK {
		t.Error("技能层关闭时 /api/skills 不应可用")
	}
}
