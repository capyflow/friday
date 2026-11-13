// files.go 是文件预览与下载接口：聊天/智能体时间线里输出的本机文件路径，前端
// 渲染成可点 chip，点开后由这里只读读取内容，侧边抽屉按类型渲染；下载端点
// 支撑手机端文件面板把文件拉到本机。
// 信任档说明：服务仅监听本机、接口需登录，可读范围与 agent 自身读工具一致
// （本机任意可读普通文件），不做目录白名单；拒绝目录与非普通文件。
package admin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

// 预览限额：文本最多回传前 512KB（超出带 truncated 标记），图片最大 8MB。
const (
	filePreviewTextMax  = 512 << 10
	filePreviewImageMax = 8 << 20
)

// resolveFilePath 解析并校验 ?path= 参数：支持 ~/ 前缀，展开为绝对路径，要求
// 存在且为普通文件。返回错误消息（空串=通过），预览与下载共用同一套口径。
func resolveFilePath(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "缺少 path 参数"
	}
	target := raw
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "解析 HOME 失败"
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~"))
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", "解析路径失败"
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "文件不存在或不可读"
	}
	if !info.Mode().IsRegular() {
		return "", "不是普通文件"
	}
	return abs, ""
}

// handleFilePreview 只读返回一个本机文件的内容供侧边预览。
// GET /api/files/preview?path=/abs/a.md（支持 ~/ 前缀）。kind：text（content 为
// 文本，.md 由前端再走 markdown 渲染）| image（dataUrl 内联）| binary（无法
// 预览，message 说明原因）；text 超限时 content 只含前 filePreviewTextMax 字节。
func (s *Server) handleFilePreview(ctx *vhttp.Context) error {
	abs, errMsg := resolveFilePath(ctx.GinContext().Query("path"))
	if errMsg != "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": errMsg + "，无法预览"})
	}
	info, err := os.Stat(abs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "文件不存在或不可读"})
	}
	f, err := os.Open(abs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "文件不可读"})
	}
	defer f.Close()

	resp := map[string]any{
		"path":    abs,
		"name":    filepath.Base(abs),
		"size":    info.Size(),
		"modTime": info.ModTime().Format(time.RFC3339),
		// canOpen：能否用系统默认应用打开（只有本机回环请求才行——远程访问
		// 在服务器上打开文件对远端用户没意义，那边给「下载到本地」）。
		"canOpen": localRequest(ctx.GinContext().Request),
	}
	// 类型判定：先嗅探 magic number 识别图片；其余看内容里有没有 NUL 字节——
	// 有即二进制（UTF-16 等个别文本会误判成 binary，宁可不渲染也不乱渲染）。
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	sniff := http.DetectContentType(head[:n])
	// 嗅探消耗了头部字节，后续读取从头再来。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取文件失败"})
	}
	if strings.HasPrefix(sniff, "image/") {
		resp["mime"] = sniff
		if info.Size() > filePreviewImageMax {
			resp["kind"] = "binary"
			resp["message"] = "图片超过 8MB，暂不支持预览"
			return ctx.JsonResponse(vhttp.Codes.Success, resp)
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取文件失败"})
		}
		resp["kind"] = "image"
		resp["dataUrl"] = "data:" + sniff + ";base64," + base64.StdEncoding.EncodeToString(data)
		return ctx.JsonResponse(vhttp.Codes.Success, resp)
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		resp["kind"] = "binary"
		resp["message"] = "二进制文件，暂不支持预览"
		return ctx.JsonResponse(vhttp.Codes.Success, resp)
	}
	data, err := io.ReadAll(io.LimitReader(f, filePreviewTextMax+1))
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取文件失败"})
	}
	resp["kind"] = "text"
	resp["mime"] = mimeLabel(filepath.Ext(abs), sniff)
	// 多读 1 字节只为判断截断，回传时裁回上限。
	if int64(len(data)) > filePreviewTextMax {
		data = data[:filePreviewTextMax]
	}
	resp["content"] = string(data)
	resp["truncated"] = info.Size() > filePreviewTextMax
	return ctx.JsonResponse(vhttp.Codes.Success, resp)
}

// mimeLabel 给前端一个展示/判断用的类型标签：扩展名优先（DetectContentType
// 不认识 .md/.yaml 等，Go 内置 mime 表在部分系统也缺这些），认不出再回落嗅探。
func mimeLabel(ext, sniff string) string {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".yaml", ".yml":
		return "application/yaml"
	}
	if ext != "" {
		if m := mime.TypeByExtension(strings.ToLower(ext)); m != "" {
			return m
		}
	}
	return sniff
}

// handleFileOpen 用系统默认应用打开一个本机文件（预览不了的那类：zip、pdf、
// 二进制…）。POST /api/files/open {"path":"…"}
//
// 只允许本机回环请求：远程访问时「在服务器上打开文件」对远端用户没有意义，
// 反而等于给远端开了个「在本机执行程序」的入口——那边给「下载到本地」就够。
func (s *Server) handleFileOpen(ctx *vhttp.Context) error {
	if !localRequest(ctx.GinContext().Request) {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{
			"error": "远程访问不能在本机打开文件，请改用「下载到本地」",
		})
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	abs, errMsg := resolveFilePath(req.Path)
	if errMsg != "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": errMsg})
	}
	if err := openWithDefaultApp(abs); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest,
			map[string]string{"error": "用系统默认应用打开失败: " + err.Error()})
	}
	return ctx.JsonResponse(vhttp.Codes.Success,
		map[string]string{"message": "已用系统默认应用打开 " + filepath.Base(abs)})
}

// openWithDefaultApp 把文件交给系统默认应用（macOS 的 open / Linux 的
// xdg-open / Windows 的 start）。不经 shell，路径原样传参。
// 做成变量：测试里替换掉，免得真去打开一个应用。
var openWithDefaultApp = func(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		return fmt.Errorf("当前系统不支持用默认应用打开")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等它退出：有的应用会一直开着（编辑器、播放器），等下去等于挂住。
	go func() { _ = cmd.Wait() }()
	return nil
}

// handleFileDownload 流式下载一个本机文件（手机端文件面板把文件拉到手机用）：
// GET /api/files/download?path=/abs/a.md（支持 ~/ 前缀）。鉴权走 wrapMedia——
// 浏览器 <a download> 带不了自定义头，token 只能放查询参数（同附件回读的取舍）。
// ServeContent 自带 Range/If-Modified-Since 支持，类型按扩展名推断。
func (s *Server) handleFileDownload(ctx *vhttp.Context) error {
	abs, errMsg := resolveFilePath(ctx.GinContext().Query("path"))
	if errMsg != "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": errMsg + "，无法下载"})
	}
	f, err := os.Open(abs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "文件不可读"})
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "读取文件失败"})
	}
	name := filepath.Base(abs)
	// 中文等非 ASCII 文件名按 RFC 5987 走 filename* 传原值；filename 兜底
	// ASCII（只留可打印安全字符），老旧客户端与下载器不至于拿到空文件名。
	var ascii strings.Builder
	for _, r := range name {
		if r > 32 && r < 127 && r != '"' && r != '\\' && r != '\'' {
			ascii.WriteRune(r)
		} else {
			ascii.WriteByte('_')
		}
	}
	ctx.GinContext().Header("Content-Disposition",
		`attachment; filename="`+ascii.String()+`"; filename*=UTF-8''`+url.PathEscape(name))
	http.ServeContent(ctx.GinContext().Writer, ctx.GinContext().Request, name, info.ModTime(), f)
	return nil
}
