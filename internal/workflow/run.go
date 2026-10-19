package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"
)

// Event 是宿主上行的一条进度事件。
type Event struct {
	Kind    string          `json:"kind"`
	Name    string          `json:"name,omitempty"` // phase 名
	Message string          `json:"message,omitempty"`
	Item    json.RawMessage `json:"item,omitempty"` // report 条目
	At      time.Time       `json:"at"`
}

// Reports 取出所有 report 条目——run 失败时也要交付的那部分产出。
func (o *Outcome) Reports() []json.RawMessage {
	var out []json.RawMessage
	for _, e := range o.Events {
		if e.Kind == KindReport && len(e.Item) > 0 {
			out = append(out, e.Item)
		}
	}
	return out
}

// PhasesSeen 按出现顺序返回去重后的阶段名（与 check 出的阶段图核对用）。
func (o *Outcome) PhasesSeen() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range o.Events {
		if e.Kind == KindPhase && e.Name != "" && !seen[e.Name] {
			seen[e.Name] = true
			out = append(out, e.Name)
		}
	}
	return out
}

// Outcome 是一次执行的产出。
type Outcome struct {
	// Value 是脚本 return 的值（一期约定为 WorkflowReport 形状）。
	Value json.RawMessage
	// Events 是全部上行事件，按到达顺序。
	Events []Event
	// Duration 是宿主实际运行时长。
	Duration time.Duration
}

// RunError 是脚本侧或宿主侧的失败。Stderr 带宿主进程的最后一段错误输出。
type RunError struct {
	Message string
	Stage   string // setup | runtime
	Stderr  string
}

func (e *RunError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("工作流执行失败（%s）：%s｜宿主输出：%s", e.Stage, e.Message, e.Stderr)
	}
	return fmt.Sprintf("工作流执行失败（%s）：%s", e.Stage, e.Message)
}

// RunOpts 是一次执行的运行参数。
type RunOpts struct {
	// Timeout 是整个 run 的墙钟上限；0 = 只受调用方 ctx 约束。
	Timeout time.Duration
	// OnEvent 非空时，每条上行事件都会回调一次（在读取 goroutine 上，勿阻塞）。
	OnEvent func(Event)
	// MaxOldSpaceMB 覆盖 Host 上的堆上限（0 = 不覆盖）。
	MaxOldSpaceMB int
}

// Run 执行一个脚本：宿主负责控制流，Go 侧 Executor 负责一切副作用。
//
// 扇出的并发在这里成立——宿主会同时挂多个在途请求，每个请求单独起 goroutine 应答；
// 若在读取循环里串行处理，脚本里的 Promise.all 就退化成队列（实测：并发 401ms / 串行 1208ms）。
func Run(ctx context.Context, h *Host, scriptPath string, args map[string]any, ex Executor, opts RunOpts) (*Outcome, error) {
	if h == nil {
		return nil, errors.New("未定位到脚本宿主（node 或 wfhost 目录缺失）")
	}
	if ex == nil {
		return nil, errors.New("缺少 Executor：脚本的副作用无处落地")
	}

	runCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	host := *h
	if opts.MaxOldSpaceMB > 0 {
		host.MaxOldSpaceMB = opts.MaxOldSpaceMB
	}

	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		return nil, err
	}
	p, err := host.startRun(runCtx, abs, args)
	if err != nil {
		return nil, err
	}
	started := time.Now()

	var (
		mu     sync.Mutex
		events []Event
		wg     sync.WaitGroup
		value  json.RawMessage
		runErr error
	)
	reply := func(id int64, result any, msg string) {
		f := &Frame{ID: id}
		if msg != "" {
			f.Error = msg
		} else {
			b, err := json.Marshal(result)
			if err != nil {
				f.Error = fmt.Sprintf("应答无法序列化：%v", err)
			} else {
				f.Result = b
			}
		}
		_ = p.w.Send(f) // 宿主已退出时写失败无所谓，它会以进程结束呈现
	}

	watchdog := make(chan struct{})
	defer close(watchdog)
	go func() {
		select {
		case <-runCtx.Done():
			// 宿主阻塞在等某个应答时不会自己结束；杀进程让 stdout 关闭，读取循环才能退出
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		case <-watchdog:
		}
	}()

	for {
		f, err := p.out.Next()
		if errors.Is(err, io.EOF) {
			if value == nil && runErr == nil {
				if runCtx.Err() != nil {
					runErr = fmt.Errorf("工作流被取消或超时：%w", runCtx.Err())
				} else {
					runErr = &RunError{Stage: "runtime", Message: "宿主提前结束且没有给出结果", Stderr: p.stderr.String()}
				}
			}
			break
		}
		if err != nil {
			runErr = &RunError{Stage: "runtime", Message: err.Error(), Stderr: p.stderr.String()}
			break
		}

		switch {
		case f.ID != 0 && f.Op != "":
			wg.Add(1)
			go func(f *Frame) {
				defer wg.Done()
				result, msg := dispatch(runCtx, ex, f)
				reply(f.ID, result, msg)
			}(f)

		case f.Op == OpEvent:
			e := Event{Kind: f.Kind, Name: f.Name, Message: f.Message, Item: f.Item, At: time.Now()}
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
			if opts.OnEvent != nil {
				opts.OnEvent(e)
			}

		case f.Op == OpResult:
			value = f.Value

		case f.Op == OpError:
			runErr = &RunError{Message: f.Error, Stage: f.Stage, Stderr: p.stderr.String()}

		default:
			// {"op":"cancel"} 等等由我们自己发起，忽略
		}

		if value != nil || runErr != nil {
			break
		}
	}

	// 收尾：关 stdin、等进程；在途请求会因 ctx 结束而返回
	_ = p.stdin.Close()
	waitErr := p.cmd.Wait()
	wg.Wait()

	out := &Outcome{Value: value, Events: events, Duration: time.Since(started)}
	if runErr != nil {
		return out, runErr
	}
	if waitErr != nil && value == nil {
		return out, &RunError{Stage: "runtime", Message: fmt.Sprintf("宿主退出：%v", waitErr), Stderr: p.stderr.String()}
	}
	return out, nil
}

