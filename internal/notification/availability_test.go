package notification

import (
	"slices"
	"testing"
)

func TestDisabledWebWithoutTargetsIsNotDelivered(t *testing.T) {
	c := newCenter(t, "", nil)
	c.SetWebEnabled(false)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	n := onlyNotice(t, c)
	if n.Status == "delivered" || len(n.DeliveredTo) != 0 || n.LastError == "" {
		t.Fatalf("没有输出仍被算作送达: %+v", n)
	}
}

func TestDisabledWebStillDeliversConfiguredTarget(t *testing.T) {
	target := &recordingTarget{name: "dingtalk"}
	c := newCenter(t, "", nil, target)
	c.SetWebEnabled(false)
	if err := c.Enqueue(testTask()); err != nil {
		t.Fatal(err)
	}
	processNotice(t, c)
	n := onlyNotice(t, c)
	if n.Status != "delivered" || !slices.Equal(n.DeliveredTo, []string{"dingtalk"}) || target.calls() != 1 {
		t.Fatalf("真实通道投递错误: %+v", n)
	}
}
