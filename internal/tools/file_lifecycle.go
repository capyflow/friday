package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// 文件生命周期工具（move_file / delete_file）。
//
// 为什么单独开这两个：内置 agent 原本只能靠 exec_command 的 mv / rm 改文件名或
// 删文件——那要走审批、走命令黑名单，rm -rf 还被兜底挡掉，agent 在真实编码
// 任务里经常因此卡住。这两个工具把"改名/移动/删除"变成第一方工具：路径解析、
// 允许根校验、审批都复用文件工具那一套，行为跟 read_file/write_file 对齐。
//
// 删除一律不硬删：文件被移进回收站目录（系统临时目录下），可以随时移回。
// 硬删不可逆，而 agent 删错文件时"找回来"的能力比"省几 KB 磁盘"重要得多。

// trashDirName 回收站目录名（挂在系统临时目录底下）。
const trashDirName = "friday-trash"

// MoveFileTool 移动/改名文件：把 from 移到 to（to 的父目录必须已存在）。
type MoveFileTool struct {
	FileTools
	Approver *approval.Queue
}

func (t *MoveFileTool) Name() string { return "move_file" }

func (t *MoveFileTool) Description() string {
	return "移动或改名文件：把 from 移到 to。from 必须存在，to 的父目录必须已存在，" +
		"两者都要在允许目录内。目标已存在时默认拒绝（不改动任何文件），" +
		"要覆盖就传 overwrite=true：旧目标会先改名成 <to>.bak 备份，再把源移过去。" +
		"confirm 审批模式下需用户在网页批准后才会执行。"
}

func (t *MoveFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"from":      map[string]any{"type": "string", "description": "源文件路径（必须存在）"},
			"to":        map[string]any{"type": "string", "description": "目标路径（父目录必须存在）"},
			"overwrite": map[string]any{"type": "boolean", "description": "目标已存在时是否覆盖（默认 false，覆盖前旧目标备份为 <to>.bak）"},
		},
		"required": []string{"from", "to"},
	}
}

func (t *MoveFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	from := strArg(args, "from")
	to := strArg(args, "to")
	if from == "" {
		return "", fmt.Errorf("缺少参数 from（要移动的源文件路径）")
	}
	if to == "" {
		return "", fmt.Errorf("缺少参数 to（目标路径，父目录必须已存在）")
	}
	overwrite := boolArg(args, "overwrite")

	// 源：用 resolveEntry——只解析父目录，最后一段保持原样。软链移动的是链接本身
	// （用 resolve 会把软链解析成真实目标，"改个软链的名字"就会把它指向的文件搬走，
	// 那不是调用方要的）。链接本身被移动，它指向的文件并没有被搬走，也不构成越权。
	src, err := t.resolveEntry(ctx, from)
	if err != nil {
		return "", fmt.Errorf("from 不存在或超出允许范围（file.allowDirs）: %s（%v）", from, err)
	}
	// 目标：resolveWrite 允许目标不存在，但父目录必须存在且在允许根内。
	dst, err := t.resolveWrite(ctx, to)
	if err != nil {
		return "", fmt.Errorf("to 的父目录不存在或超出允许范围（file.allowDirs）: %s（%v）", to, err)
	}
	if src == dst {
		return "", fmt.Errorf("源和目标是同一个文件，无需移动: %s", src)
	}

	// 先判断目标是否已存在：这里必须早于任何写动作，否则"覆盖失败"会连源文件一起丢。
	dstInfo, statErr := os.Lstat(dst)
	dstExists := statErr == nil
	if dstExists && dstInfo.IsDir() {
		return "", fmt.Errorf("目标是目录，不能被文件覆盖: %s（请换个 to 路径）", dst)
	}
	if dstExists && !overwrite {
		return "", fmt.Errorf("目标已存在: %s；未做任何改动。要覆盖请传 overwrite=true，或换一个 to 路径", dst)
	}

	detail := fmt.Sprintf("源: %s\n目标: %s\n覆盖已有目标: %v", src, dst, overwrite)
	if err := requestWriteApproval(ctx, t.Approver, t.Name(), fmt.Sprintf("%s → %s", src, dst), detail); err != nil {
		return "", err
	}

	// 覆盖：先把旧目标改名成 .bak（沿用 Friday 现有的 .bak 约定），移动失败时还能还原。
	backup := ""
	if dstExists {
		backup = dst + ".bak"
		if err := os.Rename(dst, backup); err != nil {
			return "", fmt.Errorf("备份旧目标失败（未做任何改动）: %w", err)
		}
	}
	if err := movePath(src, dst); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dst) // 还原旧目标，别把用户的文件留在半路
		}
		return "", fmt.Errorf("移动失败: %w", err)
	}

	out := fmt.Sprintf("已移动：%s → %s", src, dst)
	if backup != "" {
		out += fmt.Sprintf("\n旧目标已备份到 %s（如需还原，把它改名回 %s 即可）", backup, dst)
	}
	return out, nil
}

