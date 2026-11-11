// Package web 提供网页聊天的对话与图片上传接口。页面外壳（登录、导航、各视图）
// 由 admin 包统一提供；本包只负责把对话请求接到 agent Brain 上。
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capyflow/vortexagent/llm"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/eventbus"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/trace"
)

const (
	uploadMaxSize  = 5 << 20 // 单张图片上限 5MB
	uploadMaxCount = 4       // 单条消息最多 4 张
	uploadKeepDays = 7       // 上传文件保留天数
)

var uploadNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp)$`)

// Chat 是网页聊天服务：经 Brain 与 agent 对话。网页聊天只有唯一的主会话
// web（不传 session 就是它），后台入口（心跳/计划/目标）各有自己的会话 key。
// 支持对话暂停/继续：暂停取消进行中的生成（及时止损），被中断的问答记录在
// paused 中可一键继续；暂停期间发新问题则丢弃暂停记录、开启新对话。
type Chat struct {
	brain      *agent.Brain
	uploadsDir string
	pausedFile string // 暂停现场落盘路径（空 = 仅内存，重启丢）

	planner  *plan.Planner // 计划模式（nil = 未启用）
	runner   *plan.Runner
	goalEng  *task.Engine          // 目标模式：聊天里"目标"直接建一条即时目标任务
	goalGate func() (bool, string) // 超预算时不接新目标（nil = 不做预算拦截）

	// bus 是会话事件总线：一轮对话的增量/思考/步骤/收尾都经它广播，
	// 桌面端与手机端从同一条 /api/events/stream 收（契约 docs/EVENTS-API.md）。
	// nil = 不装配（不广播，提交式接口仍可用，只是没有跨端同步）。
	bus *eventbus.Bus

	mu sync.Mutex
	// runs 是进行中的对话：会话 key → 轮次 id → 执行句柄。
	// 两级而不是一级，是因为一级会让并发请求互相覆盖——后发的顶掉先发的，
	// 先发的收尾时又把后发的登记删掉，于是「停止」可能停错一轮
	// （见 docs/MULTI-DEVICE-SYNC.md §2.3 缺陷 2）。
	runs map[string]map[string]*chatRun
	// queues 是排队中的轮次：会话 key → 队列。主会话同一时刻只跑一轮，
	// 后到的排队等着（不再像以前那样静默阻塞）。
	queues map[string]*turnQueue
	// dispatching 标记某条会话的 dispatcher 是否在跑：队列空了它就退出，
	// 有新请求再起一个——不能每个会话常驻一个 goroutine。
	dispatching map[string]bool
	paused      map[string]*PausedTurn // 会话 key → 被暂停的问答（落盘，刷新/重启后仍可继续）
}

// EnablePlans 打开计划模式（规划轮 + 步骤执行器）。
func (c *Chat) EnablePlans(planner *plan.Planner, runner *plan.Runner) {
	c.planner, c.runner = planner, runner
}

// EnableGoals 打开目标模式：聊天里的"目标"变成一条几秒后开跑的目标任务，
// 进展与结论走通知中心（与定时目标同一条链路），gate 用于超预算时先拒。
func (c *Chat) EnableGoals(eng *task.Engine, gate func() (bool, string)) {
	c.goalEng, c.goalGate = eng, gate
}

// PausedTurn 是一次被暂停的问答：原问题、已生成的部分回答，以及当时启用的技能。
// 继续时三者一起用：问题与部分回答重新注入会话，技能决定工具面/模型不变。
type PausedTurn struct {
	Question string `json:"question"`
	Partial  string `json:"partial"`
	Skill    string `json:"skill,omitempty"`
}

// chatRun 记录一次进行中的对话：cancel 用于暂停；pausing 标记区分"用户暂停"
// 与"客户端断开"（前者要保存现场供继续，后者直接丢弃）。
type chatRun struct {
	cancel  context.CancelFunc
	pausing bool
}

// New 创建网页聊天服务。uploadsDir 为图片上传目录（随服务启动清理过期文件）；
// pausedFile 为暂停现场落盘路径（空则仅内存）。
func New(brain *agent.Brain, uploadsDir, pausedFile string) *Chat {
	c := &Chat{
		brain:       brain,
		uploadsDir:  uploadsDir,
		pausedFile:  pausedFile,
		runs:        map[string]map[string]*chatRun{},
		queues:      map[string]*turnQueue{},
		dispatching: map[string]bool{},
		paused:      map[string]*PausedTurn{},
	}
	c.sweepUploads()
	c.loadPaused()
	return c
}

// loadPaused 恢复暂停现场。文件缺失按无暂停处理；损坏时保留原文件并放弃加载
// （不用空状态覆盖用户还能点"继续"的内容）。
func (c *Chat) loadPaused() {
	if c.pausedFile == "" {
		return
	}
	data, err := os.ReadFile(c.pausedFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[网页] 读取暂停现场 %q 失败，已保留原文件: %v", c.pausedFile, err)
		}
		return
	}
	var turns map[string]*PausedTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		log.Printf("[网页] 暂停现场 %q 损坏，已保留原文件；修复后重启可恢复", c.pausedFile)
		return
	}
	c.mu.Lock()
	for k, t := range turns {
		if t != nil && strings.TrimSpace(t.Question) != "" {
			c.paused[k] = t
		}
	}
	c.mu.Unlock()
}

// savePausedLocked 原子落盘暂停现场。调用方须持 c.mu。
func (c *Chat) savePausedLocked() {
	if c.pausedFile == "" {
		return
	}
	data, err := json.Marshal(c.paused)
	if err != nil {
		log.Printf("[网页] 序列化暂停现场失败: %v", err)
		return
	}
	dir := filepath.Dir(c.pausedFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[网页] 创建暂停现场目录失败: %v", err)
		return
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(c.pausedFile)+".tmp-*")
	if err != nil {
		log.Printf("[网页] 创建暂停现场临时文件失败: %v", err)
		return
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		log.Printf("[网页] 写入暂停现场失败: %v", err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		log.Printf("[网页] 同步暂停现场失败: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("[网页] 关闭暂停现场失败: %v", err)
		return
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		log.Printf("[网页] 设置暂停现场权限失败: %v", err)
		return
	}
	if err := os.Rename(f.Name(), c.pausedFile); err != nil {
		log.Printf("[网页] 替换暂停现场文件失败: %v", err)
	}
}

// sweepUploads 清理超过保留期的上传文件。
func (c *Chat) sweepUploads() {
	entries, err := os.ReadDir(c.uploadsDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-uploadKeepDays * 24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(c.uploadsDir, e.Name()))
		}
	}
}

// ResetContext 清除指定会话的对话上下文（key 为空时清除默认网页会话）。
func (c *Chat) ResetContext(key string) error { return c.brain.Reset(key) }

// ResolveUpload 校验上传附件 ID 并返回其在磁盘上的路径（智能体对话复用同一
// 上传链路；非法 ID 一律拒绝，防止路径拼接逃逸）。
func (c *Chat) ResolveUpload(id string) (string, bool) {
	if !uploadNameRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(c.uploadsDir, id), true
}

// SetBus 注入会话事件总线（Start 之前调用）。
//
// 主会话与 ACP 必须共用**同一个**总线实例：前端一条 /api/events/stream 连接
// 按 session 路由，序号才连续，两端读到的才是同一串事件。
func (c *Chat) SetBus(bus *eventbus.Bus) {
	c.mu.Lock()
	c.bus = bus
	c.mu.Unlock()
}

// Brain 返回对话大脑（热更新时替换其内部 Agent；会话池经其获取）。
func (c *Chat) Brain() *agent.Brain { return c.brain }

// HandleUpload 处理图片上传：multipart 字段 file，限 5MB，仅 png/jpeg/webp，
// 存为 uploads/<随机id>.<ext> 并返回 {"id": "..."}——发送消息时放入 attachments。
func (c *Chat) HandleUpload(ctx *vhttp.Context) error {
	file, err := ctx.GinContext().FormFile("file")
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "缺少文件字段 file"})
	}
	header := file
	if header.Size > uploadMaxSize {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "图片超过 5MB 上限"})
	}
	f, err := file.Open()
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取上传内容失败"})
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := http.DetectContentType(head[:n])
	var ext string
	switch mime {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	default:
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": "仅支持 png/jpeg/webp 图片，当前为 " + mime})
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "读取上传内容失败"})
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "读取上传内容失败"})
	}
	if err := os.MkdirAll(c.uploadsDir, 0o755); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "存储目录创建失败"})
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "生成文件 ID 失败"})
	}
	name := hex.EncodeToString(id) + ext
	if err := os.WriteFile(filepath.Join(c.uploadsDir, name), data, 0o644); err != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "保存图片失败"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"id": name})
}

// turnSeq 是轮次 id 的自增部分。
//
// 为什么不用纯时间戳：UnixNano 在并发下可能撞（两台客户端同一微秒提交），
// 而 turnId 是「停止」精确指认那一轮的唯一依据——撞了就停错对象。
// 时间戳保留在前面，人读起来仍能看出先后。
var turnSeq atomic.Uint64

// newTurnID 生成一个进程内唯一的轮次 id。
func newTurnID() string {
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) +
		"-" + strconv.FormatUint(turnSeq.Add(1), 36)
}

// HandleSSE 提交一轮对话：POST /api/chat，请求体
// {"message":"...", "attachments":["<上传id>", ...], "session":"<可选会话key，默认 web>",
// "skill":"...", "mode":"plan|goal", "clientId":"c-xxx", "device":"手机端"}。
//
// **语义变更（2026-11-11）**：本接口不再返回 SSE 流，而是立刻返回 JSON
// {turnId, status, seq, session}（契约 docs/EVENTS-API.md §2.1）。这一轮在后台
// 执行，增量/思考/步骤/收尾全部经事件总线广播到 session=web；桌面端与手机端
// 都从 /api/events/stream 读同一条流——以前只有发起请求的那条连接看得到，
// 所以"移动端问完、桌面端纹丝不动"。
//
// 撞上正在跑的一轮时：这一轮进 FIFO 队列（先进先出），返回 202 + status=queued
// 与排队位次。排队期间发起端断开也不影响——执行发生在 dispatcher 里，与客户端
// 已解绑，结果照常广播给所有订阅者。
//
// **页面关闭/断网不再中止生成**：只有显式 POST /api/chat/stop 才停。
// 这是移动端后台化与桌面端切窗口的前提（docs/MULTI-DEVICE-SYNC.md §3.3）。
func (c *Chat) HandleSSE(ctx *vhttp.Context) error {
	var req struct {
		Message     string   `json:"message"`
		Attachments []string `json:"attachments"`
		Session     string   `json:"session"`
		Skill       string   `json:"skill"`
		Mode        string   `json:"mode"`
		ClientID    string   `json:"clientId"`
		Device      string   `json:"device"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil ||
		(strings.TrimSpace(req.Message) == "" && len(req.Attachments) == 0) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": `请求体须为 {"message": "...", "attachments": [...]}`})
	}

	// 附件 → 图片内容（base64 进 provider 请求）
	var imgs []llm.Content
	for _, id := range req.Attachments {
		if !uploadNameRe.MatchString(id) {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "非法的附件 ID: " + id})
		}
		data, err := os.ReadFile(filepath.Join(c.uploadsDir, id))
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "附件不存在或已过期"})
		}
		imgs = append(imgs, llm.Content{Type: llm.ContentImage,
			Data: data, MIME: http.DetectContentType(data)})
	}
	if strings.TrimSpace(req.Message) == "" {
		req.Message = fmt.Sprintf("（用户发来了 %d 张图片）", len(imgs))
	}

	// 会话归属：不带 session 就是生活模式的主会话；工作模式带 work-* 的 key，
	// 且必须是「新建会话」登记过的——不能靠发一次消息凭空造出一条会话来。
	key, err := resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}

	// 暂停期间发新问题 = 中止被暂停的对话、开启下一段（用户"及时止损"）。
	c.clearPaused(key)

	origin := eventbus.Origin{ClientID: req.ClientID, Device: req.Device}
	turnID := newTurnID()

	// 入队：队列空就立刻开跑，否则排队等位次。
	status, position, err := c.enqueue(key, &queuedTurn{
		id: turnID, question: strings.TrimSpace(req.Message), origin: origin,
		enqueuedAt: time.Now(),
	})
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}

	body := map[string]any{
		"turnId":  turnID,
		"status":  status,
		"seq":     c.currentSeq(key),
		"session": key,
	}
	if status == statusQueued {
		// 202：正忙，这一轮排到队尾了。前端据此显示「排队中 · 第 N 位」。
		body["position"] = position
		return ctx.JsonResponse(vhttp.Code{Code: http.StatusAccepted, MsgKey: "accepted"}, body)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, body)
}

