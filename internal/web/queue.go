// queue.go 是主会话的 FIFO 队列：同一时刻只跑一轮，后到的排队等着。
//
// 为什么需要它：以前并发提问靠 Brain 里的 per-key 互斥锁**静默阻塞**——
// 移动端在桌面端生成时提问，连接建好了却一帧不发，界面一直「思考中」，
// 用户既不知道在等什么也等不到结果。改成显式队列后，等待变成看得见的
// 「排队中 · 第 N 位」，问题也不会丢。
//
// 为什么队列在前端之外还要在后端：主会话（生活模式）只有一条上下文，
// 且没有前端队列可借（工作区那边有 enqueue/pumpQueue，主会话没有），
// 所以排队这件事只能由内核做。
//
// 队列与 Brain 的锁是两层，不冲突：dispatcher 本身已经串行，但终端 REPL、
// 钉钉、飞书这些入口也走 Brain、不走 dispatcher——那把锁仍要留着兜底。
package web

import (
	"github.com/capyflow/friday/internal/eventbus"
	"sync"
	"time"
)

const (
	// queueMax 是每个会话的排队上限：再多就别排了，让用户先等前面的跑完。
	queueMax = 20
	// queueTTL 是排队项的最长等待：超过十分钟还没轮到就丢掉——半小时前
	// 问的一句话突然开跑，对谁都是惊吓。
	queueTTL = 10 * time.Minute
)

// queuedTurn 是排队中的一轮。
type queuedTurn struct {
	// id 是轮次标识（停止时按它精确定位，见 Chat.HandleStop）。
	id string
	// question 是这一轮的问题文本。
	question string
	// origin 是发起端标识（谁发的、从哪台设备）。
	origin eventbus.Origin
	// enqueuedAt 是入队时刻（超时判定用）。
	enqueuedAt time.Time
	// partial 非空表示这是「继续被暂停的对话」：原问题已生成过一部分，
	// 继续时要把它重新注入会话（见 runTurn）。
	partial string
	// skill 是暂停现场记录的本轮技能：技能限定了工具面与模型，
	// 继续时换一套会静默改变行为，所以必须沿用。
	skill string
	// run 是这一轮真正开跑后登记的执行句柄（跑之前为 nil）。
	run *chatRun
}

// turnQueue 是单个会话 key 的 FIFO 队列。
type turnQueue struct {
	mu    sync.Mutex
	items []*queuedTurn
}

// push 入队并返回位次（1 起）。
func (q *turnQueue) push(t *queuedTurn) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, t)
	return len(q.items)
}

// peek 取队首（不弹出）。
func (q *turnQueue) peek() *queuedTurn {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	return q.items[0]
}

// pop 弹出队首。
func (q *turnQueue) pop() *queuedTurn {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	t := q.items[0]
	q.items = q.items[1:]
	return t
}

// remove 按轮次标识移除（取消排队项时用）。返回被移除的那一项。
func (q *turnQueue) remove(id string) *queuedTurn {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.items {
		if t.id == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return t
		}
	}
	return nil
}

// find 按轮次标识查找排队项。
func (q *turnQueue) find(id string) *queuedTurn {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.items {
		if t.id == id {
			return t
		}
	}
	return nil
}

// len 返回队列长度。
func (q *turnQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// snapshot 返回队列快照（给前端渲染排队位次用）。
func (q *turnQueue) snapshot() []queueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queueItem, 0, len(q.items))
	for i, t := range q.items {
		out = append(out, queueItem{
			TurnID: t.id, Position: i + 1, ClientID: t.origin.ClientID,
			Device: t.origin.Device, Text: t.question, CreatedAt: t.enqueuedAt,
		})
	}
	return out
}

// queueItem 是队列快照里的一项。
type queueItem struct {
	TurnID    string    `json:"turnId"`
	Position  int       `json:"position"`
	ClientID  string    `json:"clientId,omitempty"`
	Device    string    `json:"device,omitempty"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}
