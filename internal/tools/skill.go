// skill.go 提供 read_skill：按需读取技能正文与它的引用资料。
//
// 为什么需要它：技能命中时不再把（可能很长的）正文整段塞进系统提示词——那样每一轮
// 都贵。超过预算的技能只给概览，模型真要按这份技能做事时再用这个工具取全文，或取
// references/ 里的具体文件。这和 Anthropic Agent Skills、Cursor Rules 的
// agent-requested 一类渐进披露是同一套思路：常驻只留索引，细节按需加载。
//
// 它读的是技能根内的第一方资源，所以不套 file.allowDirs 白名单；但和文件工具一样
// 防逃逸（Clean + EvalSymlinks + 前缀校验），软链指到技能目录外面也会被拒。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/capyflow/friday/internal/skill"
)

const (
	// skillReadMaxBytes 是单次读取上限：一次把几十 KB 塞回上下文没有意义，
	// 让模型按文件分段读（与 read_file 的量级保持一致）。
	skillReadMaxBytes = 32 << 10
	// skillListMax 是目录列举上限，避免把巨大的 references/ 一把摊开。
	skillListMax = 200
	// skillIndexDescRunes 是索引里每条技能描述的长度：够认出是哪件事即可。
	skillIndexDescRunes = 90
)

// ReadSkillName 是工具名：技能收窄 allowed-tools 时也要把它补进去（否则模型取不到
// 自己那份被概览替代的正文）。
const ReadSkillName = "read_skill"

// ReadSkillTool 按需读取技能正文与引用资料。
type ReadSkillTool struct {
	Store *skill.Store
}

func (t *ReadSkillTool) Name() string { return ReadSkillName }

func (t *ReadSkillTool) Description() string {
	return "读取技能的完整说明（SKILL.md）或它目录下的引用资料。系统提示词里超长的技能只给了概览，" +
		"要按那份技能做事时用它取全文；name 留空则列出全部可用技能。"
}

func (t *ReadSkillTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string",
				"description": "技能名或 id；留空则列出全部技能（含概览，用来挑）"},
			"path": map[string]any{"type": "string",
				"description": `技能目录下的相对路径（如 "references/api.md"），"." 表示列出该技能目录；留空读 SKILL.md 全文`},
		},
	}
}

func (t *ReadSkillTool) Call(_ context.Context, args map[string]any) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("技能层未启用")
	}
	name := strings.TrimSpace(stringArg(args, "name"))
	if name == "" {
		return t.index(), nil
	}
	e, ok := t.Store.Find(name)
	if !ok {
		if e2, ok2 := t.Store.FindName(name); ok2 { // 名字对不上 id 时再按技能名找一遍
			e = e2
		}
	}
	if !ok && e == nil {
		return "", fmt.Errorf("没有叫「%s」的技能（先用 read_skill 不带 name 列出全部）", name)
	}
	rel := strings.TrimSpace(stringArg(args, "path"))
	if rel == "" {
		return t.readFile(e, filepath.Join(e.Dir(), "SKILL.md"), "技能「"+e.Name()+"」正文")
	}
	dir, err := t.safeJoin(e.Dir(), rel)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if st.IsDir() {
		return listSkillDir(e, dir)
	}
	return t.readFile(e, dir, "技能「"+e.Name()+"」资料 "+rel)
}

// index 列出全部技能：模型不知道技能名时先看这个，再按名字取正文。
func (t *ReadSkillTool) index() string {
	list := t.Store.List()
	if len(list) == 0 {
		return "当前没有可用技能。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个技能（要正文就 read_skill({\"name\":\"<名字>\"})）：\n", len(list))
	for _, s := range list {
		name := s.Name
		if name == "" {
			name = "（未命名：" + s.ID + "）"
		}
		fmt.Fprintf(&b, "- %s（id: %s）", name, s.ID)
		if s.Source != "" {
			fmt.Fprintf(&b, " · %s", s.Source)
		}
		if s.Description != "" {
			desc := []rune(s.Description)
			if len(desc) > skillIndexDescRunes {
				fmt.Fprintf(&b, "：%s…", string(desc[:skillIndexDescRunes]))
			} else {
				fmt.Fprintf(&b, "：%s", s.Description)
			}
		}
		if s.ParseErr != "" {
			b.WriteString("（文件无效，正文可能读不动）")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// safeJoin 把相对路径解析到技能目录内并挡掉逃逸（含软链）。
func (t *ReadSkillTool) safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path 必须是技能目录内的相对路径: %s", rel)
	}
	base := filepath.Clean(root)
	target := filepath.Clean(filepath.Join(base, rel))
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path 超出技能目录: %s", rel)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("路径不存在或无法访问: %s", rel)
	}
	realRoot, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("技能目录无法访问: %w", err)
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path 经软链指向了技能目录之外: %s", rel)
	}
	return real, nil
}

// readFile 读取一个文件并加上下文抬头；超长截断并说明。
func (t *ReadSkillTool) readFile(e *skill.Entry, path, label string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if st.Size() > skillReadMaxBytes {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取失败: %w", err)
		}
		text := strings.ToValidUTF8(string(data[:skillReadMaxBytes]), "") // 截断处别留下半个汉字
		return fmt.Sprintf("%s（%s，共 %d 字节，以下只给前 %d 字节——需要更多请按文件分段读）：\n%s\n…（已截断）",
			label, path, st.Size(), skillReadMaxBytes, text), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	return fmt.Sprintf("%s（%s）：\n%s", label, path, strings.TrimSpace(string(data))), nil
}

// listSkillDir 列一个技能子目录（references/ 之类），目录在前。
func listSkillDir(e *skill.Entry, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	var b strings.Builder
	fmt.Fprintf(&b, "技能「%s」的资料目录 %s（共 %d 项", e.Name(), dir, len(entries))
	if len(entries) > skillListMax {
		fmt.Fprintf(&b, "，只列前 %d 项", skillListMax)
	}
	b.WriteString("）：\n")
	for i, d := range entries {
		if i >= skillListMax {
			break
		}
		if d.IsDir() {
			fmt.Fprintf(&b, "[目录] %s/\n", d.Name())
			continue
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(&b, "[文件] %s（%d 字节）\n", d.Name(), info.Size())
			continue
		}
		fmt.Fprintf(&b, "[文件] %s\n", d.Name())
	}
	b.WriteString("读具体文件：read_skill({\"name\":\"" + e.Name() + "\",\"path\":\"<上面的相对路径>\"})")
	return b.String(), nil
}