// 轮次状态：queued 排队中 / running 正在跑。
const (
	statusQueued  = "queued"
	statusRunning = "running"
)

// enqueue 把一轮放进会话队列：队列空就立刻开跑（statusRunning），
// 否则排队等着（statusQueued + 位次）。
//
// 队列满（queueMax）时返回错误，让用户知道要等前面的跑完——而不是无限期
// 攒下去，最后一次性全放出来打他一个措手不及。
func (c *Chat) enqueue(key string, t *queuedTurn) (string, int, error) {
	c.mu.Lock()
	q := c.queues[key]
	if q == nil {
		q = &turnQueue{}
		c.queues[key] = q
	}
	if q.len() >= queueMax {
		c.mu.Unlock()
		return "", 0, fmt.Errorf("排队太长了（最多 %d 条），先等前面的跑完", queueMax)
	}
	position := q.push(t)
	busy := c.isBusyLocked(key)
	if !busy && !c.dispatching[key] {
		c.dispatching[key] = true
		c.mu.Unlock()
		go c.dispatch(key)
		return statusRunning, 1, nil
	}
	c.mu.Unlock()
	// 排队：广播出去，另一端也看得见「手机端在排队」。
	c.publish(key, eventbus.TypeTurnQueued, t.id, t.origin, map[string]any{
		"position": position, "text": t.question,
	})
	return statusQueued, position, nil
}

