package channel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capyflow/vortexagent/llm"
)

type stubAsker struct {
	mu   sync.Mutex
	last string
	err  error
}

func (s *stubAsker) Ask(_ context.Context, text string, _ []llm.Content, _ func(llm.Delta)) (string, error) {
	s.mu.Lock()
	s.last = text
	s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	return "回答：" + text, nil
}

func runDispatch(t *testing.T, allow []string, asker Asker) (chan<- Inbound, *[]Outbox, *sync.WaitGroup) {
	t.Helper()
	in := make(chan Inbound, 16)
	var sentMu sync.Mutex
	sent := &[]Outbox{}
	done := &sync.WaitGroup{}
	done.Add(1)
	go func() {
		defer done.Done()
		Dispatch(context.Background(), in, asker, allow, func(_ context.Context, inb Inbound, reply string) error {
			sentMu.Lock()
			*sent = append(*sent, Outbox{in: inb, reply: reply})
			sentMu.Unlock()
			return nil
		})
	}()
	return in, sent, done
}

type Outbox struct {
	in    Inbound
	reply string
}

func waitClose(d *sync.WaitGroup, in chan<- Inbound) {
	time.Sleep(150 * time.Millisecond) // 留时间处理
	close(in)
	d.Wait()
}

func TestDispatchWhitelist(t *testing.T) {
	asker := &stubAsker{}
	in, sent, done := runDispatch(t, []string{"user-ok"}, asker)
	in <- Inbound{Channel: "dingtalk", UserID: "user-ok", Text: "你好"}
	in <- Inbound{Channel: "dingtalk", UserID: "stranger", UserName: "陌生人", Text: "偷看"}
	waitClose(done, in)
	if len(*sent) != 1 || (*sent)[0].in.UserID != "user-ok" {
		t.Fatalf("白名单过滤不符: %+v", *sent)
	}
}

func TestDispatchErrorFallback(t *testing.T) {
	in, sent, done := runDispatch(t, nil, &stubAsker{err: context.DeadlineExceeded})
	in <- Inbound{Channel: "dingtalk", UserID: "u", Text: "你好"}
	waitClose(done, in)
	if len(*sent) != 1 || !strings.Contains((*sent)[0].reply, "出了点小问题") {
		t.Fatalf("出错时应回兜底话术: %+v", *sent)
	}
}

func TestDispatchSerialOrder(t *testing.T) {
	asker := &stubAsker{}
	in, sent, done := runDispatch(t, nil, asker)
	for i := 1; i <= 5; i++ {
		in <- Inbound{UserID: "u", Text: "消息" + string(rune('0'+i))}
	}
	waitClose(done, in)
	if len(*sent) != 5 {
		t.Fatalf("应处理 5 条，实际 %d", len(*sent))
	}
	for i := 1; i < 5; i++ {
		if strings.Compare((*sent)[i-1].reply, (*sent)[i].reply) > 0 {
			t.Fatal("回答顺序应与消息顺序一致（串行）")
		}
	}
	asker.mu.Lock()
	if asker.last != "消息5" {
		t.Fatalf("最后处理应为消息5，实际 %q", asker.last)
	}
	asker.mu.Unlock()
}
