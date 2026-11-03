// oauth.go 把框架的通用 MCP OAuth 引擎（tools/mcp/auth）接到 Friday 的 MCP
// 管理器上：令牌按服务器名落盘（0600）、浏览器授权流程的编排、401 挑战的记录
// 与刷新重试。协议细节（元数据发现、动态注册、PKCE、loopback 回调）全在框架里，
// 这一层只回答"哪个服务器的令牌、存在哪、什么时候要用户去授权"。
//
// 传输层通过 TokenProvider 取 Authorization 头并在 401 时回报，因此 stdio 服务器
// 与静态 header 服务器完全不受影响；OAuth 只是一种可配置的远程鉴权方式。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	vxauth "github.com/capyflow/vortexagent/tools/mcp/auth"
)

// 鉴权方式（config mcp.servers[].auth）。空值等同 AuthHeader：只用静态请求头。
const (
	// AuthHeader 沿用 headers 里的静态鉴权头（旧行为，也是默认）。
	AuthHeader = "header"
	// AuthOAuth 走 MCP 规范的 OAuth 2.1：发现 + 动态注册 + PKCE + loopback 回调。
	AuthOAuth = "oauth"
)

// 授权状态（设置页据此展示徽标与按钮）。
const (
	AuthStateRequired    = "required"    // 需要（重新）浏览器授权
	AuthStateAuthorizing = "authorizing" // 授权页已打开，等回调
	AuthStateAuthorized  = "authorized"  // 已有可用令牌
	AuthStateError       = "error"       // 上一次授权流程失败，Message 带原因
)

const (
	// oauthCallbackTimeout 是一次浏览器授权的等待上限：超时即关闭 loopback，
	// 状态回落到 required，用户可重新点授权。
	oauthCallbackTimeout = 5 * time.Minute
	// tokenSkew 是"提前刷新"的余量：距过期不足这个值就先换一次，避免请求正好
	// 卡在过期边界上拿到 401。
	tokenSkew = 60 * time.Second
	// oauthClientName 是注册到服务商的客户端名（显示在服务商的授权页上）。
	oauthClientName = "Friday"
)

// ErrMCPAuthRequired 表示远程 MCP 服务器要求授权且本层无法自动恢复，
// 需要用户在设置页点「授权」走一次浏览器流程。
var ErrMCPAuthRequired = errors.New("MCP 需要授权")

// TokenProvider 是 HTTP 传输需要的令牌接口（由 OAuth 按服务器名派发）。
// 两个方法都只回报"能不能带上 Authorization"，不抛错误：传输层的失败形状
// 由调用方统一处理，细节记在 OAuth 状态里供设置页展示。
type TokenProvider interface {
	// Authorization 返回当前可用令牌的头值；需要用户授权时 ok=false。
	Authorization(ctx context.Context) (value string, ok bool)
	// Unauthorized 回报一次 401（challenge 是 WWW-Authenticate 原文），
	// 记录挑战并尝试刷新；刷新成功时 ok=true 并返回新头值。
	Unauthorized(ctx context.Context, challenge string) (value string, ok bool)
}

// OAuth 管理若干台远程 MCP 服务器的授权会话。零值不可用，用 NewOAuth 创建。
type OAuth struct {
	mu     sync.Mutex
	path   string // 会话文件；空表示只存内存（测试）
	hc     *http.Client
	store  map[string]vxauth.Session // 服务器名 → 会话（含 client 注册与令牌）
	states map[string]*oauthState    // 服务器名 → 挑战记录与流程状态
	loaded bool

	// onAuthorized 在授权成功后回调（管理器用它自动重连，工具随即出现）。
	onAuthorized func(name string)
}

// oauthState 是一台服务器的授权侧状态。
type oauthState struct {
	authMode    string   // config 声明的鉴权方式
	resourceURL string   // config 声明的远程地址（会话与之不符则视为未授权）
	metadataURL string   // 401 challenge 给的 resource_metadata 地址
	scopes      []string // 401 challenge 给的 scope
	state       string   // AuthState*
	message     string   // 失败原因（不含令牌）
	pending     bool     // 有进行中的浏览器授权
	authURL     string   // 进行中流程的授权地址（供设置页「复制链接」兜底）
	gen         uint64   // 代数：改配置/移除/开始新流程时递增
	pendingGen  uint64   // 进行中流程所属代数
	cancel      func()   // 取消进行中的授权等待（改配置或移除时立即关闭 loopback）
}