// isBusyLocked 判定某会话是否有正在跑的一轮（调用方持 c.mu）。
func (c *Chat) isBusyLocked(key string) bool {
	for _, run := range c.runs[key] {
		if run != nil {
			return true
		}
	}
	return false
}

// dispatch 是这条会话的队列执行者：串行取队首，一轮跑完再跑下一轮。
//
// 队列空了它就退出（不留常驻 goroutine）；下次有请求再起一个。
func (c *Chat) dispatch(key string) {
	for {
		c.mu.Lock()
		q := c.queues[key]
		if q == nil {
			delete(c.dispatching, key)
			c.mu.Unlock()
			return
		}
		// 队首超时：丢掉（超过十分钟还没轮到，不该突然开跑）。
		t := q.peek()
		if t == nil {
			delete(c.dispatching, key)
			c.mu.Unlock()
			return
		}
		if time.Since(t.enqueuedAt) > queueTTL {
			q.pop()
			c.mu.Unlock()
			c.publish(key, eventbus.TypeTurnCanceled, t.id, t.origin, map[string]any{
				"reason": "等太久了，已取消",
			})
			continue
		}
		q.pop()
		c.mu.Unlock()

		c.runTurn(key, t)
	}
}

// publish 往事件总线投一条事件（总线未装配时静默跳过：它是可选的同步通道，
// 不该让它阻断一轮对话本身）。
func (c *Chat) publish(key, typ, turnID string, origin eventbus.Origin, data map[string]any) {
	c.mu.Lock()
	bus := c.bus
	c.mu.Unlock()
	if bus == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	bus.Publish(key, typ, turnID, origin, data)
}

