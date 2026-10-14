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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/llm"
	vhttp "github.com/capyflow/vortexv3/server/http"

	"github.com/capyflow/friday/internal/agent"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

const (
	uploadMaxSize  = 5 << 20 // 单张图片上限 5MB
	uploadMaxCount = 4       // 单条消息最多 4 张
	uploadKeepDays = 7       // 上传文件保留天数
)

var uploadNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp)$`)

// Chat 是网页聊天服务：经 Brain 与 agent 对话（网页会话 key 固定 web，
// 可通过请求体 session 字段切换到其他会话查看/续聊）。
// 支持对话暂停/继续：暂停取消进行中的生成（及时止损），被中断的问答记录在
// paused 中可一键继续；暂停期间发新问题则丢弃暂停记录、开启新对话。
type Chat struct {
	brain      *agent.Brain
	uploadsDir string

	mu     sync.Mutex
	runs   map[string]*chatRun    // 会话 key → 进行中的对话（可暂停）
	paused map[string]*PausedTurn // 会话 key → 被暂停的问答（内存态，重启丢失）
}

// PausedTurn 是一次被暂停的问答：原问题与已生成的部分回答（继续时重新注入会话）。
type PausedTurn struct {
	Question string `json:"question"`
	Partial  string `json:"partial"`
}

// chatRun 记录一次进行中的对话：cancel 用于暂停；pausing 标记区分"用户暂停"
// 与"客户端断开"（前者要保存现场供继续，后者直接丢弃）。
type chatRun struct {
	cancel  context.CancelFunc
	pausing bool
}

// New 创建网页聊天服务。uploadsDir 为图片上传目录（随服务启动清理过期文件）。
func New(brain *agent.Brain, uploadsDir string) *Chat {
	c := &Chat{
		brain:      brain,
		uploadsDir: uploadsDir,
		runs:       map[string]*chatRun{},
		paused:     map[string]*PausedTurn{},
	}
	c.sweepUploads()
	return c
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
func (c *Chat) HandleSSE(ctx *vhttp.Context) error {
	var req struct {
		Message     string   `json:"message"`
		Attachments []string `json:"attachments"`
		Session     string   `json:"session"`
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

	// 全局单对话：所有渠道共用同一个主会话，网页端不支持切换到其他会话。
	key := agent.SessionWeb
	// 暂停期间发新问题 = 中止被暂停的对话、开启下一段（用户"及时止损"）。
	c.clearPaused(key)

	question := strings.TrimSpace(req.Message)
	return c.stream(ctx, key, question, imgs, "", func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
		logx.Printf(rctx, "[网页] 输入: %s（图片 %d 张，会话 %s）", question, len(imgs), key)
		return c.brain.Ask(rctx, question, imgs, onDelta)
	})
}

// HandleResume 继续被暂停的对话（POST /api/chat/resume，SSE 流式，协议同 /api/chat）：
// 把原问题与已生成的部分回答重新注入会话后让模型接着完成。
// 继续过程同样可暂停（部分回答在原有基础上累加）。
func (c *Chat) HandleResume(ctx *vhttp.Context) error {
	key := agent.SessionWeb
	turn := c.takePaused(key)
	if turn == nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "没有可继续的暂停对话"})
	}
	logx.Printf(logx.WithID(ctx.Context(), logx.NextID()), "[网页] 继续被暂停的对话: %s", logx.Truncate(turn.Question, 80))
	return c.stream(ctx, key, turn.Question, nil, turn.Partial, func(rctx context.Context, onDelta func(llm.Delta)) (string, error) {
		return c.brain.Resume(rctx, key, turn.Question, turn.Partial, onDelta)
	})
}

// HandleStop 暂停当前进行中的对话（POST /api/chat/stop）：取消后端生成
// （token 即刻停止），已生成的部分回答保存为暂停现场，可 POST /api/chat/resume 继续。
func (c *Chat) HandleStop(ctx *vhttp.Context) error {
	key := agent.SessionWeb
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
	delete(c.paused, key)
	c.mu.Unlock()
}

// takePaused 原子取出并移除暂停现场（继续时使用；再次暂停会以累加的部分回答重新存入）。
func (c *Chat) takePaused(key string) *PausedTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := c.paused[key]
	delete(c.paused, key)
	return pt
}

// stream 是 /api/chat 与 /api/chat/resume 共用的 SSE 骨架：搭建流式连接、
// 订阅思考过程事件、桥接暂停，ask 在独立上下文中执行对话并返回完整回答。
// partialBase 为继续场景下已有一部分回答的前缀（再次暂停时累加保存）。
func (c *Chat) stream(ctx *vhttp.Context, key, question string, imgs []llm.Content, partialBase string,
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
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	rctx, cancel := context.WithCancel(logx.WithID(ctx.Context(), logx.NextID()))
	defer cancel()
	rctx = agent.WithSessionKey(rctx, key)
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
				return
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
		if rctx.Err() != nil {
			// 被暂停（而非客户端断开）：保存现场并告知前端可继续。
			c.mu.Lock()
			pausing := run.pausing
			if pausing {
				c.paused[key] = &PausedTurn{Question: question, Partial: partialBase + streamed}
			}
			c.mu.Unlock()
			if pausing {
				logx.Printf(rctx, "[网页] 对话被暂停（已生成 %d 字，可继续）", len([]rune(streamed)))
				trace.Record(rctx, "pause", "对话被用户暂停，已生成 %d 字可继续", len([]rune(streamed)))
				_ = write(map[string]any{"paused": true, "partial": streamed})
				_ = write(map[string]any{"done": true})
			}
			return nil
		}
		logx.Printf(rctx, "agent 出错: %v", err)
		trace.End(logx.IDFrom(rctx), err.Error())
		errMsg := "出了点小问题，稍后再试试吧"
		if strings.Contains(err.Error(), "support image input") || strings.Contains(err.Error(), "model_incompatible") {
			errMsg = "当前配置的模型不支持图片输入，请在 config.yaml 更换支持视觉的模型"
		}
		_ = write(map[string]any{"done": true, "error": errMsg})
		return nil
	}
	// 增量为空但拿到了完整回答（非流式路径）：补发一次，保证页面有内容。
	if strings.TrimSpace(streamed) == "" && strings.TrimSpace(answer) != "" {
		_ = write(map[string]string{"delta": answer})
	}
	trace.End(logx.IDFrom(rctx), "")
	_ = write(map[string]any{"done": true})
	return nil
}
