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
type Chat struct {
	brain      *agent.Brain
	uploadsDir string
}

// New 创建网页聊天服务。uploadsDir 为图片上传目录（随服务启动清理过期文件）。
func New(brain *agent.Brain, uploadsDir string) *Chat {
	c := &Chat{brain: brain, uploadsDir: uploadsDir}
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
func (c *Chat) ResetContext(key string) { c.brain.Reset(key) }

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
// POST /api/chat（需登录）；LLM 增量在 Ask 回调里同步写出并 flush，
// 客户端断开时取消 ctx 中止对话。
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
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(gin.Writer, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	rctx, cancel := context.WithCancel(logx.WithID(ctx.Context(), logx.NextID()))
	defer cancel()
	// 全局单对话：所有渠道共用同一个主会话，网页端不支持切换到其他会话。
	key := agent.SessionWeb
	rctx = agent.WithSessionKey(rctx, key)
	logx.Printf(rctx, "[网页] 输入: %s（图片 %d 张，会话 %s）", req.Message, len(imgs), key)
	trace.Begin(logx.IDFrom(rctx), "web", key, req.Message)

	streamed := ""
	answer, err := c.brain.Ask(rctx, strings.TrimSpace(req.Message), imgs, func(d llm.Delta) {
		if d.Text == "" {
			return
		}
		streamed += d.Text
		if werr := write(map[string]string{"delta": d.Text}); werr != nil {
			cancel() // 客户端已断开，中止对话
		}
	})
	if err != nil {
		if rctx.Err() != nil {
			return nil // 客户端断开或程序退出，连接已失效
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
