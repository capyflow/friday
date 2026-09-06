package logx

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestNextID递增(t *testing.T) {
	a, b := NextID(), NextID()
	if a == b {
		t.Fatalf("请求 ID 未递增: %s == %s", a, b)
	}
	if !strings.HasPrefix(a, "req-") {
		t.Fatalf("请求 ID 格式异常: %s", a)
	}
}

func TestWithID与IDFrom(t *testing.T) {
	ctx := context.Background()
	if got := IDFrom(ctx); got != "" {
		t.Fatalf("空 ctx 应无请求 ID，得到 %q", got)
	}
	if got := IDFrom(WithID(ctx, "req-7")); got != "req-7" {
		t.Fatalf("IDFrom = %q, 期望 req-7", got)
	}
}

func TestPrintf请求ID前缀(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Default().Writer())

	Printf(context.Background(), "hello %s", "world")
	if !strings.Contains(buf.String(), "hello world") || strings.Contains(buf.String(), "req-") {
		t.Fatalf("无请求 ID 时的日志异常: %q", buf.String())
	}

	buf.Reset()
	Printf(WithID(context.Background(), "req-9"), "链路测试 %d", 1)
	if !strings.Contains(buf.String(), "[req-9] 链路测试 1") {
		t.Fatalf("带请求 ID 的日志异常: %q", buf.String())
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"你好世界", 10, "你好世界"},
		{"你好世界", 2, "你好…"},
		{"", 3, ""},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.n); got != c.want {
			t.Fatalf("Truncate(%q, %d) = %q, 期望 %q", c.in, c.n, got, c.want)
		}
	}
}