// dispatch 把一帧请求转成一次 Executor 调用。返回 (结果, 错误信息)。
func dispatch(ctx context.Context, ex Executor, f *Frame) (any, string) {
	switch f.Op {
	case OpAsk:
		req := AskRequest{Agent: f.Agent, Persona: f.Persona, Seq: f.Seq, Instructions: f.Instructions, Schema: f.Schema}
		reply, err := ex.Ask(ctx, req)
		if err != nil {
			return nil, errText(err)
		}
		if reply.Typed() {
			return map[string]any{"value": json.RawMessage(reply.Value)}, ""
		}
		return map[string]any{"text": reply.Text}, ""

	case OpWorldRun:
		var timeout time.Duration
		if f.TimeoutMs != nil && *f.TimeoutMs > 0 {
			timeout = time.Duration(*f.TimeoutMs) * time.Millisecond
		}
		res, err := ex.WorldRun(ctx, f.Cmd, f.Args, timeout)
		if err != nil {
			return nil, errText(err)
		}
		return res, ""

	case OpFilesGlob:
		matches, err := ex.FilesGlob(ctx, f.Pattern)
		if err != nil {
			return nil, errText(err)
		}
		return matches, ""

	case OpFilesRead:
		if f.Path == nil {
			return nil, "files.read 缺少 path"
		}
		text, err := ex.FilesRead(ctx, *f.Path)
		if err != nil {
			return nil, errText(err)
		}
		return text, ""

	case OpFilesGrep:
		glob := ""
		if f.Glob != nil {
			glob = *f.Glob
		}
		hits, err := ex.FilesGrep(ctx, f.Pattern, glob)
		if err != nil {
			return nil, errText(err)
		}
		return hits, ""

	case OpGitChangedFiles:
		base := deref(f.Base)
		paths, err := ex.GitChangedFiles(ctx, base)
		if err != nil {
			return nil, errText(err)
		}
		return paths, ""

	case OpGitDiff:
		text, err := ex.GitDiff(ctx, deref(f.Base), deref(f.Path))
		if err != nil {
			return nil, errText(err)
		}
		return text, ""

	case OpGitStatus:
		st, err := ex.GitStatus(ctx)
		if err != nil {
			return nil, errText(err)
		}
		return st, ""

	case OpGitLog:
		count := 0
		if f.Count != nil {
			count = *f.Count
		}
		commits, err := ex.GitLog(ctx, count)
		if err != nil {
			return nil, errText(err)
		}
		return commits, ""

	default:
		return nil, fmt.Sprintf("未知 op：%s", f.Op)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
