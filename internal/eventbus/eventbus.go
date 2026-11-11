// Package eventbus 是会话事件的广播总线：让「一轮对话」成为内核里的一个 turn
// 实体，而不是某条 HTTP 连接上的私有字节流。
//
// 为什么需要它：对话历史本来就在共享存储里（SessionPool / ACP 投影），
// 桌面端与手机端连的是同一个内核——但 SSE 帧只写给发起请求的那条连接，
// 别的客户端没有通道能收到，于是「移动端问完、桌面端看不见」。
// 本包提供按会话 key 路由的订阅通道，让两端都变成订阅者。
//
// 三条设计约束：
//  1. 非阻塞发布：发布方（对话主链路）绝不因订阅者慢而阻塞——缓冲满即丢，
//     丢的那部分靠 seq 缺口检测触发 resync 全量重拉兜底。
//  2. 有界重放：每个会话保留有限条环形缓冲，断线重连带 since 回来补发；
//     补不齐就明说「请全量重拉」，不假装完整。
//  3. 会话隔离：不同会话 key 互不串扰，事件只发给订阅了该 key 的连接。
package eventbus

import (
	"sync"
	"time"
)

// 事件类型。与前端 web/src/events.js 的帧解析一一对应。
const (
	// TypeHello 是连接建立后的第一帧：告诉客户端当前 seq 与会话 key，
	// 客户端据此校准「我从哪一条之后开始收」。
	TypeHello = "hello"
	// TypeResync 表示缺口过大、无法重放，客户端应全量重拉历史。
	TypeResync = "resync"
	// TypeUserMessage 是某端发来的用户消息（让另一端也能显示这条气泡）。
	TypeUserMessage = "user.message"
	// TypeTurnQueued 是提问进入队列（另一端据此显示「手机端排队中」）。
	TypeTurnQueued = "turn.queued"
	// TypeTurnStart 是一轮开始。
	TypeTurnStart = "turn.start"
	// TypeDelta 是回答文本增量。
	TypeDelta = "delta"
	// TypeThinking 是思考内容增量。
	TypeThinking = "thinking"
	// TypeStep 是过程事件（轮次 / 工具调用）。
	TypeStep = "step"
	// TypePlan 是计划模式产出的计划清单。
	TypePlan = "plan"
	// TypePaused 表示这一轮被用户暂停（部分回答已保存可继续）。
	TypePaused = "paused"
	// TypeTurnEnd 是一轮结束（含 token 汇总）。
	TypeTurnEnd = "turn.end"
	// TypeTurnError 是一轮失败。
	TypeTurnError = "turn.error"
	// TypeTurnCanceled 是排队中的那一轮被取消。
	TypeTurnCanceled = "turn.canceled"
	// TypeStatus 是给用户的说明性状态（如「正在等另一端的问题跑完」）。
	// 它不携带内容增量，只是让人知道当前在等什么。
	TypeStatus = "status"
)

const (
	// defaultBuffer 是每个会话保留的事件条数上限（环形缓冲）。
	// 够覆盖「另一端断网几分钟」的重放需求，又不会让内存无界增长。
	defaultBuffer = 200
	// defaultQueue 是单个订阅连接的事件缓冲：慢客户端积压到上限即丢事件，
	// 丢了由客户端的 seq 缺口检测发现并触发 resync。
	defaultQueue = 256
)

// Event 是一条会话事件。Data 是具体载荷（形状随 Type 而定，见各发布点）。
type Event struct {
	// Seq 是该会话内单调递增的序号，从 1 开始。客户端据此去重与检测缺口。
	Seq int64 `json:"seq"`
	// Type 是事件类型（见上方常量）。
	Type string `json:"type"`
	// TurnID 是所属轮次；跨轮的 user.message 为空。
	TurnID string `json:"turnId,omitempty"`
	// ClientID 是发起端的实例标识：每次页面加载随机生成（不跨标签页共享），
	// 用来区分「这条是哪个客户端发的」。前端据此过滤掉自己发的消息，
	// 避免乐观气泡与广播气泡重复。空 = 内核产生（如心跳、计划）。
	ClientID string `json:"clientId,omitempty"`
	// Device 是发起端的设备标签（桌面端/手机端…），仅用于展示：
	// 另一端看到消息时标「来自手机端」。空 = 未知来源。
	Device string `json:"device,omitempty"`
	// Session 是会话 key（web / acp:<threadID>）。
	Session string `json:"session"`
	// Data 是载荷。
	Data any `json:"data,omitempty"`
	// At 是事件产生时刻。
	At time.Time `json:"at"`
}

// Origin 是一次发布的来源标识：谁发的、从哪台设备发的。
type Origin struct {
	// ClientID 是客户端实例标识（每次页面加载随机生成）。
	ClientID string
	// Device 是设备标签（桌面端/手机端…），仅用于展示。
	Device string
}

// subscriber 是一条已鉴权的订阅连接。
type subscriber struct {
	ch        chan Event
	done      chan struct{}
	closeOnce sync.Once
}

func (s *subscriber) close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// session 是单个会话的广播状态：环形缓冲 + 订阅者集合。
type session struct {
	mu    sync.Mutex
	seq   int64
	buf   []Event // 环形缓冲（slice + 游标实现）
	start int
	size  int
	subs  map[*subscriber]struct{}
}

// Bus 是事件总线：按会话 key 路由事件。
type Bus struct {
	mu       sync.Mutex
	sessions map[string]*session
	buffer   int
	queue    int
}