// NewOAuth 创建授权服务；path 为会话文件（相对路径按当前工作目录解析，
// 空则只存内存）。文件写成 0600：里面含 client_secret 与 refresh_token。
func NewOAuth(path string) *OAuth {
	return NewOAuthWithClient(path, nil)
}

// NewOAuthWithClient 允许注入 HTTP 客户端（测试用；nil 用带超时的默认客户端）。
func NewOAuthWithClient(path string, hc *http.Client) *OAuth {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuth{path: path, hc: hc, store: map[string]vxauth.Session{}, states: map[string]*oauthState{}}
}

// SetAuthorizedHandler 设置授权成功后的回调（可为 nil）。
func (o *OAuth) SetAuthorizedHandler(fn func(name string)) {
	o.mu.Lock()
	o.onAuthorized = fn
	o.mu.Unlock()
}

// load 首次访问时把会话文件读进内存；文件不存在视为空，读不动则记日志级错误，
// 内存态照常可用（用户可重新授权覆盖）。
func (o *OAuth) load() {
	if o.loaded {
		return
	}
	o.loaded = true
	if strings.TrimSpace(o.path) == "" {
		return
	}
	data, err := os.ReadFile(o.path)
	if err != nil || len(data) == 0 {
		return
	}
	var raw map[string]vxauth.Session
	if json.Unmarshal(data, &raw) == nil {
		for k, v := range raw {
			if _, ok := o.store[k]; !ok {
				o.store[k] = v
			}
		}
	}
}

// state 取（必要时创建）一台服务器的授权状态。调用方需持锁。
func (o *OAuth) state(name string) *oauthState {
	if st, ok := o.states[name]; ok {
		return st
	}
	st := &oauthState{state: AuthStateRequired}
	st.state = seedState(o.store[name])
	o.states[name] = st
	return st
}

// seedState 用令牌库里的会话推断初始状态。内核重启后 states 是空的、只有 store 从
// 文件恢复，不补这一下，明明令牌有效、连接正常的服务器会一直显示「待授权」，把人
// 引去重走一遍浏览器授权。
func seedState(sess vxauth.Session) string {
	if sess.Token.AccessToken == "" {
		return AuthStateRequired
	}
	// 过期但有 refresh_token 也算已授权：连上时会自动换，不用用户再点一次。
	if !sess.Token.Expired(time.Now(), tokenSkew) || sess.Token.RefreshToken != "" {
		return AuthStateAuthorized
	}
	return AuthStateRequired
}

// Configure 记录一台服务器的鉴权声明（管理器 Apply 时调用）。
// 地址变了就作废进行中的授权流程：会话里的 resource 与配置不符时视为未授权。
func (o *OAuth) Configure(name, authMode, resourceURL string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load()
	st := o.state(name)
	if st.resourceURL != strings.TrimSpace(resourceURL) || st.authMode != authMode {
		st.gen++ // 定义变了，进行中的浏览器授权不再有意义
		if st.cancel != nil {
			st.cancel()
			st.cancel = nil
		}
		st.pending = false
		st.authURL = ""
	}
	st.authMode = authMode
	st.resourceURL = strings.TrimSpace(resourceURL)
	if authMode != AuthOAuth {
		// 切回静态头：清掉授权侧的中间状态，避免设置页继续显示「待授权」。
		st.state = ""
		st.message = ""
		return
	}
	if st.state == "" || st.state == AuthStateError {
		st.state = AuthStateRequired
	}
	// 地址是令牌的绑定对象（RFC 8707）：配置里的地址换过，旧会话不再算数。
	// 与 sessionFor 的判定保持一致，别让设置页挂着「已授权」而实际连不上。
	if sess, ok := o.store[name]; ok && st.state == AuthStateAuthorized &&
		st.resourceURL != "" && sess.Resource != "" && sess.Resource != st.resourceURL {
		st.state = AuthStateRequired
		st.message = "远程地址已变更，需要重新授权"
	}
}