// currentSeq 返回这条会话最近广播出去的事件序号（提交响应带回给前端当订阅游标）。
func (c *Chat) currentSeq(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bus == nil {
		return 0
	}
	return c.bus.CurrentSeq(key)
}

// resultSink 让一轮对话在执行完成后能额外带出结构化结果（目前是计划）。
type resultSink struct {
	mu   sync.Mutex
	plan *plan.Plan
}

func (r *resultSink) setPlan(p *plan.Plan) {
	r.mu.Lock()
	r.plan = p
	r.mu.Unlock()
}

func (r *resultSink) Plan() *plan.Plan {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plan
}

// runTurn 在后台真正跑一轮：产出全部经事件总线广播，不写任何 HTTP 响应。
//
// 为什么这样拆：以前一轮对话的字节流直接写进发起请求的那条连接，
// 发起端独占——另一端什么也看不到。现在「提交」与「消费」分开：这一轮跑在
// 后台 goroutine 里，事件进总线，谁订阅谁收，两端看到的内容完全一致。
//
// 上下文由 rctx 持有：它不随 HTTP 请求取消，所以客户端断开不会中止这一轮
// （只有显式 stop 会）。
func (c *Chat) runTurn(key string, t *queuedTurn) {
	question := t.question
	origin := t.origin

	// 提问本身先广播：另一端得看到「手机端刚问了什么」，否则那边只会莫名其妙
	// 多出一段回答。
	c.publish(key, eventbus.TypeUserMessage, t.id, origin, map[string]any{
		"text": question,
	})

	rctx, cancel := context.WithCancel(logx.WithID(context.Background(), logx.NextID()))
	defer cancel()
	rctx = agent.WithSessionKey(rctx, key)
	rctx = agent.WithOrigin(rctx, agent.OriginWeb)

	// 登记进行中的对话：暂停接口按 turnId 精确定位要停哪一轮。
	run := &chatRun{cancel: cancel}
	c.mu.Lock()
	if c.runs[key] == nil {
		c.runs[key] = map[string]*chatRun{}
	}
	c.runs[key][t.id] = run
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if m := c.runs[key]; m != nil {
			delete(m, t.id)
			if len(m) == 0 {
				delete(c.runs, key)
			}
		}
		c.mu.Unlock()
	}()

	c.publish(key, eventbus.TypeTurnStart, t.id, origin, nil)

	sink := &agent.SkillSink{}
	opts := []agent.CallOpt{}
	result := &resultSink{}

	// 思考过程事件泵：订阅本轮的过程事件转发给总线。
	steps, stopSteps := agent.SubscribeSteps(logx.IDFrom(rctx))
	defer stopSteps()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case <-rctx.Done():
				// 对话已结束：把通道里积压的过程事件抽干写完再退，
				// 否则轮末的 tool ok/fail 会留在通道里，前端的工具块永远转圈。
				for {
					select {
					case st, ok := <-steps:
						if !ok {
							return
						}
						c.publish(key, eventbus.TypeStep, t.id, origin, map[string]any{
							"step": st,
						})
					default:
						return
					}
				}
			case st, ok := <-steps:
				if !ok {
					return
				}
				c.publish(key, eventbus.TypeStep, t.id, origin, map[string]any{
					"step": st,
				})
			}
		}
	}()

	streamed := ""
	answer, err := c.ask(rctx, key, question, origin, t.id, sink, result, opts, t.partial, t.skill, func(d llm.Delta) {
		if d.Thinking != "" {
			c.publish(key, eventbus.TypeThinking, t.id, origin, map[string]any{
				"thinking": d.Thinking,
			})
		}
		if d.Text == "" {
			return
		}
		streamed += d.Text
		c.publish(key, eventbus.TypeDelta, t.id, origin, map[string]any{
			"delta": d.Text,
		})
	})
	cancel() // 对话已结束（完成或中止）：停掉事件泵再等它退出，避免正常路径死锁
	<-pumpDone

	if err != nil {
		c.mu.Lock()
		pausing := run.pausing
		if pausing {
			c.paused[key] = &PausedTurn{Question: question, Partial: streamed, Skill: sink.Name()}
			c.savePausedLocked()
		}
		c.mu.Unlock()
		if pausing {
			c.publish(key, eventbus.TypePaused, t.id, origin, map[string]any{
				"partial": streamed,
			})
			c.publish(key, eventbus.TypeTurnEnd, t.id, origin, map[string]any{
				"turn": turnUsage(logx.IDFrom(rctx)),
			})
			return
		}
		c.publish(key, eventbus.TypeTurnError, t.id, origin, map[string]any{
			"error": chatErrorMessage(err),
		})
		c.publish(key, eventbus.TypeTurnEnd, t.id, origin, map[string]any{
			"turn": turnUsage(logx.IDFrom(rctx)),
		})
		return
	}

	// 增量为空但拿到了完整回答（非流式路径）：补发一次，保证页面有内容。
	if strings.TrimSpace(streamed) == "" && strings.TrimSpace(answer) != "" {
		c.publish(key, eventbus.TypeDelta, t.id, origin, map[string]any{
			"delta": answer,
		})
	}
	// 规划轮产出了计划：清单结构发给前端渲染计划卡。
	if p := result.Plan(); p != nil {
		c.publish(key, eventbus.TypePlan, t.id, origin, map[string]any{
			"plan": map[string]any{
				"id": p.ID, "title": p.Title, "summary": p.Summary,
				"state": p.State, "goal": p.Goal, "skill": p.Skill, "steps": p.Steps,
			},
		})
	}
	c.publish(key, eventbus.TypeTurnEnd, t.id, origin, map[string]any{
		"turn": turnUsage(logx.IDFrom(rctx)),
	})
}