// DeleteFileTool 删除文件（不硬删：移进回收站目录，可恢复）。
type DeleteFileTool struct {
	FileTools
	Approver *approval.Queue
	// TrashBase 回收站根目录，留空默认用系统临时目录下的 friday-trash。
	TrashBase string
}

func (t *DeleteFileTool) Name() string { return "delete_file" }

func (t *DeleteFileTool) Description() string {
	return "删除文件（只支持文件，目录请用 move_file 移走或手动处理）。" +
		"不做硬删除：文件会被移到回收站，需要时移回原路径即可恢复。" +
		"path 为允许目录内的相对或绝对路径（相对路径按本次会话的工作目录解析）。" +
		"confirm 审批模式下需用户在网页批准后才会执行。"
}

func (t *DeleteFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "要删除的文件路径（不能是目录）"},
		},
		"required": []string{"path"},
	}
}

func (t *DeleteFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path := strArg(args, "path")
	if path == "" {
		return "", fmt.Errorf("缺少参数 path（要删除的文件路径）")
	}
	// 用 resolveEntry：软链按软链自身处理（只删链接，不动它指向的目标）。
	real, err := t.resolveEntry(ctx, path)
	if err != nil {
		return "", fmt.Errorf("path 不存在或超出允许范围（file.allowDirs）: %s（%v）", path, err)
	}
	st, err := os.Lstat(real)
	if err != nil {
		return "", fmt.Errorf("文件不存在或无法访问: %s（%v）", path, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("只能删文件，目录请用 move_file 移走或手动处理: %s", path)
	}

	if err := requestWriteApproval(ctx, t.Approver, t.Name(), real, fmt.Sprintf("删除（移入回收站，可恢复）: %s", real)); err != nil {
		return "", err
	}

	trash, err := t.trashDir()
	if err != nil {
		return "", err
	}
	dest, err := reserveTrashPath(trash, filepath.Base(real), time.Now())
	if err != nil {
		return "", err
	}
	if err := movePath(real, dest); err != nil {
		_ = os.Remove(dest) // 占位文件别留在回收站里装成"已删除"
		return "", fmt.Errorf("移入回收站失败: %w", err)
	}

	return fmt.Sprintf("已删除（移入回收站）：%s\n"+
		"文件已从原路径移走，现在在：%s\n"+
		"恢复方法：把该文件移回原路径即可，例如用 move_file 把 %s 移到 %s。\n"+
		"注意：回收站位于系统临时目录（%s），会被系统定期清理；想长期保留的文件请先移走，不要直接删。",
		real, dest, dest, real, trash), nil
}

// trashDir 准备回收站目录（0700 权限，必要时创建）。
func (t *DeleteFileTool) trashDir() (string, error) {
	base := strings.TrimSpace(t.TrashBase)
	if base == "" {
		base = filepath.Join(config.TempDirBase(), trashDirName)
	}
	if _, err := os.Stat(base); err == nil {
		// 已存在就用现成的，不动用户自己设的权限。
		return base, nil
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("创建回收站目录失败: %w", err)
	}
	// MkdirAll 会受 umask 影响，再 chmod 一次保证 0700。
	if err := os.Chmod(base, 0o700); err != nil {
		return "", fmt.Errorf("设置回收站目录权限失败: %w", err)
	}
	return base, nil
}

// trashFileName 生成回收站里的文件名：时间戳 + 原名。
//
// 同名文件在同一秒内被删两次会撞名——而回收站里的同名文件会被后来的那个直接覆盖
// （用户删错了就再也找不回来）。所以撞名时加序号，直到空位。
func trashFileName(base string, now time.Time) string {
	return now.Format("20060102-150405") + "-" + base
}

// reserveTrashPath 在回收站里给 base 找一个不撞名的位置。
//
// 用 O_EXCL 抢占一个空文件名（真正的原子唯一），而不是"先看看存不存在"——后者
// 在两次删除挨得极近时会同时抢到同一个名字，后一个把前一个覆盖掉，用户删错的
// 文件就再也找不回来了。占到的空文件随后被移进来的文件替换（改名覆盖空文件是
// 安全的），移动失败时把它删掉。
func reserveTrashPath(trash, base string, now time.Time) (string, error) {
	name := trashFileName(base, now)
	for i := 0; i < 100; i++ {
		candidate := filepath.Join(trash, name)
		if i > 0 {
			candidate = filepath.Join(trash, fmt.Sprintf("%s-%d", name, i))
		}
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("创建回收站占位文件失败: %w", err)
		}
	}
	return "", fmt.Errorf("回收站里找不到可用文件名（%s）", name)
}