// Forget 删除一台服务器的授权状态与会话（从配置里移除时调用）。
// 进行中的浏览器授权随即取消，loopback 立刻关闭。
func (o *OAuth) Forget(name string) {
	o.mu.Lock()
	if st, ok := o.states[name]; ok && st.cancel != nil {
		st.cancel()
	}
	delete(o.states, name)
	_, had := o.store[name]
	delete(o.store, name)
	path := o.path
	o.mu.Unlock()
	if had {
		o.persist(path)
	}
}

// Get 实现框架的 auth.Store：按服务器名取会话（不含资源校验，供导入/导出与测试）。
func (o *OAuth) Get(ctx context.Context, name string) (*vxauth.Session, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load()
	sess, ok := o.store[name]
	if !ok {
		return nil, vxauth.ErrNotFound
	}
	cp := sess
	return &cp, nil
}

// Put 实现 auth.Store：写入会话并落盘（0600）。
func (o *OAuth) Put(ctx context.Context, name string, sess *vxauth.Session) error {
	if sess == nil {
		return errors.New("MCP 授权会话为空")
	}
	o.mu.Lock()
	o.load()
	o.store[name] = *sess
	path := o.path
	o.mu.Unlock()
	o.persist(path)
	return nil
}

// Delete 实现 auth.Store：只删本地会话，不向服务商撤销。
func (o *OAuth) Delete(ctx context.Context, name string) error {
	o.mu.Lock()
	_, had := o.store[name]
	delete(o.store, name)
	path := o.path
	o.mu.Unlock()
	if had {
		o.persist(path)
	}
	return nil
}

// StateOf 返回一台服务器当前的授权状态与说明。
func (o *OAuth) StateOf(name string) (state, message string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load()
	st := o.state(name)
	if st.authMode != AuthOAuth {
		return "", ""
	}
	return st.state, st.message
}

// AuthURL 返回进行中浏览器授权的授权地址；不在等待回调时为空串。
// 设置页用它渲染「复制授权链接」：自动打开被拦、页面刷新后重新进入都能手动完成。
func (o *OAuth) AuthURL(name string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.state(name)
	if !st.pending {
		return ""
	}
	return st.authURL
}

// Provider 返回绑定到某台服务器的令牌提供者；非 OAuth 服务器返回 nil
// （传输层据此决定要不要注入头与处理 401）。
func (o *OAuth) Provider(name string) TokenProvider {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.load()
	if o.state(name).authMode != AuthOAuth {
		return nil
	}
	return oauthProvider{o: o, name: name}
}

// Authorize 起一次浏览器授权：发现 → 注册 → 生成授权 URL，后台等回调。
// 返回的 URL 由调用方交给系统浏览器打开（后台不猜操作系统怎么开）。
// 同一服务器重复调用会复用仍在进行中的流程，避免并发起两个 loopback。
func (o *OAuth) Authorize(ctx context.Context, name string) (string, error) {
	o.mu.Lock()
	o.load()
	st := o.state(name)
	if st.authMode != AuthOAuth {
		o.mu.Unlock()
		return "", fmt.Errorf("服务器 %s 未启用 OAuth 鉴权", name)
	}
	if strings.TrimSpace(st.resourceURL) == "" {
		o.mu.Unlock()
		return "", fmt.Errorf("服务器 %s 未配置远程地址", name)
	}
	if st.pending {
		o.mu.Unlock()
		return "", fmt.Errorf("服务器 %s 的授权正在进行中，请在浏览器里完成或稍后重试", name)
	}
	resource, metadataURL := st.resourceURL, st.metadataURL
	scopes := append([]string(nil), st.scopes...)
	st.gen++
	st.pendingGen = st.gen
	gen := st.pendingGen
	st.pending = true
	st.state = AuthStateAuthorizing
	st.message = ""
	o.mu.Unlock()

	// fail 只在"还是这一次流程"时改状态，避免改配置后覆盖新流程的结果。
	fail := func(err error) error {
		o.mu.Lock()
		if cur, ok := o.states[name]; ok && cur.pending && cur.pendingGen == gen {
			cur.pending = false
			cur.cancel = nil
			cur.state = AuthStateError
			cur.message = authUserMessage(err)
		}
		o.mu.Unlock()
		return err
	}

	flow, err := vxauth.Begin(ctx, vxauth.FlowOptions{
		ResourceURL:         resource,
		ResourceMetadataURL: metadataURL,
		ClientName:          oauthClientName,
		Scopes:              scopes,
		HTTPClient:          o.hc,
	})
	if err != nil {
		return "", fail(err)
	}
	// 发现/注册用的是调用方（HTTP 请求）的 ctx；等浏览器回调另起一条有超时的
	// ctx，并把它交给状态，改配置或移除服务器时能立刻关掉 loopback。
	authURL := flow.URL()
	waitCtx, cancel := context.WithTimeout(context.Background(), oauthCallbackTimeout)
	o.mu.Lock()
	if cur, ok := o.states[name]; ok && cur.pending && cur.pendingGen == gen {
		cur.cancel = cancel
		cur.authURL = authURL // 页面刷新或没自动打开浏览器时，设置页仍能拿到链接
	} else {
		o.mu.Unlock()
		cancel()
		flow.Close()
		return "", fmt.Errorf("服务器 %s 的配置在授权发起期间已变更，请重试", name)
	}
	o.mu.Unlock()

	go o.await(name, gen, flow, waitCtx, cancel)
	return authURL, nil
}

