package tools

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

// readPDF 按页抽取文本型 PDF 的文本。offset 为起始页（1-based），limit 为页数
// （<=0 表示到最后一页）。扫描件（无文本层）与加密文件会得到明确报错或空页提示。
func (t *ReadFileTool) readPDF(path string, offset, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("读取失败: %w", err)
	}
	reader, err := pdf.NewReader(f, st.Size())
	if err != nil {
		return "", fmt.Errorf("PDF 解析失败（可能为扫描件或加密文件）: %w", err)
	}
	pages := reader.NumPage()
	if pages == 0 {
		return "", fmt.Errorf("PDF 没有可读取的页面")
	}
	if offset < 1 {
		offset = 1
	}
	if offset > pages {
		return "", fmt.Errorf("起始页 %d 超出范围（共 %d 页）", offset, pages)
	}
	end := pages
	if limit > 0 && offset-1+limit < pages {
		end = offset - 1 + limit
	}

	var b strings.Builder
	fmt.Fprintf(&b, "PDF: %s（共 %d 页）\n显示第 %d–%d 页\n", path, pages, offset, end)
	if end < pages {
		fmt.Fprintf(&b, "[未完] 用 offset=%d 续读\n", end+1)
	}
	for p := offset; p <= end; p++ {
		fmt.Fprintf(&b, "\n--- 第 %d 页 ---\n", p)
		page := reader.Page(p)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			fmt.Fprintf(&b, "(本页文本抽取失败: %v)\n", err)
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			text = "(本页无文本层，可能是扫描图片)"
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String(), nil
}
