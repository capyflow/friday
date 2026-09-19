// Package feishu 是飞书通道适配器（M1.2）：基于飞书开放平台长连接（larkws，
// WebSocket 免公网 IP）收事件，经 channel.Dispatch 串行调用对话大脑，
// 回答用 im/v1/messages API 回发（按 2000 字符分段）。
// 接入准备：飞书开放平台创建企业自建应用 → 开启机器人能力 → 订阅
// im.message.receive_v1 事件（长连接方式）→ 申请 im:message 读取与发消息权限。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/capyflow/friday/internal/channel"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
)

const (
	inQueueSize  = 16
	replySegSize = 2000 // 飞书文本消息分段长度（字符），与钉钉对齐
)

// messageAPI 抽象发消息能力（生产用 lark API 客户端，测试用假实现）。
type messageAPI interface {
	SendText(ctx context.Context, chatID, text string) error
}

// Adapter 是飞书通道。
type Adapter struct {
	cfg  config.FeishuConfig
	ask  channel.Asker
	in   chan channel.Inbound
	api  messageAPI
	atRe *regexp.Regexp // 剥离群聊 @机器人 前缀
}

// New 创建飞书适配器（生产：用 appID/appSecret 构造 lark API 客户端发消息）。
func New(cfg config.FeishuConfig, ask channel.Asker) *Adapter {
	return newWithAPI(cfg, ask, &larkAPI{client: lark.NewClient(cfg.AppID, cfg.AppSecret)})
}

// newWithAPI 注入消息 API（测试用）。
func newWithAPI(cfg config.FeishuConfig, ask channel.Asker, api messageAPI) *Adapter {
	return &Adapter{
		cfg:  cfg,
		ask:  ask,
		in:   make(chan channel.Inbound, inQueueSize),
		api:  api,
		atRe: regexp.MustCompile(`^@\S+\s*`),
	}
}

// Name 返回通道名。
func (a *Adapter) Name() string { return "feishu" }

// Start 阻塞运行：启动分发器 goroutine 与 larkws 长连接客户端（SDK 自带
// 自动重连），ctx 取消或客户端退出时返回。
func (a *Adapter) Start(ctx context.Context) error {
	go channel.Dispatch(ctx, a.in, a.ask, a.cfg.AllowOpenIDs, a.send)

	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(a.onReceive)
	cli := larkws.NewClient(a.cfg.AppID, a.cfg.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithAutoReconnect(true),
	)
	logx.Printf(ctx, "[通道:feishu] 长连接客户端启动（appID=%s）", a.cfg.AppID)
	return cli.Start(ctx)
}

// onReceive 处理 im.message.receive_v1 事件：只处理文本消息，转 Inbound 入队
// （非阻塞，队列满则丢弃并记日志）。回答由分发器异步产生后经 API 回发。
func (a *Adapter) onReceive(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return nil
	}
	msg := ev.Event.Message
	if msg.MessageType == nil || *msg.MessageType != "text" || msg.Content == nil {
		return nil // 只对话文本；图片/文件等类型忽略
	}
	text := stripAt(parseTextContent(*msg.Content))
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var chatID, senderID, senderName string
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}
	if ev.Event.Sender != nil && ev.Event.Sender.SenderId != nil {
		senderID = deref(ev.Event.Sender.SenderId.OpenId)
	}
	// 飞书事件体不直接携带昵称，open_id 兼作日志标识。
	senderName = senderID
	if senderID == "" {
		senderID = chatID // 兜底：白名单语义退化为按会话过滤
	}
	inb := channel.Inbound{
		Channel:  "feishu",
		ChatID:   chatID,
		UserID:   senderID,
		UserName: senderName,
		Text:     text,
		Reply:    chatID, // 回复上下文：chat_id（发消息 API 用 receive_id_type=chat_id）
	}
	select {
	case a.in <- inb:
	default:
		logx.Printf(ctx, "[通道:feishu] 队列已满，丢弃消息（%s）", text)
	}
	return nil
}

// send 把回答经发消息 API 回发到原会话，超长按 2000 字符分段。
func (a *Adapter) send(ctx context.Context, in channel.Inbound, replyText string) error {
	chatID, ok := in.Reply.(string)
	if !ok || chatID == "" {
		return fmt.Errorf("缺少 chat_id 回复上下文")
	}
	for _, seg := range splitRunes(replyText, replySegSize) {
		if err := a.api.SendText(ctx, chatID, seg); err != nil {
			return err
		}
	}
	return nil
}

// parseTextContent 解析消息 content JSON（{"text":"..."}），非法/非文本返回空串。
func parseTextContent(content string) string {
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return ""
	}
	return obj.Text
}

// stripAt 剥离群聊 @机器人 前缀（"@小五 你好" → "你好"）。
func stripAt(text string) string {
	atRe := regexp.MustCompile(`^@\S+\s*`)
	return strings.TrimSpace(atRe.ReplaceAllString(strings.TrimSpace(text), ""))
}

// splitRunes 按 rune 分段（避免切开多字节字符）。
func splitRunes(text string, size int) []string {
	runes := []rune(text)
	if size <= 0 {
		size = replySegSize
	}
	var segs []string
	for len(runes) > size {
		segs = append(segs, string(runes[:size]))
		runes = runes[size:]
	}
	segs = append(segs, string(runes))
	return segs
}

// deref 安全解引用字符串指针。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