// await 后台等浏览器回调：成功则落盘令牌并按需重连，超时/失败回到可重试的状态。
func (o *OAuth) await(name string, gen uint64, flow *vxauth.Flow, ctx context.Context, cancel func()) {
	defer cancel()
	defer flow.Close()

	sess, err := flow.Wait(ctx)
	o.mu.Lock()
	st, ok := o.states[name]
	if !ok || !st.pending || st.pendingGen != gen {
		o.mu.Unlock()
		return // 改配置或移除服务器，这次授权结果作废（loopback 由 defer 关闭）
	}
	st.pending = false
	st.cancel = nil // 这条流程结束，取消句柄随之交还（改配置时不再有误取消）
	st.authURL = "" // 授权地址只在等待回调期间有效（含超时与失败）
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			st.state = AuthStateRequired
			st.message = "授权超时未完成"
		} else {
			st.state = AuthStateError
			st.message = authUserMessage(err)
		}
		path := o.path
		o.mu.Unlock()
		o.persist(path)
		return
	}
	o.store[name] = *sess
	st.state = AuthStateAuthorized
	st.message = ""
	path := o.path
	hook := o.onAuthorized
	o.mu.Unlock()
	o.persist(path)
	if hook != nil {
		hook(name)
	}
}

// Disconnect 撤销并删除一台服务器的会话：先尽量 revoke（服务商侧令牌失效），
// 再清本地令牌，最后把状态打回 required。令牌已经没了也当作成功。
func (o *OAuth) Disconnect(ctx context.Context, name string) error {
	o.mu.Lock()
	sess, ok := o.store[name]
	delete(o.store, name)
	st := o.state(name)
	st.state = AuthStateRequired
	st.message = ""
	path := o.path
	o.mu.Unlock()

	var err error
	if ok {
		err = vxauth.Revoke(ctx, o.hc, &sess, sess.Token.RefreshToken, "refresh_token")
		if err == nil {
			err = vxauth.Revoke(ctx, o.hc, &sess, sess.Token.AccessToken, "access_token")
		}
	}
	o.persist(path)
	return err
}

