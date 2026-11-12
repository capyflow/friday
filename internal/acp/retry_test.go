// retry_test.go 守护「这一轮失败后要不要重来」的判据与执行。
package acp

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 对端把管道掐了（peer disconnected）属于链路故障：重建连接后应当把同一条
// 消息再发一次，用户这一轮不该就这么断掉。
func TestSendRetriesAfterPeerDisconnect(t *testing.T) {
	oldDelay := promptRetryDelay
	promptRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { promptRetryDelay = oldDelay })

	var f *fakeAgent
	f = &fakeAgent{promptScript: func(fc *fakeConn, sid string) string {
		if f.spawnCount() == 1 {
			fc.crash() // 第一轮：agent 侧掐断管道 → client 侧收到 peer disconnected
			time.Sleep(50 * time.Millisecond)
			return "end_turn"
		}
		return defaultScript(fc, sid) // 重建连接后这一轮正常跑完
	}}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	c := &collector{}
	res, err := m.Send(context.Background(), th.ID, "继续", nil, c.hooks())
	if err != nil {
		t.Fatalf("连接中断后重试应接上这一轮: %v", err)
	}
	if res.Answer != "你好，这是回答。" || res.StopReason != "end_turn" {
		t.Fatalf("重试后的轮次结果不符: %+v", res)
	}
	if got := f.spawnCount(); got != 2 {
		t.Fatalf("断线后应重建一次连接（共 2 次拉起），实际 %d", got)
	}
	blocks := f.promptBlocks()
	if len(blocks) != 2 {
		t.Fatalf("应把同一条消息原样再发一次，实际发了 %d 次: %v", len(blocks), blocks)
	}
	if len(blocks[1]) == 0 || !strings.HasSuffix(blocks[1][len(blocks[1])-1], "继续") {
		t.Fatalf("重发的应是这条用户消息: %v", blocks[1])
	}
	found := false
	for _, s := range c.statuses() {
		if strings.Contains(s, "重试") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有一条重试提示给用户: %v", c.statuses())
	}
}

// 额度用完不重试：再试多少次都是同一个结果，只是白烧额度。
func TestSendDoesNotRetryWhenQuotaExhausted(t *testing.T) {
	f := &fakeAgent{promptScript: func(fc *fakeConn, sid string) string {
		return "ERR:You exceeded your current quota, please check your plan and billing details"
	}}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	_, err := m.Send(context.Background(), th.ID, "继续", nil, TurnHooks{})
	if err == nil {
		t.Fatal("额度用完应如实报错")
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Fatalf("应把原始错误交回: %v", err)
	}
	if blocks := f.promptBlocks(); len(blocks) != 1 {
		t.Fatalf("额度用完不该重试，实际发了 %d 次: %v", len(blocks), blocks)
	}
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("额度用完不该重建连接，实际拉起 %d 次", got)
	}
}

// 一直断线：重试到上限就如实报错，并说清重试过几次。
func TestSendGivesUpAfterMaxRetries(t *testing.T) {
	oldDelay := promptRetryDelay
	promptRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { promptRetryDelay = oldDelay })

	f := &fakeAgent{promptScript: func(fc *fakeConn, sid string) string {
		fc.crash()
		time.Sleep(50 * time.Millisecond)
		return "end_turn"
	}}
	m := newTestManager(t, nil, nil, f)
	th := mustThread(t, m)

	_, err := m.Send(context.Background(), th.ID, "继续", nil, TurnHooks{})
	if err == nil {
		t.Fatal("连续断线应报错")
	}
	if !strings.Contains(err.Error(), "已重试 3 次仍失败") {
		t.Fatalf("应说清重试过几次: %v", err)
	}
	if blocks := f.promptBlocks(); len(blocks) != promptRetryMax+1 {
		t.Fatalf("首次 + %d 次重试 = %d 次尝试，实际 %d 次",
			promptRetryMax, promptRetryMax+1, len(blocks))
	}
}

// 判据本身：额度类不重试，链路类要重试，busy 归 busy 自愈管。
func TestRetryablePromptError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		procDead bool
		want     bool
	}{
		{"peer disconnected", errJSON("peer disconnected before response"), false, true},
		{"连接被重置", errJSON("read tcp: connection reset by peer"), false, true},
		{"进程没了", errJSON("boom"), true, true},
		{"额度用完", errJSON("You exceeded your current quota"), false, false},
		{"额度用完（进程也死了）", errJSON("quota exceeded"), true, false},
		{"会话忙", errJSON("Session already has an active Turn."), false, false},
		{"普通报错", errJSON("model refused"), false, false},
		{"没有错误", nil, false, false},
	}
	for _, c := range cases {
		if got := retryablePromptError(c.err, c.procDead); got != c.want {
			t.Fatalf("%s: retryablePromptError = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// errJSON 造一条 ACP 形状的错误（SDK 把错误渲染成这串 JSON）。
func errJSON(inner string) error {
	return errors.New("{\"code\":-32603,\"message\":\"Internal error\",\"data\":{\"error\":" +
		strconv.Quote(inner) + "}}")
}
