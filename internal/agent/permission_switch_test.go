package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"

	"github.com/capyflow/friday/internal/approval"
	"github.com/capyflow/friday/internal/config"
)

// writePermConfig 落一份最小可解析的 config.yaml（切档会写回 permissions.mode）。
func writePermConfig(t *testing.T) (string, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("permissions:\n  mode: confirm\n"), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	cfg := &config.Config{}
	cfg.Permissions.Mode = config.PermModeConfirm
	return path, cfg
}

// TestReloadPermissionsDuringTurnDoesNotBlock 守住「对话中也能切权限」：
// 一轮对话从进 Ask 到跑完一直持着 brain.rw 的读锁，切权限若走重建 Agent 就要等这把锁，
// 请求会被挂到本轮结束。这里模拟那把锁被占着，要求切换立刻返回。
func TestReloadPermissionsDuringTurnDoesNotBlock(t *testing.T) {
	path, cfg := writePermConfig(t)
	q := approval.New(string(approval.ModeConfirm), nil, 0)
	if _, err := NewPermissionChecker(cfg, q); err != nil {
		t.Fatalf("组装权限层失败: %v", err)
	}

	brain := &Brain{}
	brain.rw.RLock() // 一轮对话进行中
	defer brain.rw.RUnlock()

	done := make(chan error, 1)
	go func() {
		done <- ReloadPermissions(cfg, path, nil, brain, q, config.PermModeAcceptEdits)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("切权限失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("对话中切权限被挂住：2 秒还没返回（多半是又去重建 Agent 等读锁了）")
	}

	// 分层生效之一：编辑免审当场翻转，正在进行的这一轮也算数。
	if !q.EditsFree() {
		t.Error("切到 acceptEdits 后编辑类审批应立即免审")
	}
	// 配置快照立刻是新值（GET /api/permissions 读的就是它）。
	if got := cfg.Snapshot().Permissions.EffectiveMode(); got != config.PermModeAcceptEdits {
		t.Errorf("配置快照应为 %s，实际 %s", config.PermModeAcceptEdits, got)
	}
	// 落盘也已完成，重启后仍是新档位。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if !strings.Contains(string(data), config.PermModeAcceptEdits) {
		t.Errorf("config.yaml 未写回新档位：%s", data)
	}
}

// TestReloadPermissionsFullAccessFlipsLiveChecker 完全信任要落到活着的 Checker 上：
// 命令审批档位不再靠重建 Agent，而是对当前 Checker 做 SetMode。
func TestReloadPermissionsFullAccessFlipsLiveChecker(t *testing.T) {
	path, cfg := writePermConfig(t)
	q := approval.New(string(approval.ModeConfirm), nil, 0)
	checker, err := NewPermissionChecker(cfg, q)
	if err != nil {
		t.Fatalf("组装权限层失败: %v", err)
	}
	if checker.Mode() != vxagent.ModeConfirm {
		t.Fatalf("初始档位应为 %s，实际 %s", vxagent.ModeConfirm, checker.Mode())
	}

	brain := &Brain{}
	if err := ReloadPermissions(cfg, path, nil, brain, q, config.PermModeFullAccess); err != nil {
		t.Fatalf("切权限失败: %v", err)
	}
	// SetMode 在后台落位（Checker 的锁贯穿判定，不能挡住这次请求）。
	deadline := time.Now().Add(2 * time.Second)
	for checker.Mode() != vxagent.ModeFullAccess {
		if time.Now().After(deadline) {
			t.Fatalf("活 Checker 未切到 %s，实际 %s", vxagent.ModeFullAccess, checker.Mode())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !q.EditsFree() {
		t.Error("切到 fullAccess 后编辑类审批应免审")
	}
}

// TestApplyPermissionModeNowWithoutChecker 没有活 Checker 时要如实说落不进去，
// 调用方据此退回重建 Agent（而不是假装切成功了）。
func TestApplyPermissionModeNowWithoutChecker(t *testing.T) {
	_, cfg := writePermConfig(t)
	cfg.Permissions.Mode = config.PermModeFullAccess
	if ApplyPermissionModeNow(cfg, approval.New(string(approval.ModeConfirm), nil, 0)) {
		t.Error("没记录过 Checker 的队列不应报告切换成功")
	}
}
