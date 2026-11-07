package admin

import (
	"testing"
	"time"
)

// TestPairCodeLifecycle 配对码一次性语义：对的码只换一次，错码/过期码都拒绝。
func TestPairCodeLifecycle(t *testing.T) {
	s := &Server{}
	code, expires := s.newPairCode()
	if len(code) != 8 {
		t.Fatalf("配对码长度 = %d，应为 8", len(code))
	}
	if !time.Now().Before(expires) {
		t.Fatalf("配对码已过期：expires=%v", expires)
	}
	if !s.redeemPairCode(code) {
		t.Fatalf("首次兑换应成功")
	}
	if s.redeemPairCode(code) {
		t.Fatalf("同一码第二次兑换应失败（一次性）")
	}
}

func TestPairCodeRejects(t *testing.T) {
	s := &Server{}
	code, _ := s.newPairCode()
	if s.redeemPairCode(code + "X") {
		t.Fatalf("错码应拒绝")
	}
	if s.redeemPairCode("") {
		t.Fatalf("空码应拒绝")
	}
	// 过期：手工把有效期拨回过去。
	s.pair.expires = time.Now().Add(-time.Minute)
	if s.redeemPairCode(code) {
		t.Fatalf("过期码应拒绝")
	}
	// 兑换失败不清现场：修复有效期后原码仍可用（用户重试不受伤）。
	s.pair.expires = time.Now().Add(time.Minute)
	if !s.redeemPairCode(code) {
		t.Fatalf("拨回有效期后原码应可兑换")
	}
}

// TestPairCodeRegenerated 新码生成后旧码立即作废（防止多码并存）。
func TestPairCodeRegenerated(t *testing.T) {
	s := &Server{}
	old, _ := s.newPairCode()
	s.newPairCode()
	if s.redeemPairCode(old) {
		t.Fatalf("旧码应已作废")
	}
}