// New 创建事件总线。buffer 为每会话重放条数，queue 为单连接缓冲；
// 传 0 用默认值。
func New(buffer, queue int) *Bus {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	if queue <= 0 {
		queue = defaultQueue
	}
	return &Bus{sessions: map[string]*session{}, buffer: buffer, queue: queue}
}

// Publish 发布一条事件到指定会话。返回该事件最终的 seq。
//
// 绝不阻塞：订阅者缓冲满即丢这条事件（慢客户端会通过 seq 缺口发现）。
func (b *Bus) Publish(sessionKey string, typ, turnID string, origin Origin, data any) int64 {
	if b == nil || sessionKey == "" {
		return 0
	}
	s := b.sessionOf(sessionKey)
	ev := Event{
		Type: typ, TurnID: turnID, ClientID: origin.ClientID, Device: origin.Device,
		Session: sessionKey, Data: data, At: time.Now(),
	}

	s.mu.Lock()
	s.seq++
	ev.Seq = s.seq
	s.pushLocked(ev)
	subs := make([]*subscriber, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		case <-sub.done:
		default:
			// 该连接积压满了：跳过这条，不让一条慢连接拖住全局派发。
		}
	}
	return ev.Seq
}

// sessionOf 取出（没有就建）某个会话的广播状态。
func (b *Bus) sessionOf(key string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[key]
	if !ok {
		s = &session{subs: map[*subscriber]struct{}{}, buf: make([]Event, b.buffer)}
		b.sessions[key] = s
	}
	return s
}

// pushLocked 把事件写进环形缓冲（调用方持 s.mu）。
func (s *session) pushLocked(ev Event) {
	if s.size < len(s.buf) {
		idx := (s.start + s.size) % len(s.buf)
		s.buf[idx] = ev
		s.size++
		return
	}
	// 满了：覆盖最老的一条（游标前移）。
	s.buf[s.start] = ev
	s.start = (s.start + 1) % len(s.buf)
}

// Subscriber 是一条订阅。
type Subscriber struct {
	once    sync.Once
	out     chan Event
	bus     *Bus
	s       *session
	sub     *subscriber
	replay  []Event
	current int64
}

// Subscribe 订阅某个会话的事件，返回订阅句柄、补发条数与是否需要全量重拉。
//
// since 为客户端已收到的最后一条 seq；服务端会把缓冲区里 after(since) 的
// 事件先补发一遍再转入实时推送。若客户端落后到缓冲之外（补不齐），
// needResync 为真，客户端应改走全量重拉而不是假装连续。
func (b *Bus) Subscribe(sessionKey string, since int64) (sub *Subscriber, backlog int, needResync bool) {
	if b == nil || sessionKey == "" {
		return &Subscriber{}, 0, false
	}
	s := b.sessionOf(sessionKey)
	subr := &subscriber{ch: make(chan Event, b.queue), done: make(chan struct{})}

	s.mu.Lock()
	// 计算缓冲里最老一条的 seq：缺口判断的基准。
	var oldest int64
	if s.size > 0 {
		oldest = s.buf[s.start].Seq
	}
	if since > 0 && oldest > 0 && since+1 < oldest {
		// 客户端落后到缓冲之外：补不齐，明说需要全量重拉。
		needResync = true
	}
	// 收集补发事件。
	var replay []Event
	for i := 0; i < s.size; i++ {
		ev := s.buf[(s.start+i)%len(s.buf)]
		if ev.Seq > since {
			replay = append(replay, ev)
		}
	}
	s.subs[subr] = struct{}{}
	cur := s.seq
	s.mu.Unlock()

	backlog = len(replay)
	return &Subscriber{bus: b, s: s, sub: subr, replay: replay, current: cur}, backlog, needResync
}

// Events 返回事件通道。补发事件先于实时事件送达，客户端读到的顺序
// 与实际发生顺序一致。
//
// **同一个订阅只应调一次**：这里每次调用都会新建一条转发 goroutine 与通道，
// 反复调等于每调一次就丢掉上一条通道里的事件（调用方多半写成
// `for { ev := <-sub.Events() }`，一调就丢）。所以通道建一次并缓存起来。
func (s *Subscriber) Events() <-chan Event {
	s.once.Do(func() {
		out := make(chan Event, cap(s.sub.ch)+len(s.replay))
		s.out = out
		go func() {
			defer close(out)
			for _, ev := range s.replay {
				select {
				case out <- ev:
				case <-s.sub.done:
					return
				}
			}
			for {
				select {
				case ev, ok := <-s.sub.ch:
					if !ok {
						return
					}
					select {
					case out <- ev:
					case <-s.sub.done:
						return
					}
				case <-s.sub.done:
					return
				}
			}
		}()
	})
	return s.out
}

// Current 返回订阅建立时的最新 seq（用于 hello 帧）。
func (s *Subscriber) Current() int64 { return s.current }

// Close 取消订阅（断开连接时调用）。
func (s *Subscriber) Close() {
	if s == nil || s.sub == nil {
		return
	}
	s.s.mu.Lock()
	delete(s.s.subs, s.sub)
	s.s.mu.Unlock()
	s.sub.close()
}

// CurrentSeq 返回该会话最近一次发布的事件序号（没有则 0）。
//
// 提交接口把它回给前端当订阅游标：前端拿这个 seq 去订阅，正好从下一条
// 开始收，不会漏也不会重（契约 docs/EVENTS-API.md §2.1）。
func (b *Bus) CurrentSeq(sessionKey string) int64 {
	if b == nil || sessionKey == "" {
		return 0
	}
	s := b.sessionOf(sessionKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// ActiveCount 返回当前该会话的订阅连接数（诊断用）。
func (b *Bus) ActiveCount(sessionKey string) int {
	s := b.sessionOf(sessionKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}