// persist 原子写会话文件（0600）。path 为空表示内存模式，直接跳过。
// 写失败不改变内存态：Friday 仍可用当前令牌工作，只是重启后要重新授权。
func (o *OAuth) persist(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	o.mu.Lock()
	raw := make(map[string]vxauth.Session, len(o.store))
	for k, v := range o.store {
		raw[k] = v
	}
	o.mu.Unlock()

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Chmod(tmp, 0o600) // 文件已存在时 WriteFile 不改进权限，显式兜一次
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// sessionFor 取某台服务器仍然有效的会话：配置里的地址与授权时绑定的
// resource 不符时视为没有会话（RFC 8707 下令牌不可跨资源复用）。
// 调用方需持锁。
func (o *OAuth) sessionFor(name string) *vxauth.Session {
	st := o.state(name)
	sess, ok := o.store[name]
	if !ok || sess.Token.AccessToken == "" {
		return nil
	}
	if st.resourceURL != "" && sess.Resource != st.resourceURL {
		// 令牌是按资源发的（RFC 8707），换了地址等于换了资源：显式回到待授权，
		// 否则设置页还挂着「已授权」，用户只看到连不上，无从下手。
		if st.authMode == AuthOAuth && !st.pending {
			st.state = AuthStateRequired
			st.message = "远程地址已变更，需要重新授权"
		}
		return nil
	}
	cp := sess
	return &cp
}

// oauthProvider 把 OAuth 服务绑定到一台服务器，供 HTTP 传输调用。
type oauthProvider struct {
	o    *OAuth
	name string
}

func (p oauthProvider) Authorization(ctx context.Context) (string, bool) {
	p.o.mu.Lock()
	sess := p.o.sessionFor(p.name)
	hc := p.o.hc
	p.o.mu.Unlock()
	if sess == nil {
		return "", false
	}
	next, err := vxauth.EnsureToken(ctx, hc, sess, time.Now(), tokenSkew)
	if err != nil {
		p.o.recordFailure(p.name, err)
		return "", false
	}
	if next != sess {
		p.o.save(p.name, *next)
	}
	return next.Token.AuthorizationHeader(), true
}

func (p oauthProvider) Unauthorized(ctx context.Context, challenge string) (string, bool) {
	p.o.recordChallenge(p.name, challenge)
	p.o.mu.Lock()
	sess := p.o.sessionFor(p.name)
	hc := p.o.hc
	p.o.mu.Unlock()
	if sess == nil {
		return "", false
	}
	tok, err := vxauth.Refresh(ctx, hc, sess)
	if err != nil {
		p.o.recordFailure(p.name, err)
		return "", false
	}
	merged := *sess
	merged.Token = *tok
	p.o.save(p.name, merged)
	return tok.AuthorizationHeader(), true
}

// recordChallenge 记下 401 的 WWW-Authenticate：resource_metadata 与 scope 是
// 下一次浏览器授权的入口，规范推荐的发现路径就靠它。
func (o *OAuth) recordChallenge(name, challenge string) {
	c, err := vxauth.ParseChallenge(challenge)
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.state(name)
	if err == nil {
		if c.ResourceMetadataURL != "" {
			st.metadataURL = c.ResourceMetadataURL
		}
		if c.Scope != "" {
			st.scopes = strings.Fields(c.Scope)
		}
	}
	if st.authMode == AuthOAuth && !st.pending {
		st.state = AuthStateRequired
	}
}

// recordFailure 把刷新失败写进状态：invalid_grant 一类（ErrReauthRequired）
// 说清"需要重新授权"，其余保留服务商原话，方便用户判断是网络还是账号问题。
func (o *OAuth) recordFailure(name string, err error) {
	o.mu.Lock()
	st := o.state(name)
	if st.authMode != AuthOAuth || st.pending {
		o.mu.Unlock()
		return
	}
	st.state = AuthStateRequired
	if errors.Is(err, vxauth.ErrReauthRequired) {
		st.message = "登录态已失效，需要重新授权"
	} else {
		st.message = authUserMessage(err)
	}
	o.mu.Unlock()
}

// save 写回一台服务器的会话并落盘。
func (o *OAuth) save(name string, sess vxauth.Session) {
	o.mu.Lock()
	st := o.state(name)
	if st.authMode != AuthOAuth {
		o.mu.Unlock()
		return
	}
	o.store[name] = sess
	if st.state != AuthStateAuthorizing {
		st.state = AuthStateAuthorized
		st.message = ""
	}
	path := o.path
	o.mu.Unlock()
	o.persist(path)
}

// authUserMessage 把框架错误转成设置页可读的一句话（不含令牌）。
func authUserMessage(err error) string {
	var re *vxauth.RegistrationError
	if errors.As(err, &re) {
		return fmt.Sprintf("服务商不支持自动注册客户端，需要手工登记并加白回调地址 %s", re.RedirectURI)
	}
	msg := strings.TrimSpace(strings.TrimPrefix(err.Error(), "mcp auth: "))
	if msg == "" {
		return "授权失败"
	}
	return msg
}
