package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/capyflow/friday/internal/workdir"
)

// toInt 读取整数参数，缺失或非法时返回 def。
func toInt(v any, def int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
	}
	return def
}

// FileTools 是文件类工具的公共底座：所有路径必须落在允许根内（解析符号链接后校验），
// 相对路径基于第一个允许根解析。允许根 = 这次请求的会话工作目录（工作模式里建会话时
// 点选的那个，走 ctx 传进来）+ 配置的 file.allowDirs：会话目录是用户自己选的，读写
// 这个项目就是这条会话的目的，所以它算一个根；没带会话目录时与以往完全一致。
type FileTools struct {
	AllowDirs []string
}

// roots 返回本次调用的允许根，会话工作目录排在最前（它就是相对路径的基准）。
func (t *FileTools) roots(ctx context.Context) []string {
	if dir := workdir.From(ctx); dir != "" {
		return append([]string{dir}, t.AllowDirs...)
	}
	return t.AllowDirs
}

const (
	readMaxLines   = 400 // 单次最多返回行数
	readMaxBytes   = 32 * 1024
	fileMaxSize    = 10 << 20 // 超过 10MB 直接拒绝读取
	listMaxEntries = 200
)

// resolve 把用户给的路径解析为允许根内的真实绝对路径。
func (t *FileTools) resolve(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("缺少参数 path")
	}
	roots := t.roots(ctx)
	if !filepath.IsAbs(path) {
		if len(roots) == 0 {
			return "", fmt.Errorf("未配置可访问目录（file.allowDirs，或给这条会话选一个工作目录）")
		}
		path = filepath.Join(roots[0], path)
	}
	path = filepath.Clean(path)

	// 解析符号链接后的真实路径必须仍在允许根内，防止软链逃逸。
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("路径不存在或无法访问: %s", path)
	}
	// 相对输入解析后仍是相对形式，统一转为绝对路径再比对允许根
	if abs, err := filepath.Abs(real); err == nil {
		real = abs
	}
	if !t.inAllowDirs(ctx, real) {
		return "", fmt.Errorf("路径超出允许范围（不在这条会话的工作目录或 file.allowDirs 里）: %s", path)
	}
	return real, nil
}

func (t *FileTools) inAllowDirs(ctx context.Context, real string) bool {
	for _, dir := range t.roots(ctx) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if realDir, err := filepath.EvalSymlinks(abs); err == nil {
			if real == realDir || strings.HasPrefix(real, realDir+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// isBinary 通过前 8KB 是否含 NUL 字节粗判二进制文件。
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// ReadFileTool 读取白名单目录内的文本文件（支持 offset/limit 分段与 PDF）。
type ReadFileTool struct {
	FileTools
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "读取文本文件内容（带行号）。path 为允许目录内的相对或绝对路径（相对路径按本次会话的工作目录解析）；" +
		"offset 为起始行号（默认 1），" +
		"limit 为行数（默认 400）。PDF 文件按页读取，此时 offset/limit 表示起始页与页数。" +
		"内容超长会截断，用 offset 续读。"
}

func (t *ReadFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "文件路径"},
			"offset": map[string]any{"type": "integer", "description": "起始行号/PDF 起始页，默认 1"},
			"limit":  map[string]any{"type": "integer", "description": "行数/PDF 页数，默认 400/全部"},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	offset := toInt(args["offset"], 1)
	limit := toInt(args["limit"], 0)
	if offset < 1 {
		offset = 1
	}

	real, err := t.resolve(ctx, path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录，请用 list_files 列出内容", path)
	}
	if st.Size() > fileMaxSize {
		return "", fmt.Errorf("文件过大（%d 字节，上限 10MB）", st.Size())
	}

	// PDF：按页抽取文本
	if strings.EqualFold(filepath.Ext(real), ".pdf") {
		return t.readPDF(real, offset, limit)
	}

	raw, err := os.ReadFile(real)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if isBinary(raw) {
		return "", fmt.Errorf("%s 是二进制文件，无法按文本读取", filepath.Base(real))
	}

	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), "\n")
	total := len(lines)
	if limit <= 0 {
		limit = readMaxLines
	}
	if limit > readMaxLines {
		limit = readMaxLines
	}
	start := offset - 1
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	// 字节上限：超出则减少行数
	var b strings.Builder
	shown := 0
	for i := start; i < end; i++ {
		line := fmt.Sprintf("%6d| %s", i+1, lines[i])
		if b.Len()+len(line) > readMaxBytes {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		shown++
	}
	head := fmt.Sprintf("文件: %s（共 %d 行，%d 字节）\n显示第 %d–%d 行\n",
		real, total, st.Size(), start+1, start+shown)
	if end < total {
		head += fmt.Sprintf("[未完] 用 offset=%d 续读\n", end+1)
	}
	return head + b.String(), nil
}

// ListFilesTool 列出白名单目录内的文件与子目录。
type ListFilesTool struct {
	FileTools
}

func (t *ListFilesTool) Name() string { return "list_files" }

func (t *ListFilesTool) Description() string {
	return "列出目录下的文件与子目录（含大小）。dir 留空时列出本次会话的工作目录（这条会话没指定就列默认白名单目录）。用于寻找要读取的文件。"
}

func (t *ListFilesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dir": map[string]any{"type": "string", "description": "目录路径，留空为默认目录"},
		},
	}
}

func (t *ListFilesTool) Call(ctx context.Context, args map[string]any) (string, error) {
	dir, _ := args["dir"].(string)
	if roots := t.roots(ctx); strings.TrimSpace(dir) == "" && len(roots) > 0 {
		dir = roots[0] // 留空就列这条会话的工作目录（没指定才是配置的白名单首项）
	}
	real, err := t.resolve(ctx, dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", dir)
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	var b strings.Builder
	fmt.Fprintf(&b, "目录: %s（共 %d 项", real, len(entries))
	if len(entries) > listMaxEntries {
		fmt.Fprintf(&b, "，显示前 %d 项", listMaxEntries)
	}
	b.WriteString("）\n")
	for i, e := range entries {
		if i >= listMaxEntries {
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "[目录] %s/\n", e.Name())
			continue
		}
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%8d %s\n", info.Size(), e.Name())
		}
	}
	return b.String(), nil
}
