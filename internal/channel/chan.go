// Package channel 定义外部消息通道（钉钉/飞书/企业微信…）的公共层：
// 各通道适配器把平台消息转成 Inbound 投入 Dispatch，由它串行调用对话大脑
// （Brain 实现 Asker 接口）并把回答经 Sender 发回平台。
// 串行处理天然与 Brain 的全局互斥对齐：处理期间新消息在缓冲队列排队。
package channel

import (
	"context"
	"strings"

	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

// Inbound 是一条来自外部通道的用户消息。Reply 为通道自定义的回复上下文
// （如钉钉的 sessionWebhook），Sender 收到回答后据此回发。
type Inbound struct {
	Channel  string // 通道名：dingtalk / feishu / ...
	ChatID   string // 会话标识（单聊/群聊 ID，日志用）
	UserID   string // 发送者 ID（白名单过滤的依据）
	UserName string // 发送者昵称（日志用）
	Text     string // 消息文本（已剥离 @ 前缀）
	Reply    any    // 回复上下文
}

// Asker 是对话能力抽象：Brain 实现它，通道层不依赖具体实现。
type Asker interface {
	Ask(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta)) (string, error)
}

// Sender 把回答发回通道（由各适配器实现，如钉钉的 SimpleReplyText）。
type Sender func(ctx context.Context, in Inbound, replyText string) error

// Dispatch 串行消费入站消息直到 ctx 取消：
//   - 白名单 allowUserIDs 非空时，仅放行列表内的 UserID（其余静默忽略并记日志）；
//   - 一次处理一条，处理期间新消息在缓冲队列排队（不并发、不交叉）；
//   - 每条消息分配请求 ID 并留痕（source=通道名），会话 key 统一为主会话——
//     全局单对话：所有渠道与网页/终端共享同一个小五；
//   - Ask 出错时向用户回一句兜底话术。
func Dispatch(ctx context.Context, in <-chan Inbound, asker Asker, allowUserIDs []string, send Sender) {
	allowed := make(map[string]bool, len(allowUserIDs))
	for _, id := range allowUserIDs {
		allowed[strings.TrimSpace(id)] = true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case inb, ok := <-in:
			if !ok {
				return
			}
			if len(allowUserIDs) > 0 && !allowed[inb.UserID] {
				logx.Printf(ctx, "[通道:%s] 非白名单用户 %s(%s)，已忽略",
					inb.Channel, inb.UserID, inb.UserName)
				continue
			}
			if strings.TrimSpace(inb.Text) == "" {
				continue
			}
			rctx := logx.WithID(ctx, logx.NextID())
			rctx = agent.WithSessionKey(rctx, agent.SessionWeb)
			id := logx.IDFrom(rctx)
			logx.Printf(rctx, "[通道:%s] %s(%s): %s", inb.Channel, inb.UserName, inb.UserID, inb.Text)
			trace.Begin(id, inb.Channel, agent.SessionWeb, inb.Text)
			reply, err := asker.Ask(rctx, inb.Text, nil, nil)
			if err != nil {
				if rctx.Err() != nil {
					return
				}
				logx.Printf(rctx, "[通道:%s] 对话出错: %v", inb.Channel, err)
				trace.End(id, err.Error())
				reply = "出了点小问题，稍后再试试吧"
			} else {
				trace.End(id, "")
			}
			if strings.TrimSpace(reply) == "" {
				continue
			}
			if err := send(ctx, inb, reply); err != nil {
				logx.Printf(ctx, "[通道:%s] 回复发送失败: %v", inb.Channel, err)
			}
		}
	}
}
