package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/logx"
	"github.com/capyflow/friday/internal/trace"
)

// writeMaxBytes 单次写入内容上限（32KB，与 exec 输出上限对齐，防止撑爆内存）。
const writeMaxBytes = 32 * 1024

// previewLen 审批详情里内容预览的长度（rune）。
const previewLen = 800

// resolveWrite 把要写入的路径解析为白名单内的绝对路径：目录必须存在且在白名单内，
// 允许目标文件不存在（新建文件）。若目标已存在，解析符号链接后的真实路径
// 也必须在白名单内（防止用软链把内容写到白名单之外）。
func (t *FileTools) resolveWrite(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("缺少参数 path")
	}
	if !filepath.IsAbs(path) {
		if len(t.AllowDirs) == 0 {
			return "", fmt.Errorf("未配置可访问目录（file.allowDirs）")
		}
		path = filepath.Join(t.AllowDirs[0], path)
	}
	dir, base := filepath.Split(filepath.Clean(path))
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("非法的目标路径: %s", path)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("目录不存在或无法访问: %s", dir)
	}
	if abs, err := filepath.Abs(realDir); err == nil {
		realDir = abs
	}
	if !t.inAllowDirs(realDir) {
		return "", fmt.Errorf("路径超出允许范围（file.allowDirs）: %s", path)
	}
	final := filepath.Join(realDir, base)
	if st, err := os.Lstat(final); err == nil && st.Mode()&os.ModeSymlink != 0 {
		// 目标本身是软链：写入会穿透到链目标，校验链目标也在白名单内。
		if real, err := filepath.EvalSymlinks(final); err != nil || !t.inAllowDirs(real) {
			return "", fmt.Errorf("目标为指向白名单之外的软链，拒绝写入: %s", path)
		}
	}
	return final, nil
}

// requestWriteApproval 在 confirm 模式（且未开启编辑免审批）下为文件写入/编辑
// 发起审批；auto/off 模式与 acceptEdits/fullAccess 权限模式直接放行。
func requestWriteApproval(ctx context.Context, q *approval.Queue, tool, path, detail string) error {
	if q == nil || !q.InternalApprovalRequired() {
		return nil
	}
	trace.Record(ctx, "approval", "%s 请求写入 %s，等待网页审批（超时 %s）", tool, path, q.Timeout())
	if err := q.Request(tool, fmt.Sprintf("%s：%s", tool, path), detail); err != nil {
		trace.Record(ctx, "approval", "%s %s 未获批准：%v", tool, path, err)
		return err
	}
	trace.Record(ctx, "approval", "%s %s 已获批准，执行写入", tool, path)
	return nil
}

// backupAndWrite 写入前把原文件备份为 <path>.bak（原子改名），写失败自动还原。
func backupAndWrite(real, content string) error {
	bak := real + ".bak"
	if _, err := os.Stat(real); err == nil {
		if err := os.Rename(real, bak); err != nil {
			return fmt.Errorf("备份原文件失败: %w", err)
		}
		if err := os.WriteFile(real, []byte(content), 0o644); err != nil {
			if rbErr := os.Rename(bak, real); rbErr != nil {
				return fmt.Errorf("写入失败且还原失败（原内容在 %s）: %w", bak, err)
			}
			return fmt.Errorf("写入失败（已还原原文件）: %w", err)
		}
		return nil
	}
	return os.WriteFile(real, []byte(content), 0o644)
}

// WriteFileTool 写入白名单目录内的文件（覆盖写，自动备份原文件为 .bak）。
type WriteFileTool struct {
	FileTools
	Approver *approval.Queue
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "把内容写入文件（覆盖写；原文件自动备份为 .bak）。path 为白名单内的相对或绝对路径，" +
		"文件可不存在（新建）。confirm 审批模式下需用户在网页批准后才会执行。"
}

func (t *WriteFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "目标文件路径"},
			"content": map[string]any{"type": "string", "description": "要写入的完整内容"},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if len(content) > writeMaxBytes {
		return "", fmt.Errorf("内容过大（%d 字节，上限 %d）", len(content), writeMaxBytes)
	}
	real, err := t.resolveWrite(path)
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("路径: %s\n写入内容（前 %d 字符）:\n%s", real, previewLen, logx.Truncate(content, previewLen))
	if err := requestWriteApproval(ctx, t.Approver, t.Name(), real, detail); err != nil {
		return "", err
	}
	if err := backupAndWrite(real, content); err != nil {
		return "", err
	}
	return fmt.Sprintf("已写入 %s（%d 字节）", real, len(content)), nil
}

// EditFileTool 精确替换文件内容：old 必须在文件中恰好出现一次，防止误改。
type EditFileTool struct {
	FileTools
	Approver *approval.Queue
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Description() string {
	return "编辑文件：把文件中的 old 精确替换为 new。old 必须在文件中恰好出现一次" +
		"（多处匹配会被拒绝，防止误改）。编辑前自动备份原文件为 .bak。" +
		"confirm 审批模式下需用户在网页批准后才会执行。"
}

func (t *EditFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "目标文件路径"},
			"old":  map[string]any{"type": "string", "description": "要被替换的原文（须在文件中唯一）"},
			"new":  map[string]any{"type": "string", "description": "替换后的新文本"},
		},
		"required": []string{"path", "old", "new"},
	}
}

func (t *EditFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old"].(string)
	newStr, _ := args["new"].(string)
	if oldStr == "" {
		return "", fmt.Errorf("缺少参数 old")
	}
	real, err := t.resolve(path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	if isBinary(raw) {
		return "", fmt.Errorf("%s 是二进制文件，无法编辑", filepath.Base(real))
	}
	content := string(raw)
	n := strings.Count(content, oldStr)
	switch {
	case n == 0:
		return "", fmt.Errorf("old 在文件中未找到，未做任何修改")
	case n > 1:
		return "", fmt.Errorf("old 在文件中出现 %d 次（要求唯一匹配），未做任何修改；请扩大上下文使其唯一", n)
	}
	updated := strings.Replace(content, oldStr, newStr, 1)
	detail := fmt.Sprintf("路径: %s\n替换前:\n%s\n替换后:\n%s",
		real, logx.Truncate(oldStr, previewLen), logx.Truncate(newStr, previewLen))
	if err := requestWriteApproval(ctx, t.Approver, t.Name(), real, detail); err != nil {
		return "", err
	}
	if err := backupAndWrite(real, updated); err != nil {
		return "", err
	}
	return fmt.Sprintf("已编辑 %s（替换 1 处）", real), nil
}
