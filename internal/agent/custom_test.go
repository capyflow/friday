package agent

import (
	"testing"

	"github.com/capyflow/friday/internal/config"
)

func TestExpandCustom(t *testing.T) {
	b := &Brain{} // expandCustom 不依赖 agent/pool，零值可用
	b.SetCustom([]config.CustomCommand{
		{Name: "早安简报", Prompt: " 请给我一份早安简报：日期、天气、今日待办。 "},
		{Name: " 空提示词 ", Prompt: "   "},
	})

	cases := []struct {
		in, want string
		matched  bool
	}{
		{"早安简报", "请给我一份早安简报：日期、天气、今日待办。", true},
		{"  早安简报  ", "请给我一份早安简报：日期、天气、今日待办。", true},
		{"早安简报 只看天气和待办", "请给我一份早安简报：日期、天气、今日待办。\n\n用户补充：只看天气和待办", true},
		{"早安简报?", "早安简报?", false},   // 非整词命中不展开
		{"来份早安简报", "来份早安简报", false}, // 前缀不属于触发词
		{"空提示词", "空提示词", false},     // 提示词为空视为未配置
		{"", "", false},             // 空输入原样
		{"普通聊天内容", "普通聊天内容", false}, // 未命中原样
	}
	for _, c := range cases {
		got, matched := b.expandCustom(c.in)
		if got != c.want || matched != c.matched {
			t.Errorf("expandCustom(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, matched, c.want, c.matched)
		}
	}
}

func TestSetCustomReplaces(t *testing.T) {
	b := &Brain{}
	b.SetCustom([]config.CustomCommand{{Name: "旧", Prompt: "旧指令"}})
	b.SetCustom([]config.CustomCommand{{Name: "新", Prompt: "新指令"}})
	if got, _ := b.expandCustom("旧"); got != "旧" {
		t.Errorf("旧指令应已失效，实际展开为 %q", got)
	}
	if got, matched := b.expandCustom("新"); got != "新指令" || !matched {
		t.Errorf("新指令应生效，实际 (%q,%v)", got, matched)
	}
}