// ask 执行这一轮对话：普通对话 / 计划模式 / 目标模式都从这里走。
//
// 三种模式的差别只在「这一轮怎么算完」，事件出口是同一个——所以前端
// 不必为每种模式各写一套渲染。
func (c *Chat) ask(rctx context.Context, key, question string, origin eventbus.Origin, turnID string,
	sink *agent.SkillSink, result *resultSink, opts []agent.CallOpt,
	partial, skill string, onDelta func(llm.Delta)) (string, error) {
	logx.Printf(rctx, "[网页] 输入: %s（会话 %s）", question, key)
	if partial != "" {
		// 继续被暂停的对话：原问题与已生成的部分回答重新注入会话后接着完成。
		// 带上暂停现场记录的技能：技能限定了工具面与模型，继续时换一套会静默改变行为。
		opts = append(opts, agent.WithSkillName(skill))
		return c.brain.Resume(rctx, key, question, partial, onDelta, opts...)
	}
	return c.brain.AskWith(rctx, question, nil, onDelta, opts...)
}

// HandleResume 继续被暂停的对话：POST /api/chat/resume，body {"session":"<key>", "clientId":"...", "device":"..."}。
//
// 与 HandleSSE 一样是提交式：立刻返回 {turnId,status,seq,session}，这一轮在后台跑，
// 增量与收尾经事件总线广播（契约 docs/EVENTS-API.md §2.1）。
//
// 继续过程同样可暂停（部分回答在原有基础上累加）。
func (c *Chat) HandleResume(ctx *vhttp.Context) error {
	var req struct {
		Session  string `json:"session"`
		ClientID string `json:"clientId"`
		Device   string `json:"device"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body 视为继续主会话
	key, err := resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	turn := c.takePaused(key)
	if turn == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有可继续的暂停对话"})
	}
	logx.Printf(logx.WithID(ctx.Context(), logx.NextID()), "[网页] 继续被暂停的对话: %s", logx.Truncate(turn.Question, 80))

	origin := eventbus.Origin{ClientID: req.ClientID, Device: req.Device}
	turnID := newTurnID()
	status, position, err := c.enqueue(key, &queuedTurn{
		id: turnID, question: turn.Question, origin: origin,
		enqueuedAt: time.Now(), partial: turn.Partial, skill: turn.Skill,
	})
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}

	body := map[string]any{
		"turnId":  turnID,
		"status":  status,
		"seq":     c.currentSeq(key),
		"session": key,
	}
	if status == statusQueued {
		body["position"] = position
		return ctx.JsonResponse(vhttp.Code{Code: http.StatusAccepted, MsgKey: "accepted"}, body)
	}
	return ctx.JsonResponse(vhttp.Codes.Success, body)
}

// HandleQueue 返回该会话的排队快照：GET /api/chat/queue?session=web。
//
// 前端用它渲染「排队中 · 第 N 位」——位次是队列里的位置，
// 另一端排队时也能看到（配合 turn.queued 事件）。
func (c *Chat) HandleQueue(ctx *vhttp.Context) error {
	key, err := resolveSessionKey(ctx.GinContext().Query("session"))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	c.mu.Lock()
	q := c.queues[key]
	c.mu.Unlock()
	items := []queueItem{}
	if q != nil {
		items = q.snapshot()
	}
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]any{
		"session": key,
		"items":   items,
	})
}

// HandleStop 暂停当前进行中的对话：POST /api/chat/stop，body {"session":"<key>", "turnId":"t..."}。
//
// **turnId 必填**：以前按会话停，同一会话上任何一轮都可能被停掉——而两端同时
// 在用时，「停止」很容易停到对方那一轮上。现在按轮次精确指认，对不上就如实报错。
//
// 被停的这一轮，已生成的部分回答保存为暂停现场，可 POST /api/chat/resume 继续。
func (c *Chat) HandleStop(ctx *vhttp.Context) error {
	var req struct {
		Session string `json:"session"`
		TurnID  string `json:"turnId"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body 视为停主会话
	key, err := resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	if strings.TrimSpace(req.TurnID) == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "请求体须为 '{turnId: t...}'（turnId 必填：停止必须指明停哪一轮，否则可能停到另一端正在跑的那轮）",
		})
	}
	c.mu.Lock()
	run := c.runs[key][req.TurnID]
	if run != nil {
		run.pausing = true // 先标记再取消：Ask 返回时据此保存暂停现场
	}
	c.mu.Unlock()
	if run == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "这一轮已经结束了（或不存在），没有可停的对话",
		})
	}
	run.cancel()
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{
		"message": "已暂停，可点击继续接着完成，或直接输入新问题开启新对话",
	})
}