// resolveEntry 解析路径：父目录按允许根解析（软链展开），最后一段保持原样。
//
// 为什么不用 resolve：resolve 会把整条路径的软链都解析成真实目标，删软链就会
// 删掉它指向的文件。这里刻意只解析父目录，"这一项"是什么就删什么。
func (t *FileTools) resolveEntry(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("缺少参数 path")
	}
	roots := t.roots(ctx)
	if !filepath.IsAbs(path) {
		if len(roots) == 0 {
			return "", fmt.Errorf("未配置可访问目录（file.allowDirs）")
		}
		path = filepath.Join(roots[0], path)
	}
	dir, base := filepath.Split(filepath.Clean(path))
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("非法的目标路径: %s", path)
	}
	realDir, err := t.resolve(ctx, dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(realDir, base), nil
}

// movePath 把 src 移到 dst：同设备用 rename，跨设备退回复制+删除。
//
// 为什么要有跨设备兜底：macOS 上 /tmp 与用户主目录常在不同的卷上，os.Rename
// 会直接报 EXDEV；不兜底的话"删文件"和"移回收站"在很多机器上根本没法用。
func movePath(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !isCrossDevice(err) {
		return err
	}
	if cerr := copyEntry(src, dst); cerr != nil {
		return cerr
	}
	if rerr := os.Remove(src); rerr != nil {
		return fmt.Errorf("已复制到 %s，但删除源文件失败: %w", dst, rerr)
	}
	return nil
}

// isCrossDevice 判断错误是否为跨设备移动失败。
func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	// Windows 上跨卷改名报的是 ERROR_NOT_SAME_DEVICE，不是 EXDEV。
	msg := err.Error()
	return strings.Contains(msg, "cross-device link") || strings.Contains(msg, "not the same device")
}

// copyEntry 跨设备兜底用的复制：普通文件复制内容并保留权限位，软链复制链接本身。
func copyEntry(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if st.IsDir() {
		return fmt.Errorf("跨设备移动目录暂不支持: %s（请改用 exec_command 的 mv）", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// OpenFile 的权限位会被 umask 改写，再 chmod 一次保证与源一致。
	return os.Chmod(dst, st.Mode().Perm())
}
