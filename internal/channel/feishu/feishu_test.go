package feishu

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/capyflow/vortexagent/llm"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/capyflow/friday/internal/channel"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/trace"
)

// fakeAPI 记录发送的消息。
type fakeAPI struct {
	mu   sync.Mutex
	sent []struct {
		chatID string
		text   string
	}
	err error
}

func (f *fakeAPI) SendText(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, struct {
		chatID string
		text   string
	}{chatID, text})
	return nil
}

func (f *fakeAPI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.text
	}
	return out
}

// stubAsker 固定回答（满足 channel.Asker）。
type stubAsker struct{ reply string }

func (s *stubAsker) Ask(_ context.Context, text string, _ []llm.Content, _ func(llm.Delta)) (string, error) {
	return "回答：" + text, nil
}

func TestParseTextContent(t *testing.T) {
	if got := parseTextContent(`{"text":"你好"}`); got != "你好" {
		t.Fatalf("parseTextContent = %q", got)
	}
	if got := parseTextContent(`not-json`); got != "" {
		t.Fatalf("非法 JSON 应返回空串: %q", got)
	}
	if got := parseTextContent(`{"text":""}`); got != "" {
		t.Fatalf("空文本应返回空串: %q", got)
	}
}

func TestStripAt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@小五 你好", "你好"},
		{"@Friday-bot 帮我记一下", "帮我记一下"},
		{"没有艾特", "没有艾特"},
		{"  @小五 缩进", "缩进"},
	}
	for _, c := range cases {
		if got := stripAt(c.in); got != c.want {
			t.Errorf("stripAt(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestSplitRunes(t *testing.T) {
	long := strings.Repeat("中", 4500)
	segs := splitRunes(long, 2000)
	if len(segs) != 3 {
		t.Fatalf("4500 个中文字符按 2000 分段应为 3 段，实际 %d", len(segs))
	}
	for _, seg := range segs {
		if len([]rune(seg)) > 2000 {
			t.Fatal("单段不应超过 2000 字符")
		}
	}
	var joined strings.Builder
	for _, seg := range segs {
		joined.WriteString(seg)
	}
	if joined.String() != long {
		t.Fatal("分段拼回应还原原文（不丢字符）")
	}
}

// makeEvent 构造一条 im.message.receive_v1 事件。
func makeEvent(chatID, openID, msgType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr(openID)},
			},
			Message: &larkim.EventMessage{
				ChatId:      strPtr(chatID),
				ChatType:    strPtr("group"),
				MessageType: strPtr(msgType),
				Content:     strPtr(content),
			},
		},
	}
}

func strPtr(s string) *string { return &s }

// TestOnReceiveGroupMessage 验收 M1.2：群聊 @机器人 → @ 前缀被剥除后进入分发。
func TestOnReceiveGroupMessage(t *testing.T) {
	api := &fakeAPI{}
	adapter := newWithAPI(config.FeishuConfig{AppID: "a", AppSecret: "b"}, &echoAsker{}, api)

	if err := adapter.onReceive(context.Background(),
		makeEvent("oc_chat1", "ou_u1", "text", `{"text":"@小五 现在几点"}`)); err != nil {
		t.Fatalf("onReceive: %v", err)
	}
	select {
	case inb := <-adapter.in:
		if inb.Text != "现在几点" {
			t.Fatalf("@ 前缀应被剥除，实际 %q", inb.Text)
		}
		if inb.ChatID != "oc_chat1" || inb.UserID != "ou_u1" {
			t.Fatalf("chatID/userID 不符: %+v", inb)
		}
		if inb.Channel != "feishu" {
			t.Fatalf("channel 不符: %s", inb.Channel)
		}
		if inb.SessionKey() != "feishu-oc_chat1" {
			t.Fatalf("会话 key 不符: %s", inb.SessionKey())
		}
	default:
		t.Fatal("消息应入队")
	}
}

// echoAsker 简单回显（满足 channel.Asker 签名）。
type echoAsker struct{}

func (e *echoAsker) Ask(_ context.Context, text string, _ []llm.Content, _ func(llm.Delta)) (string, error) {
	return "ok:" + text, nil
}

// TestOnReceiveIgnoresNonText 验证非文本消息被忽略。
func TestOnReceiveIgnoresNonText(t *testing.T) {
	api := &fakeAPI{}
	adapter := newWithAPI(config.FeishuConfig{}, &echoAsker{}, api)
	if err := adapter.onReceive(context.Background(),
		makeEvent("oc1", "ou1", "image", `{"image_key":"img_x"}`)); err != nil {
		t.Fatalf("onReceive: %v", err)
	}
	select {
	case <-adapter.in:
		t.Fatal("图片消息不应入队")
	default:
	}
}

// TestSendSegmentation 验收 M1.2：超长回答按 2000 字符分段回发。
func TestSendSegmentation(t *testing.T) {
	api := &fakeAPI{}
	adapter := newWithAPI(config.FeishuConfig{}, &echoAsker{}, api)
	long := strings.Repeat("字", 4500)
	err := adapter.send(context.Background(), channel.Inbound{Reply: "oc_chat9"}, long)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if texts := api.texts(); len(texts) != 3 {
		t.Fatalf("应分 3 段，实际 %d", len(texts))
	}
}

// TestDispatchTraceSource 验收 M1.2：飞书消息留痕 source=feishu。
func TestDispatchTraceSource(t *testing.T) {
	api := &fakeAPI{}
	adapter := newWithAPI(config.FeishuConfig{}, &echoAsker{}, api)
	in := make(chan channel.Inbound, 4)
	done := make(chan struct{})
	go func() {
		channel.Dispatch(context.Background(), in, adapter.ask, nil, adapter.send)
		close(done)
	}()
	in <- channel.Inbound{Channel: "feishu", ChatID: "oc_t", UserID: "ou_t", Text: "你好", Reply: "oc_t"}
	close(in)
	<-done

	traces := trace.List()
	found := false
	for _, tr := range traces {
		if tr.Source == "feishu" && tr.Input == "你好" && tr.Session == "feishu-oc_t" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应产生 source=feishu 的轨迹: %+v", traces)
	}
}

// TestPushNotifier 验证心跳推送器向全部 chat_id 发送并聚合错误。
func TestPushNotifier(t *testing.T) {
	api := &fakeAPI{}
	n := &PushNotifier{API: api, ChatIDs: []string{"oc_a", "oc_b"}}
	if err := n.Notify(context.Background(), "今日提醒"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(api.texts()) != 2 {
		t.Fatalf("应发送 2 条，实际 %d", len(api.texts()))
	}

	api2 := &fakeAPI{err: errors.New("网络错误")}
	n2 := &PushNotifier{API: api2, ChatIDs: []string{"oc_a"}}
	if err := n2.Notify(context.Background(), "x"); err == nil {
		t.Fatal("发送失败应返回错误")
	}
	if n2.Name() != "feishu" {
		t.Fatalf("Name 应为 feishu: %s", n2.Name())
	}
}
