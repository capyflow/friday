package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProjectContextNothingToSay 没有 AGENTS.md / README.md 时什么都不注入：
// 个人助理跑在自己的数据目录里是常态，不该为此多一行提示词。
func TestProjectContextNothingToSay(t *testing.T) {
	dir := t.TempDir()
	if got := projectContext(dir); got != "" {
		t.Errorf("两个文件都没有时不该注入: %q", got)
	}
	if got := projectContext(""); got != "" {
		t.Errorf("没配工作目录不该注入: %q", got)
	}
}

func TestProjectContextAgentsFullAndReadmeBrief(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "AGENTS.md", "所有回复用中文；改文件前先跑测试。")
	got := projectContext(dir)
	if !strings.Contains(got, "改文件前先跑测试") {
		t.Errorf("AGENTS.md 该整段进上下文: %q", got)
	}
	if strings.Contains(got, "README") {
		t.Errorf("没有 README 时不该提它: %q", got)
	}
	if !strings.Contains(got, "不是用户说的话") {
		t.Error("要标明这是自动注入的背景，不是用户消息")
	}

	// README 长文只给开头一段，并指路 read_file——整段进系统提示词每轮都贵。
	write(t, dir, "README.md", "# Friday\n\n"+strings.Repeat("甲", readmeMaxRunes+500))
	got = projectContext(dir)
	if !strings.Contains(got, "已截断") || !strings.Contains(got, "read_file") {
		t.Errorf("README 摘要要说明被截断并指路 read_file: %q", got)
	}
	if strings.Contains(got, strings.Repeat("甲", readmeMaxRunes+1)) {
		t.Error("README 不该整段塞进来")
	}
}

// TestProjectContextTruncationIsHonest AGENTS.md 超长时必须写明"还剩多少、去哪看"，
// 且按 rune 截断不能切坏中文。
func TestProjectContextTruncationIsHonest(t *testing.T) {
	dir := t.TempDir()
	total := agentsMaxRunes + 1000
	write(t, dir, "AGENTS.md", strings.Repeat("乙", total))
	got := projectContext(dir)
	if !strings.Contains(got, "只放了前") || !strings.Contains(got, "read_file") {
		t.Errorf("截断要说明并指路: %q", got)
	}
	if !strings.Contains(got, "共 7000 字") {
		t.Errorf("要报出原文总字数（%d）: %q", total, got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("按 rune 截断不该产生替换字符")
	}
	if n := utf8.RuneCountInString(got); n > agentsMaxRunes+1500 {
		t.Errorf("注入内容远超上限: %d rune", n)
	}
}

// TestProjectContextFileNames 大小写两种写法都认（macOS 不敏感、Linux 敏感）。
func TestProjectContextFileNames(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "agents.md", "小写文件名也要认。")
	if got := projectContext(dir); !strings.Contains(got, "小写文件名也要认") {
		t.Errorf("agents.md 没被认出来: %q", got)
	}
}

// TestBrainProjectDir 项目上下文只在新会话（历史为空）时垫一条系统消息，
// 与记忆速览同一套做法：避免每轮重复计费。
func TestBrainProjectDir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "AGENTS.md", "按项目约定来。")
	b := &Brain{}
	if b.projectWarmup() != "" {
		t.Error("没设工作目录时不该有项目上下文")
	}
	b.SetProjectDir(dir)
	if got := b.projectWarmup(); !strings.Contains(got, "按项目约定来") {
		t.Errorf("设了工作目录就该读到: %q", got)
	}
	b.SetProjectDir("")
	if b.projectWarmup() != "" {
		t.Error("清空工作目录等于关掉注入")
	}
}
