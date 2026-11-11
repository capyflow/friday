package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 检索工具（search_code / find_files）。
//
// 为什么不用 list_files + read_file 凑：那是"人翻目录"的路子，仓库一大就废。
// 编码 agent 的第一件事永远是"那段代码在哪"，所以这里补上按内容搜与按文件名
// 匹配两个入口。
//
// 为什么自己实现而不调 rg 二进制：Friday 是单二进制、自托管的，用户的机器上
// 不一定装了 ripgrep（装了就多一条外部依赖，没装就是工具直接不可用）。Go 正则
// + 目录遍历够用，且不引入任何新依赖。

const (
	searchDefaultResults = 80  // 默认最多回多少条命中
	searchMaxResults     = 500 // 上限，防止模型一次要 10000 条把上下文灌满
	searchMaxFileBytes   = 2 << 20
	searchMaxOutputBytes = 24 << 10
	searchMaxLineRunes   = 240 // 单行最多带多少字符进结果
	searchMaxPathRunes   = 300
)

// searchSkipDirs 遍历时跳过的目录：依赖目录与构建产物，扫进去只会拖慢并淹没结果。
var searchSkipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true,
	"__pycache__": true, ".idea": true, ".vscode": true, ".pytest_cache": true,
	".mypy_cache": true, ".ruff_cache": true, ".tox": true, ".gradle": true,
	"dist": true, "build": true, "out": true, "target": true, ".next": true,
	".nuxt": true, ".svelte-kit": true, ".cache": true, "coverage": true,
	"DerivedData": true, "Pods": true,
}

// strArg 取字符串参数（缺失或非字符串返回空串）。
func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// boolArg 取布尔参数（JSON 里可能是 bool 也可能是字符串 "true"）。
func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	case float64:
		return v != 0
	default:
		return false
	}
}

// resolveSearchRoot 定位本次检索的根目录：path 留空就是这条会话的工作目录
// （没指定就回落到配置的白名单首项），给了就按允许根解析。
func (t *FileTools) resolveSearchRoot(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		roots := t.roots(ctx)
		if len(roots) == 0 {
			return "", fmt.Errorf("未配置可访问目录（file.allowDirs，或给这条会话选一个工作目录）")
		}
		abs, err := filepath.Abs(roots[0])
		if err != nil {
			return "", fmt.Errorf("工作目录无法解析: %w", err)
		}
		return abs, nil
	}
	return t.resolve(ctx, path)
}

// walkSearchFiles 遍历 root 下的普通文件（跳过依赖与构建目录）。fn 返回 false 时停止。
// 读不进的目录直接跳过而不是中断整场检索：一场检索因为一个无权限目录全废掉
// 是很糟的体验，宁可少几个结果。
func walkSearchFiles(root string, fn func(path string) bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if searchSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 软链、管道、设备文件一律不碰
		}
		if !fn(p) {
			return fs.SkipAll
		}
		return nil
	})
}

// globBody 把 glob 模式翻成正则体（不含锚点）：** 匹配任意层目录，* 匹配任意字符
// （不跨 /），? 匹配单个字符，其余按字面量转义。
func globBody(pattern string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// ** 跨目录层：把 "**/" 整体吃掉（可匹配零层或多层）
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
					continue
				}
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	return b.String(), nil
}

// nameMatcher 生成文件名过滤规则：模式里带 "/" 就按相对路径匹配，否则按文件名匹配
// （"*.go" 这种直觉写法不必写成 "**/*.go"）。
func nameMatcher(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	body, err := globBody(pattern)
	if err != nil {
		return nil, fmt.Errorf("文件匹配模式无效 %q: %w", pattern, err)
	}
	return regexp.Compile("^" + body + "$")
}

// globToRegex 编译一条锚定的 glob 规则。
func globToRegex(pattern string) (*regexp.Regexp, error) {
	body, err := globBody(pattern)
	if err != nil {
		return nil, err
	}
	return regexp.Compile("^" + body + "$")
}

// matchName 判断一个文件是否命中过滤规则。
func matchName(re *regexp.Regexp, pattern, abs, root string) bool {
	if re == nil {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return re.MatchString(filepath.Base(abs))
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return re.MatchString(filepath.Base(abs))
	}
	return re.MatchString(filepath.ToSlash(rel))
}

// SearchCodeTool 按内容搜索代码：正则（RE2）或字面量，命中行带行号。
type SearchCodeTool struct {
	FileTools
}

func (t *SearchCodeTool) Name() string { return "search_code" }

func (t *SearchCodeTool) Description() string {
	return "在允许目录内按正则搜索文件内容，返回 路径:行号: 匹配内容。改代码前先用它定位，" +
		"不要凭猜测改文件。pattern 是正则（Go 语法），要按字面量搜就传 fixed=true。" +
		"path 留空=本次会话的工作目录；glob 可按文件名过滤（如 *.go、**/*_test.go）。"
}

func (t *SearchCodeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "要搜索的正则（Go 语法）或关键词"},
			"path":        map[string]any{"type": "string", "description": "搜索根目录，留空为本次会话的工作目录"},
			"glob":        map[string]any{"type": "string", "description": "只搜匹配该模式的文件，如 *.go"},
			"ignore_case": map[string]any{"type": "boolean", "description": "忽略大小写，默认 false"},
			"fixed":       map[string]any{"type": "boolean", "description": "把 pattern 当字面量（不当作正则），默认 false"},
			"max_results": map[string]any{"type": "integer", "description": "最多返回多少条命中，默认 80"},
		},
		"required": []string{"pattern"},
	}
}

