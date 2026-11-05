package notification

import (
	"context"
	"path/filepath"
	"testing"
)

func manualCenter(t *testing.T) *Center {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "notifications.json"), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestPostManualVisibleAndUnread 手动通知进列表、计未读，且可标记已读。
func TestPostManualVisibleAndUnread(t *testing.T) {
	c := manualCenter(t)

	n, err := c.PostManual("成本预算已用满，主动任务暂停。", []string{"dingtalk"})
	if err != nil {
		t.Fatalf("PostManual: %v", err)
	}
	if n.ID == "" || n.Status != "delivered" {
		t.Fatalf("落档状态不符: %+v", n)
	}
	if len(c.List()) != 1 {
		t.Fatalf("列表应有 1 条: %+v", c.List())
	}
	if got := c.Unread(); got != 1 {
		t.Errorf("未读应为 1，实际 %d", got)
	}
	if err := c.MarkRead(n.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := c.Unread(); got != 0 {
		t.Errorf("已读后未读应为 0，实际 %d", got)
	}
}

// TestPostManualWithoutChannelsIsSilent 没有可用推送通道时状态记为 silent，
// 内容仍在通知页可见。
func TestPostManualWithoutChannelsIsSilent(t *testing.T) {
	c := manualCenter(t)
	n, err := c.PostManual("预算提醒（无外推通道）", nil)
	if err != nil {
		t.Fatalf("PostManual: %v", err)
	}
	if n.Status != "silent" {
		t.Errorf("无通道应为 silent，实际 %q", n.Status)
	}
	if c.Unread() != 1 {
		t.Error("silent 通知也应计未读，否则用户看不到")
	}
}

// TestPostManualNotEnqueuedForDelivery 手动通知直接落终态，工作循环不得再投递它。
func TestPostManualNotEnqueuedForDelivery(t *testing.T) {
	c := manualCenter(t)
	if _, err := c.PostManual("已推过的告警", []string{"feishu"}); err != nil {
		t.Fatal(err)
	}
	did, err := c.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if did {
		t.Error("手动通知不应进入投递队列（会被重复投递）")
	}
}

// TestPostManualRejectsEmptyText 空正文没有意义，直接拒绝。
func TestPostManualRejectsEmptyText(t *testing.T) {
	c := manualCenter(t)
	if _, err := c.PostManual("   ", nil); err == nil {
		t.Fatal("空正文应报错")
	}
}

// TestPostManualPersistsAcrossRestart 重启后仍在（成本告警不能随进程丢）。
func TestPostManualPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c, err := New(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PostManual("预算告警", []string{"dingtalk"}); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path, nil, nil)
	if err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	list := reopened.List()
	if len(list) != 1 || list[0].Text != "预算告警" {
		t.Fatalf("重启后通知丢失或变形: %+v", list)
	}
}

// TestPostOnceSameKeyPromptsOnce 幂等键是这条链路的核心：智能体的「这一轮结束」
// 可能被重复触发（重试/补投），同一个 key 只该落一条、只弹一次系统提示。
func TestPostOnceSameKeyPromptsOnce(t *testing.T) {
	c := manualCenter(t)
	var popped []string
	c.SetDesktopDispatch(func(n Notice) error {
		popped = append(popped, n.Text)
		return nil
	})

	first, err := c.PostOnce("acp:th1:t1:done", "fake 这一轮跑完了：改了 3 个文件。", nil)
	if err != nil {
		t.Fatalf("PostOnce: %v", err)
	}
	again, err := c.PostOnce("acp:th1:t1:done", "fake 这一轮跑完了：改了 3 个文件。", nil)
	if err != nil {
		t.Fatalf("重复 PostOnce: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("同键应复用同一条记录: %s vs %s", first.ID, again.ID)
	}
	if len(c.List()) != 1 {
		t.Errorf("通知中心不该堆两条: %+v", c.List())
	}
	if len(popped) != 1 {
		t.Errorf("系统提示只该弹一次，实际 %d 次", len(popped))
	}
}

// TestPostOnceDistinctKeysBothPrompt 同一轮里的两张审批卡是两个身份，各提示一次。
func TestPostOnceDistinctKeysBothPrompt(t *testing.T) {
	c := manualCenter(t)
	popped := 0
	c.SetDesktopDispatch(func(Notice) error { popped++; return nil })
	if _, err := c.PostOnce("acp:th1:approval:11", "fake 在等你批准：写 a.go", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PostOnce("acp:th1:approval:12", "fake 在等你批准：写 b.go", nil); err != nil {
		t.Fatal(err)
	}
	if popped != 2 || len(c.List()) != 2 {
		t.Fatalf("两条事件应各留一档各提示一次: popped=%d list=%d", popped, len(c.List()))
	}
}

// TestPostOnceUnreadAndPersists 走桌面桥的同时仍进通知中心（未读、重启不丢）。
func TestPostOnceUnreadAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	c, err := New(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PostOnce("acp:th1:t1:done", "fake 这一轮跑完了。", nil); err != nil {
		t.Fatal(err)
	}
	if got := c.Unread(); got != 1 {
		t.Fatalf("未读应为 1，实际 %d", got)
	}
	reopened, err := New(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list := reopened.List(); len(list) != 1 || list[0].Text != "fake 这一轮跑完了。" {
		t.Fatalf("重启后丢失或变形: %+v", list)
	}
}

// TestPostOnceRejectsEmptyKey 没有身份就没有幂等可言，直接拒绝。
func TestPostOnceRejectsEmptyKey(t *testing.T) {
	c := manualCenter(t)
	if _, err := c.PostOnce("  ", "正文", nil); err == nil {
		t.Fatal("空 key 应报错")
	}
	if _, err := c.PostOnce("acp:th1:t1:done", "  ", nil); err == nil {
		t.Fatal("空正文应报错")
	}
}

// TestPostManualKeepsNoDesktopPrompt 手动告警维持原行为：只进通知中心，
// 不抢桌面提示桥（要弹提示请走 PostOnce 给出身份键）。
func TestPostManualKeepsNoDesktopPrompt(t *testing.T) {
	c := manualCenter(t)
	popped := 0
	c.SetDesktopDispatch(func(Notice) error { popped++; return nil })
	if _, err := c.PostManual("成本预算已用满。", nil); err != nil {
		t.Fatal(err)
	}
	if popped != 0 {
		t.Error("PostManual 不该走桌面提示桥")
	}
}
