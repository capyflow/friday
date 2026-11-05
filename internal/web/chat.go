// Package web 提供网页聊天的对话与图片上传接口。页面外壳（登录、导航、各视图）
// 由 admin 包统一提供；本包只负责把对话请求接到 agent Brain 上。
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capyflow/vortexagent/llm"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/plan"
	"github.com/capyflow/friday/internal/task"
	"github.com/capyflow/friday/internal/trace"
	"github.com/capyflow/friday/internal/workdir"
)

const (
	uploadMaxSize  = 5 << 20 // 单张图片上限 5MB
	uploadMaxCount = 4       // 单条消息最多 4 张
	uploadKeepDays = 7       // 上传文件保留天数
)

var uploadNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp)$`)

// Chat 是网页聊天服务：经 Brain 与 agent 对话。会话分两种模式（见 sessions.go）：
// 生活模式只有主会话 web（不传 session 就是它），工作模式可以在聊天页新建多条
// 独立会话，每条各自的上下文。
// 支持对话暂停/继续：暂停取消进行中的生成（及时止损），被中断的问答记录在
// paused 中可一键继续；暂停期间发新问题则丢弃暂停记录、开启新对话。
type Chat struct {
	brain        *agent.Brain
	uploadsDir   string
	pausedFile   string // 暂停现场落盘路径（空 = 仅内存，重启丢）
	sessionsFile string // 工作会话注册表落盘路径（空 = 会话只活在内存里，重启清零）

	planner  *plan.Planner // 计划模式（nil = 未启用）
	runner   *plan.Runner
	goalEng  *task.Engine          // 目标模式：聊天里"目标"直接建一条即时目标任务
	goalGate func() (bool, string) // 超预算时不接新目标（nil = 不做预算拦截）

	mu     sync.Mutex
	runs   map[string]*chatRun    // 会话 key → 进行中的对话（可暂停）
	paused map[string]*PausedTurn // 会话 key → 被暂停的问答（落盘，刷新/重启后仍可继续）
	work   map[string]sessionMeta // 工作会话注册表（sessions.go，落盘 sessionsFile）
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
// pausedFile 为暂停现场落盘路径（空则仅内存）；sessionsFile 为工作会话注册表
// 落盘路径（空则工作会话不持久化，重启后列表回到只剩主会话）。
func New(brain *agent.Brain, uploadsDir, pausedFile, sessionsFile string) *Chat {
	c := &Chat{
		brain:        brain,
		uploadsDir:   uploadsDir,
		pausedFile:   pausedFile,
		sessionsFile: sessionsFile,
		runs:         map[string]*chatRun{},
		paused:       map[string]*PausedTurn{},
		work:         map[string]sessionMeta{},
	}
	c.sweepUploads()
	c.loadPaused()
	c.loadWork()
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

// HandleSSE 处理一次对话：请求体 {"message":"...", "attachments":["<上传id>", ...],
// "session":"<可选会话key，默认 web>"}，响应为 SSE 流。由 admin 包挂载到
// POST /api/chat（需登录）；LLM 增量与思考过程在回调里同步写出并 flush，
// 客户端断开或用户暂停（POST /api/chat/stop）时取消 ctx 中止对话。
//
// SSE 帧协议（data: JSON）：
//
//	{"delta": "..."}                       回答文本增量
//	{"thinking": "..."}                    思考内容增量（思考型模型）
//	{"step": {kind,name,detail,status}}    思考过程事件（轮次/工具调用）
//	{"paused": true, "partial": "..."}     对话被用户暂停（部分回答已保存可继续）
//	{"done": true} / {"done": true, "error": "..."}
//	                                      本轮结束，附 turn={in,out,ms} 本轮 token 汇总
//	                                      （轨迹里没有这一请求时为 null，前端按缺省处理）
func (c *Chat) HandleSSE(ctx *vhttp.Context) error {
	var req struct {
		Message     string   `json:"message"`
		Attachments []string `json:"attachments"`
		Session     string   `json:"session"`
		Skill       string   `json:"skill"`
		Mode        string   `json:"mode"`
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
	key, err := c.resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	// 暂停期间发新问题 = 中止被暂停的对话、开启下一段（用户"及时止损"）。
	c.clearPaused(key)

	question := strings.TrimSpace(req.Message)
	sink := &agent.SkillSink{}
	opts := []agent.CallOpt{}
	if skillName := strings.TrimSpace(req.Skill); skillName != "" {
		opts = append(opts, agent.WithSkillName(skillName))
	}
	result := &resultSink{}

	// 目标模式：把这句话登记成一个即时目标（不占用本次连接做长轮推进，
	// 结论到了通知页/聊天里由通知中心送达），零模型调用因此也就零成本。
	if mode := strings.TrimSpace(req.Mode); mode == "goal" {
		if c.goalEng == nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "目标模式未启用"})
		}
		if c.goalGate != nil {
			if ok, reason := c.goalGate(); !ok {
				return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": reason + "（目标未登记，可在运行状态页恢复）"})
			}
		}
		return c.stream(ctx, key, question, nil, "", sink, result, func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
			logx.Printf(rctx, "[网页] 登记目标: %s", logx.Truncate(question, 80))
			t, err := agent.LaunchGoal(c.goalEng, question)
			if err != nil {
				return "", err
			}
			trace.Begin(logx.IDFrom(rctx), "web", key, question)
			trace.Record(rctx, "goal_run", "已登记目标 #%s，将在后台多轮推进，结论送达通知页", t.ID)
			return fmt.Sprintf("好，把「%s」当作目标开始处理，办完或卡住都会告诉你（也可在「自动化」页看进展）。",
				logx.Truncate(question, 40)), nil
		})
	}

	// 计划模式：先跑规划轮产出可审阅的步骤清单，确认与执行都在服务端计划仓库里推进。
	if mode := strings.TrimSpace(req.Mode); mode == "plan" {
		if c.planner == nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "计划模式未启用"})
		}
		if len(imgs) > 0 {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "计划模式暂不支持图片，请先普通对话发图再制定计划"})
		}
		return c.stream(ctx, key, question, nil, "", sink, result, func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
			logx.Printf(rctx, "[网页] 制定计划: %s", logx.Truncate(question, 80))
			p, fallback, err := c.planner.Draft(rctx, question, "web", strings.TrimSpace(req.Skill))
			if err != nil {
				return "", err
			}
			if p == nil {
				// 规划没产出清单：模型的话已按普通回答返回，不留下无法执行的空计划。
				return fallback, nil
			}
			result.setPlan(p)
			return "先把要做的事拆成了 " + fmt.Sprint(len(p.Steps)) + " 步，确认后我按顺序做（也可以先改再确认）。", nil
		})
	}

	return c.stream(ctx, key, question, imgs, "", sink, result, func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
		logx.Printf(rctx, "[网页] 输入: %s（图片 %d 张，会话 %s）", question, len(imgs), key)
		return c.brain.AskWith(rctx, question, imgs, onDelta, opts...)
	})
}

// resultSink 让一次 SSE 请求在执行完成后能额外带出结构化结果（目前是计划）。
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

// HandleResume 继续被暂停的对话（POST /api/chat/resume，SSE 流式，协议同 /api/chat）：
// 把原问题与已生成的部分回答重新注入会话后让模型接着完成。
// 继续过程同样可暂停（部分回答在原有基础上累加）。
// body {"session":"<key>"} 可选，缺省继续主会话——暂停现场按会话各存一份，
// 在 A 会话暂停的，不能在 B 会话点继续。
func (c *Chat) HandleResume(ctx *vhttp.Context) error {
	var req struct {
		Session string `json:"session"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body 视为继续主会话
	key, err := c.resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	turn := c.takePaused(key)
	if turn == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有可继续的暂停对话"})
	}
	logx.Printf(logx.WithID(ctx.Context(), logx.NextID()), "[网页] 继续被暂停的对话: %s", logx.Truncate(turn.Question, 80))
	sink := &agent.SkillSink{}
	return c.stream(ctx, key, turn.Question, nil, turn.Partial, sink, &resultSink{}, func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
		// 带上暂停现场记录的技能：技能限定了工具面与模型，继续时换一套会静默改变行为。
		return c.brain.Resume(rctx, key, turn.Question, turn.Partial, onDelta,
			agent.WithSkillName(turn.Skill))
	})
}