func (t *SearchCodeTool) Call(ctx context.Context, args map[string]any) (string, error) {
	pattern := strArg(args, "pattern")
	if pattern == "" {
		return "", fmt.Errorf("缺少参数 pattern（要搜的正则或关键词）")
	}
	root, err := t.resolveSearchRoot(ctx, strArg(args, "path"))
	if err != nil {
		return "", err
	}
	globPat := strArg(args, "glob")
	nameRe, err := nameMatcher(globPat)
	if err != nil {
		return "", err
	}
	expr := pattern
	if boolArg(args, "fixed") {
		expr = regexp.QuoteMeta(pattern)
	}
	if boolArg(args, "ignore_case") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("正则无效: %w（要按字面量搜就传 fixed=true）", err)
	}
	max := toInt(args["max_results"], searchDefaultResults)
	if max <= 0 {
		max = searchDefaultResults
	}
	if max > searchMaxResults {
		max = searchMaxResults
	}

	type hit struct {
		loc  string
		text string
	}
	var hits []hit
	truncated := false
	scanned := 0
	_ = walkSearchFiles(root, func(path string) bool {
		if len(hits) >= max {
			truncated = true
			return false
		}
		if !matchName(nameRe, globPat, path, root) {
			return true
		}
		st, err := os.Stat(path)
		if err != nil || st.Size() > searchMaxFileBytes {
			return true
		}
		raw, err := os.ReadFile(path)
		if err != nil || isBinary(raw) {
			return true
		}
		scanned++
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			text := strings.TrimRight(line, "\t ")
			text = truncateRunes(text, searchMaxLineRunes)
			hits = append(hits, hit{loc: fmt.Sprintf("%s:%d", rel, i+1), text: text})
			if len(hits) >= max {
				truncated = true
				return false
			}
		}
		return true
	})

	if len(hits) == 0 {
		return fmt.Sprintf("在 %s 下没有匹配 %q 的内容（扫了 %d 个文件）", root, pattern, scanned), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "匹配 %d 处（模式: %s，根目录: %s，扫了 %d 个文件）\n", len(hits), pattern, root, scanned)
	for _, h := range hits {
		line := h.loc + ": " + h.text
		if b.Len()+len(line)+1 > searchMaxOutputBytes {
			b.WriteString("\n[输出已截断：命中过多，请缩小 path 或加上 glob 过滤]\n")
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("[命中数已达上限，未继续扫描：需要更多请缩小范围或调大 max_results]\n")
	}
	return b.String(), nil
}

// FindFilesTool 按文件名模式找文件（glob）。
type FindFilesTool struct {
	FileTools
}

func (t *FindFilesTool) Name() string { return "find_files" }

func (t *FindFilesTool) Description() string {
	return "按文件名模式查找文件（glob，如 *.go、**/*_test.go、cmd/**/main.go），" +
		"返回相对路径列表。path 留空=本次会话的工作目录。不知道文件叫什么时先用它摸清结构。"
}

func (t *FindFilesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "文件名匹配模式（glob），如 *.go"},
			"path":        map[string]any{"type": "string", "description": "查找根目录，留空为本次会话的工作目录"},
			"max_results": map[string]any{"type": "integer", "description": "最多返回多少个，默认 200"},
		},
		"required": []string{"pattern"},
	}
}

func (t *FindFilesTool) Call(ctx context.Context, args map[string]any) (string, error) {
	pattern := strArg(args, "pattern")
	if pattern == "" {
		return "", fmt.Errorf("缺少参数 pattern（文件名匹配模式，如 *.go）")
	}
	root, err := t.resolveSearchRoot(ctx, strArg(args, "path"))
	if err != nil {
		return "", err
	}
	re, err := globToRegex(pattern)
	if err != nil {
		return "", fmt.Errorf("文件匹配模式无效 %q: %w", pattern, err)
	}
	onlyBase := !strings.Contains(pattern, "/")
	max := toInt(args["max_results"], 200)
	if max <= 0 {
		max = 200
	}
	if max > searchMaxResults*2 {
		max = searchMaxResults * 2
	}

	var paths []string
	truncated := false
	_ = walkSearchFiles(root, func(path string) bool {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		target := rel
		if onlyBase {
			target = filepath.Base(rel)
		}
		if !re.MatchString(target) {
			return true
		}
		if len(paths) >= max {
			truncated = true
			return false
		}
		paths = append(paths, rel)
		return true
	})
	sort.Strings(paths)
	if len(paths) == 0 {
		return fmt.Sprintf("在 %s 下没有匹配 %q 的文件", root, pattern), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "找到 %d 个文件（模式: %s，根目录: %s）\n", len(paths), pattern, root)
	for _, p := range paths {
		line := p
		if b.Len()+len(line)+1 > searchMaxOutputBytes {
			b.WriteString("\n[输出已截断]\n")
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("[已达返回上限，未继续遍历]\n")
	}
	return b.String(), nil
}
