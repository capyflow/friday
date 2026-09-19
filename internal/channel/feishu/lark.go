// lark.go 是飞书 API 客户端实现（发消息）与心跳推送 Notifier。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// larkAPI 用飞书官方 SDK 发文本消息（receive_id_type=chat_id）。
type larkAPI struct {
	client *lark.Client
}

// SendText 发送一条文本消息到指定会话（群/单聊通用，机器人须在会话内且有发消息权限）。
func (a *larkAPI) SendText(ctx context.Context, chatID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := a.client.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, "响应为空"
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return fmt.Errorf("飞书发送失败 code=%d: %s", code, msg)
	}
	return nil
}

// NewPushNotifier 创建基于官方 SDK 的推送器（发送到配置的 chat_id 列表）。
func NewPushNotifier(appID, appSecret string, chatIDs []string) *PushNotifier {
	return &PushNotifier{API: &larkAPI{client: lark.NewClient(appID, appSecret)}, ChatIDs: chatIDs}
}

// PushNotifier 把心跳/智能任务的输出推送到配置的 chat_id 列表
// （config chan.feishu.pushChatIDs）。实现 agent.Notifier 接口。
type PushNotifier struct {
	API     messageAPI
	ChatIDs []string
}

// Name 返回通道名（与 task.Task.Channel 的过滤值一致）。
func (p *PushNotifier) Name() string { return "feishu" }

// Notify 依次发送到所有目标会话；多个失败时合并报错。
func (p *PushNotifier) Notify(ctx context.Context, text string) error {
	var errs []string
	for _, chatID := range p.ChatIDs {
		if strings.TrimSpace(chatID) == "" {
			continue
		}
		if err := p.API.SendText(ctx, strings.TrimSpace(chatID), text); err != nil {
			errs = append(errs, chatID+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return &pushError{details: errs}
	}
	return nil
}

type pushError struct{ details []string }

func (e *pushError) Error() string {
	return "部分推送失败: " + strings.Join(e.details, "；")
}
