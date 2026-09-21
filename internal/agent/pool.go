// pool.go 实现多会话池（SessionPool）：按 key 持有各自独立的对话会话，
// key 约定 web（网页）/ repl（终端）/ ding-<chatID> / feishu-<chatID>（各通道聊天）/ task（定时任务）。
// 会话经 sessionstore.Store（JSON 文件）持久化，重启恢复；不同 key 的会话互不交叉，
// 同一会话内的请求由 per-key 锁串行，不同会话可并行对话。
package agent

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
)

// 会话 ID 前缀：池内会话 ID 固定为 prefix+key，保证 JSON 存储按 key 稳定落盘与恢复。
const sessionIDPrefix = "friday-"

// 常用会话 key。通道会话为 <通道名>-<chatID>（如 ding-CIDxxx），由 Dispatch 统一拼装。
const (
	SessionWeb  = "web"
	SessionRepl = "repl"
	SessionTask = "task"
)

type sessionKey struct{}

// WithSessionKey 返回挂上了会话 key 的 ctx；Brain.Ask 据此选择会话。
func WithSessionKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, sessionKey{}, key)
}

// SessionKeyFrom 取出 ctx 中的会话 key；未设置时默认网页会话。
func SessionKeyFrom(ctx context.Context) string {
	if key, ok := ctx.Value(sessionKey{}).(string); ok && key != "" {
		return key
	}
	return SessionWeb
}

// SessionInfo 是一个会话的概要（管理后台会话列表用）。
type SessionInfo struct {
	Key       string    `json:"key"`
	ID        string    `json:"id"`
	Messages  int       `json:"messages"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SessionPool 按 key 持有会话；store 非 nil 时每次对话后落盘。
type SessionPool struct {
	mu     sync.Mutex
	sess   map[string]*sessionstore.Session
	locks  map[string]*sync.Mutex
	active map[string]time.Time // key → 最后活跃时刻（List 展示，避免触碰会话内部锁）
	store  sessionstore.Store
}

// NewSessionPool 创建会话池。store 可为 nil（纯内存，重启失忆）。
func NewSessionPool(store sessionstore.Store) *SessionPool {
	return &SessionPool{
		sess:   make(map[string]*sessionstore.Session),
		locks:  make(map[string]*sync.Mutex),
		active: make(map[string]time.Time),
		store:  store,
	}
}

// Lock 返回指定 key 的互斥锁（锁实例按 key 惰性创建、长期复用）。
// 调用方 Lock() / Unlock() 自行配对；不同 key 互不阻塞。
func (p *SessionPool) Lock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	mu, ok := p.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		p.locks[key] = mu
	}
	return mu
}

// Get 返回 key 对应的会话：内存没有则尝试从持久层恢复，再没有就新建。
// 恢复/新建在调用方的 per-key 锁内进行即可（Get 本身也加了池级锁防并发创建）。
func (p *SessionPool) Get(ctx context.Context, key string) *sessionstore.Session {
	p.mu.Lock()
	if s, ok := p.sess[key]; ok {
		p.mu.Unlock()
		return s
	}
	p.mu.Unlock()

	// 先查持久层：重启后首个请求恢复历史。
	if p.store != nil {
		if s, err := p.store.Load(ctx, sessionIDPrefix+key); err == nil && s != nil {
			p.mu.Lock()
			p.sess[key] = s
			p.mu.Unlock()
			return s
		}
	}

	s := sessionstore.NewSession("robot")
	s.ID = sessionIDPrefix + key
	p.mu.Lock()
	p.sess[key] = s
	p.mu.Unlock()
	return s
}

// Save 把会话快照写入持久层（Ask 结束后调用；store 未启用时为空操作）。
// 同时刷新该会话的最后活跃时刻。
func (p *SessionPool) Save(ctx context.Context, s *sessionstore.Session) {
	if s == nil {
		return
	}
	p.mu.Lock()
	p.active[sessionKeyOf(s)] = time.Now()
	p.mu.Unlock()
	if p.store == nil {
		return
	}
	if err := p.store.Save(ctx, s); err != nil {
		log.Printf("[会话] 持久化失败: %v", err)
	}
}

// sessionKeyOf 从会话 ID 还原池内 key（ID 固定为 prefix+key）。
func sessionKeyOf(s *sessionstore.Session) string {
	return strings.TrimPrefix(s.ID, sessionIDPrefix)
}

// History 返回指定会话的消息列表（只读，不创建会话）：
// 内存有则取内存版，否则从持久层加载；都没有返回 nil。
// 供管理后台回显历史对话——打开页面不应产生 LLM 调用，
// 也不应因为查询历史而把空会话写进池里。
func (p *SessionPool) History(ctx context.Context, key string) []llm.Message {
	key = CurrentKey(key)
	p.mu.Lock()
	s, ok := p.sess[key]
	p.mu.Unlock()
	if ok {
		return s.Messages()
	}
	if p.store != nil {
		if loaded, err := p.store.Load(ctx, sessionIDPrefix+key); err == nil && loaded != nil {
			return loaded.Messages()
		}
	}
	return nil
}

// Reset 清除指定会话：丢弃内存与持久层中的历史，下次对话从新会话开始。
func (p *SessionPool) Reset(ctx context.Context, key string) {
	p.mu.Lock()
	delete(p.sess, key)
	delete(p.active, key)
	p.mu.Unlock()
	if p.store != nil {
		if err := p.store.Delete(ctx, sessionIDPrefix+key); err != nil {
			log.Printf("[会话] 清除持久会话 %s 失败: %v", key, err)
		}
	}
}

// List 返回内存中全部会话概要（按 key 排序）。
func (p *SessionPool) List() []SessionInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SessionInfo, 0, len(p.sess))
	for key, s := range p.sess {
		out = append(out, SessionInfo{
			Key:       key,
			ID:        s.ID,
			Messages:  s.MessageCount(),
			UpdatedAt: p.active[key],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// CurrentKey 校验/规范化会话 key：允许字母数字、-、_、@、.，其余替换为 _，
// 防止怪异 key 进持久层文件名/轨迹。空串返回默认网页 key。
func CurrentKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return SessionWeb
	}
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '@', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
