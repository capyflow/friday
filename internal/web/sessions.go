// sessions.go 管网页会话的清单与放行：网页聊天只有唯一的主会话（key web，
// 网页/钉钉/飞书/心跳都接在它上面）；心跳/计划/目标这些后台入口各有自己的
// 会话 key，它们不进聊天页，只在设置页的会话列表里可见——列出来是为了能
// 清上下文，不是为了让用户去那儿对话。
//
// 曾经的「工作会话」多会话登记表已随聊天页入口收敛移除（对话历史本体仍在
// 框架的 session.json 里，可达性只看主会话）。
package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/capyflow/friday/internal/agent"
)

// 会话种类：life 是主会话（聊天页那一侧），internal 是后台入口的会话
// （心跳/计划/目标这些），只在设置页的会话列表里出现。
const (
	SessionKindLife     = "life"
	SessionKindInternal = "internal"
)

// lifeSessionTitle 是主会话在界面上的名字（生活模式没有第二条，标题是常量）。
const lifeSessionTitle = "主会话"

// ChatSession 是会话列表接口对外返回的一条会话。
type ChatSession struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Messages  int       `json:"messages"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// resolveSessionKey 把请求里的 session 字段翻成可用的会话 key。
//
// 网页聊天只有主会话：空与 web 都落到 web，其余 key 一律拒掉。这里不能像
// 历史接口那样任传一个 key——那等于让任何一次请求都能凭空造出一条会话。
func resolveSessionKey(raw string) (string, error) {
	key := agent.CurrentKey(strings.TrimSpace(raw))
	if key == "" || key == agent.SessionWeb {
		return agent.SessionWeb, nil
	}
	return "", fmt.Errorf("会话 %q 不存在：网页聊天只有主会话，不传 session 即主会话", key)
}

// ListSessions 返回网页会话清单：主会话在最前，后台入口的会话（心跳/计划/目标）
// 按 key 排序垫底且只带 key。只读——不因为列一次清单就把空会话写进池里。
func (c *Chat) ListSessions(ctx context.Context) []ChatSession {
	poolIdx := map[string]agent.SessionInfo{}
	if c.brain != nil {
		for _, s := range c.brain.Pool().List() {
			poolIdx[s.Key] = s
		}
	}

	life := ChatSession{Key: agent.SessionWeb, Title: lifeSessionTitle, Kind: SessionKindLife}
	if info, ok := poolIdx[agent.SessionWeb]; ok {
		life.Messages = info.Messages
		life.UpdatedAt = info.UpdatedAt
	}
	out := []ChatSession{life}

	var internal []string
	for key := range poolIdx {
		if key != agent.SessionWeb {
			internal = append(internal, key)
		}
	}
	sort.Strings(internal)
	for _, key := range internal {
		info := poolIdx[key]
		out = append(out, ChatSession{Key: key, Kind: SessionKindInternal,
			Messages: info.Messages, UpdatedAt: info.UpdatedAt})
	}
	return out
}
