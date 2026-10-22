// files.go 是文件预览接口：聊天/智能体时间线里输出的本机文件路径，前端渲染成
// 可点 chip，点开后由这里只读读取内容，侧边抽屉按类型渲染。
// 信任档说明：服务仅监听本机、接口需登录，可读范围与 agent 自身读工具一致
// （本机任意可读普通文件），不做目录白名单；拒绝目录与非普通文件。
package admin

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

// 预览限额：文本最多回传前 512KB（超出带 truncated 标记），图片最大 8MB。
const (
	filePreviewTextMax  = 512 << 10
	filePreviewImageMax = 8 << 20
)

// handleFilePreview 只读返回一个本机文件的内容供侧边预览。
// GET /api/files/preview?path=/abs/a.md（支持 ~/ 前缀）。kind：text（content 为
// 文本，.md 由前端再走 markdown 渲染）| image（dataUrl 内联）| binary（无法
// 预览，message 说明原因）；text 超限时 content 只含前 filePreviewTextMax 字节。
func (s *Server) handleFilePreview(ctx *vhttp.Context) error {
	raw := strings.TrimSpace(ctx.GinContext().Query("path"))
	if raw == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "缺少 path 参数"})
	}
	target := raw
	if target == "~" || strings.HasPrefix(target, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "解析 HOME 失败"})
		}
		target = filepath.Join(home, strings.TrimPrefix(target, "~"))
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "解析路径失败"})
	}
	info, err := os.Stat(abs)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "文件不存在或不可读"})
	}
	if !info.Mode().IsRegular() {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "不是普通文件，无法预览"})
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
