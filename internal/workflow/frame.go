// Package workflow 是动态工作流引擎：编排脚本（.wf.ts）由 node 子进程 wf-host 做类型检查与沙箱执行，
// 一切副作用（子代理轮、命令执行、世界读取）都回到 Go 侧履约，因此权限与用量记账不会被脚本旁路。
//
// 设计与一期范围见 docs/WORKFLOW.md。
package workflow

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// 帧大小上限：world.run 的输出与 files.read 的内容都可能很大。
const maxFrameBytes = 32 << 20

// op 取值（与 wfhost/wf-host.mjs 头部注释一一对应）。
const (
	OpAsk             = "ask"
	OpWorldRun        = "world.run"
	OpFilesGlob       = "files.glob"
	OpFilesRead       = "files.read"
	OpFilesGrep       = "files.grep"
	OpGitChangedFiles = "git.changedFiles"
	OpGitDiff         = "git.diff"
	OpGitStatus       = "git.status"
	OpGitLog          = "git.log"
	OpEvent           = "event"
	OpResult          = "result"
	OpError           = "error"
	OpCancel          = "cancel"
)

// 事件 kind。
const (
	KindPhase  = "phase"
	KindLog    = "log"
	KindReport = "report"
)

// Frame 是 Go 与 wf-host 之间的一行 JSON 消息。字段按 Op 取用，未用到的留零值。
//
// 之所以是扁平结构而不是每个 op 一个类型：脚本侧发出的帧是扁平的（{"id":1,"op":"ask",...}），
// 且一期只有这一个协议对端；换成 map 反而要把每个字段再解析一遍。
type Frame struct {
	// 请求-应答关联。事件与终止帧为 0。
	ID int64  `json:"id,omitempty"`
	Op string `json:"op,omitempty"`

	// 事件（Op == OpEvent）
	Kind    string          `json:"kind,omitempty"`
	Name    string          `json:"name,omitempty"`
	Message string          `json:"message,omitempty"`
	Item    json.RawMessage `json:"item,omitempty"`

	// ask
	Agent        string          `json:"agent,omitempty"`
	Persona      json.RawMessage `json:"persona,omitempty"`
	Seq          int             `json:"seq,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
	Schema       json.RawMessage `json:"schema,omitempty"`

	// world.run
	Cmd       string   `json:"cmd,omitempty"`
	Args      []string `json:"args,omitempty"`
	TimeoutMs *int64   `json:"timeoutMs,omitempty"`

	// files.* / git.*
	Pattern string  `json:"pattern,omitempty"`
	Glob    *string `json:"glob,omitempty"`
	Path    *string `json:"path,omitempty"`
	Base    *string `json:"base,omitempty"`
	Count   *int    `json:"count,omitempty"`

	// 应答与终止
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
	Stage  string          `json:"stage,omitempty"`
	// 脚本 return 值由 host 包成 {"op":"result","value":...}
}

// frameReader 按行读取帧。
type frameReader struct {
	sc *bufio.Scanner
}

func newFrameReader(r io.Reader) *frameReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	return &frameReader{sc: sc}
}

// Next 取下一帧；对端正常结束时返回 io.EOF。
func (fr *frameReader) Next() (*Frame, error) {
	for fr.sc.Scan() {
		line := fr.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return nil, fmt.Errorf("宿主发来的帧不是合法 JSON：%w", err)
		}
		return &f, nil
	}
	if err := fr.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// frameWriter 串行化写出，允许多个在途请求并发应答。
type frameWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newFrameWriter(w io.Writer) *frameWriter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &frameWriter{enc: enc}
}

func (fw *frameWriter) Send(f *Frame) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.enc.Encode(f)
}