// HandleStop 暂停当前进行中的对话（POST /api/chat/stop，body 可带 {"session":"<key>"}，
// 缺省停主会话）：取消后端生成（token 即刻停止），已生成的部分回答保存为暂停现场，
// 可 POST /api/chat/resume 继续。
func (c *Chat) HandleStop(ctx *vhttp.Context) error {
	var req struct {
		Session string `json:"session"`
	}
	_ = ctx.UnmarshalBody(&req) // 空 body 视为停主会话
	key, err := c.resolveSessionKey(req.Session)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	c.mu.Lock()
	run := c.runs[key]
	if run != nil {
		run.pausing = true // 先标记再取消：Ask 返回时据此保存暂停现场
	}
	c.mu.Unlock()
	if run == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有进行中的对话"})
	}
	run.cancel()
	return ctx.JsonResponse(vhttp.Codes.Success, map[string]string{"message": "已暂停，可点击继续接着完成，或直接输入新问题开启新对话"})
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
	case strings.Contains(low, "timeout") || strings.Contains(low, "dial tcp") ||
		strings.Contains(low, "connection refused") || strings.Contains(low, "no such host") ||
		strings.Contains(low, "eof"):
		return "连不上模型服务（" + logx.Truncate(cause, 120) + "）。检查「设置 → 模型服务」里的 API 地址与本机网络后重试。"
	default:
		return "这轮回复失败了：" + logx.Truncate(cause, 200)
	}
}

