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
