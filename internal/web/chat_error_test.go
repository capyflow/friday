package web

import (
	"errors"
	"strings"
	"testing"
)

// TestChatErrorMessage 错误要能照着做：以前所有失败都显示「出了点小问题」，
// 用户完全看不出是预算不够、密钥坏了还是被限流。
func TestChatErrorMessage(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		wants []string
	}{
		{
			name:  "只有思考没有正文",
			err:   errors.New("agent: 模型未返回任何文本内容"),
			wants: []string{"思考", "最大输出 token", "设置 → 模型服务", "8192"},
		},
		{
			name:  "模型不支持图片",
			err:   errors.New("agent: 第 2 轮调用失败: model does not support image input"),
			wants: []string{"不支持图片输入"},
		},
		{
			name:  "超窗",
			err:   errors.New("agent: 第 1 轮调用失败: this model's maximum context length is 8192 tokens"),
			wants: []string{"上下文窗口", "清除上下文"},
		},
		{
			name:  "密钥被拒",
			err:   errors.New("agent: 第 1 轮调用失败: 401 Unauthorized: invalid api key"),
			wants: []string{"密钥"},
		},
		{
			name:  "限流",
			err:   errors.New("agent: 第 1 轮调用失败: 429 Too Many Requests: rate limit exceeded"),
			wants: []string{"限流"},
		},
		{
			name:  "连不上",
			err:   errors.New("agent: 第 1 轮调用失败: dial tcp: lookup api.example.com: no such host"),
			wants: []string{"连不上模型服务", "API 地址"},
		},
		{
			name:  "未知错误带出原文",
			err:   errors.New("agent: 第 3 轮调用失败: 某个服务商自创的怪错误"),
			wants: []string{"这轮回复失败了", "某个服务商自创的怪错误"},
		},
	}
	for _, tc := range cases {
		got := chatErrorMessage(tc.err)
		for _, w := range tc.wants {
			if !strings.Contains(got, w) {
				t.Errorf("%s: 提示应含 %q，实际 %q", tc.name, w, got)
			}
		}
		if got == "" || strings.Contains(got, "出了点小问题") {
			t.Errorf("%s: 不该再出现万能兜底文案: %q", tc.name, got)
		}
	}
}
