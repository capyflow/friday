package dingtalk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/channel"
	"github.com/capyflow/friday/internal/config"
)

func TestTrimAtPrefix(t *testing.T) {
	a := New(config.DingtalkConfig{}, nil)
	cases := []struct{ in, want string }{
		{"@小五 你好", "你好"},
		{"@小五", ""},
		{"@小五\n帮我记事", "帮我记事"},
		{"你好（无@）", "你好（无@）"},
	}
	for _, c := range cases {
		if got := strings.TrimSpace(a.atRe.ReplaceAllString(c.in, "")); got != c.want {
			t.Errorf("trimAt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type fakeAsker struct {
	last string
	err  error
}

func (f *fakeAsker) Ask(_ context.Context, text string, _ []llm.Content, _ func(llm.Delta)) (string, error) {
	f.last = text
	if f.err != nil {
		return "", f.err
	}
	return "回答：" + text, nil
}

func TestSendSegmentsLongReply(t *testing.T) {
	a := New(config.DingtalkConfig{}, nil)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		bodies = append(bodies, req.Text.Content)
	}))
	defer srv.Close()
	long := strings.Repeat("好", 4500)
	if err := a.send(context.Background(), channel.Inbound{Reply: srv.URL}, long); err != nil {
		t.Fatalf("send 失败: %v", err)
	}
	if len(bodies) != 3 { // 4500 字符按 2000 分段 → 3 条
		t.Fatalf("应分段为 3 条，实际 %d 条", len(bodies))
	}
	if got := strings.Join(bodies, ""); got != long {
		t.Fatal("分段拼回应与原文一致")
	}
}

func TestSendMissingWebhook(t *testing.T) {
	a := New(config.DingtalkConfig{}, nil)
	if err := a.send(context.Background(), channel.Inbound{}, "hi"); err == nil {
		t.Fatal("缺少回复上下文应报错")
	}
}