// PausedTurn 返回会话的暂停现场（无则 nil），供历史接口与页面恢复展示。
func (c *Chat) PausedTurn(key string) *PausedTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.paused[key]
	if pt == nil {
		return nil
	}
	cp := *pt
	return &cp
}

// clearPaused 丢弃会话的暂停现场（新问题 = 放弃被暂停的对话）。
func (c *Chat) clearPaused(key string) {
	c.mu.Lock()
	if _, ok := c.paused[key]; ok {
		delete(c.paused, key)
		c.savePausedLocked()
	}
	c.mu.Unlock()
}

// takePaused 原子取出并移除暂停现场（继续时使用；再次暂停会以累加的部分回答重新存入）。
func (c *Chat) takePaused(key string) *PausedTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.paused[key]
	if pt == nil {
		return nil
	}
	delete(c.paused, key)
	c.savePausedLocked()
	return pt
}

// turnUsage 汇总本轮问答的 token 与墙钟耗时，随 done 帧发给前端。
// 数取自 trace（OnLLMCall 逐轮累加，按请求 ID 归并），不另起一套记账；
// 暂停与出错路径同样带上——已经烧掉的 token 不该因为这一轮没跑完就不显示。
// 历史回显走同一个 Request.Usage()，所以刷新前后的数字一致。
func turnUsage(id string) *trace.TurnUsage {
	req, ok := trace.Get(id)
	if !ok {
		return nil
	}
	return req.Usage()
}