// stream 是 /api/chat 与 /api/chat/resume 共用的 SSE 骨架：搭建流式连接、
// 订阅思考过程事件、桥接暂停，ask 在独立上下文中执行对话并返回完整回答。
// partialBase 为继续场景下已有一部分回答的前缀（再次暂停时累加保存）；
// sink 用于记下本轮实际启用的技能，暂停现场要带上它才能在下一次继续时复用；
// result 用于在执行完成后额外带出结构化结果（计划模式的步骤清单）。
func (c *Chat) stream(ctx *vhttp.Context, key, question string, imgs []llm.Content, partialBase string,
	sink *agent.SkillSink, result *resultSink,
	ask func(rctx context.Context, onDelta func(llm.Delta)) (string, error)) error {
	gin := ctx.GinContext()
	gin.Header("Content-Type", "text/event-stream; charset=utf-8")
	gin.Header("Cache-Control", "no-cache")
	gin.Header("X-Accel-Buffering", "no")
	gin.Writer.WriteHeader(http.StatusOK)
	flusher, ok := gin.Writer.(http.Flusher)
	if !ok {
		return ctx.JsonResponse(vhttp.Codes.InternalError,
			map[string]string{"error": "连接不支持流式响应"})
	}

	var wmu sync.Mutex // delta 回调与事件泵并发写流
	// 写失败 = 浏览器那侧的连接已经没了。用它（而不是 rctx.Err()）区分
	// "客户端断开"与"模型出错"：rctx 在对话结束时被我们主动 cancel，判不出来源。
	var writeFailed atomic.Bool
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
			writeFailed.Store(true)
			return err
		}
		flusher.Flush()
		return nil
	}

	rctx, cancel := context.WithCancel(logx.WithID(ctx.Context(), logx.NextID()))
	defer cancel()
	rctx = agent.WithSessionKey(rctx, key)
	rctx = agent.WithOrigin(rctx, agent.OriginWeb)
	// 这条会话选了工作目录就随请求带下去：首轮的项目上下文从那儿读 AGENTS.md，
	// 文件工具也把那个目录当作允许根（没选就是平时的行为）。
	if dir := c.SessionDir(key); dir != "" {
		rctx = workdir.With(rctx, dir)
	}
	rctx = agent.WithSkillSink(rctx, sink)
	trace.Begin(logx.IDFrom(rctx), "web", key, question)

	// 登记进行中的对话（暂停接口按会话 key 取消），退出时注销。
	run := &chatRun{cancel: cancel}
	c.mu.Lock()
	c.runs[key] = run
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.runs, key)
		c.mu.Unlock()
	}()

	// 思考过程事件泵：订阅本请求的过程事件转发给前端；连接结束即停。
	steps, stopSteps := agent.SubscribeSteps(logx.IDFrom(rctx))
	defer stopSteps()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case <-rctx.Done():
				// 对话已结束、不再有新事件发布：把通道里积压的过程事件抽干
				// 写完再退。select 在两侧同时就绪时随机挑一个，直接 return
				// 会把轮末的 tool ok/fail 留在通道里，前端的工具块永远停在
				// 运行中（转圈）。
				for {
					select {
					case s, ok := <-steps:
						if !ok {
							return
						}
						_ = write(map[string]any{"step": s})
					default:
						return
					}
				}
			case s, ok := <-steps:
				if !ok {
					return
				}
				_ = write(map[string]any{"step": s})
			}
		}
	}()

	streamed := ""
	answer, err := ask(rctx, func(d llm.Delta) {
		if d.Thinking != "" {
			if werr := write(map[string]string{"thinking": d.Thinking}); werr != nil {
				cancel() // 客户端已断开，中止对话
				return
			}
		}
		if d.Text == "" {
			return
		}
		streamed += d.Text
		if werr := write(map[string]string{"delta": d.Text}); werr != nil {
			cancel() // 客户端已断开，中止对话
		}
	})
	cancel() // 对话已结束（完成或中止）：停掉事件泵再等它退出，避免正常路径死锁
	<-pumpDone

	if err != nil {
		c.mu.Lock()
		pausing := run.pausing
		if pausing {
			c.paused[key] = &PausedTurn{Question: question, Partial: partialBase + streamed,
				Skill: sink.Name()}
			c.savePausedLocked()
		}
		c.mu.Unlock()
		switch {
		case pausing:
			logx.Printf(rctx, "[网页] 对话被暂停（已生成 %d 字，可继续）", len([]rune(streamed)))
			trace.Record(rctx, "pause", "对话被用户暂停，已生成 %d 字可继续", len([]rune(streamed)))
			_ = write(map[string]any{"paused": true, "partial": streamed})
			_ = write(map[string]any{"done": true, "turn": turnUsage(logx.IDFrom(rctx))})
			return nil
		case writeFailed.Load():
			// 浏览器已断开：没有连接可发错误帧，记一条日志收尾即可。
			logx.Printf(rctx, "[网页] 连接已断开，本轮对话中止: %v", err)
			trace.End(logx.IDFrom(rctx), err.Error())
			return nil
		}
		logx.Printf(rctx, "agent 出错: %v", err)
		trace.End(logx.IDFrom(rctx), err.Error())
		_ = write(map[string]any{"done": true, "error": chatErrorMessage(err), "turn": turnUsage(logx.IDFrom(rctx))})
		return nil
	}
	// 增量为空但拿到了完整回答（非流式路径）：补发一次，保证页面有内容。
	if strings.TrimSpace(streamed) == "" && strings.TrimSpace(answer) != "" {
		_ = write(map[string]string{"delta": answer})
	}
	trace.End(logx.IDFrom(rctx), "")
	// 规划轮产出了计划：把清单结构发给前端渲染计划卡（确认/改步骤/取消都在卡片里做）。
	if p := result.Plan(); p != nil {
		_ = write(map[string]any{"plan": map[string]any{
			"id": p.ID, "title": p.Title, "summary": p.Summary, "state": p.State,
			"goal": p.Goal, "skill": p.Skill, "steps": p.Steps,
		}})
	}
	_ = write(map[string]any{"done": true, "turn": turnUsage(logx.IDFrom(rctx))})
	return nil
}
