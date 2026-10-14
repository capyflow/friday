// brain.go 定义机器人的"对话大脑"：所有入口（网页/终端/消息通道/定时任务）
// 共用同一个 Agent，按会话 key（SessionPool）持有各自独立的对话历史。
// 同一会话内的请求串行执行（上下文连续、流式不交叉），不同会话可并行对话；
// 流式增量经 Agent.WithDelta 按请求路由到各自的订阅者，互不串扰。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/config"
)

// Brain 持有 Agent 与会话池，按会话 key 串行化提问。
// rw 锁保护 Agent 的原子替换：Ask 持读锁（不同会话并行），SetAgent 持写锁
// （等待全部进行中的请求结束后生效）。
type Brain struct {
	rw    sync.RWMutex
	agent *vxagent.Agent
	pool  *SessionPool
	ctx   *ContextManager // 上下文引擎管理器（Reset 清归档、热更新重建用；可为 nil）

	customMu sync.RWMutex           // 保护 custom（自定义指令表，SetCustom 可运行期替换）
	custom   []config.CustomCommand // 自定义指令：触发词 → 提示词展开
}

// NewBrain 创建对话大脑。pool 为会话池（含持久化存储），ctx 为上下文引擎
// 管理器（均可为 nil：pool 无持久化则重启失忆，ctx 为 nil 则不治理上下文），
// 由调用方构建。
func NewBrain(ag *vxagent.Agent, pool *SessionPool, ctx *ContextManager) *Brain {
	return &Brain{agent: ag, pool: pool, ctx: ctx}
}

// CtxMgr 返回上下文引擎管理器（热更新路径重建引擎用，可为 nil）。
func (b *Brain) CtxMgr() *ContextManager { return b.ctx }

// Ask 提问并等待完整回答。会话由 ctx 中的会话 key 决定（WithSessionKey，
// 未设置时用网页会话）；onDelta 为本次请求的流式增量回调（可为 nil）；
// imgs 为随消息发送的图片内容（可为 nil），会作为一条独立的用户消息
// 紧邻问题文本之前注入会话。
func (b *Brain) Ask(ctx context.Context, text string, imgs []llm.Content, onDelta func(llm.Delta)) (answer string, err error) {
	key := CurrentKey(SessionKeyFrom(ctx))
	keyMu := b.pool.Lock(key)
	keyMu.Lock()
	defer keyMu.Unlock()

	// 自定义指令：命中触发词先展开为提示词（网页/终端/钉钉/飞书全入口生效）。
	text = b.expandCustom(text)

	b.rw.RLock()
	defer b.rw.RUnlock()
	ag := b.agent
	if onDelta != nil {
		ag = ag.WithDelta(onDelta) // 流式增量只发给本次请求的订阅者
	}

	sess := b.pool.Get(ctx, key)
	defer func() {
		if saveErr := b.pool.Save(context.WithoutCancel(ctx), sess); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("保存对话失败: %w", saveErr))
		}
	}()

	if len(imgs) == 0 {
		return ag.Ask(ctx, sess, text)
	}
	// 多模态：先注入仅含图片的用户消息，再走文本 Ask。
	// 框架的 Ask 只收字符串，且它失败时会回滚到自己的起点（不含图片消息），
	// 因此这里自行管理回滚；成功后把图片数据替换为占位文本，
	// 避免大体积 base64 常驻会话（每轮重复上传、内存膨胀）。
	preLen := len(sess.Messages())
	sess.Add(llm.Message{Role: llm.RoleUser, Content: imgs})
	answer, err = ag.Ask(ctx, sess, text)
	if err != nil {
		sess.Rollback(preLen)
		return answer, err
	}
	b.sanitizeImages(sess, preLen, len(imgs))
	return answer, nil
}

// Resume 继续被暂停的对话。ctx 取消时框架会回滚本轮（用户消息与已生成的
// 部分回答都不在会话里），这里把它们重新注入会话历史，再以继续指令发问，
// 让模型接着上次中断的地方完成任务——继续产生的内容与原问题在同一会话上下文中。
// partial 为空表示被暂停时还没有任何输出（仅注入原问题）。
func (b *Brain) Resume(ctx context.Context, key, question, partial string, onDelta func(llm.Delta)) (answer string, err error) {
	fullKey := CurrentKey(key)
	keyMu := b.pool.Lock(fullKey)
	keyMu.Lock()
	defer keyMu.Unlock()

	b.rw.RLock()
	defer b.rw.RUnlock()
	ag := b.agent
	if onDelta != nil {
		ag = ag.WithDelta(onDelta)
	}

	sess := b.pool.Get(ctx, fullKey)
	defer func() {
		if saveErr := b.pool.Save(context.WithoutCancel(ctx), sess); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("保存对话失败: %w", saveErr))
		}
	}()

	preLen := len(sess.Messages())
	sess.Add(llm.NewTextMessage(llm.RoleUser, question))
	if partial != "" {
		sess.Add(llm.NewTextMessage(llm.RoleAssistant,
			partial+"\n\n（这个回答被用户暂停，尚未完成）"))
	}
	answer, err = ag.Ask(ctx, sess, "请接着上面被暂停的回答继续完成任务，把没做完的部分做完；已经生成过的内容不要重复输出。")
	if err != nil {
		sess.Rollback(preLen)
		return answer, err
	}
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
	sess.ResetTo(msgs)
}

// Reset 清除指定会话的上下文：丢弃内存与持久层历史，下次对话新开会话；
// 已归档的早期对话与引擎运行态一并清除——会话 ID 固定为 prefix+key，
// 归档不清的话新会话会带出旧摘要区，等于没清干净。
func (b *Brain) Reset(key string) error {
	fullKey := CurrentKey(key)
	mu := b.pool.Lock(fullKey)
	mu.Lock()
	defer mu.Unlock()
	b.rw.RLock()
	defer b.rw.RUnlock()
	if err := b.pool.Reset(context.Background(), fullKey); err != nil {
		return err
	}
	if b.ctx != nil {
		return b.ctx.ResetSession(context.Background(), sessionIDPrefix+fullKey)
	}
	return nil
}

// Reconfigure 等待在途请求落盘后构建并切换 Agent；失败保留原实例。
func (b *Brain) Reconfigure(build func() (*vxagent.Agent, error)) error {
	b.rw.Lock()
	defer b.rw.Unlock()
	ag, err := build()
	if err != nil {
		return err
	}
	b.agent = ag
	return nil
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
