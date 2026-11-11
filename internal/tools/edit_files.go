package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/logx"
)

// EditFilesTool 批量精确编辑：一次改多个文件，全成或全不成。
//
// 为什么要有这个：edit_file 一次只能改一处，且 old 必须唯一匹配。真实任务
// （改一个函数要同时动三四个文件、或同一文件里改多处）用 edit_file 就是
// 一轮轮工具调用，每次都要重新读文件，慢且容易改漏。
//
// 语义定死三条，避免"改了一半"的烂摊子：
//  1. old 在目标文件里必须恰好出现一次——多处匹配就整批拒绝，不猜；
//  2. 先全部校验通过再落盘，不做部分写入；
//  3. 落盘中途失败会回滚已写的文件（新建的删掉，改过的还原）。

const (
	editFilesMaxEdits = 50
)

// EditFilesTool 批量编辑文件。
type EditFilesTool struct {
	FileTools
	Approver *approval.Queue
}

func (t *EditFilesTool) Name() string { return "edit_files" }

func (t *EditFilesTool) Description() string {
	return "批量精确编辑文件：edits 是数组，每项 {path, old, new}。old 在对应文件里必须恰好出现一次" +
		"（多处匹配整批拒绝），整批要么全改成功要么完全不改。适合一次动多个文件或同一文件多处改动。" +
		"confirm 审批模式下需用户在网页批准后才会执行。"
}

func (t *EditFilesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"edits": map[string]any{
				"type":        "array",
				"description": "要做的编辑列表，每项 {path, old, new}",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "目标文件路径"},
						"old":  map[string]any{"type": "string", "description": "要被替换的原文（须在文件中唯一）"},
						"new":  map[string]any{"type": "string", "description": "替换后的新文本"},
					},
					"required": []string{"path", "old", "new"},
				},
			},
		},
		"required": []string{"edits"},
	}
}

// fileEdit 一条待执行的编辑。
type fileEdit struct {
	path string // 解析后的绝对路径
	old  string
	new  string
	// 内容为编辑后的新正文（校验通过后才有值）
	updated string
	existed bool // 落盘前文件是否已存在（决定回滚时删还是还原）
}

func (t *EditFilesTool) Call(ctx context.Context, args map[string]any) (string, error) {
	raw, _ := args["edits"].([]any)
	if len(raw) == 0 {
		return "", fmt.Errorf("缺少参数 edits（数组，每项 {path, old, new}）")
	}
	if len(raw) > editFilesMaxEdits {
		return "", fmt.Errorf("一次最多 %d 处编辑，收到 %d 处；拆成多次调用", editFilesMaxEdits, len(raw))
	}

	edits := make([]*fileEdit, 0, len(raw))
	seen := map[string]bool{}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("edits[%d] 不是对象（应为 {path, old, new}）", i)
		}
		path, _ := m["path"].(string)
		oldStr, _ := m["old"].(string)
		newStr, _ := m["new"].(string)
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("edits[%d] 缺少 path", i)
		}
		if oldStr == "" {
			return "", fmt.Errorf("edits[%d] 缺少 old", i)
		}
		real, err := t.resolve(ctx, path)
		if err != nil {
			return "", fmt.Errorf("edits[%d]: %w", i, err)
		}
		if seen[real] {
			return "", fmt.Errorf("edits[%d]: %s 在同一批里出现多次；请合并成一次编辑", i, path)
		}
		seen[real] = true

		st, err := os.Stat(real)
		if err != nil {
			return "", fmt.Errorf("edits[%d]: 读取 %s 失败: %w（edit_files 只能改已存在的文件，新建请用 write_file）", i, path, err)
		}
		if st.IsDir() {
			return "", fmt.Errorf("edits[%d]: %s 是目录", i, path)
		}
		rawContent, err := os.ReadFile(real)
		if err != nil {
			return "", fmt.Errorf("edits[%d]: 读取 %s 失败: %w", i, path, err)
		}
		if isBinary(rawContent) {
			return "", fmt.Errorf("edits[%d]: %s 是二进制文件，无法编辑", i, path)
		}
		content := string(rawContent)
		n := strings.Count(content, oldStr)
		switch {
		case n == 0:
			return "", fmt.Errorf("edits[%d]: old 在 %s 里没找到，整批未做任何修改", i, path)
		case n > 1:
			return "", fmt.Errorf("edits[%d]: old 在 %s 里出现 %d 次（要求唯一匹配），整批未做任何修改；请扩大上下文使其唯一", i, path, n)
		}
		edits = append(edits, &fileEdit{
			path:    real,
			old:     oldStr,
			new:     newStr,
			updated: strings.Replace(content, oldStr, newStr, 1),
			existed: true,
		})
	}

	// 审批按整批投一张卡：用户一次看清"这轮要改哪几个文件"，比逐文件弹 N 张卡强。
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 处编辑，涉及 %d 个文件：\n", len(edits), len(edits))
	for _, e := range edits {
		fmt.Fprintf(&b, "- %s\n", e.path)
	}
	detail := b.String() + logx.Truncate("编辑内容详情见 diff。", 100)
	if err := requestWriteApproval(ctx, t.Approver, t.Name(), strings.Join(pathsOf(edits), ", "), detail); err != nil {
		return "", err
	}

	// 落盘；中途失败就把已写的回滚，绝不留下改一半的状态。
	var written []string
	for _, e := range edits {
		if err := backupAndWrite(e.path, e.updated); err != nil {
			rollbackWrites(written)
			return "", fmt.Errorf("写入 %s 失败: %w（已回滚本批前面写过的文件）", e.path, err)
		}
		written = append(written, e.path)
	}
	var summary strings.Builder
	summary.WriteString(fmt.Sprintf("已修改 %d 个文件：\n", len(edits)))
	for _, e := range edits {
		summary.WriteString("- " + e.path + "\n")
	}
	return summary.String(), nil
}

// pathsOf 取编辑涉及的文件路径（审批摘要用）。
func pathsOf(edits []*fileEdit) []string {
	out := make([]string, 0, len(edits))
	for _, e := range edits {
		out = append(out, e.path)
	}
	return out
}

// rollbackWrites 回滚已写入的文件：备份在 .bak 的还原回去，没有备份（新建文件）的删掉。
func rollbackWrites(paths []string) {
	for i := len(paths) - 1; i >= 0; i-- {
		p := paths[i]
		if _, err := os.Stat(p + ".bak"); err == nil {
			_ = os.Rename(p+".bak", p)
			continue
		}
		_ = os.Remove(p)
	}
}
