// brain.go 定义机器人的"对话大脑"：所有入口（网页/终端/消息通道/定时任务）
// 共用同一个 Agent，按会话 key（SessionPool）持有各自独立的对话历史。
// 同一会话内的请求串行执行（上下文连续、流式不交叉），不同会话可并行对话；
// 流式增量经 Agent.WithDelta 按请求路由到各自的订阅者，互不串扰。
package agent

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// Brain 持有 Agent 与会话池，按会话 key 串行化提问。
// rw 锁保护 Agent 的原子替换：Ask 持读锁（不同会话并行），SetAgent 持写锁
// （等待全部进行中的请求结束后生效）。
type Brain struct {
	rw    sync.RWMutex
	agent *vxagent.Agent
	pool  *SessionPool
}

// NewBrain 创建对话大脑。pool 为会话池（含持久化存储），由调用方构建。
func NewBrain(ag *vxagent.Agent, pool *SessionPool) *Brain {
	return &Brain{agent: ag, pool: pool}
}

// Ask 提问并等待完整回答。会话由 ctx 中的会话 key 决定（WithSessionKey，
// 未设置时用网页会话）；onDelta 为本次请求的流式增量回调（可为 nil）；
// imgs 为随消息发送的图片内容（可为 nil），会作为一条独立的用户消息
// 紧邻问题文本之前注入会话。
func (b *Brain) Ask(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta)) (string, error) {
	key := CurrentKey(SessionKeyFrom(ctx))
	keyMu := b.pool.Lock(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	b.rw.RLock()
	ag := b.agent
	b.rw.RUnlock()
	if onDelta != nil {
		ag = ag.WithDelta(onDelta) // 流式增量只发给本次请求的订阅者
	}

	sess := b.pool.Get(ctx, key)
	defer b.pool.Save(ctx, sess)

	if len(imgs) == 0 {
		return ag.Ask(ctx, sess, text)
	}
	// 多模态：先注入仅含图片的用户消息，再走文本 Ask。
	// 框架的 Ask 只收字符串，且它失败时会回滚到自己的起点（不含图片消息），
	// 因此这里自行管理回滚；成功后把图片数据替换为占位文本，
	// 避免大体积 base64 常驻会话（每轮重复上传、内存膨胀）。
	preLen := len(sess.Messages())
	sess.Add(llm.Message{Role: llm.RoleUser, Content: imgs})
	answer, err := ag.Ask(ctx, sess, text)
	if err != nil {
		sess.Rollback(preLen)
		return answer, err
	}
	b.sanitizeImages(sess, preLen, len(imgs))
	return answer, nil
}

// sanitizeImages 把历史中某条消息的图片内容替换为占位文本。
func (b *Brain) sanitizeImages(sess *sessionstore.Session, idx, n int) {
	msgs := sess.Messages()
	if idx >= len(msgs) {
		return
	}
	var keep []llm.Content
	for _, c := range msgs[idx].Content {
		if c.Type != llm.ContentImage {
			keep = append(keep, c)
		}
	}
	keep = append(keep, llm.Content{Type: llm.ContentText,
		Text: fmt.Sprintf("[用户发送了 %d 张图片，内容已于本轮处理]", n)})
	msgs[idx].Content = keep
	sess.ReplaceHistory(msgs, sessionstore.OffloadedChunk{
		ID:       "img-sanitized-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		StartIdx: idx, EndIdx: idx + 1,
	})
}

// Reset 清除指定会话的上下文：丢弃内存与持久层历史，下次对话新开会话。
func (b *Brain) Reset(key string) {
	b.pool.Reset(context.Background(), CurrentKey(key))
}

// SetAgent 原子替换内部 Agent（热更新模型/供应商时使用）。
// 等待所有会话中进行中的请求完成后生效。
func (b *Brain) SetAgent(ag *vxagent.Agent) {
	b.rw.Lock()
	defer b.rw.Unlock()
	b.agent = ag
}

// Pool 返回会话池（管理后台会话列表/清除、热更新重建 Agent 时使用）。
func (b *Brain) Pool() *SessionPool { return b.pool }
