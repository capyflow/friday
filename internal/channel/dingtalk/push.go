// push.go 是钉钉侧的主动消息推送：心跳/智能任务输出经自定义机器人 webhook
// （群机器人"Webhook 接入"的地址，config chan.dingtalk.pushWebhook）投递。
// Stream 模式应用本身只支持"收消息+sessionWebhook 回复"，主动推送走独立 webhook。
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// WebhookNotifier 通过自定义机器人 webhook 推送主动消息，实现 agent.Notifier。
type WebhookNotifier struct {
	Webhook string // https://oapi.dingtalk.com/robot/send?access_token=xxx
}

// Name 返回通道名（与 task.Task.Channel 的过滤值一致）。
func (n *WebhookNotifier) Name() string { return "dingtalk" }

// Notify 推送一条文本消息到目标群。
func (n *WebhookNotifier) Notify(ctx context.Context, text string) error {
	if n.Webhook == "" {
		return fmt.Errorf("未配置钉钉推送 webhook（chan.dingtalk.pushWebhook）")
	}
	body, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("解析钉钉 webhook 响应失败: %w", err)
	}
	if out.ErrCode != 0 {
		return fmt.Errorf("钉钉 webhook 返回 errcode=%d: %s", out.ErrCode, out.ErrMsg)
	}
	return nil
}
