// Package dingtalk 是钉钉通道适配器：基于钉钉开放平台 Stream 模式（WebSocket，
// 免公网 IP），官方 SDK 自动重连。收到机器人消息后转成 channel.Inbound 投入
// 分发器，回答经 sessionWebhook 回发（按 2000 字符分段）。
package dingtalk

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/capyflow/friday/internal/channel"
	"github.com/capyflow/friday/internal/config"
	"github.com/capyflow/friday/internal/logx"
)

const (
	inQueueSize  = 16
	replySegSize = 2000 // 钉钉文本消息分段长度（字符）
)

// Adapter 是钉钉通道。
type Adapter struct {
	cfg  config.DingtalkConfig
	ask  channel.Asker
	in   chan channel.Inbound
	atRe *regexp.Regexp // 剥离群聊 @机器人 前缀
}

// New 创建钉钉适配器。ask 为对话大脑，消息经分发器串行处理。
func New(cfg config.DingtalkConfig, ask channel.Asker) *Adapter {
	return &Adapter{
		cfg:  cfg,
		ask:  ask,
		in:   make(chan channel.Inbound, inQueueSize),
		atRe: regexp.MustCompile(`^@\S+\s*`),
	}
}

// Name 返回通道名。
func (a *Adapter) Name() string { return "dingtalk" }

// Start 阻塞运行：启动分发器 goroutine 与 Stream 客户端（SDK 自带断线重连），
// ctx 取消或客户端退出时返回。
func (a *Adapter) Start(ctx context.Context) error {
	go channel.Dispatch(ctx, a.in, a.ask, a.cfg.AllowUserIDs, a.send)

	cli := client.NewStreamClient(
		client.WithAppCredential(client.NewAppCredentialConfig(a.cfg.ClientID, a.cfg.ClientSecret)),
		client.WithAutoReconnect(true),
	)
	cli.RegisterChatBotCallbackRouter(a.onMessage)
	logx.Printf(ctx, "[通道:dingtalk] Stream 客户端启动（clientID=%s）", a.cfg.ClientID)
	return cli.Start(ctx)
}

// onMessage 是机器人消息回调：转 Inbound 入队（非阻塞，队列满则丢弃并记日志）。
// 回答由分发器异步产生后经 sessionWebhook 回发，这里立即返回空响应。
func (a *Adapter) onMessage(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	text := strings.TrimSpace(data.Text.Content)
	text = strings.TrimSpace(a.atRe.ReplaceAllString(text, "")) // 群聊剥 @机器人
	if text == "" {
		return []byte(""), nil
	}
	user := data.SenderId
	if user == "" {
		user = data.SenderStaffId
	}
	inb := channel.Inbound{
		Channel:  "dingtalk",
		ChatID:   data.ConversationId,
		UserID:   user,
		UserName: data.SenderNick,
		Text:     text,
		Reply:    data.SessionWebhook,
	}
	select {
	case a.in <- inb:
	default:
		logx.Printf(ctx, "[通道:dingtalk] 队列已满，丢弃消息（%s）", text)
	}
	return []byte(""), nil
}

// send 把回答经 sessionWebhook 回发，超长按 2000 字符分段。
func (a *Adapter) send(ctx context.Context, in channel.Inbound, replyText string) error {
	webhook, ok := in.Reply.(string)
	if !ok || webhook == "" {
		return fmt.Errorf("缺少 sessionWebhook 回复上下文")
	}
	replier := chatbot.NewChatbotReplier()
	runes := []rune(replyText) // 按 rune 分段，避免把多字节字符切开
	for len(runes) > replySegSize {
		if err := replier.SimpleReplyText(ctx, webhook, []byte(string(runes[:replySegSize]))); err != nil {
			return err
		}
		runes = runes[replySegSize:]
	}
	return replier.SimpleReplyText(ctx, webhook, []byte(string(runes)))
}
