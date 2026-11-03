package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capyflow/friday/internal/skill"
)

// newSkillStore 造一个只含一个可写根的仓库，dev 技能的正文里带可核对的标记。
func newSkillStore(t *testing.T, body string) *skill.Store {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("dev/SKILL.md", "---\nname: 开发技能\ndescription: 写代码时用\nauto: true\n---\n"+body)
	write("dev/references/api.md", "接口说明正文")
	write("other/SKILL.md", "---\nname: 另一个技能\ndescription: 别的事\n---\n正文\n")
	return skill.NewStore(dir, "")
}

func runSkill(t *testing.T, store *skill.Store, args map[string]any) (string, error) {
	t.Helper()
	return (&ReadSkillTool{Store: store}).Call(context.Background(), args)
}

// TestReadSkillIndex 不给名字就列索引：模型不知道技能叫什么时先看这个。
func TestReadSkillIndex(t *testing.T) {
	store := newSkillStore(t, "正文")
	out, err := runSkill(t, store, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"开发技能", "另一个技能", "写代码时用", "id: dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("索引应包含 %q:\n%s", want, out)
		}
	}
}

// TestReadSkillByNameAndID 名字与目录 id 都能取到全文。
func TestReadSkillByNameAndID(t *testing.T) {
	store := newSkillStore(t, "正文标记 BODY-1")
	for _, key := range []string{"开发技能", "dev"} {
		out, err := runSkill(t, store, map[string]any{"name": key})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if !strings.Contains(out, "BODY-1") {
			t.Errorf("%s 应返回 SKILL.md 全文:\n%s", key, out)
		}
	}
	if _, err := runSkill(t, store, map[string]any{"name": "不存在"}); err == nil ||
		!strings.Contains(err.Error(), "先用 read_skill 不带 name") {
		t.Errorf("找不到技能时要说清下一步: %v", err)
	}
}

// TestReadSkillReferences 读引用资料：文件给内容，目录给清单。
func TestReadSkillReferences(t *testing.T) {
	store := newSkillStore(t, "正文")
	out, err := runSkill(t, store, map[string]any{"name": "dev", "path": "references"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "api.md") {
		t.Errorf("目录应列出文件: %s", out)
	}
	out, err = runSkill(t, store, map[string]any{"name": "dev", "path": "references/api.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "接口说明正文") {
		t.Errorf("应返回文件内容: %s", out)
	}
}

// TestReadSkillRejectsEscape 路径不能跑出技能目录（含软链）。
func TestReadSkillRejectsEscape(t *testing.T) {
	store := newSkillStore(t, "正文")
	dir := store.Roots()[0].Dir

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("别人的文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []map[string]any{
		{"name": "dev", "path": "../../etc/passwd"}, // ../../ 跑出技能根
		{"name": "dev", "path": outside},            // 绝对路径
	}
	for _, args := range cases {
		if _, err := runSkill(t, store, args); err == nil {
			t.Errorf("越界路径应被拒绝: %v", args)
		}
	}

	// 软链指到目录外：EvalSymlinks 之后必须还在技能目录内
	link := filepath.Join(dir, "dev", "refs-link")
	if err := os.Symlink(filepath.Dir(outside), link); err == nil {
		if _, err := runSkill(t, store, map[string]any{"name": "dev", "path": "refs-link/secret.txt"}); err == nil {
			t.Error("软链逃逸应被拒绝")
		}
	}
}

// TestReadSkillDisabled 技能层关闭时不冒充可用。
func TestReadSkillDisabled(t *testing.T) {
	if _, err := (&ReadSkillTool{}).Call(context.Background(), map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "技能层未启用") {
		t.Errorf("技能层关闭应明说: %v", err)
	}
}
