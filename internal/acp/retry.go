// retry.go 回答一个问题：这一轮失败之后，要不要重来？
//
// 判据只有一条——失败是「链路断了」还是「被拒绝了」。
//
//   - 链路断了（agent 进程崩了、对端把管道掐了、连接被重置）：这一轮其实一个
//     指令都没执行完，用户要的只是"把这句话发出去"。重建连接后重发通常就能成，
//     值得重试。
//   - 被拒绝了（额度用完、鉴权失败、模型报参数错）：重试只是把同一个失败重复
//     几次，还白烧时间与额度，不值得——原样把错误交回，让用户去处理根因。
//
// busy（agent 侧还有活跃轮）另有专门的自愈路径，见 manager.go 的 busy 自愈，
// 这里不插手。
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/capyflow/friday/internal/config"
)

// promptRetryMax 是「链路故障」的重试上限（不含首次那一次尝试）：
// 首次失败后最多再试 3 次。
var promptRetryMax = 3

// promptRetryDelay 是每次重试前的等待基数（指数退避：第 n 次等 delay×2^(n-1)）。
// 重建 agent 进程本身就要花时间（npx 冷启动能到十几秒），这里只做礼貌的错开。
var promptRetryDelay = 500 * time.Millisecond

// transportSignals 是链路层故障的特征串：命中即说明不是"被拒绝"，而是"路断了"。
var transportSignals = []string{
	"peer disconnected",
	"connection reset",
	"connection closed",
	"connection refused",
	"broken pipe",
	"unexpected eof",
	"use of closed network connection",
	"no such process",
	"process exited",
	"stdin closed",
}

// quotaSignals 是额度/配额类错误的特征串：命中就不重试（重试只是白烧额度）。
var quotaSignals = []string{
	"quota",
	"insufficient_balance",
	"insufficient balance",
	"not enough balance",
	"billing",
	"payment required",
	"credit balance",
	"额度",
	"配额",
	"余额",
	"欠费",
}

// errorText 摊开错误里"像原因"的那些文本。
//
// ACP 的错误是一串 JSON：
//
//	{"code":-32603,"message":"Internal error","data":{"error":"peer disconnected before response"}}
//
// 直接拿整串做关键字匹配，既会被字段名干扰，也会漏掉 data 里真正的细节。
// 这里把 message、data.error/details/reason 与原文摊平成一段文本再匹配。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return raw
	}
	var v struct {
		Message string `json:"message"`
		Data    any    `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	parts := []string{raw, v.Message}
	if m, ok := v.Data.(map[string]any); ok {
		for _, k := range []string{"error", "details", "reason", "message"} {
			if s, ok := m[k].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " ")
}

// quotaExhausted 判断错误是不是"额度/配额用完"。
func quotaExhausted(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(errorText(err))
	for _, s := range quotaSignals {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// transportFailure 判断错误是不是链路层故障。procDead 是"agent 进程已经不在了"
// 这条独立证据：不管错误文本长什么样，进程没了就说明这一轮没跑成。
func transportFailure(err error, procDead bool) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(errorText(err))
	for _, s := range transportSignals {
		if strings.Contains(text, s) {
			return true
		}
	}
	return procDead
}

// retryablePromptError 综合判断这一轮失败后值不值得重来。
func retryablePromptError(err error, procDead bool) bool {
	if err == nil {
		return false
	}
	if activeTurnBusy(err) {
		return false // 会话忙有专门的自愈路径，不在这里重试
	}
	if quotaExhausted(err) {
		return false // 额度用完：再试也是同一个结果，只是白烧额度
	}
	return transportFailure(err, procDead)
}

// retryPrompt 在链路故障上重试这一轮。
//
// 每次重试都重建连接（进程 + 会话）：老连接已经断了，重发必须落在一条活着的
// 链路上。重建走的是与 busy 自愈同一条路（killLive → ensureLive → session/load
// 重接），所以 agent 支持重接时上下文还在，不支持时照例标注"上下文已重置"。
//
// 返回：新一轮的响应、当前连接、错误、是否降级、实际重试了几次。
func (m *Manager) retryPrompt(
	tctx context.Context,
	id string,
	th *Thread,
	agentCfg config.AcpAgentConfig,
	hooks TurnHooks,
	lv *live,
	turnID string,
	turnDone chan struct{},
	sink *turnSink,
	blocks []acp.ContentBlock,
	res acp.PromptResponse,
	promptErr error,
	degraded bool,
) (acp.PromptResponse, *live, error, bool, int) {
	attempts := 0
	for attempt := 1; attempt <= promptRetryMax; attempt++ {
		if tctx.Err() != nil || lv.stopping.Load() {
			break // 内核关闭或用户按了停止：不再重试
		}
		if !retryablePromptError(promptErr, !lv.proc.alive()) {
			break // 不是链路故障（或额度用完）：原样交回
		}
		if sink.allText() != "" {
			break // 这一轮已经吐过正文：重发会把回答重复一遍
		}
		attempts = attempt
		wait := promptRetryDelay * time.Duration(1<<(attempt-1))
		log.Printf("[智能体] %s 连接中断，准备第 %d/%d 次重试: %v",
			th.Agent, attempt, promptRetryMax, promptErr)
		hooks.status(fmt.Sprintf("%s 连接中断，正在重试（第 %d/%d 次）…",
			th.Agent, attempt, promptRetryMax))
		select {
		case <-tctx.Done():
		case <-time.After(wait):
		}
		if tctx.Err() != nil || lv.stopping.Load() {
			break
		}
		// 老连接已经死了：重建进程与会话，再把这条消息原样发一次。
		m.killLive(id, "连接中断，重试前重建连接")
		newLv, deg, lerr := m.ensureLive(tctx, th, agentCfg, hooks)
		if lerr != nil {
			promptErr = fmt.Errorf("重建连接失败: %v（原错误: %w）", lerr, promptErr)
			break
		}
		if newLv == nil || !newLv.proc.alive() {
			promptErr = fmt.Errorf("重建连接后 agent 进程未就绪（原错误: %w）", promptErr)
			break
		}
		lv = newLv
		degraded = degraded || deg
		m.mu.Lock()
		lv.turnDone = turnDone
		lv.turnID = turnID
		lv.stopping.Store(false)
		m.mu.Unlock()
		lv.addSink(sink)
		res, promptErr = lv.proc.conn.Prompt(tctx, acp.PromptRequest{
			SessionId: lv.sessionID, Prompt: blocks,
		})
		if promptErr == nil {
			log.Printf("[智能体] %s 重试第 %d 次成功", th.Agent, attempt)
			break
		}
	}
	return res, lv, promptErr, degraded, attempts
}