// chatErrorMessage 把后端/服务商的错误翻成用户能照着做的一句话。
//
// 之前所有失败都显示「出了点小问题，稍后再试试吧」，看不出是预算不够、密钥坏了
// 还是服务商限流，所以按错误特征分派；认不出来的把原始错误一起带出来，
// 至少能拿去搜。框架错误带 "agent: 第 N 轮调用失败: " 前缀，剥掉只留原因。
func chatErrorMessage(err error) string {
	msg := err.Error()
	cause := msg
	if i := strings.Index(cause, ": "); i >= 0 && strings.HasPrefix(cause, "agent:") {
		cause = cause[strings.Index(cause, ": ")+2:]
	}
	low := strings.ToLower(cause)
	switch {
	case strings.Contains(cause, "模型未返回任何文本内容"):
		return "模型这一轮只生成了思考、正文是空的。思考与正文共用「最大输出 token」这份预算，" +
			"当前预算不够写出完整回答——到「设置 → 模型服务」把该模型的「最大输出 token」调大（如 8192）再重试。"
	case strings.Contains(low, "support image input") || strings.Contains(low, "model_incompatible"):
		return "当前配置的模型不支持图片输入，请在 config.yaml 更换支持视觉的模型"
	case strings.Contains(low, "context length") || strings.Contains(low, "context_length_exceeded") ||
		strings.Contains(cause, "上下文"):
		return "这轮对话超出了模型的上下文窗口。可以先「清除上下文」，或在「设置 → 模型服务」核对该模型的上下文窗口设置。"
	case strings.Contains(low, "401") || strings.Contains(low, "invalid api key") ||
		strings.Contains(low, "authentication"):
		return "模型服务拒绝了当前密钥。到「设置 → 模型服务」重新填写 API 密钥（或检查该服务商的密钥是否已失效）。"
	case strings.Contains(low, "429") || strings.Contains(low, "rate limit") ||
		strings.Contains(low, "quota") || strings.Contains(low, "insufficient"):
		return "模型服务限流或额度不足。稍等片刻再试，或检查服务商账户余额/配额。"
	case strings.Contains(cause, "轮数上限"):
		return "这一轮的工具调用轮数到了上限（可在「设置 → 运行上限」把「单轮工具调用上限」调大），" +
			"后半截活儿没做完；已完成的工具操作都保留着，调大后重试即可。"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(low, "timeout") ||
		strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") || strings.Contains(low, "no such host") ||
		strings.Contains(low, "eof"):
		return "连不上模型服务（" + logx.Truncate(cause, 120) + "）。检查「设置 → 模型服务」里的 API 地址与本机网络后重试。"
	default:
		return "这轮回复失败了：" + logx.Truncate(cause, 200)
	}
}
