// Package workdir 携带"这一次请求在哪个目录里干活"。
//
// 为什么走 ctx 而不是一个全局值：Brain 上原来只有一个 projectDir（启动时按进程
// 工作目录设一次），项目上下文注入从那儿读 AGENTS.md/README.md。工作模式允许同时
// 开着好几条会话，还有心跳/计划/目标在后台并发跑，把它们的工作目录塞进同一个全局
// 字段会互相串——A 会话把目录改成自己的项目，B 会话下一轮就被带跑了。所以会话
// 指定的目录随请求 ctx 传下去，取用侧（项目上下文注入、文件工具的可读写根）按
// ctx 优先、没有就回落到进程级的默认工作目录。
//
// 谁在用：
//   - internal/agent：会话首轮注入项目上下文时按 ctx 取目录；
//   - internal/tools：文件类工具把会话目录当作额外的允许根与相对路径基准；
//   - internal/web：建工作会话时校验用户选的目录，并在每次对话请求上挂上它。
package workdir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ctxKey struct{}

// With 把会话的工作目录挂进 ctx。空串等于不挂——调用方按"这条会话没指定目录"
// 处理，行为与以往一致（用进程的工作目录）。
func With(ctx context.Context, dir string) context.Context {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, dir)
}

// From 取出本次请求的工作目录；没设置返回空串。
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if dir, ok := ctx.Value(ctxKey{}).(string); ok {
		return strings.TrimSpace(dir)
	}
	return ""
}

// Normalize 把用户给的目录翻成可用的绝对路径：展开 ~、去掉首尾空白与尾部斜杠、
// 转绝对路径，并要求它已经存在且是个目录。空串原样返回空（= 不指定）。
//
// 报错都说清下一步：这条会话的目录是用户在界面上点选的，"不存在"通常意味着
// 目录后来被删了或路径打错了，重选一个就好，不是让调用方去猜规则。
func Normalize(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开主目录失败: %w（改用绝对路径重选一个目录）", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("目录路径无法解析: %q（改用绝对路径重选一个目录）", dir)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("目录不存在：%s（在「选择目录」里重选一个，或留空沿用日常目录）", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是目录（选目录本身，不是里面的文件）", abs)
	}
	return abs, nil
}
