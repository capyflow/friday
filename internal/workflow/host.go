package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Host 是脚本沙箱宿主的定位信息：node 可执行文件 + wfhost 包目录。
type Host struct {
	Node string
	Dir  string
	// MaxOldSpaceMB 非 0 时给 node 传 --max-old-space-size，作为脚本侧内存的硬兜底。
	// 这是选 node 子进程而不是进程内解释器的主要原因之一：进程内无法设堆上限。
	MaxOldSpaceMB int
}

// hostEntry 是宿主入口文件。
const hostEntry = "wf-host.mjs"

// Detect 定位 node 与 wfhost 目录；任一找不到就返回错误——工作流功能随之整体关闭，
// 不做"部分可用"（脚本没地方跑，比不跑更容易让人误会）。
func Detect(nodePath, hostDir string) (*Host, error) {
	node := nodePath
	if node == "" {
		p, err := exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("未找到 node：工作流功能需要它（可在 workflow.nodePath 指定绝对路径）")
		}
		node = p
	} else if !filepath.IsAbs(node) {
		p, err := exec.LookPath(node)
		if err != nil {
			return nil, fmt.Errorf("workflow.nodePath=%s 不可执行", nodePath)
		}
		node = p
	}

	dir, err := resolveHostDir(hostDir)
	if err != nil {
		return nil, err
	}
	return &Host{Node: node, Dir: dir}, nil
}

// resolveHostDir 按显式配置 → cwd 及各级父目录 → 可执行文件同目录及上一级 的顺序找 wfhost/wf-host.mjs。
// 覆盖三种形态：从仓库根跑、go test 的包目录、以及打包后与二进制并排的 wfhost。
func resolveHostDir(explicit string) (string, error) {
	if explicit != "" {
		if fileInDir(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("workflow.hostDir=%s 下没有 %s", explicit, hostEntry)
	}

	var tried []string
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; dir != "" && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			cand := filepath.Join(dir, "wfhost")
			if fileInDir(cand) {
				return cand, nil
			}
			tried = append(tried, cand)
		}
	}
	if exe, err := os.Executable(); err == nil {
		for _, base := range []string{filepath.Dir(exe), filepath.Dir(filepath.Dir(exe))} {
			cand := filepath.Join(base, "wfhost")
			if fileInDir(cand) {
				return cand, nil
			}
			tried = append(tried, cand)
		}
	}
	return "", fmt.Errorf("没找到脚本宿主目录（试过的路径：%v）", tried)
}

func fileInDir(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, hostEntry))
	return err == nil
}

// nodeArgs 组装公共的 node 启动参数。
func (h *Host) nodeArgs(script string, tail ...string) []string {
	args := make([]string, 0, len(tail)+3)
	if h.MaxOldSpaceMB > 0 {
		args = append(args, fmt.Sprintf("--max-old-space-size=%d", h.MaxOldSpaceMB))
	}
	args = append(args, script)
	return append(args, tail...)
}

// runOnce 跑一次一次性的宿主子命令并收全 stdout（check / version 用）。
func (h *Host) runOnce(ctx context.Context, sub string, tail ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, h.Node, h.nodeArgs(hostEntry, append([]string{sub}, tail...)...)...)
	cmd.Dir = h.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// Version 返回宿主环境信息，用于设置页展示与可用性判断。
func (h *Host) Version(ctx context.Context) (map[string]string, error) {
	stdout, stderr, err := h.runOnce(ctx, "version")
	if err != nil && len(bytes.TrimSpace(stdout)) == 0 {
		return nil, fmt.Errorf("宿主 version 失败：%s", trimTail(stderr))
	}
	var out map[string]string
	if err := json.Unmarshal(lastJSONLine(stdout), &out); err != nil {
		return nil, fmt.Errorf("宿主 version 输出无法解析：%w", err)
	}
	return out, nil
}

// proc 是一次正在运行的宿主脚本进程。
type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	w      *frameWriter
	out    *frameReader
	stderr *lockedBuffer
}

// startRun 拉起 `wf-host run <script>`。args 为 nil 时不传 --args（脚本里的 args 为 {}）。
func (h *Host) startRun(ctx context.Context, scriptPath string, args any) (*proc, error) {
	tail := []string{"run", scriptPath}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("入参无法序列化：%w", err)
		}
		tail = append(tail, "--args", string(b))
	}
	cmd := exec.CommandContext(ctx, h.Node, h.nodeArgs(hostEntry, tail...)...)
	cmd.Dir = h.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errBuf := &lockedBuffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动脚本宿主失败：%w", err)
	}
	return &proc{cmd: cmd, stdin: stdin, w: newFrameWriter(stdin), out: newFrameReader(stdoutR), stderr: errBuf}, nil
}

// lastJSONLine 取多行输出里最后一个像 JSON 的起始行（宿主正常只输出一行，
// 但 node 的弃用告警会先占一行）。
func lastJSONLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) > 0 && (l[0] == '{' || l[0] == '[') {
			return l
		}
	}
	return bytes.TrimSpace(b)
}

func trimTail(b []byte) string {
	s := bytes.TrimSpace(b)
	if len(s) > 400 {
		return string(s[len(s)-400:])
	}
	return string(s)
}

// lockedBuffer 允许 node 的 stderr 写入与 Go 侧读取并发。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return trimTail(b.buf.Bytes())
}
